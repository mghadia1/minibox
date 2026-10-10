package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Entry is one entry of a layer built for a test.
type Entry struct {
	Header tar.Header
	Body   string
}

var testTime = time.Unix(1700000000, 0)

// File, Folder, Symlink and Hardlink make layer entries.
func File(name, body string, mode int64) Entry {
	return Entry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(body)), ModTime: testTime}, body}
}

func Folder(name string) Entry {
	return Entry{tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755, ModTime: testTime}, ""}
}

func Symlink(name, target string) Entry {
	return Entry{tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, ModTime: testTime}, ""}
}

func Hardlink(name, target string) Entry {
	return Entry{tar.Header{Typeflag: tar.TypeLink, Name: name, Linkname: target, ModTime: testTime}, ""}
}

// Tar writes entries as an uncompressed tar: a layer.
func Tar(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.Header
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.Body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Gzip compresses a layer.
func Gzip(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Digest is data's sha256 digest, as OCI writes it.
func Digest(data []byte) string {
	s := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(s[:])
}

// RootfsLayer is the test root filesystem as a layer: Alpine's
// minirootfs, uncompressed.
func RootfsLayer(t testing.TB) []byte {
	t.Helper()
	archive := os.Getenv("MINIBOX_TEST_ROOTFS")
	if archive == "" {
		Require(t, "MINIBOX_TEST_ROOTFS is not set (scripts/rootfs.sh)")
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Registry is a fake OCI registry for one repository, served the way
// Docker Hub serves: a 401 with a bearer challenge until a token is shown,
// manifests by tag or digest (with Docker-Content-Digest), blobs by
// digest. Tests may change what it serves.
type Registry struct {
	Server    *httptest.Server
	Repo      string
	Manifests map[string][]byte // by tag or digest
	Types     map[string]string
	Blobs     map[string][]byte
	Tokens    atomic.Int32
}

// NewRegistry starts a registry for repo ("library/test").
func NewRegistry(t testing.TB, repo string) *Registry {
	r := &Registry{Repo: repo, Manifests: map[string][]byte{}, Types: map[string]string{}, Blobs: map[string][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)
	return r
}

// Host is the registry's host:port.
func (r *Registry) Host() string {
	u, _ := url.Parse(r.Server.URL)
	return u.Host
}

// Name is how an image tag of this registry is written: host/repo:tag.
func (r *Registry) Name(tag string) string { return r.Host() + "/" + r.Repo + ":" + tag }

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		if req.URL.Query().Get("scope") != "repository:"+r.Repo+":pull" || req.URL.Query().Get("service") != "fake" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		r.Tokens.Add(1)
		fmt.Fprint(w, `{"token":"t0k3n"}`)
		return
	}
	if req.Header.Get("Authorization") != "Bearer t0k3n" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:%s:pull"`, r.Server.URL, r.Repo))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/"+r.Repo+"/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	switch {
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		body, ok := r.Manifests[ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", r.Types[ref])
		w.Header().Set("Docker-Content-Digest", Digest(body))
		_, _ = w.Write(body)
	case strings.HasPrefix(rest, "blobs/"):
		body, ok := r.Blobs[strings.TrimPrefix(rest, "blobs/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, req)
	}
}

// ImageSpec is what to publish: layers bottom first (uncompressed tars,
// gzipped on the way), and the image's configuration.
type ImageSpec struct {
	Layers     [][]byte
	Env        []string
	Entrypoint []string
	Cmd        []string
	WorkingDir string
	User       string
	// Arch overrides the architecture written in the config (to test a
	// mismatch); empty is this machine's.
	Arch string
	// DiffIDs, when set, is written in the config instead of the layers'
	// real uncompressed digests (to test that they are checked).
	DiffIDs []string
	// ConfigSizeLie is added to the config's size in the manifest: a
	// manifest that misstates a size (to test that sizes are checked).
	ConfigSizeLie int
}

// Published is the digests of what Publish made.
type Published struct {
	Index, Manifest, Config string
	Layers                  []string // compressed layer digests
	DiffIDs                 []string
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

func desc(mediaType string, data []byte) descriptor {
	return descriptor{MediaType: mediaType, Digest: Digest(data), Size: len(data)}
}

// Publish puts an image under tag: an OCI index listing this machine's
// manifest and one for s390x (which must never be chosen here).
func (r *Registry) Publish(t testing.TB, tag string, spec ImageSpec) Published {
	t.Helper()
	var pub Published
	arch := runtime.GOARCH
	variant := ""
	if arch == "arm64" {
		variant = "v8"
	}
	cfgArch := arch
	if spec.Arch != "" {
		cfgArch = spec.Arch
	}
	config := map[string]any{
		"architecture": cfgArch,
		"os":           "linux",
		"config": map[string]any{
			"Env": spec.Env, "Entrypoint": spec.Entrypoint, "Cmd": spec.Cmd,
			"WorkingDir": spec.WorkingDir, "User": spec.User,
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{}},
	}
	var layers []descriptor
	for _, l := range spec.Layers {
		gz := Gzip(t, l)
		r.Blobs[Digest(gz)] = gz
		layers = append(layers, desc("application/vnd.oci.image.layer.v1.tar+gzip", gz))
		pub.Layers = append(pub.Layers, Digest(gz))
		pub.DiffIDs = append(pub.DiffIDs, Digest(l))
	}
	config["rootfs"].(map[string]any)["diff_ids"] = pub.DiffIDs
	if spec.DiffIDs != nil {
		config["rootfs"].(map[string]any)["diff_ids"] = spec.DiffIDs
	}
	cfg, _ := json.Marshal(config)
	r.Blobs[Digest(cfg)] = cfg
	pub.Config = Digest(cfg)
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": func() descriptor {
			d := desc("application/vnd.oci.image.config.v1+json", cfg)
			d.Size += spec.ConfigSizeLie
			return d
		}(),
		"layers": layers,
	})
	pub.Manifest = Digest(manifest)
	r.Manifests[pub.Manifest], r.Types[pub.Manifest] = manifest, "application/vnd.oci.image.manifest.v1+json"
	other, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "layers": []any{}})
	r.Manifests[Digest(other)], r.Types[Digest(other)] = other, "application/vnd.oci.image.manifest.v1+json"

	mine := desc("application/vnd.oci.image.manifest.v1+json", manifest)
	mine.Platform = &struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	}{arch, "linux", variant}
	s390 := desc("application/vnd.oci.image.manifest.v1+json", other)
	s390.Platform = &struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	}{"s390x", "linux", ""}
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []descriptor{s390, mine},
	})
	pub.Index = Digest(index)
	r.Manifests[tag], r.Types[tag] = index, "application/vnd.oci.image.index.v1+json"
	return pub
}
