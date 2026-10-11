//go:build linux

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// setupRootfs makes the container's root filesystem its / and gives it
// /proc, /dev and /sys. It runs in Init, in the container's new mount
// namespace: every mount made here goes when the namespace does.
func setupRootfs(cfg Config) error {
	rootfs := cfg.Rootfs
	// Mount events must not travel back to the host. systemd makes / a
	// shared mount, and a new mount namespace starts as a copy of it, so
	// without this the mounts below would also appear on the host.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mounts private: %w", err)
	}
	info, err := os.Lstat(rootfs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", rootfs)
	}
	if len(cfg.Lower) > 0 {
		// The image's layers, read-only, under the container's own: the
		// overlay is mounted inside this namespace, so it cannot outlive
		// the container even if `minibox run` is killed.
		if err := mountOverlay(cfg); err != nil {
			return err
		}
	} else if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		// pivot_root needs the new root to be a mount point.
		return fmt.Errorf("bind-mounting %s: %w", rootfs, err)
	}
	if err := pivotRoot(rootfs); err != nil {
		return err
	}
	// From here every path is the container's: a symbolic link in its root
	// filesystem can only point at something else in it.
	return mountSpecial()
}

// maxMountData is the most a mount's options may hold: one page.
const maxMountData = 4096

func mountOverlay(cfg Config) error {
	data := "lowerdir=" + strings.Join(cfg.Lower, ":") + ",upperdir=" + cfg.Upper + ",workdir=" + cfg.Work
	if len(data) >= maxMountData {
		return fmt.Errorf("%d layers: their paths make overlay options of %d bytes, over the kernel's %d", len(cfg.Lower), len(data), maxMountData)
	}
	if err := unix.Mount("overlay", cfg.Rootfs, "overlay", 0, data); err != nil {
		return fmt.Errorf("mounting the image's layers: %w", err)
	}
	return nil
}

// pivotRoot swaps the root and detaches the old one, so that no path
// leads back to the host's files (chroot only changes where "/" starts:
// the old root stays reachable). pivot_root(".", ".") stacks the old root
// on top of the new one, as runc does, so no directory has to be made in
// the container's files for it.
func pivotRoot(rootfs string) error {
	oldRoot, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(oldRoot)
	newRoot, err := unix.Open(rootfs, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(newRoot)

	if err := unix.Fchdir(newRoot); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// "." is now the old root, mounted over the new one: detach it.
	if err := unix.Fchdir(oldRoot); err != nil {
		return err
	}
	if err := unix.Mount("", ".", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return err
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching the old root: %w", err)
	}
	return unix.Chdir("/")
}

type device struct {
	name         string
	major, minor uint32
}

// The device nodes a container gets, as Docker gives them.
var devices = []device{
	{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7},
	{"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
}

// mountSpecial mounts /proc (this PID namespace's), a /dev of its own
// with only the devices above, and /sys read-only.
func mountSpecial() error {
	const nosuid, nodev, noexec = unix.MS_NOSUID, unix.MS_NODEV, unix.MS_NOEXEC
	for _, dir := range []string{"/proc", "/dev", "/sys"} {
		if err := mountPoint(dir); err != nil {
			return err
		}
	}
	mounts := []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", nosuid | nodev | noexec, ""},
		{"tmpfs", "/dev", "tmpfs", nosuid | unix.MS_STRICTATIME, "mode=755,size=65536k"},
		{"devpts", "/dev/pts", "devpts", nosuid | noexec, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{"shm", "/dev/shm", "tmpfs", nosuid | nodev | noexec, "mode=1777,size=65536k"},
		{"mqueue", "/dev/mqueue", "mqueue", nosuid | nodev | noexec, ""},
		// Read-only: /sys is the host kernel's, not the container's.
		{"sysfs", "/sys", "sysfs", unix.MS_RDONLY | nosuid | nodev | noexec, ""},
		// The container's own cgroup, read-only: in its cgroup namespace
		// it is the root, so it can read its limits and usage.
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_RDONLY | nosuid | nodev | noexec, ""},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mounting %s on %s: %w", m.fstype, m.target, err)
		}
	}

	old := unix.Umask(0)
	defer unix.Umask(old)
	for _, d := range devices {
		path := filepath.Join("/dev", d.name)
		if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
	}
	links := [][2]string{
		{"/proc/self/fd", "/dev/fd"},
		{"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"},
		{"/proc/self/fd/2", "/dev/stderr"},
		{"pts/ptmx", "/dev/ptmx"},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], l[1]); err != nil {
			return err
		}
	}
	return nil
}

// mountPoint makes sure a top-level mount point is a real directory: one
// that is missing is made, one that is a link or a file is refused.
func mountPoint(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s in the root filesystem is not a directory (%s)", dir, info.Mode().Type())
	}
	return nil
}
