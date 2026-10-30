package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

func TestStatusOK(t *testing.T) {
	tests := []struct {
		name  string
		code  int
		allow []int
		want  bool
	}{
		{name: "default 200 ok", code: 200, want: true},
		{name: "default 204 ok", code: 204, want: true},
		{name: "default 299 ok", code: 299, want: true},
		{name: "default 199 not ok", code: 199, want: false},
		{name: "default 301 not ok", code: 301, want: false},
		{name: "default 403 not ok", code: 403, want: false},
		{name: "explicit list match", code: 418, allow: []int{418, 451}, want: true},
		{name: "explicit list other match", code: 451, allow: []int{418, 451}, want: true},
		{name: "explicit list excludes 200", code: 200, allow: []int{418}, want: false},
		{name: "explicit list miss", code: 500, allow: []int{418}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusOK(tc.code, tc.allow); got != tc.want {
				t.Fatalf("statusOK(%d, %v) = %v, want %v", tc.code, tc.allow, got, tc.want)
			}
		})
	}
}

func TestFilterLabels(t *testing.T) {
	in := map[string]string{
		"dockpaum.policy":            "critical",
		"com.docker.compose.project": "alpha",
		"team":                       "platform",
		"secret":                     "hunter2",
	}

	tests := []struct {
		name      string
		in        map[string]string
		allowlist []string
		want      map[string]string
	}{
		{
			name: "nil input stays nil",
			in:   nil,
			want: nil,
		},
		{
			name:      "empty allowlist keeps only prefixed labels",
			in:        in,
			allowlist: nil,
			want: map[string]string{
				"dockpaum.policy":            "critical",
				"com.docker.compose.project": "alpha",
			},
		},
		{
			name:      "exact allowlist match passes",
			in:        in,
			allowlist: []string{"team"},
			want: map[string]string{
				"dockpaum.policy":            "critical",
				"com.docker.compose.project": "alpha",
				"team":                       "platform",
			},
		},
		{
			name:      "wildcard passes everything",
			in:        in,
			allowlist: []string{"*"},
			want:      in,
		},
		{
			name:      "non-listed non-prefixed label dropped",
			in:        in,
			allowlist: []string{"team"},
			want: map[string]string{
				"dockpaum.policy":            "critical",
				"com.docker.compose.project": "alpha",
				"team":                       "platform",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterLabels(tc.in, tc.allowlist)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("filterLabels = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("filterLabels = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("filterLabels[%q] = %q, want %q", k, got[k], v)
				}
			}
			if _, leaked := got["secret"]; leaked && tc.allowlist != nil && tc.allowlist[0] != "*" {
				t.Error("filterLabels leaked non-allowlisted label \"secret\"")
			}
		})
	}
}

func TestApprovalRequestID(t *testing.T) {
	id := approvalRequestID("prod-1", "web-1", "sha256:new")
	if id != approvalRequestID("prod-1", "web-1", "sha256:new") {
		t.Fatal("the request ID must be stable across runs")
	}
	for _, other := range []string{
		approvalRequestID("prod-2", "web-1", "sha256:new"),
		approvalRequestID("prod-1", "web-2", "sha256:new"),
		approvalRequestID("prod-1", "web-1", "sha256:newer"),
		approvalRequestID("prod-1web-1", "", "sha256:new"),
	} {
		if other == id {
			t.Fatalf("different updates share the request ID %s", id)
		}
	}
}

func TestAppendApprovalQuery(t *testing.T) {
	body := approvalRequest{
		RequestID:   "rid",
		Host:        "prod-1",
		ContainerID: "abc123",
		Container:   "web-1",
		Image:       "nginx:1.25",
		OldDigest:   "sha256:old",
		NewDigest:   "sha256:new",
	}

	t.Run("params appended and existing query preserved", func(t *testing.T) {
		got, err := appendApprovalQuery("https://approver.example/hook?token=abc&x=1", body)
		if err != nil {
			t.Fatalf("appendApprovalQuery: %v", err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse result %q: %v", got, err)
		}
		q := u.Query()
		want := map[string]string{
			"request_id":   "rid",
			"host":         "prod-1",
			"container":    "web-1",
			"container_id": "abc123",
			"image":        "nginx:1.25",
			"old_digest":   "sha256:old",
			"new_digest":   "sha256:new",
			"token":        "abc",
			"x":            "1",
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Errorf("query[%q] = %q, want %q", k, q.Get(k), v)
			}
		}
		if u.Scheme != "https" || u.Host != "approver.example" || u.Path != "/hook" {
			t.Errorf("base URL mangled: %q", got)
		}
	})

	t.Run("invalid url errors", func(t *testing.T) {
		if _, err := appendApprovalQuery("http://[::1", body); err == nil {
			t.Fatal("expected error for invalid url, got nil")
		}
	})
}

func askOnce(cfg *config.ApprovalConfig, host string) (bool, string, error) {
	g := newApprovalGate(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	ans, err := g.ask(context.Background(), host, approvalFC(), approvalSubject{OldDigest: "sha256:old", NewDigest: "sha256:new"}, approvalPhaseInitial)
	return ans.approved, ans.reason, err
}

func approvalFC() *docker.FullContainer {
	return &docker.FullContainer{
		ID:      "abc123",
		Name:    "web-1",
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
		Config: &container.Config{
			Labels: map[string]string{
				"dockpaum.policy": "critical",
				"secret":          "hunter2",
			},
		},
	}
}

func TestAskApprovalPostSendsJSONBody(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotAuthHeader  string
		gotBody        approvalRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAuthHeader = r.Header.Get("X-Auth")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.ApprovalConfig{
		URL:     srv.URL,
		Headers: map[string]string{"X-Auth": "tok"},
	}
	approved, reason, err := askOnce(cfg, "prod-1")
	if err != nil {
		t.Fatalf("askApproval: %v", err)
	}
	if !approved {
		t.Fatalf("approved = false, want true (reason %q)", reason)
	}
	if !strings.Contains(reason, "approved") {
		t.Errorf("reason = %q, want it to mention approval", reason)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST (default)", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAuthHeader != "tok" {
		t.Errorf("X-Auth header = %q, want %q", gotAuthHeader, "tok")
	}
	if gotBody.Host != "prod-1" {
		t.Errorf("body.host = %q, want %q", gotBody.Host, "prod-1")
	}
	if gotBody.ContainerID != "abc123" {
		t.Errorf("body.container_id = %q, want %q", gotBody.ContainerID, "abc123")
	}
	if gotBody.Container != "web-1" {
		t.Errorf("body.container = %q, want %q", gotBody.Container, "web-1")
	}
	if gotBody.Image != "nginx:1.25" {
		t.Errorf("body.image = %q, want %q", gotBody.Image, "nginx:1.25")
	}
	if gotBody.OldDigest != "sha256:old" {
		t.Errorf("body.old_digest = %q, want %q", gotBody.OldDigest, "sha256:old")
	}
	if gotBody.NewDigest != "sha256:new" {
		t.Errorf("body.new_digest = %q, want %q", gotBody.NewDigest, "sha256:new")
	}
	if gotBody.At == "" {
		t.Error("body.at is empty, want RFC3339 timestamp")
	}
	if want := approvalRequestID("prod-1", "web-1", "sha256:new"); gotBody.RequestID != want || len(want) != 32 {
		t.Errorf("body.request_id = %q, want %q", gotBody.RequestID, want)
	}
	if gotBody.Labels["dockpaum.policy"] != "critical" {
		t.Errorf("body.labels missing dockpaum.policy, got %v", gotBody.Labels)
	}
	if _, leaked := gotBody.Labels["secret"]; leaked {
		t.Error("body.labels leaked non-allowlisted label \"secret\"")
	}
}

func TestAskApprovalGetSendsQueryAndNoBody(t *testing.T) {
	var (
		gotMethod string
		gotQuery  url.Values
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.Query()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := &config.ApprovalConfig{URL: srv.URL + "/approve?token=abc", Method: "get"}
	approved, reason, err := askOnce(cfg, "prod-1")
	if err != nil {
		t.Fatalf("askApproval: %v", err)
	}
	if !approved {
		t.Fatalf("approved = false, want true (reason %q)", reason)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Errorf("GET sent a body of %d bytes, want none", len(gotBody))
	}
	want := map[string]string{
		"host":         "prod-1",
		"container":    "web-1",
		"container_id": "abc123",
		"image":        "nginx:1.25",
		"old_digest":   "sha256:old",
		"new_digest":   "sha256:new",
		"token":        "abc",
	}
	for k, v := range want {
		if gotQuery.Get(k) != v {
			t.Errorf("query[%q] = %q, want %q", k, gotQuery.Get(k), v)
		}
	}
}

func TestAskApprovalDeniedOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()

	cfg := &config.ApprovalConfig{URL: srv.URL}
	approved, reason, err := askOnce(cfg, "prod-1")
	if err != nil {
		t.Fatalf("askApproval: %v", err)
	}
	if approved {
		t.Fatal("approved = true, want false for HTTP 403")
	}
	if !strings.Contains(reason, "denied") || !strings.Contains(reason, "403") {
		t.Errorf("reason = %q, want denial with status code", reason)
	}
}

func TestAskApprovalOkStatusesList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	cfg := &config.ApprovalConfig{URL: srv.URL, OkStatuses: []int{418}}
	approved, reason, err := askOnce(cfg, "prod-1")
	if err != nil {
		t.Fatalf("askApproval: %v", err)
	}
	if !approved {
		t.Fatalf("approved = false, want true for HTTP 418 with ok_statuses [418] (reason %q)", reason)
	}
}

func TestAskApprovalTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	t.Run("fail_open approves", func(t *testing.T) {
		cfg := &config.ApprovalConfig{URL: deadURL, FailOpen: true}
		approved, reason, err := askOnce(cfg, "prod-1")
		if err != nil {
			t.Fatalf("askApproval: %v", err)
		}
		if !approved {
			t.Fatalf("approved = false, want true with fail_open (reason %q)", reason)
		}
		if !strings.Contains(reason, "fail-open") {
			t.Errorf("reason = %q, want fail-open mention", reason)
		}
	})

	t.Run("fail_closed default denies", func(t *testing.T) {
		cfg := &config.ApprovalConfig{URL: deadURL}
		approved, reason, err := askOnce(cfg, "prod-1")
		if err != nil {
			t.Fatalf("askApproval: %v", err)
		}
		if approved {
			t.Fatal("approved = true, want false on transport error without fail_open")
		}
		if !strings.Contains(reason, "transport error") {
			t.Errorf("reason = %q, want transport error mention", reason)
		}
	})
}

func TestAskApprovalUnconfigured(t *testing.T) {
	for _, cfg := range []*config.ApprovalConfig{nil, {URL: ""}} {
		approved, reason, err := askOnce(cfg, "h")
		if err != nil {
			t.Fatalf("askApproval(%v): %v", cfg, err)
		}
		if !approved || reason != "" {
			t.Fatalf("askApproval(%v) = (%v, %q), want (true, \"\")", cfg, approved, reason)
		}
	}
}

func TestApprovalReasonOmitsSecretURLParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	cfg := &config.ApprovalConfig{URL: srv.URL + "/hooks/SECRET-PATH?token=SECRET-QUERY"}
	_, reason, err := askOnce(cfg, "h")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reason, "SECRET") || !strings.Contains(reason, srv.URL) {
		t.Fatalf("reason = %q, want the origin only", reason)
	}
}

func TestApprovalRefusesCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	cfg := &config.ApprovalConfig{URL: origin.URL, Headers: map[string]string{"Authorization": "Bearer approve-secret"}}
	approved, _, err := askOnce(cfg, "h")
	if err != nil {
		t.Fatal(err)
	}
	if approved || leaked.Load() {
		t.Fatalf("approved=%v leaked=%v: a cross-origin redirect must deny without sending the token", approved, leaked.Load())
	}
}

func TestReadApprovalAnswer(t *testing.T) {
	yes, no := true, false
	long := strings.Repeat("é", 300)
	tests := []struct {
		name       string
		body       string
		approved   *bool
		reason     string
		retryAfter time.Duration
	}{
		{name: "full answer", body: `{"approved":false,"reason":"workload-busy","retry_after_seconds":600}`,
			approved: &no, reason: "workload-busy", retryAfter: 10 * time.Minute},
		{name: "approval", body: ` {"approved": true} `, approved: &yes},
		{name: "empty body", body: ``},
		{name: "not json", body: `denied, try later`},
		{name: "json array", body: `[1,2]`},
		{name: "null approved is absent", body: `{"approved":null,"reason":"x"}`, reason: "x"},
		{name: "a string is no approval", body: `{"approved":"true"}`, approved: &no},
		{name: "malformed field leaves the others", body: `{"approved":false,"reason":7,"retry_after_seconds":"soon"}`, approved: &no},
		{name: "fractional seconds", body: `{"retry_after_seconds":90.9}`, retryAfter: 90 * time.Second},
		{name: "negative seconds ignored", body: `{"retry_after_seconds":-5}`},
		{name: "retry capped", body: `{"retry_after_seconds":1e12}`, retryAfter: maxApprovalRetry},
		{name: "control characters removed", body: "{\"reason\":\"line one\\nline\\u0000two\\u202e\\tend\"}", reason: "line one line two end"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readApprovalAnswer(strings.NewReader(tc.body))
			if (got.approved == nil) != (tc.approved == nil) || (got.approved != nil && *got.approved != *tc.approved) {
				t.Errorf("approved = %v, want %v", got.approved, tc.approved)
			}
			if got.reason != tc.reason || got.retryAfter != tc.retryAfter {
				t.Errorf("reason %q retry %s, want %q %s", got.reason, got.retryAfter, tc.reason, tc.retryAfter)
			}
		})
	}
	t.Run("long reason truncated on a rune boundary", func(t *testing.T) {
		got := readApprovalAnswer(strings.NewReader(`{"reason":"` + long + `"}`)).reason
		if len(got) > maxApprovalReason || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
			t.Fatalf("reason of %d bytes: %q", len(got), got)
		}
	})
	t.Run("oversized body ignored", func(t *testing.T) {
		big := `{"approved":false,"reason":"` + strings.Repeat("x", maxApprovalAnswer) + `"}`
		if got := readApprovalAnswer(strings.NewReader(big)); got.approved != nil || got.reason != "" {
			t.Fatalf("an answer over %d bytes must be ignored, got %+v", maxApprovalAnswer, got)
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		header   string
		fromBody time.Duration
		want     time.Duration
	}{
		{"none", "", 0, 0},
		{"seconds", "120", 0, 2 * time.Minute},
		{"http date", now.Add(time.Hour).Format(http.TimeFormat), 0, time.Hour},
		{"date in the past", now.Add(-time.Hour).Format(http.TimeFormat), 0, 0},
		{"the later one wins", "60", 5 * time.Minute, 5 * time.Minute},
		{"header later than body", "600", time.Minute, 10 * time.Minute},
		{"garbage", "soon", 0, 0},
		{"negative", "-30", 0, 0},
		{"capped", "999999999999", 0, maxApprovalRetry},
	}
	for _, tc := range tests {
		if got := retryAfter(tc.header, tc.fromBody, now); got != tc.want {
			t.Errorf("%s: retryAfter = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestApprovalAnswerHandling(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		header       http.Header
		body         string
		okStatuses   []int
		wantApproved bool
		wantResult   string
		wantReason   []string
		wantRetry    time.Duration
	}{
		{name: "204 approves", status: 204, wantApproved: true, wantResult: approvalApproved},
		{name: "409 with JSON reason and retry", status: 409, body: `{"approved":false,"reason":"outside the maintenance window","retry_after_seconds":3600}`,
			wantResult: approvalDenied, wantReason: []string{"denied by", "HTTP 409", ": outside the maintenance window"}, wantRetry: time.Hour},
		{name: "Retry-After header", status: 503, header: http.Header{"Retry-After": {"120"}},
			wantResult: approvalDenied, wantReason: []string{"HTTP 503"}, wantRetry: 2 * time.Minute},
		{name: "2xx with approved false denies", status: 200, body: `{"approved":false,"reason":"paused"}`,
			wantResult: approvalDenied, wantReason: []string{"HTTP 200, approved: false", "paused"}},
		{name: "status decides over approved true", status: 403, body: `{"approved":true}`,
			wantResult: approvalDenied, wantReason: []string{"HTTP 403"}},
		{name: "approval ignores Retry-After", status: 200, header: http.Header{"Retry-After": {"600"}}, body: `{"approved":true,"reason":"ok"}`,
			wantApproved: true, wantResult: approvalApproved, wantReason: []string{"approved by", ": ok"}},
		{name: "ok_statuses", status: 409, okStatuses: []int{409}, wantApproved: true, wantResult: approvalApproved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				if tc.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
			g := newApprovalGate(&config.ApprovalConfig{URL: srv.URL, OkStatuses: tc.okStatuses}, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now })
			ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
			if err != nil {
				t.Fatal(err)
			}
			if ans.approved != tc.wantApproved || ans.result != tc.wantResult || ans.status.Approved != tc.wantApproved || ans.status.HTTPStatus != tc.status {
				t.Fatalf("answer %+v status %+v", ans, ans.status)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(ans.reason, want) {
					t.Errorf("reason %q lacks %q", ans.reason, want)
				}
			}
			var wantAt time.Time
			if tc.wantRetry > 0 {
				wantAt = now.Add(tc.wantRetry)
			}
			if !ans.status.RetryAt.Equal(wantAt) || ans.status.Phase != approvalPhaseInitial || ans.status.RequestID == "" {
				t.Errorf("status %+v, want retry at %s", ans.status, wantAt)
			}
		})
	}
}

func TestApprovalAnswerBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		chunk := []byte(strings.Repeat("x", 32<<10))
		for i := 0; i < 1<<12; i++ {
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
		}
	}))
	defer srv.Close()
	g := newApprovalGate(&config.ApprovalConfig{URL: srv.URL, Timeout: config.Duration(5 * time.Second)}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	start := time.Now()
	ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
	if err != nil {
		t.Fatal(err)
	}
	if ans.approved || ans.result != approvalDenied || time.Since(start) > 4*time.Second {
		t.Fatalf("answer %+v after %s", ans, time.Since(start))
	}
}

func TestApprovalRequestCarriesPhase(t *testing.T) {
	var got []approvalRequest
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body approvalRequest
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
			got = append(got, body)
		} else {
			queries = append(queries, r.URL.Query().Get("phase")+"/"+r.URL.Query().Get("request_id"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	for _, method := range []string{"POST", "GET"} {
		g := newApprovalGate(&config.ApprovalConfig{URL: srv.URL, Method: method}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
		for _, phase := range []string{approvalPhaseInitial, approvalPhaseFinal} {
			if _, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, phase); err != nil {
				t.Fatal(err)
			}
		}
	}
	id := approvalRequestID("h", "web-1", "sha256:new")
	if len(got) != 2 || got[0].Phase != "initial" || got[1].Phase != "final" || got[0].RequestID != id || got[1].RequestID != id {
		t.Fatalf("bodies %+v: the phase changes, the request ID stays", got)
	}
	if len(queries) != 2 || queries[0] != "initial/"+id || queries[1] != "final/"+id {
		t.Fatalf("queries %v", queries)
	}
}

func TestApproveOnFailure(t *testing.T) {
	type answer struct {
		status int
		header http.Header
		body   string
	}
	tests := []struct {
		name       string
		answer     answer
		okStatuses []int
		phase      string
		approved   bool
		result     string
		reason     string
	}{
		{name: "200 denies", answer: answer{status: 200}, result: approvalDenied,
			reason: "denied: %s answered HTTP 200 (approve_on: failure)"},
		{name: "204 denies", answer: answer{status: 204}, result: approvalDenied,
			reason: "denied: %s answered HTTP 204 (approve_on: failure)"},
		{name: "503 approves", answer: answer{status: 503}, approved: true, result: approvalApproved,
			reason: "approved: %s answered HTTP 503 (approve_on: failure)"},
		{name: "403 approves", answer: answer{status: 403}, approved: true, result: approvalApproved,
			reason: "approved: %s answered HTTP 403 (approve_on: failure)"},
		{name: "ok_statuses status denies", answer: answer{status: 418}, okStatuses: []int{418}, result: approvalDenied,
			reason: "denied: %s answered HTTP 418 (approve_on: failure)"},
		{name: "2xx outside ok_statuses approves", answer: answer{status: 200}, okStatuses: []int{418}, approved: true, result: approvalApproved,
			reason: "approved: %s answered HTTP 200 (approve_on: failure)"},
		{name: "explicit approved: false is ignored", answer: answer{status: 503, header: http.Header{"Content-Type": {"application/json"}},
			body: `{"approved":false,"reason":"incident open","retry_after_seconds":600}`}, approved: true, result: approvalApproved,
			reason: "approved: %s answered HTTP 503 (approve_on: failure): incident open"},
		{name: "explicit approved: true is ignored", answer: answer{status: 200, body: `{"approved":true}`}, result: approvalDenied,
			reason: "denied: %s answered HTTP 200 (approve_on: failure)"},
		{name: "Retry-After is ignored", answer: answer{status: 200, header: http.Header{"Retry-After": {"3600"}},
			body: `{"reason":"change freeze","retry_after_seconds":600}`}, result: approvalDenied,
			reason: "denied: %s answered HTTP 200 (approve_on: failure): change freeze"},
		{name: "final phase", answer: answer{status: 200}, phase: approvalPhaseFinal, result: approvalDenied,
			reason: "denied: %s answered HTTP 200 at the final check before the stop (approve_on: failure)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.answer.header {
					w.Header()[k] = v
				}
				w.WriteHeader(tc.answer.status)
				_, _ = io.WriteString(w, tc.answer.body)
			}))
			defer srv.Close()
			cfg := &config.ApprovalConfig{URL: srv.URL + "/status/SECRETPATH?token=SECRETQUERY", OkStatuses: tc.okStatuses,
				ApproveOn: config.ApproveOnFailure}
			phase := tc.phase
			if phase == "" {
				phase = approvalPhaseInitial
			}
			g := newApprovalGate(cfg, discard(), time.Now)
			ans, err := g.ask(context.Background(), "prod-1", approvalFC(), approvalSubject{OldDigest: "sha256:old", NewDigest: "sha256:new"}, phase)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(tc.reason, srv.URL); ans.reason != want {
				t.Fatalf("reason %q, want %q", ans.reason, want)
			}
			if ans.approved != tc.approved || ans.result != tc.result || ans.status.Approved != tc.approved ||
				ans.status.HTTPStatus != tc.answer.status || !ans.status.RetryAt.IsZero() {
				t.Fatalf("answer %+v, status %+v", ans, ans.status)
			}
		})
	}

	t.Run("an endpoint that cannot be reached approves, whatever fail_open says", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead := srv.URL
		srv.Close()
		for _, failOpen := range []bool{false, true} {
			cfg := &config.ApprovalConfig{URL: dead + "/status?token=SECRETQUERY", FailOpen: failOpen, ApproveOn: config.ApproveOnFailure}
			g := newApprovalGate(cfg, discard(), time.Now)
			ans, err := g.ask(context.Background(), "prod-1", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
			if err != nil {
				t.Fatal(err)
			}
			if !ans.approved || ans.result != approvalError || !ans.status.Approved ||
				!strings.HasPrefix(ans.reason, "approved: "+dead+" unreachable (approve_on: failure): ") ||
				strings.Contains(ans.reason, "SECRET") || strings.Contains(ans.reason, "/status") {
				t.Fatalf("fail_open %v: answer %+v", failOpen, ans)
			}
		}
	})

	t.Run("a request that cannot be signed never approves", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		cfg := &config.ApprovalConfig{URL: srv.URL, FailOpen: true, ApproveOn: config.ApproveOnFailure,
			Signing: &config.ApprovalSigning{KeyID: "k", SecretFile: filepath.Join(t.TempDir(), "absent")}}
		g := newApprovalGate(cfg, discard(), time.Now)
		ans, err := g.ask(context.Background(), "prod-1", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
		if err != nil {
			t.Fatal(err)
		}
		if ans.approved || ans.result != approvalError || calls.Load() != 0 || !strings.Contains(ans.reason, "not sent") {
			t.Fatalf("answer %+v, %d request(s) sent", ans, calls.Load())
		}
	})

	t.Run("approve_on: success is unchanged", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer srv.Close()
		for _, on := range []string{"", config.ApproveOnSuccess} {
			approved, reason, err := askOnce(&config.ApprovalConfig{URL: srv.URL, ApproveOn: on}, "prod-1")
			if err != nil || !approved || reason != "approved by "+srv.URL+" (HTTP 200)" {
				t.Fatalf("approve_on %q: approved %v, reason %q, err %v", on, approved, reason, err)
			}
		}
	})
}
