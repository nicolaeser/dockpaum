package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/registry"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

func TestRegistryLogin(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AcceptLogin("ghcr.io", "bot", "good")
	c := testClient(t, d)
	ctx := context.Background()
	if err := c.RegistryLogin(ctx, &config.RegistryAuth{Server: "ghcr.io/acme", Username: "bot", Password: "good"}); err != nil {
		t.Fatalf("login with a good token: %v", err)
	}
	err := c.RegistryLogin(ctx, &config.RegistryAuth{Server: "ghcr.io", Username: "bot", Password: "expired"})
	if err == nil || !IsDenied(err) {
		t.Fatalf("login with a wrong token = %v, want a denial", err)
	}
	calls := d.Calls("POST", "/auth")
	var ac registry.AuthConfig
	if err := json.Unmarshal(calls[0].Body, &ac); err != nil || ac.ServerAddress != "ghcr.io" || ac.Username != "bot" {
		t.Fatalf("login request %+v, %v", ac, err)
	}
}

func TestIsDenied(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New(`pull example.com/app:1: Head "https://example.com/v2/app/manifests/1": unauthorized: authentication required`), true},
		{errors.New("pull access denied for app, repository does not exist or may require 'docker login'"), true},
		{errors.New("Error response from daemon: Get https://ghcr.io/v2/: denied: denied"), true},
		{fmt.Errorf("wrapped: %w", errors.New("no basic auth credentials")), true},
		{errors.New("manifest unknown"), false},
		{errors.New("dial tcp: i/o timeout"), false},
	} {
		if got := IsDenied(tc.err); got != tc.want {
			t.Errorf("IsDenied(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
