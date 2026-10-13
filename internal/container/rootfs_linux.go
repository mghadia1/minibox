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
	if cfg.UserNS {
		// A mount namespace made together with a user namespace is "less
		// privileged": the kernel already turns the inherited shared
		// mounts into slaves (so nothing propagates to the host) and
		// locks them, so their propagation can no longer be changed. Ask
		// for slave, the state they are already in, and accept a refusal.
		_ = unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, "")
	} else if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
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
	// /proc, /dev, /sys and the masks are set up inside rootfs *before*
	// pivot_root, as runc does: a fresh procfs or sysfs can be mounted in
	// a user namespace only while a fully visible one is still in the
	// tree, and pivot_root detaches the host's. The top-level mount points
	// are checked to be plain directories, and every other path is then
	// inside one of these new mounts, so a symlink in the image cannot
	// redirect a mount out of the root filesystem.
	if err := mountSpecial(cfg, rootfs); err != nil {
		return err
	}
	return pivotRoot(rootfs)
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
// with only the devices above, and /sys read-only, all inside root (the
// container's root filesystem, not yet pivoted to), then masks and seals
// the parts of /proc and /sys that leak the host or let a process reach
// into the kernel. In a user namespace the devices are bind-mounted from
// the host's /dev (still visible before pivot_root) instead of created
// with mknod, which the mapped root may not do.
func mountSpecial(cfg Config, root string) error {
	const nosuid, nodev, noexec = unix.MS_NOSUID, unix.MS_NODEV, unix.MS_NOEXEC
	for _, dir := range []string{"/proc", "/dev", "/sys"} {
		if err := mountPoint(filepath.Join(root, dir)); err != nil {
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
		// The top-level /proc, /dev and /sys are checked above; /dev/pts
		// and the rest sit inside the tmpfs and sysfs just mounted, so
		// MkdirAll cannot be redirected by a symlink in the image.
		target := filepath.Join(root, m.target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.source, target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mounting %s on %s: %w", m.fstype, m.target, err)
		}
	}

	old := unix.Umask(0)
	defer unix.Umask(old)
	for _, d := range devices {
		target := filepath.Join(root, "/dev", d.name)
		if cfg.UserNS {
			if err := bindHostDevice(d.name, target); err != nil {
				return err
			}
			continue
		}
		if err := unix.Mknod(target, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil {
			return fmt.Errorf("creating /dev/%s: %w", d.name, err)
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
		if err := os.Symlink(l[0], filepath.Join(root, l[1])); err != nil {
			return err
		}
	}
	return applyProcMasks(root)
}

// bindHostDevice bind-mounts the host's /dev/<name> onto target, for a
// user namespace where the mapped root may not mknod a real device. Run
// before pivot_root, so the host's /dev is still at /dev.
func bindHostDevice(name, target string) error {
	if err := os.WriteFile(target, nil, 0o666); err != nil { // the mount point
		return fmt.Errorf("creating /dev/%s: %w", name, err)
	}
	if err := unix.Mount("/dev/"+name, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mounting /dev/%s from the host: %w", name, err)
	}
	return nil
}

// maskedPaths are made unreadable: a file is covered with /dev/null, a
// directory with an empty read-only tmpfs. They leak the host kernel's
// memory, keys, timers, hardware or power use. Docker's default list.
var maskedPaths = []string{
	"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys",
	"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
	"/proc/sched_debug", "/proc/scsi", "/sys/firmware",
	"/sys/devices/virtual/powercap",
}

// readonlyPaths can be read but not written, so a process cannot retune
// the host kernel or trigger it. /proc/sysrq-trigger is the sharpest: a
// write to it can crash or reboot the host. Docker's default list.
var readonlyPaths = []string{
	"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
}

// applyProcMasks hides the masked paths and seals the read-only ones,
// inside root. A path the kernel does not have is skipped.
func applyProcMasks(root string) error {
	for _, p := range maskedPaths {
		if err := maskPath(filepath.Join(root, p)); err != nil {
			return err
		}
	}
	for _, p := range readonlyPaths {
		if err := readonlyPath(filepath.Join(root, p)); err != nil {
			return err
		}
	}
	return nil
}

func maskPath(p string) error {
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := unix.Mount("tmpfs", p, "tmpfs", unix.MS_RDONLY, "mode=000,size=0"); err != nil {
			return fmt.Errorf("masking directory %s: %w", p, err)
		}
		return nil
	}
	if err := unix.Mount("/dev/null", p, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("masking %s: %w", p, err)
	}
	return nil
}

func readonlyPath(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Bind the path onto itself, then remount that bind read-only: a
	// remount can only change the mount it is given, so the bind must
	// exist first.
	if err := unix.Mount(p, p, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mounting %s read-only: %w", p, err)
	}
	if err := unix.Mount("", p, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remounting %s read-only: %w", p, err)
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
