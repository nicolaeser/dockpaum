package updater

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/pulltoken"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

func localHosts(hosts []config.HostConfig) func(string) bool {
	local := map[string]bool{}
	for _, h := range hosts {
		local[h.Name] = localEndpoint(h.Endpoint)
	}
	return func(name string) bool { return local[name] }
}

func localEndpoint(endpoint string) bool {
	if endpoint == "" {
		endpoint = os.Getenv("DOCKER_HOST")
	}
	if endpoint == "" {
		return true
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "unix", "npipe":
		return true
	case "tcp", "http", "https":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func projectDirs(targets []docker.Container) []string {
	var out []string
	for _, ct := range targets {
		if dir := pulltoken.ProjectDir(ct.Labels); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

func (u *Updater) loadTokens(ctx context.Context, c *docker.Client, all, targets []docker.Container) map[string]*pulltoken.File {
	u.selfMu.Lock()
	self := pulltoken.Self{ID: u.selfID, Clients: u.deps.Clients.List()}
	u.selfMu.Unlock()
	return u.tokens.Load(ctx, c, self, all, projectDirs(targets))
}

func (u *Updater) credentialsFor(hr *hostRun, host string, labels map[string]string, imageRef string) *registry.Credentials {
	ref := registry.Parse(imageRef)
	dir := pulltoken.ProjectDir(labels)
	var f *pulltoken.File
	if hr != nil && dir != "" {
		f = hr.tokens[dir]
	}
	if f == nil {
		return u.keyring.Credentials("", u.central, ref, "")
	}
	if cs := u.keyring.Credentials(host+"\x00"+dir, f.Credentials, ref, f.Problem); cs.Len() > 0 {
		return cs
	}
	return u.keyring.Credentials("", u.central, ref, f.Problem)
}

func pullWith(ctx context.Context, c *docker.Client, ref string, creds *registry.Credentials, platform string) (string, error) {
	var id string
	err := creds.Try(func(a *config.RegistryAuth) error {
		var err error
		id, err = c.PullImagePlatform(ctx, ref, a, platform)
		return err
	}, docker.IsDenied)
	return id, err
}
