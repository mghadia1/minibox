//go:build !linux

package host

import "runtime"

// Check on any other system: containers are a Linux kernel feature.
func Check() Report {
	return Report{{
		Name:   "operating system",
		Status: Fail,
		Detail: runtime.GOOS + ": MiniBox needs Linux (on a Mac, the Lima VM in lima/minibox.yaml)",
	}}
}
