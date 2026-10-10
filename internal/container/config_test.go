package container

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnvironment(t *testing.T) {
	env, err := Environment("box", "xterm", nil, []string{"A=1", "PATH=/bin", "EMPTY="})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/bin", "HOSTNAME=box", "TERM=xterm", "A=1", "EMPTY="}
	if !slices.Equal(env, want) {
		t.Fatalf("Environment = %q, want %q", env, want)
	}
	if env, _ := Environment("box", "", nil, nil); slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "TERM=") }) {
		t.Fatalf("no TERM given, but got %q", env)
	}
	for _, bad := range []string{"NOVALUE", "=x", "A\x00B=1"} {
		if _, err := Environment("box", "", nil, []string{bad}); err == nil {
			t.Errorf("Environment accepted %q", bad)
		}
	}
	// An image's own PATH replaces the default; its variables come before
	// HOSTNAME and the caller's, which replace them.
	env, err = Environment("box", "", []string{"PATH=/opt/bin", "LANG=C.UTF-8", "HOSTNAME=image", "A=image"}, []string{"A=caller"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"PATH=/opt/bin", "LANG=C.UTF-8", "HOSTNAME=box", "A=caller"}
	if !slices.Equal(env, want) {
		t.Fatalf("with an image: %q, want %q", env, want)
	}
}

func TestValidate(t *testing.T) {
	good := Config{ID: "abc", Rootfs: "/var/lib/x", Hostname: "abc", Args: []string{"sh"}, Cwd: "/"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"no command":        func(c *Config) { c.Args = nil },
		"empty command":     func(c *Config) { c.Args = []string{""} },
		"relative rootfs":   func(c *Config) { c.Rootfs = "rootfs" },
		"host root":         func(c *Config) { c.Rootfs = "/" },
		"host root, dotted": func(c *Config) { c.Rootfs = "/tmp/.." },
		"relative cwd":      func(c *Config) { c.Cwd = "tmp" },
		"no hostname":       func(c *Config) { c.Hostname = "" },
		"long hostname":     func(c *Config) { c.Hostname = strings.Repeat("h", 65) },
		"relative layer":    func(c *Config) { c.Lower, c.Upper, c.Work = []string{"l"}, "/u", "/w" },
		"colon in a layer":  func(c *Config) { c.Lower, c.Upper, c.Work = []string{"/l:x"}, "/u", "/w" },
		"comma in upper":    func(c *Config) { c.Lower, c.Upper, c.Work = []string{"/l"}, "/u,x", "/w" },
	} {
		c := good
		change(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 12 || a == b {
		t.Fatalf("ids %q and %q", a, b)
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	other := filepath.Join(dir, "other")
	for _, d := range []string{bin, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "tool"), 0o755)
	write(filepath.Join(other, "plain"), 0o644)
	write(filepath.Join(bin, "plain"), 0o755)
	write(filepath.Join(other, "data"), 0o644)
	pathEnv := other + ":" + bin

	for _, tc := range []struct {
		name string
		want string
		err  error
	}{
		{"tool", filepath.Join(bin, "tool"), nil},
		{"plain", filepath.Join(bin, "plain"), nil}, // not executable in other: keep looking
		{"data", "", errNotExecutable},
		{"missing", "", errNotFound},
		{filepath.Join(bin, "tool"), filepath.Join(bin, "tool"), nil},
		{filepath.Join(other, "data"), filepath.Join(other, "data"), errNotExecutable},
		{"./nowhere/x", "./nowhere/x", errNotFound},
		{bin, bin, errNotExecutable}, // a directory
	} {
		got, err := lookPath(tc.name, pathEnv)
		if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
			t.Errorf("lookPath(%q) = %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestLookupEnv(t *testing.T) {
	env := []string{"A=1", "B=2", "A=3"}
	if v, ok := lookupEnv(env, "A"); !ok || v != "3" {
		t.Fatalf("lookupEnv A = %q, %v", v, ok)
	}
	if _, ok := lookupEnv(env, "C"); ok {
		t.Fatal("found C")
	}
}
