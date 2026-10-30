package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/registry/registrytest"
)

func TestImageConfigResolvesWithoutPulling(t *testing.T) {
	reg := registrytest.New(t)
	built := time.Date(2026, time.May, 4, 3, 2, 1, 0, time.UTC)
	labels := map[string]string{"org.opencontainers.image.version": "1.27.3"}
	pub := reg.Push("library/app", "1.27",
		registrytest.Image{Architecture: "amd64", Created: built, Labels: labels, Build: "x"},
		registrytest.Image{Architecture: "arm64", Created: built, Build: "y"},
	)
	c := New(nil)
	m := &recordingMetrics{}
	c.AttachMetrics(m)
	ref := Reference{Registry: reg.Host(), Repo: "library/app", Tag: "1.27"}
	ctx := context.Background()

	d, err := c.ManifestDigest(ctx, ref)
	if err != nil || d.Digest != pub.Digest || !d.IsIndex() {
		t.Fatalf("ManifestDigest = %+v, %v; want the index %s", d, err, pub.Digest)
	}
	m1, err := c.PlatformManifest(ctx, ref, d, Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || m1.Digest != pub.Manifests[0] {
		t.Fatalf("PlatformManifest = %+v, %v", m1, err)
	}
	cfg, err := c.ImageConfig(ctx, ref, m1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest != pub.Configs[0] || cfg.Manifest != pub.Manifests[0] || !cfg.Created.Equal(built) ||
		cfg.Platform != (Platform{OS: "linux", Architecture: "amd64"}) || cfg.Labels["org.opencontainers.image.version"] != "1.27.3" {
		t.Fatalf("ImageConfig = %+v", cfg)
	}
	if n := len(reg.Calls(http.MethodGet, "/blobs/")); n != 1 {
		t.Fatalf("%d blob requests, want 1", n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.manifest, ",") != "ok,ok,ok,ok" || len(m.list) != 0 {
		t.Fatalf("manifest metrics %v, list metrics %v; the blob counts with the manifest requests", m.manifest, m.list)
	}
}

func TestImageConfigOfASingleManifest(t *testing.T) {
	reg := registrytest.New(t)
	pub := reg.Push("app", "1", registrytest.Image{Build: "a"})
	c := New(nil)
	ref := Reference{Registry: reg.Host(), Repo: "app", Tag: "1"}
	d, err := c.ManifestDigest(context.Background(), ref)
	if err != nil || d.IsIndex() {
		t.Fatalf("ManifestDigest = %+v, %v", d, err)
	}
	cfg, err := c.ImageConfig(context.Background(), ref, d)
	if err != nil || cfg.Digest != pub.Configs[0] || !cfg.Created.IsZero() {
		t.Fatalf("ImageConfig = %+v, %v; a config without created gives the zero time", cfg, err)
	}
}

func TestImageConfigRefusals(t *testing.T) {
	reg := registrytest.New(t)
	pub := reg.Push("app", "multi", registrytest.Image{Architecture: "amd64"}, registrytest.Image{Architecture: "arm64"})
	c := New(nil)
	ref := Reference{Registry: reg.Host(), Repo: "app"}
	ctx := context.Background()
	if _, err := c.ImageConfig(ctx, ref, Descriptor{MediaType: MediaTypeOCIIndex, Digest: pub.Digest}); err == nil || !strings.Contains(err.Error(), "is an index") {
		t.Fatalf("index descriptor: %v", err)
	}
	if _, err := c.ImageConfig(ctx, ref, Descriptor{Digest: pub.Digest}); err == nil || !strings.Contains(err.Error(), "is an index") {
		t.Fatalf("index body: %v", err)
	}
	if _, err := c.ImageConfig(ctx, ref, Descriptor{Digest: "sha256:../x"}); err == nil {
		t.Fatal("an invalid digest is refused before any request")
	}
	if _, err := c.ImageConfig(ctx, ref, Descriptor{Digest: testDigestA}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown manifest: %v", err)
	}
}

func configServer(t *testing.T, configDigest string, blob http.HandlerFunc) (*httptest.Server, Descriptor) {
	t.Helper()
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},"layers":[]}`,
		MediaTypeOCIManifest, configDigest)
	md := sha256Digest([]byte(manifest))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/app/manifests/"+md:
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			_, _ = fmt.Fprint(w, manifest)
		case strings.HasPrefix(r.URL.Path, "/v2/app/blobs/"):
			blob(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, Descriptor{MediaType: MediaTypeOCIManifest, Digest: md}
}

func TestImageConfigVerifiesTheBlob(t *testing.T) {
	cfg := `{"created":"2026-01-02T03:04:05Z","os":"linux","architecture":"amd64"}`
	srv, desc := configServer(t, sha256Digest([]byte(cfg)), func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, strings.Replace(cfg, "2026", "2016", 1))
	})
	_, err := New(nil).ImageConfig(context.Background(), Reference{Registry: hostOf(srv), Repo: "app"}, desc)
	if err == nil || !strings.Contains(err.Error(), "content digest") {
		t.Fatalf("err = %v, want a content digest mismatch", err)
	}
}

func TestImageConfigBlobRedirectDropsCredentials(t *testing.T) {
	cfg := `{"created":"2026-01-02T03:04:05Z","os":"linux","architecture":"amd64"}`
	var leaked atomic.Bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = fmt.Fprint(w, cfg)
	}))
	defer storage.Close()
	srv, desc := configServer(t, sha256Digest([]byte(cfg)), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("the registry itself gets the credentials")
		}
		http.Redirect(w, r, storage.URL+"/blob", http.StatusTemporaryRedirect)
	})
	host := hostOf(srv)
	c := New([]config.RegistryAuth{{Server: host, Username: "u", Password: "p"}})
	got, err := c.ImageConfig(context.Background(), Reference{Registry: host, Repo: "app"}, desc)
	if err != nil || got.Created.Year() != 2026 {
		t.Fatalf("ImageConfig = %+v, %v", got, err)
	}
	if leaked.Load() {
		t.Fatal("credentials were sent to the blob storage of another origin")
	}
}

func TestImageConfigRateLimited(t *testing.T) {
	reg := registrytest.New(t)
	pub := reg.Push("app", "1", registrytest.Image{})
	reg.Fail(http.MethodGet, "/manifests/", 1, http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}})
	_, err := New(nil).ImageConfig(context.Background(), Reference{Registry: reg.Host(), Repo: "app"},
		Descriptor{MediaType: MediaTypeOCIManifest, Digest: pub.Manifests[0]})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Op != "manifest" || rl.RetryAfter != 2*time.Minute {
		t.Fatalf("err = %v, want a rate limit with its Retry-After", err)
	}
}

func TestImageConfigRefusesOversizedBlob(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("x", maxConfigBytes) + `"}`
	srv, desc := configServer(t, sha256Digest([]byte(big)), func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, big)
	})
	_, err := New(nil).ImageConfig(context.Background(), Reference{Registry: hostOf(srv), Repo: "app"}, desc)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size error", err)
	}
}

func TestDigestHelpers(t *testing.T) {
	d := testDigestA
	for in, want := range map[string]string{
		"nginx":                              "nginx@" + d,
		"nginx:1.27":                         "nginx@" + d,
		"localhost:5000/app:1":               "localhost:5000/app@" + d,
		"localhost:5000/app":                 "localhost:5000/app@" + d,
		"ghcr.io/acme/app:v2@" + testDigestB: "ghcr.io/acme/app@" + d,
	} {
		if got := DigestReference(in, d); got != want {
			t.Errorf("DigestReference(%q) = %q, want %q", in, got, want)
		}
	}

	digests := []string{"example.com/other@" + testDigestB, "docker.io/library/nginx@" + d, "broken"}
	if got := RepoDigestFor(digests, Parse("nginx:1.27")); got != "docker.io/library/nginx@"+d {
		t.Errorf("RepoDigestFor = %q", got)
	}
	if got := RepoDigestFor(digests, Parse("example.com/app:1")); got != "" {
		t.Errorf("RepoDigestFor of another repository = %q", got)
	}

	for in, ok := range map[string]bool{"linux/amd64": true, "linux/arm/v7": true, "linux": false, "linux/arm/": false, "/amd64": false, "a/b/c/d": false} {
		if _, got := ParsePlatform(in); got != ok {
			t.Errorf("ParsePlatform(%q) ok = %v", in, got)
		}
	}
	if p, _ := ParsePlatform("linux/arm/v6"); p != (Platform{OS: "linux", Architecture: "arm", Variant: "v6"}) {
		t.Errorf("ParsePlatform = %+v", p)
	}
}

func TestVersionOfAndReleaseLabels(t *testing.T) {
	for _, tt := range []struct {
		label, tag, want string
	}{
		{"", "1.27", "1.27"},
		{"1.27.3", "1.27", "1.27.3"},
		{"1.27.3", "latest", "1.27.3"},
		{"3.19.1", "1.27", "1.27"},
		{"", "latest", ""},
	} {
		labels := map[string]string{}
		if tt.label != "" {
			labels["org.opencontainers.image.version"] = tt.label
		}
		if got := VersionOf(labels, tt.tag); got != tt.want {
			t.Errorf("VersionOf(%q, %q) = %q, want %q", tt.label, tt.tag, got, tt.want)
		}
	}
	all := map[string]string{
		"org.opencontainers.image.source":  "https://github.com/acme/app",
		"org.opencontainers.image.version": "1",
		"maintainer":                       "x",
		"dockpaum.link.template":           "https://example.org/${tag}",
	}
	got := ReleaseLabels(all, "dockpaum.link.template")
	if len(got) != 3 || got["maintainer"] != "" || got["dockpaum.link.template"] == "" {
		t.Errorf("ReleaseLabels = %v", got)
	}
	if ReleaseLabels(map[string]string{"x": "y"}) != nil {
		t.Error("no release label gives nil")
	}
}
