package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestCheckRedirect(t *testing.T) {
	tests := []struct {
		name      string
		from, to  string
		hops      int
		wantErr   string
		keepCreds bool
	}{
		{name: "same origin", from: "https://example.com/v2/a", to: "https://example.com/v2/b", keepCreds: true},
		{name: "explicit default port", from: "https://example.com/v2/a", to: "https://example.com:443/v2/b", keepCreds: true},
		{name: "host case", from: "https://Example.com/v2/a", to: "https://example.COM/v2/b", keepCreds: true},
		{name: "other port", from: "https://example.com/v2/a", to: "https://example.com:8443/v2/b"},
		{name: "subdomain", from: "https://example.com/v2/a", to: "https://cdn.example.com/blob"},
		{name: "other host", from: "https://registry.example.com/v2/a", to: "https://storage.example.net/blob?sig=1"},
		{name: "downgrade same host", from: "https://example.com/v2/a", to: "http://example.com/v2/a", wantErr: "plain HTTP"},
		{name: "downgrade to loopback", from: "https://example.com/v2/a", to: "http://127.0.0.1:5000/v2/a", wantErr: "plain HTTP"},
		{name: "loopback registry stays", from: "http://localhost:5000/v2/a", to: "http://localhost:5000/v2/b", keepCreds: true},
		{name: "loopback registry other port", from: "http://localhost:5000/v2/a", to: "http://127.0.0.1:5001/v2/a"},
		{name: "loopback registry to plain remote", from: "http://localhost:5000/v2/a", to: "http://registry.example.com/v2/a", wantErr: "plain HTTP"},
		{name: "loopback registry to https", from: "http://localhost:5000/v2/a", to: "https://registry.example.com/v2/a"},
		{name: "unsupported scheme", from: "https://example.com/v2/a", to: "ftp://example.com/v2/a", wantErr: "unsupported scheme"},
		{name: "too many", from: "https://example.com/v2/a", to: "https://example.com/v2/b", hops: maxRedirects, wantErr: "stopped after 5 redirects"},
		{name: "last allowed hop", from: "https://example.com/v2/a", to: "https://example.com/v2/b", hops: maxRedirects - 1, keepCreds: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, err := http.NewRequest(http.MethodGet, tt.from, nil)
			if err != nil {
				t.Fatal(err)
			}
			via := []*http.Request{first}
			for len(via) < tt.hops {
				via = append(via, first)
			}
			req, err := http.NewRequest(http.MethodGet, tt.to, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth("u", "s3cret")
			err = checkRedirect(req, via)
			if tt.wantErr != "" {
				var re *redirectError
				if !errors.As(err, &re) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("checkRedirect = %v, want a redirectError containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkRedirect = %v", err)
			}
			if got := req.Header.Get("Authorization") != ""; got != tt.keepCreds {
				t.Errorf("Authorization kept = %v, want %v", got, tt.keepCreds)
			}
		})
	}
}

type redirectHarness struct {
	registry *httptest.Server
	tlsSink  *httptest.Server
	httpSink *httptest.Server

	bearer        bool
	location      string
	tokenLocation string

	mu        sync.Mutex
	sinkAuths []string
	movedAuth []string
	redirects atomic.Int32
}

func newRedirectHarness(t *testing.T, bearer bool, location string) (*redirectHarness, *Client) {
	t.Helper()
	h := &redirectHarness{bearer: bearer, location: location}
	h.registry = httptest.NewTLSServer(http.HandlerFunc(h.serveRegistry))
	h.tlsSink = httptest.NewTLSServer(http.HandlerFunc(h.serveSink))
	h.httpSink = httptest.NewServer(http.HandlerFunc(h.serveSink))
	t.Cleanup(func() {
		h.registry.Close()
		h.tlsSink.Close()
		h.httpSink.Close()
	})
	routes := map[string]string{
		"example.com:443":     h.registry.Listener.Addr().String(),
		"example.com:8443":    h.tlsSink.Listener.Addr().String(),
		"sub.example.com:443": h.tlsSink.Listener.Addr().String(),
		"127.0.0.1:443":       h.tlsSink.Listener.Addr().String(),
		"example.com:80":      h.httpSink.Listener.Addr().String(),
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.registry.Certificate())
	c := New([]config.RegistryAuth{{Server: "example.com", Username: "u", Password: "s3cret"}})
	c.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target, ok := routes[addr]
			if !ok {
				return nil, fmt.Errorf("test: no route to %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, target)
		},
	}
	return h, c
}

func (h *redirectHarness) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" && h.tokenLocation != "" {
		http.Redirect(w, r, h.tokenLocation+"/token", http.StatusFound)
		return
	}
	if r.URL.Path == "/token" {
		if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"tok","expires_in":300}`)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/moved/") {
		h.mu.Lock()
		h.movedAuth = append(h.movedAuth, r.Header.Get("Authorization"))
		h.mu.Unlock()
		serveRegistryContent(w, r)
		return
	}
	if h.bearer && r.Header.Get("Authorization") != "Bearer tok" {
		w.Header().Set("Www-Authenticate", `Bearer realm="https://example.com/token",service="example.com",scope="repository:app:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.redirects.Add(1)
	http.Redirect(w, r, h.location+redirectedPath(r), http.StatusFound)
}

func redirectedPath(r *http.Request) string {
	if strings.HasSuffix(r.URL.Path, "/tags/list") {
		return "/v2/app/tags/list"
	}
	return "/v2/app/manifests/redirected"
}

func (h *redirectHarness) serveSink(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.sinkAuths = append(h.sinkAuths, r.Header.Get("Authorization"))
	h.mu.Unlock()
	serveRegistryContent(w, r)
}

func serveRegistryContent(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/tags/list"):
		_, _ = fmt.Fprint(w, `{"tags":["1"]}`)
	case strings.Contains(r.URL.Path, "/manifests/") && r.Method == http.MethodHead:
		w.Header().Set("Docker-Content-Digest", testDigestA)
		w.Header().Set("Content-Type", MediaTypeDockerManifest)
	case strings.Contains(r.URL.Path, "/manifests/"):
		w.Header().Set("Content-Type", MediaTypeOCIIndex)
		_, _ = fmt.Fprint(w, testIndex)
	default:
		http.NotFound(w, r)
	}
}

func TestRedirectsNeverCarryCredentials(t *testing.T) {
	calls := []struct {
		name string
		do   func(*Client) error
	}{
		{"tags/list", func(c *Client) error {
			_, err := c.ListTags(context.Background(), Reference{Registry: "example.com", Repo: "app"})
			return err
		}},
		{"manifest HEAD", func(c *Client) error {
			_, err := c.ManifestDigest(context.Background(), Reference{Registry: "example.com", Repo: "app", Tag: "1"})
			return err
		}},
		{"manifest GET", func(c *Client) error {
			desc := Descriptor{MediaType: MediaTypeOCIIndex, Digest: sha256Digest([]byte(testIndex))}
			_, err := c.PlatformManifest(context.Background(), Reference{Registry: "example.com", Repo: "app"},
				desc, Platform{OS: "linux", Architecture: "amd64"})
			return err
		}},
	}
	targets := []struct {
		name     string
		location string
		refused  bool
		sameSite bool
	}{
		{name: "downgrade to plain HTTP", location: "http://example.com", refused: true},
		{name: "same host other port", location: "https://example.com:8443"},
		{name: "subdomain", location: "https://sub.example.com"},
		{name: "other host", location: "https://127.0.0.1"},
		{name: "same origin", location: "https://example.com/moved", sameSite: true},
	}
	for _, bearer := range []bool{false, true} {
		wantAuth := "Basic dTpzM2NyZXQ="
		authName := "basic"
		if bearer {
			wantAuth, authName = "Bearer tok", "bearer"
		}
		for _, call := range calls {
			for _, target := range targets {
				t.Run(authName+"/"+call.name+"/"+target.name, func(t *testing.T) {
					h, c := newRedirectHarness(t, bearer, target.location)
					err := call.do(c)
					h.mu.Lock()
					defer h.mu.Unlock()
					if target.refused {
						var re *redirectError
						if !errors.As(err, &re) || !strings.Contains(err.Error(), "plain HTTP") {
							t.Fatalf("err = %v, want a refused redirect", err)
						}
						for _, secret := range []string{"s3cret", "dTpzM2NyZXQ", "Bearer"} {
							if strings.Contains(err.Error(), secret) {
								t.Errorf("error leaks a credential: %v", err)
							}
						}
						if len(h.sinkAuths) != 0 {
							t.Errorf("plain-HTTP sink got %d requests", len(h.sinkAuths))
						}
						if got := h.redirects.Load(); got != 1 {
							t.Errorf("registry redirected %d times, want 1 (a refusal is not retried)", got)
						}
						return
					}
					if err != nil {
						t.Fatalf("redirected call: %v", err)
					}
					if target.sameSite {
						if len(h.movedAuth) != 1 || h.movedAuth[0] != wantAuth || len(h.sinkAuths) != 0 {
							t.Errorf("same-origin redirect got Authorization %q (sinks %q), want %q", h.movedAuth, h.sinkAuths, wantAuth)
						}
						return
					}
					if len(h.sinkAuths) != 1 || h.sinkAuths[0] != "" {
						t.Errorf("sink got Authorization %q, want one request without it", h.sinkAuths)
					}
				})
			}
		}
	}
}

func TestRedirectLoopStopsWithoutRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/v2/app/manifests/1", http.StatusFound)
	}))
	defer srv.Close()
	_, err := New(nil).ManifestDigest(context.Background(), Reference{Registry: hostOf(srv), Repo: "app", Tag: "1"})
	var re *redirectError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("err = %v, want the redirect bound", err)
	}
	if got := hits.Load(); got != maxRedirects {
		t.Errorf("server got %d requests, want %d (one chain, no retries)", got, maxRedirects)
	}
}

func TestTokenRedirectsNeverCarryCredentials(t *testing.T) {
	tests := []struct {
		name     string
		location string
		refused  bool
	}{
		{name: "downgrade to plain HTTP", location: "http://example.com", refused: true},
		{name: "subdomain", location: "https://sub.example.com"},
		{name: "same host other port", location: "https://example.com:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, c := newRedirectHarness(t, true, "https://example.com/moved")
			h.tokenLocation = tt.location
			_, err := c.ListTags(context.Background(), Reference{Registry: "example.com", Repo: "app"})
			if err == nil {
				t.Fatal("ListTags succeeded without a token")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if tt.refused {
				var re *redirectError
				if !errors.As(err, &re) {
					t.Errorf("err = %v, want a refused redirect", err)
				}
				if len(h.sinkAuths) != 0 {
					t.Errorf("plain-HTTP sink got %d requests", len(h.sinkAuths))
				}
				return
			}
			if len(h.sinkAuths) != 1 || h.sinkAuths[0] != "" {
				t.Errorf("token redirect target got Authorization %q, want one request without it", h.sinkAuths)
			}
		})
	}
}
