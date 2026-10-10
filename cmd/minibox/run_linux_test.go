//go:build linux

package main

import (
	"bytes"
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
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitForProcess(t, strings.Join(sleep, " "), false)
}
