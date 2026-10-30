package cli

import (
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
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociConfigType   = "application/vnd.oci.image.config.v1+json"
)

type testRegistry struct {
	srv *httptest.Server

	mu    sync.Mutex
	docs  map[string]registryDoc
	tags  map[string]map[string]string
	calls []string
}

type registryDoc struct {
	mediaType string
	body      []byte
}

type registryImage struct {
	build   string
	created time.Time
	labels  map[string]string
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()
	r := &testRegistry{docs: map[string]registryDoc{}, tags: map[string]map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *testRegistry) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *testRegistry) push(repo, tag string, img registryImage) (manifest, configDigest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := map[string]any{
		"os": "linux", "architecture": "amd64",
		"config": map[string]any{"Labels": img.labels},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{contentDigest([]byte(repo + "\x00" + img.build))}},
	}
	if !img.created.IsZero() {
		cfg["created"] = img.created.UTC().Format(time.RFC3339Nano)
	}
	configDigest, size := r.store(ociConfigType, cfg)
	manifest, _ = r.store(ociManifestType, map[string]any{
		"schemaVersion": 2, "mediaType": ociManifestType,
		"config": map[string]any{"mediaType": ociConfigType, "digest": configDigest, "size": size},
		"layers": []any{},
	})
	if r.tags[repo] == nil {
		r.tags[repo] = map[string]string{}
	}
	r.tags[repo][tag] = manifest
	return manifest, configDigest
}

func (r *testRegistry) count(method, contains string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		m, path, _ := strings.Cut(c, " ")
		if (method == "" || m == method) && strings.Contains(path, contains) {
			n++
		}
	}
	return n
}

func (r *testRegistry) store(mediaType string, v any) (string, int) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	d := contentDigest(body)
	r.docs[d] = registryDoc{mediaType: mediaType, body: body}
	return d, len(body)
}

func contentDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *testRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req.Method+" "+req.URL.Path)
	path := req.URL.Path
	if path == "/v2/" || path == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	if repo, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		tags := make([]string, 0, len(r.tags[repo]))
		for t := range r.tags[repo] {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
		return
	}
	var doc registryDoc
	var digest string
	switch {
	case strings.Contains(rest, "/manifests/"):
		i := strings.LastIndex(rest, "/manifests/")
		repo, ref := rest[:i], rest[i+len("/manifests/"):]
		digest = ref
		if !strings.HasPrefix(ref, "sha256:") {
			digest = r.tags[repo][ref]
		}
		d, found := r.docs[digest]
		if !found || d.mediaType != ociManifestType {
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return
		}
		doc = d
	case strings.Contains(rest, "/blobs/"):
		digest = rest[strings.LastIndex(rest, "/blobs/")+len("/blobs/"):]
		d, found := r.docs[digest]
		if !found || d.mediaType != ociConfigType {
			registryError(w, http.StatusNotFound, "BLOB_UNKNOWN")
			return
		}
		doc = d
	default:
		http.NotFound(w, req)
		return
	}
	switch req.Method {
	case http.MethodHead, http.MethodGet:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", doc.mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(doc.body)))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write(doc.body)
	}
}

func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": strings.ToLower(code)}}})
}
