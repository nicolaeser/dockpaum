package updater

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

func TestAuthForMatchesRegistryHost(t *testing.T) {
	u := &Updater{keyring: registry.NewKeyring(nil), central: registry.CentralCredentials([]config.RegistryAuth{
		{Server: "ghcr.io", Username: "gh"},
		{Server: "https://docker.io/", Username: "dh"},
		{Server: "registry.example.com:5000", Username: "ex"},
		{Server: "ghcr.io/acme/project-a", Username: "gh-a"},
	})}

	cases := []struct {
		ref      string
		wantUser string
	}{
		{"ghcr.io/owner/app:1.0.0", "gh"},
		{"nginx:1.25", "dh"},
		{"docker.io/library/redis:7", "dh"},
		{"registry.example.com:5000/team/svc:2", "ex"},
		{"quay.io/prometheus/prometheus:v2", ""},
		{"ghcr.io/acme/project-a/api:1", "gh-a"},
		{"ghcr.io/acme/project-b:1", "gh"},
	}
	for _, c := range cases {
		got := u.credentialsFor(nil, "fake", nil, c.ref).First()
		if c.wantUser == "" {
			if got != nil {
				t.Errorf("credentialsFor(%q) = %+v, want none", c.ref, got)
			}
			continue
		}
		if got == nil || got.Username != c.wantUser {
			t.Errorf("credentialsFor(%q) user = %v, want %q", c.ref, got, c.wantUser)
		}
	}
}

func TestNewRunIDFormat(t *testing.T) {
	id := newRunID()
	if len(id) != 16 {
		t.Fatalf("run id %q length = %d, want 16 hex chars", id, len(id))
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Fatalf("run id %q has non-hex char %q", id, r)
		}
	}
	if newRunID() == id {
		t.Error("two run ids should differ")
	}
}

func TestRecoveryContextSurvivesParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	rctx, rcancel := recoveryContext(parent)
	defer rcancel()
	cancel()
	if err := rctx.Err(); err != nil {
		t.Fatalf("recovery context must survive parent cancellation, got %v", err)
	}
	if _, ok := rctx.Deadline(); !ok {
		t.Error("recovery context should still be bounded by a deadline")
	}
	rcancel()
	if !errors.Is(rctx.Err(), context.Canceled) {
		t.Errorf("recovery context should cancel on its own cancel, got %v", rctx.Err())
	}
}

func TestRecoveryContextDeadlineBounded(t *testing.T) {
	rctx, cancel := recoveryContext(context.Background())
	defer cancel()
	dl, ok := rctx.Deadline()
	if !ok || time.Until(dl) > 3*time.Minute {
		t.Errorf("recovery context deadline unbounded or too far: ok=%v dl=%v", ok, dl)
	}
}
