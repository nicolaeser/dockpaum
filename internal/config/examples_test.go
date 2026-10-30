package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExampleConfigsParse(t *testing.T) {
	isolateEnv(t)
	for _, kv := range [][2]string{
		{"APPROVAL_TOKEN", "test-approval-token"},
		{"REG_USER", "test-user"},
		{"REG_PASS", "test-pass"},
		{"SLACK_WEBHOOK", "https://hooks.slack.com/services/T000/B000/TEST"},
		{"DISCORD_WEBHOOK", "https://discord.com/api/webhooks/000/test"},
		{"SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T000/B000/TEST"},
		{"UPDATE_API_TOKEN", "test-api-token"},
		{"HTTP_API_TOKEN", "test-api-token"},
		{"NTFY_TOPIC", "test-topic"},
	} {
		t.Setenv(kv[0], kv[1])
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "examples", "*.yml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no example configs found in ../../docs/examples — did the directory move?")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			if _, err := decode(raw, file); err != nil {
				t.Errorf("decode %s: %v", file, err)
			}
		})
	}

	t.Run("embedded-default", func(t *testing.T) {
		raw := DefaultConfig()
		if len(raw) == 0 {
			t.Fatal("DefaultConfig() returned empty content")
		}
		if _, err := decode(raw, "embedded default.yml"); err != nil {
			t.Errorf("decode embedded default config: %v", err)
		}
	})
}
