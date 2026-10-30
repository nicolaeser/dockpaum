package cli

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/server"
)

type guardedHealth struct {
	port string
	mu   sync.Mutex
	auth []string
}

func newGuardedHealth(t *testing.T, cfg config.HTTPConfig) *guardedHealth {
	t.Helper()
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	h := server.New(cfg, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), server.Version{}, nil, server.API{}).Handler()
	g := &guardedHealth{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	g.port = port
	return g
}

func (g *guardedHealth) lastAuth() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.auth) == 0 {
		return ""
	}
	return g.auth[len(g.auth)-1]
}

func secretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunHealthProbeSendsTheToken(t *testing.T) {
	tests := []struct {
		name     string
		server   config.HTTPConfig
		http     string
		env      map[string]string
		wantAuth string
	}{
		{name: "token", server: config.HTTPConfig{Token: "secret"}, http: "  token: secret\n", wantAuth: "Bearer secret"},
		{name: "token_file", server: config.HTTPConfig{Token: "secret"}, http: "  token_file: {secret}\n", wantAuth: "Bearer secret"},
		{name: "read_token is preferred", server: config.HTTPConfig{Token: "secret", ReadToken: "reader"},
			http: "  token: secret\n  read_token: reader\n", wantAuth: "Bearer reader"},
		{name: "read_token alone", server: config.HTTPConfig{ReadToken: "reader"}, http: "  read_token: reader\n", wantAuth: "Bearer reader"},
		{name: "read_token_file", server: config.HTTPConfig{Token: "secret", ReadToken: "reader"},
			http: "  token: secret\n  read_token_file: {reader}\n", wantAuth: "Bearer reader"},
		{name: "DOCKPAUM_HTTP_TOKEN", server: config.HTTPConfig{Token: "secret"},
			env: map[string]string{"DOCKPAUM_HTTP_TOKEN": "secret"}, wantAuth: "Bearer secret"},
		{name: "DOCKPAUM_HTTP_READ_TOKEN_FILE", server: config.HTTPConfig{Token: "secret", ReadToken: "reader"},
			env: map[string]string{"DOCKPAUM_HTTP_READ_TOKEN_FILE": "{reader}"}, http: "  token: secret\n", wantAuth: "Bearer reader"},
		{name: "no token anywhere", server: config.HTTPConfig{}, wantAuth: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			files := strings.NewReplacer("{secret}", secretFile(t, "secret\n"), "{reader}", secretFile(t, "reader\n"))
			for k, v := range tt.env {
				t.Setenv(k, files.Replace(v))
			}
			g := newGuardedHealth(t, tt.server)
			p := writeConfig(t, fmt.Sprintf("http:\n  port: %s\n%s", g.port, files.Replace(tt.http)))
			var stderr bytes.Buffer
			if code := runHealthProbe(p, &stderr); code != exitOK {
				t.Fatalf("runHealthProbe = %d, want %d: %s", code, exitOK, stderr.String())
			}
			if got := g.lastAuth(); got != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tt.wantAuth)
			}
		})
	}
}

func TestRunHealthProbeFailsWithoutTheRightToken(t *testing.T) {
	for _, tt := range []struct{ name, http string }{
		{"nothing configured", ""},
		{"a different one configured", "  token: other\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			g := newGuardedHealth(t, config.HTTPConfig{Token: "secret"})
			p := writeConfig(t, fmt.Sprintf("http:\n  port: %s\n%s", g.port, tt.http))
			var stderr bytes.Buffer
			if code := runHealthProbe(p, &stderr); code == exitOK {
				t.Fatal("a health endpoint that refuses the probe must fail the check")
			}
			if !strings.Contains(stderr.String(), "healthcheck: HTTP 401") {
				t.Errorf("stderr = %q, want the 401 named", stderr.String())
			}
		})
	}
}

func TestRunHealthProbeHTTPSwitchedOff(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		yaml string
		env  string
	}{
		{"http.enabled: off", "http:\n  enabled: off\n  port: " + port + "\n  token: secret\n", ""},
		{"http.enabled: false", "http:\n  enabled: false\n  port: " + port + "\n", ""},
		{"DOCKPAUM_HTTP_ENABLED=false", "http:\n  port: " + port + "\n  token: secret\n", "false"},
		{"DOCKPAUM_HTTP_ENABLED=OFF wins over the file", "http:\n  enabled: true\n  port: " + port + "\n", "OFF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			if tt.env != "" {
				t.Setenv("DOCKPAUM_HTTP_ENABLED", tt.env)
			}
			var stderr bytes.Buffer
			if code := runHealthProbe(writeConfig(t, tt.yaml), &stderr); code != exitOK {
				t.Fatalf("runHealthProbe = %d, want %d without a server to probe: %s", code, exitOK, stderr.String())
			}
		})
	}
	clearConfigEnv(t)
	t.Setenv("DOCKPAUM_HTTP_ENABLED", "true")
	if code := runHealthProbe(writeConfig(t, "http:\n  enabled: false\n  port: "+port+"\n"), io.Discard); code == exitOK {
		t.Fatal("DOCKPAUM_HTTP_ENABLED=true must turn the probe back on, and nothing listens")
	}
}
