// Package oci runs containers through a subset of the OCI runtime
// lifecycle: create, start, state, kill and delete on a bundle (a
// directory with a config.json and a root filesystem), as runc does. It
// reuses MiniBox's container machinery; config.json is translated to a
// container.Config.
//
// Honest scope. MiniBox reads a subset of the runtime spec: the process
// (args, env, cwd, user, no terminal), the root path, the hostname, which
// namespaces to make (it always isolates uts/pid/mount/ipc/cgroup and
// makes a network namespace or shares the host's per the config; a
// namespace that joins an existing one by path is refused), and the
// cgroup resources (memory, cpu, pids). It does NOT honor the config's
// mounts, masked/readonly paths, capabilities, seccomp, rlimits, hooks or
// annotations: it applies its own standard mounts and its own Milestone-5
// hardening. So two runtimes agree on a bundle's lifecycle and on a
// process's output and exit, not on capability or mount fidelity.
package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/container"
)

// Spec is the subset of config.json MiniBox reads.
type Spec struct {
	OCIVersion string    `json:"ociVersion"`
	Process    *Process  `json:"process"`
	Root       *Root     `json:"root"`
	Hostname   string    `json:"hostname"`
	Linux      *LinuxCfg `json:"linux"`
}

type Process struct {
	Terminal bool     `json:"terminal"`
	User     User     `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type LinuxCfg struct {
	Namespaces []Namespace `json:"namespaces"`
	Resources  *Resources  `json:"resources"`
}

type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type Resources struct {
	Memory *struct {
		Limit *int64 `json:"limit"`
	} `json:"memory"`
	CPU *struct {
		Quota  *int64  `json:"quota"`
		Period *uint64 `json:"period"`
	} `json:"cpu"`
	Pids *struct {
		Limit int64 `json:"limit"`
	} `json:"pids"`
}

// defaultPath is given to a process whose config sets no PATH, so the
// command can be found by name (runc leaves PATH to the config; a bundle
// from `runc spec` always sets one).
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// LoadSpec reads and parses bundle/config.json.
func LoadSpec(bundle string) (*Spec, error) {
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	return &s, nil
}

// ToConfig translates a spec into a container.Config for the container
// id, from the bundle directory. fifo is the exec fifo path.
func (s *Spec) ToConfig(id, bundle, fifo string) (container.Config, error) {
	var cfg container.Config
	if s.Process == nil || len(s.Process.Args) == 0 {
		return cfg, fmt.Errorf("config.json: process.args is required")
	}
	if s.Process.Terminal {
		return cfg, fmt.Errorf("config.json: process.terminal true is not supported (no terminal support)")
	}
	if s.Root == nil || s.Root.Path == "" {
		return cfg, fmt.Errorf("config.json: root.path is required")
	}
	rootfs := s.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundle, rootfs)
	}

	cfg.ID = id
	cfg.Rootfs = rootfs
	cfg.Hostname = s.Hostname
	if cfg.Hostname == "" {
		cfg.Hostname = id
	}
	cfg.Args = s.Process.Args
	cfg.Cwd = s.Process.Cwd
	if cfg.Cwd == "" {
		cfg.Cwd = "/"
	}
	cfg.Env = s.Process.Env
	if _, ok := lookupEnv(cfg.Env, "PATH"); !ok {
		cfg.Env = append([]string{"PATH=" + defaultPath}, cfg.Env...)
	}
	if s.Process.User.UID != 0 || s.Process.User.GID != 0 {
		cfg.User = fmt.Sprintf("%d:%d", s.Process.User.UID, s.Process.User.GID)
	}
	cfg.ExecFifo = fifo

	// Namespaces: MiniBox always isolates uts/pid/mount/ipc/cgroup. It
	// honors the network namespace (make a private one, or share the
	// host's if the config lists none) and the user namespace. A
	// namespace that joins an existing one by path is refused.
	hasNet, hasUser := false, false
	for _, ns := range namespaces(s) {
		if ns.Path != "" {
			return cfg, fmt.Errorf("config.json: joining an existing %s namespace by path is not supported", ns.Type)
		}
		switch ns.Type {
		case "network":
			hasNet = true
		case "user":
			hasUser = true
		}
	}
	if hasNet {
		cfg.NetworkMode = "none" // a private network namespace, loopback only
	} else {
		cfg.NetworkMode = "host"
	}
	cfg.UserNS = hasUser

	limits, err := resourceLimits(s)
	if err != nil {
		return cfg, err
	}
	cfg.Limits = limits
	return cfg, nil
}

func namespaces(s *Spec) []Namespace {
	if s.Linux == nil {
		return nil
	}
	return s.Linux.Namespaces
}

// resourceLimits maps linux.resources to MiniBox's cgroup limits.
func resourceLimits(s *Spec) (cgroup.Limits, error) {
	var l cgroup.Limits
	if s.Linux == nil || s.Linux.Resources == nil {
		return l, nil
	}
	r := s.Linux.Resources
	if r.Memory != nil && r.Memory.Limit != nil && *r.Memory.Limit > 0 {
		l.Memory = *r.Memory.Limit
	}
	if r.Pids != nil && r.Pids.Limit > 0 {
		l.Pids = r.Pids.Limit
	}
	if r.CPU != nil && r.CPU.Quota != nil && *r.CPU.Quota > 0 {
		period := uint64(100000)
		if r.CPU.Period != nil && *r.CPU.Period > 0 {
			period = *r.CPU.Period
		}
		l.CPUs = float64(*r.CPU.Quota) / float64(period)
	}
	if err := l.Validate(); err != nil {
		return l, fmt.Errorf("config.json resources: %w", err)
	}
	return l, nil
}

func lookupEnv(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}
