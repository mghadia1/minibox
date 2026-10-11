// Package cgroup gives each container a cgroup v2 group of its own, with
// its limits, and reads back what happened in it: whether the kernel
// killed something for memory, how much CPU and memory it used.
package cgroup

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Limits for one container. Zero means no limit.
type Limits struct {
	Memory int64   // bytes: memory.max
	Swap   int64   // bytes of swap on top of Memory: memory.swap.max; -1 is no limit
	CPUs   float64 // CPUs' worth of time: cpu.max
	Pids   int64   // processes and threads: pids.max
}

// The period cpu.max is written with: 100 ms, as Docker writes it.
const cpuPeriod = 100_000

// ParseBytes reads a size as Docker's flags take it: "512m", "1g",
// "300000k", "1048576" (bytes).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'b':
		s = s[:len(s)-1]
	case 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("size %q is not a number with an optional b, k, m or g", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n * mult, nil
}

// Validate checks limits a kernel would refuse, or that make no sense.
func (l Limits) Validate() error {
	switch {
	case l.Memory != 0 && l.Memory < 6<<20:
		return fmt.Errorf("a memory limit under 6 MiB leaves no room for the container's init")
	case l.Swap < -1:
		return fmt.Errorf("swap %d: use -1 for no limit", l.Swap)
	case l.Swap != 0 && l.Memory == 0:
		return fmt.Errorf("a swap limit needs a memory limit")
	case l.CPUs < 0 || (l.CPUs > 0 && l.CPUs*cpuPeriod < 1000):
		return fmt.Errorf("--cpus %g: at least 0.01", l.CPUs)
	case l.Pids < 0 || (l.Pids > 0 && l.Pids < 8):
		// Init alone is several threads: Go's runtime.
		return fmt.Errorf("--pids %d: at least 8 (MiniBox's init is a Go program, which runs several threads)", l.Pids)
	}
	return nil
}

// files are the cgroup files to write for these limits, in order. Swap
// defaults to none when memory is limited, so a container over its limit
// is stopped rather than pushed into swap.
func (l Limits) files() [][2]string {
	var out [][2]string
	if l.Memory > 0 {
		out = append(out, [2]string{"memory.max", strconv.FormatInt(l.Memory, 10)})
		swap := "0"
		if l.Swap == -1 {
			swap = "max"
		} else if l.Swap > 0 {
			swap = strconv.FormatInt(l.Swap, 10)
		}
		out = append(out, [2]string{"memory.swap.max", swap})
	}
	if l.CPUs > 0 {
		quota := int64(math.Round(l.CPUs * cpuPeriod))
		out = append(out, [2]string{"cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)})
	}
	if l.Pids > 0 {
		out = append(out, [2]string{"pids.max", strconv.FormatInt(l.Pids, 10)})
	}
	return out
}

// Usage is what a container used, read when it has finished.
type Usage struct {
	OOMKills    int64   // processes the kernel killed for memory (memory.events oom_kill)
	MemoryPeak  int64   // bytes, highest memory use (memory.peak; 0 if the kernel lacks it)
	CPUSeconds  float64 // user + system time (cpu.stat usage_usec)
	Throttled   float64 // seconds the CPU limit held it back (cpu.stat throttled_usec)
	PidsRefused int64   // forks refused at the pids limit (pids.events max)
}

// keyValues reads "key value" lines (memory.events, cpu.stat).
func keyValues(text string) map[string]int64 {
	out := map[string]int64{}
	for line := range strings.Lines(text) {
		f := strings.Fields(line)
		if len(f) == 2 {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				out[f[0]] = n
			}
		}
	}
	return out
}
