//go:build linux

// Command probe runs inside a test container and tries to get out. It is
// built static by the tests (testutil.Probe) and prints what it found.
//
//	probe chroot-escape PATH   the classic chroot escape, then: is PATH there?
//	probe alloc MIB            allocate and touch MIB mebibytes, then exit
//	probe spin SECONDS         keep one CPU busy for SECONDS of wall time
//	probe spawn N              start up to N sleeping children; print how many started
//	probe serve PORT           answer HTTP on PORT with "hello from HOSTNAME"
//	probe get URL              print the body at URL (retrying for 3 s), or exit 1
//	probe caps                 print the effective caps, no_new_privs and seccomp mode
//	probe mount                try to mount a tmpfs; print blocked or allowed
//	probe open-by-handle       try open_by_handle_at on /; print blocked or allowed
//	probe module               try finit_module; print blocked or reached
//	probe mknod-disk           try to mknod a block device; print blocked or allowed
//	probe raw-socket           try a raw and a packet socket; print blocked or allowed
//	probe write PATH           try to write to PATH; print blocked or allowed
//	probe read-len PATH        print how many bytes PATH yields (0 if masked)
//	probe keyctl               try keyctl (no capability needed); print blocked or allowed
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe chroot-escape PATH")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "chroot-escape":
		chrootEscape(os.Args[2])
	case "alloc":
		alloc(number(os.Args[2]))
	case "spin":
		spin(number(os.Args[2]))
	case "spawn":
		spawn(number(os.Args[2]))
	case "serve":
		serve(os.Args[2])
	case "get":
		get(os.Args[2])
	case "caps":
		caps()
	case "mount":
		mountProbe()
	case "open-by-handle":
		openByHandle()
	case "module":
		moduleProbe()
	case "mknod-disk":
		mknodDisk()
	case "raw-socket":
		rawSocket()
	case "write":
		writeProbe(os.Args[2])
	case "read-len":
		readLen(os.Args[2])
	case "keyctl":
		keyctlProbe()
	default:
		fmt.Fprintln(os.Stderr, "probe: unknown test", os.Args[1])
		os.Exit(2)
	}
}

// chrootEscape: chroot into a subdirectory while keeping the current
// directory outside it, walk up with "..", and chroot to where that ends.
// Inside a plain chroot this reaches the real root. Inside a container
// made with pivot_root and the old root detached, it reaches only the
// container's own root.
func chrootEscape(marker string) {
	must(os.MkdirAll("/escape-dir", 0o755))
	must(syscall.Chdir("/"))
	must(syscall.Chroot("/escape-dir"))
	for range 64 {
		must(syscall.Chdir(".."))
	}
	must(syscall.Chroot("."))
	if _, err := os.Stat(marker); err == nil {
		fmt.Println("escaped")
		return
	}
	fmt.Println("contained")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func number(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(2)
	}
	return n
}

// alloc touches every page, so the memory is really used, not only
// reserved.
func alloc(mib int) {
	blocks := make([][]byte, 0, mib)
	for range mib {
		b := make([]byte, 1<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		blocks = append(blocks, b)
	}
	fmt.Println("allocated", len(blocks), "MiB")
}

func spin(seconds int) {
	end := time.Now().Add(time.Duration(seconds) * time.Second)
	n := 0
	for time.Now().Before(end) {
		n++
	}
	fmt.Println("spun", n > 0)
}

// spawn starts children until one fails (at the pids limit, fork fails
// with EAGAIN), and prints how many started. They are left sleeping; the
// container's end takes them.
func spawn(n int) {
	started := 0
	for range n {
		_, err := syscall.ForkExec("/bin/sleep", []string{"sleep", "30"}, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
		if err != nil {
			fmt.Println("started", started, "then", err)
			return
		}
		started++
	}
	fmt.Println("started", started)
}

func serve(port string) {
	name, _ := os.Hostname()
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from %s", name)
	})
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func get(url string) {
	client := http.Client{Timeout: time.Second}
	var err error
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var resp *http.Response
		if resp, err = client.Get(url); err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				fmt.Print(string(body))
				return
			}
			err = rerr
		}
	}
	fmt.Fprintln(os.Stderr, "probe:", err)
	os.Exit(1)
}

// classify turns an attempt's error into "blocked" (the kernel refused
// it: EPERM, EACCES or EROFS) or "ALLOWED" (it worked). Any other error
// is printed as-is, so a test does not read a surprise as a pass.
func classify(err error) string {
	switch {
	case err == nil:
		return "ALLOWED"
	case err == unix.EPERM || err == unix.EACCES || err == unix.EROFS:
		return "blocked"
	default:
		return "error: " + err.Error()
	}
}

// caps prints the thread's effective capability mask, whether
// no_new_privs is set, and the seccomp mode, read from /proc/self/status.
func caps() {
	data, err := os.ReadFile("/proc/self/status")
	must(err)
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	fmt.Printf("caps=0x%s bnd=0x%s nnp=%s seccomp=%s\n", fields["CapEff"], fields["CapBnd"], fields["NoNewPrivs"], fields["Seccomp"])
}

// mountProbe tries to mount a tmpfs: it needs CAP_SYS_ADMIN, which the
// container does not have. The mount point is under /dev/shm, a tmpfs the
// container owns, so it can be made even in a user namespace (where the
// root filesystem is owned by the unmapped host root).
func mountProbe() {
	must(os.MkdirAll("/dev/shm/mountpoint", 0o755))
	err := unix.Mount("tmpfs", "/dev/shm/mountpoint", "tmpfs", 0, "")
	if err == nil {
		_ = unix.Unmount("/dev/shm/mountpoint", 0)
	}
	fmt.Println("mount:", classify(err))
}

// openByHandle gets a real handle for "/" (name_to_handle_at is allowed),
// then tries to open it: open_by_handle_at needs CAP_DAC_READ_SEARCH and
// is on the seccomp denylist. It can reopen any inode on the filesystem
// by handle, sidestepping the container's view of it.
func openByHandle() {
	handle, mountID, err := unix.NameToHandleAt(unix.AT_FDCWD, "/", 0)
	if err != nil {
		fmt.Println("open-by-handle: error:", err) // the first step should work
		return
	}
	fd, err := unix.OpenByHandleAt(mountID, handle, unix.O_RDONLY)
	if err == nil {
		unix.Close(fd)
	}
	fmt.Println("open-by-handle:", classify(err))
}

// moduleProbe tries to load a kernel module from /dev/null: blocked by
// the seccomp filter and by the missing CAP_SYS_MODULE. "reached" means
// the call got into the kernel (ENOEXEC: /dev/null is not a module), so
// nothing stopped it.
func moduleProbe() {
	fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	must(err)
	defer unix.Close(fd)
	err = unix.FinitModule(fd, "", 0)
	switch err {
	case unix.EPERM, unix.EACCES:
		fmt.Println("module: blocked")
	case unix.ENOEXEC:
		fmt.Println("module: reached")
	case nil:
		fmt.Println("module: ALLOWED")
	default:
		fmt.Println("module: error:", err)
	}
}

// mknodDisk tries to make a block device for the host's first disk: it
// needs CAP_MKNOD, which the container does not have (and MiniBox has no
// devices cgroup, so a node that worked would read the host's disk).
func mknodDisk() {
	_ = os.Remove("/escape-disk")
	err := unix.Mknod("/escape-disk", unix.S_IFBLK|0o600, int(unix.Mkdev(8, 0)))
	fmt.Println("mknod-disk:", classify(err))
}

// rawSocket tries a raw IP socket and a packet socket: both need
// CAP_NET_RAW, which the container does not have, so it cannot sniff or
// forge packets on the shared bridge.
func rawSocket() {
	raw, errRaw := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP)
	if errRaw == nil {
		unix.Close(raw)
	}
	pkt, errPkt := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if errPkt == nil {
		unix.Close(pkt)
	}
	if errRaw == nil || errPkt == nil {
		fmt.Println("raw-socket: ALLOWED")
		return
	}
	fmt.Println("raw-socket:", classify(errRaw))
}

// writeProbe tries to write one byte to a path: for a read-only path like
// /proc/sysrq-trigger the open fails with EROFS.
func writeProbe(path string) {
	fd, err := unix.Open(path, unix.O_WRONLY, 0)
	if err == nil {
		_, err = unix.Write(fd, []byte("0"))
		unix.Close(fd)
	}
	fmt.Println("write:", classify(err))
}

// readLen prints how many bytes a path yields: a masked file is backed by
// /dev/null and reads as empty, so 0 means it was masked.
func readLen(path string) {
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		fmt.Println("read-len: error:", err)
		return
	}
	defer unix.Close(fd)
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err != nil {
		fmt.Println("read-len: error:", err)
		return
	}
	fmt.Println("read-len:", n)
}

// keyctlProbe asks for the id of the user keyring: a keyctl that needs no
// capability, so it works in any container unless the seccomp filter
// refuses the keyctl system call. It shows the filter does something the
// capability drop does not.
func keyctlProbe() {
	_, err := unix.KeyctlGetKeyringID(unix.KEY_SPEC_USER_KEYRING, false)
	fmt.Println("keyctl:", classify(err))
}
