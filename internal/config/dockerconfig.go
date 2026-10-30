package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

func ReadDockerConfig(path string) (auths []RegistryAuth, warnings []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("docker_config: %w", err)
	}
	var doc struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("docker_config %s: %w", path, err)
	}
	if doc.CredsStore != "" {
		warnings = append(warnings, fmt.Sprintf("credsStore %q is not supported; only inline auths are used", doc.CredsStore))
	}
	if len(doc.CredHelpers) > 0 {
		warnings = append(warnings, "credHelpers are not supported; only inline auths are used")
	}
	servers := make([]string, 0, len(doc.Auths))
	for s := range doc.Auths {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	for _, server := range servers {
		e := doc.Auths[server]
		user, pass := e.Username, e.Password
		if e.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return nil, nil, fmt.Errorf("docker_config %s: auth for %s is not base64", path, server)
			}
			u, p, ok := strings.Cut(string(dec), ":")
			if !ok {
				return nil, nil, fmt.Errorf("docker_config %s: auth for %s is not user:password", path, server)
			}
			user, pass = u, p
		}
		if user == "" {
			if e.IdentityToken != "" {
				warnings = append(warnings, fmt.Sprintf("identitytoken for %s is not supported", server))
			}
			continue
		}
		auths = append(auths, RegistryAuth{Server: dockerConfigServer(server), Username: user, Password: pass})
	}
	return auths, warnings, nil
}

func dockerConfigServer(key string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	s = strings.TrimSuffix(s, "/v2")
	return s
}
