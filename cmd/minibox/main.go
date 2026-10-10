// Command minibox runs Linux containers, built from the kernel's own
// pieces: namespaces, pivot_root, cgroup v2, overlayfs.
//
//	minibox check      can this machine run containers? (exit 1 if not)
//	minibox version    print the version
//
// MiniBox needs Linux and root. On a Mac it runs in the Lima VM described
// in lima/minibox.yaml.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/mghadia1/minibox/internal/host"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

// checkHost is host.Check; the tests replace it to see both outcomes.
var checkHost = host.Check

const usage = `usage: minibox <command>

commands:
  check      can this machine run containers? (exit 1 if not)
  version    print the version
`

// Exit codes for MiniBox's own errors. A container's exit code, from
// Milestone 1, is passed through as the shell would give it.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "check":
		report := checkHost()
		if err := report.Write(stdout); err != nil {
			return exitFail
		}
		if report.Failed() {
			return exitFail
		}
		return exitOK
	case "version", "--version":
		fmt.Fprintln(stdout, "minibox", version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "minibox: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}
