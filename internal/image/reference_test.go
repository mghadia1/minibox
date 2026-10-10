package image

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		in    string
		want  Reference
		short string
	}{
		{"alpine", Reference{dockerHub, "library/alpine", "latest", ""}, "alpine:latest"},
		{"alpine:3.22", Reference{dockerHub, "library/alpine", "3.22", ""}, "alpine:3.22"},
		{"bitnami/redis:8", Reference{dockerHub, "bitnami/redis", "8", ""}, "bitnami/redis:8"},
		{"ghcr.io/owner/app:v1.2", Reference{"ghcr.io", "owner/app", "v1.2", ""}, "ghcr.io/owner/app:v1.2"},
		{"localhost:5000/x", Reference{"localhost:5000", "x", "latest", ""}, "localhost:5000/x:latest"},
		{"localhost/x:1", Reference{"localhost", "x", "1", ""}, "localhost/x:1"},
		{"alpine@" + digest, Reference{dockerHub, "library/alpine", "", digest}, "alpine@" + digest},
		{"alpine:3.22@" + digest, Reference{dockerHub, "library/alpine", "3.22", digest}, "alpine:3.22@" + digest},
		{"registry.example.com:443/a/b/c", Reference{"registry.example.com:443", "a/b/c", "latest", ""}, "registry.example.com:443/a/b/c:latest"},
	} {
		got, err := ParseReference(tc.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want || got.Short() != tc.short {
			t.Errorf("ParseReference(%q) = %+v (%s), want %+v (%s)", tc.in, got, got.Short(), tc.want, tc.short)
		}
	}
	for _, bad := range []string{"", "Alpine", "alpine:", "alpine:-x", "alpine@sha256:abc", "a//b", "/alpine", "alpine/", "ghcr.io/", "al..pine", "alpine:" + strings.Repeat("t", 129)} {
		if r, err := ParseReference(bad); err == nil {
			t.Errorf("ParseReference(%q) accepted: %+v", bad, r)
		}
	}
}

func TestChoosePlatform(t *testing.T) {
	idx := Index{Manifests: []Descriptor{
		{Digest: "amd", Platform: &Platform{"amd64", "linux", ""}},
		{Digest: "armv7", Platform: &Platform{"arm", "linux", "v7"}},
		{Digest: "arm64v8", Platform: &Platform{"arm64", "linux", "v8"}},
		{Digest: "attestation", Platform: &Platform{"unknown", "unknown", ""}},
		{Digest: "win", Platform: &Platform{"amd64", "windows", ""}},
	}}
	for _, tc := range []struct{ os, arch, variant, want string }{
		{"linux", "amd64", "", "amd"},
		{"linux", "arm64", "v8", "arm64v8"},
		{"linux", "arm", "v7", "armv7"},
		{"windows", "amd64", "", "win"},
	} {
		d, err := choosePlatform(idx, tc.os, tc.arch, tc.variant)
		if err != nil || d.Digest != tc.want {
			t.Errorf("choosePlatform(%s/%s/%s) = %q, %v; want %q", tc.os, tc.arch, tc.variant, d.Digest, err, tc.want)
		}
	}
	// An arm64 entry with no variant is taken when no v8 one is listed.
	plain := Index{Manifests: []Descriptor{{Digest: "arm64", Platform: &Platform{"arm64", "linux", ""}}}}
	if d, err := choosePlatform(plain, "linux", "arm64", "v8"); err != nil || d.Digest != "arm64" {
		t.Errorf("fallback to no variant: %q, %v", d.Digest, err)
	}
	if _, err := choosePlatform(idx, "linux", "riscv64", ""); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("missing platform: %v", err)
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull,push"`)
	want := map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io", "scope": "repository:library/alpine:pull,push"}
	if len(got) != len(want) {
		t.Fatalf("parseChallenge = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if got := parseChallenge(`realm=x, Service="y"`); got["realm"] != "x" || got["service"] != "y" {
		t.Errorf("unquoted and capitalised: %v", got)
	}
}

func TestManifestKind(t *testing.T) {
	for _, tc := range []struct{ header, body, want string }{
		{MediaOCIIndex, `{}`, MediaOCIIndex},
		{"application/json", `{"mediaType":"` + MediaDockerManifest + `"}`, MediaDockerManifest},
		{"", `{"manifests":[]}`, MediaOCIIndex},
		{"", `{"layers":[]}`, MediaOCIManifest},
	} {
		got, err := manifestKind(tc.header, []byte(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("manifestKind(%q, %s) = %q, %v; want %q", tc.header, tc.body, got, err, tc.want)
		}
	}
	if _, err := manifestKind("", []byte("not json")); err == nil {
		t.Error("accepted a document that is not JSON")
	}
}
