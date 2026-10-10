// Package testutil holds what MiniBox's tests share: requiring root on
// Linux (a skip, or a failure where containers must work), the test root
// filesystem, and the probe program that tries escapes from inside a
// container.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Require skips the test with reason, unless MINIBOX_TEST_REQUIRE=1 (set
// in the VM and in CI), where a skip would hide that nothing was tested:
// then it fails.
func Require(t testing.TB, reason string) {
	t.Helper()
	if os.Getenv("MINIBOX_TEST_REQUIRE") == "1" {
		t.Fatalf("required but not possible here: %s", reason)
	}
	t.Skip(reason)
}

// RequireRoot needs Linux and uid 0: containers need both.
func RequireRoot(t testing.TB) {
	t.Helper()
	if runtime.GOOS != "linux" {
		Require(t, "containers need Linux")
	}
	if os.Geteuid() != 0 {
		Require(t, "containers need root")
	}
}

// Rootfs unpacks the test root filesystem (MINIBOX_TEST_ROOTFS, Alpine's
// minirootfs from scripts/rootfs.sh) into a new temporary directory and
// returns it. Unpacked with the system's tar: the tests' own tool, not
// MiniBox's.
func Rootfs(t testing.TB) string {
	t.Helper()
	archive := os.Getenv("MINIBOX_TEST_ROOTFS")
	if archive == "" {
		Require(t, "MINIBOX_TEST_ROOTFS is not set (scripts/rootfs.sh)")
	}
	dir := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-xzf", archive, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("unpacking %s: %v\n%s", archive, err, out)
	}
	return dir
}

var (
	probeOnce sync.Once
	probePath string
	probeErr  error
)

// Probe builds the probe program (internal/testutil/probe) once, static,
// so it runs in any root filesystem, and copies it into rootfs as /probe.
func Probe(t testing.TB, rootfs string) {
	t.Helper()
	probeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "minibox-probe")
		if err != nil {
			probeErr = err
			return
		}
		probePath = filepath.Join(dir, "probe")
		cmd := exec.Command("go", "build", "-o", probePath, "github.com/mghadia1/minibox/internal/testutil/probe")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = err
			probePath = string(out)
		}
	})
	if probeErr != nil {
		t.Fatalf("building the probe: %v\n%s", probeErr, probePath)
	}
	data, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "probe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
}
