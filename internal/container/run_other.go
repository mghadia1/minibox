//go:build !linux

package container

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/mghadia1/minibox/internal/cgroup"
)

// Forwarded is empty where containers cannot run.
var Forwarded []os.Signal

// Stdio is what the command reads and writes.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// SetupError is a container that could not be started.
type SetupError struct {
	Code    int
	Message string
}

func (e *SetupError) Error() string { return e.Message }

// Result is how a container ended, and what it used.
type Result struct {
	Code  int
	Usage cgroup.Usage
}

// Run fails anywhere but Linux.
func Run(Config, Stdio, <-chan os.Signal) (Result, error) {
	return Result{Code: ExitSetup}, &SetupError{ExitSetup, fmt.Sprintf("containers need Linux, not %s (on a Mac, the Lima VM in lima/minibox.yaml)", runtime.GOOS)}
}

// Init fails anywhere but Linux.
func Init() {
	fmt.Fprintln(os.Stderr, "minibox init needs Linux")
	os.Exit(ExitSetup)
}
