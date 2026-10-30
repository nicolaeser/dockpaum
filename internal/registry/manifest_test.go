package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var (
	testDigestA = "sha256:" + strings.Repeat("a", 64)
	testDigestB = "sha256:" + strings.Repeat("b", 64)
)

type recordingMetrics struct {
	mu        sync.Mutex
	list      []string
	auth      []string
	manifest  []string
	rateLimit []string
}

func (m *recordingMetrics) ListCall(registry, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, result)
}

func (m *recordingMetrics) AuthCall(registry, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auth = append(m.auth, result)
}

func (m *recordingMetrics) ManifestCall(registry, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manifest = append(m.manifest, result)
}

func (m *recordingMetrics) RateLimit(registry, account string, limit, remaining int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := fmt.Sprintf("%d/%d", remaining, limit)
	if account != "" {
		entry = account + ":" + entry
	}
	m.rateLimit = append(m.rateLimit, entry)
}

func TestManifestDigestHeadWithBearerTokenShared(t *testing.T) {
	var srv *httptest.Server
	var tokenCalls, heads atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls.Add(1)
			if got := r.URL.Query().Get("scope"); got != "repository:library/nginx:pull" {
				t.Errorf("token scope = %q", got)
			}
			_, _ = fmt.Fprint(w, `{"token":"t","expires_in":300}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="x",scope="repository:library/nginx:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/library/nginx/manifests/1.27":
			heads.Add(1)
			if r.Method != http.MethodHead {
				t.Errorf("method = %s, want HEAD", r.Method)
			}
			accept := r.Header.Get("Accept")
			for _, mt := range []string{MediaTypeOCIIndex, MediaTypeDockerManifestList, MediaTypeDockerManifest, MediaTypeOCIManifest} {
				if !strings.Contains(accept, mt) {
					t.Errorf("Accept %q lacks %s", accept, mt)
				}
			}
			if !strings.HasPrefix(accept, MediaTypeOCIIndex) {
				t.Errorf("Accept %q must prefer the OCI index", accept)
			}
			w.Header().Set("Docker-Content-Digest", testDigestA)
			w.Header().Set("Content-Type", MediaTypeOCIIndex+"; charset=utf-8")
			w.Header().Set("Content-Length", "1234")
			w.Header().Set("RateLimit-Limit", "100;w=21600")
			w.Header().Set("RateLimit-Remaining", "99;w=21600")
		case "/v2/library/nginx/tags/list":
			_, _ = fmt.Fprint(w, `{"tags":["1.27"]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &recordingMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	ref := Reference{Registry: hostOf(srv), Repo: "library/nginx", Tag: "1.27"}
	d, err := c.ManifestDigest(context.Background(), ref)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	want := Descriptor{MediaType: MediaTypeOCIIndex, Digest: testDigestA, Size: 1234}
	if d.MediaType != want.MediaType || d.Digest != want.Digest || d.Size != want.Size || !d.IsIndex() {
		t.Errorf("ManifestDigest = %+v, want %+v", d, want)
	}
	if _, err := c.ListTags(context.Background(), ref); err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 (shared cache)", got)
	}
	if got := heads.Load(); got != 1 {
		t.Errorf("manifest HEADs = %d, want 1", got)
	}
	rl, ok := c.LastRateLimit(hostOf(srv), "")
	if !ok || rl.Limit != 100 || rl.Remaining != 99 || rl.Window != 6*time.Hour {
		t.Errorf("LastRateLimit = %+v, %v", rl, ok)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.manifest, ",") != "ok" || strings.Join(m.list, ",") != "ok" {
		t.Errorf("metrics manifest=%v list=%v, want one ok each", m.manifest, m.list)
	}
	if strings.Join(m.rateLimit, ",") != "99/100" {
		t.Errorf("rate limit metrics = %v", m.rateLimit)
	}
}

func TestManifestDigestReference(t *testing.T) {
	tests := []struct {
		name     string
		ref      Reference
		wantPath string
	}{
		{name: "tag", ref: Reference{Repo: "app", Tag: "1.0"}, wantPath: "/v2/app/manifests/1.0"},
		{name: "implicit latest", ref: Reference{Repo: "app"}, wantPath: "/v2/app/manifests/latest"},
		{name: "digest wins over tag", ref: Reference{Repo: "app", Tag: "1.0", Digest: testDigestA}, wantPath: "/v2/app/manifests/" + testDigestA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tt.wantPath)
				}
				w.Header().Set("Docker-Content-Digest", testDigestA)
				w.Header().Set("Content-Type", MediaTypeDockerManifest)
			}))
			defer srv.Close()
			tt.ref.Registry = hostOf(srv)
			d, err := New(nil).ManifestDigest(context.Background(), tt.ref)
			if err != nil || d.Digest != testDigestA || d.MediaType != MediaTypeDockerManifest || d.IsIndex() {
				t.Fatalf("ManifestDigest = %+v, %v", d, err)
			}
		})
	}
}

func TestManifestDigestErrors(t *testing.T) {
	tests := []struct {
		name    string
		ref     Reference
		handler http.HandlerFunc
		check   func(t *testing.T, err error)
	}{
		{
			name:    "missing digest header",
			ref:     Reference{Repo: "app", Tag: "1"},
			handler: func(w http.ResponseWriter, r *http.Request) {},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoDigest) {
					t.Errorf("err = %v, want ErrNoDigest", err)
				}
			},
		},
		{
			name: "malformed digest header",
			ref:  Reference{Repo: "app", Tag: "1"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Docker-Content-Digest", "sha256:../../etc")
			},
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "invalid Docker-Content-Digest") {
					t.Errorf("err = %v", err)
				}
			},
		},
		{
			name: "not found",
			ref:  Reference{Repo: "app", Tag: "gone"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			check: func(t *testing.T, err error) {
				var he *HTTPError
				if !errors.Is(err, ErrNotFound) || !errors.As(err, &he) || he.Op != "manifest" || err.Error() != "manifest HTTP 404" {
					t.Errorf("err = %v, want a manifest 404 HTTPError", err)
				}
			},
		},
		{
			name: "rate limited",
			ref:  Reference{Repo: "app", Tag: "1"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.Header().Set("RateLimit-Limit", "100;w=21600")
				w.Header().Set("RateLimit-Remaining", "0;w=21600")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			check: func(t *testing.T, err error) {
				var rl *RateLimitError
				if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) {
					t.Fatalf("err = %v, want a RateLimitError", err)
				}
				if rl.Op != "manifest" || rl.RetryAfter != 30*time.Second || !rl.HasQuota || rl.Quota.Remaining != 0 || rl.Quota.Limit != 100 {
					t.Errorf("RateLimitError = %+v", rl)
				}
			},
		},
		{
			name: "digest mismatch",
			ref:  Reference{Repo: "app", Digest: testDigestA},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Docker-Content-Digest", testDigestB)
			},
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "answered with digest") {
					t.Errorf("err = %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			tt.ref.Registry = hostOf(srv)
			_, err := New(nil).ManifestDigest(context.Background(), tt.ref)
			tt.check(t, err)
		})
	}
}

func TestManifestDigestRefusesInvalidReferences(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	for _, ref := range []Reference{
		{Repo: "app", Tag: "../../v2/other/manifests/1"},
		{Repo: "app", Tag: "1?x=y"},
		{Repo: "Owner/App", Tag: "1"},
		{Repo: "app/../other", Tag: "1"},
		{Repo: "app", Digest: "sha256:short"},
	} {
		ref.Registry = hostOf(srv)
		if _, err := New(nil).ManifestDigest(context.Background(), ref); err == nil {
			t.Errorf("ManifestDigest(%+v) succeeded", ref)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("invalid references reached the registry %d times", calls.Load())
	}
}

const testIndex = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111", "size": 100, "platform": {"os": "linux", "architecture": "amd64"}},
    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222", "size": 100, "platform": {"os": "linux", "architecture": "arm64", "variant": "v8"}},
    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333", "size": 100, "platform": {"os": "linux", "architecture": "arm", "variant": "v7"}},
    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444", "size": 100, "platform": {"os": "linux", "architecture": "arm", "variant": "v6"}},
    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555", "size": 100, "platform": {"os": "unknown", "architecture": "unknown"}}
  ]
}`

func manifestServer(t *testing.T, body, contentType string, gets *atomic.Int32) *httptest.Server {
	t.Helper()
	digest := sha256Digest([]byte(body))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/app/manifests/"+digest {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gets.Add(1)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestPlatformManifestResolvesIndex(t *testing.T) {
	var gets atomic.Int32
	srv := manifestServer(t, testIndex, MediaTypeOCIIndex, &gets)
	defer srv.Close()
	ref := Reference{Registry: hostOf(srv), Repo: "app", Tag: "1"}
	desc := Descriptor{MediaType: MediaTypeOCIIndex, Digest: sha256Digest([]byte(testIndex))}

	tests := []struct {
		platform Platform
		want     string
	}{
		{Platform{OS: "linux", Architecture: "amd64"}, "1"},
		{Platform{OS: "linux", Architecture: "x86_64"}, "1"},
		{Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}, "1"},
		{Platform{OS: "linux", Architecture: "arm64"}, "2"},
		{Platform{OS: "linux", Architecture: "aarch64"}, "2"},
		{Platform{OS: "Linux", Architecture: "arm64", Variant: "v8"}, "2"},
		{Platform{OS: "linux", Architecture: "arm"}, "3"},
		{Platform{OS: "linux", Architecture: "arm", Variant: "7"}, "3"},
		{Platform{OS: "linux", Architecture: "armhf"}, ""},
		{Platform{OS: "linux", Architecture: "armel"}, ""},
		{Platform{OS: "linux", Architecture: "armhf", Variant: "v7"}, ""},
		{Platform{OS: "linux", Architecture: "arm", Variant: "v6"}, "4"},
		{Platform{OS: "linux", Architecture: "arm", Variant: "v5"}, ""},
		{Platform{OS: "windows", Architecture: "amd64"}, ""},
		{Platform{OS: "linux", Architecture: "s390x"}, ""},
		{Platform{OS: "unknown", Architecture: "arm64"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.platform.String(), func(t *testing.T) {
			got, err := New(nil).PlatformManifest(context.Background(), ref, desc, tt.platform)
			if tt.want == "" {
				if !errors.Is(err, ErrPlatformNotFound) {
					t.Fatalf("PlatformManifest = %+v, %v; want ErrPlatformNotFound", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := "sha256:" + strings.Repeat(tt.want, 64); got.Digest != want || got.MediaType != MediaTypeOCIManifest || got.Platform == nil {
				t.Errorf("PlatformManifest = %+v, want digest %s", got, want)
			}
		})
	}
	if int(gets.Load()) != len(tests) {
		t.Errorf("GETs = %d, want one per resolution", gets.Load())
	}
}

func TestPlatformManifestSingleManifestNeedsNoRequest(t *testing.T) {
	var gets atomic.Int32
	srv := manifestServer(t, "{}", "", &gets)
	defer srv.Close()
	desc := Descriptor{MediaType: MediaTypeDockerManifest, Digest: testDigestA, Size: 10}
	got, err := New(nil).PlatformManifest(context.Background(),
		Reference{Registry: hostOf(srv), Repo: "app"}, desc, Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || got.Digest != desc.Digest || got.MediaType != desc.MediaType {
		t.Fatalf("PlatformManifest = %+v, %v; want the descriptor itself", got, err)
	}
	if gets.Load() != 0 {
		t.Errorf("a single-platform manifest was fetched %d times", gets.Load())
	}
}

func TestPlatformManifestUnknownMediaTypeIsFetched(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantIndex   bool
	}{
		{
			name:        "manifest without content type",
			body:        `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:x"}}`,
			contentType: "",
		},
		{
			name:        "index identified by body",
			body:        testIndex,
			contentType: "application/json",
			wantIndex:   true,
		},
		{
			name:        "index without any media type",
			body:        strings.Replace(testIndex, `"mediaType": "application/vnd.oci.image.index.v1+json",`, "", 1),
			contentType: "",
			wantIndex:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gets atomic.Int32
			srv := manifestServer(t, tt.body, tt.contentType, &gets)
			defer srv.Close()
			desc := Descriptor{Digest: sha256Digest([]byte(tt.body))}
			got, err := New(nil).PlatformManifest(context.Background(),
				Reference{Registry: hostOf(srv), Repo: "app"}, desc, Platform{OS: "linux", Architecture: "amd64"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantIndex {
				if got.Digest != "sha256:"+strings.Repeat("1", 64) {
					t.Errorf("PlatformManifest = %+v, want the amd64 entry", got)
				}
			} else if got.Digest != desc.Digest || got.MediaType != MediaTypeDockerManifest || got.Size != int64(len(tt.body)) {
				t.Errorf("PlatformManifest = %+v, want the fetched manifest", got)
			}
			if gets.Load() != 1 {
				t.Errorf("GETs = %d, want 1", gets.Load())
			}
		})
	}
}

func TestPlatformManifestVerifiesContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaTypeOCIIndex)
		_, _ = fmt.Fprint(w, testIndex)
	}))
	defer srv.Close()
	desc := Descriptor{MediaType: MediaTypeOCIIndex, Digest: testDigestA}
	_, err := New(nil).PlatformManifest(context.Background(),
		Reference{Registry: hostOf(srv), Repo: "app"}, desc, Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "content digest") {
		t.Fatalf("err = %v, want a content digest mismatch", err)
	}
}

func TestPlatformManifestRefusesOversizedManifest(t *testing.T) {
	big := `{"manifests":[],"pad":"` + strings.Repeat("x", maxManifestBytes) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, big)
	}))
	defer srv.Close()
	desc := Descriptor{MediaType: MediaTypeOCIIndex, Digest: sha256Digest([]byte(big))}
	_, err := New(nil).PlatformManifest(context.Background(),
		Reference{Registry: hostOf(srv), Repo: "app"}, desc, Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size error", err)
	}
}

func TestPlatformManifestRejectsInvalidEntryDigest(t *testing.T) {
	body := `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:../x","platform":{"os":"linux","architecture":"amd64"}}]}`
	var gets atomic.Int32
	srv := manifestServer(t, body, MediaTypeOCIIndex, &gets)
	defer srv.Close()
	desc := Descriptor{MediaType: MediaTypeOCIIndex, Digest: sha256Digest([]byte(body))}
	_, err := New(nil).PlatformManifest(context.Background(),
		Reference{Registry: hostOf(srv), Repo: "app"}, desc, Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "invalid digest") {
		t.Fatalf("err = %v, want an invalid entry digest error", err)
	}
}

func TestHasRepoDigest(t *testing.T) {
	repoDigests := []string{
		"nginx@" + testDigestA,
		"ghcr.io/acme/app@" + testDigestB,
		"malformed",
	}
	tests := []struct {
		name   string
		ref    string
		digest string
		want   bool
	}{
		{name: "short hub name", ref: "nginx:1.27", digest: testDigestA, want: true},
		{name: "docker.io spelling", ref: "docker.io/library/nginx:1.27", digest: testDigestA, want: true},
		{name: "index.docker.io spelling", ref: "index.docker.io/nginx", digest: testDigestA, want: true},
		{name: "other digest", ref: "nginx:1.27", digest: testDigestB, want: false},
		{name: "same digest other repository", ref: "ghcr.io/acme/other:1", digest: testDigestB, want: false},
		{name: "explicit registry", ref: "ghcr.io/acme/app:1", digest: testDigestB, want: true},
		{name: "empty digest", ref: "nginx", digest: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRepoDigest(repoDigests, Parse(tt.ref), tt.digest); got != tt.want {
				t.Errorf("HasRepoDigest(%q, %q) = %v, want %v", tt.ref, tt.digest, got, tt.want)
			}
		})
	}
}

func TestListTagsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"errors":[{"code":"TOOMANYREQUESTS"}]}`)
	}))
	defer srv.Close()
	m := &recordingMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	_, err := c.ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	var rl *RateLimitError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != 2*time.Minute || rl.Op != "tags/list" {
		t.Fatalf("err = %v, want a tags/list RateLimitError", err)
	}
	if !strings.HasPrefix(err.Error(), "tags/list HTTP 429") || !strings.Contains(err.Error(), "TOOMANYREQUESTS") {
		t.Errorf("message = %q", err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.list, ",") != "rate_limited" || len(m.manifest) != 0 {
		t.Errorf("metrics list=%v manifest=%v", m.list, m.manifest)
	}
}

func TestListTagsErrorKeepsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, "denied\n")
	}))
	defer srv.Close()
	_, err := New(nil).ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err == nil || err.Error() != "tags/list HTTP 403: denied" {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenRateLimited(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="x"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := &recordingMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	_, err := c.ManifestDigest(context.Background(), Reference{Registry: hostOf(srv), Repo: "app", Tag: "1"})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Op != "token" {
		t.Fatalf("err = %v, want a token RateLimitError", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.manifest, ",") != "rate_limited" || strings.Join(m.auth, ",") != "error" {
		t.Errorf("metrics manifest=%v auth=%v", m.manifest, m.auth)
	}
}

type listOnlyMetrics struct{ list atomic.Int32 }

func (m *listOnlyMetrics) ListCall(string, string) { m.list.Add(1) }
func (m *listOnlyMetrics) AuthCall(string, string) {}

func TestManifestCallsWithoutManifestMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", testDigestA)
		w.Header().Set("RateLimit-Limit", "100")
		w.Header().Set("RateLimit-Remaining", "50")
	}))
	defer srv.Close()
	m := &listOnlyMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	if _, err := c.ManifestDigest(context.Background(), Reference{Registry: hostOf(srv), Repo: "app", Tag: "1"}); err != nil {
		t.Fatal(err)
	}
	if m.list.Load() != 0 {
		t.Error("a manifest request was counted as a tag listing")
	}
}

func TestManifestDigestConcurrentQuotaTracking(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", testDigestA)
		w.Header().Set("RateLimit-Limit", "100;w=3600")
		w.Header().Set("RateLimit-Remaining", fmt.Sprintf("%d;w=3600", 100-n.Add(1)))
	}))
	defer srv.Close()
	m := &recordingMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	ref := Reference{Registry: hostOf(srv), Repo: "app", Tag: "1"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ManifestDigest(context.Background(), ref); err != nil {
				t.Error(err)
			}
			_, _ = c.LastRateLimit(ref.Registry, "")
			_ = c.RateLimits()
		}()
	}
	wg.Wait()
	rl, ok := c.LastRateLimit(ref.Registry, "")
	if !ok || rl.Limit != 100 || rl.Remaining < 92 || rl.Remaining > 99 {
		t.Errorf("LastRateLimit = %+v, %v", rl, ok)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.manifest) != 8 {
		t.Errorf("recorded %d manifest calls, want 8", len(m.manifest))
	}
	want := fmt.Sprintf("%d/%d", rl.Remaining, rl.Limit)
	if n := len(m.rateLimit); n < 1 || n > 8 || m.rateLimit[n-1] != want {
		t.Errorf("rate limit metrics = %v, want at most 8 ending with %s", m.rateLimit, want)
	}
}
