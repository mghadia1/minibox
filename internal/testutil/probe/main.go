// Command probe runs inside a test container and tries to get out. It is
// built static by the tests (testutil.Probe) and prints what it found.
//
//	probe chroot-escape PATH   the classic chroot escape, then: is PATH there?
package main

import (
	"fmt"
	"os"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe chroot-escape PATH")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "chroot-escape":
		chrootEscape(os.Args[2])
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
