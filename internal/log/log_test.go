package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestNewLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "warning", "error", "", "bogus"} {
		for _, format := range []string{"json", "text", ""} {
			l := New(config.LogConfig{Level: lvl, Format: format})
			if l == nil {
				t.Fatalf("New returned nil for level=%q format=%q", lvl, format)
			}
		}
	}
}

func TestNewIsUsable(t *testing.T) {
	l := New(config.LogConfig{Level: "debug", Format: "json"})
	l.Info("hello", "k", "v")
	l.Debug("dbg")
	l.Warn("warn")
	l.Error("err", "err", "boom")
}

func TestNewToFormats(t *testing.T) {
	cases := []struct {
		format string
		json   bool
	}{
		{"", true},
		{"json", true},
		{"JSON", true},
		{"text", false},
		{"Text", false},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		NewTo(&buf, config.LogConfig{Format: c.format}).Info("hello", "k", "v")
		out := strings.TrimSpace(buf.String())
		var doc map[string]any
		isJSON := json.Unmarshal([]byte(out), &doc) == nil
		if isJSON != c.json {
			t.Errorf("format %q: output %q, json=%v want %v", c.format, out, isJSON, c.json)
		}
		if !strings.Contains(out, "dockpaum") || !strings.Contains(out, "hello") {
			t.Errorf("format %q: output %q lacks app or message", c.format, out)
		}
		if IsText(config.LogConfig{Format: c.format}) == c.json {
			t.Errorf("IsText(%q) = %v", c.format, !c.json)
		}
	}
}

func TestNewToLevel(t *testing.T) {
	var buf bytes.Buffer
	l := NewTo(&buf, config.LogConfig{Level: "warn"})
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("level warn output = %q", buf.String())
	}
}
