package image

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mghadia1/minibox/internal/testutil"
)

func publishOne(t *testing.T) (*testutil.Registry, Reference, testutil.Published) {
	r := testutil.NewRegistry(t, "library/test")
	pub := r.Publish(t, "1", testutil.ImageSpec{Layers: [][]byte{makeTar(t, file("a", "x", 0o644))}, Cmd: []string{"/bin/sh"}})
	return r, Reference{Registry: r.Host(), Repository: "library/test", Tag: "1"}, pub
}

func TestClientAnswersTheChallengeOnce(t *testing.T) {
	r, ref, pub := publishOne(t)
	c := testClient()
	body, kind, digest, err := c.Manifest(ref, "1")
	if err != nil {
		t.Fatal(err)
	}
	if kind != MediaOCIIndex || digest != pub.Index || digest != testutil.Digest(body) {
		t.Fatalf("kind %q digest %s", kind, digest)
	}
	if _, _, _, err := c.Manifest(ref, "1"); err != nil {
		t.Fatal(err)
	}
	if n := r.Tokens.Load(); n != 1 {
		t.Fatalf("%d tokens fetched for two requests, want 1", n)
	}
}

func TestResolveChoosesThisPlatform(t *testing.T) {
	_, ref, pub := publishOne(t)
	m, _, digest, err := testClient().Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	if digest != pub.Manifest || m.Config.Digest != pub.Config || len(m.Layers) != 1 || m.Layers[0].Digest != pub.Layers[0] {
		t.Fatalf("resolved %s %+v, want manifest %s", digest, m, pub.Manifest)
	}
}

func TestManifestDigestIsChecked(t *testing.T) {
	r, ref, pub := publishOne(t)
	// The registry serves different bytes under a manifest's digest.
	r.Manifests[pub.Manifest] = append(r.Manifests[pub.Manifest], ' ')
	if _, _, _, err := testClient().Resolve(ref); err == nil || !strings.Contains(err.Error(), "manifest digest") {
		t.Fatalf("a manifest that does not match its digest: %v", err)
	}
}

func TestBlobIsChecked(t *testing.T) {
	r, ref, pub := publishOne(t)
	c := testClient()
	good := r.Blobs[pub.Config]
	d := Descriptor{Digest: pub.Config, Size: int64(len(good))}
	if _, err := c.ReadBlob(ref, d, 1<<20); err != nil {
		t.Fatalf("good blob: %v", err)
	}
	for name, served := range map[string][]byte{
		"changed":   bytes.Replace(good, []byte("linux"), []byte("LINUX"), 1),
		"truncated": good[:len(good)-1],
		"longer":    append(append([]byte{}, good...), '\n'),
	} {
		r.Blobs[pub.Config] = served
		if _, err := c.ReadBlob(ref, d, 1<<20); err == nil {
			t.Errorf("%s blob accepted", name)
		}
	}
	r.Blobs[pub.Config] = good
	if _, err := c.ReadBlob(ref, d, 10); err == nil {
		t.Error("blob over the limit accepted")
	}
}
