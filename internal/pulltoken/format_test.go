package pulltoken

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestParse(t *testing.T) {
	body := "\ufeff# tokens for the shop\r\n" +
		"\r\n" +
		"ghcr.io   shop-bot   tok-everything\r\n" +
		"  ghcr.io/Acme\tacme-bot\ttok-acme  \n" +
		"https://ghcr.io/acme/billing/ billing tok-billing\n" +
		"index.docker.io hub-user tok-hub\n" +
		"registry.example.com:5000 deploy pass with spaces\n" +
		"localhost:5000 dev tok-local\n" +
		"ghcr.io second-bot tok-second\n"
	entries, errs := Parse([]byte(body))
	if len(errs) != 0 {
		t.Fatalf("errors %v", errs)
	}
	want := []Entry{
		{Line: 3, Auth: config.RegistryAuth{Server: "ghcr.io", Username: "shop-bot", Password: "tok-everything"}},
		{Line: 4, Auth: config.RegistryAuth{Server: "ghcr.io/acme", Username: "acme-bot", Password: "tok-acme"}},
		{Line: 5, Auth: config.RegistryAuth{Server: "ghcr.io/acme/billing", Username: "billing", Password: "tok-billing"}},
		{Line: 6, Auth: config.RegistryAuth{Server: "docker.io", Username: "hub-user", Password: "tok-hub"}},
		{Line: 7, Auth: config.RegistryAuth{Server: "registry.example.com:5000", Username: "deploy", Password: "pass with spaces"}},
		{Line: 8, Auth: config.RegistryAuth{Server: "localhost:5000", Username: "dev", Password: "tok-local"}},
		{Line: 9, Auth: config.RegistryAuth{Server: "ghcr.io", Username: "second-bot", Password: "tok-second"}},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries\n%+v\nwant\n%+v", entries, want)
	}
}

func TestParseReportsInvalidLinesWithoutTheirSecrets(t *testing.T) {
	body := strings.Join([]string{
		"ghcr.io only-a-user",
		"myregistry user secret-1",
		"ghcr.io/acme/app:1 user secret-2",
		"ghcr.io ghp_secret3 user",
		"ghcr.io/acme@sha256:abc user secret-4",
		"ghcr.io fine secret-5",
	}, "\n")
	entries, errs := Parse([]byte(body))
	if len(entries) != 1 || entries[0].Line != 6 {
		t.Fatalf("entries %+v: the valid line is kept", entries)
	}
	wantLines := []int{1, 2, 3, 4, 5}
	var lines []int
	for _, e := range errs {
		lines = append(lines, e.Line)
		msg := e.Error()
		if strings.Contains(msg, "secret") {
			t.Errorf("error %q echoes a secret", msg)
		}
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines %v, want %v (%v)", lines, wantLines, errs)
	}
	for i, want := range []string{"expected", "not a registry host", "tag or digest", "looks like a token", "tag or digest"} {
		if !strings.Contains(errs[i].Reason, want) {
			t.Errorf("line %d: %q lacks %q", errs[i].Line, errs[i].Reason, want)
		}
	}
}

func TestProjectDir(t *testing.T) {
	if got := ProjectDir(map[string]string{WorkingDirLabel: " /srv/app "}); got != "/srv/app" {
		t.Errorf("ProjectDir = %q", got)
	}
	if got := ProjectDir(map[string]string{"com.docker.compose.project": "app"}); got != "" {
		t.Errorf("ProjectDir without a working_dir = %q", got)
	}
}
