package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"api":      writeSecret(t, dir, "api", "api-token\n"),
		"read":     writeSecret(t, dir, "read", "  read-token  \n"),
		"hook":     writeSecret(t, dir, "hook", "https://hooks.slack.com/services/T000/B000/HOOKSECRET\n"),
		"gotify":   writeSecret(t, dir, "gotify", "gotify-token"),
		"smtp":     writeSecret(t, dir, "smtp", "mail-pass\n"),
		"auth":     writeSecret(t, dir, "auth", "Bearer hook-auth\n"),
		"approval": writeSecret(t, dir, "approval", "Bearer approval-auth"),
	}
	raw := `
http:
  token_file: ` + files["api"] + `
  read_token_file: ` + files["read"] + `
notify:
  - type: slack
    url_file: ` + files["hook"] + `
  - type: gotify
    url: https://gotify.example.com
    token_file: ` + files["gotify"] + `
  - type: email
    to: [ops@example.com]
    smtp: { host: mail.example.com, port: 587, username: bot, password_file: ` + files["smtp"] + ` }
  - type: webhook
    url: https://hooks.example.com/x
    headers: { X-Team: ops }
    header_files: { Authorization: ` + files["auth"] + ` }
update:
  approval:
    url: https://approve.example.com/check
    header_files: { Authorization: ` + files["approval"] + ` }
`
	cfg, err := decodeEnv([]byte(raw), "secrets", envMap{})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ got, want string }{
		"http.token":                {cfg.HTTP.Token, "api-token"},
		"http.read_token":           {cfg.HTTP.ReadToken, "read-token"},
		"notify[0].url":             {cfg.Notify[0].URL, "https://hooks.slack.com/services/T000/B000/HOOKSECRET"},
		"notify[1].token":           {cfg.Notify[1].Token, "gotify-token"},
		"notify[2].smtp.password":   {cfg.Notify[2].SMTP.Password, "mail-pass"},
		"notify[3] Authorization":   {cfg.Notify[3].Headers["Authorization"], "Bearer hook-auth"},
		"notify[3] inline header":   {cfg.Notify[3].Headers["X-Team"], "ops"},
		"approval Authorization":    {cfg.Update.Approval.Headers["Authorization"], "Bearer approval-auth"},
		"smtp file path is kept":    {cfg.Notify[2].SMTP.PasswordFile, files["smtp"]},
		"notify url_file path kept": {cfg.Notify[0].URLFile, files["hook"]},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}

	red := cfg.Redacted()
	out := strings.Join([]string{
		red.HTTP.Token, red.HTTP.ReadToken, red.Notify[0].URL, red.Notify[1].Token,
		red.Notify[2].SMTP.Password, red.Notify[3].Headers["Authorization"], red.Update.Approval.Headers["Authorization"],
	}, "\n")
	for _, secret := range []string{"api-token", "read-token", "HOOKSECRET", "gotify-token", "mail-pass", "hook-auth", "approval-auth"} {
		if strings.Contains(out, secret) {
			t.Errorf("Redacted() leaks %q:\n%s", secret, out)
		}
	}
	if red.HTTP.TokenFile != files["api"] || red.Notify[1].TokenFile != files["gotify"] {
		t.Error("Redacted() must keep the *_file paths, they are not secrets")
	}
	if cfg.Update.Approval.HeaderFiles["Authorization"] != files["approval"] {
		t.Error("header_files paths must stay for --print-config")
	}
}

func TestSecretFileErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeSecret(t, dir, "good", "s3cr3t-content\n")
	empty := writeSecret(t, dir, "empty", " \n")
	big := writeSecret(t, dir, "big", strings.Repeat("x", maxSecretFileSize+1))
	missing := filepath.Join(dir, "missing")
	tests := []struct {
		name, yaml, want string
	}{
		{"value and file", "http:\n  token: x\n  token_file: " + good + "\n", "http.token: set either token or token_file, not both"},
		{"missing file", "http:\n  token_file: " + missing + "\n", "http.token_file: open " + missing},
		{"empty file", "http:\n  read_token_file: " + empty + "\n", "http.read_token_file: " + empty + " is empty"},
		{"oversized file", "http:\n  token_file: " + big + "\n", "exceeds 65536 bytes"},
		{"notifier url and url_file", "notify:\n  - type: slack\n    url: https://x\n    url_file: " + good + "\n", "notify[0].url: set either url or url_file, not both"},
		{"notifier token file missing", "notify:\n  - type: gotify\n    url: https://g\n    token_file: " + missing + "\n", "notify[0].token_file"},
		{"smtp password and file", "notify:\n  - type: email\n    to: [a@b.c]\n    smtp: { host: h, password: p, password_file: " + good + " }\n", "notify[0].smtp.password: set either password or password_file"},
		{"header inline and file", "notify:\n  - type: webhook\n    url: https://x\n    headers: { authorization: a }\n    header_files: { Authorization: " + good + " }\n", "header Authorization is set in both headers and header_files"},
		{"approval header file missing", "update:\n  approval:\n    url: https://a\n    header_files: { X-Key: " + missing + " }\n", "update.approval.header_files.X-Key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeEnv([]byte(tt.yaml), "file", envMap{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cr3t-content") {
				t.Errorf("error leaks the file content: %v", err)
			}
		})
	}
}
