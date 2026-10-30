package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

func newTestUpdater(t *testing.T, cfg *config.Config) (*Updater, *metrics.Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg.Hooks.Timeout == 0 {
		cfg.Hooks.Timeout = config.Duration(5 * time.Second)
	}
	al, err := audit.New(config.AuditConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	u, err := New(Deps{
		Clients:  &docker.Clients{},
		Notifier: notify.Build(nil, logger),
		Metrics:  reg,
		Logger:   logger,
		Config:   cfg,
		Audit:    al,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, reg
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestRunExecutesRunHooks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	u, _ := newTestUpdater(t, &config.Config{Hooks: config.HookConfig{
		PreRun:  []string{`printf 'pre %s\n' "$DOCKPAUM_POLICY" >> ` + out},
		PostRun: []string{`printf 'post %s %s\n' "$DOCKPAUM_POLICY" "$DOCKPAUM_UPDATED" >> ` + out},
	}})
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.RunPolicy(context.Background(), "database"); err != nil {
		t.Fatal(err)
	}
	want := "pre \npost  0\npre database\npost database 0\n"
	if got := read(t, out); got != want {
		t.Fatalf("hook output = %q, want %q", got, want)
	}
}

func TestRunAbortsWhenPreRunHookFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	u, reg := newTestUpdater(t, &config.Config{Hooks: config.HookConfig{
		PreRun:  []string{"exit 3"},
		PostRun: []string{"echo post > " + out},
	}})
	err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pre-run hook") {
		t.Fatalf("Run() = %v, want a pre-run hook error", err)
	}
	if read(t, out) != "" {
		t.Error("post_run must not run when pre_run failed")
	}
	if !strings.Contains(scrape(t, reg), "dockpaum_run_failures_total 1") {
		t.Error("a failed pre_run hook must count as a failed run")
	}
}

func TestDryRunSkipsRunHooksAndJobs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	u, reg := newTestUpdater(t, &config.Config{
		DryRun: true,
		Hooks:  config.HookConfig{PreRun: []string{"echo pre >> " + out}, PostRun: []string{"echo post >> " + out}},
		Jobs:   []config.JobConfig{{Name: "nightly", Cron: "@daily", Commands: []string{"echo job >> " + out}}},
	})
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.RunJob(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, out); got != "" {
		t.Fatalf("dry-run executed scripts: %q", got)
	}
	if !strings.Contains(scrape(t, reg), `dockpaum_job_runs_total{job="nightly",result="dry_run"} 1`) {
		t.Error("dry-run job skip should be counted")
	}
}

func TestRunJob(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	u, reg := newTestUpdater(t, &config.Config{Jobs: []config.JobConfig{
		{Name: "ok", Cron: "@daily", Commands: []string{`printf '%s' "$DOCKPAUM_JOB" > ` + out}, Timeout: config.Duration(5 * time.Second)},
		{Name: "bad", Cron: "@daily", Commands: []string{"exit 1"}, Timeout: config.Duration(5 * time.Second)},
	}})
	if err := u.RunJob(context.Background(), "ok"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, out); got != "ok" {
		t.Errorf("job output = %q", got)
	}
	if err := u.RunJob(context.Background(), "bad"); err == nil {
		t.Error("expected the failing job to return an error")
	}
	if err := u.RunJob(context.Background(), "unknown"); err == nil {
		t.Error("expected an error for an unknown job")
	}
	body := scrape(t, reg)
	for _, want := range []string{
		`dockpaum_job_runs_total{job="ok",result="ok"} 1`,
		`dockpaum_job_runs_total{job="bad",result="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestInScope(t *testing.T) {
	scheduled := policy.Resolved{PolicyID: "database", Scheduled: true}
	regular := policy.Resolved{PolicyID: "default"}
	gated := policy.Resolved{Skip: true, SkipReason: "dockpaum.enable=false"}
	unresolved := policy.Resolved{PolicyID: "databse", Skip: true, SkipReason: `dockpaum.policy="databse": unknown policy`}
	tests := []struct {
		name      string
		eff       policy.Resolved
		container string
		opts      RunOptions
		want      bool
	}{
		{"regular run covers unscheduled policy", regular, "web", RunOptions{}, true},
		{"regular run leaves scheduled policy alone", scheduled, "db", RunOptions{}, false},
		{"regular run keeps gated containers for auditing", gated, "x", RunOptions{}, true},
		{"regular run keeps unresolved labels for auditing", unresolved, "x", RunOptions{}, true},
		{"policy run covers its policy", scheduled, "db", RunOptions{Policy: "database"}, true},
		{"policy run ignores other policies", regular, "web", RunOptions{Policy: "database"}, false},
		{"policy run ignores gated containers", gated, "x", RunOptions{Policy: "database"}, false},
		{"policy run ignores a misspelled policy", unresolved, "x", RunOptions{Policy: "databse"}, false},
		{"policy run ignores other scheduled policies", policy.Resolved{PolicyID: "web", Scheduled: true}, "w", RunOptions{Policy: "database"}, false},
		{"explicit container covers a scheduled policy", scheduled, "db", RunOptions{Containers: []string{"db"}}, true},
		{"explicit container list excludes others", regular, "web", RunOptions{Containers: []string{"db"}}, false},
		{"container and policy must both match", regular, "web", RunOptions{Containers: []string{"web"}, Policy: "database"}, false},
		{"image request selects by repository", regular, "web", RunOptions{Images: []string{"example.com/app"}}, true},
		{"image request covers a scheduled policy", scheduled, "db", RunOptions{Images: []string{"example.com/app"}}, true},
		{"image request excludes other images", regular, "web", RunOptions{Images: []string{"example.com/other"}}, false},
		{"image request with a tag selects only that tag", regular, "web", RunOptions{Images: []string{"example.com/app:2"}}, false},
		{"image and container must both match", regular, "web", RunOptions{Images: []string{"example.com/app"}, Containers: []string{"db"}}, false},
		{"image and policy must both match", regular, "web", RunOptions{Images: []string{"example.com/app"}, Policy: "database"}, false},
	}
	for _, tt := range tests {
		if got := inScope(tt.eff, tt.container, "example.com/app:1", tt.opts); got != tt.want {
			t.Errorf("%s: inScope = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRunError(t *testing.T) {
	if err := runError(nil, 0); err != nil {
		t.Fatalf("clean run returned %v", err)
	}
	err := runError([]error{errors.New("docker host(s) unreachable: nas")}, 2)
	if err == nil || !strings.Contains(err.Error(), "unreachable: nas") || !strings.Contains(err.Error(), "2 container update(s) failed") {
		t.Fatalf("runError = %v", err)
	}
}

func TestRunRecordsSummary(t *testing.T) {
	u, _ := newTestUpdater(t, &config.Config{})
	if u.LastRun() != nil {
		t.Fatal("no run yet")
	}
	if err := u.RunWith(context.Background(), RunOptions{Trigger: "api", Containers: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	last := u.LastRun()
	if last == nil || last.Trigger != "api" || len(last.Containers) != 1 || last.Finished.Before(last.Started) {
		t.Fatalf("summary = %+v", last)
	}
}

func TestFindNewerTagFallsBackForNonVersionTags(t *testing.T) {
	u, _ := newTestUpdater(t, &config.Config{})
	for _, ref := range []string{"nginx:latest", "nginx", "ghcr.io/a/b:stable"} {
		_, _, outcome, _, err := u.findNewerTag(context.Background(), "run", ref, config.ModeMinor, registry.TagRules{})
		if err != nil || outcome != tagFallback {
			t.Errorf("findNewerTag(%s) = %v, %v; want fallback to tag-follow", ref, outcome, err)
		}
	}
}

func TestFindNewerTagExplainsTooShortVersions(t *testing.T) {
	u, _ := newTestUpdater(t, &config.Config{})
	_, _, outcome, reason, err := u.findNewerTag(context.Background(), "run", "postgres:16.2", config.ModePatch, registry.TagRules{})
	if err != nil || outcome != tagNone || !strings.Contains(reason, "use minor") {
		t.Fatalf("patch on 16.2 = %v %q %v; want an explicit skip reason", outcome, reason, err)
	}
}
