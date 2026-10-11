package cgroup

import (
	"slices"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"1048576": 1 << 20, "512m": 512 << 20, "1G": 1 << 30, "300000k": 300000 << 10, "64b": 64, " 2g ": 2 << 30} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "m", "-1m", "1.5g", "1t", "99999999999999999999", "9223372036854775807g"} {
		if got, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want an error", bad, got)
		}
	}
}

func TestFiles(t *testing.T) {
	for _, tc := range []struct {
		l    Limits
		want [][2]string
	}{
		{Limits{}, nil},
		{Limits{Memory: 64 << 20}, [][2]string{{"memory.max", "67108864"}, {"memory.swap.max", "0"}}},
		{Limits{Memory: 64 << 20, Swap: -1}, [][2]string{{"memory.max", "67108864"}, {"memory.swap.max", "max"}}},
		{Limits{Memory: 64 << 20, Swap: 1 << 20}, [][2]string{{"memory.max", "67108864"}, {"memory.swap.max", "1048576"}}},
		{Limits{CPUs: 0.5}, [][2]string{{"cpu.max", "50000 100000"}}},
		{Limits{CPUs: 2}, [][2]string{{"cpu.max", "200000 100000"}}},
		{Limits{Pids: 64}, [][2]string{{"pids.max", "64"}}},
	} {
		if got := tc.l.files(); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: files %q, want %q", tc.l, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, bad := range []Limits{{Memory: 1 << 20}, {Swap: 5}, {Memory: 64 << 20, Swap: -2}, {CPUs: -1}, {CPUs: 0.001}, {Pids: 3}, {Pids: -1}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	for _, good := range []Limits{{}, {Memory: 6 << 20}, {CPUs: 0.01}, {Pids: 8}, {Memory: 1 << 30, Swap: -1, CPUs: 1.5, Pids: 100}} {
		if err := good.Validate(); err != nil {
			t.Errorf("%+v refused: %v", good, err)
		}
	}
}

func TestKeyValues(t *testing.T) {
	got := keyValues("low 0\nhigh 3\noom 1\noom_kill 2\nbad line here\nnan x\n")
	if got["oom_kill"] != 2 || got["high"] != 3 || len(got) != 4 {
		t.Fatalf("keyValues = %v", got)
	}
}
