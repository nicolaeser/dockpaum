package cli

import (
	"context"
	"io"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	dlog "github.com/nicolaeser/dockpaum/internal/log"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

func runNotifyTest(inv *invocation, stdout, stderr io.Writer) int {
	cfg, err := config.Validate(inv.configPath)
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return exitUsage
	}
	if len(cfg.Notify) == 0 {
		printf(stderr, "notify-test: no notifiers configured in %s (notify)\n", configSource(cfg))
		return exitFailure
	}
	logger := dlog.NewTo(stderr, config.LogConfig{Level: "warn", Format: "text"})
	notifier := notify.Build(cfg.Notify, logger)
	notifier.SetInstance(instanceName(cfg))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	failed := 0
	for _, r := range notifier.Test(ctx, notify.Event{}, inv.notifyNames...) {
		label := r.Name
		if r.Type != "" && r.Type != r.Name {
			label += " (" + r.Type + ")"
		}
		if r.Err != nil {
			failed++
			printf(stdout, "FAIL  %s: %s\n", cell(label), cell(r.Err.Error()))
			continue
		}
		printf(stdout, "ok    %s\n", cell(label))
	}
	if failed > 0 {
		printf(stderr, "notify-test: %d notifier%s failed\n", failed, plural(failed))
		return exitFailure
	}
	return exitOK
}

func instanceName(cfg *config.Config) string {
	if name := strings.TrimSpace(cfg.Instance); name != "" {
		return name
	}
	return updater.DefaultInstance()
}

func startedEvent(cfg *config.Config, version string, next, now time.Time) notify.Event {
	if cfg.RunOnStart() {
		next = now
	}
	hosts := make([]string, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		hosts = append(hosts, h.Name)
	}
	return notify.Event{
		Type: notify.EventStarted,
		At:   now,
		Startup: &notify.StartupInfo{
			Version: version, Hosts: hosts, Schedule: scheduleSummary(cfg.Schedule), NextRun: next, DryRun: cfg.DryRun,
		},
	}
}

func stoppingEvent(version string, now time.Time) notify.Event {
	return notify.Event{Type: notify.EventStopping, At: now, Reason: "shutdown signal received", Startup: &notify.StartupInfo{Version: version}}
}

func scheduleSummary(s config.ScheduleConfig) string {
	var out string
	if s.Cron != "" {
		out = "cron " + s.Cron
	} else {
		iv := s.Interval.Std()
		if iv <= 0 {
			iv = config.DefaultInterval
		}
		out = "every " + shortDuration(iv)
	}
	if s.Timezone != "" {
		out += " (" + s.Timezone + ")"
	}
	return out
}

func shortDuration(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	s := d.String()
	s = strings.TrimSuffix(s, "m0s")
	if strings.HasSuffix(d.String(), "m0s") {
		s += "m"
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
