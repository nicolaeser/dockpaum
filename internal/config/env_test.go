package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); IsConfigEnv(name) {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestIsConfigEnv(t *testing.T) {
	for name, want := range map[string]bool{
		"DOCKPAUM_DRY_RUN": true, "DOCKPAUM_TARGET": true, "WATCHTOWER_CLEANUP": true, "DOCKER_HOST": true,
		"REPO_USER": true, "REPO_PASS": true, "TZ": true,
		"HOME": false, "TZDIR": false, "REPO": false, "HTTP_TOKEN": false,
	} {
		if got := IsConfigEnv(name); got != want {
			t.Errorf("IsConfigEnv(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestResolvePath(t *testing.T) {
	tests := []struct {
		name, flag string
		set        bool
		env, want  string
	}{
		{"default", DefaultConfigPath, false, "", DefaultConfigPath},
		{"env replaces the default", DefaultConfigPath, false, "/config/dockpaum.yml", "/config/dockpaum.yml"},
		{"explicit flag wins over env", "/etc/x.yml", true, "/config/dockpaum.yml", "/etc/x.yml"},
		{"explicit flag without env", "/etc/x.yml", true, "", "/etc/x.yml"},
		{"blank env is unset", DefaultConfigPath, false, "  ", DefaultConfigPath},
		{"empty flag value falls back", "", false, "", DefaultConfigPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvConfigPath, tt.env)
			if got := ResolvePath(tt.flag, tt.set); got != tt.want {
				t.Errorf("ResolvePath(%q, %v) = %q, want %q", tt.flag, tt.set, got, tt.want)
			}
		})
	}
}

func envOnly(t *testing.T, env envMap) *Config {
	t.Helper()
	cfg, err := decodeEnv(DefaultConfig(), embeddedSource, env)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDockpaumEnvWithoutConfigFile(t *testing.T) {
	cfg := envOnly(t, envMap{
		"DOCKPAUM_DRY_RUN":          "false",
		"DOCKPAUM_SCHEDULE":         "0 4 * * *",
		"DOCKPAUM_TIMEZONE":         "Europe/Berlin",
		"DOCKPAUM_LOG_LEVEL":        "DEBUG",
		"DOCKPAUM_LOG_FORMAT":       "text",
		"DOCKPAUM_HTTP_PORT":        "9090",
		"DOCKPAUM_HTTP_TOKEN":       "api-token",
		"DOCKPAUM_HTTP_READ_TOKEN":  "read-token",
		"DOCKPAUM_HTTP_UPDATE_API":  "yes",
		"DOCKPAUM_NOTIFY_URLS":      "discord://tok@123 ntfy://ntfy.sh/updates,discord://tok2@456",
		"DOCKPAUM_NOTIFY_EVENTS":    "update.failure,run.completed",
		"DOCKPAUM_LABEL_ENABLE":     "true",
		"DOCKPAUM_SCOPE":            "prod",
		"DOCKPAUM_CLEANUP":          "1",
		"DOCKPAUM_INCLUDE_STOPPED":  "on",
		"DOCKPAUM_MODE":             "minor",
		"DOCKPAUM_MIN_IMAGE_AGE":    "0s",
		"DOCKPAUM_HEALTH_TIMEOUT":   "5m",
		"DOCKPAUM_ROLLBACK":         "true",
		"DOCKPAUM_SELF_UPDATE":      "true",
		"DOCKPAUM_HOOK_SCRIPT_PATH": "/scripts",
	})
	def := policyByID(t, cfg, "default")
	checks := map[string]struct{ got, want any }{
		"dry_run":              {cfg.DryRun, false},
		"schedule.cron":        {cfg.Schedule.Cron, "0 4 * * *"},
		"schedule.interval":    {cfg.Schedule.Interval, Duration(0)},
		"cron spec zone":       {cfg.Schedule.CronSpec(), "CRON_TZ=Europe/Berlin 0 4 * * *"},
		"log.level":            {cfg.Log.Level, "debug"},
		"log.format":           {cfg.Log.Format, "text"},
		"listen addr":          {cfg.HTTP.ListenAddr(), ":9090"},
		"http.token":           {cfg.HTTP.Token, "api-token"},
		"http.read_token":      {cfg.HTTP.ReadToken, "read-token"},
		"http.update_api":      {cfg.HTTP.UpdateAPI, true},
		"notifiers":            {len(cfg.Notify), 3},
		"second discord named": {cfg.Notify[2].Name, "discord-2"},
		"events":               {strings.Join(cfg.Notify[1].Events, ","), "update.failure,run.completed"},
		"label_enable":         {cfg.Filters.LabelEnable, true},
		"scope":                {cfg.Filters.Scope, "prod"},
		"cleanup":              {cfg.Update.CleanupImages, true},
		"include_stopped":      {cfg.Filters.IncludeStopped, true},
		"mode":                 {def.Mode, ModeMinor},
		"min_image_age":        {def.MinImageAge, Duration(0)},
		"health_timeout":       {def.HealthTimeout, Duration(5 * time.Minute)},
		"rollback":             {cfg.Update.Rollback(), true},
		"self_update":          {cfg.Update.SelfUpdate.IsEnabled(), true},
		"one default policy":   {len(cfg.Update.Policies), 1},
		"notifier label":       {cfg.notifyLabel(1), "DOCKPAUM_NOTIFY_URLS[1]"},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "DOCKPAUM_HOOK_SCRIPT_PATH") {
		t.Errorf("an unrelated DOCKPAUM_* variable warned:\n%s", w)
	}
	var vars []string
	for _, o := range cfg.EnvOverrides() {
		vars = append(vars, o.Var+"→"+o.Key)
	}
	got := strings.Join(vars, " ")
	for _, want := range []string{"DOCKPAUM_DRY_RUN→dry_run", "DOCKPAUM_NOTIFY_URLS→notify[]", "DOCKPAUM_MODE→update.policies[default].mode", "DOCKPAUM_HTTP_TOKEN→http.token"} {
		if !strings.Contains(got, want) {
			t.Errorf("EnvOverrides() misses %s: %s", want, got)
		}
	}
}

func TestDockpaumEnvOverridesTheFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := writeSecret(t, dir, "token", "file-token\n")
	urlFile := writeSecret(t, dir, "urls", "# alerts\ndiscord://tok@1\n")
	raw := `
dry_run: false
schedule:
  interval: 30m
http:
  addr: 127.0.0.1:8080
  token: yaml-token
notify:
  - type: webhook
    url: https://hooks.example.com/a
  - type: webhook
    url: https://hooks.example.com/b
    events: [update.success]
update:
  policies:
    - id: default
      mode: patch
    - id: db
      mode: none
`
	cfg, err := decodeEnv([]byte(raw), "file", envMap{
		"DOCKPAUM_DRY_RUN":          "true",
		"DOCKPAUM_SCHEDULE":         "6h",
		"DOCKPAUM_HTTP_PORT":        "9000",
		"DOCKPAUM_HTTP_TOKEN_FILE":  tokenFile,
		"DOCKPAUM_NOTIFY_URLS_FILE": urlFile,
		"DOCKPAUM_NOTIFY_EVENTS":    "update.failure",
		"DOCKPAUM_MODE":             "major",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun || cfg.Schedule.Interval != Duration(6*time.Hour) || cfg.Schedule.Cron != "" {
		t.Errorf("dry_run %v, schedule %+v", cfg.DryRun, cfg.Schedule)
	}
	if cfg.HTTP.ListenAddr() != "127.0.0.1:9000" {
		t.Errorf("listen addr %q: the port must replace the file's while its host stays", cfg.HTTP.ListenAddr())
	}
	if cfg.HTTP.Token != "file-token" {
		t.Errorf("http.token = %q, want the content of DOCKPAUM_HTTP_TOKEN_FILE", cfg.HTTP.Token)
	}
	if len(cfg.Notify) != 3 || cfg.Notify[2].Type != "discord" {
		t.Fatalf("notify = %+v: env notifiers are added after the file's", cfg.Notify)
	}
	if got := [3]string{strings.Join(cfg.Notify[0].Events, ","), strings.Join(cfg.Notify[1].Events, ","), strings.Join(cfg.Notify[2].Events, ",")}; got != [3]string{"update.failure", "update.success", "update.failure"} {
		t.Errorf("events %v: DOCKPAUM_NOTIFY_EVENTS fills notifiers without events and keeps explicit lists", got)
	}
	if p := policyByID(t, cfg, "default"); p.Mode != ModeMajor {
		t.Errorf("default mode %q, want major", p.Mode)
	}
	if p := policyByID(t, cfg, "db"); p.Mode != ModeNone {
		t.Errorf("DOCKPAUM_MODE must only change the default policy, db = %q", p.Mode)
	}
}

func TestDockpaumEnvErrorsNameTheVariable(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		env  envMap
		want string
	}{
		{"bool", envMap{"DOCKPAUM_DRY_RUN": "maybe"}, `DOCKPAUM_DRY_RUN="maybe": want true or false`},
		{"schedule", envMap{"DOCKPAUM_SCHEDULE": "every day"}, `DOCKPAUM_SCHEDULE="every day": neither an interval`},
		{"zero interval", envMap{"DOCKPAUM_SCHEDULE": "0s"}, "an interval must be positive"},
		{"timezone", envMap{"DOCKPAUM_TIMEZONE": "Mars/Olympus"}, `DOCKPAUM_TIMEZONE="Mars/Olympus"`},
		{"log level", envMap{"DOCKPAUM_LOG_LEVEL": "verbose"}, `DOCKPAUM_LOG_LEVEL="verbose": want debug, info, warn or error`},
		{"log format", envMap{"DOCKPAUM_LOG_FORMAT": "pretty"}, `DOCKPAUM_LOG_FORMAT="pretty": want json or text`},
		{"port range", envMap{"DOCKPAUM_HTTP_PORT": "70000"}, "DOCKPAUM_HTTP_PORT=\"70000\": want a port number"},
		{"port text", envMap{"DOCKPAUM_HTTP_PORT": "http"}, "DOCKPAUM_HTTP_PORT"},
		{"mode", envMap{"DOCKPAUM_MODE": "yolo"}, `DOCKPAUM_MODE="yolo": unknown mode`},
		{"negative age", envMap{"DOCKPAUM_MIN_IMAGE_AGE": "-1h"}, "DOCKPAUM_MIN_IMAGE_AGE=\"-1h\": must not be negative"},
		{"bad duration", envMap{"DOCKPAUM_HEALTH_TIMEOUT": "soon"}, "DOCKPAUM_HEALTH_TIMEOUT"},
		{"event typo", envMap{"DOCKPAUM_NOTIFY_EVENTS": "update.sucess"}, `unknown event "update.sucess"`},
		{"token and file", envMap{"DOCKPAUM_HTTP_TOKEN": "a", "DOCKPAUM_HTTP_TOKEN_FILE": "/x"}, "set either DOCKPAUM_HTTP_TOKEN or DOCKPAUM_HTTP_TOKEN_FILE, not both"},
		{"token file missing", envMap{"DOCKPAUM_HTTP_READ_TOKEN_FILE": filepath.Join(dir, "nope")}, "DOCKPAUM_HTTP_READ_TOKEN_FILE: open"},
		{"plain https notify url", envMap{"DOCKPAUM_NOTIFY_URLS": "https://hooks.example.com/SECRET"}, "DOCKPAUM_NOTIFY_URLS[0]: want a notification service URL"},
		{"bad notify url", envMap{"DOCKPAUM_NOTIFY_URLS": "ntfy://ntfy.sh/ok discord://SECRET"}, "DOCKPAUM_NOTIFY_URLS[1]: discord:// URL: want discord://<token>@<webhook id>"},
		{"update api without token", envMap{"DOCKPAUM_HTTP_UPDATE_API": "true"}, "DOCKPAUM_HTTP_UPDATE_API: the update API requires a token"},
		{"watchtower switch", envMap{"DOCKPAUM_WATCHTOWER_ENV": "nah"}, `DOCKPAUM_WATCHTOWER_ENV="nah"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeEnv(DefaultConfig(), embeddedSource, tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if !strings.HasPrefix(err.Error(), "environment: ") {
				t.Errorf("err = %v: environment errors must say where they come from", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error echoes a secret: %v", err)
			}
		})
	}
}

func TestEnvUpdateAPIWithFileToken(t *testing.T) {
	if _, err := decodeEnv([]byte("http:\n  token: t\n"), "file", envMap{"DOCKPAUM_HTTP_UPDATE_API": "true"}); err != nil {
		t.Errorf("a token from the config file protects the API: %v", err)
	}
}

func TestMisspeltDockpaumEnvWarns(t *testing.T) {
	cfg := envOnly(t, envMap{"DOCKPAUM_DRYRUN": "true", "DOCKPAUM_NOTIFY_URL": "discord://a@1"})
	w := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{
		"DOCKPAUM_DRYRUN is not a dockpaum setting and is ignored — did you mean DOCKPAUM_DRY_RUN?",
		"DOCKPAUM_NOTIFY_URL is not a dockpaum setting and is ignored — did you mean DOCKPAUM_NOTIFY_URLS?",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
	if cfg.DryRun {
		t.Error("a misspelt variable must not take effect")
	}
}

func TestDeferredRefNamesTheOverlayVariable(t *testing.T) {
	_, err := decodeEnv([]byte("http:\n  token: ${DOCKPAUM_HTTP_TOKEN}\n"), "file", envMap{})
	if err == nil || !strings.Contains(err.Error(), "DOCKPAUM_HTTP_TOKEN is read from the environment by itself") {
		t.Fatalf("err = %v, want a hint that the variable works without the reference", err)
	}
	_, err = decodeEnv([]byte("notify:\n  - type: slack\n    url: ${DOCKPAUM_SLACK}\n"), "file", envMap{})
	if err == nil || !strings.Contains(err.Error(), "reference a variable without the DOCKPAUM_ prefix") {
		t.Fatalf("err = %v, want the rename hint", err)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"abc", "abc", 0}, {"DRYRUN", "DRY_RUN", 1}, {"kitten", "sitting", 3}, {"", "ab", 2}} {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestOverlayAvoidsHookVariables(t *testing.T) {
	hookVars := []string{
		"DOCKPAUM_TARGET", "DOCKPAUM_HOST", "DOCKPAUM_IMAGE", "DOCKPAUM_NEW_IMAGE", "DOCKPAUM_POLICY", "DOCKPAUM_PHASE",
		"DOCKPAUM_FAILED", "DOCKPAUM_JOB", "DOCKPAUM_RUN_ID", "DOCKPAUM_SCANNED", "DOCKPAUM_UPDATED",
	}
	env := envMap{}
	for _, name := range hookVars {
		if slices.Contains(knownDockpaumEnv, name) {
			t.Errorf("%s is both a hook variable and a setting", name)
		}
		env[name] = "x"
	}
	if w := envOnly(t, env).Warnings(); len(w) != 0 {
		t.Errorf("hook variables in the daemon environment warned: %v", w)
	}
}

func TestTimezoneFallbackComesFromTheEnvMap(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	if cfg := envOnly(t, envMap{}); cfg.Timezone != "UTC" {
		t.Errorf("timezone %q, want UTC without TZ in the map", cfg.Timezone)
	}
	if cfg := envOnly(t, envMap{"TZ": ":Europe/Berlin"}); cfg.Timezone != "Europe/Berlin" {
		t.Errorf("timezone %q, want the map's TZ", cfg.Timezone)
	}
	if cfg := envOnly(t, envMap{"TZ": "Europe/Berlin", "DOCKPAUM_TIMEZONE": "America/New_York"}); cfg.Timezone != "America/New_York" {
		t.Errorf("timezone %q: DOCKPAUM_TIMEZONE wins over TZ", cfg.Timezone)
	}
}

func TestBlankVariablesCountAsUnset(t *testing.T) {
	cfg := envOnly(t, envMap{"DOCKPAUM_DRY_RUN": " ", "DOCKPAUM_DRYRUN": "", "WATCHTOWER_CLEANUP": "", "DOCKPAUM_NOTIFY_URLS": "  "})
	if cfg.DryRun || len(cfg.EnvOverrides()) != 0 || len(cfg.Warnings()) != 0 || len(cfg.Notify) != 0 {
		t.Errorf("blank variables changed the config: overrides %+v, warnings %v", cfg.EnvOverrides(), cfg.Warnings())
	}
}

func TestPrintConfigHidesEnvSecrets(t *testing.T) {
	dir := t.TempDir()
	readTokenFile := writeSecret(t, dir, "read", "S-readtoken\n")
	urlFile := writeSecret(t, dir, "urls", "matrix://:S-matrixtoken@matrix.example.com/?rooms=!r:example.com\n")
	secrets := []string{
		"S-apitoken", "S-readtoken", "S-discord", "S-telegram", "S-gotify", "S-smtppass", "S-pushapp", "S-pushuser",
		"S-matrixtoken", "S-ntfytoken", "S-generic", "S-slack", "S-teams", "S-repopass", "S-apprise", "S-wtgotify",
	}
	cfg, err := decodeEnv([]byte("notify:\n  - type: apprise\n    url: https://apprise.example.com/notify/\n    to: [\"tgram://S-apprise/chat\"]\n"), "print", envMap{ // #nosec G101 -- dummy credentials of a test fixture
		"DOCKPAUM_HTTP_TOKEN":           "S-apitoken",
		"DOCKPAUM_HTTP_READ_TOKEN_FILE": readTokenFile,
		"DOCKPAUM_NOTIFY_URLS": strings.Join([]string{
			"discord://S-discord@123",
			"telegram://1:S-telegram@telegram?chats=5",
			"gotify://g.example.com/S-gotify",
			"smtp://bot:S-smtppass@mail.example.com:587/?from=a@example.com&to=b@example.com",
			"pushover://shoutrrr:S-pushapp@S-pushuser",
			"ntfy://ntfy.example.com/topic?token=S-ntfytoken",
			"generic+https://hooks.example.com/in?@Authorization=Bearer%20S-generic",
			"https://hooks.slack.com/services/T0/B0/S-slack",
			"https://prod-00.westus.logic.azure.com/workflows/S-teams/invoke",
		}, " "),
		"DOCKPAUM_NOTIFY_URLS_FILE":            "",
		"DOCKPAUM_WATCHTOWER_ENV":              "true",
		"WATCHTOWER_NOTIFICATION_URL":          urlFile,
		"WATCHTOWER_NOTIFICATIONS":             "gotify",
		"WATCHTOWER_NOTIFICATION_GOTIFY_URL":   "https://wt-gotify.example.com",
		"WATCHTOWER_NOTIFICATION_GOTIFY_TOKEN": "S-wtgotify",
		"REPO_USER":                            "bot",
		"REPO_PASS":                            "S-repopass",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(cfg.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(out), s) {
			t.Errorf("--print-config shows %s:\n%s", s, out)
		}
	}
	if !strings.Contains(string(out), "matrix.example.com") || !strings.Contains(string(out), "type: pushover") {
		t.Errorf("non-secret structure must stay visible:\n%s", out)
	}
}

func TestInstanceAndStateDirFromTheEnvironment(t *testing.T) {
	cfg := envOnly(t, envMap{"DOCKPAUM_INSTANCE": "  nas-1 ", "DOCKPAUM_STATE_DIR": "/data/state"})
	if cfg.Instance != "nas-1" || cfg.State.Dir != "/data/state" || cfg.State.Path() != "/data/state/state.json" {
		t.Errorf("instance %q, state %+v", cfg.Instance, cfg.State)
	}
	var got []string
	for _, o := range cfg.EnvOverrides() {
		got = append(got, o.Var+"→"+o.Key)
	}
	if s := strings.Join(got, " "); s != "DOCKPAUM_INSTANCE→instance DOCKPAUM_STATE_DIR→state.dir" {
		t.Errorf("EnvOverrides() = %s", s)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}

	cfg, err := decodeEnv([]byte("instance: file\nstate:\n  enabled: false\n"), "dockpaum.yml",
		envMap{"DOCKPAUM_INSTANCE": "env", "DOCKPAUM_STATE_DIR": "/data/state"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instance != "env" || cfg.State.IsEnabled() || cfg.State.Path() != "" {
		t.Errorf("instance %q, state %+v", cfg.Instance, cfg.State)
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(w, "DOCKPAUM_STATE_DIR (state.dir) has no effect: state.enabled is false") || strings.Contains(w, "line ") {
		t.Errorf("warnings = %s, want the variable named without a file line", w)
	}

	for _, tt := range []struct{ name, value, want string }{
		{"DOCKPAUM_STATE_DIR", "data/state", `DOCKPAUM_STATE_DIR="data/state": want an absolute path`},
		{"DOCKPAUM_INSTANCE", "a\nb", `DOCKPAUM_INSTANCE: "a\nb" contains a line break (U+000A)`},
	} {
		_, err := decodeEnv(DefaultConfig(), embeddedSource, envMap{tt.name: tt.value})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}
