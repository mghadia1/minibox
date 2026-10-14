//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Root is where the cgroup v2 hierarchy is mounted.
var Root = "/sys/fs/cgroup"

// Parent is the group every container's group goes in: Root/minibox/ID.
// (A variable so that a test can start from a parent that does not exist.)
var Parent = "minibox"

// controllers MiniBox enables for its groups.
var controllers = []string{"cpu", "memory", "pids"}

// Group is one container's cgroup.
type Group struct {
	Path string
	fd   int
}

// Create makes the container's group, writes its limits, and opens it for
// clone3's CLONE_INTO_CGROUP: the container's init is born inside it, so
// no process of the container ever runs outside its limits.
func Create(id string, l Limits) (*Group, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	parent := filepath.Join(Root, Parent)
	if err := enable(Root); err != nil {
		return nil, err
	}
	if err := os.Mkdir(parent, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := enable(parent); err != nil {
		return nil, err
	}
	sweep(parent)
	dir := filepath.Join(parent, id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	g := &Group{Path: dir, fd: -1}
	for _, f := range l.files() {
		if err := os.WriteFile(filepath.Join(dir, f[0]), []byte(f[1]), 0); err != nil {
			g.Remove()
			return nil, fmt.Errorf("cgroup %s: %w", f[0], err)
		}
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		g.Remove()
		return nil, err
	}
	g.fd = fd
	return g, nil
}

// FD is the group's directory, for SysProcAttr.CgroupFD.
func (g *Group) FD() int { return g.fd }

// RemoveByID removes a container's group by id, killing anything still in
// it. For a process that did not create the group (`minibox delete`,
// which runs after `minibox create` has exited).
func RemoveByID(id string) error {
	g := &Group{Path: filepath.Join(Root, Parent, id), fd: -1}
	return g.Remove()
}

// enable turns on MiniBox's controllers for the children of dir. A cgroup
// v2 group can use a controller only if its parent enables it for its
// children ("subtree_control").
func enable(dir string) error {
	have, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	enabled := strings.Fields(string(have))
	for _, c := range controllers {
		if slices.Contains(enabled, c) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+c), 0); err != nil {
			return fmt.Errorf("enabling the %s controller in %s: %w", c, dir, err)
		}
	}
	return nil
}

// sweep removes groups left by containers whose `minibox run` was killed
// before it could remove them: empty, and more than a minute old (a group
// just made for a container that is starting is empty too).
func sweep(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		if !populated(dir) {
			_ = unix.Rmdir(dir)
		}
	}
}

// populated reads cgroup.events: does the group (or a child) hold a process?
func populated(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	return keyValues(string(data))["populated"] != 0
}

// Usage reads what the container used. Call it once the container has
// exited and before Remove.
func (g *Group) Usage() Usage {
	read := func(name string) map[string]int64 {
		data, _ := os.ReadFile(filepath.Join(g.Path, name))
		return keyValues(string(data))
	}
	var u Usage
	u.OOMKills = read("memory.events")["oom_kill"]
	if data, err := os.ReadFile(filepath.Join(g.Path, "memory.peak")); err == nil {
		fmt.Sscan(string(data), &u.MemoryPeak)
	}
	cpu := read("cpu.stat")
	u.CPUSeconds = float64(cpu["usage_usec"]) / 1e6
	u.Throttled = float64(cpu["throttled_usec"]) / 1e6
	u.PidsRefused = read("pids.events")["max"]
	return u
}

// Remove kills anything left in the group (cgroup.kill), waits until it
// is empty, and removes it.
func (g *Group) Remove() error {
	if g.fd >= 0 {
		unix.Close(g.fd)
		g.fd = -1
	}
	_ = os.WriteFile(filepath.Join(g.Path, "cgroup.kill"), []byte("1"), 0)
	// The kernel refuses to remove a group (EBUSY) while anything in it is
	// still being torn down, which can outlast "populated 0" for a moment:
	// keep trying until the deadline.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if populated(g.Path) {
			err = errors.New("it still has processes")
		} else if err = unix.Rmdir(g.Path); err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		} else if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("removing cgroup %s: %w", g.Path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("removing cgroup %s after 5 s: %w", g.Path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
