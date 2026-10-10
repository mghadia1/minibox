//go:build linux

package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ExtractStats counts what a layer held.
type ExtractStats struct {
	Files, Dirs, Symlinks, Hardlinks, Devices int
	Whiteouts, Opaque                         int
	SkippedXattrs                             int
	Bytes                                     int64
}

// ErrUnsafe marks a layer entry that would reach outside its directory.
var ErrUnsafe = errors.New("unsafe layer entry")

// Extract unpacks one layer, an uncompressed tar stream, into dir (empty,
// on the host) as overlayfs reads a layer: a whiteout ".wh.NAME" becomes a
// 0/0 character device named NAME, and ".wh..wh..opq" marks its directory
// opaque (trusted.overlay.opaque=y). maxBytes caps the files' total size
// (a small compressed layer can hold a huge one).
//
// A layer is untrusted input. Nothing is written outside dir: every parent
// directory is opened with openat2(RESOLVE_IN_ROOT), which resolves ".."
// and symbolic links as if dir were "/", and the entry itself is made with
// the *at calls on that directory, never following a link in its place.
func Extract(r io.Reader, dir string, maxBytes int64) (ExtractStats, error) {
	var st ExtractStats
	root, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer unix.Close(root)
	x := &extractor{root: root, maxBytes: maxBytes, st: &st}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("reading the layer: %w", err)
		}
		if err := x.entry(hdr, tr); err != nil {
			return st, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	// Directory times last: making entries inside a directory changes its own.
	for i := len(x.dirTimes) - 1; i >= 0; i-- {
		d := x.dirTimes[i]
		if err := x.setTimes(d.path, d.mtime); err != nil {
			return st, err
		}
	}
	return st, nil
}

type extractor struct {
	root     int
	maxBytes int64
	st       *ExtractStats
	dirTimes []dirTime
}

type dirTime struct {
	path  string
	mtime time.Time
}

// clean turns an entry's name into a path below the layer's root, or
// refuses it. "./a/b", "/a/b" and "a//b/" are all "a/b".
func clean(name string) (string, error) {
	p := path.Clean("/" + name)
	if p == "/" {
		return ".", nil
	}
	// path.Clean("/"+x) cannot climb above "/", but a name that tried to
	// is refused rather than quietly moved.
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q climbs out with \"..\"", ErrUnsafe, name)
		}
	}
	return p[1:], nil
}

// openDir opens a directory below the root, resolving links inside it
// only. With create, missing directories on the way are made (0755, as
// Docker makes them).
func (x *extractor) openDir(rel string, create bool) (int, error) {
	how := unix.OpenHow{
		Flags:   unix.O_DIRECTORY | unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	fd, err := unix.Openat2(x.root, rel, &how)
	if err == nil || !create || !errors.Is(err, unix.ENOENT) || rel == "." {
		return fd, err
	}
	parent, err := x.openDir(path.Dir(rel), true)
	if err != nil {
		return -1, err
	}
	err = unix.Mkdirat(parent, path.Base(rel), 0o755)
	unix.Close(parent)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	return unix.Openat2(x.root, rel, &how)
}

func (x *extractor) entry(hdr *tar.Header, body io.Reader) error {
	rel, err := clean(hdr.Name)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil // the layer's root itself: its own directory already
	}
	dirRel, base := path.Dir(rel), path.Base(rel)
	parent, err := x.openDir(dirRel, true)
	if err != nil {
		return fmt.Errorf("opening its directory: %w", err)
	}
	defer unix.Close(parent)

	if strings.HasPrefix(base, ".wh.") {
		return x.whiteout(parent, dirRel, base)
	}

	// Whatever is at this name in the layer already is replaced, except
	// that a directory stays a directory when a directory comes again.
	var existing unix.Stat_t
	if err := unix.Fstatat(parent, base, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		isDir := existing.Mode&unix.S_IFMT == unix.S_IFDIR
		if !(isDir && hdr.Typeflag == tar.TypeDir) {
			if err := removeAt(parent, base); err != nil {
				return err
			}
		}
	}

	mode := uint32(hdr.Mode) & 0o7777
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(parent, base, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		x.st.Dirs++
		x.dirTimes = append(x.dirTimes, dirTime{rel, hdr.ModTime})
	case tar.TypeReg, tar.TypeRegA:
		if x.st.Bytes+hdr.Size > x.maxBytes {
			return fmt.Errorf("the layer's files exceed %d bytes", x.maxBytes)
		}
		fd, err := unix.Openat(parent, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), rel)
		n, err := io.Copy(f, io.LimitReader(body, hdr.Size))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if n != hdr.Size {
			return fmt.Errorf("%d bytes of %d", n, hdr.Size)
		}
		if closeErr != nil {
			return closeErr
		}
		x.st.Files++
		x.st.Bytes += n
	case tar.TypeSymlink:
		// The target is kept as written: it is read inside the container,
		// where "/" is the container's root.
		if err := unix.Symlinkat(hdr.Linkname, parent, base); err != nil {
			return err
		}
		x.st.Symlinks++
	case tar.TypeLink:
		target, err := clean(hdr.Linkname)
		if err != nil || target == "." {
			return fmt.Errorf("%w: hard link to %q", ErrUnsafe, hdr.Linkname)
		}
		targetDir, err := x.openDir(path.Dir(target), false)
		if err != nil {
			return fmt.Errorf("hard link target %q: %w", hdr.Linkname, err)
		}
		// Flags 0: a link to a symbolic link is to the link itself.
		err = unix.Linkat(targetDir, path.Base(target), parent, base, 0)
		unix.Close(targetDir)
		if err != nil {
			return fmt.Errorf("hard link to %q: %w", hdr.Linkname, err)
		}
		x.st.Hardlinks++
		return nil // a hard link shares its target's owner, mode and times
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		kind := map[byte]uint32{tar.TypeChar: unix.S_IFCHR, tar.TypeBlock: unix.S_IFBLK, tar.TypeFifo: unix.S_IFIFO}[hdr.Typeflag]
		dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
		if err := unix.Mknodat(parent, base, kind|0o600, int(dev)); err != nil {
			return err
		}
		x.st.Devices++
	case tar.TypeXGlobalHeader:
		return nil
	default:
		return fmt.Errorf("unsupported entry type %q", hdr.Typeflag)
	}

	if err := unix.Fchownat(parent, base, hdr.Uid, hdr.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeSymlink {
		// After chown, which clears set-user-id bits.
		if err := unix.Fchmodat(parent, base, mode, 0); err != nil {
			return err
		}
	}
	if err := x.xattrs(parent, base, hdr); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeDir {
		ts := []unix.Timespec{unix.NsecToTimespec(hdr.ModTime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
		return unix.UtimesNanoAt(parent, base, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
	return nil
}

// whiteout records a deletion: ".wh..wh..opq" hides everything below its
// directory in the layers beneath; ".wh.NAME" hides NAME.
func (x *extractor) whiteout(parent int, dirRel, base string) error {
	if base == ".wh..wh..opq" {
		fd, err := x.openDir(dirRel, false)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		if err := unix.Fsetxattr(fd, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
			return fmt.Errorf("marking %s opaque: %w", dirRel, err)
		}
		x.st.Opaque++
		return nil
	}
	if strings.HasPrefix(base, ".wh..wh.") {
		return nil // other AUFS bookkeeping, meaningless here
	}
	name := strings.TrimPrefix(base, ".wh.")
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: whiteout %q", ErrUnsafe, base)
	}
	if err := removeAt(parent, name); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Mknodat(parent, name, unix.S_IFCHR, 0); err != nil {
		return fmt.Errorf("whiteout for %s: %w", name, err)
	}
	x.st.Whiteouts++
	return nil
}

// xattrs applies the extended attributes a layer carries (PAX records
// SCHILY.xattr.*) that are safe to: file capabilities and user.*. Never
// trusted.* (a layer could forge overlayfs's own whiteouts and opaque
// marks with them) nor anything else.
func (x *extractor) xattrs(parent int, base string, hdr *tar.Header) error {
	for key, value := range hdr.PAXRecords {
		name, ok := strings.CutPrefix(key, "SCHILY.xattr.")
		if !ok {
			continue
		}
		if name != "security.capability" && !strings.HasPrefix(name, "user.") {
			x.st.SkippedXattrs++
			continue
		}
		if hdr.Typeflag == tar.TypeSymlink {
			x.st.SkippedXattrs++
			continue
		}
		fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_PATH, 0)
		if err != nil {
			return err
		}
		// An O_PATH descriptor cannot carry xattrs; its /proc path can.
		err = unix.Setxattr(fmt.Sprintf("/proc/self/fd/%d", fd), name, []byte(value), 0)
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("xattr %s: %w", name, err)
		}
	}
	return nil
}

func (x *extractor) setTimes(rel string, mtime time.Time) error {
	ts := []unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	if rel == "." {
		return unix.UtimesNanoAt(x.root, ".", ts, 0)
	}
	parent, err := x.openDir(path.Dir(rel), false)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return unix.UtimesNanoAt(parent, path.Base(rel), ts, unix.AT_SYMLINK_NOFOLLOW)
}

// removeAt removes name in the directory parent, a directory with
// everything in it.
func removeAt(parent int, name string) error {
	err := unix.Unlinkat(parent, name, 0)
	if err == nil || !(errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM)) {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	names, err := dir.Readdirnames(-1)
	if err != nil {
		dir.Close()
		return err
	}
	for _, n := range names {
		if err := removeAt(fd, n); err != nil {
			dir.Close()
			return err
		}
	}
	dir.Close()
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
