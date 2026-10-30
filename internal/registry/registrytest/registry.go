package registrytest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	MediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
)

type Image struct {
	OS           string
	Architecture string
	Variant      string
	Created      time.Time
	Labels       map[string]string
	Build        string
}

type Published struct {
	Digest    string
	Manifests []string
	Configs   []string
}

type Call struct {
	Method string
	Path   string
}

type Registry struct {
	srv *httptest.Server

	mu      sync.Mutex
	content map[string]content
	tags    map[string]map[string]string
	calls   []Call
	faults  []fault
	users   map[string][]Login
	grants  map[string]string
	logins  []TokenRequest
}

type Login struct {
	Username, Password string
}

type TokenRequest struct {
	Repo     string
	Username string
	Password string
	Granted  bool
}

type content struct {
	mediaType string
	body      []byte
}

type fault struct {
	method, contains string
	status           int
	header           http.Header
	remaining        int
}

func New(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{content: map[string]content{}, tags: map[string]map[string]string{}, users: map[string][]Login{}, grants: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *Registry) Host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *Registry) Push(repo, tag string, imgs ...Image) Published {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out Published
	var entries []map[string]any
	for _, img := range imgs {
		if img.OS == "" {
			img.OS = "linux"
		}
		if img.Architecture == "" {
			img.Architecture = "amd64"
		}
		cfg := map[string]any{
			"os": img.OS, "architecture": img.Architecture,
			"config": map[string]any{"Labels": img.Labels},
			"rootfs": map[string]any{"type": "layers", "diff_ids": []string{digestOf([]byte(repo + "\x00" + img.Build))}},
		}
		if img.Variant != "" {
			cfg["variant"] = img.Variant
		}
		if !img.Created.IsZero() {
			cfg["created"] = img.Created.UTC().Format(time.RFC3339Nano)
		}
		cfgDigest, cfgSize := r.store(mediaTypeConfig, cfg)
		man := map[string]any{
			"schemaVersion": 2, "mediaType": MediaTypeManifest,
			"config": map[string]any{"mediaType": mediaTypeConfig, "digest": cfgDigest, "size": cfgSize},
			"layers": []any{},
		}
		manDigest, manSize := r.store(MediaTypeManifest, man)
		out.Configs = append(out.Configs, cfgDigest)
		out.Manifests = append(out.Manifests, manDigest)
		platform := map[string]any{"os": img.OS, "architecture": img.Architecture}
		if img.Variant != "" {
			platform["variant"] = img.Variant
		}
		entries = append(entries, map[string]any{"mediaType": MediaTypeManifest, "digest": manDigest, "size": manSize, "platform": platform})
	}
	switch len(entries) {
	case 0:
		return out
	case 1:
		out.Digest = out.Manifests[0]
	default:
		out.Digest, _ = r.store(MediaTypeIndex, map[string]any{"schemaVersion": 2, "mediaType": MediaTypeIndex, "manifests": entries})
	}
	if r.tags[repo] == nil {
		r.tags[repo] = map[string]string{}
	}
	r.tags[repo][tag] = out.Digest
	return out
}

func (r *Registry) Tag(repo, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tags[repo] == nil {
		r.tags[repo] = map[string]string{}
	}
	if _, ok := r.tags[repo][tag]; !ok {
		r.tags[repo][tag] = ""
	}
}

func (r *Registry) RequireAuth(repo string, logins ...Login) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[repo] = append(r.users[repo], logins...)
}

func (r *Registry) TokenRequests() []TokenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TokenRequest(nil), r.logins...)
}

func (r *Registry) Fail(method, contains string, n, status int, header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, fault{method: method, contains: contains, status: status, header: header, remaining: n})
}

func (r *Registry) Calls(method, contains string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if (method == "" || c.Method == method) && strings.Contains(c.Path, contains) {
			out = append(out, c)
		}
	}
	return out
}

func (r *Registry) store(mediaType string, v any) (string, int) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	d := digestOf(body)
	r.content[d] = content{mediaType: mediaType, body: body}
	return d, len(body)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: req.Method, Path: req.URL.Path})
	for i := range r.faults {
		f := &r.faults[i]
		if f.remaining == 0 || (f.method != "" && f.method != req.Method) || !strings.Contains(req.URL.Path, f.contains) {
			continue
		}
		if f.remaining > 0 {
			f.remaining--
		}
		for k, vs := range f.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"errors":[{"code":"INJECTED","message":"injected by the test"}]}`))
		return
	}
	path := req.URL.Path
	if path == "/token" {
		r.token(w, req)
		return
	}
	if repo := repoOf(path); repo != "" && r.users[repo] != nil {
		bearer, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok || r.grants[bearer] != repo {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="registrytest",scope="repository:`+repo+`:pull"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
			return
		}
	}
	switch {
	case path == "/v2/" || path == "/v2":
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(path, "/tags/list") && req.Method == http.MethodGet:
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
		tags, ok := r.tags[repo]
		if !ok {
			notFound(w, "NAME_UNKNOWN")
			return
		}
		list := make([]string, 0, len(tags))
		for t := range tags {
			list = append(list, t)
		}
		sort.Strings(list)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": list})
	case strings.Contains(path, "/manifests/") && (req.Method == http.MethodGet || req.Method == http.MethodHead):
		repo, ref, _ := strings.Cut(strings.TrimPrefix(path, "/v2/"), "/manifests/")
		digest := ref
		if !strings.HasPrefix(ref, "sha256:") {
			digest = r.tags[repo][ref]
		}
		c, ok := r.content[digest]
		if !ok || c.mediaType == mediaTypeConfig || r.tags[repo] == nil {
			notFound(w, "MANIFEST_UNKNOWN")
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", c.mediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(c.body)))
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(c.body)
		}
	case strings.Contains(path, "/blobs/") && req.Method == http.MethodGet:
		_, digest, _ := strings.Cut(path, "/blobs/")
		c, ok := r.content[digest]
		if !ok || c.mediaType != mediaTypeConfig {
			notFound(w, "BLOB_UNKNOWN")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(c.body)
	default:
		notFound(w, "UNSUPPORTED")
	}
}

func repoOf(path string) string {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return ""
	}
	for _, sep := range []string{"/tags/list", "/manifests/", "/blobs/"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}

func (r *Registry) token(w http.ResponseWriter, req *http.Request) {
	repo := strings.TrimSuffix(strings.TrimPrefix(req.URL.Query().Get("scope"), "repository:"), ":pull")
	user, pass, _ := req.BasicAuth()
	granted := false
	for _, l := range r.users[repo] {
		if l.Username == user && l.Password == pass {
			granted = true
		}
	}
	r.logins = append(r.logins, TokenRequest{Repo: repo, Username: user, Password: pass, Granted: granted})
	w.Header().Set("Content-Type", "application/json")
	if !granted {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"incorrect username or password"}]}`))
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	r.grants[tok] = repo
	_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_in": 300})
}

func notFound(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"errors":[{"code":"` + code + `"}]}`))
}
