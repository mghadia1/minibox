//go:build linux

package oci

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/container"
)

// Create sets a container up from its bundle and leaves it in the
// "created" state: its init is running in all the namespaces, blocked on
// the exec fifo, with its cgroup and network in place. The command does
// not run until Start.
func Create(id, bundle string) (err error) {
	if err := validID(id); err != nil {
		return err
	}
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		return err
	}
	if exists(id) {
		return fmt.Errorf("container %q already exists", id)
	}
	spec, err := LoadSpec(bundle)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir(id), 0o700); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir(id))
		}
	}()
	cfg, err := spec.ToConfig(id, bundle, fifoPath(id))
	if err != nil {
		return err
	}
	if err := unix.Mkfifo(fifoPath(id), 0o600); err != nil {
		return fmt.Errorf("exec fifo: %w", err)
	}
	logf, err := os.OpenFile(logPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()

	group, err := cgroup.Create(id, cfg.Limits)
	if err != nil {
		return err
	}
	defer func() {
		if !ok {
			_ = group.Remove()
		}
	}()

	pid, err := container.CreateDetached(cfg, group, container.Stdio{In: devnull, Out: logf, Err: logf})
	if err != nil {
		return err
	}
	st := &State{
		OCIVersion: ociVersion, ID: id, Status: StatusCreated,
		Pid: pid, Bundle: bundle, Created: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeState(st); err != nil {
		_ = unix.Kill(pid, unix.SIGKILL)
		return err
	}
	ok = true
	return nil
}

// Start runs a created container's command by writing to its exec fifo,
// which unblocks the init waiting on it.
func Start(id string) error {
	st, err := GetState(id)
	if err != nil {
		return err
	}
	switch st.Status {
	case StatusCreated:
	case StatusStopped:
		return fmt.Errorf("container %q is stopped", id)
	default:
		return fmt.Errorf("container %q is not created (it is %s)", id, st.Status)
	}
	// Init holds the read end, so this open does not block; the byte is
	// what it is waiting for.
	f, err := os.OpenFile(fifoPath(id), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening exec fifo: %w", err)
	}
	_, werr := f.Write([]byte{0})
	f.Close()
	if werr != nil {
		return fmt.Errorf("starting the container: %w", werr)
	}
	st.Status = StatusRunning
	return writeState(st)
}

// GetState returns the container's state, recomputing "stopped" from
// whether its process is still alive.
func GetState(id string) (*State, error) {
	st, err := readState(id)
	if err != nil {
		return nil, err
	}
	if st.Status != StatusStopped && !alive(st.Pid) {
		st.Status = StatusStopped
		_ = writeState(st)
	}
	return st, nil
}

// Kill sends a signal (a name like TERM or KILL, with or without the SIG
// prefix, or a number) to the container's process.
func Kill(id, signal string) error {
	st, err := GetState(id)
	if err != nil {
		return err
	}
	if st.Status == StatusStopped {
		return fmt.Errorf("container %q is not running", id)
	}
	sig, err := parseSignal(signal)
	if err != nil {
		return err
	}
	return unix.Kill(st.Pid, sig)
}

// Delete removes a stopped container: its cgroup (killing anything left
// if force) and its state. The bundle and its root filesystem, which the
// caller owns, are left alone.
func Delete(id string, force bool) error {
	st, err := GetState(id)
	if err != nil {
		return err
	}
	if st.Status != StatusStopped && !force {
		return fmt.Errorf("container %q is not stopped (use --force)", id)
	}
	// Reap the container's process if it is our child, so a lingering
	// zombie does not keep its cgroup populated and un-removable. When
	// `minibox delete` runs as its own process the container is not its
	// child (it is the host init's), and this is a harmless no-op.
	var ws unix.WaitStatus
	_, _ = unix.Wait4(st.Pid, &ws, unix.WNOHANG, nil)
	_ = cgroup.RemoveByID(id)
	return os.RemoveAll(dir(id))
}

// alive reports whether the container's process is still running. A
// process that has exited but not yet been reaped is a zombie: its
// command is over, so the container counts as stopped. (In production the
// process is the host init's child and is reaped promptly; the zombie
// check matters when the parent has not reaped it, as in the tests.)
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // the process is gone
	}
	// The state is the character after the last ')': 'Z' is a zombie.
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
		return false
	}
	return true
}

// signals are the names parseSignal accepts, the ones a container is
// normally sent. The numbers are the same on the architectures MiniBox
// builds for.
var signals = map[string]unix.Signal{
	"HUP": unix.SIGHUP, "INT": unix.SIGINT, "QUIT": unix.SIGQUIT,
	"KILL": unix.SIGKILL, "TERM": unix.SIGTERM, "USR1": unix.SIGUSR1,
	"USR2": unix.SIGUSR2, "STOP": unix.SIGSTOP, "CONT": unix.SIGCONT,
}

func parseSignal(s string) (unix.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n >= 64 {
			return 0, fmt.Errorf("signal %q is out of range", s)
		}
		return unix.Signal(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if sig, ok := signals[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}
