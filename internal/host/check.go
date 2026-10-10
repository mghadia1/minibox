// Package host checks that a machine can run MiniBox's containers: Linux,
// root, the namespaces, cgroup v2 with the controllers MiniBox uses, and
// overlayfs. Each check is a Result; any failure means containers cannot
// run, a warning means a later feature will not work.
package host

import (
	"fmt"
	"io"
	"strings"
)

// Status of one check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	default:
		return "FAIL"
	}
}

// Result is one check's outcome, in words a person can act on.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Report is every check, in the order they ran.
type Report []Result

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, res := range r {
		if res.Status == Fail {
			return true
		}
	}
	return false
}

// Write prints one line per check: "ok   cgroup v2: cpu memory pids".
func (r Report) Write(w io.Writer) error {
	for _, res := range r {
		if _, err := fmt.Fprintf(w, "%-4s %s: %s\n", res.Status, res.Name, res.Detail); err != nil {
			return err
		}
	}
	return nil
}

// Namespaces MiniBox uses (Milestones 1-5), as named under /proc/self/ns.
var Namespaces = []string{"uts", "pid", "mnt", "ipc", "net", "user", "cgroup"}

// Controllers MiniBox's limits need in the cgroup v2 hierarchy (Milestone 3).
var Controllers = []string{"cpu", "memory", "pids"}

// missing returns the names in want that are not in have, in want's order.
func missing(want, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

// filesystems reads /proc/filesystems ("nodev\toverlay" or "\text4" lines)
// into the names of the filesystems the kernel supports.
func filesystems(text string) []string {
	var names []string
	for line := range strings.SplitSeq(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			names = append(names, fields[len(fields)-1])
		}
	}
	return names
}

// kernelRelease turns the first field of /proc/sys/kernel/osrelease (or
// uname's release, "6.8.0-85-generic") into its major and minor numbers.
func kernelRelease(release string) (major, minor int, ok bool) {
	release = strings.TrimSpace(release)
	if _, err := fmt.Sscanf(release, "%d.%d", &major, &minor); err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
