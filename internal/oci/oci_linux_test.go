//go:build linux

package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/container"
	"github.com/mghadia1/minibox/internal/testutil"
)

// This test binary stands in for the minibox binary: CreateDetached
// re-execs /proc/self/exe with "init", which must run container.Init.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		container.Init()
	}
	os.Exit(m.Run())
}

var idCounter int64

// newBundle writes a bundle whose root filesystem is a fresh Alpine tree
// and whose config runs args, and returns the bundle path and a unique
// container id (cleaned up at the end of the test).
func newBundle(t *testing.T, resources map[string]any, args ...string) (bundle, id string) {
	t.Helper()
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t) // an absolute Alpine directory
	bundle = t.TempDir()
	linux := map[string]any{
		"namespaces": []map[string]any{
			{"type": "pid"}, {"type": "mount"}, {"type": "ipc"},
			{"type": "uts"}, {"type": "cgroup"}, {"type": "network"},
		},
	}
	if resources != nil {
		linux["resources"] = resources
	}
	spec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"args": args, "cwd": "/",
			"env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		},
		"root":     map[string]any{"path": rootfs},
		"hostname": "box",
		"linux":    linux,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	id = fmt.Sprintf("mbtest-%d-%d", os.Getpid(), atomic.AddInt64(&idCounter, 1))
	t.Cleanup(func() { _ = Delete(id, true) })
	return bundle, id
}

func waitStopped(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := GetState(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status == StatusStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("container %s did not stop", id)
}

func waitLog(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(logPath(id)); strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(logPath(id))
	t.Fatalf("container %s log never contained %q:\n%s", id, want, data)
}

// The whole lifecycle: create leaves it "created", start runs the
// command, it reaches "stopped", and delete removes it.
func TestLifecycle(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/sh", "-c", "echo hello-oci; hostname; id -u")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	st, err := GetState(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusCreated || st.Pid <= 0 {
		t.Fatalf("after create: status %q, pid %d, want created with a pid", st.Status, st.Pid)
	}
	if st.OCIVersion == "" || st.ID != id || st.Bundle != bundle {
		t.Fatalf("state fields: %+v", st)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, id)
	log, _ := os.ReadFile(logPath(id))
	for _, want := range []string{"hello-oci", "box", "0"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log %q missing %q", log, want)
		}
	}
	if err := Delete(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := GetState(id); err == nil {
		t.Error("the container still exists after delete")
	}
}

// A created container that is never started can be deleted (the init,
// blocked on the fifo, is killed).
func TestDeleteBeforeStart(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/true")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if st, _ := GetState(id); st.Status != StatusCreated {
		t.Fatalf("status %q, want created", st.Status)
	}
	if err := Delete(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := GetState(id); err == nil {
		t.Error("the container still exists after delete")
	}
}

// start refuses a container that is not in the created state.
func TestStartTwiceRefused(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/sh", "-c", "echo up; while true; do sleep 0.1; done")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitLog(t, id, "up")
	if st, _ := GetState(id); st.Status != StatusRunning {
		t.Fatalf("status %q, want running", st.Status)
	}
	if err := Start(id); err == nil {
		t.Error("starting a running container should be refused")
	}
	_ = Kill(id, "KILL")
	waitStopped(t, id)
}

// kill signals the container's process; delete on a stopped one works.
func TestKill(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/sh", "-c", "echo up; while true; do sleep 0.1; done")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitLog(t, id, "up")
	if err := Kill(id, "KILL"); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, id)
	if err := Kill(id, "TERM"); err == nil {
		t.Error("killing a stopped container should be refused")
	}
	if err := Delete(id, false); err != nil {
		t.Fatalf("delete of a stopped container: %v", err)
	}
}

// delete refuses a running container unless forced.
func TestDeleteRunningRefusedUnlessForced(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/sh", "-c", "echo up; while true; do sleep 0.1; done")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitLog(t, id, "up")
	if err := Delete(id, false); err == nil {
		t.Error("delete of a running container should be refused without --force")
	}
	if err := Delete(id, true); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
	if _, err := GetState(id); err == nil {
		t.Error("the container still exists after a forced delete")
	}
}

// The config's cgroup resources are applied: the container sees its own
// memory limit.
func TestResourcesApplied(t *testing.T) {
	resources := map[string]any{"memory": map[string]any{"limit": 67108864}}
	bundle, id := newBundle(t, resources, "/bin/cat", "/sys/fs/cgroup/memory.max")
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, id)
	log, _ := os.ReadFile(logPath(id))
	if strings.TrimSpace(string(log)) != "67108864" {
		t.Errorf("memory.max inside = %q, want 67108864", log)
	}
}

// The process runs in the config's working directory.
func TestWorkingDirectory(t *testing.T) {
	bundle, id := newBundle(t, nil, "/bin/pwd")
	// Rewrite the bundle's cwd to /tmp.
	data, _ := os.ReadFile(filepath.Join(bundle, "config.json"))
	data = []byte(strings.Replace(string(data), `"cwd":"/"`, `"cwd":"/tmp"`, 1))
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Create(id, bundle); err != nil {
		t.Fatal(err)
	}
	if err := Start(id); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, id)
	log, _ := os.ReadFile(logPath(id))
	if strings.TrimSpace(string(log)) != "/tmp" {
		t.Errorf("pwd = %q, want /tmp (the config's cwd)", log)
	}
}

// Two commands that do not exist or a bad bundle are reported by create.
func TestCreateErrors(t *testing.T) {
	testutil.RequireRoot(t)
	if err := Create("mbtest-nobundle", t.TempDir()); err == nil {
		t.Error("a bundle with no config.json should fail")
	}
	bundle, id := newBundle(t, nil, "/no/such/command")
	if err := Create(id, bundle); err == nil {
		_ = Delete(id, true)
		t.Error("a command that does not exist should fail at create")
	}
}
