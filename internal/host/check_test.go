package host

import (
	"bytes"
	"os"
	"slices"
	"testing"
)

func TestFilesystems(t *testing.T) {
	text := "nodev\tsysfs\nnodev\tcgroup2\n\text4\nnodev\toverlay\n\n"
	got := filesystems(text)
	want := []string{"sysfs", "cgroup2", "ext4", "overlay"}
	if !slices.Equal(got, want) {
		t.Fatalf("filesystems = %q, want %q", got, want)
	}
}

func TestKernelRelease(t *testing.T) {
	for _, tc := range []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"6.8.0-85-generic\n", 6, 8, true},
		{"5.15.167.4-microsoft-standard-WSL2", 5, 15, true},
		{"6.12", 6, 12, true},
		{"linux", 0, 0, false},
		{"", 0, 0, false},
	} {
		major, minor, ok := kernelRelease(tc.in)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("kernelRelease(%q) = %d, %d, %v; want %d, %d, %v", tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

func TestMissing(t *testing.T) {
	got := missing([]string{"cpu", "memory", "pids"}, []string{"pids", "io", "cpu"})
	if !slices.Equal(got, []string{"memory"}) {
		t.Fatalf("missing = %q", got)
	}
	if got := missing(Controllers, Controllers); got != nil {
		t.Fatalf("missing of a set from itself = %q", got)
	}
}

func TestReport(t *testing.T) {
	r := Report{
		{"root", OK, "uid 0"},
		{"nftables", Warn, "nft not found"},
	}
	if r.Failed() {
		t.Fatal("a warning is not a failure")
	}
	var buf bytes.Buffer
	if err := r.Write(&buf); err != nil {
		t.Fatal(err)
	}
	want := "ok   root: uid 0\nwarn nftables: nft not found\n"
	if buf.String() != want {
		t.Fatalf("Write = %q, want %q", buf.String(), want)
	}
	r = append(r, Result{"cgroup v2", Fail, "not cgroup v2"})
	if !r.Failed() {
		t.Fatal("a failed check must fail the report")
	}
}

// On the machine the tests run on, every check gives a result. Where
// containers must work (the VM and CI set MINIBOX_TEST_REQUIRE=1), none
// may fail.
func TestCheckRunsEverywhere(t *testing.T) {
	r := Check()
	if len(r) == 0 {
		t.Fatal("no results")
	}
	for _, res := range r {
		if res.Name == "" || res.Detail == "" {
			t.Errorf("result without a name or detail: %+v", res)
		}
	}
	if os.Getenv("MINIBOX_TEST_REQUIRE") == "1" && r.Failed() {
		var buf bytes.Buffer
		_ = r.Write(&buf)
		t.Fatalf("this machine must run containers, but:\n%s", buf.String())
	}
}
