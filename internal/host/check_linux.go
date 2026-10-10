//go:build linux

package host

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// cgroup2Magic is CGROUP2_SUPER_MAGIC from linux/magic.h: what statfs
// reports for a cgroup v2 mount.
const cgroup2Magic = 0x63677270

// Check runs every check against this machine.
func Check() Report {
	return Report{
		checkRoot(),
		checkKernel(),
		checkNamespaces(),
		checkCgroup(),
		checkOverlay(),
		checkUserNamespaces(),
		checkNft(),
	}
}

func checkRoot() Result {
	if uid := os.Geteuid(); uid != 0 {
		return Result{"root", Fail, fmt.Sprintf("running as uid %d: namespaces, mounts and cgroups need root (sudo)", uid)}
	}
	return Result{"root", OK, "uid 0"}
}

func checkKernel() Result {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return Result{"kernel", Warn, "release not readable: " + err.Error()}
	}
	release := strings.TrimSpace(string(data))
	if _, _, ok := kernelRelease(release); !ok {
		return Result{"kernel", Warn, "release not understood: " + release}
	}
	return Result{"kernel", OK, release}
}

func checkNamespaces() Result {
	var have []string
	for _, ns := range Namespaces {
		if _, err := os.Lstat("/proc/self/ns/" + ns); err == nil {
			have = append(have, ns)
		}
	}
	if gone := missing(Namespaces, have); len(gone) > 0 {
		return Result{"namespaces", Fail, "not in this kernel: " + strings.Join(gone, " ")}
	}
	return Result{"namespaces", OK, strings.Join(have, " ")}
}

func checkCgroup() Result {
	var fs syscall.Statfs_t
	if err := syscall.Statfs("/sys/fs/cgroup", &fs); err != nil {
		return Result{"cgroup v2", Fail, "/sys/fs/cgroup: " + err.Error()}
	}
	if fs.Type != cgroup2Magic {
		return Result{"cgroup v2", Fail, fmt.Sprintf("/sys/fs/cgroup is not cgroup v2 (filesystem type %#x): MiniBox does not support cgroup v1", fs.Type)}
	}
	data, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return Result{"cgroup v2", Fail, err.Error()}
	}
	have := strings.Fields(string(data))
	if gone := missing(Controllers, have); len(gone) > 0 {
		return Result{"cgroup v2", Fail, "controllers not available: " + strings.Join(gone, " ")}
	}
	return Result{"cgroup v2", OK, strings.Join(Controllers, " ")}
}

func checkOverlay() Result {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return Result{"overlayfs", Fail, err.Error()}
	}
	for _, name := range filesystems(string(data)) {
		if name == "overlay" {
			return Result{"overlayfs", OK, "in /proc/filesystems"}
		}
	}
	// The module may not be loaded yet: the kernel loads it on first mount.
	if _, err := os.Stat("/sys/module/overlay"); err == nil {
		return Result{"overlayfs", OK, "module present"}
	}
	return Result{"overlayfs", Warn, "not loaded yet (the first mount may load it; images need it from Milestone 2)"}
}

func checkUserNamespaces() Result {
	data, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return Result{"user namespaces", Warn, err.Error()}
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n == 0 {
		return Result{"user namespaces", Warn, "disabled (max_user_namespaces is 0): --userns will not work"}
	}
	return Result{"user namespaces", OK, fmt.Sprintf("up to %d", n)}
}

func checkNft() Result {
	path, err := exec.LookPath("nft")
	if err != nil {
		return Result{"nftables", Warn, "nft not found: container networking (Milestone 4) will not work"}
	}
	return Result{"nftables", OK, path}
}
