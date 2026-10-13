//go:build linux

package container

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file hardens the container just before its command is exec'd:
// capabilities cut to a small set, no_new_privs, and a seccomp filter.
// It runs in Init, on a locked OS thread, so that the fork for the
// command inherits exactly this thread's capabilities, bounding set and
// seccomp filter (all three are per-thread on Linux, passed on by
// fork/execve). Init's other threads keep their privileges, which does
// not matter: the command is what is sandboxed, not PID 1.

// DefaultCaps is the capability set a container keeps: Docker's default
// set, minus two that MiniBox drops on purpose.
//
//   - CAP_NET_RAW is dropped: every bridged container shares one bridge
//     with no per-container packet filtering, so a raw or packet socket
//     could sniff or spoof its neighbours' traffic.
//   - CAP_MKNOD is dropped: MiniBox has no devices cgroup, so a device
//     node the container made (mknod /dev/sda b 8 0) would read the
//     host's disks. Docker keeps CAP_MKNOD only because it pairs it with
//     a devices cgroup that denies access to anything so created.
//
// Everything else matches Docker's default: the file-permission caps a
// normal program expects, setuid/setgid to drop to a user, chroot, kill,
// binding low ports, and audit_write.
var DefaultCaps = []uintptr{
	unix.CAP_CHOWN,
	unix.CAP_DAC_OVERRIDE,
	unix.CAP_FOWNER,
	unix.CAP_FSETID,
	unix.CAP_KILL,
	unix.CAP_SETGID,
	unix.CAP_SETUID,
	unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE,
	unix.CAP_SYS_CHROOT,
	unix.CAP_AUDIT_WRITE,
	unix.CAP_SETFCAP,
}

// deniedSyscalls are refused by the seccomp filter with EPERM, whatever
// capabilities the command has. A hand-picked subset of Docker's default
// denials: the ones with no use in an ordinary program and a real cost
// if allowed. The per-architecture numbers come from the build.
//
// Capabilities already block most of these (loading a module needs
// CAP_SYS_MODULE, open_by_handle_at CAP_DAC_READ_SEARCH, kexec and reboot
// CAP_SYS_BOOT, swap CAP_SYS_ADMIN) — the filter is a second line, in
// case a capability is ever added back or a bug leaves one in the set.
var deniedSyscalls = []int{
	unix.SYS_INIT_MODULE,       // load a kernel module into the host kernel
	unix.SYS_FINIT_MODULE,      // the same, from a file descriptor
	unix.SYS_DELETE_MODULE,     // unload one
	unix.SYS_KEXEC_LOAD,        // stage a new kernel to boot into
	unix.SYS_KEXEC_FILE_LOAD,   // the same, from a file
	unix.SYS_OPEN_BY_HANDLE_AT, // open a file by handle: escapes a bind mount
	unix.SYS_PERF_EVENT_OPEN,   // a wide kernel attack surface
	unix.SYS_REBOOT,            // reboot the host
	unix.SYS_SWAPON,            // add host swap
	unix.SYS_SWAPOFF,           // remove it
	unix.SYS_ADD_KEY,           // the shared kernel keyring
	unix.SYS_KEYCTL,            // manage keys on it
	unix.SYS_REQUEST_KEY,       // ask for one
}

// harden drops capabilities, sets no_new_privs and installs the seccomp
// filter, in that order. The caller must have locked its OS thread and
// must fork the command from the same thread straight after.
func harden() error {
	if err := dropCapabilities(); err != nil {
		return err
	}
	// no_new_privs: a set-user-id or file-capability binary the command
	// exec's cannot gain privileges beyond the set above. Also lets
	// seccomp be installed without CAP_SYS_ADMIN.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := loadSeccomp(); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return nil
}

// dropCapabilities removes every capability not in DefaultCaps from the
// thread's bounding set and sets the permitted and effective sets to
// DefaultCaps. Inheritable and ambient are left empty: a container that
// runs as root keeps DefaultCaps (the kernel's rule for a root process
// exec'ing a file with no file capabilities grants it the bounding set),
// and one that drops to a non-root user (--user) keeps none, which is the
// point of running as a user. The command inherits all of this from the
// fork that starts it.
func dropCapabilities() error {
	keep := map[uintptr]bool{}
	for _, c := range DefaultCaps {
		keep[c] = true
	}
	// The bounding set caps what the command can ever hold, across every
	// execve: dropping is one-way and cannot be undone.
	for c := uintptr(0); c <= uintptr(lastCap()); c++ {
		if keep[c] {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0); err != nil {
			return fmt.Errorf("dropping capability %d from the bounding set: %w", c, err)
		}
	}

	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	for _, c := range DefaultCaps {
		data[c>>5].Effective |= 1 << (c & 31)
		data[c>>5].Permitted |= 1 << (c & 31)
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("setting capabilities: %w", err)
	}
	return nil
}

// lastCap is the highest capability this kernel knows, from
// /proc/sys/kernel/cap_last_cap; the header's value is a ceiling if the
// file cannot be read (dropping an unknown cap would be EINVAL).
func lastCap() int {
	if data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			return n
		}
	}
	return unix.CAP_LAST_CAP
}

// Classic-BPF opcodes for the seccomp filter (linux/bpf_common.h). The
// filter is a small program the kernel runs for every system call.
const (
	bpfLd  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS  // load a 32-bit word from seccomp_data
	bpfJeq = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K // jump if the accumulator equals K
	bpfRet = unix.BPF_RET | unix.BPF_K                // return K
)

// Offsets into struct seccomp_data: the system call number, then the
// architecture (linux/seccomp.h).
const (
	seccompNrOffset   = 0
	seccompArchOffset = 4
)

// loadSeccomp builds and installs the filter: refuse a wrong architecture
// outright (so a second ABI cannot reach the kernel with numbers this
// filter never checked), return EPERM for each denied syscall, allow the
// rest.
func loadSeccomp() error {
	errnoRet := uint32(unix.SECCOMP_RET_ERRNO | (uint32(unix.EPERM) & unix.SECCOMP_RET_DATA))
	// Layout: [0] load arch, [1] keep if native else [2] kill, [3] load
	// nr, then one compare per denied call jumping to the EPERM return,
	// then allow, then the EPERM return last.
	filter := []unix.SockFilter{
		{Code: bpfLd, K: seccompArchOffset},
		{Code: bpfJeq, K: nativeAudit, Jt: 1, Jf: 0},
		{Code: bpfRet, K: uint32(unix.SECCOMP_RET_KILL_PROCESS)},
		{Code: bpfLd, K: seccompNrOffset},
	}
	errnoIndex := len(filter) + len(deniedSyscalls) + 1
	for _, nr := range deniedSyscalls {
		// Each compare jumps forward to the EPERM return. len(filter) is
		// where this instruction will land, so the distance from the
		// instruction after it to the return is errnoIndex-len(filter)-1.
		jt := errnoIndex - len(filter) - 1
		filter = append(filter, unix.SockFilter{Code: bpfJeq, K: uint32(nr), Jt: uint8(jt), Jf: 0})
	}
	filter = append(filter,
		unix.SockFilter{Code: bpfRet, K: uint32(unix.SECCOMP_RET_ALLOW)},
		unix.SockFilter{Code: bpfRet, K: errnoRet},
	)

	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), 0, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(filter)
	runtime.KeepAlive(&prog)
	if errno != 0 {
		return errno
	}
	return nil
}
