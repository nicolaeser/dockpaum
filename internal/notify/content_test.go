package notify

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestFenceSafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"one ` tick", "one ` tick"},
		{"``", "`\u200b`"},
		{"a```b", "a`\u200b`\u200b`b"},
		{"````", "`\u200b`\u200b`\u200b`"},
		{"ä``ö", "ä`\u200b`ö"},
	}
	for _, tc := range cases {
		got := fenceSafe(tc.in)
		if got != tc.want {
			t.Errorf("fenceSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "``") {
			t.Errorf("fenceSafe(%q) keeps adjacent backticks", tc.in)
		}
		if strings.ReplaceAll(got, "\u200b", "") != tc.in {
			t.Errorf("fenceSafe(%q) changed the visible text", tc.in)
		}
	}
}

func TestSlackAndDiscordEscape(t *testing.T) {
	if got := slackEscape("a & <b> <!here>"); got != "a &amp; &lt;b&gt; &lt;!here&gt;" {
		t.Errorf("slackEscape = %q", got)
	}
	cases := []struct{ in, want string }{
		{"web-1.2_x", `web-1.2\_x`},
		{"**x** __y__ ~~z~~ ||s||", `\*\*x\*\* \_\_y\_\_ \~\~z\~\~ \|\|s\|\|`},
		{"> quote # head [a](b) @everyone <@1>", `\> quote \# head \[a\](b) \@everyone \<\@1\>`},
		{`back\slash`, `back\\slash`},
	}
	for _, tc := range cases {
		if got := discordEscape(tc.in); got != tc.want {
			t.Errorf("discordEscape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if code("") != "" || code("1.2") != "`1.2`" || code("a`b") != "a\\`b" {
		t.Errorf("code: %q %q %q", code(""), code("1.2"), code("a`b"))
	}
}

func TestHTTPLink(t *testing.T) {
	ok := []string{
		"https://github.com/acme/web/releases/tag/v1.3.0",
		"http://gitlab.example/acme/web/-/releases/v1.3.0",
		"https://example.org/notes?a=1&b=2#frag",
		"https://example.org/a_(b)",
	}
	for _, raw := range ok {
		if httpLink(raw) != raw {
			t.Errorf("rejected %q", raw)
		}
	}
	bad := []string{
		"", "javascript:alert(1)", "data:text/html,x", "ftp://example.org/x", "//example.org/x",
		"https:///nohost", "https://user:pass@example.org/", "https://example.org/a b",
		`https://example.org/"onmouseover="x`, "https://example.org/<x>", "https://example.org/a|b",
		"https://example.org/\r\nx", "https://example.org/`x`", "https://example.org/" + strings.Repeat("a", 2100),
	}
	for _, raw := range bad {
		if httpLink(raw) != "" {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestJoinLines(t *testing.T) {
	lines := []string{"aaaa", "bbbb", "cccc", "dddd"}
	if got := joinLines(lines, 100); got != "aaaa\nbbbb\ncccc\ndddd" {
		t.Errorf("fits: %q", got)
	}
	got := joinLines(lines, 27)
	if len(got) > 27 || !strings.HasSuffix(got, "more") || !strings.HasPrefix(got, "aaaa\n") {
		t.Errorf("cut: %q (%d bytes)", got, len(got))
	}
	for limit := 0; limit < 40; limit++ {
		if got := joinLines(lines, limit); len(got) > limit {
			t.Errorf("limit %d: %q is %d bytes", limit, got, len(got))
		}
	}
	if got := joinLines([]string{strings.Repeat("x", 50), "y"}, 30); len(got) > 30 || !strings.HasSuffix(got, "1 more") {
		t.Errorf("long first line: %q", got)
	}
}

func TestChangesAndLine(t *testing.T) {
	cases := []struct{ oldD, newD, want string }{
		{"", "", ""},
		{"sha256:0123456789abcdef", "sha256:fedcba9876543210", "0123456789ab → fedcba987654"},
		{"", "sha256:fedcba9876543210", "→ fedcba987654"},
		{"sha256:0123456789abcdef", "", "0123456789ab"},
		{"sha256:0123456789abcdef", "sha256:0123456789abcdef", "0123456789ab"},
	}
	for _, tc := range cases {
		if got := digestChange(tc.oldD, tc.newD); got != tc.want {
			t.Errorf("digestChange(%q, %q) = %q, want %q", tc.oldD, tc.newD, got, tc.want)
		}
	}
	if tagChange("1.2", "1.3") != "1.2 → 1.3" || tagChange("", "1.3") != "1.3" || tagChange("1.2", "") != "1.2" || tagChange("", "") != "" {
		t.Error("tagChange")
	}

	lines := []struct {
		it   RunItem
		want string
	}{
		{RunItem{Host: "local", Container: "web", OldTag: "1.2", NewTag: "1.3", Result: "updated"}, "local/web: 1.2 → 1.3 (updated)"},
		{RunItem{Container: "web", Image: "nginx:latest", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb", Result: "monitor-only"}, "web: nginx:latest @ aaaa → bbbb (monitor-only)"},
		{RunItem{Host: "local", Container: "db", Result: "check-failed"}, "local/db (check-failed)"},
	}
	for _, tc := range lines {
		if got := tc.it.Line(); got != tc.want {
			t.Errorf("Line() = %q, want %q", got, tc.want)
		}
	}
}

func TestPlainBody(t *testing.T) {
	ev := Event{
		Type: EventRunCompleted, Instance: "nas", Host: "local", RunID: "r-1",
		ReleaseURL: releaseURL,
		RunStats: &RunStats{Scanned: 4, Updated: 1, Skipped: 2, Took: 1500 * time.Millisecond, Items: []RunItem{
			{Host: "local", Container: "web", OldTag: "1", NewTag: "2", Result: "updated", ReleaseURL: releaseURL},
		}},
	}
	want := "instance: nas\nhost: local\nrelease notes: " + releaseURL + "\n" +
		"scanned: 4, updated: 1, skipped: 2, took: 1.5s\n" +
		"• local/web: 1 → 2 (updated) · " + releaseURL + "\n" +
		"run: r-1\n"
	if got := formatBody(ev); got != want {
		t.Errorf("formatBody =\n%s\nwant\n%s", got, want)
	}
	if got := plainBody(ev, false); strings.Contains(got, "https://") || !strings.Contains(got, "• local/web: 1 → 2 (updated)\n") {
		t.Errorf("plainBody without links =\n%s", got)
	}
}

func TestStanding(t *testing.T) {
	for result, want := range map[string]bool{
		"monitor-only": true, "dry-run": true, "approval-denied": true, "gated-window": true, "gated-min-age": true,
		"updated": false, "failed": false, "rolled-back": false, "check-failed": false, "recovered": false, "": false,
	} {
		if standing(result) != want {
			t.Errorf("standing(%q) = %v", result, !want)
		}
	}
}

func TestKnownEvents(t *testing.T) {
	for _, e := range []string{
		EventUpdateSuccess, EventUpdateFailure, EventAvailable, EventNoUpdate, EventDenied, EventRolledBack, EventRecovered,
		EventCheckFailed, EventSelfUpdateStarted, EventSelfUpdateSuccess, EventSelfUpdateFailure,
		EventRunStarted, EventRunCompleted, "dockpaum.started", "dockpaum.stopping", "notify.test",
	} {
		if !knownEvent(e) {
			t.Errorf("%s is not known", e)
		}
	}
	if knownEvent("update.rejected") || knownEvent("*") {
		t.Error("unexpected known event")
	}
}

func TestKnownEventsMatchTheConfiguration(t *testing.T) {
	raw, err := config.JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var find func(v any) []string
	find = func(v any) []string {
		switch x := v.(type) {
		case map[string]any:
			if enum, ok := x["enum"].([]any); ok && slices.Contains(enum, any(EventUpdateSuccess)) {
				var out []string
				for _, e := range enum {
					if s, ok := e.(string); ok && s != "*" {
						out = append(out, s)
					}
				}
				return out
			}
			for _, k := range slices.Sorted(maps.Keys(x)) {
				if got := find(x[k]); got != nil {
					return got
				}
			}
		case []any:
			for _, e := range x {
				if got := find(e); got != nil {
					return got
				}
			}
		}
		return nil
	}
	if got := find(schema); !slices.Equal(got, knownEvents) {
		t.Fatalf("notify[].events accepts %v, the notifiers know %v", got, knownEvents)
	}
}

func TestSeverityOfEvents(t *testing.T) {
	for _, typ := range knownEvents {
		ev := Event{Type: typ}
		want := typ == EventUpdateFailure || typ == EventDenied || typ == EventRolledBack || typ == EventCheckFailed || typ == EventSelfUpdateFailure
		if isProblem(ev) != want {
			t.Errorf("isProblem(%s) = %v", typ, !want)
		}
		if got := discordColor(ev); (got == discordRed) != want {
			t.Errorf("discord colour of %s = %#x", typ, got)
		}
		if got := emailColor(ev); (got == "#d6453d") != want {
			t.Errorf("email colour of %s = %s", typ, got)
		}
		if want && appriseType(ev) != "failure" {
			t.Errorf("apprise type of %s = %s", typ, appriseType(ev))
		}
	}
	for _, typ := range []string{EventAvailable, EventNoUpdate, EventRecovered} {
		if got := appriseType(Event{Type: typ}); got != "info" {
			t.Errorf("apprise type of %s = %s, want info", typ, got)
		}
	}
}
