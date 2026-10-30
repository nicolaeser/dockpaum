package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestAuthFor(t *testing.T) {
	auths := []config.RegistryAuth{
		{Server: "ghcr.io", Username: "gh-default"},
		{Server: "ghcr.io/acme", Username: "gh-acme"},
		{Server: "https://ghcr.io/acme/project-a/", Username: "gh-project-a"},
		{Server: "docker.io/myorg", Username: "hub-myorg"},
		{Server: "index.docker.io", Username: "hub"},
		{Server: "registry.example.com:5000/team", Username: "ex-team"},
		{Server: "example.org", Username: "parent-domain"},
	}
	tests := []struct {
		ref      string
		wantUser string
	}{
		{"ghcr.io/other/app:1", "gh-default"},
		{"ghcr.io/acme/tool:1", "gh-acme"},
		{"ghcr.io/acme/project-a:1", "gh-project-a"},
		{"ghcr.io/acme/project-a/api:1", "gh-project-a"},
		{"ghcr.io/acme/project-ab/api:1", "gh-acme"},
		{"ghcr.io/ACME/Project-A/api:1", "gh-project-a"},
		{"myorg/app:1", "hub-myorg"},
		{"docker.io/myorg/app:1", "hub-myorg"},
		{"index.docker.io/myorg/app:1", "hub-myorg"},
		{"docker.io/nginx:1.25", "hub"},
		{"nginx:1.25", "hub"},
		{"registry.example.com:5000/team/svc:2", "ex-team"},
		{"registry.example.com:5000/other/svc:2", ""},
		{"registry.example.org/x/y:1", "parent-domain"},
		{"quay.io/prometheus/prometheus:v2", ""},
		{"ghcr.io.evil.com/acme/project-a:1", ""},
		{"evilghcr.io/acme/project-a:1", ""},
		{"localhost:5000/acme/project-a:1", ""},
		{"ghcr.io/acme/project-a@sha256:" + strings.Repeat("a", 64), "gh-project-a"},
	}
	for _, tt := range tests {
		got := AuthFor(auths, Parse(tt.ref))
		if tt.wantUser == "" {
			if got != nil {
				t.Errorf("AuthFor(%q) = %q, want nil", tt.ref, got.Username)
			}
			continue
		}
		if got == nil || got.Username != tt.wantUser {
			t.Errorf("AuthFor(%q) = %v, want %q", tt.ref, got, tt.wantUser)
		}
	}
}

func TestAuthForFirstHostEntryWins(t *testing.T) {
	auths := []config.RegistryAuth{
		{Server: "ghcr.io", Username: "first"},
		{Server: "ghcr.io", Username: "second"},
	}
	if got := AuthFor(auths, Parse("ghcr.io/a/b:1")); got == nil || got.Username != "first" {
		t.Fatalf("AuthFor = %v, want first", got)
	}
}

func basicAuthRegistry(t *testing.T, want map[string][2]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
		cred, ok := want[repo]
		user, pass, hasAuth := r.BasicAuth()
		if !ok || !hasAuth || user != cred[0] || pass != cred[1] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"tags":["1.0.0"]}`)
	}))
}

func TestListTagsUsesRepositoryScopedCredentials(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), ".pull-token")
	if err := os.WriteFile(tokenFile, []byte("token-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := basicAuthRegistry(t, map[string][2]string{
		"acme/project-a/api": {"bot-a", "token-a"},
		"acme/project-b/api": {"bot-b", "token-b"},
	})
	defer srv.Close()
	host := hostOf(srv)

	c := New([]config.RegistryAuth{
		{Server: host, Username: "fallback", Password: "wrong"},
		{Server: host + "/acme/project-a", Username: "bot-a", Password: "token-a"},
		{Server: host + "/acme/project-b", Username: "bot-b", PasswordFile: tokenFile},
	})
	for _, repo := range []string{"acme/project-a/api", "acme/project-b/api"} {
		if _, err := c.ListTags(context.Background(), Reference{Registry: host, Repo: repo}); err != nil {
			t.Errorf("ListTags(%s): %v", repo, err)
		}
	}
	if _, err := c.ListTags(context.Background(), Reference{Registry: host, Repo: "acme/project-c/api"}); err == nil {
		t.Error("ListTags(project-c) succeeded with the fallback credentials")
	}
}

func TestListTagsPasswordFileErrorIsNotAnonymous(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"tags":[]}`)
	}))
	defer srv.Close()
	host := hostOf(srv)

	c := New([]config.RegistryAuth{
		{Server: host, Username: "bot", PasswordFile: filepath.Join(t.TempDir(), "missing")},
	})
	_, err := c.ListTags(context.Background(), Reference{Registry: host, Repo: "repo"})
	if err == nil || !strings.Contains(err.Error(), "password_file") {
		t.Fatalf("expected a password_file error, got %v", err)
	}
	if calls.Load() != 0 {
		t.Error("an unreadable password_file must not fall back to an anonymous request")
	}
}

func TestListTagsRefusesCrossOriginPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://elsewhere.example/v2/repo/tags/list?last=b>; rel="next"`)
		_, _ = fmt.Fprint(w, `{"name":"repo","tags":["a","b"]}`)
	}))
	defer srv.Close()

	c := New([]config.RegistryAuth{{Server: hostOf(srv), Username: "bot", Password: "secret"}})
	_, err := c.ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("expected cross-origin pagination to be refused, got %v", err)
	}
}

func TestBearerTokenRefusesPlainHTTPRealmWithCredentials(t *testing.T) {
	c := New(nil)
	_, err := c.bearerToken(context.Background(), `Bearer realm="http://auth.example.com/token",service="x"`,
		"bot", "secret", "registry.example.com", "repo", "")
	if err == nil || !strings.Contains(err.Error(), "https required") {
		t.Fatalf("expected the plain-http realm to be refused, got %v", err)
	}
}
