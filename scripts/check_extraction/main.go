//go:build linux

// Command check_extraction checks MiniBox's layer extraction against GNU
// tar's, on real images: each layer is downloaded (digest checked),
// unpacked by MiniBox and by tar (a reference, used only here), and the
// two trees compared entry by entry: type, mode, owner, size, content
// (SHA-256), hard-link count, link target, device numbers, and the
// extended attributes a layer may carry. tar writes a whiteout as the
// file ".wh.NAME" it is in the layer; MiniBox writes the 0/0 character
// device overlayfs reads; the comparison maps one to the other.
//
//	sudo go run ./scripts/check_extraction alpine:3.22 python:3.14-slim ...
package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mghadia1/minibox/internal/image"
	"golang.org/x/sys/unix"
)

type entry struct {
	kind, mode, owner, size, content, nlink, link, rdev, xattrs string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: check_extraction IMAGE...")
		os.Exit(2)
	}
	failed := false
	for _, name := range os.Args[1:] {
		if err := check(name); err != nil {
			fmt.Printf("%s: %v\n", name, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func check(name string) error {
	ref, err := image.ParseReference(name)
	if err != nil {
		return err
	}
	c := image.NewClient()
	m, _, digest, err := c.Resolve(ref)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("/var/tmp", "check-extraction-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	fmt.Printf("%s (%s): %d layers\n", ref.Short(), digest[:19], len(m.Layers))
	for i, d := range m.Layers {
		blobPath := filepath.Join(work, fmt.Sprintf("layer%d.tar.gz", i))
		if err := download(c, ref, d, blobPath); err != nil {
			return err
		}
		ours, theirs := filepath.Join(work, fmt.Sprintf("ours%d", i)), filepath.Join(work, fmt.Sprintf("tar%d", i))
		for _, dir := range []string{ours, theirs} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
		}
		f, err := os.Open(blobPath)
		if err != nil {
			return err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return err
		}
		st, err := image.Extract(zr, ours, 8<<30)
		f.Close()
		if err != nil {
			return fmt.Errorf("layer %d: minibox: %w", i+1, err)
		}
		cmd := exec.Command("tar", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-xzpf", blobPath, "-C", theirs)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("layer %d: tar: %v\n%s", i+1, err, out)
		}
		n, diffs, err := compare(ours, theirs)
		if err != nil {
			return err
		}
		fmt.Printf("  layer %d: %d entries, %d differences (%d whiteouts, %d opaque, %d xattrs skipped)\n", i+1, n, diffs, st.Whiteouts, st.Opaque, st.SkippedXattrs)
		if diffs > 0 {
			return fmt.Errorf("layer %d differs", i+1)
		}
	}
	return nil
}

func download(c *image.Client, ref image.Reference, d image.Descriptor, path string) error {
	b, err := c.OpenBlob(ref, d)
	if err != nil {
		return err
	}
	defer b.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return b.Verify()
}

func compare(oursDir, refDir string) (int, int, error) {
	ours, err := scan(oursDir, false)
	if err != nil {
		return 0, 0, err
	}
	ref, err := scan(refDir, true)
	if err != nil {
		return 0, 0, err
	}
	paths := map[string]bool{}
	for p := range ours {
		paths[p] = true
	}
	for p := range ref {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	diffs := 0
	for _, p := range sorted {
		a, inA := ours[p]
		b, inB := ref[p]
		switch {
		case !inA:
			fmt.Printf("    only tar has %s\n", p)
			diffs++
		case !inB:
			fmt.Printf("    only minibox has %s\n", p)
			diffs++
		case a != b:
			fmt.Printf("    %s differs:\n      minibox %+v\n      tar     %+v\n", p, a, b)
			diffs++
		}
	}
	return len(sorted), diffs, nil
}

func scan(root string, oci bool) (map[string]entry, error) {
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		e := entry{
			mode:  fmt.Sprintf("%04o", st.Mode&0o7777),
			owner: fmt.Sprintf("%d:%d", st.Uid, st.Gid),
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			e.kind = "dir"
		case unix.S_IFREG:
			e.kind = "file"
			e.size = fmt.Sprint(st.Size)
			e.nlink = fmt.Sprint(st.Nlink)
			sum, err := hashFile(path)
			if err != nil {
				return err
			}
			e.content = sum
		case unix.S_IFLNK:
			e.kind = "symlink"
			e.mode = ""
			e.link, _ = os.Readlink(path)
		case unix.S_IFCHR:
			e.kind = "char"
			e.rdev = fmt.Sprintf("%d,%d", unix.Major(st.Rdev), unix.Minor(st.Rdev))
		case unix.S_IFBLK:
			e.kind = "block"
			e.rdev = fmt.Sprintf("%d,%d", unix.Major(st.Rdev), unix.Minor(st.Rdev))
		case unix.S_IFIFO:
			e.kind = "fifo"
		}
		e.xattrs = xattrs(path)
		base := filepath.Base(rel)
		if oci && base == ".wh..wh..opq" {
			parent := filepath.Dir(rel)
			p := out[parent]
			p.xattrs = strings.TrimPrefix(p.xattrs+",trusted.overlay.opaque=79", ",")
			out[parent] = p
			return nil
		}
		if oci && strings.HasPrefix(base, ".wh.") {
			rel = filepath.Join(filepath.Dir(rel), strings.TrimPrefix(base, ".wh."))
			e = entry{kind: "char", mode: "0000", owner: "0:0", rdev: "0,0"}
		}
		out[rel] = e
		return nil
	})
	return out, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// xattrs lists the attributes a layer may set (security.capability,
// user.*) and overlayfs's opaque mark, sorted.
func xattrs(path string) string {
	buf := make([]byte, 4096)
	n, err := unix.Llistxattr(path, buf)
	if err != nil || n == 0 {
		return ""
	}
	var keep []string
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if name == "security.capability" || strings.HasPrefix(name, "user.") || name == "trusted.overlay.opaque" {
			val := make([]byte, 256)
			m, _ := unix.Lgetxattr(path, name, val)
			keep = append(keep, fmt.Sprintf("%s=%x", name, val[:max(m, 0)]))
		}
	}
	sort.Strings(keep)
	return strings.Join(keep, ",")
}
