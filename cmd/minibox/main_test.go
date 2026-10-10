package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mghadia1/minibox/internal/host"
)

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string // in stdout or stderr
	}{
		{nil, exitUsage, "usage: minibox"},
		{[]string{"bogus"}, exitUsage, `unknown command "bogus"`},
		{[]string{"help"}, exitOK, "commands:"},
		{[]string{"version"}, exitOK, "minibox dev"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, nil, &stdout, &stderr)
		if code != tc.code {
			t.Errorf("run(%q) = %d, want %d", tc.args, code, tc.code)
		}
		if out := stdout.String() + stderr.String(); !strings.Contains(out, tc.out) {
			t.Errorf("run(%q) printed %q, want it to contain %q", tc.args, out, tc.out)
		}
	}
}

// check's exit code follows its report: 1 exactly when a check failed;
// a warning alone is not a failure.
func TestCheckExitCode(t *testing.T) {
	defer func(f func() host.Report) { checkHost = f }(checkHost)
	for _, tc := range []struct {
		report host.Report
		code   int
	}{
		{host.Report{{Name: "root", Status: host.OK, Detail: "uid 0"}}, exitOK},
		{host.Report{{Name: "nftables", Status: host.Warn, Detail: "not found"}}, exitOK},
		{host.Report{{Name: "root", Status: host.OK, Detail: "uid 0"}, {Name: "cgroup v2", Status: host.Fail, Detail: "v1"}}, exitFail},
	} {
		checkHost = func() host.Report { return tc.report }
		var stdout, stderr bytes.Buffer
		if code := run([]string{"check"}, nil, &stdout, &stderr); code != tc.code {
			t.Errorf("check with %v exited %d, want %d", tc.report, code, tc.code)
		}
		if !strings.Contains(stdout.String(), tc.report[len(tc.report)-1].Detail) {
			t.Errorf("check did not print its report: %q", stdout.String())
		}
	}
}
