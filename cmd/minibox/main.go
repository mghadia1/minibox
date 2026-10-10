// Command minibox runs Linux containers, built from the kernel's own
// pieces: namespaces, pivot_root, cgroup v2, overlayfs.
//
//	minibox run [options] [--] COMMAND [ARG...]   run a command in a container
//	minibox check                                 can this machine run containers?
//	minibox version                               print the version
//
// MiniBox needs Linux and root. On a Mac it runs in the Lima VM described
// in lima/minibox.yaml. `minibox init` is internal: the container's PID 1,
// started by `minibox run`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/mghadia1/minibox/internal/container"
	"github.com/mghadia1/minibox/internal/host"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

// checkHost is host.Check; the tests replace it to see both outcomes.
var checkHost = host.Check

const usage = `usage: minibox <command>

commands:
  run [options] [--] COMMAND [ARG...]
             run a command in a container (minibox run -h for options)
  check      can this machine run containers? (exit 1 if not)
  version    print the version
`

// Exit codes for MiniBox's own commands. `minibox run` exits with the
// container's code instead: the command's, 125 if MiniBox failed, 126 if
// the command could not be invoked, 127 if it was not found.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		container.Init() // never returns
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runContainer(args[1:], stdin, stdout, stderr)
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

// listFlag collects a repeated flag (-e A=1 -e B=2).
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func runContainer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("minibox run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootfs := fs.String("rootfs", "", "the container's root `directory` (required); the container writes into it")
	hostname := fs.String("hostname", "", "the container's hostname (default: its id)")
	workdir := fs.String("w", "/", "working `directory` inside the container")
	var env listFlag
	fs.Var(&env, "e", "set an environment variable (`KEY=value`; repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: minibox run --rootfs DIR [options] [--] COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return container.ExitSetup
	}
	if *rootfs == "" || fs.NArg() == 0 {
		fs.Usage()
		return container.ExitSetup
	}
	root, err := filepath.Abs(*rootfs)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return container.ExitSetup
	}

	id := container.NewID()
	if *hostname == "" {
		*hostname = id
	}
	environment, err := container.Environment(*hostname, os.Getenv("TERM"), env)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return container.ExitSetup
	}
	cfg := container.Config{
		ID:       id,
		Rootfs:   root,
		Hostname: *hostname,
		Args:     fs.Args(),
		Env:      environment,
		Cwd:      *workdir,
	}

	signals := make(chan os.Signal, 16)
	signal.Notify(signals, container.Forwarded...)
	defer signal.Stop(signals)

	code, err := container.Run(cfg, container.Stdio{In: stdin, Out: stdout, Err: stderr}, signals)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
	}
	return code
}
