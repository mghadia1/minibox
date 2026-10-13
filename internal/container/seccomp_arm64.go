//go:build linux && arm64

package container

import "golang.org/x/sys/unix"

// nativeAudit is this build's AUDIT_ARCH_* value: the seccomp filter kills
// a system call made with any other architecture's ABI.
const nativeAudit = uint32(unix.AUDIT_ARCH_AARCH64)
