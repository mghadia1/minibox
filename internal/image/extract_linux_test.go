//go:build linux

package image

import (
	"archive/tar"
	"bytes"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/testutil"
	"golang.org/x/sys/unix"
)

// layerDir is an empty folder to extract into, inside a folder of its own
// with a "host" sibling that nothing may touch.
func layerDir(t *testing.T) (dir, host string) {
	t.Helper()
	base := t.TempDir()
	dir, host = filepath.Join(base, "layer"), filepath.Join(base, "host")
	for _, d := range []string{dir, host} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(host, "secret"), []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, host
}

func extract(t *testing.T, dir string, entries ...tarEntry) (ExtractStats, error) {
	t.Helper()
	return Extract(bytes.NewReader(makeTar(t, entries...)), dir, 1<<20)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func lstat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestExtractEntries(t *testing.T) {
	testutil.RequireRoot(t)
	dir, _ := layerDir(t)
	owned := file("usr/bin/tool", "#!/bin/sh\n", 0o4755)
	owned.Header.Uid, owned.Header.Gid = 1000, 1001
	fifo := tarEntry{Header: tar.Header{Typeflag: tar.TypeFifo, Name: "run/fifo", Mode: 0o600, ModTime: time.Unix(1700000000, 0)}}
	null := tarEntry{Header: tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3, ModTime: time.Unix(1700000000, 0)}}
	st, err := extract(t, dir,
		folder("etc/"),
		file("etc/hostname", "box\n", 0o644),
		owned,
		symlink("bin", "usr/bin"),
		hardlink("usr/bin/tool-again", "usr/bin/tool"),
		file("./deep/implicit/parents", "x", 0o600),
		fifo, null,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "etc/hostname")); got != "box\n" {
		t.Errorf("etc/hostname = %q", got)
	}
	tool := lstat(t, filepath.Join(dir, "usr/bin/tool"))
	if tool.Uid != 1000 || tool.Gid != 1001 || tool.Mode&0o7777 != 0o4755 {
		t.Errorf("tool: uid %d gid %d mode %o (set-user-id must survive chown)", tool.Uid, tool.Gid, tool.Mode&0o7777)
	}
	if tool.Mtim.Sec != 1700000000 {
		t.Errorf("tool mtime %d", tool.Mtim.Sec)
	}
	if again := lstat(t, filepath.Join(dir, "usr/bin/tool-again")); again.Ino != tool.Ino {
		t.Error("hard link is not the same file")
	}
	if target, _ := os.Readlink(filepath.Join(dir, "bin")); target != "usr/bin" {
		t.Errorf("bin -> %q", target)
	}
	if d := lstat(t, filepath.Join(dir, "deep/implicit")); d.Mode&0o777 != 0o755 {
		t.Errorf("implicit parent mode %o", d.Mode&0o777)
	}
	if f := lstat(t, filepath.Join(dir, "run/fifo")); f.Mode&unix.S_IFMT != unix.S_IFIFO {
		t.Error("fifo is not a fifo")
	}
	if n := lstat(t, filepath.Join(dir, "dev/null")); n.Mode&unix.S_IFMT != unix.S_IFCHR || n.Rdev != unix.Mkdev(1, 3) {
		t.Error("dev/null is not device 1,3")
	}
	if e := lstat(t, filepath.Join(dir, "etc")); e.Mtim.Sec != 1700000000 {
		t.Errorf("etc's mtime %d: directory times must be set after their contents", e.Mtim.Sec)
	}
	if st.Files != 3 || st.Symlinks != 1 || st.Hardlinks != 1 || st.Devices != 2 {
		t.Errorf("stats %+v", st)
	}
}

// Each of these tries to write outside the layer. None may, and the
// layer's own copy is where a link inside it leads.
func TestExtractStaysInside(t *testing.T) {
	testutil.RequireRoot(t)
	for _, tc := range []struct {
		name    string
		entries []tarEntry
		refuse  bool   // the layer must be refused
		inside  string // else: a file that must now exist inside the layer
	}{
		{"dot-dot", []tarEntry{file("../host/secret", "pwned", 0o644)}, true, ""},
		{"dot-dot deeper", []tarEntry{file("a/../../host/secret", "pwned", 0o644)}, true, ""},
		{"absolute name", []tarEntry{file("/host/secret", "pwned", 0o644)}, false, "host/secret"},
		// A link is followed as if the layer were "/": an absolute or
		// climbing link leads to the layer's own folder.
		{"through an absolute link", []tarEntry{folder("inner/"), symlink("out", "/inner"), file("out/secret", "pwned", 0o644)}, false, "inner/secret"},
		{"through a relative link", []tarEntry{folder("host/"), symlink("up", "../../../../../host"), file("up/secret", "pwned", 0o644)}, false, "host/secret"},
		{"through a link to a link", []tarEntry{folder("host/"), symlink("a", "b"), symlink("b", "/../host"), file("a/secret", "pwned", 0o644)}, false, "host/secret"},
		// Where a link leads nowhere inside the layer, the entry is refused
		// (Docker would make the folders; MiniBox is stricter).
		{"through a link to the host's folder", []tarEntry{symlink("out", "HOST"), file("out/secret", "pwned", 0o644)}, true, ""},
		{"over a link", []tarEntry{symlink("f", "HOST/secret"), file("f", "pwned", 0o644)}, false, "f"},
		{"hard link out", []tarEntry{hardlink("h", "../host/secret")}, true, ""},
		{"hard link through a link", []tarEntry{symlink("out", "HOST"), hardlink("h", "out/secret")}, true, ""},
		// A hard link to a symbolic link is to the link itself, not to the
		// host's file it names.
		{"hard link to a link out", []tarEntry{symlink("s", "HOST/secret"), hardlink("h", "s")}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, host := layerDir(t)
			hostDir := filepath.Dir(dir) + "/host"
			for i := range tc.entries {
				tc.entries[i].Header.Linkname = strings.ReplaceAll(tc.entries[i].Header.Linkname, "HOST", hostDir)
			}
			_, err := extract(t, dir, tc.entries...)
			if tc.refuse && err == nil {
				t.Fatal("refused nothing")
			}
			if !tc.refuse && err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(host, "secret")); got != "host data" {
				t.Fatalf("the host's file now holds %q", got)
			}
			if entries, _ := os.ReadDir(host); len(entries) != 1 {
				t.Fatalf("something was added next to the host's file: %v", entries)
			}
			hostSt := lstat(t, filepath.Join(host, "secret"))
			_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
				var st unix.Stat_t
				if err == nil && unix.Lstat(p, &st) == nil && st.Ino == hostSt.Ino && st.Dev == hostSt.Dev {
					t.Fatalf("%s inside the layer is the host's file", p)
				}
				return nil
			})
			if tc.inside != "" {
				p := filepath.Join(dir, strings.ReplaceAll(tc.inside, "HOST", hostDir))
				if got := readFile(t, p); got != "pwned" {
					t.Fatalf("%s inside the layer holds %q", tc.inside, got)
				}
				if st := lstat(t, p); st.Mode&unix.S_IFMT != unix.S_IFREG {
					t.Fatalf("%s is not a regular file", tc.inside)
				}
			}
		})
	}
}

func TestExtractWhiteouts(t *testing.T) {
	testutil.RequireRoot(t)
	dir, _ := layerDir(t)
	fakeOpaque := file("forged/x", "", 0o644)
	fakeOpaque.Header.PAXRecords = map[string]string{"SCHILY.xattr.trusted.overlay.opaque": "y"}
	capFile := file("bin/ping", "", 0o755)
	capFile.Header.PAXRecords = map[string]string{"SCHILY.xattr.user.note": "hello"}
	st, err := extract(t, dir,
		file("etc/.wh.motd", "", 0o644),
		folder("var/cache/"),
		file("var/cache/.wh..wh..opq", "", 0o644),
		file("var/cache/new", "x", 0o644),
		file("a/.wh..wh.plnk", "", 0o644),
		folder("forged/"),
		fakeOpaque,
		capFile,
	)
	if err != nil {
		t.Fatal(err)
	}
	wh := lstat(t, filepath.Join(dir, "etc/motd"))
	if wh.Mode&unix.S_IFMT != unix.S_IFCHR || wh.Rdev != 0 {
		t.Errorf("etc/motd is not a 0/0 character device (mode %o rdev %d)", wh.Mode, wh.Rdev)
	}
	opaque := make([]byte, 8)
	if n, err := unix.Lgetxattr(filepath.Join(dir, "var/cache"), "trusted.overlay.opaque", opaque); err != nil || string(opaque[:n]) != "y" {
		t.Errorf("var/cache not opaque: %v", err)
	}
	if _, err := unix.Lgetxattr(filepath.Join(dir, "forged/x"), "trusted.overlay.opaque", opaque); !errors.Is(err, unix.ENODATA) {
		t.Errorf("a layer set a trusted.* attribute itself (%v)", err)
	}
	if n, err := unix.Lgetxattr(filepath.Join(dir, "bin/ping"), "user.note", opaque); err != nil || string(opaque[:n]) != "hello" {
		t.Errorf("user.note not kept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a/.wh..wh.plnk")); err == nil {
		t.Error("AUFS bookkeeping was written as a file")
	}
	if st.Whiteouts != 1 || st.Opaque != 1 || st.SkippedXattrs != 1 {
		t.Errorf("stats %+v", st)
	}
	for _, bad := range []string{".wh..", ".wh."} {
		d, _ := layerDir(t)
		if _, err := extract(t, d, file("x/"+bad, "", 0o644)); err == nil {
			t.Errorf("whiteout %q accepted", bad)
		}
	}
}

func TestExtractReplaces(t *testing.T) {
	testutil.RequireRoot(t)
	dir, _ := layerDir(t)
	if _, err := extract(t, dir,
		file("a", "file", 0o644), folder("a/"), file("a/inner", "1", 0o644), // file, then a directory
		folder("b/"), file("b/inner", "1", 0o644), file("b", "file", 0o644), // directory, then a file
		folder("c/"), file("c/kept", "1", 0o644), folder("c/"), // directory twice: contents stay
		file("d", "first", 0o644), file("d", "second", 0o600), // the later entry wins
	); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dir, "a/inner")) != "1" || readFile(t, filepath.Join(dir, "b")) != "file" ||
		readFile(t, filepath.Join(dir, "c/kept")) != "1" || readFile(t, filepath.Join(dir, "d")) != "second" {
		t.Fatal("later entries did not replace earlier ones as tar does")
	}
}

func TestExtractLimit(t *testing.T) {
	testutil.RequireRoot(t)
	dir, _ := layerDir(t)
	big := strings.Repeat("x", 600<<10)
	if _, err := extract(t, dir, file("a", big, 0o644), file("b", big, 0o644)); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("1.2 MB through a 1 MB limit: %v", err)
	}
}

// Random layers made of everything awkward: names with "..", links
// pointing anywhere (out of the layer, at each other, in loops), hard
// links, whiteouts, entries replacing each other. Whatever is accepted or
// refused, nothing outside the layer changes, and nothing inside it is a
// path that leads out.
func TestExtractRandomLayersStayInside(t *testing.T) {
	testutil.RequireRoot(t)
	rng := rand.New(rand.NewPCG(1, 2))
	names := []string{"a", "b", "a/b", "b/a", "..", "../host", "a/../..", "/abs", "c/d/e", ".wh.a", "a/.wh..wh..opq", "host", "./a"}
	targets := []string{"/", "..", "../..", "../host", "/host", "a", "b", "a/b", "/abs", "self", "HOST", "HOST/secret"}
	for i := range 300 {
		dir, host := layerDir(t)
		hostDir := filepath.Dir(dir) + "/host"
		var entries []tarEntry
		for range 1 + rng.IntN(8) {
			name := names[rng.IntN(len(names))]
			target := strings.ReplaceAll(targets[rng.IntN(len(targets))], "HOST", hostDir)
			switch rng.IntN(4) {
			case 0:
				entries = append(entries, file(name, "pwned", 0o644))
			case 1:
				entries = append(entries, folder(name))
			case 2:
				entries = append(entries, symlink(name, target))
			case 3:
				entries = append(entries, hardlink(name, target))
			}
		}
		_, _ = extract(t, dir, entries...)
		if got := readFile(t, filepath.Join(host, "secret")); got != "host data" {
			t.Fatalf("layer %d %v changed the host's file", i, entries)
		}
		if es, _ := os.ReadDir(host); len(es) != 1 {
			t.Fatalf("layer %d %v added to the host folder", i, entries)
		}
		// Every hard link inside must be to a file inside: none may share
		// the host file's inode.
		hostSt := lstat(t, filepath.Join(host, "secret"))
		_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
			var st unix.Stat_t
			if err == nil && unix.Lstat(p, &st) == nil && st.Ino == hostSt.Ino && st.Dev == hostSt.Dev {
				t.Fatalf("layer %d: %s is the host's file", i, p)
			}
			return nil
		})
		os.RemoveAll(filepath.Dir(dir))
	}
}
