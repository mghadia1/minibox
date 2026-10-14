//go:build linux

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/network"
)

// Namespaces a container gets: its own hostname, process ids, mounts,
// System V IPC and cgroup view (its own group is its "/"), and its own
// network unless it asks for the host's. It still shares the host's users
// (Milestone 5).
const cloneFlags = syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | unix.CLONE_NEWCGROUP

// The host uid/gid range a --userns container's 0..65535 map onto:
// 100000-165535, a block unlikely to be a real user, so the container's
// root is unprivileged on the host.
const (
	userNSBase = 100000
	userNSSize = 65536
)

// Forwarded are the signals passed on to the container's command: Run
// passes them to Init, Init to the command. Not "every signal": the Go
// runtime sends itself SIGURG to preempt goroutines, SIGCHLD is how Init
// learns of exits, and SIGKILL and SIGSTOP cannot be caught.
var Forwarded = []os.Signal{
	syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGALRM,
}

// Stdio is what the command reads and writes. An *os.File is passed to
// the container as it is (a terminal stays a terminal); anything else goes
// through a pipe.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// status is Init's one message to Run: the command started, or why not.
type status struct {
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// Result is how a container ended: its exit code, as a shell reports it,
// and what it used, from its cgroup.
type Result struct {
	Code  int
	Usage cgroup.Usage
}

// SetupError is a container that could not be started: Code is 125, 126
// or 127.
type SetupError struct {
	Code    int
	Message string
}

func (e *SetupError) Error() string { return e.Message }

// Run runs a container and returns how its command ended and what it
// used. Signals arriving on signals are passed on to the command. A
// *SetupError means the command never ran.
func Run(cfg Config, stdio Stdio, signals <-chan os.Signal) (res Result, err error) {
	setup := func(err error) (Result, error) {
		var se *SetupError
		if errors.As(err, &se) {
			return Result{Code: se.Code}, err
		}
		return Result{Code: ExitSetup}, &SetupError{ExitSetup, err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return setup(err)
	}
	if cfg.NetworkMode == "" {
		cfg.NetworkMode = network.Bridge
	}
	flags := uintptr(cloneFlags)
	if cfg.NetworkMode != network.Host {
		flags |= syscall.CLONE_NEWNET
	}
	if cfg.UserNS {
		flags |= syscall.CLONE_NEWUSER
	}
	// A cgroup of its own, made first: Init is born inside it (clone3's
	// CLONE_INTO_CGROUP), so nothing of the container runs outside its
	// limits, even for an instant. Removed last, after anything still in
	// it is killed.
	group, err := cgroup.Create(cfg.ID, cfg.Limits)
	if err != nil {
		return setup(err)
	}
	defer func() {
		// Reported, not ignored: a group left behind once went unexplained.
		if rerr := group.Remove(); rerr != nil && err == nil {
			err = rerr
		}
	}()

	// The container is started from this OS thread and must die with this
	// process: Pdeathsig fires when the thread that started the child
	// exits, not the process, and the Go runtime may end a thread that a
	// goroutine leaves locked. Keep this one until the container is gone.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	configR, configW, err := os.Pipe()
	if err != nil {
		return setup(err)
	}
	defer configR.Close()
	defer configW.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return setup(err)
	}
	defer statusR.Close()
	defer statusW.Close()

	self, err := os.Executable()
	if err != nil {
		return setup(err)
	}
	cmd := exec.Command(self, "init")
	cmd.Args[0] = "minibox-init"
	cmd.Env = []string{} // nothing of the host's environment reaches the container
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	cmd.ExtraFiles = []*os.File{configR, statusW} // fds 3 and 4 in Init
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
		// A session of its own: a terminal's Ctrl-C reaches `minibox run`,
		// which passes it on, rather than every process at once.
		Setsid:      true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true,
		CgroupFD:    group.FD(),
	}
	if cfg.UserNS {
		// Container ids 0..65535 map to an unprivileged host range, so the
		// container's root is host uid userNSBase, with no authority on
		// the host. `minibox run` is real root, so it may write the maps
		// directly (no newuidmap, no /etc/subuid). The clone leaves Init
		// as the unmapped overflow uid, which loses the namespace's
		// capabilities at the exec into minibox-init; the Credential
		// changes it to uid/gid 0 after the maps are written and before
		// that exec, while it still holds them.
		applyUserNS(cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		return setup(fmt.Errorf("starting the container: %w", err))
	}
	configR.Close()
	statusW.Close()

	// Init exists, and waits for its configuration: its network namespace
	// can be wired up now, from here.
	switch cfg.NetworkMode {
	case network.Bridge:
		// Not "att, err :=": that would make a new err here, and the
		// deferred function below would report into it, not into Run's.
		att, aerr := network.Attach(cfg.ID, cmd.Process.Pid, cfg.Ports)
		if aerr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return setup(fmt.Errorf("network: %w", aerr))
		}
		defer func() {
			// Reported like the group's removal: a lease or a published
			// port left behind would block the next container.
			if derr := att.Detach(); derr != nil && err == nil {
				err = derr
			}
		}()
		cfg.Network = att.InsideConfig(cfg.Hostname)
	case network.None:
		cfg.Network = network.NoneConfig(cfg.Hostname)
	case network.Host:
		cfg.Network = network.Inside{Mode: network.Host}
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return setup(err)
	}
	if _, err := configW.Write(payload); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return setup(fmt.Errorf("sending the configuration: %w", err))
	}
	configW.Close()

	var st status
	if err := json.NewDecoder(statusR).Decode(&st); err != nil {
		// Init died before reporting: its exit says more than the pipe.
		waitErr := cmd.Wait()
		return setup(fmt.Errorf("the container's init exited during setup (%v)", errors.Join(err, waitErr)))
	}
	statusR.Close()
	if st.Error != "" {
		_ = cmd.Wait()
		return setup(&SetupError{st.Code, st.Error})
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	code, err := exitCode(cmd.ProcessState, err)
	return Result{Code: code, Usage: group.Usage()}, err
}

// exitCode turns how a process ended into a shell's exit code.
func exitCode(state *os.ProcessState, err error) (int, error) {
	if state == nil {
		return ExitSetup, err
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return ExitSetup, fmt.Errorf("unexpected wait status %v", state.Sys())
	}
	return shellStatus(ws), nil
}

// shellStatus is a wait status as a shell gives it: the exit code, or
// 128 + the number of the signal that killed the process.
func shellStatus(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return exitSignalBase + int(ws.Signal())
	}
	return ws.ExitStatus()
}
