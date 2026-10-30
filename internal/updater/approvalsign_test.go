package updater

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

type approvalVector struct {
	Name            string            `json:"name"`
	Secret          string            `json:"secret"`
	KeyID           string            `json:"key_id"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	RequestURI      string            `json:"request_uri"`
	Host            string            `json:"host"`
	Timestamp       string            `json:"timestamp"`
	Nonce           string            `json:"nonce"`
	Body            string            `json:"body"`
	BodySHA256      string            `json:"body_sha256"`
	CanonicalString string            `json:"canonical_string"`
	Signature       string            `json:"signature"`
	Headers         map[string]string `json:"headers"`
}

func loadApprovalVectors(t *testing.T) []approvalVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "approval-v1-vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Scheme string `json:"scheme"`
		approvalVector
		Additional []approvalVector `json:"additional_vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Scheme != approvalScheme {
		t.Fatalf("vector scheme %q", doc.Scheme)
	}
	return append([]approvalVector{doc.approvalVector}, doc.Additional...)
}

func TestApprovalSignatureVectors(t *testing.T) {
	for _, v := range loadApprovalVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			req, err := http.NewRequest(v.Method, v.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := req.URL.RequestURI(); got != v.RequestURI {
				t.Fatalf("request URI %q, want %q", got, v.RequestURI)
			}
			if got := canonicalApprovalHost(req.URL.Scheme, req.Host); got != v.Host {
				t.Fatalf("host %q, want %q", got, v.Host)
			}
			canonical := approvalCanonical(v.Method, v.RequestURI, v.Host, v.Timestamp, v.Nonce, []byte(v.Body))
			if canonical != v.CanonicalString {
				t.Fatalf("canonical string\n%q\nwant\n%q", canonical, v.CanonicalString)
			}
			if !strings.HasSuffix(canonical, "\n"+v.BodySHA256) {
				t.Fatalf("body hash %s not covered", v.BodySHA256)
			}
			if got := approvalSignature([]byte(v.Secret), canonical); got != v.Signature || v.Headers[headerApprovalSignature] != "v1="+got {
				t.Fatalf("signature %s, want %s", got, v.Signature)
			}
		})
	}
}

func TestApprovalRequestsMatchTheVector(t *testing.T) {
	vectors := loadApprovalVectors(t)
	tests := []struct {
		vector  approvalVector
		baseURL string
		method  string
		phase   string
	}{
		{vectors[0], vectors[0].URL, http.MethodPost, approvalPhaseInitial},
		{vectors[1], "https://approver.example.com:8443/approve?token=abc", http.MethodGet, approvalPhaseFinal},
	}
	for _, tc := range tests {
		v := tc.vector
		t.Run(v.Name, func(t *testing.T) {
			ts, _ := strconv.ParseInt(v.Timestamp, 10, 64)
			g := newApprovalGate(&config.ApprovalConfig{
				URL: tc.baseURL, Method: tc.method,
				Signing: &config.ApprovalSigning{KeyID: v.KeyID, SecretFile: secretFile(t, v.Secret)},
			}, discard(), fixedClock(time.Unix(ts, 0)))
			g.signer.nonce = func() string { return v.Nonce }
			var got *http.Request
			var body []byte
			g.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				got = r
				if r.Body != nil {
					body, _ = io.ReadAll(r.Body)
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: http.Header{}}, nil
			})}
			ans, err := g.ask(context.Background(), "docker-1", vectorContainer(), vectorSubject(), tc.phase)
			if err != nil || !ans.approved {
				t.Fatalf("ask: %+v, %v", ans, err)
			}
			if got.URL.RequestURI() != v.RequestURI {
				t.Fatalf("request URI\n%s\nwant\n%s", got.URL.RequestURI(), v.RequestURI)
			}
			if string(body) != v.Body {
				t.Fatalf("body\n%s\nwant\n%s", body, v.Body)
			}
			for k, want := range v.Headers {
				if got.Header.Get(k) != want {
					t.Errorf("%s: %q, want %q", k, got.Header.Get(k), want)
				}
			}
		})
	}
}

func vectorContainer() *docker.FullContainer {
	return &docker.FullContainer{
		ID: "0123456789ab" + strings.Repeat("0", 52), Name: "web", Image: "registry.example.com/team/web:1.4",
		Config: &container.Config{Labels: map[string]string{
			"com.docker.compose.project": "shop", "dockpaum.policy": "default", "unrelated": "x",
		}},
	}
}

func vectorSubject() approvalSubject {
	return approvalSubject{
		OldDigest: "sha256:" + strings.Repeat("1", 64), NewDigest: "sha256:" + strings.Repeat("2", 64),
		OldRepoDigest: "registry.example.com/team/web@sha256:" + strings.Repeat("a", 64),
		NewRepoDigest: "registry.example.com/team/web@sha256:" + strings.Repeat("b", 64),
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func secretFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "approval-secret")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var nonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func verifyApproval(r *http.Request, body []byte, secret string, now time.Time) string {
	ts := r.Header.Get(headerApprovalTimestamp)
	sec, err := strconv.ParseInt(ts, 10, 64)
	switch {
	case err != nil:
		return "bad timestamp"
	case now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute:
		return "timestamp outside the window"
	case !nonceRE.MatchString(r.Header.Get(headerApprovalNonce)):
		return "bad nonce"
	}
	canonical := approvalCanonical(r.Method, r.RequestURI, canonicalApprovalHost("http", r.Host), ts, r.Header.Get(headerApprovalNonce), body)
	want := "v1=" + approvalSignature([]byte(secret), canonical)
	if !hmac.Equal([]byte(r.Header.Get(headerApprovalSignature)), []byte(want)) {
		return "bad signature"
	}
	return ""
}

func TestSignedApprovalRequestsVerify(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef-rotating"
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			var problem atomic.Value
			var mu sync.Mutex
			nonces := map[string]bool{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				body, _ := io.ReadAll(r.Body)
				p := verifyApproval(r, body, secret, time.Now())
				if nonces[r.Header.Get(headerApprovalNonce)] {
					p = "nonce reused"
				}
				nonces[r.Header.Get(headerApprovalNonce)] = true
				if r.Header.Get(headerApprovalKeyID) != "k2" {
					p = "key id " + r.Header.Get(headerApprovalKeyID)
				}
				problem.Store(p)
				if p != "" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			g := newApprovalGate(&config.ApprovalConfig{
				URL: srv.URL + "/hooks/approve?x=a%2Fb", Method: method,
				Signing: &config.ApprovalSigning{KeyID: "k2", SecretFile: secretFile(t, secret)},
			}, discard(), time.Now)
			for range 2 {
				ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
				if err != nil || !ans.approved {
					t.Fatalf("ask: %+v %v (approver: %v)", ans, err, problem.Load())
				}
			}
		})
	}
}

func TestApprovalSecretIsReadAgainWhenItChanges(t *testing.T) {
	secrets := []string{strings.Repeat("a", 32), strings.Repeat("b", 40)}
	var current atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if verifyApproval(r, body, current.Load().(string), time.Now()) != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	file := secretFile(t, secrets[0])
	g := newApprovalGate(&config.ApprovalConfig{URL: srv.URL, Signing: &config.ApprovalSigning{KeyID: "k", SecretFile: file}}, discard(), time.Now)
	ask := func() approvalAnswer {
		ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	current.Store(secrets[0])
	if !ask().approved {
		t.Fatal("first secret")
	}
	if err := os.WriteFile(file, []byte(secrets[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	current.Store(secrets[1])
	if ans := ask(); !ans.approved {
		t.Fatalf("rotated secret not used: %+v", ans)
	}
}

func TestUnsignableApprovalRequestIsNeverSent(t *testing.T) {
	tests := map[string]func(t *testing.T) string{
		"secret too short": func(t *testing.T) string { return secretFile(t, strings.Repeat("s", config.MinApprovalSecretLen-1)) },
		"secret missing":   func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		"secret empty":     func(t *testing.T) string { return secretFile(t, "  ") },
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			g := newApprovalGate(&config.ApprovalConfig{URL: srv.URL, FailOpen: true,
				Signing: &config.ApprovalSigning{KeyID: "k", SecretFile: file(t)}}, discard(), time.Now)
			ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseFinal)
			if err != nil {
				t.Fatal(err)
			}
			if ans.approved || ans.result != approvalError || calls.Load() != 0 {
				t.Fatalf("answer %+v, %d request(s) sent", ans, calls.Load())
			}
			if !strings.Contains(ans.reason, "not sent") || !strings.Contains(ans.reason, "final check") || strings.Contains(ans.reason, strings.Repeat("s", 8)) {
				t.Fatalf("reason %q", ans.reason)
			}
		})
	}
}

func TestSignedApprovalFollowsNoRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	g := newApprovalGate(&config.ApprovalConfig{URL: origin.URL + "/approve", FailOpen: true,
		Signing: &config.ApprovalSigning{KeyID: "k", SecretFile: secretFile(t, strings.Repeat("x", 32))}}, discard(), time.Now)
	ans, err := g.ask(context.Background(), "h", approvalFC(), approvalSubject{NewDigest: "sha256:new"}, approvalPhaseInitial)
	if err != nil {
		t.Fatal(err)
	}
	if ans.approved || ans.result != approvalDenied || followed.Load() || !strings.Contains(ans.reason, "HTTP 307") {
		t.Fatalf("answer %+v, followed %v: a redirect of a signed request is a denial", ans, followed.Load())
	}
}

func TestCanonicalApprovalHost(t *testing.T) {
	tests := []struct{ scheme, host, want string }{
		{"https", "Approver.Example.COM", "approver.example.com"},
		{"https", "approver.example.com:443", "approver.example.com"},
		{"https", "approver.example.com:8443", "approver.example.com:8443"},
		{"https", "approver.example.com:80", "approver.example.com:80"},
		{"http", "approver.example.com:80", "approver.example.com"},
		{"http", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"https", "[2001:DB8::1]:443", "[2001:db8::1]"},
		{"https", "[2001:db8::1]:9443", "[2001:db8::1]:9443"},
	}
	for _, tc := range tests {
		if got := canonicalApprovalHost(tc.scheme, tc.host); got != tc.want {
			t.Errorf("canonicalApprovalHost(%s, %s) = %s, want %s", tc.scheme, tc.host, got, tc.want)
		}
	}
}

func TestApprovalNonceFormat(t *testing.T) {
	s := newApprovalSigner(config.ApprovalSigning{}, discard())
	seen := map[string]bool{}
	for range 100 {
		n := s.nonce()
		if !nonceRE.MatchString(n) || seen[n] {
			t.Fatalf("nonce %q", n)
		}
		seen[n] = true
	}
}
