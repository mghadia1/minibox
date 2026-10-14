//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/network"
)

// applyUserNS adds the uid/gid maps and the change-to-root for a user
// namespace. Shared by Run and CreateDetached. See run_linux.go for why.
func applyUserNS(sa *syscall.SysProcAttr) {
	sa.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: userNSBase, Size: userNSSize}}
	sa.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: userNSBase, Size: userNSSize}}
	sa.GidMappingsEnableSetgroups = true
	sa.Credential = &syscall.Credential{Uid: 0, Gid: 0}
}

// CreateDetached starts the container's init in new namespaces, inside
// group, detached from the caller: no Pdeathsig, a session of its own, so
// it survives `minibox create` exiting and is reparented to the host's
// init. Init sets the container up and then blocks on its exec fifo (set
// in cfg.ExecFifo) until `start`. CreateDetached returns once setup has
// succeeded, with the container's PID 1 — which, after `start` execs the
// command, is the command. It does not attach a bridge (OCI leaves the
// network to the caller): the container gets a private network namespace
// with loopback, or the host's with network.Host.
//
// stdio must be *os.File (a detached container cannot keep a pipe to a
// process that has exited): the OCI caller passes the bundle's streams or
// a log file.
func CreateDetached(cfg Config, group *cgroup.Group, stdio Stdio) (pid int, err error) {
	if cfg.ExecFifo == "" {
		return 0, fmt.Errorf("CreateDetached needs an exec fifo")
	}
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	if cfg.NetworkMode == "" {
		cfg.NetworkMode = network.None
	}
	flags := uintptr(cloneFlags)
	if cfg.NetworkMode != network.Host {
		flags |= syscall.CLONE_NEWNET
	}
	if cfg.UserNS {
		flags |= syscall.CLONE_NEWUSER
	}
	switch cfg.NetworkMode {
	case network.Host:
		cfg.Network = network.Inside{Mode: network.Host}
	default:
		cfg.Network = network.NoneConfig(cfg.Hostname)
	}

	configR, configW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer configR.Close()
	defer configW.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer statusR.Close()
	defer statusW.Close()

	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, "init")
	cmd.Args[0] = "minibox-init"
	cmd.Env = []string{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	cmd.ExtraFiles = []*os.File{configR, statusW}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  flags,
		Setsid:      true, // a session of its own, detached from any terminal
		UseCgroupFD: true,
		CgroupFD:    group.FD(),
		// No Pdeathsig: the container outlives `minibox create`.
	}
	if cfg.UserNS {
		applyUserNS(cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting the container: %w", err)
	}
	pid = cmd.Process.Pid
	configR.Close()
	statusW.Close()

	payload, err := json.Marshal(cfg)
	if err != nil {
		_ = cmd.Process.Kill()
		return 0, err
	}
	if _, err := configW.Write(payload); err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("sending the configuration: %w", err)
	}
	configW.Close()

	var st status
	if err := json.NewDecoder(statusR).Decode(&st); err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("the container's init exited during setup")
	}
	if st.Error != "" {
		_ = cmd.Process.Kill()
		return 0, &SetupError{st.Code, st.Error}
	}
	// Setup succeeded and Init is blocked on the fifo. Release our handle
	// so the Go runtime does not wait on it; the container is now the
	// host init's child and lives until `start` runs it and it exits.
	_ = cmd.Process.Release()
	return pid, nil
}
