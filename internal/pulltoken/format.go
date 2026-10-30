package pulltoken

import (
	"fmt"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type Entry struct {
	Auth config.RegistryAuth
	Line int
}

type LineError struct {
	Line   int
	Reason string
}

func (e LineError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Reason) }

const expectedLine = `expected "<registry>[/<namespace>[/<repo>]] <username> <token>"`

var dockerHubHosts = map[string]bool{
	"docker.io":               true,
	"index.docker.io":         true,
	"registry-1.docker.io":    true,
	"registry.hub.docker.com": true,
}

var tokenPrefixes = []string{"ghp_", "gho_", "ghs_", "ghu_", "github_pat_", "glpat-", "gldt-", "dckr_pat_", "dckr_oat_"}

func Parse(body []byte) ([]Entry, []LineError) {
	var entries []Entry
	var errs []LineError
	text := strings.TrimPrefix(string(body), "\ufeff")
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		auth, reason := parseLine(line)
		if reason != "" {
			errs = append(errs, LineError{Line: i + 1, Reason: reason})
			continue
		}
		entries = append(entries, Entry{Line: i + 1, Auth: auth})
	}
	return entries, errs
}

func parseLine(line string) (config.RegistryAuth, string) {
	first, rest := cutField(line)
	user, secret := cutField(rest)
	if user == "" || secret == "" {
		return config.RegistryAuth{}, expectedLine
	}
	s := first
	for _, scheme := range []string{"https://", "http://"} {
		if len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			s = s[len(scheme):]
			break
		}
	}
	host, path, _ := strings.Cut(strings.Trim(s, "/"), "/")
	host = strings.ToLower(host)
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return config.RegistryAuth{}, "the first field is not a registry host (like ghcr.io, docker.io or registry.example.com:5000); " + expectedLine
	}
	if strings.ContainsAny(path, ":@") {
		return config.RegistryAuth{}, "the repository path must not contain a tag or digest"
	}
	if looksLikeToken(user) {
		return config.RegistryAuth{}, "the second field looks like a token, not a username; " + expectedLine
	}
	if dockerHubHosts[host] {
		host = "docker.io"
	}
	scope := host
	if path = strings.ToLower(strings.Trim(path, "/")); path != "" {
		scope += "/" + path
	}
	return config.RegistryAuth{Server: scope, Username: user, Password: secret}, ""
}

func cutField(s string) (field, rest string) {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i:])
	}
	return s, ""
}

func looksLikeToken(s string) bool {
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
