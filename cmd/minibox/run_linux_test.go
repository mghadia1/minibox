//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/container"
	"github.com/mghadia1/minibox/internal/network"
	"github.com/mghadia1/minibox/internal/testutil"
)

// This test binary stands in for minibox: as the container's init
// ("init"), and as the whole command when MINIBOX_TEST_MAIN=1, so that a
// test can run `minibox run` as a process of its own and kill it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		container.Init()
	}
	if os.Getenv("MINIBOX_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestRunDefaultsTheHostnameToTheID(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--rootfs", rootfs, "-e", "A=b c", "--", "/bin/sh", "-c", `hostname; echo "$HOSTNAME $A"`}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(lines[0]) || lines[1] != lines[0]+" b c" {
		t.Fatalf("output %q", stdout.String())
	}
}

func TestRunUsageErrorsAre125(t *testing.T) {
	for _, args := range [][]string{
		{"run"},
		{"run", "--rootfs", "/x"},
		{"run", "--bogus", "x"},
		{"run", "--rootfs", "/x", "-e", "NOVALUE", "sh"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != container.ExitSetup {
			t.Errorf("%q: exit %d, want 125", args, code)
		}
	}
}

// minibox starts `minibox run ARGS` as a process of its own.
func minibox(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"run"}, args...)...)
	cmd.Env = append(os.Environ(), "MINIBOX_TEST_MAIN=1")
	// If this test binary dies (a timeout), its `minibox run` goes too,
	// and with it the container: nothing is left for a later run to find.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &out
}

// findProcess returns the host pid of a process with exactly this command
// line, or 0. The host sees every container's processes in its /proc.
func findProcess(t *testing.T, cmdline string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && string(data) == strings.ReplaceAll(cmdline, " ", "\x00")+"\x00" {
			return pid
		}
	}
	return 0
}

func waitForProcess(t *testing.T, cmdline string, present bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if (findProcess(t, cmdline) != 0) == present {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %q present=%v never happened", cmdline, !present)
}

// marker is a command line no other run of the tests uses: a sleep for a
// random number of seconds (bounded, so a leftover ends by itself).
func marker() []string {
	return []string{"sleep", fmt.Sprint(60 + rand.IntN(900))}
}

// Ctrl-C at a terminal reaches `minibox run` (the container has a session
// of its own); it must pass it on, and exit as the command did.
func TestInterruptReachesTheContainer(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	sleep := marker()
	cmd, out := minibox(t, append([]string{"--rootfs", rootfs, "--"}, sleep...)...)
	waitForProcess(t, strings.Join(sleep, " "), true)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 128+2 {
		t.Fatalf("exit %d (%v), want 130:\n%s", code, err, out)
	}
	waitForProcess(t, strings.Join(sleep, " "), false)
}

// If `minibox run` is killed outright, the container goes with it rather
// than running on with nobody to stop it.
func TestContainerDiesWithMinibox(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	sleep := marker()
	cmd, _ := minibox(t, append([]string{"--rootfs", rootfs, "--"}, sleep...)...)
	waitForProcess(t, strings.Join(sleep, " "), true)
	// The container's cgroup, as the host sees it: "0::/minibox/ID".
	own, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", findProcess(t, strings.Join(sleep, " "))))
	if err != nil {
		t.Fatal(err)
	}
	group := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(strings.TrimSpace(string(own)), "0::"))
	id := filepath.Base(group)
	veth := network.HostVethName(id)
	if !linkExists(veth) || !leased(t, id) {
		t.Fatalf("the running container has no veth %s or no lease", veth)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitForProcess(t, strings.Join(sleep, " "), false)
	// Its veth pair went with its network namespace (the kernel removes
	// a namespace's devices in the background: allow it a moment).
	for deadline := time.Now().Add(5 * time.Second); linkExists(veth); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the killed container's veth %s is still on the host", veth)
		}
	}
	// Its lease is left, and taken back by the next container to start.
	if !leased(t, id) {
		t.Fatal("the killed container's lease is gone before any other container started")
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--rootfs", rootfs, "--", "/bin/true"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("the next container: exit %d, %s", code, stderr.String())
	}
	if leased(t, id) {
		t.Error("the killed container's lease was not taken back by the next container")
	}
	// A killed `minibox run` cannot remove its group: it is left, empty,
	// for a later run to sweep. Check nothing escaped it, then remove it
	// here, so that no later test or smoke check finds it.
	events, err := os.ReadFile(filepath.Join(group, "cgroup.events"))
	if err != nil {
		t.Fatalf("the killed container's group %s: %v", group, err)
	}
	if !strings.Contains(string(events), "populated 0") {
		t.Fatalf("processes are still in %s after the container died:\n%s", group, events)
	}
	if err := syscall.Rmdir(group); err != nil {
		t.Fatal(err)
	}
}

func TestLimitFlags(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--rootfs", rootfs, "--memory", "48m", "--stats", "--", "/probe", "alloc", "200"}, nil, &stdout, &stderr)
	if code != 128+9 || !strings.Contains(stderr.String(), "went over its memory limit (48.0 MiB)") || !strings.Contains(stderr.String(), "killed for memory 1") {
		t.Fatalf("over the memory limit: exit %d, stderr %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"run", "--rootfs", rootfs, "--cpus", "0.5", "--pids", "64", "--stats", "--", "/bin/true"}, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "minibox: cpu ") || strings.Contains(stderr.String(), "memory limit") {
		t.Fatalf("within limits: exit %d, stderr %q", code, stderr.String())
	}
	for _, bad := range [][]string{{"--memory", "1m"}, {"--memory", "lots"}, {"--pids", "3"}, {"--cpus", "-1"}, {"--swap", "1g"}} {
		stderr.Reset()
		args := append(append([]string{"run", "--rootfs", rootfs}, bad...), "--", "/bin/true")
		if code := run(args, nil, &stdout, &stderr); code != container.ExitSetup {
			t.Errorf("%q: exit %d, want 125 (%s)", bad, code, stderr.String())
		}
	}
}

func linkExists(name string) bool {
	_, err := os.Stat(filepath.Join("/sys/class/net", name))
	return err == nil
}

// leased reports whether the container id holds an address lease.
func leased(t *testing.T, id string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(network.StateDir, "leases.json"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	var leases map[string]struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &leases); err != nil {
		t.Fatal(err)
	}
	for _, l := range leases {
		if l.ID == id {
			return true
		}
	}
	return false
}
