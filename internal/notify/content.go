package notify

import (
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type row struct {
	label, value string
	link         string
	stat, item   bool
}

func eventRows(ev Event) []row {
	var rows []row
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, row{label: k, value: v})
		}
	}
	if ev.Type == EventTest {
		add("Test", "delivery to this channel works")
	}
	add("Instance", ev.Instance)
	add("Host", ev.Host)
	add("Container", ev.Container)
	add("Image", ev.Image)
	add("Tag", tagChange(ev.OldTag, ev.NewTag))
	add("Digest", digestChange(ev.OldDigest, ev.NewDigest))
	add("Policy", ev.Policy)
	add("Reason", ev.Reason)
	if ev.Err != nil {
		add("Error", ev.Err.Error())
	}
	if l := httpLink(ev.ReleaseURL); l != "" {
		rows = append(rows, row{label: "Release notes", value: l, link: l})
	}
	if s := ev.Startup; s != nil {
		add("Version", s.Version)
		add("Hosts", strings.Join(s.Hosts, ", "))
		if s.DryRun {
			add("Mode", "observe (dry-run): updates are reported, not applied")
		} else {
			add("Mode", "live: updates are applied")
		}
		add("Schedule", s.Schedule)
		if !s.NextRun.IsZero() {
			add("Next run", s.NextRun.Format("2006-01-02 15:04 MST"))
		}
	}
	if s := ev.RunStats; s != nil {
		stat := func(k string, v string) { rows = append(rows, row{label: k, value: v, stat: true}) }
		stat("Scanned", itoa(s.Scanned))
		stat("Updated", itoa(s.Updated))
		stat("Skipped", itoa(s.Skipped))
		if s.Failed > 0 {
			stat("Failed", itoa(s.Failed))
		}
		stat("Took", s.Took.Round(time.Millisecond).String())
		for _, it := range s.Items {
			rows = append(rows, row{label: "Change", value: it.Line(), link: httpLink(it.ReleaseURL), item: true})
		}
	}
	add("Run", ev.RunID)
	return rows
}

func plainBody(ev Event, links bool) string {
	var sb strings.Builder
	var stats []string
	flush := func() {
		if len(stats) > 0 {
			sb.WriteString(strings.Join(stats, ", ") + "\n")
			stats = nil
		}
	}
	for _, r := range eventRows(ev) {
		if r.stat {
			stats = append(stats, strings.ToLower(r.label)+": "+r.value)
			continue
		}
		flush()
		switch {
		case r.item:
			sb.WriteString("• " + r.value)
			if links && r.link != "" {
				sb.WriteString(" · " + r.link)
			}
			sb.WriteString("\n")
		case r.link != "" && !links:
		default:
			sb.WriteString(strings.ToLower(r.label) + ": " + r.value + "\n")
		}
	}
	flush()
	return sb.String()
}

func cardTitle(ev Event) string {
	switch ev.Type {
	case EventUpdateSuccess:
		return "✅  " + ev.Container + " · updated"
	case EventUpdateFailure:
		return "🔥  " + ev.Container + " · update failed"
	case EventAvailable:
		return "🆕  " + ev.Container + " · update available"
	case EventNoUpdate:
		return "⏭️  " + ev.Container + " · skipped"
	case EventDenied:
		return "⛔  " + ev.Container + " · update denied"
	case EventRolledBack:
		return "↩️  " + ev.Container + " · rolled back"
	case EventRecovered:
		return "🩹  " + ev.Container + " · recovered"
	case EventCheckFailed:
		return "⚠️  " + ev.Container + " · check failed"
	case EventSelfUpdateStarted:
		return "🔄  Self-update started"
	case EventSelfUpdateSuccess:
		return "✅  Self-update completed"
	case EventSelfUpdateFailure:
		return "🔥  Self-update failed"
	case EventStopping:
		return "🛑  dockpaum stopping"
	case EventRunStarted:
		return "▶️  Run started"
	case EventRunCompleted:
		if ev.Err != nil {
			return "🏁  Run failed"
		}
		return "🏁  Run completed"
	case EventStarted:
		return "🚀  dockpaum started"
	case EventTest:
		return "🔔  Test notification"
	}
	return "dockpaum: " + ev.Type
}

type releaseLink struct{ label, url string }

func releaseLinks(ev Event) []releaseLink {
	var out []releaseLink
	if l := httpLink(ev.ReleaseURL); l != "" {
		out = append(out, releaseLink{"Release notes", l})
	}
	if s := ev.RunStats; s != nil {
		for _, it := range s.Items {
			if l := httpLink(it.ReleaseURL); l != "" {
				label := "Release notes: " + it.Container
				if it.NewTag != "" {
					label += " " + it.NewTag
				}
				out = append(out, releaseLink{label, l})
			}
		}
	}
	return out
}

func isProblem(ev Event) bool {
	return failureEvent(ev.Type) || (ev.Type == EventRunCompleted && (ev.Err != nil || (ev.RunStats != nil && ev.RunStats.Failed > 0)))
}

func tagChange(oldT, newT string) string {
	if newT == "" {
		newT = oldT
	}
	if oldT == "" {
		oldT = newT
	}
	if oldT != newT {
		return oldT + " → " + newT
	}
	return oldT
}

func digestChange(oldD, newD string) string {
	o, n := short(oldD), short(newD)
	switch {
	case o == "" && n == "":
		return ""
	case o == "":
		return "→ " + n
	case n == "" || o == n:
		return o
	}
	return o + " → " + n
}

func httpLink(raw string) string {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "<>\"'`|\\{}^ ") {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	ellipsis := "…"
	if n < len(ellipsis) {
		ellipsis = ""
	}
	cut := max(n-len(ellipsis), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

func joinLines(lines []string, limit int) string {
	var b strings.Builder
	for i, l := range lines {
		more := ""
		if rest := len(lines) - i - 1; rest > 0 {
			more = "\n… and " + itoa(rest) + " more"
		}
		need := len(l) + len(more)
		if b.Len() > 0 {
			need++
		}
		if b.Len()+need > limit {
			if b.Len() == 0 {
				if avail := limit - len(more); avail > len("…") {
					return truncate(l, avail) + more
				}
				return truncate(l, limit)
			}
			return b.String() + "\n… and " + itoa(len(lines)-i) + " more"
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l)
	}
	return b.String()
}
