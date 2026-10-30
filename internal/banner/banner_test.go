package banner

import (
	"os"
	"strings"
	"testing"
)

func TestPrintWritesBanner(t *testing.T) {
	var sb strings.Builder
	Print(&sb, Options{
		Big:     "DOCKPAUM",
		Tagline: "automated docker container updates",
		Version: "v1.2.3",
		Commit:  "abc123",
		Extra:   "2 hosts",
		Stops:   [][3]int{{250, 204, 100}, {214, 138, 31}},
		Color:   false,
	})
	out := sb.String()
	if out == "" {
		t.Fatal("banner produced no output")
	}
	if !strings.Contains(out, "v1.2.3") || !strings.Contains(out, "2 hosts") {
		t.Errorf("banner metadata missing version/extra:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("color=false but ANSI escapes present")
	}
}

func TestPrintColorEmitsANSI(t *testing.T) {
	var sb strings.Builder
	Print(&sb, Options{Big: "DP", Stops: [][3]int{{1, 2, 3}, {4, 5, 6}}, Color: true})
	if !strings.Contains(sb.String(), "\x1b[") {
		t.Error("color=true should emit ANSI escape sequences")
	}
}

func TestColorEnabledRespectsNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	if !ColorEnabled() {
		t.Error("ColorEnabled should be true when NO_COLOR is empty")
	}
	t.Setenv("NO_COLOR", "1")
	if ColorEnabled() {
		t.Error("ColorEnabled should be false when NO_COLOR is set")
	}
}

func TestColorEnabledRespectsDumbTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if ColorEnabled() {
		t.Error("ColorEnabled should be false for TERM=dumb")
	}
	t.Setenv("TERM", "xterm-256color")
	if !ColorEnabled() {
		t.Error("ColorEnabled should be true for a capable terminal")
	}
}

func TestColorForNeedsTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var sb strings.Builder
	if ColorFor(&sb) {
		t.Error("a buffer is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if ColorFor(f) || IsTerminal(f) {
		t.Error("a regular file is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = r.Close(), w.Close() }()
	if ColorFor(w) {
		t.Error("a pipe (container log stream) is not a terminal")
	}
	if IsTerminal(nil) {
		t.Error("nil is not a terminal")
	}
}

func TestBuildMetaVersionPrefix(t *testing.T) {
	cases := map[string]string{
		"1.2.3":  "v1.2.3",
		"v1.2.3": "v1.2.3",
		"dev":    "dev",
	}
	for in, want := range cases {
		if got := buildMeta(Options{Version: in}); got != want {
			t.Errorf("buildMeta(Version=%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMetaSkipsPlaceholders(t *testing.T) {
	got := buildMeta(Options{Version: "dev", Commit: "none", Date: "unknown", Extra: "1 host"})
	if got != "dev  ·  1 host" {
		t.Errorf("buildMeta with placeholders = %q", got)
	}
	got = buildMeta(Options{Version: "1.0.0", Commit: "0123456789", Date: "2026-10-01", Extra: "2 hosts"})
	if got != "v1.0.0  ·  0123456  ·  2026-10-01  ·  2 hosts" {
		t.Errorf("buildMeta = %q", got)
	}
}
