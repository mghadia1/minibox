//go:build !linux

package image

import (
	"errors"
	"io"
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

// Extract needs Linux: openat2, whiteout devices and trusted.* attributes.
func Extract(io.Reader, string, int64) (ExtractStats, error) {
	return ExtractStats{}, errors.New("unpacking layers needs Linux")
}
