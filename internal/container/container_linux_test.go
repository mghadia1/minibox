//go:build linux

package container

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/testutil"
)

// Run starts this test binary as the container's init ("/proc/self/exe
// init"), so it must be able to act as one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		Init()
	}
	os.Exit(m.Run())
}

type result struct {
	stdout, stderr string
	code           int
	err            error
}

func config(rootfs string, args ...string) Config {
	env, err := Environment("box", "", nil, nil)
	if err != nil {
		panic(err)
	}
	// A new id each time: a container's cgroup is named after it, and
	// containers run at the same time (here and in other packages).
	return Config{ID: NewID(), Rootfs: rootfs, Hostname: "box", Args: args, Env: env, Cwd: "/"}
}

func runConfig(t *testing.T, cfg Config, stdin string, signals <-chan os.Signal) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	res, err := Run(cfg, Stdio{In: strings.NewReader(stdin), Out: &stdout, Err: &stderr}, signals)
	return result{stdout.String(), stderr.String(), res.Code, err}
}

// sh runs a shell command in a fresh container on rootfs.
func sh(t *testing.T, rootfs, script string) result {
	t.Helper()
	return runConfig(t, config(rootfs, "/bin/sh", "-c", script), "", nil)
}

func mustSucceed(t *testing.T, r result) string {
	t.Helper()
	if r.err != nil || r.code != 0 {
		t.Fatalf("exit %d, error %v\nstdout: %s\nstderr: %s", r.code, r.err, r.stdout, r.stderr)
	}
	return r.stdout
}

func TestNamespaces(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	names := []string{"uts", "pid", "mnt", "ipc", "net", "user", "cgroup"}
	out := mustSucceed(t, sh(t, rootfs, "for n in "+strings.Join(names, " ")+"; do readlink /proc/self/ns/$n; done"))
	inside := strings.Fields(out)
	if len(inside) != len(names) {
		t.Fatalf("namespaces inside: %q", out)
	}
	for i, n := range names {
		host, err := os.Readlink("/proc/self/ns/" + n)
		if err != nil {
			t.Fatal(err)
		}
		// Network and users are shared until Milestones 4 and 5.
		own := slices.Contains([]string{"uts", "pid", "mnt", "ipc", "cgroup"}, n)
		if own == (inside[i] == host) {
			t.Errorf("%s namespace: inside %s, host %s (should differ: %v)", n, inside[i], host, own)
		}
	}
}

func TestHostname(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	before, _ := os.Hostname()
	if got := strings.TrimSpace(mustSucceed(t, sh(t, rootfs, "hostname"))); got != "box" {
		t.Fatalf("hostname inside = %q", got)
	}
	r := sh(t, rootfs, "hostname changed-inside")
	mustSucceed(t, r)
	if after, _ := os.Hostname(); after != before {
		t.Fatalf("the host's hostname changed from %q to %q", before, after)
	}
}

func TestOnlyTheContainersProcesses(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	// Each process: pid and the first word of its command line.
	out := mustSucceed(t, sh(t, rootfs, `for p in /proc/[0-9]*; do printf '%s %s\n' "${p#/proc/}" "$(tr '\0' ' ' < $p/cmdline)"; done`))
	var commands []string
	for line := range strings.Lines(out) {
		pid, cmdline, _ := strings.Cut(strings.TrimSpace(line), " ")
		if pid == "1" && !strings.HasPrefix(cmdline, "minibox-init init") {
			t.Errorf("PID 1 is %q, not minibox-init", cmdline)
		}
		commands = append(commands, strings.Fields(cmdline + " -")[0])
	}
	for _, c := range commands {
		if !slices.Contains([]string{"minibox-init", "/bin/sh", "tr", "-"}, c) {
			t.Errorf("a process that is not the container's: %q (all: %q)", c, commands)
		}
	}
}

// Init leads a session of its own, so a terminal's signals go to
// `minibox run` alone. In the container's PID namespace that session is
// 1; a session led from outside would show as 0.
func TestInitLeadsItsOwnSession(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	out := mustSucceed(t, sh(t, rootfs, `read -r stat < /proc/1/stat; set -- ${stat##*) }; echo "$4"`))
	if got := strings.TrimSpace(out); got != "1" {
		t.Fatalf("init's session inside the container is %q, want 1", got)
	}
}

func TestRootIsTheRootfs(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	hostDir := t.TempDir()
	hostMarker := filepath.Join(hostDir, "host-marker")
	if err := os.WriteFile(hostMarker, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "marker"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustSucceed(t, sh(t, rootfs, "cat /marker; echo; cd /..; pwd; test -e "+hostMarker+" && echo host-visible; cat /proc/self/mountinfo"))
	lines := strings.Split(out, "\n")
	if lines[0] != "inside" || lines[1] != "/" {
		t.Fatalf("root or parent of root wrong:\n%s", out)
	}
	if strings.Contains(out, "host-visible") {
		t.Fatalf("a host file is visible inside:\n%s", out)
	}
	// Exactly the container's own mounts, and nothing else.
	var points []string
	for _, line := range lines[2:] {
		if f := strings.Fields(line); len(f) > 4 {
			points = append(points, f[4])
		}
	}
	want := []string{"/", "/proc", "/dev", "/dev/pts", "/dev/shm", "/dev/mqueue", "/sys", "/sys/fs/cgroup"}
	if !slices.Equal(points, want) {
		t.Fatalf("mount points inside = %q, want %q", points, want)
	}
}

func TestChrootEscapeFails(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	hostMarker := filepath.Join(t.TempDir(), "host-marker")
	if err := os.WriteFile(hostMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := runConfig(t, config(rootfs, "/probe", "chroot-escape", hostMarker), "", nil)
	if got := strings.TrimSpace(mustSucceed(t, r)); got != "contained" {
		t.Fatalf("probe says %q", got)
	}
}

func TestDev(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	out := mustSucceed(t, sh(t, rootfs, "ls /dev | tr '\\n' ' '; echo; echo x > /dev/null; head -c 3 /dev/zero | wc -c; stat -c '%t,%T %a' /dev/null"))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := "fd full mqueue null ptmx pts random shm stderr stdin stdout tty urandom zero"
	if strings.TrimSpace(lines[0]) != want {
		t.Fatalf("/dev holds %q, want %q", lines[0], want)
	}
	if strings.TrimSpace(lines[1]) != "3" || lines[2] != "1,3 666" {
		t.Fatalf("devices do not work as devices:\n%s", out)
	}
}

func TestSysIsReadOnly(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	r := sh(t, rootfs, "touch /sys/kernel/minibox-test")
	if r.code == 0 || !strings.Contains(r.stderr, "Read-only file system") {
		t.Fatalf("writing under /sys: exit %d, %q", r.code, r.stderr)
	}
}

func TestExitCodes(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	if err := os.WriteFile(filepath.Join(rootfs, "no-interpreter"), []byte("#!/nonexistent/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"/bin/sh", "-c", "exit 0"}, 0},
		{[]string{"/bin/sh", "-c", "exit 3"}, 3},
		{[]string{"/bin/sh", "-c", "kill -KILL $$"}, 128 + 9},
		{[]string{"/bin/sh", "-c", "kill -TERM $$"}, 128 + 15},
		{[]string{"no-such-command"}, ExitNotFound},
		{[]string{"/no/such/file"}, ExitNotFound},
		{[]string{"/no-interpreter"}, ExitNotFound},
		{[]string{"/etc/passwd"}, ExitCannotInvoke},
		{[]string{"/etc"}, ExitCannotInvoke},
	} {
		r := runConfig(t, config(rootfs, tc.args...), "", nil)
		if r.code != tc.code {
			t.Errorf("%q: exit %d, want %d (error %v, stderr %q)", tc.args, r.code, tc.code, r.err, r.stderr)
		}
		if tc.code >= ExitCannotInvoke && tc.code <= ExitNotFound {
			if _, ok := r.err.(*SetupError); !ok {
				t.Errorf("%q: error %v is not a SetupError", tc.args, r.err)
			}
		}
	}
}

func TestZombiesAreReaped(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	// The subshell exits at once, so sleep is re-parented to PID 1; when
	// it ends, only PID 1 can reap it.
	out := mustSucceed(t, sh(t, rootfs, `(sleep 0.2 &); sleep 1; for p in /proc/[0-9]*; do read -r stat < $p/stat; set -- ${stat##*) }; echo "${p#/proc/} $1"; done`))
	for line := range strings.Lines(out) {
		if strings.HasSuffix(strings.TrimSpace(line), " Z") {
			t.Fatalf("a zombie was left:\n%s", out)
		}
	}
}

// waitFor polls until path exists.
func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestSignalsReachTheCommand(t *testing.T) {
	testutil.RequireRoot(t)
	for _, tc := range []struct {
		name   string
		script string
		sig    syscall.Signal
		code   int
	}{
		// Each would end by itself in about 10 s (exit 99 or 0) if the
		// signal never arrived.
		{"trapped", `trap "exit 7" TERM; touch /ready; i=0; while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done; exit 99`, syscall.SIGTERM, 7},
		{"default action", `touch /ready; exec sleep 10`, syscall.SIGTERM, 128 + 15},
		{"interrupt", `touch /ready; exec sleep 10`, syscall.SIGINT, 128 + 2},
		{"hang-up", `trap "exit 9" HUP; touch /ready; i=0; while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done; exit 99`, syscall.SIGHUP, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootfs := testutil.Rootfs(t)
			signals := make(chan os.Signal, 1)
			go func() {
				waitFor(t, filepath.Join(rootfs, "ready"))
				signals <- tc.sig
			}()
			start := time.Now()
			r := runConfig(t, config(rootfs, "/bin/sh", "-c", tc.script), "", signals)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d (%v, %q)", r.code, tc.code, r.err, r.stderr)
			}
			if d := time.Since(start); d > 8*time.Second {
				t.Fatalf("took %v: the signal did not end it", d)
			}
		})
	}
}

// mountsUnder lists the host's mounts at or below dir.
func mountsUnder(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) > 4 && (f[4] == dir || strings.HasPrefix(f[4], dir+"/")) {
			found = append(found, f[4])
		}
	}
	return found
}

func TestMountsStayInside(t *testing.T) {
	testutil.RequireRoot(t)
	// The test means something only where / propagates mounts (systemd
	// makes it shared): there, a container that forgot to make its mounts
	// private would show them on the host.
	data, _ := os.ReadFile("/proc/self/mountinfo")
	shared := false
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) > 6 && f[4] == "/" && strings.Contains(line, " shared:") {
			shared = true
		}
	}
	if !shared {
		testutil.Require(t, "/ is not a shared mount here, so leaks could not show")
	}
	rootfs := testutil.Rootfs(t)
	done := make(chan result, 1)
	go func() {
		done <- sh(t, rootfs, "mount -t tmpfs none /mnt && touch /ready && while [ ! -e /go ]; do sleep 0.05; done")
	}()
	waitFor(t, filepath.Join(rootfs, "ready"))
	during := mountsUnder(t, rootfs)
	if err := os.WriteFile(filepath.Join(rootfs, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustSucceed(t, <-done)
	if len(during) > 0 {
		t.Fatalf("the container's mounts showed on the host: %q", during)
	}
	if after := mountsUnder(t, rootfs); len(after) > 0 {
		t.Fatalf("mounts left on the host after the container: %q", after)
	}
}

func TestEnvironmentAndWorkingDirectory(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	t.Setenv("MINIBOX_HOST_ONLY", "must-not-leak")
	cfg := config(rootfs, "/bin/sh", "-c", "pwd; env")
	cfg.Cwd = "/tmp"
	cfg.Env = append(cfg.Env, "GREETING=hello there")
	out := mustSucceed(t, runConfig(t, cfg, "", nil))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "/tmp" {
		t.Fatalf("working directory %q", lines[0])
	}
	var names []string
	for _, kv := range lines[1:] {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	slices.Sort(names)
	// PWD and SHLVL are the shell's own.
	want := []string{"GREETING", "HOME", "HOSTNAME", "PATH", "PWD", "SHLVL"}
	if !slices.Equal(names, want) {
		t.Fatalf("environment inside: %q, want exactly %q", names, want)
	}
	if !strings.Contains(out, "GREETING=hello there") {
		t.Fatalf("variable not passed:\n%s", out)
	}
}

func TestStdinAndOnlyStandardFiles(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	// The command's open files: only 0, 1 and 2 (and the directory ls
	// reads), never Init's pipes to Run.
	cfg := config(rootfs, "/bin/sh", "-c", "cat; ls /proc/self/fd | tr '\\n' ' '")
	out := mustSucceed(t, runConfig(t, cfg, "from stdin\n", nil))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "from stdin" {
		t.Fatalf("stdin not passed: %q", out)
	}
	fds := strings.Fields(lines[1])
	if len(fds) != 4 || !slices.Equal(fds[:3], []string{"0", "1", "2"}) {
		t.Fatalf("open files inside: %q (want 0 1 2 and the one ls reads)", fds)
	}
}

func TestRootfsProblemsAreRefused(t *testing.T) {
	testutil.RequireRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(rootfs string) string // returns the rootfs to use
		want  string
	}{
		{"proc is a link to the host's /etc", func(r string) string {
			must(t, os.RemoveAll(filepath.Join(r, "proc")))
			must(t, os.Symlink("/etc", filepath.Join(r, "proc")))
			return r
		}, "/proc in the root filesystem is not a directory"},
		{"sys is a file", func(r string) string {
			must(t, os.RemoveAll(filepath.Join(r, "sys")))
			must(t, os.WriteFile(filepath.Join(r, "sys"), nil, 0o644))
			return r
		}, "/sys in the root filesystem is not a directory"},
		{"the rootfs is a link", func(r string) string {
			link := filepath.Join(filepath.Dir(r), "link")
			must(t, os.Symlink(r, link))
			return link
		}, "is not a directory"},
		{"the rootfs does not exist", func(r string) string { return r + "-missing" }, "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootfs := tc.setup(testutil.Rootfs(t))
			r := runConfig(t, config(rootfs, "/bin/true"), "", nil)
			if r.code != ExitSetup || r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
				t.Fatalf("exit %d, error %v; want 125 and %q", r.code, r.err, tc.want)
			}
		})
	}
	// The host's /etc was never mounted over.
	if _, err := os.Stat("/etc/os-release"); err != nil {
		t.Fatalf("/etc on the host: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// After every test above, nothing of theirs is still running: no Init
// whose parent is this test process. (Only its own: other packages' tests
// run at the same time, with containers of their own.)
func TestNoContainerLeftRunning(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	mustSucceed(t, sh(t, rootfs, "sleep 0.1 & sleep 0.2 &"))
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	me := fmt.Sprint(os.Getpid())
	for _, e := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.HasPrefix(cmdline, []byte("minibox-init\x00")) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The fields after the command name: state, then the parent's pid.
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		if len(fields) > 1 && fields[1] == me {
			t.Errorf("an init of this test is still running: pid %s", e.Name())
		}
	}
}
