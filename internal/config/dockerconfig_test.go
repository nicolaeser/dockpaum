package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDockerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("hubuser:hubpass"))
	body := `{
  "auths": {
    "https://index.docker.io/v1/": {"auth": "` + auth + `"},
    "ghcr.io": {"username": "gh", "password": "tok"},
    "quay.io": {"identitytoken": "x"}
  },
  "credsStore": "desktop"
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	auths, warnings, err := ReadDockerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("auths = %+v", auths)
	}
	if auths[0].Server != "ghcr.io" || auths[0].Password != "tok" {
		t.Errorf("ghcr entry = %+v", auths[0])
	}
	if auths[1].Server != "index.docker.io" || auths[1].Username != "hubuser" || auths[1].Password != "hubpass" {
		t.Errorf("hub entry = %+v", auths[1])
	}
	joined := strings.Join(warnings, "; ")
	if !strings.Contains(joined, "credsStore") || !strings.Contains(joined, "identitytoken") {
		t.Errorf("warnings = %v", warnings)
	}
	if _, _, err := ReadDockerConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file must be an error")
	}
}
