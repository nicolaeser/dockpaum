package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withWatchtower(env envMap) envMap {
	out := envMap{envWatchtowerEnv: "true"}
	for k, v := range env {
		out[k] = v
	}
	return out
}

func watchtowerOnly(t *testing.T, env envMap) *Config {
	t.Helper()
	return envOnly(t, withWatchtower(env))
}

func TestWatchtowerEnv(t *testing.T) {
	cfg := watchtowerOnly(t, envMap{
		"WATCHTOWER_SCHEDULE":            "0 0 4 * * *",
		"WATCHTOWER_CLEANUP":             "true",
		"WATCHTOWER_LABEL_ENABLE":        "true",
		"WATCHTOWER_INCLUDE_STOPPED":     "true",
		"WATCHTOWER_ROLLING_RESTART":     "true",
		"WATCHTOWER_TIMEOUT":             "45s",
		"WATCHTOWER_SCOPE":               "prod",
		"WATCHTOWER_DISABLE_CONTAINERS":  "db, /cache,db",
		"WATCHTOWER_LIFECYCLE_HOOKS":     "true",
		"WATCHTOWER_UPDATE_ON_START":     "true",
		"WATCHTOWER_COOLDOWN_DELAY":      "1M",
		"WATCHTOWER_TRACE":               "true",
		"WATCHTOWER_LOG_LEVEL":           "error",
		"WATCHTOWER_LOG_FORMAT":          "LogFmt",
		"WATCHTOWER_HTTP_API_TOKEN":      "wt-token",
		"WATCHTOWER_HTTP_API_UPDATE":     "true",
		"WATCHTOWER_HTTP_API_METRICS":    "true",
		"WATCHTOWER_HTTP_API_PORT":       "8081",
		"WATCHTOWER_NOTIFICATION_URL":    "discord://tok@123 ntfy://ntfy.sh/wt",
		"WATCHTOWER_NOTIFICATIONS_LEVEL": "info",
		"WATCHTOWER_RUN_ONCE":            "true",
	})
	def := policyByID(t, cfg, "default")
	checks := map[string]struct{ got, want any }{
		"cron":            {cfg.Schedule.Cron, "0 0 4 * * *"},
		"cleanup":         {cfg.Update.CleanupImages, true},
		"label_enable":    {cfg.Filters.LabelEnable, true},
		"include_stopped": {cfg.Filters.IncludeStopped, true},
		"strategy":        {cfg.Update.Strategy, StrategyRolling},
		"stop_timeout":    {cfg.Update.StopTimeout, Duration(45 * time.Second)},
		"scope":           {cfg.Filters.Scope, "prod"},
		"exclude_names":   {strings.Join(cfg.Filters.ExcludeNames, ","), "db,cache"},
		"label_hooks":     {cfg.Hooks.LabelHooks, true},
		"run_on_start":    {cfg.Schedule.RunOnStart != nil && *cfg.Schedule.RunOnStart, true},
		"cooldown":        {def.MinImageAge, Duration(30 * 24 * time.Hour)},
		"trace wins":      {cfg.Log.Level, "debug"},
		"log format":      {cfg.Log.Format, "text"},
		"token":           {cfg.HTTP.Token, "wt-token"},
		"update_api":      {cfg.HTTP.UpdateAPI, true},
		"http enabled":    {cfg.HTTP.IsEnabled(), true},
		"listen":          {cfg.HTTP.ListenAddr(), ":8081"},
		"notifiers":       {len(cfg.Notify), 2},
		"events":          {strings.Join(cfg.Notify[0].Events, ","), "update.success,update.failure,dockpaum.started"},
		"run once":        {cfg.RunOnce(), true},
		"still observing": {cfg.DryRun, true},
		"label":           {cfg.notifyLabel(1), "WATCHTOWER_NOTIFICATION_URL[1]"},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	w := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{
		"DOCKPAUM_WATCHTOWER_ENV=true keeps dry_run on",
		"WATCHTOWER_HTTP_API_UPDATE without WATCHTOWER_HTTP_API_PERIODIC_POLLS",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
}

func TestWatchtowerEnvPrecedence(t *testing.T) {
	raw := "dry_run: false\nschedule:\n  cron: \"0 3 * * *\"\nupdate:\n  cleanup_images: true\n"
	cfg, err := decodeEnv([]byte(raw), "file", withWatchtower(envMap{
		"WATCHTOWER_POLL_INTERVAL": "300",
		"WATCHTOWER_CLEANUP":       "false",
		"WATCHTOWER_MONITOR_ONLY":  "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Schedule.Interval != Duration(5*time.Minute) || cfg.Schedule.Cron != "" {
		t.Errorf("schedule %+v: WATCHTOWER_POLL_INTERVAL replaces the file's cron", cfg.Schedule)
	}
	if cfg.Update.CleanupImages {
		t.Error("WATCHTOWER_CLEANUP=false must override the file")
	}
	if !cfg.DryRun {
		t.Error("DOCKPAUM_WATCHTOWER_ENV=true keeps dry_run on, even over the file's dry_run: false")
	}
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "DOCKPAUM_WATCHTOWER_ENV=true keeps dry_run on") ||
		!strings.Contains(w, "the config file's dry_run: false is overridden until then") {
		t.Errorf("the observe-mode note must say the file's dry_run was overridden:\n%s", w)
	}

	cfg, err = decodeEnv([]byte("dry_run: false\n"), "file", withWatchtower(envMap{"WATCHTOWER_MONITOR_ONLY": "true"}))
	if err != nil || !cfg.DryRun {
		t.Errorf("WATCHTOWER_MONITOR_ONLY=true: dry_run %v, err %v", cfg != nil && cfg.DryRun, err)
	}
	cfg, err = decodeEnv([]byte(raw), "file", withWatchtower(envMap{"WATCHTOWER_CLEANUP": "true", "DOCKPAUM_DRY_RUN": "false"}))
	if err != nil || cfg.DryRun {
		t.Fatalf("DOCKPAUM_DRY_RUN=false must apply updates: dry_run %v, err %v", cfg != nil && cfg.DryRun, err)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "dry_run on") {
		t.Errorf("no observe-mode note once DOCKPAUM_DRY_RUN decides:\n%s", w)
	}
	cfg = watchtowerOnly(t, envMap{"WATCHTOWER_MONITOR_ONLY": "true", "DOCKPAUM_DRY_RUN": "false", "WATCHTOWER_SCHEDULE": "@daily", "DOCKPAUM_SCHEDULE": "2h"})
	if cfg.DryRun || cfg.Schedule.Interval != Duration(2*time.Hour) || cfg.Schedule.Cron != "" {
		t.Errorf("DOCKPAUM_* must win over WATCHTOWER_*: dry_run %v, schedule %+v", cfg.DryRun, cfg.Schedule)
	}
	var keys []string
	for _, o := range cfg.EnvOverrides() {
		keys = append(keys, o.Var+"→"+o.Key)
	}
	if got := strings.Join(keys, " "); strings.Contains(got, "WATCHTOWER_SCHEDULE") || !strings.Contains(got, "DOCKPAUM_SCHEDULE→schedule") ||
		strings.Contains(got, "WATCHTOWER_MONITOR_ONLY") || !strings.Contains(got, "DOCKPAUM_DRY_RUN→dry_run") {
		t.Errorf("EnvOverrides() = %s: a key set by both lists the DOCKPAUM_* variable only", got)
	}
}

func TestWatchtowerLeftoversDoNotOverrideTheFile(t *testing.T) {
	const file = "dry_run: false\nfilters:\n  label_enable: true\nschedule:\n  cron: \"0 0 4 * * *\"\nhttp:\n  token: file-token\nhooks:\n  label_hooks: false\n"
	leftovers := envMap{
		"WATCHTOWER_LABEL_ENABLE":    "false",
		"WATCHTOWER_POLL_INTERVAL":   "300",
		"WATCHTOWER_HTTP_API_TOKEN":  "old",
		"WATCHTOWER_CLEANUP":         "true",
		"WATCHTOWER_LIFECYCLE_HOOKS": "true",
		"REPO_USER":                  "u",
		"REPO_PASS":                  "p",
	}

	cfg, err := decodeEnv([]byte(file), "file", leftovers)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DryRun || !cfg.Filters.LabelEnable || cfg.Schedule.Cron != "0 0 4 * * *" || cfg.Schedule.Interval != 0 ||
		cfg.HTTP.Token != "file-token" || cfg.Update.CleanupImages || cfg.Hooks.LabelHooks || len(cfg.Registry) != 0 ||
		len(cfg.EnvOverrides()) != 0 {
		t.Errorf("leftover WATCHTOWER_* variables changed the file's settings: %+v", cfg.EnvOverrides())
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(w, "ignoring WATCHTOWER_CLEANUP, WATCHTOWER_HTTP_API_TOKEN, WATCHTOWER_LABEL_ENABLE, WATCHTOWER_LIFECYCLE_HOOKS, WATCHTOWER_POLL_INTERVAL, REPO_USER, REPO_PASS: set DOCKPAUM_WATCHTOWER_ENV=true to read the Watchtower settings") {
		t.Errorf("the ignored variables must be listed once:\n%s", w)
	}
	if strings.Contains(w, "old") {
		t.Errorf("values must not be echoed:\n%s", w)
	}

	cfg, err = decodeEnv([]byte(file), "file", withWatchtower(leftovers))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun {
		t.Error("an opted-in migration must not go live over the file's dry_run: false")
	}
	w = strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{
		"WATCHTOWER_LABEL_ENABLE replaces filters.label_enable from the config file",
		"WATCHTOWER_POLL_INTERVAL replaces schedule.cron from the config file",
		"WATCHTOWER_HTTP_API_TOKEN replaces http.token from the config file",
		"WATCHTOWER_LIFECYCLE_HOOKS replaces hooks.label_hooks from the config file",
		"the config file's dry_run: false is overridden until then",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "WATCHTOWER_CLEANUP replaces") {
		t.Errorf("the file does not set cleanup_images, so nothing of it is replaced:\n%s", w)
	}

	cfg, err = decodeEnv([]byte(file), "file", withWatchtower(envMap{"WATCHTOWER_LABEL_ENABLE": "false", "DOCKPAUM_LABEL_ENABLE": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "WATCHTOWER_LABEL_ENABLE replaces") || !cfg.Filters.LabelEnable {
		t.Errorf("label_enable %v, warnings:\n%s", cfg.Filters.LabelEnable, w)
	}
}

func TestWatchtowerReplacedFileKeys(t *testing.T) {
	tokenFile := writeSecret(t, t.TempDir(), "token", "file-token\n")
	tests := []struct {
		name, yaml string
		env        envMap
		want       string
	}{
		{"cooldown on the default policy", "update:\n  policies:\n    - id: db\n      mode: minor\n    - id: default\n      min_image_age: 2h\n",
			envMap{"WATCHTOWER_COOLDOWN_DELAY": "3d"}, "WATCHTOWER_COOLDOWN_DELAY replaces update.policies[1].min_image_age"},
		{"cooldown on a map-form default policy", "update:\n  policies:\n    default:\n      min_image_age: 2h\n",
			envMap{"WATCHTOWER_COOLDOWN_DELAY": "3d"}, "WATCHTOWER_COOLDOWN_DELAY replaces update.policies[0].min_image_age"},
		{"cooldown with an unset gate", "update:\n  policies:\n    - id: default\n      mode: minor\n",
			envMap{"WATCHTOWER_COOLDOWN_DELAY": "3d"}, ""},
		{"token file", "http:\n  token_file: " + tokenFile + "\n", envMap{"WATCHTOWER_HTTP_API_TOKEN": "x"}, "WATCHTOWER_HTTP_API_TOKEN replaces http.token_file"},
		{"api port over addr", "http:\n  addr: 127.0.0.1:8080\n", envMap{"WATCHTOWER_HTTP_API_PORT": "9000"}, "WATCHTOWER_HTTP_API_PORT replaces http.addr"},
		{"schedule shorthand", "schedule: 6h\n", envMap{"WATCHTOWER_SCHEDULE": "@daily"}, "WATCHTOWER_SCHEDULE replaces schedule.interval"},
		{"http switched off", "http:\n  enabled: false\n", envMap{"WATCHTOWER_HTTP_API_METRICS": "true"}, "WATCHTOWER_HTTP_API_METRICS replaces http.enabled"},
		{"lists only gain entries", "filters:\n  exclude_names: [db]\n", envMap{"WATCHTOWER_DISABLE_CONTAINERS": "cache"}, ""},
		{"run once is no file key", "{}", envMap{"WATCHTOWER_RUN_ONCE": "true"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := tt.env["WATCHTOWER_TIMEOUT"]; !ok {
				tt.env["WATCHTOWER_TIMEOUT"] = "10s"
			}
			cfg, err := decodeEnv([]byte(tt.yaml), "file", withWatchtower(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			var replaced []string
			for _, w := range cfg.Warnings() {
				if strings.Contains(w, "from the config file") {
					replaced = append(replaced, w)
				}
			}
			switch {
			case tt.want == "" && len(replaced) > 0:
				t.Errorf("unexpected warnings %q", replaced)
			case tt.want != "" && (len(replaced) != 1 || !strings.Contains(replaced[0], tt.want)):
				t.Errorf("warnings %q, want one with %q", replaced, tt.want)
			}
		})
	}
}

func TestWatchtowerEnvCanBeIgnored(t *testing.T) {
	env := envMap{"WATCHTOWER_CLEANUP": "true", "WATCHTOWER_BOGUS": "1", "REPO_USER": "u"}
	cfg := envOnly(t, env)
	if cfg.Update.CleanupImages || len(cfg.EnvOverrides()) != 0 || len(cfg.Registry) != 0 || cfg.Schedule.Interval.Std() != DefaultInterval {
		t.Errorf("without DOCKPAUM_WATCHTOWER_ENV=true, WATCHTOWER_* and REPO_* are ignored: %+v", cfg.EnvOverrides())
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(w, "ignoring WATCHTOWER_BOGUS, WATCHTOWER_CLEANUP, REPO_USER: set DOCKPAUM_WATCHTOWER_ENV=true") {
		t.Errorf("the ignored variables must be listed once:\n%s", w)
	}
	if strings.Contains(w, "has no dockpaum equivalent") || strings.Contains(w, "DOCKPAUM_WATCHTOWER_ENV=true keeps dry_run on") || strings.Contains(w, "24h") {
		t.Errorf("ignored variables get no per-variable warnings:\n%s", w)
	}
	env[envWatchtowerEnv] = "false"
	cfg = envOnly(t, env)
	if cfg.Update.CleanupImages || len(cfg.EnvOverrides()) != 0 || len(cfg.Registry) != 0 {
		t.Errorf("DOCKPAUM_WATCHTOWER_ENV=false ignores them: %v", cfg.EnvOverrides())
	}
	if w := cfg.Warnings(); len(w) != 1 || w[0] != "DOCKPAUM_WATCHTOWER_ENV=false: ignoring WATCHTOWER_BOGUS, WATCHTOWER_CLEANUP, REPO_USER" {
		t.Errorf("the switched-off variables must still be listed once: %q", w)
	}
	for _, e := range []envMap{{}, {envWatchtowerEnv: "false"}} {
		if cfg := envOnly(t, e); len(cfg.Warnings()) != 0 {
			t.Errorf("%v: nothing to ignore, nothing to say: %v", e, cfg.Warnings())
		}
	}
}

func TestWatchtowerUnsupportedVarsWarn(t *testing.T) {
	cfg := watchtowerOnly(t, envMap{
		"WATCHTOWER_REVIVE_STOPPED":                      "true",
		"WATCHTOWER_NO_RESTART":                          "false",
		"WATCHTOWER_NO_PULL":                             "true",
		"WATCHTOWER_INCLUDE_RESTARTING":                  "true",
		"WATCHTOWER_NOTIFICATION_TEMPLATE":               "{{.}}",
		"WATCHTOWER_GIT_ENABLE":                          "true",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER":           "mail.example.com",
		"WATCHTOWER_NOTIFICATION_GOTIFY_TLS_SKIP_VERIFY": "true",
	})
	w := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{
		"WATCHTOWER_REVIVE_STOPPED is not supported and is ignored: stopped containers stay stopped",
		"WATCHTOWER_NO_PULL is not supported and is ignored: there is no global switch; label the containers dockpaum.no-pull=true",
		"WATCHTOWER_INCLUDE_RESTARTING is not supported on its own",
		"WATCHTOWER_NOTIFICATION_TEMPLATE is not supported and is ignored",
		"WATCHTOWER_GIT_ENABLE is set but has no dockpaum equivalent",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER is ignored: WATCHTOWER_NOTIFICATIONS does not list email",
		"WATCHTOWER_NOTIFICATION_GOTIFY_TLS_SKIP_VERIFY is ignored: WATCHTOWER_NOTIFICATIONS does not list gotify",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "WATCHTOWER_NO_RESTART") {
		t.Errorf("a switched-off setting must not warn:\n%s", w)
	}
	if !strings.Contains(w, "DOCKPAUM_WATCHTOWER_ENV=true keeps dry_run on") {
		t.Errorf("an opted-in migration observes and says so:\n%s", w)
	}
}

func TestWatchtowerLegacyNotifications(t *testing.T) {
	dir := t.TempDir()
	pass := writeSecret(t, dir, "smtp", "mail-pass\n")
	gotifyToken := writeSecret(t, dir, "gotify", "AbCdEf\n")
	cfg := watchtowerOnly(t, envMap{
		"WATCHTOWER_NOTIFICATIONS":                      "email slack,gotify msteams shoutrrr",
		"WATCHTOWER_NOTIFICATION_EMAIL_FROM":            "wt@example.com",
		"WATCHTOWER_NOTIFICATION_EMAIL_TO":              "ops@example.com,dev@example.com",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER":          "mail.example.com",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER_PORT":     "587",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER_USER":     "bot",
		"WATCHTOWER_NOTIFICATION_EMAIL_SERVER_PASSWORD": pass,
		"WATCHTOWER_NOTIFICATION_SLACK_HOOK_URL":        "https://hooks.slack.com/services/T/B/X",
		"WATCHTOWER_NOTIFICATION_SLACK_CHANNEL":         "#ops",
		"WATCHTOWER_NOTIFICATION_GOTIFY_URL":            "https://gotify.example.com",
		"WATCHTOWER_NOTIFICATION_GOTIFY_TOKEN":          gotifyToken,
		"WATCHTOWER_NOTIFICATION_MSTEAMS_HOOK_URL":      "https://prod-00.westus.logic.azure.com/workflows/abc",
		"WATCHTOWER_NOTIFICATION_REPORT":                "true",
		"WATCHTOWER_NO_STARTUP_MESSAGE":                 "true",
	})
	if len(cfg.Notify) != 4 {
		t.Fatalf("notify = %+v, want email, slack, gotify and teams", cfg.Notify)
	}
	email, slack, gotify, teams := cfg.Notify[0], cfg.Notify[1], cfg.Notify[2], cfg.Notify[3]
	if email.Type != "email" || email.From != "wt@example.com" || strings.Join(email.To, ",") != "ops@example.com,dev@example.com" ||
		email.SMTP.Host != "mail.example.com" || email.SMTP.Port != 587 || email.SMTP.Username != "bot" || email.SMTP.Password != "mail-pass" {
		t.Errorf("email = %+v %+v", email, email.SMTP)
	}
	if slack.Type != "slack" || slack.Channel != "#ops" || gotify.Token != "AbCdEf" || teams.Type != "teams" {
		t.Errorf("slack %+v, gotify %+v, teams %+v", slack, gotify, teams)
	}
	for i, n := range cfg.Notify {
		if strings.Join(n.Events, ",") != "run.completed" {
			t.Errorf("notify[%d].events = %v: a report is the run digest, without the startup message", i, n.Events)
		}
	}
	if cfg.notifyLabel(0) != "WATCHTOWER_NOTIFICATIONS (email)" {
		t.Errorf("label %q", cfg.notifyLabel(0))
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "EMAIL") || strings.Contains(w, "GOTIFY") {
		t.Errorf("consumed variables must not warn:\n%s", w)
	}
}

func TestWatchtowerNotifyEvents(t *testing.T) {
	tests := []struct {
		env  envMap
		want string
	}{
		{envMap{}, "update.success,update.failure,dockpaum.started"},
		{envMap{"WATCHTOWER_NOTIFICATIONS_LEVEL": "warn"}, "update.failure,dockpaum.started"},
		{envMap{"WATCHTOWER_NOTIFICATIONS_LEVEL": "debug", "WATCHTOWER_NO_STARTUP_MESSAGE": "true"}, "update.success,update.failure"},
		{envMap{"WATCHTOWER_NOTIFICATION_REPORT": "true", "WATCHTOWER_NOTIFICATIONS_LEVEL": "error"}, "run.completed,dockpaum.started"},
		{envMap{"WATCHTOWER_NOTIFICATIONS_LEVEL": "error", "DOCKPAUM_NOTIFY_EVENTS": "*"}, "*"},
	}
	for _, tt := range tests {
		tt.env["WATCHTOWER_NOTIFICATION_URL"] = "discord://tok@1"
		cfg := watchtowerOnly(t, tt.env)
		if got := strings.Join(cfg.Notify[0].Events, ","); got != tt.want {
			t.Errorf("%v: events %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestWatchtowerFileValues(t *testing.T) {
	dir := t.TempDir()
	token := writeSecret(t, dir, "token", "file-token\n")
	urls := writeSecret(t, dir, "urls", "discord://tok@1\n\nntfy://ntfy.sh/t\n")
	cfg := watchtowerOnly(t, envMap{
		"WATCHTOWER_HTTP_API_TOKEN":   token,
		"WATCHTOWER_NOTIFICATION_URL": urls + " gotify://g.example.com/tok",
		"REPO_USER":                   "bot",
		"REPO_PASS":                   token,
	})
	if cfg.HTTP.Token != "file-token" || len(cfg.Notify) != 3 || cfg.Registry[0].Password != "file-token" {
		t.Errorf("token %q, notify %d, registry %+v", cfg.HTTP.Token, len(cfg.Notify), cfg.Registry)
	}
	_, err := decodeEnv(DefaultConfig(), embeddedSource, withWatchtower(envMap{"WATCHTOWER_HTTP_API_TOKEN": filepath.Join(dir, "missing")}))
	if err == nil || !strings.Contains(err.Error(), "WATCHTOWER_HTTP_API_TOKEN: open") {
		t.Errorf("a missing secret file must not become the token: %v", err)
	}
}

func TestWatchtowerRegistryCredentials(t *testing.T) {
	const file = "registry:\n  - server: ghcr.io\n    username: g\n    password: g\n  - server: docker.io/acme\n    username: a\n    password: a\n  - server: https://index.docker.io\n    username: file\n    password: f\n"
	cfg, err := decodeEnv([]byte(file), "file", withWatchtower(envMap{"REPO_USER": "env", "REPO_PASS": "e"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Registry) != 4 || cfg.Registry[0].Username != "env" || cfg.Registry[0].Server != "docker.io" {
		t.Errorf("registry = %+v: REPO_* comes first so it wins on docker.io", cfg.Registry)
	}
	w := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{
		"REPO_USER/REPO_PASS apply to Docker Hub (docker.io) only",
		"REPO_USER/REPO_PASS replace the Docker Hub credentials of registry[2] (https://index.docker.io) from the config file",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "registry[0]") || strings.Contains(w, "registry[1]") {
		t.Errorf("other registries and the more specific docker.io/acme keep their credentials:\n%s", w)
	}
	if red := cfg.Redacted(); red.Registry[0].Password != "***" {
		t.Error("REPO_PASS must be masked")
	}
	if _, err := decodeEnv(DefaultConfig(), embeddedSource, withWatchtower(envMap{"REPO_USER": "u"})); err == nil || !strings.Contains(err.Error(), "REPO_USER and REPO_PASS must be set together") {
		t.Errorf("err = %v", err)
	}
}

func TestWatchtowerEnvErrors(t *testing.T) {
	tests := []struct {
		name string
		env  envMap
		want string
	}{
		{"schedule and interval", envMap{"WATCHTOWER_SCHEDULE": "@daily", "WATCHTOWER_POLL_INTERVAL": "300"}, "WATCHTOWER_SCHEDULE and WATCHTOWER_POLL_INTERVAL are both set"},
		{"bad cron", envMap{"WATCHTOWER_SCHEDULE": "0 4 * *"}, `WATCHTOWER_SCHEDULE="0 4 * *": expected 5 fields`},
		{"bad interval", envMap{"WATCHTOWER_POLL_INTERVAL": "often"}, `WATCHTOWER_POLL_INTERVAL="often"`},
		{"bad bool", envMap{"WATCHTOWER_CLEANUP": "sure"}, `WATCHTOWER_CLEANUP="sure": want true or false`},
		{"bad level", envMap{"WATCHTOWER_LOG_LEVEL": "loud"}, `WATCHTOWER_LOG_LEVEL="loud"`},
		{"bad format", envMap{"WATCHTOWER_LOG_FORMAT": "xml"}, `WATCHTOWER_LOG_FORMAT="xml"`},
		{"update api without token", envMap{"WATCHTOWER_HTTP_API_UPDATE": "true"}, "WATCHTOWER_HTTP_API_UPDATE: the update API requires a token"},
		{"unknown legacy type", envMap{"WATCHTOWER_NOTIFICATIONS": "pigeon"}, `unknown notification type "pigeon"`},
		{"incomplete legacy email", envMap{"WATCHTOWER_NOTIFICATIONS": "email", "WATCHTOWER_NOTIFICATION_EMAIL_TO": "a@b.c"}, "WATCHTOWER_NOTIFICATIONS=email needs WATCHTOWER_NOTIFICATION_EMAIL_SERVER"},
		{"retired teams connector", envMap{"WATCHTOWER_NOTIFICATIONS": "msteams", "WATCHTOWER_NOTIFICATION_MSTEAMS_HOOK_URL": "https://acme.webhook.office.com/webhookb2/SECRET"}, "Office 365 connector webhooks were retired"},
		{"unsupported url scheme", envMap{"WATCHTOWER_NOTIFICATION_URL": "pushbullet://SECRET"}, "WATCHTOWER_NOTIFICATION_URL[0]: pushbullet:// is not a supported notification service"},
		{"bad cooldown", envMap{"WATCHTOWER_COOLDOWN_DELAY": "a while"}, "WATCHTOWER_COOLDOWN_DELAY"},
		{"bad api host", envMap{"WATCHTOWER_HTTP_API_HOST": "http://x"}, "WATCHTOWER_HTTP_API_HOST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeEnv(DefaultConfig(), embeddedSource, withWatchtower(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error echoes a secret: %v", err)
			}
		})
	}
}

func TestWatchtowerLogLevels(t *testing.T) {
	for level, want := range map[string]string{"panic": "error", "Fatal": "error", "warning": "warn", "info": "info", "trace": "debug"} {
		cfg := watchtowerOnly(t, envMap{"WATCHTOWER_LOG_LEVEL": level})
		if cfg.Log.Level != want {
			t.Errorf("WATCHTOWER_LOG_LEVEL=%s: level %q, want %q", level, cfg.Log.Level, want)
		}
	}
	if cfg := watchtowerOnly(t, envMap{"WATCHTOWER_DEBUG": "true", "WATCHTOWER_LOG_LEVEL": "error"}); cfg.Log.Level != "debug" {
		t.Error("WATCHTOWER_DEBUG wins over WATCHTOWER_LOG_LEVEL, as in Watchtower")
	}
	if cfg := watchtowerOnly(t, envMap{"WATCHTOWER_LOG_LEVEL": "warn", "DOCKPAUM_LOG_LEVEL": "info"}); cfg.Log.Level != "info" {
		t.Error("DOCKPAUM_LOG_LEVEL wins over WATCHTOWER_LOG_LEVEL")
	}
}

func TestWatchtowerHTTPAPIHost(t *testing.T) {
	cfg := watchtowerOnly(t, envMap{"WATCHTOWER_HTTP_API_HOST": "127.0.0.1"})
	if cfg.HTTP.ListenAddr() != "127.0.0.1:8080" {
		t.Errorf("listen %q", cfg.HTTP.ListenAddr())
	}
	cfg = watchtowerOnly(t, envMap{"WATCHTOWER_HTTP_API_HOST": "::1", "WATCHTOWER_HTTP_API_PORT": "9000", "DOCKPAUM_HTTP_PORT": "9100"})
	if cfg.HTTP.ListenAddr() != "[::1]:9100" {
		t.Errorf("listen %q: DOCKPAUM_HTTP_PORT keeps the host and wins on the port", cfg.HTTP.ListenAddr())
	}
}

func TestDockerClientEnv(t *testing.T) {
	tests := []struct {
		name, yaml string
		env        envMap
		warn, err  string
	}{
		{"ssh host", "{}", envMap{"DOCKER_HOST": "ssh://admin@nas"}, "", "DOCKER_HOST: ssh:// is not supported"},
		{"plain tcp", "{}", envMap{"DOCKER_HOST": "tcp://nas:2375"}, "DOCKER_HOST: tcp:// without TLS", ""},
		{"plain tcp on loopback", "{}", envMap{"DOCKER_HOST": "tcp://127.0.0.1:23750"}, "", ""},
		{"certs without verify", "{}", envMap{"DOCKER_HOST": "tcp://nas:2376", "DOCKER_CERT_PATH": "/certs"}, "TLS certificate verification of the Docker host is off", ""},
		{"verified tls", "{}", envMap{"DOCKER_HOST": "tcp://nas:2376", "DOCKER_CERT_PATH": "/certs", "DOCKER_TLS_VERIFY": "1"}, "", ""},
		{"pinned api version", "{}", envMap{"DOCKER_API_VERSION": "1.25"}, "DOCKER_API_VERSION=1.25 pins the Docker API version", ""},
		{"ssh host for one of several hosts", "hosts:\n  - name: nas\n    endpoint: unix:///var/run/docker.sock\n  - name: local\n", envMap{"DOCKER_HOST": "ssh://admin@nas"},
			"DOCKER_HOST: ssh:// is not supported, so hosts[1] (local) cannot connect", ""},
		{"ssh host for every host", "hosts:\n  - name: local\n", envMap{"DOCKER_HOST": "ssh://admin@nas"}, "", "DOCKER_HOST: ssh:// is not supported"},
		{"explicit endpoints ignore DOCKER_HOST", "hosts:\n  - name: nas\n    endpoint: unix:///var/run/docker.sock\n", envMap{"DOCKER_HOST": "ssh://x"}, "DOCKER_HOST is ignored", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decodeEnv([]byte(tt.yaml), "docker", tt.env)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w := strings.Join(cfg.Warnings(), "\n")
			if tt.warn == "" && strings.Contains(w, "DOCKER_") {
				t.Errorf("unexpected warning:\n%s", w)
			}
			if tt.warn != "" && !strings.Contains(w, tt.warn) {
				t.Errorf("warnings miss %q:\n%s", tt.warn, w)
			}
		})
	}
}

func TestDockerConfigDir(t *testing.T) {
	withFile := t.TempDir()
	writeSecret(t, withFile, "config.json", `{"auths":{}}`)
	empty := t.TempDir()

	cfg := envOnly(t, envMap{"DOCKER_CONFIG": withFile})
	if want := filepath.Join(withFile, "config.json"); cfg.DockerConfig != want {
		t.Errorf("docker_config = %q, want %q", cfg.DockerConfig, want)
	}
	if o := cfg.EnvOverrides(); len(o) != 1 || o[0] != (EnvOverride{Var: "DOCKER_CONFIG", Key: "docker_config"}) {
		t.Errorf("EnvOverrides() = %+v", o)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); w != "" {
		t.Errorf("DOCKER_CONFIG is a Docker setting, read without DOCKPAUM_WATCHTOWER_ENV:\n%s", w)
	}

	cfg = envOnly(t, envMap{"DOCKER_CONFIG": empty})
	if cfg.DockerConfig != "" || !strings.Contains(strings.Join(cfg.Warnings(), "\n"), "there is no config.json in it") {
		t.Errorf("a directory without config.json: docker_config %q, warnings %v", cfg.DockerConfig, cfg.Warnings())
	}

	cfg, err := decodeEnv([]byte("docker_config: /etc/dockpaum/config.json\n"), "file", envMap{"DOCKER_CONFIG": withFile})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DockerConfig != "/etc/dockpaum/config.json" || !strings.Contains(strings.Join(cfg.Warnings(), "\n"), "DOCKER_CONFIG is ignored: docker_config is set") {
		t.Errorf("docker_config from the file wins: %q, warnings %v", cfg.DockerConfig, cfg.Warnings())
	}
}

func TestWatchtowerDefaultInterval(t *testing.T) {
	const note = "checking every 24h as Watchtower did"
	tests := []struct {
		name, yaml string
		env        envMap
		interval   time.Duration
		cron       string
		noted      bool
	}{
		{"watchtower env without a schedule", "{}", envMap{"WATCHTOWER_CLEANUP": "true"}, 24 * time.Hour, "", true},
		{"an unsupported variable still means Watchtower", "{}", envMap{"WATCHTOWER_GIT_ENABLE": "true"}, 24 * time.Hour, "", true},
		{"no watchtower env keeps the code default", "{}", envMap{}, DefaultInterval, "", false},
		{"blank watchtower variables are unset", "{}", envMap{"WATCHTOWER_CLEANUP": " "}, DefaultInterval, "", false},
		{"the file's interval wins", "schedule:\n  interval: 30m\n", envMap{"WATCHTOWER_CLEANUP": "true"}, 30 * time.Minute, "", false},
		{"the file's cron wins", "schedule:\n  cron: \"0 4 * * *\"\n", envMap{"WATCHTOWER_CLEANUP": "true"}, 0, "0 4 * * *", false},
		{"DOCKPAUM_SCHEDULE wins", "{}", envMap{"WATCHTOWER_CLEANUP": "true", "DOCKPAUM_SCHEDULE": "2h"}, 2 * time.Hour, "", false},
		{"switched off", "{}", envMap{"WATCHTOWER_CLEANUP": "true", "DOCKPAUM_WATCHTOWER_ENV": "false"}, DefaultInterval, "", false},
		{"not switched on", "{}", envMap{"WATCHTOWER_CLEANUP": "true", "DOCKPAUM_WATCHTOWER_ENV": " "}, DefaultInterval, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decodeEnv([]byte(tt.yaml), "interval", withWatchtower(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Schedule.Interval.Std() != tt.interval || cfg.Schedule.Cron != tt.cron {
				t.Errorf("schedule %+v, want interval %v cron %q", cfg.Schedule, tt.interval, tt.cron)
			}
			if got := strings.Contains(strings.Join(cfg.Warnings(), "\n"), note); got != tt.noted {
				t.Errorf("24h note shown = %v, want %v: %v", got, tt.noted, cfg.Warnings())
			}
		})
	}
}

func TestWatchtowerHTTPAPIOpensTheWatchtowerRoutes(t *testing.T) {
	keys := func(c *Config) string {
		var out []string
		for _, o := range c.EnvOverrides() {
			if o.Key == "http.watchtower_compat" {
				out = append(out, o.Var)
			}
		}
		return strings.Join(out, ",")
	}
	tests := []struct {
		name   string
		env    envMap
		compat bool
		from   string
	}{
		{"metrics", envMap{"WATCHTOWER_HTTP_API_METRICS": "true"}, true, "WATCHTOWER_HTTP_API_METRICS"},
		{"update", envMap{"WATCHTOWER_HTTP_API_UPDATE": "true", "WATCHTOWER_HTTP_API_TOKEN": "tok"}, true, "WATCHTOWER_HTTP_API_UPDATE"},
		{"both", envMap{"WATCHTOWER_HTTP_API_UPDATE": "true", "WATCHTOWER_HTTP_API_METRICS": "true", "WATCHTOWER_HTTP_API_TOKEN": "tok"}, true, "WATCHTOWER_HTTP_API_METRICS"},
		{"neither", envMap{"WATCHTOWER_HTTP_API_TOKEN": "tok"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := watchtowerOnly(t, tt.env)
			if c.HTTP.WatchtowerCompat != tt.compat || keys(c) != tt.from {
				t.Errorf("watchtower_compat %v from %q, want %v from %q", c.HTTP.WatchtowerCompat, keys(c), tt.compat, tt.from)
			}
		})
	}
	c := envOnly(t, envMap{envWatchtowerEnv: "false", "WATCHTOWER_HTTP_API_METRICS": "true"})
	if c.HTTP.WatchtowerCompat {
		t.Error("DOCKPAUM_WATCHTOWER_ENV=false must not open the Watchtower routes")
	}
	c, err := decodeEnv([]byte("http:\n  read_token: r\n"), "dockpaum.yml", withWatchtower(envMap{"WATCHTOWER_HTTP_API_METRICS": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Join(c.Notes(), "\n")
	if want := "WATCHTOWER_HTTP_API_METRICS (http.watchtower_compat): only /v1/metrics is served; /v1/update also needs http.update_api"; !strings.Contains(n, want) || strings.Contains(n, "line ") {
		t.Errorf("notes = %s, want %q without a file line", n, want)
	}
}
