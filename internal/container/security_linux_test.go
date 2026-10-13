//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mghadia1/minibox/internal/testutil"
)

// makeOthersCanReach adds other-read-and-execute up the directory chain
// from path (inclusive) to the temp root, so a mapped, unprivileged uid
// can reach it. A --userns container's root runs as a high host uid, but
// `go test` leaves the test binary and the temp rootfs in 0700
// directories only root can enter; the real minibox binary and its
// rootfs live at world-reachable paths, so this is a test-only fixup.
func makeOthersCanReach(t *testing.T, path string) {
	t.Helper()
	for d := path; d != "/"; {
		info, err := os.Stat(d)
		if err != nil {
			break
		}
		_ = os.Chmod(d, info.Mode()|0o055)
		if d == os.TempDir() {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
}

// makeSelfExecutable lets that uid exec this test binary (see above).
func makeSelfExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	makeOthersCanReach(t, filepath.Dir(exe))
}

// wantCapMask is the effective-capability mask a container should have:
// one bit per capability in DefaultCaps. The single source of truth is
// DefaultCaps, so the test cannot drift from the set the code applies.
func wantCapMask() uint64 {
	var mask uint64
	for _, c := range DefaultCaps {
		mask |= 1 << c
	}
	return mask
}

// parseCaps reads "caps=0x.. bnd=0x.. nnp=.. seccomp=.." from `probe caps`.
func parseCaps(t *testing.T, out string) (eff, bnd uint64, nnp, seccomp string) {
	t.Helper()
	hex := func(v string) uint64 {
		n, err := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64)
		if err != nil {
			t.Fatalf("capability value %q: %v", v, err)
		}
		return n
	}
	for _, f := range strings.Fields(strings.TrimSpace(out)) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "caps":
			eff = hex(v)
		case "bnd":
			bnd = hex(v)
		case "nnp":
			nnp = v
		case "seccomp":
			seccomp = v
		}
	}
	return eff, bnd, nnp, seccomp
}

// A container's root keeps exactly DefaultCaps, has no_new_privs set, and
// runs under a seccomp filter (mode 2).
func TestCapabilitiesNoNewPrivsAndSeccomp(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	out := mustSucceed(t, runConfig(t, config(rootfs, "/probe", "caps"), "", nil))
	eff, bnd, nnp, seccomp := parseCaps(t, out)
	if eff != wantCapMask() {
		t.Errorf("effective caps 0x%x, want 0x%x (Docker's default set minus NET_RAW and MKNOD)", eff, wantCapMask())
	}
	if bnd != wantCapMask() {
		t.Errorf("bounding set 0x%x, want 0x%x (so no capability can be regained by execing a setuid or file-capability binary)", bnd, wantCapMask())
	}
	if nnp != "1" {
		t.Errorf("no_new_privs is %q, want 1", nnp)
	}
	if seccomp != "2" {
		t.Errorf("seccomp mode is %q, want 2 (filter)", seccomp)
	}
}

// The command, run as a non-root user (--user), has no capabilities at
// all: dropping to a user sheds them, which is the point of --user and
// matches Docker, whose default does not add ambient caps for a user.
func TestNonRootHasNoCapabilities(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	cfg := config(rootfs, "/probe", "caps")
	cfg.User = "nobody"
	out := mustSucceed(t, runConfig(t, cfg, "", nil))
	if eff, _, _, _ := parseCaps(t, out); eff != 0 {
		t.Errorf("nobody's effective caps 0x%x, want 0", eff)
	}
}

// The escapes the milestone names all fail from inside a container.
func TestKnownEscapesAreBlocked(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"mount", []string{"/probe", "mount"}, "mount: blocked"},
		{"open_by_handle_at", []string{"/probe", "open-by-handle"}, "open-by-handle: blocked"},
		{"load a kernel module", []string{"/probe", "module"}, "module: blocked"},
		{"mknod a block device", []string{"/probe", "mknod-disk"}, "mknod-disk: blocked"},
		{"a raw or packet socket", []string{"/probe", "raw-socket"}, "raw-socket: blocked"},
		{"write /proc/sysrq-trigger", []string{"/probe", "write", "/proc/sysrq-trigger"}, "write: blocked"},
		// keyctl needs no capability, so only the seccomp filter stops it:
		// this proves the filter blocks something the dropped caps do not.
		{"keyctl (seccomp only)", []string{"/probe", "keyctl"}, "keyctl: blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := mustSucceed(t, runConfig(t, config(rootfs, tc.args...), "", nil))
			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Masked /proc paths read as empty; read-only /proc paths still read.
func TestMaskedAndReadonlyProcPaths(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	// /proc/kcore (the kernel's memory) is masked with /dev/null: 0 bytes.
	out := mustSucceed(t, runConfig(t, config(rootfs, "/probe", "read-len", "/proc/kcore"), "", nil))
	if got := strings.TrimSpace(out); got != "read-len: 0" {
		t.Errorf("reading masked /proc/kcore: %q, want read-len: 0", got)
	}
	// /proc/sys is read-only but still readable.
	out = mustSucceed(t, runConfig(t, config(rootfs, "/bin/cat", "/proc/sys/kernel/ostype"), "", nil))
	if got := strings.TrimSpace(out); got != "Linux" {
		t.Errorf("reading read-only /proc/sys/kernel/ostype: %q, want Linux", got)
	}
}

// Running an image under a user namespace is refused with a clear
// message, since the image store is the host root's.
func TestUserNSWithImageRefused(t *testing.T) {
	cfg := Config{
		ID: NewID(), Rootfs: "/x", Hostname: "box", Cwd: "/",
		Args: []string{"/bin/true"}, UserNS: true,
		Lower: []string{"/a"}, Upper: "/u", Work: "/w",
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not supported with an image") {
		t.Errorf("--userns with an image: Validate = %v, want it refused", err)
	}
}

// With --userns the container's root maps to an unprivileged host uid, a
// device node works (bind-mounted, since mknod cannot), and the command
// still sees itself as root; the capability and seccomp hardening still
// applies, so even the mapped root cannot mount.
func TestUserNamespace(t *testing.T) {
	testutil.RequireRoot(t)
	makeSelfExecutable(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	makeOthersCanReach(t, rootfs)

	cfg := config(rootfs, "/bin/sh", "-c", "id -u; cat /proc/self/uid_map; echo x > /dev/null && echo devok")
	cfg.UserNS = true
	out := mustSucceed(t, runConfig(t, cfg, "", nil))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("output %q", out)
	}
	if lines[0] != "0" {
		t.Errorf("id -u inside is %q, want 0 (container root)", lines[0])
	}
	if !strings.Contains(lines[1], fmt.Sprint(userNSBase)) || !strings.Contains(lines[1], fmt.Sprint(userNSSize)) {
		t.Errorf("uid_map %q, want 0 -> %d, size %d", lines[1], userNSBase, userNSSize)
	}
	if lines[len(lines)-1] != "devok" {
		t.Errorf("/dev/null did not work in the user namespace: %q", out)
	}

	// The hardening does not depend on the user namespace: the mapped
	// root has CAP_SYS_ADMIN over its own namespaces, but MiniBox has
	// dropped it, so a mount is still refused.
	cfg2 := config(rootfs, "/probe", "mount")
	cfg2.UserNS = true
	out2 := mustSucceed(t, runConfig(t, cfg2, "", nil))
	if got := strings.TrimSpace(out2); got != "mount: blocked" {
		t.Errorf("mount in a user namespace: %q, want mount: blocked", got)
	}
}
