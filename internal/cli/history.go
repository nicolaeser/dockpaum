package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
)

func runHistory(inv *invocation, stdout, stderr io.Writer, now time.Time) int {
	since, err := parseSince(inv.history.since, now)
	if err != nil {
		printf(stderr, "history: %v\n", err)
		return exitUsage
	}
	dir := inv.history.dir
	if dir == "" {
		cfg, err := config.Validate(inv.configPath)
		if err != nil {
			printf(stderr, "config: %s\n", configErr(err))
			return exitUsage
		}
		if !cfg.Audit.IsEnabled() {
			printf(stderr, "history: audit is disabled in %s (audit.enabled), so no decisions are recorded\n", inv.configPath)
			return exitFailure
		}
		dir = cfg.Audit.Dir
	}

	res, err := audit.Read(dir, audit.Query{Containers: inv.history.containers, Since: since, Limit: inv.history.limit})
	if errors.Is(err, fs.ErrNotExist) {
		printf(stderr, "history: no audit directory at %s (is the audit volume mounted? use --dir to read another one)\n", dir)
		return exitFailure
	}
	if err != nil {
		printf(stderr, "history: %v\n", err)
		return exitFailure
	}
	if res.Malformed > 0 {
		printf(stderr, "warning: skipped %d malformed audit line%s\n", res.Malformed, plural(res.Malformed))
	}

	if inv.json {
		recs := res.Records
		if recs == nil {
			recs = []audit.Record{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(recs); err != nil {
			printf(stderr, "history: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	if len(res.Records) == 0 {
		printf(stderr, "no matching audit records in %s\n", dir)
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	printf(tw, "TIME\tHOST\tCONTAINER\tDECISION\tIMAGE\tCHANGE\tDETAIL\n")
	for _, r := range res.Records {
		detail := r.Error
		if detail == "" {
			detail = r.Reason
		}
		printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.At.Local().Format("2006-01-02 15:04:05 MST"),
			cell(r.Host), cell(r.Container), cell(string(r.Decision)), cell(r.Image), cell(change(r)), cell(detail))
	}
	if err := tw.Flush(); err != nil {
		printf(stderr, "history: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if strings.Trim(strings.TrimLeft(s, "+-"), "0123456789.") == "" {
		return time.Time{}, fmt.Errorf("--since %q: add a unit (24h, 7d) or give a date (2026-10-01)", s)
	}
	if d, err := config.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since %q: the duration must be positive", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: use a duration (24h, 7d) or a time (2026-10-01, 2026-10-01T08:00:00Z)", s)
}

func change(r audit.Record) string {
	switch {
	case r.OldTag != "" && r.NewTag != "" && r.OldTag != r.NewTag:
		return r.OldTag + " -> " + r.NewTag
	case r.OldDigest != "" && r.NewDigest != "" && r.OldDigest != r.NewDigest:
		return shortDigest(r.OldDigest) + " -> " + shortDigest(r.NewDigest)
	case r.NewTag != "":
		return r.NewTag
	}
	return ""
}

func shortDigest(d string) string {
	if _, hex, ok := strings.Cut(d, ":"); ok {
		d = hex
	}
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

func cell(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if s == "" {
		return "-"
	}
	return s
}
