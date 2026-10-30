package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func metricsStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("# metrics")) })
}

func do(s *Server, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthNeedsTheTokenOnceOneIsSet(t *testing.T) {
	s := New(config.HTTPConfig{Token: "secret", ReadToken: "reader"}, metricsStub(), discard(), Version{Version: "test"}, nil, API{})
	if rec := do(s, "GET", "/internal-api/health", ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("health without a token = %d, want 401 with a challenge", rec.Code)
	}
	for _, auth := range []string{"Bearer reader", "Bearer secret"} {
		if rec := do(s, "GET", "/internal-api/health", auth); rec.Code != 200 || rec.Body.String() != "ok" {
			t.Fatalf("health with %q = %d %q", auth, rec.Code, rec.Body.String())
		}
	}
	open := New(config.HTTPConfig{}, metricsStub(), discard(), Version{Version: "test"}, nil, API{})
	if rec := do(open, "GET", "/internal-api/health", ""); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("health without any token configured = %d %q", rec.Code, rec.Body.String())
	}
}

func TestReadinessFollowsProbe(t *testing.T) {
	var probeErr error
	s := New(config.HTTPConfig{Token: "secret"}, metricsStub(), discard(), Version{}, func() error { return probeErr }, API{})
	if rec := do(s, "GET", "/internal-api/ready", ""); rec.Code != 401 {
		t.Fatalf("ready without the token = %d, want 401", rec.Code)
	}
	if rec := do(s, "GET", "/internal-api/ready", "Bearer secret"); rec.Code != 200 || rec.Body.String() != "ready" {
		t.Fatalf("ready with passing probe = %d %q, want 200 ready", rec.Code, rec.Body.String())
	}
	probeErr = errors.New("scheduler not running")
	rec := do(s, "GET", "/internal-api/ready", "Bearer secret")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "scheduler not running") {
		t.Fatalf("ready with failing probe = %d %q, want 503 with the reason", rec.Code, rec.Body.String())
	}
}

func TestReadinessWithoutProbe(t *testing.T) {
	s := New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{})
	if rec := do(s, "GET", "/internal-api/ready", ""); rec.Code != 200 {
		t.Fatalf("ready without probe = %d, want 200", rec.Code)
	}
}

func TestBearerAuth(t *testing.T) {
	s := New(config.HTTPConfig{Token: "secret"}, metricsStub(), discard(), Version{Version: "test"}, nil, API{})
	for _, path := range []string{"/internal-api/version", "/internal-api/metrics"} {
		if rec := do(s, "GET", path, ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s without token = %d, want 401 with a challenge", path, rec.Code)
		}
		if rec := do(s, "GET", path, "Bearer wrong"); rec.Code != 401 {
			t.Errorf("%s wrong token = %d, want 401", path, rec.Code)
		}
		if rec := do(s, "GET", path, "Bearer secret"); rec.Code != 200 {
			t.Errorf("%s correct token = %d, want 200", path, rec.Code)
		}
	}
}

func TestNoTokenLeavesEndpointsOpen(t *testing.T) {
	s := New(config.HTTPConfig{}, metricsStub(), discard(), Version{Version: "test"}, nil, API{})
	if rec := do(s, "GET", "/internal-api/version", ""); rec.Code != 200 {
		t.Fatalf("version without token configured = %d, want 200", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/internal-api/version", ""); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST version = %d, Allow %q, want 405 with GET, HEAD", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBearerPrefixRequired(t *testing.T) {
	s := New(config.HTTPConfig{Token: "secret"}, metricsStub(), discard(), Version{Version: "test"}, nil, API{})
	for _, h := range []string{"secret", "Basic secret", "Bearer", "Bearer ", "Bearer wrong", "Bearer secret2", "Bearer secre"} {
		if rec := do(s, http.MethodGet, "/internal-api/version", h); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", h, rec.Code)
		}
	}
	if rec := do(s, http.MethodGet, "/internal-api/version", "bearer secret"); rec.Code != http.StatusOK {
		t.Errorf("case-insensitive Bearer scheme: status %d", rec.Code)
	}
}

func TestVersionDocument(t *testing.T) {
	s := New(config.HTTPConfig{}, metricsStub(), discard(), Version{Version: "1.2.3", Commit: "abc1234", Date: "2026-10-01", Go: "go1.25.0", Platform: "linux/amd64"}, nil, API{})
	rec := do(s, http.MethodGet, "/internal-api/version", "")
	for _, want := range []string{`"version":"1.2.3"`, `"commit":"abc1234"`, `"date":"2026-10-01"`, `"go":"go1.25.0"`, `"platform":"linux/amd64"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("version body %s missing %s", rec.Body.String(), want)
		}
	}
}

func TestReadTokenScopes(t *testing.T) {
	api := API{
		Trigger:           func(TriggerRequest) string { return "queued" },
		Status:            func() any { return map[string]string{"ok": "yes"} },
		Containers:        func(ContainersQuery) any { return []string{} },
		Summary:           func() Summary { return Summary{} },
		Busy:              func() Busy { return Busy{} },
		WatchtowerMetrics: metricsStub(),
		WatchtowerCompat:  true,
	}
	type call struct {
		method, path string
	}
	reads := []call{
		{http.MethodGet, "/internal-api/health"},
		{http.MethodGet, "/internal-api/ready"},
		{http.MethodGet, "/internal-api/busy"},
		{http.MethodGet, "/internal-api/version"},
		{http.MethodGet, "/internal-api/metrics"},
		{http.MethodGet, "/internal-api/v1/status"},
		{http.MethodGet, "/internal-api/v1/containers"},
		{http.MethodGet, "/internal-api/v1/summary"},
		{http.MethodGet, "/v1/metrics"},
	}
	writes := []call{
		{http.MethodPost, "/internal-api/v1/update"},
		{http.MethodPost, "/internal-api/v1/check"},
		{http.MethodPost, "/v1/update"},
		{http.MethodGet, "/v1/update"},
	}
	cases := []struct {
		name      string
		cfg       config.HTTPConfig
		auth      string
		readCode  int
		writeCode int
	}{
		{"full token", config.HTTPConfig{Token: "full", ReadToken: "reader", UpdateAPI: true}, "Bearer full", 200, 202},
		{"read token", config.HTTPConfig{Token: "full", ReadToken: "reader", UpdateAPI: true}, "Bearer reader", 200, 403},
		{"no credentials", config.HTTPConfig{Token: "full", ReadToken: "reader", UpdateAPI: true}, "", 401, 401},
		{"wrong token", config.HTTPConfig{Token: "full", ReadToken: "reader", UpdateAPI: true}, "Bearer nope", 401, 401},
		{"read token only configured", config.HTTPConfig{ReadToken: "reader"}, "Bearer reader", 200, 404},
		{"read token only, no credentials", config.HTTPConfig{ReadToken: "reader"}, "", 401, 404},
		{"read token only, wrong token", config.HTTPConfig{ReadToken: "reader"}, "Bearer full", 401, 404},
		{"nothing configured", config.HTTPConfig{}, "", 200, 404},
		{"identical tokens act as full", config.HTTPConfig{Token: "same", ReadToken: "same", UpdateAPI: true}, "Bearer same", 200, 202},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg, metricsStub(), discard(), Version{}, nil, api)
			for _, c := range reads {
				if rec := do(s, c.method, c.path, tc.auth); rec.Code != tc.readCode {
					t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, tc.readCode)
				}
			}
			for _, c := range writes {
				if rec := do(s, c.method, c.path, tc.auth); rec.Code != tc.writeCode {
					t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, tc.writeCode)
				}
			}
		})
	}
}

func TestTokenEqual(t *testing.T) {
	cases := []struct {
		got, want string
		ok        bool
	}{
		{"secret", "secret", true},
		{"secret", "Secret", false},
		{"secret", "secret ", false},
		{"", "", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := tokenEqual(c.got, c.want); got != c.ok {
			t.Errorf("tokenEqual(%q, %q) = %v, want %v", c.got, c.want, got, c.ok)
		}
	}
}

func TestUpdateAPI(t *testing.T) {
	var got TriggerRequest
	api := API{
		Trigger: func(r TriggerRequest) string { got = r; return "queued" },
		Status:  func() any { return map[string]string{"ok": "yes"} },
	}
	do := func(cfg config.HTTPConfig, method, target, auth string) *httptest.ResponseRecorder {
		return do(New(cfg, metricsStub(), discard(), Version{}, nil, api), method, target, auth)
	}
	on := config.HTTPConfig{Token: "secret", UpdateAPI: true}
	if rec := do(config.HTTPConfig{Token: "secret"}, http.MethodPost, "/internal-api/v1/update", "Bearer secret"); rec.Code != http.StatusNotFound {
		t.Errorf("disabled update API: %d, want 404", rec.Code)
	}
	if rec := do(on, http.MethodPost, "/internal-api/v1/update", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("update without token: %d, want 401", rec.Code)
	}
	if rec := do(on, http.MethodGet, "/internal-api/v1/update", "Bearer secret"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET update: %d Allow=%q, want 405 Allow=POST", rec.Code, rec.Header().Get("Allow"))
	}
	rec := do(on, http.MethodPost, "/internal-api/v1/update?container=web&container=api&policy=db", "Bearer secret")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued"`) {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if len(got.Containers) != 2 || got.Containers[1] != "api" || got.Policy != "db" || got.Endpoint != EndpointInternal {
		t.Errorf("trigger request = %+v", got)
	}
	if rec := do(on, http.MethodGet, "/internal-api/v1/status", "Bearer secret"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("status: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(on, http.MethodGet, "/internal-api/v1/status", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status without token: %d, want 401", rec.Code)
	}
}

func TestUpdateAPIStatusCodes(t *testing.T) {
	for status, code := range map[string]int{
		"queued":          http.StatusAccepted,
		"already-queued":  http.StatusAccepted,
		"too-many-queued": http.StatusTooManyRequests,
		"unavailable":     http.StatusServiceUnavailable,
	} {
		api := API{Trigger: func(TriggerRequest) string { return status }, WatchtowerCompat: true}
		s := New(config.HTTPConfig{Token: "secret", UpdateAPI: true}, metricsStub(), discard(), Version{}, nil, api)
		for _, path := range []string{"/internal-api/v1/update", "/internal-api/v1/check", "/v1/update"} {
			rec := do(s, http.MethodPost, path, "Bearer secret")
			if rec.Code != code || !strings.Contains(rec.Body.String(), `"`+status+`"`) {
				t.Errorf("%s with %q = %d %s, want %d", path, status, rec.Code, rec.Body.String(), code)
			}
		}
	}
}

func TestWatchtowerUpdate(t *testing.T) {
	var got TriggerRequest
	calls := 0
	api := API{Trigger: func(r TriggerRequest) string { got = r; calls++; return "queued" }, WatchtowerCompat: true}
	on := config.HTTPConfig{Token: "secret", UpdateAPI: true}
	s := New(on, metricsStub(), discard(), Version{}, nil, api)

	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if rec := do(s, m, "/v1/update", "Bearer secret"); rec.Code != http.StatusAccepted {
			t.Errorf("%s /v1/update = %d, want 202", m, rec.Code)
		}
		if got.Endpoint != EndpointWatchtower || len(got.Images) != 0 {
			t.Errorf("%s /v1/update request = %+v", m, got)
		}
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodHead} {
		if rec := do(s, m, "/v1/update", "Bearer secret"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
			t.Errorf("%s /v1/update = %d Allow=%q, want 405 Allow=GET, POST", m, rec.Code, rec.Header().Get("Allow"))
		}
	}

	rec := do(s, http.MethodGet, "/v1/update?image=nginx,redis:7&image=+ghcr.io/acme/api+&image=nginx&image=,", "Bearer secret")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("image update = %d %s", rec.Code, rec.Body.String())
	}
	want := []string{"nginx", "redis:7", "ghcr.io/acme/api"}
	if fmt.Sprint(got.Images) != fmt.Sprint(want) {
		t.Errorf("images = %q, want %q", got.Images, want)
	}

	before := calls
	if rec := do(s, http.MethodGet, "/v1/update?image=bad%20name", "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("image with a space = %d, want 400", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/v1/update?image=bad%0Aname", "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("image with a newline = %d, want 400", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/v1/update?image="+strings.Repeat("a", 513), "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("overlong image = %d, want 400", rec.Code)
	}
	var many strings.Builder
	for i := range maxTriggerTargets + 1 {
		fmt.Fprintf(&many, "img%d,", i)
	}
	if rec := do(s, http.MethodGet, "/v1/update?image="+many.String(), "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("too many images = %d, want 400", rec.Code)
	}
	if calls != before {
		t.Errorf("rejected requests reached Trigger %d times", calls-before)
	}

	off := New(config.HTTPConfig{Token: "secret"}, metricsStub(), discard(), Version{}, nil, api)
	if rec := do(off, http.MethodGet, "/v1/update", "Bearer secret"); rec.Code != http.StatusNotFound {
		t.Errorf("/v1/update with update_api off = %d, want 404", rec.Code)
	}
}

func TestWatchtowerRoutesNeedCompat(t *testing.T) {
	calls := 0
	api := API{
		Trigger:           func(TriggerRequest) string { calls++; return "queued" },
		WatchtowerMetrics: metricsStub(),
	}
	s := New(config.HTTPConfig{Token: "secret", UpdateAPI: true}, metricsStub(), discard(), Version{}, nil, api)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/update"},
		{http.MethodPost, "/v1/update"},
		{http.MethodGet, "/v1/metrics"},
	} {
		if rec := do(s, c.method, c.path, "Bearer secret"); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s without WatchtowerCompat = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if calls != 0 {
		t.Errorf("Trigger called %d times", calls)
	}
	if rec := do(s, http.MethodPost, "/internal-api/v1/update", "Bearer secret"); rec.Code != http.StatusAccepted {
		t.Errorf("/internal-api/v1/update = %d, want 202", rec.Code)
	}
}

func TestParseTriggerPolicyAndDuplicates(t *testing.T) {
	req, err := parseTrigger(url.Values{"policy": {"db", " db "}, "container": {"web", "/web", "db"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Policy != "db" || strings.Join(req.Containers, ",") != "web,db" {
		t.Errorf("request = %+v, want policy db and containers web,db", req)
	}
	if _, err := parseTrigger(url.Values{"policy": {"db", "web"}}); err == nil || !strings.Contains(err.Error(), "more than one policy") {
		t.Errorf("two policies: err = %v", err)
	}
}

func TestParseTriggerTrimsContainerSlash(t *testing.T) {
	req, err := parseTrigger(url.Values{"container": {"/web", " db ", "/"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(req.Containers, ",") != "web,db" {
		t.Errorf("containers = %q, want web and db", req.Containers)
	}
}

func TestParseTriggerLimits(t *testing.T) {
	values := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return out
	}
	cases := []struct {
		name   string
		q      url.Values
		images int
		err    string
	}{
		{"images at the limit", url.Values{"image": {strings.Join(values("img", maxTriggerTargets), ",")}}, maxTriggerTargets, ""},
		{"images past the limit", url.Values{"image": {strings.Join(values("img", maxTriggerTargets+1), ",")}}, 0, "more than 256"},
		{"containers and images together", url.Values{"container": values("c", 200), "image": values("img", 57)}, 0, "more than 256"},
		{"containers past the limit", url.Values{"container": values("c", maxTriggerTargets+1)}, 0, "more than 256"},
		{"duplicates", url.Values{"image": {strings.Repeat("nginx,", 100000)}}, 1, ""},
		{"stops at the limit", url.Values{"image": {strings.Join(values("img", maxTriggerTargets+1), ",") + ",bad\x01value"}}, 0, "more than 256"},
		{"invalid value", url.Values{"image": {"ok,bad\x01value"}}, 0, "control characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := parseTrigger(c.q)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Images) != c.images {
				t.Errorf("images = %d, want %d", len(req.Images), c.images)
			}
		})
	}
}

func TestWatchtowerMetricsRoute(t *testing.T) {
	wt := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("watchtower_containers_scanned 3\n"))
	})
	s := New(config.HTTPConfig{Token: "secret"}, metricsStub(), discard(), Version{}, nil, API{WatchtowerMetrics: wt, WatchtowerCompat: true})
	if rec := do(s, http.MethodGet, "/v1/metrics", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/metrics without token = %d, want 401", rec.Code)
	}
	rec := do(s, http.MethodGet, "/v1/metrics", "Bearer secret")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "watchtower_containers_scanned 3") {
		t.Errorf("/v1/metrics = %d %q", rec.Code, rec.Body.String())
	}
	none := New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{WatchtowerCompat: true})
	if rec := do(none, http.MethodGet, "/v1/metrics", ""); rec.Code != http.StatusNotFound {
		t.Errorf("/v1/metrics without a handler = %d, want 404", rec.Code)
	}
}

func TestMatchesImage(t *testing.T) {
	cases := []struct {
		want  []string
		image string
		match bool
	}{
		{nil, "anything:1", true},
		{[]string{"nginx"}, "nginx", true},
		{[]string{"nginx"}, "nginx:1.27", true},
		{[]string{"nginx"}, "docker.io/library/nginx:1.27", true},
		{[]string{"docker.io/nginx"}, "nginx:latest", true},
		{[]string{"library/nginx"}, "index.docker.io/library/nginx", true},
		{[]string{"nginx"}, "nginx@sha256:abc", true},
		{[]string{"nginx"}, "nginxinc/nginx-unprivileged", false},
		{[]string{"nginx"}, "ghcr.io/library/nginx", false},
		{[]string{"nginx:1.27"}, "nginx:1.27", true},
		{[]string{"nginx:1.27"}, "nginx:1.26", false},
		{[]string{"nginx:latest"}, "nginx", true},
		{[]string{"nginx@sha256:abc"}, "nginx@sha256:abc", true},
		{[]string{"nginx@sha256:abc"}, "nginx:latest", false},
		{[]string{"ghcr.io/acme/api"}, "ghcr.io/acme/api:2.0", true},
		{[]string{"GHCR.io/acme/api"}, "ghcr.io/acme/api", true},
		{[]string{"localhost:5000/app"}, "localhost:5000/app:dev", true},
		{[]string{"localhost:5000/app"}, "localhost:5001/app:dev", false},
		{[]string{"redis", "postgres"}, "postgres:16", true},
		{[]string{"redis", "postgres"}, "mysql:8", false},
	}
	for _, c := range cases {
		if got := (TriggerRequest{Images: c.want}).MatchesImage(c.image); got != c.match {
			t.Errorf("Images %q MatchesImage(%q) = %v, want %v", c.want, c.image, got, c.match)
		}
	}
}

func TestContainersRoute(t *testing.T) {
	type row struct {
		Host, Container string
		Pending         bool
	}
	all := []row{{"local", "web", true}, {"local", "db", false}, {"edge", "web", false}}
	var got ContainersQuery
	api := API{Containers: func(q ContainersQuery) any {
		got = q
		out := []row{}
		for _, r := range all {
			if q.Matches(r.Host, r.Container, r.Pending) {
				out = append(out, r)
			}
		}
		return out
	}}
	s := New(config.HTTPConfig{Token: "secret", ReadToken: "reader"}, metricsStub(), discard(), Version{}, nil, api)

	decode := func(rec *httptest.ResponseRecorder) []row {
		t.Helper()
		var doc struct {
			Containers []row `json:"containers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		return doc.Containers
	}
	cases := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?pending=true", 1},
		{"?pending=false", 3},
		{"?host=local", 2},
		{"?container=/web", 2},
		{"?container=web&host=edge", 1},
		{"?container=web&container=db&host=local", 2},
		{"?host=nowhere", 0},
	}
	for _, c := range cases {
		rec := do(s, http.MethodGet, "/internal-api/v1/containers"+c.query, "Bearer reader")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %q", c.query, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rows := decode(rec); len(rows) != c.want {
			t.Errorf("%s: %d containers %+v, want %d (query %+v)", c.query, len(rows), rows, c.want, got)
		}
	}
	if rec := do(s, http.MethodGet, "/internal-api/v1/containers?pending=maybe", "Bearer reader"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid pending = %d, want 400", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/internal-api/v1/containers", "Bearer secret"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	tooMany := "?container=" + strings.Repeat("x&container=", maxTriggerTargets) + "x"
	if rec := do(s, http.MethodGet, "/internal-api/v1/containers"+tooMany, "Bearer reader"); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized query = %d, want 400", rec.Code)
	}

	s = New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{})
	if rec := do(s, http.MethodGet, "/internal-api/v1/containers", ""); rec.Code != http.StatusNotFound {
		t.Errorf("without Containers = %d, want 404", rec.Code)
	}
}

func TestContainersQueryMatches(t *testing.T) {
	q := ContainersQuery{}
	if !q.Matches("h", "c", false) {
		t.Error("an empty query must match everything")
	}
	q = ContainersQuery{Hosts: []string{"a"}, Containers: []string{"web"}, PendingOnly: true}
	for _, c := range []struct {
		host, container string
		pending, want   bool
	}{
		{"a", "web", true, true},
		{"a", "web", false, false},
		{"b", "web", true, false},
		{"a", "db", true, false},
	} {
		if got := q.Matches(c.host, c.container, c.pending); got != c.want {
			t.Errorf("Matches(%q, %q, %v) = %v, want %v", c.host, c.container, c.pending, got, c.want)
		}
	}
}

var goldens = map[string]string{
	"summary.json": `{
  "monitored": 12,
  "pending_updates": 3,
  "gated": 2,
  "updated_24h": 4,
  "failed_24h": 1,
  "dry_run": true,
  "next_run_at": "2026-10-17T04:00:00Z",
  "version": "v1.2.3",
  "instance": "edge-1"
}
`,
	"summary-empty.json": `{
  "monitored": 0,
  "pending_updates": 0,
  "gated": 0,
  "updated_24h": 0,
  "failed_24h": 0,
  "dry_run": false,
  "next_run_at": null,
  "version": "dev",
  "instance": "host"
}
`,
}

func goldenJSON(t *testing.T, name string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(body), "", "  "); err != nil {
		t.Fatalf("%s: body is not JSON: %v\n%s", name, err, body)
	}
	buf.WriteByte('\n')
	if want := goldens[name]; buf.String() != want {
		t.Errorf("%s changed, a machine contract:\ngot:\n%s\nwant:\n%s", name, buf.Bytes(), want)
	}
}

func TestSummaryRoute(t *testing.T) {
	next := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	full := Summary{
		Monitored: 12, PendingUpdates: 3, Gated: 2, Updated24h: 4, Failed24h: 1,
		DryRun: true, NextRunAt: &next, Version: "v1.2.3", Instance: "edge-1",
	}
	sum := full
	api := API{Summary: func() Summary { return sum }}
	s := New(config.HTTPConfig{Token: "secret", ReadToken: "reader"}, metricsStub(), discard(), Version{}, nil, api)

	rec := do(s, http.MethodGet, "/internal-api/v1/summary", "Bearer reader")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	goldenJSON(t, "summary.json", rec.Body.Bytes())

	sum = Summary{Version: "dev", Instance: "host"}
	rec = do(s, http.MethodGet, "/internal-api/v1/summary", "Bearer secret")
	goldenJSON(t, "summary-empty.json", rec.Body.Bytes())

	if rec := do(s, http.MethodGet, "/internal-api/v1/summary", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", rec.Code)
	}
	if rec := do(s, http.MethodHead, "/internal-api/v1/summary", "Bearer reader"); rec.Code != http.StatusOK {
		t.Errorf("HEAD = %d, want 200", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/internal-api/v1/summary", "Bearer secret"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d (Allow %q), want 405", rec.Code, rec.Header().Get("Allow"))
	}
	s = New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{})
	if rec := do(s, http.MethodGet, "/internal-api/v1/summary", ""); rec.Code != http.StatusNotFound {
		t.Errorf("without Summary = %d, want 404", rec.Code)
	}
}

func TestHandlerServesTheRoutes(t *testing.T) {
	s := New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal-api/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("Handler health = %d %q", rec.Code, rec.Body.String())
	}
}

func TestCheckAPI(t *testing.T) {
	var got []TriggerRequest
	api := API{Trigger: func(r TriggerRequest) string { got = append(got, r); return "queued" }}
	on := config.HTTPConfig{Token: "secret", ReadToken: "reader", UpdateAPI: true}
	s := New(on, metricsStub(), discard(), Version{}, nil, api)

	rec := do(s, http.MethodPost, "/internal-api/v1/check?container=web&container=+api+&policy=db&image=nginx,redis:7&image=nginx", "Bearer secret")
	if rec.Code != http.StatusAccepted || rec.Header().Get("Content-Type") != "application/json" || strings.TrimSpace(rec.Body.String()) != `{"status":"queued"}` {
		t.Fatalf("check = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("Trigger called %d times", len(got))
	}
	want := TriggerRequest{Containers: []string{"web", "api"}, Policy: "db", Images: []string{"nginx", "redis:7"}, Endpoint: EndpointInternal, DryRun: true}
	if fmt.Sprintf("%+v", got[0]) != fmt.Sprintf("%+v", want) {
		t.Errorf("check request = %+v, want %+v", got[0], want)
	}
	if rec := do(s, http.MethodPost, "/internal-api/v1/update?container=web", "Bearer secret"); rec.Code != http.StatusAccepted || got[1].DryRun {
		t.Errorf("update = %d, request %+v: an update is never a dry run", rec.Code, got[1])
	}

	got = nil
	for _, c := range []struct {
		name, method, target, auth string
		code                       int
	}{
		{"no credentials", http.MethodPost, "/internal-api/v1/check", "", http.StatusUnauthorized},
		{"wrong token", http.MethodPost, "/internal-api/v1/check", "Bearer nope", http.StatusUnauthorized},
		{"read token", http.MethodPost, "/internal-api/v1/check", "Bearer reader", http.StatusForbidden},
		{"GET", http.MethodGet, "/internal-api/v1/check", "Bearer secret", http.StatusMethodNotAllowed},
		{"image with a space", http.MethodPost, "/internal-api/v1/check?image=bad%20name", "Bearer secret", http.StatusBadRequest},
		{"blank container is no target", http.MethodPost, "/internal-api/v1/check?container=", "Bearer secret", http.StatusAccepted},
	} {
		rec := do(s, c.method, c.target, c.auth)
		if rec.Code != c.code {
			t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.target, rec.Code, c.code)
		}
		if c.code == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s: Allow = %q, want POST", c.name, rec.Header().Get("Allow"))
		}
	}
	var many strings.Builder
	for i := range maxTriggerTargets + 1 {
		fmt.Fprintf(&many, "container=c%d&", i)
	}
	if rec := do(s, http.MethodPost, "/internal-api/v1/check?"+many.String(), "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("too many containers = %d, want 400", rec.Code)
	}
	if len(got) != 1 || len(got[0].Containers) != 0 || !got[0].DryRun {
		t.Errorf("only the accepted requests may reach Trigger: %+v", got)
	}
}

func TestCheckAPIFollowsTheUpdateAPISwitch(t *testing.T) {
	calls := 0
	api := API{Trigger: func(TriggerRequest) string { calls++; return "queued" }}
	for _, c := range []struct {
		name string
		cfg  config.HTTPConfig
		auth string
		code int
	}{
		{"update_api off", config.HTTPConfig{Token: "secret"}, "Bearer secret", http.StatusNotFound},
		{"update_api off, no credentials", config.HTTPConfig{Token: "secret"}, "", http.StatusUnauthorized},
		{"no token", config.HTTPConfig{UpdateAPI: true}, "", http.StatusNotFound},
		{"read token only", config.HTTPConfig{ReadToken: "reader", UpdateAPI: true}, "Bearer reader", http.StatusNotFound},
		{"nothing configured", config.HTTPConfig{}, "", http.StatusNotFound},
	} {
		s := New(c.cfg, metricsStub(), discard(), Version{}, nil, api)
		for _, path := range []string{"/internal-api/v1/update", "/internal-api/v1/check"} {
			if rec := do(s, http.MethodPost, path, c.auth); rec.Code != c.code {
				t.Errorf("%s: %s = %d, want %d", c.name, path, rec.Code, c.code)
			}
		}
	}
	s := New(config.HTTPConfig{Token: "secret", UpdateAPI: true}, metricsStub(), discard(), Version{}, nil, API{})
	if rec := do(s, http.MethodPost, "/internal-api/v1/check", "Bearer secret"); rec.Code != http.StatusNotFound {
		t.Errorf("check without a Trigger = %d, want 404", rec.Code)
	}
	if calls != 0 {
		t.Errorf("Trigger called %d times", calls)
	}
}

func TestBusyRoute(t *testing.T) {
	var state Busy
	api := API{Busy: func() Busy { return state }}
	s := New(config.HTTPConfig{Token: "secret", ReadToken: "reader"}, metricsStub(), discard(), Version{}, nil, api)

	body := func(auth string) string {
		t.Helper()
		rec := do(s, http.MethodGet, "/internal-api/busy", auth)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("busy = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got := body("Bearer reader"); got != `{"busy":false}` {
		t.Errorf("idle = %s", got)
	}
	state = Busy{RunID: "stale", StartedAt: time.Now()}
	if got := body("Bearer secret"); got != `{"busy":false}` {
		t.Errorf("idle with leftover details = %s, want them dropped", got)
	}
	state = Busy{Busy: true, RunID: "f4e1d70ff4d3862c", StartedAt: time.Date(2026, 10, 30, 14, 49, 9, 155991000, time.FixedZone("CET", 3600))}
	if got := body("Bearer reader"); got != `{"busy":true,"run_id":"f4e1d70ff4d3862c","started_at":"2026-10-30T13:49:09Z"}` {
		t.Errorf("busy = %s", got)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body("Bearer reader")), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, doc["started_at"].(string)); err != nil {
		t.Errorf("started_at is not RFC 3339: %v", err)
	}

	if rec := do(s, http.MethodGet, "/internal-api/busy", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/internal-api/busy", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
	if rec := do(s, http.MethodHead, "/internal-api/busy", "Bearer reader"); rec.Code != http.StatusOK {
		t.Errorf("HEAD = %d, want 200", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/internal-api/busy", "Bearer secret"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d (Allow %q), want 405", rec.Code, rec.Header().Get("Allow"))
	}

	open := New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, API{})
	rec := do(open, http.MethodGet, "/internal-api/busy", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"busy":false}` {
		t.Errorf("without a token configured or a Busy source = %d %s", rec.Code, rec.Body.String())
	}
}

func TestEveryInternalRouteNeedsTheTokenOnceOneIsSet(t *testing.T) {
	api := API{
		Trigger:    func(TriggerRequest) string { return "queued" },
		Status:     func() any { return map[string]string{} },
		Containers: func(ContainersQuery) any { return []string{} },
		Summary:    func() Summary { return Summary{} },
		Busy:       func() Busy { return Busy{} },
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/internal-api/health"},
		{http.MethodGet, "/internal-api/ready"},
		{http.MethodGet, "/internal-api/busy"},
		{http.MethodGet, "/internal-api/version"},
		{http.MethodGet, "/internal-api/metrics"},
		{http.MethodGet, "/internal-api/v1/status"},
		{http.MethodGet, "/internal-api/v1/containers"},
		{http.MethodGet, "/internal-api/v1/summary"},
		{http.MethodPost, "/internal-api/v1/update"},
		{http.MethodPost, "/internal-api/v1/check"},
	}
	for _, cfg := range []config.HTTPConfig{
		{Token: "secret", UpdateAPI: true},
		{ReadToken: "reader"},
		{Token: "secret", ReadToken: "reader", UpdateAPI: true},
	} {
		s := New(cfg, metricsStub(), discard(), Version{}, nil, api)
		for _, r := range routes {
			rec := do(s, r.method, r.path, "")
			if r.method == http.MethodPost && cfg.Token == "" {
				if rec.Code != http.StatusNotFound {
					t.Errorf("token %q read_token %q: %s %s = %d, want 404 without a write token", cfg.Token, cfg.ReadToken, r.method, r.path, rec.Code)
				}
				continue
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("token %q read_token %q: %s %s without credentials = %d, want 401", cfg.Token, cfg.ReadToken, r.method, r.path, rec.Code)
			}
		}
	}
	s := New(config.HTTPConfig{}, metricsStub(), discard(), Version{}, nil, api)
	for _, r := range routes {
		rec := do(s, r.method, r.path, "")
		want := http.StatusOK
		if r.method == http.MethodPost {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Errorf("no token: %s %s = %d, want %d", r.method, r.path, rec.Code, want)
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func TestDisabledServerOpensNoPort(t *testing.T) {
	off := false
	addr := freePort(t)
	s := New(config.HTTPConfig{Enabled: &off, Addr: addr}, metricsStub(), discard(), Version{}, nil, API{})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run of a disabled server must return at once instead of serving")
	}
	if listening(addr) {
		t.Fatalf("a disabled server listens on %s", addr)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}

	on := New(config.HTTPConfig{Addr: addr}, metricsStub(), discard(), Version{}, nil, API{})
	go func() { done <- on.Run(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for !listening(addr) {
		if time.Now().After(deadline) {
			t.Fatalf("an enabled server does not listen on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := on.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Run after Shutdown = %v", err)
	}
}
