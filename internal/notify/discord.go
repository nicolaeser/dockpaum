package notify

import (
	"context"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type discord struct{ url string }

func newDiscord(c config.NotifierConfig) *discord { return &discord{url: c.URL} }

const (
	discordGreen   = 0x3BA55D
	discordRed     = 0xED4245
	discordBlurple = 0x5865F2
	discordAmber   = 0xFAA61A
	discordGrey    = 0x949BA4
)

const (
	discordTitleMax  = 256
	discordFieldMax  = 1024
	discordFooterMax = 2048
)

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

func (d *discord) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, d.url, discordPayload(ev), nil)
}

func discordPayload(ev Event) map[string]any {
	embed := map[string]any{
		"title":     truncate(discordTitle(ev), discordTitleMax),
		"color":     discordColor(ev),
		"timestamp": ev.At.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if l := httpLink(ev.ReleaseURL); l != "" {
		embed["url"] = l
	}
	if ev.Host != "" {
		embed["author"] = map[string]any{"name": truncate("🖥  "+ev.Host, discordTitleMax)}
	}
	if f := discordFooter(ev); f != "" {
		embed["footer"] = map[string]any{"text": truncate(f, discordFooterMax)}
	}
	if f := discordFields(ev); len(f) > 0 {
		embed["fields"] = f
	}
	switch {
	case ev.Err != nil:
		embed["description"] = "```\n" + fenceSafe(truncate(ev.Err.Error(), 1000)) + "\n```"
	case ev.Type == EventRunStarted:
		embed["description"] = "Scanning containers for available updates…"
	case ev.Type == EventTest:
		embed["description"] = "Delivery to this channel works."
	}
	return map[string]any{
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

func discordTitle(ev Event) string { return cardTitle(ev) }

func discordColor(ev Event) int {
	switch {
	case failureEvent(ev.Type):
		return discordRed
	}
	switch ev.Type {
	case EventNoUpdate, EventAvailable:
		return discordGrey
	case EventRunStarted, EventStarted, EventTest, EventRecovered, EventSelfUpdateStarted, EventStopping:
		return discordBlurple
	case EventRunCompleted:
		if ev.Err != nil || (ev.RunStats != nil && ev.RunStats.Failed > 0) {
			return discordAmber
		}
		return discordBlurple
	}
	return discordGreen
}

func discordFooter(ev Event) string {
	var parts []string
	if ev.Instance != "" {
		parts = append(parts, ev.Instance)
	}
	if ev.RunID != "" {
		parts = append(parts, "run "+ev.RunID)
	}
	if len(parts) == 0 {
		return ""
	}
	return "dockpaum · " + strings.Join(parts, " · ")
}

const discordLineMax = discordFieldMax / 2

func discordFields(ev Event) []discordField {
	var f []discordField
	add := func(name, val string, inline bool) {
		if val != "" {
			f = append(f, discordField{Name: name, Value: truncate(val, discordFieldMax), Inline: inline})
		}
	}
	codeField := func(s string) string { return codeWithin(s, discordFieldMax) }
	add("Image", codeField(ev.Image), false)
	if t := tagChange(ev.OldTag, ev.NewTag); t != "" {
		if ev.OldTag != "" && ev.NewTag != "" && ev.OldTag != ev.NewTag {
			const arrow = " → "
			half := (discordFieldMax - len(arrow)) / 2
			add("Tag", codeWithin(ev.OldTag, half)+arrow+codeWithin(ev.NewTag, half), true)
		} else {
			add("Tag", codeField(t), true)
		}
	}
	add("Digest", codeField(digestChange(ev.OldDigest, ev.NewDigest)), true)
	add("Policy", codeField(ev.Policy), true)
	add("Reason", discordEscapeWithin(ev.Reason, discordFieldMax), false)
	if l := httpLink(ev.ReleaseURL); l != "" {
		if link := discordLink("Release notes", l); len(link) <= discordFieldMax {
			add("Release notes", link, false)
		}
	}
	if s := ev.Startup; s != nil {
		add("Version", codeField(s.Version), true)
		add("Hosts", codeField(strings.Join(s.Hosts, ", ")), true)
		mode := "live"
		if s.DryRun {
			mode = "observe (dry-run)"
		}
		add("Mode", mode, true)
		add("Schedule", codeField(s.Schedule), true)
		if !s.NextRun.IsZero() {
			add("Next run", code(s.NextRun.Format("2006-01-02 15:04 MST")), true)
		}
	}
	if s := ev.RunStats; s != nil {
		add("Scanned", code(itoa(s.Scanned)), true)
		add("Updated", code(itoa(s.Updated)), true)
		add("Skipped", code(itoa(s.Skipped)), true)
		if s.Failed > 0 {
			add("Failed", code(itoa(s.Failed)), true)
		}
		add("Took", code(s.Took.Round(time.Millisecond).String()), true)
		if len(s.Items) > 0 {
			lines := make([]string, 0, len(s.Items))
			for _, it := range s.Items {
				lines = append(lines, discordItemLine(it))
			}
			add("Changes", joinLines(lines, discordFieldMax), false)
		}
	}
	return f
}

func discordItemLine(it RunItem) string {
	line := codeWithin(it.Line(), discordLineMax)
	if l := httpLink(it.ReleaseURL); l != "" {
		if withLink := line + " · " + discordLink("notes", l); len(withLink) <= discordLineMax {
			line = withLink
		}
	}
	return line
}

func discordLink(text, u string) string {
	u = strings.NewReplacer("(", "%28", ")", "%29").Replace(u)
	return "[" + text + "](" + u + ")"
}
