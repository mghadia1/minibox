//go:build linux

package image

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mghadia1/minibox/internal/testutil"
)

func newStore(t *testing.T) (*Store, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	s, err := Open(filepath.Join(t.TempDir(), "store"), &log)
	if err != nil {
		t.Fatal(err)
	}
	return s, &log
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestPull(t *testing.T) {
	testutil.RequireRoot(t)
	r := testutil.NewRegistry(t, "library/test")
	base := makeTar(t, file("etc/os-release", "ID=test\n", 0o644), file("etc/motd", "hi\n", 0o644))
	top := makeTar(t, file("etc/.wh.motd", "", 0o644), file("app", "v1", 0o755))
	pub := r.Publish(t, "1", testutil.ImageSpec{Layers: [][]byte{base, top}, Cmd: []string{"/app"}, Env: []string{"A=1"}})
	s, log := newStore(t)

	img, err := s.Pull(testClient(), r.Name("1"))
	if err != nil {
		t.Fatal(err)
	}
	if img.Manifest != pub.Manifest || img.Config != pub.Config || !slices.Equal(img.Layers, pub.DiffIDs) {
		t.Fatalf("pulled %+v, published %+v", img, pub)
	}
	for _, d := range pub.DiffIDs {
		if _, err := os.Stat(filepath.Join(s.Root, "layers", strings.TrimPrefix(d, "sha256:"))); err != nil {
			t.Errorf("layer %s not unpacked: %v", d, err)
		}
	}
	got, cfg, err := s.Lookup(r.Name("1"))
	if err != nil || got.Manifest != pub.Manifest || !slices.Equal(cfg.Config.Cmd, []string{"/app"}) || !slices.Equal(cfg.Config.Env, []string{"A=1"}) {
		t.Fatalf("Lookup = %+v, %+v, %v", got, cfg.Config, err)
	}
	list, err := s.Images()
	if err != nil || len(list) != 1 || list[0].Reference != r.Host()+"/library/test:1" {
		t.Fatalf("Images = %+v, %v", list, err)
	}

	// The same layers again are not fetched again.
	pub2 := r.Publish(t, "2", testutil.ImageSpec{Layers: [][]byte{base, top}, Cmd: []string{"/other"}})
	log.Reset()
	if _, err := s.Pull(testClient(), r.Name("2")); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(log.String(), "already here"); n != 2 {
		t.Fatalf("second image with the same layers: %d layers reused, want 2\n%s", n, log)
	}
	_ = pub2
}

// A pull that fails part-way leaves nothing behind: no layer, no
// temporary folder, no image.
func TestPullFailures(t *testing.T) {
	testutil.RequireRoot(t)
	for _, tc := range []struct {
		name   string
		spoil  func(r *testutil.Registry, pub testutil.Published)
		spec   testutil.ImageSpec
		reason string
	}{
		{"a layer's bytes changed", func(r *testutil.Registry, pub testutil.Published) {
			b := append([]byte{}, r.Blobs[pub.Layers[1]]...)
			b[len(b)-10] ^= 0xff
			r.Blobs[pub.Layers[1]] = b
		}, testutil.ImageSpec{}, ""},
		{"a layer cut short", func(r *testutil.Registry, pub testutil.Published) {
			r.Blobs[pub.Layers[1]] = r.Blobs[pub.Layers[1]][:20]
		}, testutil.ImageSpec{}, ""},
		{"a layer that is not what the config says", nil, testutil.ImageSpec{
			// The layer is served and described consistently, but the
			// config lists another uncompressed digest for it.
			Layers:  [][]byte{makeTar(t, file("a", "1", 0o644)), makeTar(t, file("b", "2", 0o644))},
			DiffIDs: []string{testutil.Digest(makeTar(t, file("a", "1", 0o644))), testutil.Digest([]byte("something else"))},
		}, "diff_id"},
		{"an image for another architecture", nil, testutil.ImageSpec{Arch: "s390x"}, "is for linux/s390x"},
		// The config's bytes match its digest, but the manifest says it is
		// longer than it is.
		{"a manifest that misstates a size", nil, testutil.ImageSpec{ConfigSizeLie: 100}, "expected"},
		{"a layer that escapes", nil, testutil.ImageSpec{Layers: [][]byte{makeTar(t, file("../escape", "x", 0o644))}}, "climbs out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testutil.NewRegistry(t, "library/test")
			spec := tc.spec
			if spec.Layers == nil {
				spec.Layers = [][]byte{makeTar(t, file("a", "1", 0o644)), makeTar(t, file("b", strings.Repeat("2", 4096), 0o644))}
			}
			pub := r.Publish(t, "1", spec)
			if tc.spoil != nil {
				tc.spoil(r, pub)
			}
			s, _ := newStore(t)
			_, err := s.Pull(testClient(), r.Name("1"))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("pull: %v (want an error with %q)", err, tc.reason)
			}
			for _, name := range entries(t, filepath.Join(s.Root, "layers")) {
				if strings.HasPrefix(name, ".tmp-") || (len(pub.DiffIDs) > 1 && name == strings.TrimPrefix(pub.DiffIDs[1], "sha256:")) {
					t.Errorf("left behind: layers/%s", name)
				}
			}
			if list, _ := s.Images(); len(list) != 0 {
				t.Errorf("image recorded after a failed pull: %+v", list)
			}
		})
	}
}

func TestRemoveKeepsSharedLayers(t *testing.T) {
	testutil.RequireRoot(t)
	r := testutil.NewRegistry(t, "library/test")
	base := makeTar(t, file("base", "b", 0o644))
	one := r.Publish(t, "one", testutil.ImageSpec{Layers: [][]byte{base, makeTar(t, file("one", "1", 0o644))}})
	two := r.Publish(t, "two", testutil.ImageSpec{Layers: [][]byte{base, makeTar(t, file("two", "2", 0o644))}})
	s, _ := newStore(t)
	for _, tag := range []string{"one", "two"} {
		if _, err := s.Pull(testClient(), r.Name(tag)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Remove(r.Name("one")); err != nil {
		t.Fatal(err)
	}
	layers := entries(t, filepath.Join(s.Root, "layers"))
	want := []string{strings.TrimPrefix(two.DiffIDs[0], "sha256:"), strings.TrimPrefix(two.DiffIDs[1], "sha256:")}
	slices.Sort(want)
	if !slices.Equal(layers, want) {
		t.Fatalf("layers left %q, want %q (one's own layer gone, the shared base kept)", layers, want)
	}
	blobs := entries(t, filepath.Join(s.Root, "blobs", "sha256"))
	if slices.Contains(blobs, strings.TrimPrefix(one.Manifest, "sha256:")) || !slices.Contains(blobs, strings.TrimPrefix(two.Config, "sha256:")) {
		t.Fatalf("blobs left %q", blobs)
	}
	if _, _, err := s.Lookup(r.Name("one")); err == nil {
		t.Fatal("removed image still found")
	}
	if err := s.Remove(r.Name("one")); err == nil {
		t.Fatal("removing it twice did not fail")
	}
}

func TestNewOverlayListsTheTopLayerFirst(t *testing.T) {
	s, _ := newStore(t)
	img := Image{Layers: []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}}
	o, err := s.NewOverlay("c1", img)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(o.Lower[0]) != strings.Repeat("b", 64) || filepath.Base(o.Lower[1]) != strings.Repeat("a", 64) {
		t.Fatalf("lower = %q: overlayfs takes the top layer first", o.Lower)
	}
	for _, d := range []string{o.Upper, o.Work, o.Merged} {
		if _, err := os.Stat(d); err != nil {
			t.Fatal(err)
		}
	}
}
