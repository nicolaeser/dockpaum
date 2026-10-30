package registry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/registry/registrytest"
)

func cred(server, user, pass, source string) Credential {
	return NewCredential(config.RegistryAuth{Server: server, Username: user, Password: pass}, source)
}

func users(list []Credential) []string {
	var out []string
	for _, c := range list {
		out = append(out, c.Auth.Username)
	}
	return out
}

func TestCandidatesPreferTheMostSpecificScopeAndKeepEqualScopesInOrder(t *testing.T) {
	list := []Credential{
		cred("ghcr.io", "any-1", "t", "l1"),
		cred("ghcr.io/acme", "acme-1", "t", "l2"),
		cred("https://ghcr.io/ACME/", "acme-2", "t", "l3"),
		cred("ghcr.io", "any-2", "t", "l4"),
		cred("ghcr.io/acme/billing", "billing", "t", "l5"),
		cred("docker.io", "hub-1", "t", "l6"),
		cred("index.docker.io", "hub-2", "t", "l7"),
		cred("example.org", "parent", "t", "l8"),
		cred("registry.example.org", "exact", "t", "l9"),
	}
	for _, tc := range []struct {
		ref  string
		want string
	}{
		{"ghcr.io/acme/web:1", "acme-1,acme-2"},
		{"ghcr.io/acme/billing:2", "billing"},
		{"ghcr.io/someone/tool", "any-1,any-2"},
		{"nginx:1", "hub-1,hub-2"},
		{"registry.example.org/x/y:1", "parent"},
		{"quay.io/x/y:1", ""},
	} {
		if got := strings.Join(users(Candidates(list, Parse(tc.ref))), ","); got != tc.want {
			t.Errorf("Candidates(%s) = %s, want %s", tc.ref, got, tc.want)
		}
	}
}

func deniedFor(bad ...string) (func(*config.RegistryAuth) error, *[]string) {
	var calls []string
	return func(a *config.RegistryAuth) error {
		calls = append(calls, a.Username+":"+a.Password)
		for _, b := range bad {
			if a.Password == b {
				return denied(errors.New("token HTTP 401: unauthorized"))
			}
		}
		return nil
	}, &calls
}

func TestTryFallsBackToEqualScopeEntriesAndRemembersTheOneThatWorked(t *testing.T) {
	var logs bytes.Buffer
	k := NewKeyring(slog.New(slog.NewTextHandler(&logs, nil)))
	ref := Parse("ghcr.io/acme/app:1")
	list := []Credential{cred("ghcr.io", "old", "expired-secret", "/srv/a/.pull_token line 1"), cred("ghcr.io", "new", "fresh-secret", "/srv/a/.pull_token line 2")}

	fn, calls := deniedFor("expired-secret")
	if err := k.Credentials("a", list, ref, "").Try(fn, IsDenied); err != nil {
		t.Fatal(err)
	}
	if err := k.Credentials("a", list, ref, "").Try(fn, IsDenied); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, " "); got != "old:expired-secret new:fresh-secret new:fresh-secret" {
		t.Fatalf("attempts %s: the entry that worked must be tried first next time", got)
	}
	out := logs.String()
	if strings.Count(out, "registry rejected the credentials; trying the next entry") != 1 ||
		!strings.Contains(out, `source="/srv/a/.pull_token line 1"`) || !strings.Contains(out, "username=old") {
		t.Fatalf("logs:\n%s", out)
	}

	fn, calls = deniedFor("expired-secret")
	if err := k.Credentials("b", list, ref, "").Try(fn, IsDenied); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, " "); got != "old:expired-secret new:fresh-secret" {
		t.Fatalf("attempts %s: another project starts from its own file order", got)
	}

	fn, _ = deniedFor("expired-secret", "fresh-secret")
	err := k.Credentials("a", list, ref, "").Try(fn, IsDenied)
	if err == nil || !IsDenied(err) || !strings.Contains(err.Error(), "/srv/a/.pull_token line 1 (ghcr.io, user old)") ||
		!strings.Contains(err.Error(), "/srv/a/.pull_token line 2 (ghcr.io, user new)") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(logs.String(), "secret") {
		t.Fatalf("a token leaked: %v\n%s", err, logs.String())
	}
}

func TestTryStopsAtErrorsThatAreNotDenials(t *testing.T) {
	k := NewKeyring(nil)
	list := []Credential{cred("ghcr.io", "a", "1", "l1"), cred("ghcr.io", "b", "2", "l2")}
	var calls int
	boom := errors.New("dial tcp: i/o timeout")
	err := k.Credentials("", list, Parse("ghcr.io/x/y"), "").Try(func(*config.RegistryAuth) error { calls++; return boom }, IsDenied)
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

func TestTryAddsTheNoteToDenials(t *testing.T) {
	k := NewKeyring(nil)
	cs := k.Credentials("p", nil, Parse("ghcr.io/x/y"), "invalid lines in /srv/p/.pull_token are ignored: line 2: bad")
	err := cs.Try(func(a *config.RegistryAuth) error {
		if a != nil {
			t.Fatal("no credentials must be anonymous")
		}
		return denied(errors.New("token HTTP 401"))
	}, IsDenied)
	if err == nil || !strings.Contains(err.Error(), "/srv/p/.pull_token") {
		t.Fatalf("err = %v", err)
	}
	other := errors.New("manifest unknown")
	if err := cs.Try(func(*config.RegistryAuth) error { return other }, IsDenied); err != other {
		t.Fatalf("a non-denial must pass unchanged, got %v", err)
	}
}

type authFixture struct {
	reg *registrytest.Registry
	ref Reference
	c   *Client
	k   *Keyring
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	reg := registrytest.New(t)
	reg.Push("acme/app", "1", registrytest.Image{Build: "1", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	reg.RequireAuth("acme/app",
		registrytest.Login{Username: "alice", Password: "alice-token"},
		registrytest.Login{Username: "bob", Password: "bob-token"},
		registrytest.Login{Username: "shared", Password: "token-of-project-a"})
	return &authFixture{reg: reg, ref: Parse(reg.Host() + "/acme/app:1"), c: New(nil), k: NewKeyring(nil)}
}

func (f *authFixture) check(project, user, pass string) error {
	cs := f.k.Credentials(project, []Credential{cred(f.reg.Host(), user, pass, "/srv/"+project+"/.pull_token line 1")}, f.ref, "")
	_, err := f.c.ManifestDigest(WithCredentials(context.Background(), cs), f.ref)
	return err
}

func (f *authFixture) tokenPasswords() []string {
	var out []string
	for _, r := range f.reg.TokenRequests() {
		out = append(out, r.Password)
	}
	return out
}

func TestProjectsWithDifferentUsersForOneRepositoryEachAuthenticateThemselves(t *testing.T) {
	f := newAuthFixture(t)
	for i := 0; i < 2; i++ {
		if err := f.check("a", "alice", "alice-token"); err != nil {
			t.Fatal(err)
		}
		if err := f.check("b", "bob", "bob-token"); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.tokenPasswords(), ","); got != "alice-token,bob-token" {
		t.Fatalf("token requests %s: each project gets and caches its own token", got)
	}
}

func TestAWrongTokenFailsEvenWhenAnotherProjectJustAuthenticated(t *testing.T) {
	for _, order := range [][]string{{"a", "b", "a", "b"}, {"b", "a", "b", "a"}} {
		f := newAuthFixture(t)
		for _, project := range order {
			pass := map[string]string{"a": "token-of-project-a", "b": "outdated-token-of-b"}[project]
			err := f.check(project, "shared", pass)
			switch project {
			case "a":
				if err != nil {
					t.Fatalf("order %v: project a: %v", order, err)
				}
			case "b":
				if err == nil || !IsDenied(err) || !strings.Contains(err.Error(), "/srv/b/.pull_token line 1") {
					t.Fatalf("order %v: project b must fail, got %v", order, err)
				}
				if strings.Contains(err.Error(), "outdated-token") {
					t.Fatalf("the token leaked into %v", err)
				}
			}
		}
		var a, b int
		for _, r := range f.reg.TokenRequests() {
			switch {
			case r.Password == "token-of-project-a" && r.Granted:
				a++
			case r.Password == "outdated-token-of-b" && !r.Granted:
				b++
			default:
				t.Fatalf("order %v: unexpected token request %+v", order, r)
			}
		}
		if a != 1 || b != 2 {
			t.Fatalf("order %v: %d token requests for a, %d for b; a's token is cached for a only", order, a, b)
		}
	}
}

func TestEqualScopeEntriesOfOneFileAreTriedInOrderAndTheGoodOneIsRemembered(t *testing.T) {
	f := newAuthFixture(t)
	list := []Credential{
		cred(f.reg.Host(), "alice", "revoked", "/srv/a/.pull_token line 1"),
		cred(f.reg.Host(), "bob", "bob-token", "/srv/a/.pull_token line 2"),
	}
	for i := 0; i < 3; i++ {
		cs := f.k.Credentials("a", list, f.ref, "")
		if _, err := f.c.ManifestDigest(WithCredentials(context.Background(), cs), f.ref); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.tokenPasswords(), ","); got != "revoked,bob-token" {
		t.Fatalf("token requests %s: the revoked entry is tried once, then the remembered one comes first", got)
	}
}

func TestEqualScopeCentralEntriesFallBack(t *testing.T) {
	f := newAuthFixture(t)
	c := New([]config.RegistryAuth{
		{Server: f.reg.Host(), Username: "alice", Password: "revoked"},
		{Server: f.reg.Host(), Username: "alice", Password: "alice-token"},
	})
	if _, err := c.ManifestDigest(context.Background(), f.ref); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.tokenPasswords(), ","); got != "revoked,alice-token" {
		t.Fatalf("token requests %s", got)
	}
}
