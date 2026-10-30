package docker

import (
	"context"
	"errors"
	"strings"

	"github.com/moby/moby/client"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func (c *Client) RegistryLogin(ctx context.Context, a *config.RegistryAuth) error {
	user, pass, err := a.Credentials()
	if err != nil {
		return err
	}
	host, _ := a.Scope()
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	_, err = c.api.RegistryLogin(ctx, client.RegistryLoginOptions{Username: user, Password: pass, ServerAddress: host})
	return err
}

var deniedMarkers = []string{
	"unauthorized",
	"denied",
	"forbidden",
	"authentication required",
	"no basic auth credentials",
	"incorrect username or password",
}

func IsDenied(err error) bool {
	if err == nil {
		return false
	}
	var unauthorized interface{ Unauthorized() }
	var forbidden interface{ Forbidden() }
	if errors.As(err, &unauthorized) || errors.As(err, &forbidden) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range deniedMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
