package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/robfig/cron/v3"
)

var cronFields = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type CronParser struct{}

func (CronParser) Parse(spec string) (cron.Schedule, error) { return ParseCron(spec) }

func ParseCron(spec string) (cron.Schedule, error) {
	zone, rest, err := splitCronZone(spec)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(rest)
	switch {
	case len(fields) == 0:
		return nil, errors.New("empty spec")
	case fields[0] == "@every":
		if len(fields) != 2 {
			return nil, fmt.Errorf("%q: want @every <duration>, e.g. @every 6h or @every 1d", rest)
		}
		d, err := ParseDuration(fields[1])
		if err != nil {
			return nil, fmt.Errorf("@every %s: %w", fields[1], err)
		}
		if d < time.Second {
			return nil, fmt.Errorf("@every %s: must be at least 1s", fields[1])
		}
		rest = "@every " + d.String()
	case strings.HasPrefix(fields[0], "@"):
		rest = strings.Join(fields, " ")
	default:
		if n := len(fields); n != 5 && n != 6 {
			return nil, fmt.Errorf("expected 5 fields (minute hour day-of-month month day-of-week) or 6 with seconds first, found %d in %q", n, rest)
		}
		rest = strings.Join(fields, " ")
	}
	plain := rest
	if zone != "" {
		rest = "CRON_TZ=" + zone + " " + rest
	}
	sched, err := cronFields.Parse(rest)
	if err != nil {
		return nil, err
	}
	if sched.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("%q never fires: no date matches it", plain)
	}
	return sched, nil
}

func splitCronZone(spec string) (zone, rest string, err error) {
	spec = strings.TrimSpace(spec)
	for _, prefix := range []string{"CRON_TZ=", "TZ="} {
		if !strings.HasPrefix(spec, prefix) {
			continue
		}
		name := strings.TrimSuffix(prefix, "=")
		end := strings.IndexFunc(spec, unicode.IsSpace)
		if end < 0 {
			return "", "", fmt.Errorf("%s needs a schedule after the zone, e.g. %s%s 0 4 * * *", name, prefix, spec[len(prefix):])
		}
		zone = spec[len(prefix):end]
		if zone == "" {
			return "", "", fmt.Errorf("%s: missing time zone name", name)
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return "", "", fmt.Errorf("%s: %w", name, err)
		}
		return zone, strings.TrimSpace(spec[end:]), nil
	}
	return "", spec, nil
}

func withTimezone(spec, tz string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || tz == "" || strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return spec
	}
	return "CRON_TZ=" + tz + " " + spec
}
