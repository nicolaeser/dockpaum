package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const maxConfigBytes = 8 << 20

type ImageConfig struct {
	Digest   string
	Manifest string
	Created  time.Time
	Platform Platform
	Labels   map[string]string
}

func (c *Client) ImageConfig(ctx context.Context, ref Reference, desc Descriptor) (ImageConfig, error) {
	if !digestRE.MatchString(desc.Digest) {
		return ImageConfig{}, fmt.Errorf("registry: invalid manifest digest %q", desc.Digest)
	}
	if desc.IsIndex() {
		return ImageConfig{}, fmt.Errorf("registry: %s/%s@%s is an index, not a platform's manifest", ref.Registry, ref.Repo, desc.Digest)
	}
	body, _, err := c.fetchManifest(ctx, ref, desc.Digest)
	if err != nil {
		return ImageConfig{}, err
	}
	var doc struct {
		Manifests []json.RawMessage `json:"manifests"`
		Config    Descriptor        `json:"config"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ImageConfig{}, fmt.Errorf("registry: decode manifest %s: %w", desc.Digest, err)
	}
	switch {
	case len(doc.Manifests) > 0:
		return ImageConfig{}, fmt.Errorf("registry: %s/%s@%s is an index, not a platform's manifest", ref.Registry, ref.Repo, desc.Digest)
	case doc.Config.Digest == "":
		return ImageConfig{}, fmt.Errorf("registry: manifest %s/%s@%s names no config (schema 1 manifests are not supported)", ref.Registry, ref.Repo, desc.Digest)
	case !digestRE.MatchString(doc.Config.Digest):
		return ImageConfig{}, fmt.Errorf("registry: manifest %s names invalid config digest %q", desc.Digest, doc.Config.Digest)
	}
	blob, err := c.fetchBlob(ctx, ref, doc.Config.Digest, maxConfigBytes)
	if err != nil {
		return ImageConfig{}, err
	}
	var cfg struct {
		Created      string `json:"created"`
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return ImageConfig{}, fmt.Errorf("registry: decode image config %s: %w", doc.Config.Digest, err)
	}
	out := ImageConfig{
		Digest:   doc.Config.Digest,
		Manifest: desc.Digest,
		Platform: Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant},
		Labels:   cfg.Config.Labels,
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(cfg.Created)); err == nil {
		out.Created = t
	}
	return out, nil
}

func (c *Client) fetchBlob(ctx context.Context, ref Reference, digest string, limit int64) ([]byte, error) {
	if !repoRE.MatchString(ref.Repo) {
		return nil, fmt.Errorf("registry: invalid repository %q", ref.Repo)
	}
	if !digestRE.MatchString(digest) {
		return nil, fmt.Errorf("registry: invalid blob digest %q", digest)
	}
	u := registryScheme(ref.Registry) + "://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + digest
	resp, err := c.doAuthed(ctx, ref.Registry, ref.Repo, apiRequest{endpoint: endpointBlob, method: http.MethodGet, url: u})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, limit, "blob")
	if err != nil {
		return nil, err
	}
	if got := digestOf(digest, body); got != digest {
		return nil, fmt.Errorf("registry: blob %s/%s@%s has content digest %s", ref.Registry, ref.Repo, digest, got)
	}
	return body, nil
}

func ParsePlatform(s string) (Platform, bool) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, false
	}
	p := Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		if parts[2] == "" {
			return Platform{}, false
		}
		p.Variant = parts[2]
	}
	return p, true
}

func RepoDigestFor(repoDigests []string, ref Reference) string {
	for _, rd := range repoDigests {
		name, d, ok := strings.Cut(rd, "@")
		if !ok || !digestRE.MatchString(d) {
			continue
		}
		p := Parse(name)
		if canonicalHost(p.Registry) == canonicalHost(ref.Registry) && p.Repo == ref.Repo {
			return rd
		}
	}
	return ""
}

func DigestReference(imageRef, digest string) string {
	name := imageRef
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return name + "@" + digest
}

func VersionOf(labels map[string]string, tag string) string {
	v, _, ok := releaseVersion(strings.TrimSpace(tag), strings.TrimSpace(labels[labelOCIVersion]))
	if !ok {
		return ""
	}
	return v
}

func ReleaseLabels(labels map[string]string, extra ...string) map[string]string {
	var out map[string]string
	keep := func(k string) {
		if v, ok := labels[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	for _, k := range []string{labelOCISource, labelOCIURL, labelOCIVersion} {
		keep(k)
	}
	for _, k := range extra {
		keep(k)
	}
	return out
}
