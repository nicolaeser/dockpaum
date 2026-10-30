package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
)

func seedAudit(t *testing.T, dir string, recs ...audit.Record) {
	t.Helper()
	on := true
	l, err := audit.New(config.AuditConfig{Enabled: &on, Dir: dir, Retention: config.Duration(100 * 365 * 24 * time.Hour)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		l.Append(r)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryCommand(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	seedAudit(t, dir,
		audit.Record{At: now.Add(-72 * time.Hour), Host: "local", Container: "web", Image: "nginx:1.26", Decision: audit.DecisionUpdated, OldTag: "1.26.1", NewTag: "1.26.2"},
		audit.Record{At: now.Add(-2 * time.Hour), Host: "local", Container: "db", Image: "postgres:16", Decision: audit.DecisionGatedAge, Reason: "image younger than 1h"},
		audit.Record{At: now.Add(-1 * time.Hour), Host: "local", Container: "web", Image: "nginx:latest", Decision: audit.DecisionFailed,
			OldDigest: "sha256:aaaaaaaaaaaaaaaaaaaa", NewDigest: "sha256:bbbbbbbbbbbbbbbbbbbb", Error: "start failed:\nexit 1\x1b[31m"},
	)

	code, out, errOut := execute(t, "history", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "TIME") {
		t.Fatalf("table:\n%s", out)
	}
	for _, want := range []string{"1.26.1 -> 1.26.2", "aaaaaaaaaaaa -> bbbbbbbbbbbb", "image younger than 1h", "start failed: exit 1 [31m"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("control characters from error text reached the terminal")
	}

	code, out, _ = execute(t, "history", "--dir", dir, "--container", "web", "--since", "24h")
	if code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 1 || !strings.Contains(out, "failed") {
		t.Errorf("filtered table (exit %d):\n%s", code, out)
	}

	code, out, _ = execute(t, "history", "--dir", dir, "--json", "--limit", "2")
	var recs []audit.Record
	if code != 0 || json.Unmarshal([]byte(out), &recs) != nil || len(recs) != 2 || recs[0].Container != "db" || recs[1].Container != "web" {
		t.Errorf("json (exit %d): %s", code, out)
	}

	code, out, _ = execute(t, "history", "--dir", dir, "--json", "--container", "nope")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty json (exit %d): %q", code, out)
	}
	code, out, errOut = execute(t, "history", "--dir", dir, "--container", "nope")
	if code != 0 || out != "" || !strings.Contains(errOut, "no matching audit records") {
		t.Errorf("empty table (exit %d): %q %q", code, out, errOut)
	}
}

func TestHistoryFromConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	seedAudit(t, dir, audit.Record{Host: "local", Container: "web", Decision: audit.DecisionDryRun})
	cfg := writeConfig(t, "audit: {enabled: true, dir: "+dir+"}\n")
	if code, out, errOut := execute(t, "history", "--config", cfg); code != 0 || !strings.Contains(out, "dry-run") {
		t.Errorf("history via config: exit %d\n%s%s", code, out, errOut)
	}

	off := writeConfig(t, "audit: {enabled: false}\n")
	if code, _, errOut := execute(t, "history", "--config", off); code != exitFailure || !strings.Contains(errOut, "audit is disabled") {
		t.Errorf("disabled audit: exit %d %s", code, errOut)
	}
	missing := writeConfig(t, "audit: {enabled: true, dir: "+filepath.Join(t.TempDir(), "gone")+"}\n")
	if code, _, errOut := execute(t, "history", "--config", missing); code != exitFailure || !strings.Contains(errOut, "no audit directory") {
		t.Errorf("missing dir: exit %d %s", code, errOut)
	}
	bad := writeConfig(t, "schedule: {cron: \"nonsense\"}\n")
	if code, _, _ := execute(t, "history", "--config", bad); code != exitUsage {
		t.Errorf("invalid config: exit %d, want %d", code, exitUsage)
	}
	absent := filepath.Join(t.TempDir(), "absent.yml")
	_, _, _ = execute(t, "history", "--config", absent)
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Error("history created the config file")
	}
	if code, _, errOut := execute(t, "history", "--dir", dir, "--since", "yesterday"); code != exitUsage || !strings.Contains(errOut, "--since") {
		t.Errorf("bad --since: exit %d %s", code, errOut)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{"", time.Time{}, false},
		{"24h", now.Add(-24 * time.Hour), false},
		{"7d", now.Add(-7 * 24 * time.Hour), false},
		{"2w", now.Add(-14 * 24 * time.Hour), false},
		{"2026-10-01T08:00:00Z", time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), false},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), false},
		{"0s", time.Time{}, true},
		{"-5h", time.Time{}, true},
		{"yesterday", time.Time{}, true},
		{"2026", time.Time{}, true},
		{"300", time.Time{}, true},
		{"1.5", time.Time{}, true},
		{"-2026", time.Time{}, true},
		{"90m", now.Add(-90 * time.Minute), false},
	}
	for _, c := range cases {
		got, err := parseSince(c.in, now)
		if (err != nil) != c.err || (err == nil && !got.Equal(c.want)) {
			t.Errorf("parseSince(%q) = %v, %v; want %v (error %v)", c.in, got, err, c.want, c.err)
		}
	}
}

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"":            "-",
		"  ":          "-",
		"plain":       "plain",
		"a\nb\tc":     "a b c",
		"\x1b[31mred": "[31mred",
	} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
}
