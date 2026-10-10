package image

import (
	"encoding/json"
	"fmt"
	"runtime"
)

// Media types: OCI's, and Docker's older ones, which registries still
// serve for most images.
const (
	MediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	MediaOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	MediaOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaOCILayer        = "application/vnd.oci.image.layer.v1.tar"
	MediaOCILayerZstd    = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// Descriptor points at a blob or manifest by digest and size.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Platform is an index entry's operating system and architecture.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Index lists one manifest per platform (OCI index, Docker manifest list).
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// Manifest is one platform's image: its config and its layers, bottom
// layer first.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Config is the part of an image's configuration MiniBox uses.
type Config struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		WorkingDir string   `json:"WorkingDir"`
		User       string   `json:"User"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"` // sha256 of each layer uncompressed
	} `json:"rootfs"`
}

// manifestKind reads a manifest document's media type, from the response
// header or, failing that, the document itself.
func manifestKind(header string, body []byte) (string, error) {
	switch header {
	case MediaOCIIndex, MediaDockerList, MediaOCIManifest, MediaDockerManifest:
		return header, nil
	}
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Layers    []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("manifest is not JSON: %w", err)
	}
	switch {
	case probe.MediaType != "":
		return probe.MediaType, nil
	case probe.Manifests != nil:
		return MediaOCIIndex, nil
	case probe.Layers != nil:
		return MediaOCIManifest, nil
	}
	return "", fmt.Errorf("unknown manifest type %q", header)
}

// choosePlatform picks the manifest for os/arch (and variant, when the
// index gives one for this architecture) from an index.
func choosePlatform(idx Index, os, arch, variant string) (Descriptor, error) {
	var fallback *Descriptor
	for i, m := range idx.Manifests {
		p := m.Platform
		if p == nil || p.OS != os || p.Architecture != arch {
			continue
		}
		if variant == "" || p.Variant == variant {
			return m, nil
		}
		if p.Variant == "" && fallback == nil {
			fallback = &idx.Manifests[i]
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	var have []string
	for _, m := range idx.Manifests {
		if m.Platform != nil {
			have = append(have, m.Platform.OS+"/"+m.Platform.Architecture)
		}
	}
	return Descriptor{}, fmt.Errorf("no image for %s/%s (the index has %v)", os, arch, have)
}

// HostPlatform is the os, architecture and variant images are pulled for:
// this machine's (arm64 images are listed with variant v8).
func HostPlatform() (os, arch, variant string) {
	arch = runtime.GOARCH
	if arch == "arm64" {
		variant = "v8"
	}
	return "linux", arch, variant
}
