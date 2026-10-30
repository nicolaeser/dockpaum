package registry

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

const (
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
)

var manifestAccept = strings.Join([]string{
	MediaTypeOCIIndex,
	MediaTypeDockerManifestList,
	MediaTypeDockerManifest,
	MediaTypeOCIManifest,
}, ", ")

const maxManifestBytes = 4 << 20

var (
	ErrNoDigest         = errors.New("registry: manifest response has no Docker-Content-Digest")
	ErrPlatformNotFound = errors.New("registry: no manifest for platform")
)

type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

func (d Descriptor) IsIndex() bool { return isIndexType(d.MediaType) }

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func isIndexType(mt string) bool {
	return mt == MediaTypeOCIIndex || mt == MediaTypeDockerManifestList
}

func isSingleManifestType(mt string) bool {
	switch mt {
	case MediaTypeOCIManifest, MediaTypeDockerManifest,
		"application/vnd.docker.distribution.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v1+prettyjws":
		return true
	}
	return false
}

var (
	digestRE = regexp.MustCompile(`^(sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	repoRE   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
)

func manifestURL(ref Reference, reference string) (string, error) {
	if !repoRE.MatchString(ref.Repo) {
		return "", fmt.Errorf("registry: invalid repository %q", ref.Repo)
	}
	if !digestRE.MatchString(reference) && !tagRE.MatchString(reference) {
		return "", fmt.Errorf("registry: invalid tag or digest %q", reference)
	}
	return registryScheme(ref.Registry) + "://" + ref.Registry + "/v2/" + ref.Repo + "/manifests/" + reference, nil
}

func (c *Client) ManifestDigest(ctx context.Context, ref Reference) (Descriptor, error) {
	reference := ref.Digest
	if reference == "" {
		reference = ref.Tag
	}
	if reference == "" {
		reference = "latest"
	}
	u, err := manifestURL(ref, reference)
	if err != nil {
		return Descriptor{}, err
	}
	resp, err := c.doAuthed(ctx, ref.Registry, ref.Repo, apiRequest{
		endpoint: endpointManifest, method: http.MethodHead, url: u, accept: manifestAccept,
	})
	if err != nil {
		return Descriptor{}, err
	}
	_ = resp.Body.Close()
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return Descriptor{}, fmt.Errorf("%w (%s/%s:%s)", ErrNoDigest, ref.Registry, ref.Repo, reference)
	}
	if !digestRE.MatchString(digest) {
		return Descriptor{}, fmt.Errorf("registry: %s/%s:%s: invalid Docker-Content-Digest %q", ref.Registry, ref.Repo, reference, digest)
	}
	if ref.Digest != "" && digest != ref.Digest {
		return Descriptor{}, fmt.Errorf("registry: %s/%s@%s answered with digest %s", ref.Registry, ref.Repo, ref.Digest, digest)
	}
	d := Descriptor{MediaType: mediaTypeOf(resp.Header.Get("Content-Type")), Digest: digest}
	if resp.ContentLength > 0 {
		d.Size = resp.ContentLength
	}
	return d, nil
}

func mediaTypeOf(contentType string) string {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		return mt
	}
	return strings.TrimSpace(contentType)
}

type manifestDoc struct {
	MediaType string          `json:"mediaType"`
	Manifests []Descriptor    `json:"manifests"`
	Config    json.RawMessage `json:"config"`
}

func (c *Client) PlatformManifest(ctx context.Context, ref Reference, desc Descriptor, platform Platform) (Descriptor, error) {
	if !digestRE.MatchString(desc.Digest) {
		return Descriptor{}, fmt.Errorf("registry: invalid manifest digest %q", desc.Digest)
	}
	if isSingleManifestType(desc.MediaType) {
		return desc, nil
	}
	body, mediaType, err := c.fetchManifest(ctx, ref, desc.Digest)
	if err != nil {
		return Descriptor{}, err
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return Descriptor{}, fmt.Errorf("registry: decode manifest %s: %w", desc.Digest, err)
	}
	if !isIndexType(mediaType) && !isSingleManifestType(mediaType) {
		mediaType = doc.MediaType
	}
	index := isIndexType(mediaType) || (mediaType == "" && len(doc.Manifests) > 0 && len(doc.Config) == 0)
	if !index {
		return Descriptor{MediaType: mediaType, Digest: desc.Digest, Size: int64(len(body))}, nil
	}
	m, ok := matchPlatform(doc.Manifests, platform)
	if !ok {
		return Descriptor{}, fmt.Errorf("%w %s in %s/%s@%s", ErrPlatformNotFound, platform, ref.Registry, ref.Repo, desc.Digest)
	}
	if !digestRE.MatchString(m.Digest) {
		return Descriptor{}, fmt.Errorf("registry: index %s lists invalid digest %q", desc.Digest, m.Digest)
	}
	return m, nil
}

func (c *Client) fetchManifest(ctx context.Context, ref Reference, digest string) (body []byte, mediaType string, err error) {
	u, err := manifestURL(ref, digest)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.doAuthed(ctx, ref.Registry, ref.Repo, apiRequest{
		endpoint: endpointManifest, method: http.MethodGet, url: u, accept: manifestAccept,
	})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err = readLimited(resp.Body, maxManifestBytes, "manifest")
	if err != nil {
		return nil, "", err
	}
	if got := digestOf(digest, body); got != digest {
		return nil, "", fmt.Errorf("registry: manifest %s/%s@%s has content digest %s", ref.Registry, ref.Repo, digest, got)
	}
	return body, mediaTypeOf(resp.Header.Get("Content-Type")), nil
}

func digestOf(want string, b []byte) string {
	if strings.HasPrefix(want, "sha512:") {
		sum := sha512.Sum512(b)
		return "sha512:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizePlatform(p Platform) Platform {
	osName := strings.ToLower(strings.TrimSpace(p.OS))
	arch := strings.ToLower(strings.TrimSpace(p.Architecture))
	variant := strings.ToLower(strings.TrimSpace(p.Variant))
	switch arch {
	case "x86_64", "x86-64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	case "i386", "i686":
		arch = "386"
	}
	if variant != "" && isDigitByte(variant[0]) {
		variant = "v" + variant
	}
	switch arch {
	case "arm64":
		if variant == "v8" {
			variant = ""
		}
	case "arm":
		if variant == "" {
			variant = "v7"
		}
	case "amd64":
		if variant == "v1" {
			variant = ""
		}
	}
	return Platform{OS: osName, Architecture: arch, Variant: variant}
}

func matchPlatform(entries []Descriptor, want Platform) (Descriptor, bool) {
	w := normalizePlatform(want)
	var sameArch []Descriptor
	for _, e := range entries {
		if e.Platform == nil {
			continue
		}
		p := normalizePlatform(*e.Platform)
		if p.OS != w.OS || p.Architecture != w.Architecture {
			continue
		}
		if p.Variant == w.Variant {
			return e, true
		}
		sameArch = append(sameArch, e)
	}
	if len(sameArch) == 1 && normalizePlatform(*sameArch[0].Platform).Variant == "" {
		return sameArch[0], true
	}
	return Descriptor{}, false
}

func HasRepoDigest(repoDigests []string, ref Reference, digest string) bool {
	if digest == "" {
		return false
	}
	for _, rd := range repoDigests {
		name, d, ok := strings.Cut(rd, "@")
		if !ok || d != digest {
			continue
		}
		p := Parse(name)
		if canonicalHost(p.Registry) == canonicalHost(ref.Registry) && p.Repo == ref.Repo {
			return true
		}
	}
	return false
}
