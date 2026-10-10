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
)

// Namespaces a container gets in this milestone: its own hostname, process
// ids, mounts and System V IPC. It still shares the host's network and
// users (Milestones 4 and 5).
const cloneFlags = syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC

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

// SetupError is a container that could not be started: Code is 125, 126
// or 127.
type SetupError struct {
	Code    int
	Message string
}

func (e *SetupError) Error() string { return e.Message }

// Run runs a container and returns the exit code its command ended with,
// as a shell reports it. Signals arriving on signals are passed on to the
// command. A *SetupError means the command never ran.
func Run(cfg Config, stdio Stdio, signals <-chan os.Signal) (int, error) {
	if err := cfg.Validate(); err != nil {
		return ExitSetup, &SetupError{ExitSetup, err.Error()}
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return ExitSetup, err
	}

	// The container is started from this OS thread and must die with this
	// process: Pdeathsig fires when the thread that started the child
	// exits, not the process, and the Go runtime may end a thread that a
	// goroutine leaves locked. Keep this one until the container is gone.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	configR, configW, err := os.Pipe()
	if err != nil {
		return ExitSetup, err
	}
	defer configR.Close()
	defer configW.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return ExitSetup, err
	}
	defer statusR.Close()
	defer statusW.Close()

	self, err := os.Executable()
	if err != nil {
		return ExitSetup, err
	}
	cmd := exec.Command(self, "init")
	cmd.Args[0] = "minibox-init"
	cmd.Env = []string{} // nothing of the host's environment reaches the container
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	cmd.ExtraFiles = []*os.File{configR, statusW} // fds 3 and 4 in Init
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
		// A session of its own: a terminal's Ctrl-C reaches `minibox run`,
		// which passes it on, rather than every process at once.
		Setsid:    true,
		Pdeathsig: syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return ExitSetup, &SetupError{ExitSetup, "starting the container: " + err.Error()}
	}
	configR.Close()
	statusW.Close()

	if _, err := configW.Write(payload); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ExitSetup, &SetupError{ExitSetup, "sending the configuration: " + err.Error()}
	}
	configW.Close()

	var st status
	if err := json.NewDecoder(statusR).Decode(&st); err != nil {
		// Init died before reporting: its exit says more than the pipe.
		waitErr := cmd.Wait()
		return ExitSetup, &SetupError{ExitSetup, fmt.Sprintf("the container's init exited during setup (%v)", errors.Join(err, waitErr))}
	}
	statusR.Close()
	if st.Error != "" {
		_ = cmd.Wait()
		return st.Code, &SetupError{st.Code, st.Error}
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
	return exitCode(cmd.ProcessState, err)
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
