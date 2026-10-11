//go:build linux

package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/testutil"
)

func TestCreateWritesLimitsAndRemoveCleansUp(t *testing.T) {
	testutil.RequireRoot(t)
	g, err := Create("test-"+t.Name(), Limits{Memory: 32 << 20, CPUs: 0.25, Pids: 20})
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"memory.max": "33554432\n", "memory.swap.max": "0\n", "cpu.max": "25000 100000\n", "pids.max": "20\n"} {
		if got, err := os.ReadFile(filepath.Join(g.Path, file)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", file, got, err, want)
		}
	}
	if err := g.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("group still there: %v", err)
	}
}

// A group left empty by a `minibox run` that was killed is removed by the
// next one, once it is over a minute old; a fresh one is left alone.
func TestStaleGroupsAreSwept(t *testing.T) {
	testutil.RequireRoot(t)
	parent := filepath.Join(Root, Parent)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(parent, "stale-test"), filepath.Join(parent, "fresh-test")
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	defer os.Remove(fresh)
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("cannot age a cgroup directory here: %v", err)
	}
	g, err := Create("sweeper-test", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Remove()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale group is still there (%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh group was removed: %v", err)
	}
}

// MiniBox enables its controllers in its parent group itself, rather than
// relying on an earlier run (or anyone else) having done it: in a parent
// made fresh, a group still takes every limit.
func TestControllersAreEnabledInANewParent(t *testing.T) {
	testutil.RequireRoot(t)
	old := Parent
	Parent = "minibox-test-" + strconv.Itoa(os.Getpid())
	fresh := filepath.Join(Root, Parent)
	defer func() {
		Parent = old
		_ = os.Remove(fresh)
	}()
	g, err := Create("c", Limits{Memory: 32 << 20, Pids: 20, CPUs: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(); err != nil {
		t.Fatal(err)
	}
}
