package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/mghadia1/minibox/internal/oci"
)

// The OCI runtime lifecycle commands. Each takes a container id; create
// also takes a bundle. On failure they print to stderr and return 1, as
// an OCI runtime does.

func ociCreate(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bundle string
	fs.StringVar(&bundle, "bundle", ".", "path to the bundle (its config.json and root filesystem)")
	fs.StringVar(&bundle, "b", ".", "short for --bundle")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: minibox create [--bundle DIR] ID") }
	if err := fs.Parse(args); err != nil {
		return exitFail
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	if err := oci.Create(fs.Arg(0), bundle); err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	return exitOK
}

func ociStart(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: minibox start ID")
		return exitUsage
	}
	if err := oci.Start(args[0]); err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	return exitOK
}

func ociState(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: minibox state ID")
		return exitUsage
	}
	st, err := oci.GetState(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	fmt.Fprintln(stdout, string(data))
	return exitOK
}

func ociKill(args []string, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(stderr, "usage: minibox kill ID [SIGNAL]")
		return exitUsage
	}
	signal := "TERM"
	if len(args) == 2 {
		signal = args[1]
	}
	if err := oci.Kill(args[0], signal); err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	return exitOK
}

func ociDelete(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "kill the container if it is still running, then delete it")
	fs.BoolVar(force, "f", false, "short for --force")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: minibox delete [--force] ID") }
	if err := fs.Parse(args); err != nil {
		return exitFail
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	if err := oci.Delete(fs.Arg(0), *force); err != nil {
		fmt.Fprintln(stderr, "minibox:", err)
		return exitFail
	}
	return exitOK
}
