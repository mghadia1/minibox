package image

import (
	"fmt"
	"regexp"
	"strings"
)

// Reference names an image: registry, repository, and a tag or digest.
// "alpine:3.22" is registry-1.docker.io, library/alpine, tag 3.22, as
// Docker reads it.
type Reference struct {
	Registry   string // host[:port] of the registry's API
	Repository string // path, "library/alpine"
	Tag        string // "3.22"; empty when Digest is set
	Digest     string // "sha256:...", or empty
}

const (
	dockerHub      = "registry-1.docker.io"
	defaultTag     = "latest"
	officialPrefix = "library/"
)

var (
	// A path component, as the distribution spec allows.
	componentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ParseReference reads "alpine", "alpine:3.22", "ghcr.io/owner/app:v1" or
// "name@sha256:...". The first component is a registry when it has a dot
// or a port, or is "localhost", as Docker decides.
func ParseReference(s string) (Reference, error) {
	var ref Reference
	name := s
	if at := strings.LastIndex(name, "@"); at >= 0 {
		ref.Digest = name[at+1:]
		name = name[:at]
		if !digestPattern.MatchString(ref.Digest) {
			return Reference{}, fmt.Errorf("image %q: digest %q is not sha256:<64 hex digits>", s, ref.Digest)
		}
	}
	// A tag follows the last ":" after the last "/" (a ":" before it is a port).
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		ref.Tag = name[colon+1:]
		name = name[:colon]
		if !tagPattern.MatchString(ref.Tag) {
			return Reference{}, fmt.Errorf("image %q: tag %q is not valid", s, ref.Tag)
		}
	}
	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Registry, name = first, rest
	} else {
		ref.Registry = dockerHub
		if !hasSlash {
			name = officialPrefix + name
		}
	}
	if name == "" {
		return Reference{}, fmt.Errorf("image %q has no repository", s)
	}
	for _, c := range strings.Split(name, "/") {
		if !componentPattern.MatchString(c) {
			return Reference{}, fmt.Errorf("image %q: %q is not a valid repository name component", s, c)
		}
	}
	ref.Repository = name
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = defaultTag
	}
	return ref, nil
}

// String is the reference written out in full.
func (r Reference) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Short is the reference as people write it: "alpine:3.22".
func (r Reference) Short() string {
	s := r.Repository
	if r.Registry == dockerHub {
		s = strings.TrimPrefix(s, officialPrefix)
	} else {
		s = r.Registry + "/" + s
	}
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// ref is what to ask the registry for: the digest when there is one.
func (r Reference) ref() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}
