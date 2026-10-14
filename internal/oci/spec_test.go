package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundle writes a config.json (from the given spec map) into a new
// bundle directory with an empty rootfs, and returns the bundle path.
func writeBundle(t *testing.T, spec map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// minimalSpec is a valid config with the given args.
func minimalSpec(args ...string) map[string]any {
	return map[string]any{
		"ociVersion": "1.0.2",
		"process":    map[string]any{"args": args, "cwd": "/"},
		"root":       map[string]any{"path": "rootfs"},
		"hostname":   "box",
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"}, {"type": "mount"}, {"type": "ipc"},
				{"type": "uts"}, {"type": "cgroup"}, {"type": "network"},
			},
		},
	}
}

func TestToConfigReadsTheSpec(t *testing.T) {
	spec := minimalSpec("/bin/sh", "-c", "echo hi")
	spec["process"].(map[string]any)["env"] = []string{"FOO=bar"}
	spec["process"].(map[string]any)["user"] = map[string]any{"uid": 65534, "gid": 65534}
	spec["linux"].(map[string]any)["resources"] = map[string]any{
		"memory": map[string]any{"limit": 134217728},
		"pids":   map[string]any{"limit": 100},
		"cpu":    map[string]any{"quota": 50000, "period": 100000},
	}
	bundle := writeBundle(t, spec)
	s, err := LoadSpec(bundle)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.ToConfig("c1", bundle, "/run/fifo")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rootfs != filepath.Join(bundle, "rootfs") {
		t.Errorf("rootfs %q", cfg.Rootfs)
	}
	if cfg.Hostname != "box" || cfg.Cwd != "/" || cfg.ExecFifo != "/run/fifo" {
		t.Errorf("hostname/cwd/fifo: %q %q %q", cfg.Hostname, cfg.Cwd, cfg.ExecFifo)
	}
	if cfg.User != "65534:65534" {
		t.Errorf("user %q, want 65534:65534", cfg.User)
	}
	if _, ok := lookupEnv(cfg.Env, "PATH"); !ok {
		t.Error("a default PATH should be added when the spec sets none")
	}
	if v, _ := lookupEnv(cfg.Env, "FOO"); v != "bar" {
		t.Error("the spec's env was not kept")
	}
	if cfg.NetworkMode != "none" {
		t.Errorf("a network namespace should give a private network, got %q", cfg.NetworkMode)
	}
	if cfg.Limits.Memory != 134217728 || cfg.Limits.Pids != 100 || cfg.Limits.CPUs != 0.5 {
		t.Errorf("resources: %+v", cfg.Limits)
	}
}

func TestToConfigHostNetworkWhenNoNetNamespace(t *testing.T) {
	spec := minimalSpec("/bin/true")
	// Drop the network namespace.
	spec["linux"].(map[string]any)["namespaces"] = []map[string]any{{"type": "pid"}, {"type": "mount"}}
	bundle := writeBundle(t, spec)
	s, _ := LoadSpec(bundle)
	cfg, err := s.ToConfig("c1", bundle, "f")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkMode != "host" {
		t.Errorf("no network namespace should share the host's, got %q", cfg.NetworkMode)
	}
}

func TestToConfigHostnameDefaultsToID(t *testing.T) {
	spec := minimalSpec("/bin/true")
	delete(spec, "hostname")
	bundle := writeBundle(t, spec)
	s, _ := LoadSpec(bundle)
	cfg, err := s.ToConfig("abc123", bundle, "f")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "abc123" {
		t.Errorf("hostname %q, want the id", cfg.Hostname)
	}
}

func TestToConfigRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(map[string]any)
		want string
	}{
		{"a terminal", func(s map[string]any) { s["process"].(map[string]any)["terminal"] = true }, "terminal"},
		{"no args", func(s map[string]any) { s["process"].(map[string]any)["args"] = []string{} }, "args is required"},
		{"no root", func(s map[string]any) { delete(s, "root") }, "root.path"},
		{"a namespace path", func(s map[string]any) {
			s["linux"].(map[string]any)["namespaces"] = []map[string]any{{"type": "network", "path": "/proc/1/ns/net"}}
		}, "joining an existing"},
		{"a tiny memory limit", func(s map[string]any) {
			s["linux"].(map[string]any)["resources"] = map[string]any{"memory": map[string]any{"limit": 1024}}
		}, "memory limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalSpec("/bin/true")
			tc.mut(spec)
			bundle := writeBundle(t, spec)
			s, err := LoadSpec(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ToConfig("c1", bundle, "f"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ToConfig = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestParseSignalInvalidName(t *testing.T) {
	// A cross-platform check of the id validator, which the CLI relies on.
	for _, bad := range []string{"", "a/b", ".", "..", "x\x00y"} {
		if err := validID(bad); err == nil {
			t.Errorf("validID(%q) should fail", bad)
		}
	}
	for _, ok := range []string{"c1", "my-container", "abc123"} {
		if err := validID(ok); err != nil {
			t.Errorf("validID(%q) = %v", ok, err)
		}
	}
}
