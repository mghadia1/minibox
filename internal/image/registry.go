package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Limits on what a registry may send: a manifest is a small JSON file, a
// blob is checked against the size its descriptor gives.
const (
	maxManifestSize = 4 << 20
	maxTokenSize    = 1 << 20
)

// Client talks to registries with the OCI distribution API, anonymously.
type Client struct {
	HTTP *http.Client
	// Scheme is "https"; tests use "http" for a local registry.
	Scheme string
	tokens map[string]string // by registry and repository
}

// NewClient returns a client with a timeout per request.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, Scheme: "https"}
}

var manifestAccept = strings.Join([]string{MediaOCIIndex, MediaDockerList, MediaOCIManifest, MediaDockerManifest}, ", ")

// get sends a GET, answering a 401 bearer challenge once with an
// anonymous token from the realm the registry names.
func (c *Client) get(ref Reference, path, accept string) (*http.Response, error) {
	u := c.Scheme + "://" + ref.Registry + "/v2/" + ref.Repository + path
	key := ref.Registry + "/" + ref.Repository
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if tok := c.tokens[key]; tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, err := c.token(challenge)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref.Short(), err)
			}
			if c.tokens == nil {
				c.tokens = map[string]string{}
			}
			c.tokens[key] = tok
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s: GET %s: %s", ref.Short(), path, resp.Status)
		}
		return resp, nil
	}
}

// token fetches an anonymous bearer token as a WWW-Authenticate challenge
// asks: Bearer realm="https://auth.docker.io/token",service="...",scope="...".
func (c *Client) token(challenge string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry wants %q authentication: only anonymous bearer tokens are supported", scheme)
	}
	fields := parseChallenge(params)
	realm := fields["realm"]
	ru, err := url.Parse(realm)
	if err != nil || (ru.Scheme != "https" && ru.Scheme != c.Scheme) || ru.Host == "" {
		return "", fmt.Errorf("token realm %q is not a usable URL", realm)
	}
	q := ru.Query()
	for _, k := range []string{"service", "scope"} {
		if v := fields[k]; v != "" {
			q.Set(k, v)
		}
	}
	ru.RawQuery = q.Encode()
	resp, err := c.HTTP.Get(ru.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenSize)).Decode(&body); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token: empty")
}

// parseChallenge reads key="value" pairs separated by commas; a value may
// hold commas inside its quotes (a scope with several actions).
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, " ,")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			value, s = rest[1:1+end], rest[2+end:]
		} else {
			value, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return out
}

// Manifest fetches a manifest or index by tag or digest, and checks its
// digest: against the reference's digest, or the one the registry states.
// It returns the document, its media type and its digest.
func (c *Client) Manifest(ref Reference, refOrDigest string) ([]byte, string, string, error) {
	resp, err := c.get(ref, "/manifests/"+refOrDigest, manifestAccept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(body) > maxManifestSize {
		return nil, "", "", fmt.Errorf("%s: manifest larger than %d bytes", ref.Short(), maxManifestSize)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	want := ""
	if strings.HasPrefix(refOrDigest, "sha256:") {
		want = refOrDigest
	} else if stated := resp.Header.Get("Docker-Content-Digest"); stated != "" {
		want = stated
	}
	if want != "" && want != digest {
		return nil, "", "", fmt.Errorf("%s: manifest digest is %s, expected %s", ref.Short(), digest, want)
	}
	kind, err := manifestKind(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", ref.Short(), err)
	}
	return body, kind, digest, nil
}

// OpenBlob starts reading a blob. The reader hashes what it gives out;
// once it has been read to the end, Verify says whether the blob had the
// descriptor's size and digest. Nothing read from it can be trusted
// before that.
func (c *Client) OpenBlob(ref Reference, d Descriptor) (*BlobReader, error) {
	if !digestPattern.MatchString(d.Digest) {
		return nil, fmt.Errorf("blob digest %q is not sha256", d.Digest)
	}
	resp, err := c.get(ref, "/blobs/"+d.Digest, "")
	if err != nil {
		return nil, err
	}
	return &BlobReader{body: resp.Body, limited: io.LimitReader(resp.Body, d.Size+1), h: sha256.New(), d: d}, nil
}

// BlobReader is a blob being read, and checked.
type BlobReader struct {
	body    io.ReadCloser
	limited io.Reader
	h       hash.Hash
	n       int64
	d       Descriptor
}

func (b *BlobReader) Read(p []byte) (int, error) {
	n, err := b.limited.Read(p)
	b.h.Write(p[:n])
	b.n += int64(n)
	return n, err
}

// Verify reads whatever is left and checks the size and digest.
func (b *BlobReader) Verify() error {
	if _, err := io.Copy(io.Discard, b); err != nil {
		return fmt.Errorf("blob %s: %w", short(b.d.Digest), err)
	}
	return checkBlob(b.d, b.n, b.h)
}

// Close closes the connection.
func (b *BlobReader) Close() error { return b.body.Close() }

// ReadBlob reads a small blob (a config) whole and checks it.
func (c *Client) ReadBlob(ref Reference, d Descriptor, limit int64) ([]byte, error) {
	if d.Size > limit {
		return nil, fmt.Errorf("blob %s: %d bytes, more than the %d allowed", short(d.Digest), d.Size, limit)
	}
	b, err := c.OpenBlob(ref, d)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	data, err := io.ReadAll(b)
	if err != nil {
		return nil, err
	}
	if err := b.Verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func checkBlob(d Descriptor, n int64, h hash.Hash) error {
	if n != d.Size {
		return fmt.Errorf("blob %s: %d bytes, expected %d", short(d.Digest), n, d.Size)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return fmt.Errorf("blob %s: content has digest %s", short(d.Digest), short(got))
	}
	return nil
}

// short is a digest's first 12 hex digits, for messages.
func short(digest string) string {
	hexPart := strings.TrimPrefix(digest, "sha256:")
	if len(hexPart) > 12 {
		hexPart = hexPart[:12]
	}
	return hexPart
}

// Resolve finds this machine's manifest for a reference: through the
// index when the registry serves one (every digest checked on the way).
// It returns the manifest, its bytes and its digest.
func (c *Client) Resolve(ref Reference) (Manifest, []byte, string, error) {
	body, kind, digest, err := c.Manifest(ref, ref.ref())
	if err != nil {
		return Manifest{}, nil, "", err
	}
	if kind == MediaOCIIndex || kind == MediaDockerList {
		var idx Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return Manifest{}, nil, "", fmt.Errorf("%s: index: %w", ref.Short(), err)
		}
		goos, arch, variant := HostPlatform()
		d, err := choosePlatform(idx, goos, arch, variant)
		if err != nil {
			return Manifest{}, nil, "", fmt.Errorf("%s: %w", ref.Short(), err)
		}
		if body, kind, digest, err = c.Manifest(ref, d.Digest); err != nil {
			return Manifest{}, nil, "", err
		}
	}
	if kind != MediaOCIManifest && kind != MediaDockerManifest {
		return Manifest{}, nil, "", fmt.Errorf("%s: unsupported manifest type %q", ref.Short(), kind)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, nil, "", fmt.Errorf("%s: manifest: %w", ref.Short(), err)
	}
	return m, body, digest, nil
}
