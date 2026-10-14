// Command minibox runs Linux containers, built from the kernel's own
// pieces: namespaces, pivot_root, cgroup v2, overlayfs.
//
//	minibox run [options] IMAGE [COMMAND [ARG...]]     run an image
//	minibox run [options] --rootfs DIR [--] COMMAND...  run a command on a folder
//	minibox pull IMAGE                                 fetch an image from its registry
//	minibox images                                     list the images pulled
//	minibox rmi IMAGE                                  remove an image
//	minibox check                                      can this machine run containers?
//	minibox version                                    print the version
//
// Images and containers live in /var/lib/minibox ($MINIBOX_ROOT).
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
	"text/tabwriter"
	"time"

	"github.com/mghadia1/minibox/internal/cgroup"
	"github.com/mghadia1/minibox/internal/container"
	"github.com/mghadia1/minibox/internal/host"
	"github.com/mghadia1/minibox/internal/image"
	"github.com/mghadia1/minibox/internal/network"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

// checkHost is host.Check; the tests replace it to see both outcomes.
var checkHost = host.Check

// newClient makes the registry client; tests point it at a local registry.
var newClient = image.NewClient

const usage = `usage: minibox <command>

commands:
  run [options] IMAGE [COMMAND [ARG...]]
  run [options] --rootfs DIR [--] COMMAND [ARG...]
             run a container (minibox run -h for options)
  pull IMAGE fetch an image from its registry (the only network MiniBox uses)
  images     list the images pulled
  rmi IMAGE  remove an image, and the layers no other image uses
  check      can this machine run containers? (exit 1 if not)
  version    print the version

  OCI runtime lifecycle, on a bundle (a config.json and a root filesystem):
  create --bundle DIR ID   set a container up, ready to start
  start ID                 run a created container's command
  state ID                 print a container's state as JSON
  kill ID [SIGNAL]         signal a container's process (default TERM)
  delete ID [--force]      remove a stopped container
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
	case "pull":
		return pull(args[1:], stdout, stderr)
	case "images":
		return images(args[1:], stdout, stderr)
	case "rmi":
		return rmi(args[1:], stdout, stderr)
	case "create":
		return ociCreate(args[1:], stderr)
	case "start":
		return ociStart(args[1:], stderr)
	case "state":
		return ociState(args[1:], stdout, stderr)
	case "kill":
		return ociKill(args[1:], stderr)
	case "delete":
		return ociDelete(args[1:], stderr)
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

// storeRoot is where images and containers are kept.
func storeRoot() string {
	if r := os.Getenv("MINIBOX_ROOT"); r != "" {
		return r
	}
	return image.DefaultRoot
}

func openStore(log io.Writer) (*image.Store, error) {
	return image.Open(storeRoot(), log)
}

func pull(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: minibox pull IMAGE")
		return exitUsage
	}
	s, err := openStore(stdout)
	if err == nil {
		_, err = s.Pull(newClient(), args[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	return exitOK
}

func images(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: minibox images")
		return exitUsage
	}
	s, err := openStore(nil)
	var list []image.Image
	if err == nil {
		list, err = s.Images()
	}
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "IMAGE\tMANIFEST\tLAYERS\tSIZE\tPULLED")
	for _, img := range list {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f MB\t%s\n", img.Reference, strings.TrimPrefix(img.Manifest, "sha256:")[:12],
			len(img.Layers), float64(img.Size)/1e6, img.Pulled.Local().Format(time.DateTime))
	}
	_ = tw.Flush()
	return exitOK
}

func rmi(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: minibox rmi IMAGE")
		return exitUsage
	}
	s, err := openStore(nil)
	if err == nil {
		err = s.Remove(args[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	fmt.Fprintln(stdout, "removed", args[0])
	return exitOK
}

// optionalString is a flag that knows whether it was given at all
// (--entrypoint "" clears the image's entrypoint).
type optionalString struct {
	value string
	set   bool
}

func (o *optionalString) String() string     { return o.value }
func (o *optionalString) Set(v string) error { o.value, o.set = v, true; return nil }

func runContainer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("minibox run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootfs := fs.String("rootfs", "", "run on this root `directory` instead of an image; the container writes into it")
	hostname := fs.String("hostname", "", "the container's hostname (default: its id)")
	workdir := fs.String("w", "", "working `directory` inside the container (default: the image's, or /)")
	userSpec := fs.String("u", "", "run as `user`: name, uid, name:group or uid:gid (default: the image's, or root)")
	keep := fs.Bool("keep", false, "keep the container's folder (its writable layer) after it exits")
	stats := fs.Bool("stats", false, "print what the container used when it exits (CPU, memory, forks refused)")
	var memory, swap bytesFlag
	fs.Var(&memory, "memory", "memory limit (`size`: 512m, 1g); no swap unless --swap")
	fs.Var(&swap, "swap", "swap allowed on top of --memory (`size`)")
	cpus := fs.Float64("cpus", 0, "CPU limit, in CPUs (0.5 is half of one)")
	pids := fs.Int64("pids", 0, "most processes and threads at once")
	netMode := fs.String("network", "bridge", "`mode`: bridge (its own address on MiniBox's bridge), none (loopback only) or host (the host's network)")
	var ports listFlag
	fs.Var(&ports, "p", "publish a port: `HOST:CONTAINER`[/tcp|/udp] (repeatable; bridge only)")
	userns := fs.Bool("userns", false, "run in a user namespace, so the container's root is an unprivileged user on the host")
	var entrypoint optionalString
	fs.Var(&entrypoint, "entrypoint", "replace the image's entrypoint (`command`; \"\" for none)")
	var env listFlag
	fs.Var(&env, "e", "set an environment variable (`KEY=value`; repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: minibox run [options] IMAGE [COMMAND [ARG...]]")
		fmt.Fprintln(stderr, "       minibox run [options] --rootfs DIR [--] COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return container.ExitSetup
	}
	rest := fs.Args()

	id := container.NewID()
	if *hostname == "" {
		*hostname = id
	}
	cfg := container.Config{ID: id, Hostname: *hostname, User: *userSpec, UserNS: *userns,
		Limits: cgroup.Limits{Memory: memory.n, Swap: swap.n, CPUs: *cpus, Pids: *pids}}
	if err := cfg.Limits.Validate(); err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return container.ExitSetup
	}
	mode, err := network.ParseMode(*netMode)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return container.ExitSetup
	}
	cfg.NetworkMode = mode
	for _, p := range ports {
		port, err := network.ParsePort(p)
		if err != nil {
			fmt.Fprintln(stderr, "minibox:", err)
			return container.ExitSetup
		}
		cfg.Ports = append(cfg.Ports, port)
	}
	var imageEnv []string
	var cleanup func()

	if *rootfs != "" {
		if len(rest) == 0 || entrypoint.set {
			fs.Usage()
			return container.ExitSetup
		}
		root, err := filepath.Abs(*rootfs)
		if err != nil {
			fmt.Fprintln(stderr, "minibox:", err)
			return container.ExitSetup
		}
		cfg.Rootfs, cfg.Args, cfg.Cwd = root, rest, "/"
	} else {
		if len(rest) == 0 {
			fs.Usage()
			return container.ExitSetup
		}
		name, command := rest[0], rest[1:]
		if len(command) > 0 && command[0] == "--" {
			command = command[1:]
		}
		s, err := openStore(nil)
		if err != nil {
			fmt.Fprintln(stderr, "minibox:", err)
			return container.ExitSetup
		}
		img, imgCfg, err := s.Lookup(name)
		if err != nil {
			fmt.Fprintln(stderr, "minibox:", err)
			return container.ExitSetup
		}
		cfg.Args = imageArgs(imgCfg, entrypoint, command)
		if len(cfg.Args) == 0 {
			fmt.Fprintf(stderr, "minibox: %s has no command to run: give one\n", img.Reference)
			return container.ExitSetup
		}
		cfg.Cwd = imgCfg.Config.WorkingDir
		if cfg.User == "" {
			cfg.User = imgCfg.Config.User
		}
		imageEnv = imgCfg.Config.Env
		ov, err := s.NewOverlay(id, img)
		if err != nil {
			fmt.Fprintln(stderr, "minibox:", err)
			return container.ExitSetup
		}
		cfg.Rootfs, cfg.Lower, cfg.Upper, cfg.Work = ov.Merged, ov.Lower, ov.Upper, ov.Work
		cleanup = func() {
			if *keep {
				fmt.Fprintf(stderr, "minibox: kept %s (its writes are in %s)\n", ov.Dir, ov.Upper)
				return
			}
			_ = os.RemoveAll(ov.Dir)
		}
	}
	if *workdir != "" {
		cfg.Cwd = *workdir
	}
	if cfg.Cwd == "" {
		cfg.Cwd = "/"
	}
	environment, err := container.Environment(*hostname, os.Getenv("TERM"), imageEnv, env)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		if cleanup != nil {
			cleanup()
		}
		return container.ExitSetup
	}
	cfg.Env = environment

	signals := make(chan os.Signal, 16)
	signal.Notify(signals, container.Forwarded...)
	defer signal.Stop(signals)

	res, err := container.Run(cfg, container.Stdio{In: stdin, Out: stdout, Err: stderr}, signals)
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
	}
	if cleanup != nil {
		cleanup()
	}
	u := res.Usage
	if u.OOMKills > 0 {
		fmt.Fprintf(stderr, "minibox: the container went over its memory limit (%s): the kernel killed %d process(es)\n", humanBytes(cfg.Limits.Memory), u.OOMKills)
	}
	if *stats {
		fmt.Fprintf(stderr, "minibox: cpu %.2f s (held back %.2f s by the limit), memory peak %s, killed for memory %d, forks refused %d\n",
			u.CPUSeconds, u.Throttled, humanBytes(u.MemoryPeak), u.OOMKills, u.PidsRefused)
	}
	return res.Code
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// bytesFlag is a size flag: "512m", "1g".
type bytesFlag struct{ n int64 }

func (b *bytesFlag) String() string { return fmt.Sprint(b.n) }
func (b *bytesFlag) Set(v string) error {
	n, err := cgroup.ParseBytes(v)
	b.n = n
	return err
}

// imageArgs is what an image runs, as Docker decides it: the entrypoint
// (the image's, or --entrypoint), then the command given, or else the
// image's Cmd. --entrypoint also drops the image's Cmd.
func imageArgs(cfg image.Config, entrypoint optionalString, command []string) []string {
	ep := cfg.Config.Entrypoint
	cmd := cfg.Config.Cmd
	if entrypoint.set {
		ep, cmd = nil, nil
		if entrypoint.value != "" {
			ep = []string{entrypoint.value}
		}
	}
	if len(command) > 0 {
		cmd = command
	}
	return append(append([]string{}, ep...), cmd...)
}
