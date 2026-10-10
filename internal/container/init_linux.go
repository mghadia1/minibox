//go:build linux

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Init is `minibox init`: PID 1 of a new container, started by Run. It
// never returns.
func Init() {
	statusW := os.NewFile(4, "status")
	fail := func(code int, err error) {
		if statusW != nil {
			_ = json.NewEncoder(statusW).Encode(status{Error: err.Error(), Code: code})
		}
		os.Exit(code)
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "minibox init is started by `minibox run`, not by hand")
		os.Exit(ExitSetup)
	}
	// Neither pipe may reach the command: it would hold Run's end open.
	unix.CloseOnExec(3)
	unix.CloseOnExec(4)

	var cfg Config
	configR := os.NewFile(3, "config")
	if err := json.NewDecoder(configR).Decode(&cfg); err != nil {
		fail(ExitSetup, fmt.Errorf("reading the configuration: %w", err))
	}
	configR.Close()
	if err := cfg.Validate(); err != nil {
		fail(ExitSetup, err)
	}

	if err := setupRootfs(cfg.Rootfs); err != nil {
		fail(ExitSetup, fmt.Errorf("root filesystem: %w", err))
	}
	if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
		fail(ExitSetup, fmt.Errorf("hostname: %w", err))
	}

	// Ask for signals before the command exists, so that no exit and no
	// forwarded signal is missed between starting it and waiting for it.
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, append([]os.Signal{syscall.SIGCHLD}, Forwarded...)...)

	pathEnv, _ := lookupEnv(cfg.Env, "PATH")
	file, err := lookPath(cfg.Args[0], pathEnv)
	if err != nil {
		fail(invokeCode(err), fmt.Errorf("%s: %w", cfg.Args[0], err))
	}
	child, err := syscall.ForkExec(file, cfg.Args, &syscall.ProcAttr{
		Dir:   cfg.Cwd,
		Env:   cfg.Env,
		Files: []uintptr{0, 1, 2},
	})
	if err != nil {
		fail(invokeCode(err), fmt.Errorf("%s: %w", cfg.Args[0], err))
	}
	_ = json.NewEncoder(statusW).Encode(status{})
	statusW.Close()
	statusW = nil

	os.Exit(supervise(child, sigs))
}

// invokeCode is the exit code for a command that could not be started:
// 127 if it does not exist, 126 if it exists but cannot be run.
func invokeCode(err error) int {
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, syscall.ENOENT):
		return ExitNotFound
	case errors.Is(err, errNotExecutable), errors.Is(err, syscall.EACCES),
		errors.Is(err, syscall.ENOEXEC), errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EISDIR):
		return ExitCannotInvoke
	}
	return ExitSetup
}

// supervise is PID 1's job until the command exits: reap every process
// that ends in the container (orphans are re-parented to PID 1, and a
// zombie nobody waits for stays in the process table), and pass forwarded
// signals on to the command. It returns the command's exit code.
//
// All waiting happens here, with wait4(-1): a second waiter (os/exec's
// Wait) could reap the command first and lose its status.
func supervise(child int, sigs <-chan os.Signal) int {
	for sig := range sigs {
		if sig != syscall.SIGCHLD {
			_ = syscall.Kill(child, sig.(syscall.Signal))
			continue
		}
		// Signals coalesce: one SIGCHLD can stand for many exits.
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			if pid <= 0 || err != nil {
				break
			}
			if pid == child {
				// Returning ends PID 1, and the kernel kills whatever is
				// left in the container.
				return shellStatus(ws)
			}
		}
	}
	return ExitSetup // unreachable: sigs is never closed
}
