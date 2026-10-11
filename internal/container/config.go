// Package container runs a command in a container: new namespaces, a root
// filesystem of its own reached with pivot_root, its own /proc, /dev and
// /sys, and an init as PID 1.
//
// Two processes take part. Run, in `minibox run`, starts a copy of the
// minibox binary ("/proc/self/exe init") in new namespaces and sends it
// the Config through a pipe. That copy, Init, is PID 1 in the container:
// it sets up the root filesystem, starts the command as PID 2, reaps every
// process that exits in the container, passes signals on to the command,
// and exits with the command's status.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/network"
)

// Config is everything Init needs, sent from Run as JSON.
type Config struct {
	ID       string   `json:"id"`
	Rootfs   string   `json:"rootfs"`   // absolute path on the host
	Hostname string   `json:"hostname"` // the container's, in its own UTS namespace
	Args     []string `json:"args"`     // the command and its arguments
	Env      []string `json:"env"`      // KEY=value, the command's whole environment
	Cwd      string   `json:"cwd"`      // absolute, inside the container

	// An image's layers, when set: Init mounts them as an overlay on
	// Rootfs (an empty folder) before anything else. Lower is top layer
	// first, read-only; Upper takes the container's writes.
	Lower []string `json:"lower,omitempty"`
	Upper string   `json:"upper,omitempty"`
	Work  string   `json:"work,omitempty"`

	// User runs the command as "name", "uid", "name:group" or "uid:gid",
	// looked up in the container's own /etc/passwd and /etc/group. Empty
	// is root.
	User string `json:"user,omitempty"`

	// Limits for the container's cgroup, applied by Run before Init
	// starts (Init never sees them).
	Limits cgroup.Limits `json:"-"`

	// NetworkMode is bridge (the default), none or host; Ports are
	// published on the bridge. Run attaches the container and fills in
	// Network, which Init sets up inside.
	NetworkMode network.Mode   `json:"-"`
	Ports       []network.Port `json:"-"`
	Network     network.Inside `json:"network"`
}

// Exit codes for a container that could not be run, as Docker gives them:
// 125 when MiniBox itself failed, 126 when the command could not be
// invoked, 127 when it was not found. Otherwise the command's own status,
// or 128 + the signal that ended it.
const (
	ExitSetup         = 125
	ExitCannotInvoke  = 126
	ExitNotFound      = 127
	exitSignalBase    = 128
	defaultPath       = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	hostnameMaxLength = 64 // HOST_NAME_MAX
)

// NewID returns a random container id: 12 hex digits, as Docker shows them.
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b)
}

// Environment builds the command's environment: a default PATH unless
// the image has one, the image's variables, HOSTNAME, TERM if the caller
// has one, then the variables asked for (KEY=value), each replacing one
// of the same name. HOME is added by Init, from the user's passwd entry.
// The host's own environment is never passed in.
func Environment(hostname, term string, image, extra []string) ([]string, error) {
	var env []string
	if _, ok := lookupEnv(image, "PATH"); !ok {
		env = append(env, "PATH="+defaultPath)
	}
	for _, kv := range image {
		if name, _, ok := strings.Cut(kv, "="); ok && name != "" {
			env = setEnv(env, kv)
		}
	}
	env = setEnv(env, "HOSTNAME="+hostname)
	if term != "" {
		env = setEnv(env, "TERM="+term)
	}
	for _, kv := range extra {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsRune(name, 0) {
			return nil, fmt.Errorf("environment variable %q is not KEY=value", kv)
		}
		env = setEnv(env, kv)
	}
	return env, nil
}

func setEnv(env []string, kv string) []string {
	name, _, _ := strings.Cut(kv, "=")
	for i, old := range env {
		if oldName, _, _ := strings.Cut(old, "="); oldName == name {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}

// Validate checks a Config before anything is started.
func (c Config) Validate() error {
	switch {
	case len(c.Args) == 0 || c.Args[0] == "":
		return errors.New("no command to run")
	case !path.IsAbs(c.Rootfs):
		return fmt.Errorf("root filesystem %q is not an absolute path", c.Rootfs)
	case path.Clean(c.Rootfs) == "/":
		// pivot_root to the host's own root would be no container at all.
		return errors.New("the root filesystem cannot be the host's /")
	case !path.IsAbs(c.Cwd):
		return fmt.Errorf("working directory %q is not an absolute path", c.Cwd)
	case c.Hostname == "" || len(c.Hostname) > hostnameMaxLength:
		return fmt.Errorf("hostname %q must be 1 to %d characters", c.Hostname, hostnameMaxLength)
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if c.NetworkMode != "" && c.NetworkMode != network.Bridge && len(c.Ports) > 0 {
		return fmt.Errorf("ports can be published only on the bridge network, not %q", c.NetworkMode)
	}
	if len(c.Lower) > 0 {
		// overlayfs reads its options as "lowerdir=a:b,upperdir=...".
		for _, p := range append([]string{c.Upper, c.Work}, c.Lower...) {
			if !path.IsAbs(p) || strings.ContainsAny(p, ":,") {
				return fmt.Errorf("overlay folder %q must be an absolute path without ':' or ','", p)
			}
		}
	}
	return nil
}

// lookupEnv returns a variable's value from a KEY=value list.
func lookupEnv(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}
