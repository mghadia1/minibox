//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/image"
	"github.com/mghadia1/minibox/internal/testutil"
)

const helloScript = `#!/bin/sh
echo "hello $1 from $(id -u):$(id -g) in $(pwd), HOME=$HOME IMAGE_VAR=$IMAGE_VAR"
`

// testImage publishes an image on a fake registry and pulls it with
// `minibox pull` into a store of its own ($MINIBOX_ROOT). Its layers:
// Alpine's minirootfs; then /opt/a, a script, and a whiteout for
// /etc/motd; then /opt made opaque with only /opt/b.
func testImage(t *testing.T) (name, root string) {
	t.Helper()
	testutil.RequireRoot(t)
	r := testutil.NewRegistry(t, "library/test")
	layers := [][]byte{
		testutil.RootfsLayer(t),
		testutil.Tar(t,
			testutil.File("opt/a", "a", 0o644),
			testutil.File("usr/local/bin/hello", helloScript, 0o755),
			testutil.File("etc/.wh.motd", "", 0o644),
		),
		testutil.Tar(t,
			testutil.Folder("opt/"),
			testutil.File("opt/.wh..wh..opq", "", 0o644),
			testutil.File("opt/b", "b", 0o644),
		),
	}
	r.Publish(t, "1", testutil.ImageSpec{
		Layers:     layers,
		Env:        []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "IMAGE_VAR=1"},
		Entrypoint: []string{"/usr/local/bin/hello"},
		Cmd:        []string{"world"},
		WorkingDir: "/work",
		User:       "nobody",
	})
	root = filepath.Join(t.TempDir(), "store")
	t.Setenv("MINIBOX_ROOT", root)
	old := newClient
	newClient = func() *image.Client { c := image.NewClient(); c.Scheme = "http"; return c }
	t.Cleanup(func() { newClient = old })

	name = r.Name("1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pull", name}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("pull: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "3 layers") {
		t.Fatalf("pull printed %q", stdout.String())
	}
	return name, root
}

func minibox1(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, nil, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestRunAnImage(t *testing.T) {
	name, _ := testImage(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		// The image's entrypoint, cmd, working directory (made, as it did
		// not exist), user and environment; HOME from nobody's passwd entry.
		{[]string{name}, "hello world from 65534:65534 in /work, HOME=/ IMAGE_VAR=1"},
		{[]string{name, "there"}, "hello there from 65534:65534 in /work, HOME=/ IMAGE_VAR=1"},
		{[]string{"-u", "root", "-w", "/tmp", "-e", "IMAGE_VAR=2", name}, "hello world from 0:0 in /tmp, HOME=/root IMAGE_VAR=2"},
		{[]string{"-u", "1234:5678", name}, "hello world from 1234:5678 in /work, HOME=/ IMAGE_VAR=1"},
		{[]string{"--entrypoint", "/bin/echo", name, "replaced"}, "replaced"},
		{[]string{"--entrypoint", "", name, "id", "-un"}, "nobody"},
	} {
		stdout, stderr, code := minibox1(t, append([]string{"run"}, tc.args...)...)
		if code != 0 || strings.TrimSpace(stdout) != tc.want {
			t.Errorf("run %q: exit %d, %q (stderr %q); want %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
}

func TestLayersAreMerged(t *testing.T) {
	name, _ := testImage(t)
	// The whiteout hides Alpine's /etc/motd; the opaque /opt hides /opt/a.
	stdout, stderr, code := minibox1(t, "run", "-u", "root", "--entrypoint", "", name, "sh", "-c", "test -e /etc/motd && echo motd; ls /opt; cat /etc/alpine-release")
	lines := strings.Fields(stdout)
	if code != 0 || len(lines) != 2 || lines[0] != "b" || !strings.HasPrefix(lines[1], "3.22.") {
		t.Fatalf("exit %d, %q (stderr %q); want only /opt/b and Alpine 3.22", code, stdout, stderr)
	}
}

func TestWritesStayInTheContainer(t *testing.T) {
	name, root := testImage(t)
	_, stderr, code := minibox1(t, "run", "--keep", "-u", "root", "--entrypoint", "", name, "sh", "-c", "echo x > /etc/written && rm /etc/os-release")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	kept, _ := filepath.Glob(filepath.Join(root, "containers", "*", "upper"))
	if len(kept) != 1 {
		t.Fatalf("--keep kept %q", kept)
	}
	upper := kept[0]
	if data, err := os.ReadFile(filepath.Join(upper, "etc/written")); err != nil || string(data) != "x\n" {
		t.Fatalf("the write is not in the container's layer: %q, %v", data, err)
	}
	var st os.FileInfo
	if st, _ = os.Lstat(filepath.Join(upper, "etc/os-release")); st == nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("the deletion is not a whiteout in the container's layer: %v", st)
	}
	// The image's layers are untouched, and a new container sees the image.
	layers, _ := filepath.Glob(filepath.Join(root, "layers", "*", "etc", "os-release"))
	if len(layers) != 1 {
		t.Fatalf("Alpine's os-release in the layers: %q", layers)
	}
	stdout, _, code := minibox1(t, "run", "-u", "root", "--entrypoint", "", name, "sh", "-c", "test -e /etc/written && echo leaked; test -e /etc/os-release && echo intact")
	if code != 0 || strings.TrimSpace(stdout) != "intact" {
		t.Fatalf("a second container saw %q", stdout)
	}
	// Without --keep, a container's folder goes with it.
	if dirs, _ := filepath.Glob(filepath.Join(root, "containers", "*")); len(dirs) != 1 {
		t.Fatalf("containers left: %q (only the kept one should be)", dirs)
	}
}

func TestTheOverlayIsNotOnTheHost(t *testing.T) {
	name, root := testImage(t)
	// The container says it is ready by writing a file (its writes land in
	// its upper folder on the host), then waits for a line on stdin. Not a
	// file written into the upper folder from outside: overlayfs does not
	// promise to show changes made under it while mounted.
	release, stdin := io.Pipe()
	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- run([]string{"run", "-u", "root", "--entrypoint", "", name, "sh", "-c", "touch /ready; read line"}, release, &stdout, &stderr)
	}()
	var upper string
	deadline := time.Now().Add(10 * time.Second)
	for upper == "" && time.Now().Before(deadline) {
		if m, _ := filepath.Glob(filepath.Join(root, "containers", "*", "upper", "ready")); len(m) == 1 {
			upper = filepath.Dir(m[0])
		}
		time.Sleep(20 * time.Millisecond)
	}
	mountinfo, _ := os.ReadFile("/proc/self/mountinfo")
	_, _ = io.WriteString(stdin, "go\n")
	stdin.Close()
	if code := <-done; code != 0 || upper == "" {
		t.Fatalf("exit %d, started %v", code, upper != "")
	}
	if strings.Contains(string(mountinfo), root) {
		t.Fatal("the container's overlay is mounted on the host")
	}
	if after, _ := os.ReadFile("/proc/self/mountinfo"); strings.Contains(string(after), root) {
		t.Fatal("a mount is left on the host after the container")
	}
}

func TestImagesAndRmi(t *testing.T) {
	name, root := testImage(t)
	stdout, _, code := minibox1(t, "images")
	if code != 0 || !strings.Contains(stdout, strings.TrimSuffix(name, ":1")+":1") || !strings.Contains(stdout, "IMAGE") {
		t.Fatalf("images: %d %q", code, stdout)
	}
	if _, stderr, code := minibox1(t, "rmi", name); code != 0 {
		t.Fatalf("rmi: %s", stderr)
	}
	if layers, _ := os.ReadDir(filepath.Join(root, "layers")); len(layers) != 0 {
		t.Fatalf("layers left after rmi: %d", len(layers))
	}
	if _, stderr, code := minibox1(t, "run", name); code != 125 || !strings.Contains(stderr, "minibox pull") {
		t.Fatalf("run after rmi: %d %q", code, stderr)
	}
}
