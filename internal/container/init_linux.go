//go:build linux

package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/network"
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

	// The OCI exec fifo lives on the host; open it before pivot_root
	// detaches the host's filesystem. O_RDWR, not O_RDONLY: opening a fifo
	// read-only when no writer is present makes a later read return EOF at
	// once, but holding a write end too (which never blocks the open on
	// Linux) keeps the read blocked until `start` writes. O_CLOEXEC so it
	// does not reach the command.
	fifoFD := -1
	if cfg.ExecFifo != "" {
		fd, err := unix.Open(cfg.ExecFifo, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			fail(ExitSetup, fmt.Errorf("opening the exec fifo: %w", err))
		}
		fifoFD = fd
	}

	if err := setupRootfs(cfg); err != nil {
		fail(ExitSetup, fmt.Errorf("root filesystem: %w", err))
	}
	if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
		fail(ExitSetup, fmt.Errorf("hostname: %w", err))
	}
	if cfg.Network.Mode != network.Host {
		if err := network.SetUpInside(cfg.Network); err != nil {
			fail(ExitSetup, fmt.Errorf("network: %w", err))
		}
	}
	// From here on, every file is the container's own.
	creds, err := user(cfg.User)
	if err != nil {
		fail(ExitSetup, err)
	}
	env := cfg.Env
	if _, ok := lookupEnv(env, "HOME"); !ok {
		env = append(env, "HOME="+creds.Home)
	}
	// An image's working directory may not exist yet: Docker makes it.
	if err := os.MkdirAll(cfg.Cwd, 0o755); err != nil {
		fail(ExitSetup, fmt.Errorf("working directory: %w", err))
	}

	pathEnv, _ := lookupEnv(env, "PATH")
	file, err := lookPath(cfg.Args[0], pathEnv)
	if err != nil {
		fail(invokeCode(err), fmt.Errorf("%s: %w", cfg.Args[0], err))
	}

	if cfg.ExecFifo != "" {
		// OCI lifecycle. Setup is done, so report "created" and wait for
		// `start`; then become the command, so it is PID 1 as runc does.
		_ = json.NewEncoder(statusW).Encode(status{})
		statusW.Close()
		statusW = nil
		if err := waitExecFifo(fifoFD); err != nil {
			fmt.Fprintln(os.Stderr, "minibox init:", err)
			os.Exit(ExitSetup)
		}
		runtime.LockOSThread()
		if err := harden(); err != nil {
			fmt.Fprintln(os.Stderr, "minibox init: hardening:", err)
			os.Exit(ExitSetup)
		}
		if cfg.User != "" {
			if err := becomeUser(creds); err != nil {
				fmt.Fprintln(os.Stderr, "minibox init:", err)
				os.Exit(ExitSetup)
			}
		}
		// execve keeps the working directory, so set it here (the fork
		// path uses ProcAttr.Dir instead).
		if err := unix.Chdir(cfg.Cwd); err != nil {
			fmt.Fprintf(os.Stderr, "minibox init: working directory %s: %v\n", cfg.Cwd, err)
			os.Exit(ExitSetup)
		}
		err := syscall.Exec(file, cfg.Args, env)
		// Exec only returns on failure; the status pipe is already closed,
		// so the error goes to the container's own stderr.
		fmt.Fprintf(os.Stderr, "minibox init: %s: %v\n", cfg.Args[0], err)
		os.Exit(invokeCode(err))
	}

	// `minibox run`: Init stays PID 1 and supervises the command.
	// Ask for signals before the command exists, so that no exit and no
	// forwarded signal is missed between starting it and waiting for it.
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, append([]os.Signal{syscall.SIGCHLD}, Forwarded...)...)

	attr := &syscall.ProcAttr{Dir: cfg.Cwd, Env: env, Files: []uintptr{0, 1, 2}}
	if cfg.User != "" {
		attr.Sys = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: creds.UID, Gid: creds.GID, Groups: creds.Groups}}
	}
	// Harden on a locked OS thread, then fork from the same thread: the
	// command inherits this thread's reduced capabilities, bounding set,
	// no_new_privs and seccomp filter (all per-thread, passed on by the
	// fork). Everything privileged (mounts, hostname, the network) is
	// done above, with full capabilities.
	runtime.LockOSThread()
	if err := harden(); err != nil {
		fail(ExitSetup, fmt.Errorf("hardening the container: %w", err))
	}
	child, err := syscall.ForkExec(file, cfg.Args, attr)
	if err != nil {
		fail(invokeCode(err), fmt.Errorf("%s: %w", cfg.Args[0], err))
	}
	_ = json.NewEncoder(statusW).Encode(status{})
	statusW.Close()
	statusW = nil

	os.Exit(supervise(child, sigs))
}

// waitExecFifo blocks until `start` writes the one byte to the exec fifo.
// The fd is O_RDWR, so the read blocks for data rather than returning EOF
// before any writer has appeared.
func waitExecFifo(fd int) error {
	buf := make([]byte, 1)
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for start: %w", err)
		}
		if n == 0 {
			return errors.New("the container was deleted before it started")
		}
		return nil
	}
}

// becomeUser drops to a non-root user before the OCI exec, in the order
// that keeps it allowed (groups, then gid, then uid). The run path does
// this through the fork's Credential instead.
func becomeUser(c Credentials) error {
	groups := make([]int, len(c.Groups))
	for i, g := range c.Groups {
		groups[i] = int(g)
	}
	if err := unix.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setresgid(int(c.GID), int(c.GID), int(c.GID)); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := unix.Setresuid(int(c.UID), int(c.UID), int(c.UID)); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

// user finds who the command runs as in the container's /etc/passwd and
// /etc/group (either may be missing: a scratch image has neither). With
// no user given it is root, and only HOME is looked up.
func user(spec string) (Credentials, error) {
	open := func(name string) io.Reader {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil
		}
		return bytes.NewReader(data)
	}
	lookup := spec
	if lookup == "" {
		lookup = "0"
	}
	creds, err := ResolveUser(lookup, open("/etc/passwd"), open("/etc/group"))
	if err != nil {
		return Credentials{}, fmt.Errorf("user: %w", err)
	}
	if creds.Home == "" {
		creds.Home = "/"
		if creds.UID == 0 {
			creds.Home = "/root"
		}
	}
	return creds, nil
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
