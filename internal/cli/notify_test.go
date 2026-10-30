package cli

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/notify"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestScheduleSummary(t *testing.T) {
	cases := []struct {
		s    config.ScheduleConfig
		want string
	}{
		{config.ScheduleConfig{Cron: "0 4 * * *", Timezone: "Europe/Berlin"}, "cron 0 4 * * * (Europe/Berlin)"},
		{config.ScheduleConfig{Interval: config.Duration(6 * time.Hour), Timezone: "UTC"}, "every 6h (UTC)"},
		{config.ScheduleConfig{Interval: config.Duration(90 * time.Minute)}, "every 1h30m"},
		{config.ScheduleConfig{Interval: config.Duration(48 * time.Hour)}, "every 2d"},
		{config.ScheduleConfig{}, "every 5m"},
	}
	for _, c := range cases {
		if got := scheduleSummary(c.s); got != c.want {
			t.Errorf("scheduleSummary(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:              "30s",
		5 * time.Minute:               "5m",
		time.Hour:                     "1h",
		time.Hour + 30*time.Minute:    "1h30m",
		time.Hour + 30*time.Second:    "1h0m30s",
		24 * time.Hour:                "1d",
		36 * time.Hour:                "36h",
		2*time.Minute + 5*time.Second: "2m5s",
		1500 * time.Millisecond:       "1.5s",
		14 * 24 * time.Hour:           "14d",
		25*time.Hour + 30*time.Minute: "25h30m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestStoppingEvent(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	ev := stoppingEvent("v1.2.3", now)
	if ev.Type != notify.EventStopping || !ev.At.Equal(now) || ev.Startup == nil || ev.Startup.Version != "v1.2.3" || ev.Reason == "" {
		t.Errorf("stoppingEvent = %+v", ev)
	}
}

func TestStartedEvent(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	next := now.Add(20 * time.Hour)
	off := false
	cfg := &config.Config{
		DryRun:   true,
		Hosts:    []config.HostConfig{{Name: "local"}, {Name: "edge"}},
		Schedule: config.ScheduleConfig{Cron: "0 4 * * *", Timezone: "UTC", RunOnStart: &off},
	}
	ev := startedEvent(cfg, "v1.2.3", next, now)
	if ev.Type != notify.EventStarted || !ev.At.Equal(now) || ev.Startup == nil {
		t.Fatalf("startedEvent = %+v", ev)
	}
	s := ev.Startup
	if s.Version != "v1.2.3" || len(s.Hosts) != 2 || s.Hosts[1] != "edge" || s.Schedule != "cron 0 4 * * * (UTC)" ||
		!s.NextRun.Equal(next) || !s.DryRun {
		t.Errorf("startup info = %+v", s)
	}
	on := true
	cfg.Schedule.RunOnStart = &on
	if ev := startedEvent(cfg, "v1.2.3", next, now); !ev.Startup.NextRun.Equal(now) {
		t.Errorf("next run with run_on_start = %s, want now", ev.Startup.NextRun)
	}
}

func TestInstanceName(t *testing.T) {
	if got := instanceName(&config.Config{Instance: " edge-1 "}); got != "edge-1" {
		t.Errorf("instanceName = %q, want edge-1", got)
	}
	if got := instanceName(&config.Config{}); got == "" {
		t.Error("instanceName without instance must fall back to the host name")
	}
}
