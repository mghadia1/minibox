package image

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultRoot is where images and containers live.
const DefaultRoot = "/var/lib/minibox"

const (
	maxConfigSize = 4 << 20
	// The most a layer may unpack to: the VM's disk is 10 GiB.
	defaultMaxLayerBytes = 4 << 30
)

// Store keeps images on disk:
//
//	blobs/sha256/HEX      manifests and configs, by digest
//	layers/HEX            each layer unpacked, named by its diff_id (the
//	                      digest of the uncompressed tar), shared by images
//	images.json           image references -> manifest, config, layers
//	containers/ID/        upper, work, merged: one container's overlay
type Store struct {
	Root          string
	MaxLayerBytes int64
	Log           io.Writer // progress, one line per step
}

// Image is one pulled image, as images.json records it.
type Image struct {
	Reference string    `json:"reference"` // "alpine:3.22"
	Manifest  string    `json:"manifest"`  // digest
	Config    string    `json:"config"`    // digest
	Layers    []string  `json:"layers"`    // diff_ids, bottom first
	Size      int64     `json:"size"`      // bytes unpacked
	Pulled    time.Time `json:"pulled"`
}

// Open makes the store's folders if needed.
func Open(root string, log io.Writer) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "layers", "containers"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	if log == nil {
		log = io.Discard
	}
	return &Store{Root: root, MaxLayerBytes: defaultMaxLayerBytes, Log: log}, nil
}

// lock takes the store's lock (flock on a file): one pull, removal or
// container set-up at a time.
func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (s *Store) logf(format string, args ...any) { fmt.Fprintf(s.Log, format+"\n", args...) }

// Pull fetches an image and unpacks its layers, checking every digest:
// the manifest's, the config's, each compressed layer's and each
// uncompressed layer's diff_id. A layer is renamed into place only once
// all of them match.
func (s *Store) Pull(c *Client, name string) (Image, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return Image{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Image{}, err
	}
	defer unlock()

	m, body, digest, err := c.Resolve(ref)
	if err != nil {
		return Image{}, err
	}
	configData, err := c.ReadBlob(ref, m.Config, maxConfigSize)
	if err != nil {
		return Image{}, err
	}
	var cfg Config
	if err := json.Unmarshal(configData, &cfg); err != nil {
		return Image{}, fmt.Errorf("%s: config: %w", ref.Short(), err)
	}
	goos, arch, _ := HostPlatform()
	if cfg.OS != goos || cfg.Architecture != arch {
		return Image{}, fmt.Errorf("%s: the image is for %s/%s, this machine is %s/%s", ref.Short(), cfg.OS, cfg.Architecture, goos, arch)
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return Image{}, fmt.Errorf("%s: %d layers but %d diff_ids", ref.Short(), len(m.Layers), len(cfg.RootFS.DiffIDs))
	}

	var size int64
	for i, layer := range m.Layers {
		diffID := cfg.RootFS.DiffIDs[i]
		n, err := s.pullLayer(c, ref, layer, diffID)
		if err != nil {
			return Image{}, fmt.Errorf("%s: layer %d of %d: %w", ref.Short(), i+1, len(m.Layers), err)
		}
		size += n
	}
	if err := s.writeBlob(digest, body); err != nil {
		return Image{}, err
	}
	if err := s.writeBlob(m.Config.Digest, configData); err != nil {
		return Image{}, err
	}
	img := Image{Reference: ref.Short(), Manifest: digest, Config: m.Config.Digest, Layers: cfg.RootFS.DiffIDs, Size: size, Pulled: time.Now().UTC()}
	images, err := s.readImages()
	if err != nil {
		return Image{}, err
	}
	images[img.Reference] = img
	if err := s.writeImages(images); err != nil {
		return Image{}, err
	}
	s.logf("%s: %d layers, %s", ref.Short(), len(m.Layers), digest)
	return img, nil
}

// pullLayer streams one layer from the registry through gunzip into a
// temporary folder, then checks both digests before renaming it into
// place. It returns the bytes unpacked.
func (s *Store) pullLayer(c *Client, ref Reference, d Descriptor, diffID string) (int64, error) {
	if !digestPattern.MatchString(diffID) {
		return 0, fmt.Errorf("diff_id %q is not sha256", diffID)
	}
	final := filepath.Join(s.Root, "layers", strings.TrimPrefix(diffID, "sha256:"))
	if _, err := os.Stat(final); err == nil {
		s.logf("  %s already here", short(diffID))
		return dirSize(final), nil
	}
	var gz bool
	switch d.MediaType {
	case MediaOCILayerGzip, MediaDockerLayerGzip:
		gz = true
	case MediaOCILayer:
	case MediaOCILayerZstd:
		return 0, errors.New("zstd layers are not supported (Go's standard library has no zstd)")
	default:
		return 0, fmt.Errorf("unsupported layer type %q", d.MediaType)
	}
	tmp := filepath.Join(s.Root, "layers", ".tmp-"+randomHex())
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return 0, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(tmp)
		}
	}()

	blob, err := c.OpenBlob(ref, d)
	if err != nil {
		return 0, err
	}
	defer blob.Close()
	var tarStream io.Reader = blob
	if gz {
		zr, err := gzip.NewReader(blob)
		if err != nil {
			return 0, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		tarStream = zr
	}
	h := sha256.New()
	tee := io.TeeReader(tarStream, h)
	st, err := Extract(tee, tmp, s.MaxLayerBytes)
	if err != nil {
		return 0, err
	}
	// The tar's end padding counts toward the diff_id too.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return 0, err
	}
	if err := blob.Verify(); err != nil {
		return 0, err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != diffID {
		return 0, fmt.Errorf("unpacked layer has diff_id %s, the config says %s", short(got), short(diffID))
	}
	if err := os.Rename(tmp, final); err != nil {
		return 0, err
	}
	keep = true
	s.logf("  %s: %d files, %d dirs, %d links, %d whiteouts, %d bytes", short(diffID), st.Files, st.Dirs, st.Symlinks+st.Hardlinks, st.Whiteouts+st.Opaque, st.Bytes)
	return st.Bytes, nil
}

func (s *Store) writeBlob(digest string, data []byte) error {
	path := filepath.Join(s.Root, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	return writeAtomic(path, data)
}

// Blob reads a stored manifest or config.
func (s *Store) Blob(digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("digest %q is not sha256", digest)
	}
	return os.ReadFile(filepath.Join(s.Root, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
}

func (s *Store) readImages() (map[string]Image, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, "images.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Image{}, nil
	}
	if err != nil {
		return nil, err
	}
	images := map[string]Image{}
	if err := json.Unmarshal(data, &images); err != nil {
		return nil, fmt.Errorf("images.json: %w", err)
	}
	return images, nil
}

func (s *Store) writeImages(images map[string]Image) error {
	data, err := json.MarshalIndent(images, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Root, "images.json"), append(data, '\n'))
}

// Images lists the pulled images, by reference.
func (s *Store) Images() ([]Image, error) {
	images, err := s.readImages()
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(images))
	for _, img := range images {
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reference < out[j].Reference })
	return out, nil
}

// Lookup finds a pulled image by the name it was pulled as ("alpine"
// means "alpine:latest", as for a pull).
func (s *Store) Lookup(name string) (Image, Config, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return Image{}, Config{}, err
	}
	images, err := s.readImages()
	if err != nil {
		return Image{}, Config{}, err
	}
	img, ok := images[ref.Short()]
	if !ok {
		return Image{}, Config{}, fmt.Errorf("image %s is not here: minibox pull %s", ref.Short(), ref.Short())
	}
	data, err := s.Blob(img.Config)
	if err != nil {
		return Image{}, Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Image{}, Config{}, err
	}
	return img, cfg, nil
}

// Remove forgets an image and deletes the layers, manifests and configs
// no other image uses.
func (s *Store) Remove(name string) error {
	ref, err := ParseReference(name)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	images, err := s.readImages()
	if err != nil {
		return err
	}
	if _, ok := images[ref.Short()]; !ok {
		return fmt.Errorf("image %s is not here", ref.Short())
	}
	delete(images, ref.Short())
	if err := s.writeImages(images); err != nil {
		return err
	}
	used := map[string]bool{}
	for _, img := range images {
		used[img.Manifest], used[img.Config] = true, true
		for _, l := range img.Layers {
			used[l] = true
		}
	}
	for _, sub := range []string{"layers", "blobs/sha256"} {
		entries, err := os.ReadDir(filepath.Join(s.Root, sub))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !used["sha256:"+e.Name()] {
				if err := os.RemoveAll(filepath.Join(s.Root, sub, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Overlay is one container's filesystem: the image's layers, read-only,
// under a writable layer of its own.
type Overlay struct {
	Lower  []string // layer folders, top layer first, as overlayfs lists them
	Upper  string
	Work   string
	Merged string
	Dir    string // the container's folder, removed with it
}

// NewOverlay makes the folders for a container on img.
func (s *Store) NewOverlay(id string, img Image) (Overlay, error) {
	dir := filepath.Join(s.Root, "containers", id)
	o := Overlay{Upper: filepath.Join(dir, "upper"), Work: filepath.Join(dir, "work"), Merged: filepath.Join(dir, "merged"), Dir: dir}
	for _, l := range slices.Backward(img.Layers) {
		o.Lower = append(o.Lower, filepath.Join(s.Root, "layers", strings.TrimPrefix(l, "sha256:")))
	}
	for _, d := range []string{o.Upper, o.Work, o.Merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Overlay{}, err
		}
	}
	return o, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp-" + randomHex()
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
