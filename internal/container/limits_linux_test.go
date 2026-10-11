//go:build linux

package container

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/testutil"
)

func runLimited(t *testing.T, rootfs string, limits cgroup.Limits, args ...string) (result, Result) {
	t.Helper()
	cfg := config(rootfs, args...)
	cfg.ID = NewID()
	cfg.Limits = limits
	var stdout, stderr bytes.Buffer
	res, err := Run(cfg, Stdio{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, nil)
	if _, statErr := os.Stat(filepath.Join(cgroup.Root, cgroup.Parent, cfg.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the container's cgroup is still there after it exited (%v)", statErr)
	}
	return result{stdout.String(), stderr.String(), res.Code, err}, res
}

func TestTheContainerSeesItsOwnCgroup(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	r, _ := runLimited(t, rootfs, cgroup.Limits{Memory: 64 << 20, Pids: 50, CPUs: 1.5},
		"/bin/sh", "-c", "cat /proc/self/cgroup /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/pids.max /sys/fs/cgroup/cpu.max; echo 1 > /sys/fs/cgroup/pids.max && echo writable; true")
	want := "0::/\n67108864\n0\n50\n150000 100000\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("exit %d, stdout %q, want %q (stderr %q)", r.code, r.stdout, want, r.stderr)
	}
	if !strings.Contains(r.stderr, "Read-only file system") {
		t.Fatalf("the container could write its own limits: %q", r.stderr)
	}
}

func TestMemoryLimit(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	r, res := runLimited(t, rootfs, cgroup.Limits{Memory: 64 << 20}, "/probe", "alloc", "200")
	if r.code != 128+9 || res.Usage.OOMKills < 1 {
		t.Fatalf("200 MiB in a 64 MiB container: exit %d, %d OOM kills (%q)", r.code, res.Usage.OOMKills, r.stdout)
	}
	r, res = runLimited(t, rootfs, cgroup.Limits{Memory: 64 << 20}, "/probe", "alloc", "24")
	if r.code != 0 || res.Usage.OOMKills != 0 || res.Usage.MemoryPeak < 24<<20 || res.Usage.MemoryPeak > 64<<20 {
		t.Fatalf("24 MiB in a 64 MiB container: exit %d, peak %d, %d OOM kills", r.code, res.Usage.MemoryPeak, res.Usage.OOMKills)
	}
}

func TestPidsLimit(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	r, res := runLimited(t, rootfs, cgroup.Limits{Pids: 32}, "/probe", "spawn", "100")
	var started int
	if f := strings.Fields(r.stdout); len(f) > 1 {
		started, _ = strconv.Atoi(f[1])
	}
	if r.code != 0 || started == 0 || started >= 32 || !strings.Contains(r.stdout, "resource temporarily unavailable") || res.Usage.PidsRefused < 1 {
		t.Fatalf("100 processes under a limit of 32: exit %d, %q, %d refused", r.code, r.stdout, res.Usage.PidsRefused)
	}
}

// A real fork bomb: every copy starts two more. It is contained by the
// pids limit, and gone with the container.
func TestForkBombIsContained(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	start := time.Now()
	r, res := runLimited(t, rootfs, cgroup.Limits{Pids: 64}, "/bin/sh", "-c", "f() { f | f & }; f; sleep 2; echo survived")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "survived" || res.Usage.PidsRefused < 1 {
		t.Fatalf("fork bomb: exit %d, %q, %d forks refused (stderr %d bytes)", r.code, r.stdout, res.Usage.PidsRefused, len(r.stderr))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Half a CPU for 3 s of wall time is about 1.5 s of CPU, however hard the
// program spins. (The upper bound is the limit; the lower one is loose,
// since other tests may share the VM's two CPUs.)
func TestCPULimit(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	r, res := runLimited(t, rootfs, cgroup.Limits{CPUs: 0.5}, "/probe", "spin", "3")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if cpu := res.Usage.CPUSeconds; cpu > 1.5*1.15 || cpu < 1.5*0.5 || res.Usage.Throttled < 0.5 {
		t.Fatalf("CPU used %.2f s, held back %.2f s: want about 1.5 s used", cpu, res.Usage.Throttled)
	}
}

func TestBadLimitsAreRefused(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	r, _ := runLimited(t, rootfs, cgroup.Limits{Memory: 1 << 20}, "/bin/true")
	if r.code != ExitSetup || r.err == nil {
		t.Fatalf("a 1 MiB limit: exit %d, %v", r.code, r.err)
	}
}
