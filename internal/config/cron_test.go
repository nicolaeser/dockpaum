package config

import (
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestParseCron(t *testing.T) {
	const fieldHint = "expected 5 fields"
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "five fields", spec: "0 4 * * *"},
		{name: "six fields, seconds first", spec: "0 0 4 * * *"},
		{name: "five fields with day names", spec: "30 3 * * SUN"},
		{name: "surrounding and inner whitespace", spec: "  0   4\t* * *  "},
		{name: "descriptor", spec: "@daily"},
		{name: "@every with a Go duration", spec: "@every 90m"},
		{name: "@every in days", spec: "@every 1d"},
		{name: "@every in weeks", spec: "@every 2w"},
		{name: "CRON_TZ prefix", spec: "CRON_TZ=Europe/Berlin 0 4 * * *"},
		{name: "TZ prefix, six fields", spec: "TZ=UTC 0 0 4 * * *"},
		{name: "zone prefix on a descriptor", spec: "CRON_TZ=Europe/Berlin @every 1d"},
		{name: "four fields", spec: "0 4 * *", wantErr: fieldHint},
		{name: "seven fields", spec: "0 0 0 4 * * *", wantErr: fieldHint},
		{name: "empty", spec: "  ", wantErr: "empty spec"},
		{name: "out of range field", spec: "0 61 * * *", wantErr: "above maximum"},
		{name: "unknown descriptor", spec: "@fortnightly", wantErr: "unrecognized descriptor"},
		{name: "@every without a duration", spec: "@every", wantErr: "want @every <duration>"},
		{name: "@every below a second", spec: "@every 0s", wantErr: "at least 1s"},
		{name: "@every with an unknown unit", spec: "@every 1y", wantErr: "@every 1y"},
		{name: "zone prefix without a schedule", spec: "CRON_TZ=Europe/Berlin", wantErr: "needs a schedule"},
		{name: "zone prefix without a zone", spec: "TZ= 0 4 * * *", wantErr: "missing time zone"},
		{name: "unknown zone", spec: "CRON_TZ=Mars/Olympus 0 4 * * *", wantErr: "CRON_TZ: unknown time zone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, err := ParseCron(tt.spec)
			if tt.wantErr == "" {
				if err != nil || sched == nil {
					t.Fatalf("ParseCron(%q) = %v, %v; want a schedule", tt.spec, sched, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseCron(%q) succeeded, want an error containing %q", tt.spec, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseCron(%q) error %q does not contain %q", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr != fieldHint && strings.Contains(err.Error(), fieldHint) {
				t.Errorf("ParseCron(%q) error %q carries the field-count hint", tt.spec, err)
			}
		})
	}
}

func TestParseCronFiveAndSixFieldsAgree(t *testing.T) {
	from := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, pair := range [][2]string{
		{"0 4 * * *", "0 0 4 * * *"},
		{"*/15 * * * *", "0 */15 * * * *"},
		{"30 3 * * SUN", "0 30 3 * * SUN"},
		{"CRON_TZ=Europe/Berlin 0 4 * * *", "CRON_TZ=Europe/Berlin 0 0 4 * * *"},
	} {
		five, err := ParseCron(pair[0])
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", pair[0], err)
		}
		six, err := ParseCron(pair[1])
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", pair[1], err)
		}
		if a, b := five.Next(from), six.Next(from); !a.Equal(b) {
			t.Errorf("Next(%q) = %v, Next(%q) = %v; want the same instant", pair[0], a, pair[1], b)
		}
	}
}

func TestParseCronNextFireTimes(t *testing.T) {
	from := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		spec string
		want time.Time
	}{
		{"0 4 * * *", time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)},
		{"CRON_TZ=Europe/Berlin 0 4 * * *", time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC)},
		{"@every 1d", from.Add(24 * time.Hour)},
		{"@every 2w", from.Add(14 * 24 * time.Hour)},
	}
	for _, tt := range tests {
		sched, err := ParseCron(tt.spec)
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", tt.spec, err)
		}
		if got := sched.Next(from); !got.Equal(tt.want) {
			t.Errorf("ParseCron(%q).Next(%v) = %v, want %v", tt.spec, from, got, tt.want)
		}
	}
}

func TestCronParserIsParseCron(t *testing.T) {
	var p cron.ScheduleParser = CronParser{}
	if _, err := p.Parse("0 4 * * *"); err != nil {
		t.Errorf("CronParser rejected a five-field spec: %v", err)
	}
	if _, err := p.Parse("@every 1d"); err != nil {
		t.Errorf("CronParser rejected @every 1d: %v", err)
	}
	if _, err := p.Parse("0 4 * *"); err == nil {
		t.Error("CronParser accepted a four-field spec")
	}
}

func TestJobCronSpec(t *testing.T) {
	tests := []struct {
		j    JobConfig
		want string
	}{
		{JobConfig{Cron: "0 2 * * *"}, "0 2 * * *"},
		{JobConfig{Cron: "0 2 * * *", Timezone: "Europe/Berlin"}, "CRON_TZ=Europe/Berlin 0 2 * * *"},
		{JobConfig{Cron: "CRON_TZ=UTC 0 2 * * *", Timezone: "Europe/Berlin"}, "CRON_TZ=UTC 0 2 * * *"},
	}
	for _, tt := range tests {
		if got := tt.j.CronSpec(); got != tt.want {
			t.Errorf("CronSpec(%+v) = %q, want %q", tt.j, got, tt.want)
		}
	}
}

func TestParseCronRejectsSpecsThatNeverFire(t *testing.T) {
	for _, spec := range []string{"0 0 30 2 *", "0 0 0 31 4 *", "CRON_TZ=Europe/Berlin 0 4 31 11 *"} {
		if _, err := ParseCron(spec); err == nil || !strings.Contains(err.Error(), "never fires") {
			t.Errorf("ParseCron(%q) = %v, want a never-fires error", spec, err)
		}
	}
	if _, err := ParseCron("0 0 29 2 *"); err != nil {
		t.Errorf("ParseCron(Feb 29): %v", err)
	}
}
