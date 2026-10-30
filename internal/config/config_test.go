package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandEnvPreservingRuntime(t *testing.T) {
	t.Setenv("CFGTEST_VAR", "hello")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "braced var", in: "x: ${CFGTEST_VAR}", want: "x: hello"},
		{name: "bare var", in: "x: $CFGTEST_VAR", want: "x: hello"},
		{name: "double dollar escapes", in: "password: pa$$word", want: "password: pa$word"},
		{name: "braced runtime var preserved", in: "cmd: ${DOCKPAUM_X}", want: "cmd: ${DOCKPAUM_X}"},
		{name: "bare runtime var normalized to braced", in: "cmd: $DOCKPAUM_X", want: "cmd: ${DOCKPAUM_X}"},
		{name: "lone dollar before non-name char", in: "$-foo", want: "$-foo"},
		{name: "dollar before digit", in: "$1abc", want: "$1abc"},
		{name: "trailing dollar", in: "end$", want: "end$"},
		{name: "unterminated brace left alone", in: "${UNCLOSED", want: "${UNCLOSED"},
		{name: "unset braced var expands to empty", in: "x: <${CFGTEST_VAR_DEFINITELY_UNSET}>", want: "x: <>"},
		{name: "unset bare var expands to empty", in: "x: <$CFGTEST_VAR_DEFINITELY_UNSET>", want: "x: <>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expandEnvPreservingRuntime(tt.in); got != tt.want {
				t.Errorf("expandEnvPreservingRuntime(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeValidate(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		errContains string
	}{
		{
			name: "minimal valid config",
			yaml: "hosts:\n  - name: local\n",
		},
		{
			name:        "unknown field rejected",
			yaml:        "bogus_field: 1\n",
			wantErr:     true,
			errContains: "bogus_field",
		},
		{
			name:        "invalid cron spec rejected",
			yaml:        "schedule:\n  cron: \"not a cron\"\n",
			wantErr:     true,
			errContains: "schedule.cron",
		},
		{
			name: "valid 6-field cron accepted",
			yaml: "schedule:\n  cron: \"0 */5 * * * *\"\n",
		},
		{
			name:        "cron and interval together rejected",
			yaml:        "schedule:\n  cron: \"0 */5 * * * *\"\n  interval: 5m\n",
			wantErr:     true,
			errContains: "either cron or interval",
		},
		{
			name:        "bad timezone rejected",
			yaml:        "schedule:\n  timezone: \"Mars/OlympusMons\"\n",
			wantErr:     true,
			errContains: "schedule.timezone",
		},
		{
			name:        "negative interval rejected",
			yaml:        "schedule:\n  interval: -5m\n",
			wantErr:     true,
			errContains: "schedule.interval",
		},
		{
			name:        "invalid strategy rejected",
			yaml:        "update:\n  strategy: yolo\n",
			wantErr:     true,
			errContains: "update.strategy",
		},
		{
			name: "duplicate policy ids rejected",
			yaml: "update:\n  policies:\n" +
				"    - id: web\n      mode: digest\n" +
				"    - id: web\n      mode: patch\n",
			wantErr:     true,
			errContains: "duplicate",
		},
		{
			name: "two default policies rejected",
			yaml: "update:\n  policies:\n" +
				"    - id: \"\"\n      mode: tag-follow\n" +
				"    - id: default\n      mode: digest\n",
			wantErr: true,
		},
		{
			name:        "reserved policy id none rejected",
			yaml:        "update:\n  policies:\n    - id: none\n",
			wantErr:     true,
			errContains: "reserved",
		},
		{
			name:        "unknown policy mode rejected",
			yaml:        "update:\n  policies:\n    - id: x\n      mode: yolo\n",
			wantErr:     true,
			errContains: "unknown mode",
		},
		{
			name:        "unknown notify type rejected",
			yaml:        "notify:\n  - type: pigeon\n",
			wantErr:     true,
			errContains: "unknown type",
		},
		{
			name:        "notify without type rejected",
			yaml:        "notify:\n  - url: https://example.com\n",
			wantErr:     true,
			errContains: "type is required",
		},
		{
			name:        "slack notify without url rejected",
			yaml:        "notify:\n  - type: slack\n",
			wantErr:     true,
			errContains: "url is required",
		},
		{
			name: "invalid window time in policy rejected",
			yaml: "update:\n  policies:\n" +
				"    - id: night\n      windows:\n" +
				"        - days: [mon]\n          times: [\"25:00-26:00\"]\n",
			wantErr:     true,
			errContains: "windows",
		},
		{
			name: "valid windows in schedule and policy accepted",
			yaml: "schedule:\n  timezone: \"Europe/Berlin\"\n  windows:\n" +
				"    - days: [mon-fri]\n      times: [\"02:00-04:00\"]\n" +
				"update:\n  policies:\n" +
				"    - id: night\n      windows:\n" +
				"        - days: [sat]\n          times: [\"22:00-02:00\"]\n",
		},
		{
			name:        "registry without server rejected",
			yaml:        "registry:\n  - username: bob\n",
			wantErr:     true,
			errContains: "server is required",
		},
		{
			name: "repository-scoped registries with password_file accepted",
			yaml: "registry:\n" +
				"  - server: ghcr.io/acme/project-a\n    username: bot\n    password_file: /run/secrets/a\n" +
				"  - server: https://ghcr.io/acme/project-b/\n    username: bot\n    password: tok\n",
		},
		{
			name:        "registry password and password_file rejected",
			yaml:        "registry:\n  - server: ghcr.io\n    username: bot\n    password: x\n    password_file: /f\n",
			wantErr:     true,
			errContains: "either password or password_file",
		},
		{
			name:        "registry password_file without username rejected",
			yaml:        "registry:\n  - server: ghcr.io\n    password_file: /f\n",
			wantErr:     true,
			errContains: "username is required",
		},
		{
			name:        "registry path with tag rejected",
			yaml:        "registry:\n  - server: ghcr.io/acme/app:1.0\n",
			wantErr:     true,
			errContains: "repository path",
		},
		{
			name:        "registry server without host rejected",
			yaml:        "registry:\n  - server: https://\n",
			wantErr:     true,
			errContains: "no host",
		},
		{
			name: "policy schedule and hooks accepted",
			yaml: "update:\n  policies:\n" +
				"    - id: database\n      timezone: Europe/Berlin\n      schedule: \"0 0 3 * * SUN\"\n" +
				"      hooks:\n        pre_update: [\"host:/scripts/backup.sh\", \"exec:@self:pg_dumpall\"]\n",
		},
		{
			name:        "invalid policy schedule rejected",
			yaml:        "update:\n  policies:\n    - id: database\n      schedule: \"0 3 * SUN\"\n",
			wantErr:     true,
			errContains: "update.policies[0] (id=\"database\").schedule: expected 5 fields",
		},
		{
			name: "five-field schedule, policy schedule and job accepted",
			yaml: "schedule:\n  cron: \"0 4 * * *\"\nupdate:\n  policies:\n    - id: database\n      schedule: \"30 3 * * SUN\"\n" +
				"jobs:\n  - name: a\n    cron: \"0 2 * * *\"\n    commands: [\"true\"]\n",
		},
		{
			name: "@every with day and week units accepted",
			yaml: "schedule:\n  cron: \"@every 1d\"\njobs:\n  - name: a\n    cron: \"@every 2w\"\n    commands: [\"true\"]\n",
		},
		{
			name:        "cron zone prefix without a schedule rejected",
			yaml:        "schedule:\n  cron: \"CRON_TZ=Europe/Berlin\"\n",
			wantErr:     true,
			errContains: "schedule.cron: CRON_TZ needs a schedule",
		},
		{
			name:        "malformed policy exec hook rejected",
			yaml:        "update:\n  policies:\n    - id: x\n      hooks:\n        on_failure: [\"exec:db\"]\n",
			wantErr:     true,
			errContains: "hooks.on_failure",
		},
		{
			name: "run hooks accepted",
			yaml: "hooks:\n  pre_run: [\"host:/scripts/pre.sh\"]\n  post_run: [\"/scripts/post.sh\"]\n",
		},
		{
			name:        "exec run hook rejected",
			yaml:        "hooks:\n  pre_run: [\"exec:db:true\"]\n",
			wantErr:     true,
			errContains: "hooks.pre_run",
		},
		{
			name: "job accepted",
			yaml: "jobs:\n  - name: backup\n    cron: \"0 0 2 * * *\"\n    timeout: 30m\n" +
				"    commands: [\"host:/scripts/backup.sh\", \"exec:db:pg_dumpall\"]\n",
		},
		{
			name:        "job without name rejected",
			yaml:        "jobs:\n  - cron: \"0 0 2 * * *\"\n    commands: [\"true\"]\n",
			wantErr:     true,
			errContains: "name is required",
		},
		{
			name: "duplicate job names rejected",
			yaml: "jobs:\n  - name: a\n    cron: \"0 0 2 * * *\"\n    commands: [\"true\"]\n" +
				"  - name: a\n    cron: \"0 0 3 * * *\"\n    commands: [\"true\"]\n",
			wantErr:     true,
			errContains: "duplicate name",
		},
		{
			name:        "job with invalid cron rejected",
			yaml:        "jobs:\n  - name: a\n    cron: \"0 2 * *\"\n    commands: [\"true\"]\n",
			wantErr:     true,
			errContains: "jobs[0] (a).cron: expected 5 fields",
		},
		{
			name:        "job with a bad timezone rejected",
			yaml:        "jobs:\n  - name: a\n    cron: \"0 2 * * *\"\n    timezone: Mars/Olympus\n    commands: [\"true\"]\n",
			wantErr:     true,
			errContains: "jobs[0] (a).timezone",
		},
		{
			name:        "job without commands rejected",
			yaml:        "jobs:\n  - name: a\n    cron: \"0 0 2 * * *\"\n",
			wantErr:     true,
			errContains: "commands is required",
		},
		{
			name:        "job exec without container rejected",
			yaml:        "jobs:\n  - name: a\n    cron: \"0 0 2 * * *\"\n    commands: [\"exec:@self:true\"]\n",
			wantErr:     true,
			errContains: "must name a container",
		},
		{
			name:        "job with unknown host rejected",
			yaml:        "jobs:\n  - name: a\n    cron: \"0 0 2 * * *\"\n    host: nope\n    commands: [\"true\"]\n",
			wantErr:     true,
			errContains: "unknown host",
		},
		{
			name: "job exec with several hosts needs host",
			yaml: "hosts:\n  - name: a\n  - name: b\n" +
				"jobs:\n  - name: j\n    cron: \"0 0 2 * * *\"\n    commands: [\"exec:db:true\"]\n",
			wantErr:     true,
			errContains: "host is required",
		},
		{
			name:        "update api without token rejected",
			yaml:        "http:\n  update_api: true\n",
			wantErr:     true,
			errContains: "http.update_api",
		},
		{
			name: "update api with token accepted",
			yaml: "http:\n  update_api: true\n  token: secret\n",
		},
		{
			name:        "telegram without chat rejected",
			yaml:        "notify:\n  - type: telegram\n    token: t\n",
			wantErr:     true,
			errContains: "telegram",
		},
		{
			name:        "gotify without token rejected",
			yaml:        "notify:\n  - type: gotify\n    url: https://gotify.example.com\n",
			wantErr:     true,
			errContains: "gotify",
		},
		{
			name:        "ntfy without url rejected",
			yaml:        "notify:\n  - type: ntfy\n",
			wantErr:     true,
			errContains: "url is required",
		},
		{
			name: "new notifier types and v1 keys accepted",
			yaml: "notify:\n  - type: telegram\n    token: t\n    channel: \"-100\"\n  - type: ntfy\n    url: https://ntfy.sh/x\n" +
				"  - type: gotify\n    url: https://g\n    token: t\n  - type: teams\n    url: https://teams\n" +
				"filters:\n  scope: prod\n  watchtower_labels: false\nhooks:\n  label_hooks: true\ndocker_config: /config.json\n",
		},
		{
			name: "job exec with several hosts and host accepted",
			yaml: "hosts:\n  - name: a\n  - name: b\n" +
				"jobs:\n  - name: j\n    cron: \"0 0 2 * * *\"\n    host: b\n    commands: [\"exec:db:true\"]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decode([]byte(tt.yaml), tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decode(%q): expected error, got config %+v", tt.yaml, cfg)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("decode(%q): error %q does not contain %q", tt.yaml, err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode(%q): unexpected error: %v", tt.yaml, err)
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	isolateEnv(t)
	raw := `
http:
  token: supersecret
registry:
  - server: ghcr.io
    username: bob
    password: hunter2
  - server: ghcr.io/acme/project-a
    username: bot
    password_file: /run/secrets/project-a.pull-token
notify:
  - type: slack
    name: team
    url: https://hooks.slack.com/services/T000/B000/SECRET
    token: tok123
    headers:
      Authorization: Bearer abc
  - type: email
    to: ["ops@example.com"]
    smtp:
      host: mail.example.com
      port: 587
      username: mailer
      password: mailpass
update:
  approval:
    url: https://approvals.example.com/x
    headers:
      X-Auth: approvalsecret
`
	cfg, err := decode([]byte(raw), "redacted-test")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	red := cfg.Redacted()

	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "http token", got: red.HTTP.Token, want: "***"},
		{name: "registry password", got: red.Registry[0].Password, want: "***"},
		{name: "registry username kept", got: red.Registry[0].Username, want: "bob"},
		{name: "registry password_file path kept", got: red.Registry[1].PasswordFile, want: "/run/secrets/project-a.pull-token"},
		{name: "registry password stays empty with password_file", got: red.Registry[1].Password, want: ""},
		{name: "notifier token", got: red.Notify[0].Token, want: "***"},
		{name: "notifier url masked to scheme://host/***", got: red.Notify[0].URL, want: "https://hooks.slack.com/***"},
		{name: "notifier header value", got: red.Notify[0].Headers["Authorization"], want: "***"},
		{name: "smtp password", got: red.Notify[1].SMTP.Password, want: "***"},
		{name: "smtp host kept", got: red.Notify[1].SMTP.Host, want: "mail.example.com"},
		{name: "approval header value", got: red.Update.Approval.Headers["X-Auth"], want: "***"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Redacted(): %s = %q, want %q", c.name, c.got, c.want)
		}
	}

	originals := []struct {
		name string
		got  string
		want string
	}{
		{name: "http token", got: cfg.HTTP.Token, want: "supersecret"},
		{name: "registry password", got: cfg.Registry[0].Password, want: "hunter2"},
		{name: "notifier token", got: cfg.Notify[0].Token, want: "tok123"},
		{name: "notifier url", got: cfg.Notify[0].URL, want: "https://hooks.slack.com/services/T000/B000/SECRET"},
		{name: "notifier header value", got: cfg.Notify[0].Headers["Authorization"], want: "Bearer abc"},
		{name: "smtp password", got: cfg.Notify[1].SMTP.Password, want: "mailpass"},
		{name: "approval header value", got: cfg.Update.Approval.Headers["X-Auth"], want: "approvalsecret"},
	}
	for _, c := range originals {
		if c.got != c.want {
			t.Errorf("Redacted() mutated original: %s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	var c Config
	c.applyDefaults()

	if c.HTTP.Port != 8080 {
		t.Errorf("HTTP.Port = %d, want 8080", c.HTTP.Port)
	}
	if got := c.Schedule.Interval.Std(); got != 5*time.Minute {
		t.Errorf("Schedule.Interval = %v, want 5m", got)
	}
	if len(c.Update.Policies) != 1 || !c.Update.Policies[0].IsDefault() ||
		c.Update.Policies[0].MinImageAge.Std() != time.Hour || c.Update.Policies[0].HealthTimeout.Std() != 2*time.Minute {
		t.Errorf("Update.Policies = %+v, want the default policy with a 1h soak and a 2m health gate", c.Update.Policies)
	}
	if !c.Audit.IsEnabled() || c.Audit.Dir != DefaultAuditDir {
		t.Errorf("Audit = %+v, want enabled at %s", c.Audit, DefaultAuditDir)
	}
	if c.Update.Strategy != StrategyRolling {
		t.Errorf("Update.Strategy = %q, want %q", c.Update.Strategy, StrategyRolling)
	}
	if len(c.Hosts) != 1 || c.Hosts[0].Name != "local" || c.Hosts[0].Endpoint != "" {
		t.Errorf("Hosts = %+v, want one local host with empty endpoint", c.Hosts)
	}
	if c.Update.Concurrency != 1 {
		t.Errorf("Update.Concurrency = %d, want 1", c.Update.Concurrency)
	}
	if got := c.Update.StopTimeout.Std(); got != 30*time.Second {
		t.Errorf("Update.StopTimeout = %v, want 30s", got)
	}
	if c.Log.Level != "info" || c.Log.Format != "json" {
		t.Errorf("Log = %+v, want level=info format=json", c.Log)
	}
}

func TestJobTimeoutDefaultsToHookTimeout(t *testing.T) {
	isolateEnv(t)
	cfg, err := decode([]byte("hooks:\n  timeout: 2m\njobs:\n"+
		"  - name: a\n    cron: \"0 0 2 * * *\"\n    commands: [\"true\"]\n"+
		"  - name: b\n    cron: \"0 0 2 * * *\"\n    timeout: 1h\n    commands: [\"true\"]\n"), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Jobs[0].Timeout.Std(); got != 2*time.Minute {
		t.Errorf("job a timeout = %v, want hooks.timeout 2m", got)
	}
	if got := cfg.Jobs[1].Timeout.Std(); got != time.Hour {
		t.Errorf("job b timeout = %v, want 1h", got)
	}
}

func TestPolicyCronSpec(t *testing.T) {
	tests := []struct {
		p    Policy
		want string
	}{
		{Policy{}, ""},
		{Policy{Schedule: "0 0 3 * * SUN"}, "0 0 3 * * SUN"},
		{Policy{Schedule: " 0 0 3 * * SUN ", Timezone: "Europe/Berlin"}, "CRON_TZ=Europe/Berlin 0 0 3 * * SUN"},
		{Policy{Schedule: "CRON_TZ=UTC 0 0 3 * * SUN", Timezone: "Europe/Berlin"}, "CRON_TZ=UTC 0 0 3 * * SUN"},
		{Policy{Schedule: "TZ=UTC @daily", Timezone: "Europe/Berlin"}, "TZ=UTC @daily"},
	}
	for _, tt := range tests {
		if got := tt.p.CronSpec(); got != tt.want {
			t.Errorf("CronSpec(%+v) = %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestRegistryAuthScope(t *testing.T) {
	tests := []struct {
		server, host, path string
	}{
		{"ghcr.io", "ghcr.io", ""},
		{"https://GHCR.io/", "ghcr.io", ""},
		{"ghcr.io/Acme/project-a/", "ghcr.io", "acme/project-a"},
		{"http://registry.example.com:5000/team", "registry.example.com:5000", "team"},
	}
	for _, tt := range tests {
		host, path := RegistryAuth{Server: tt.server}.Scope()
		if host != tt.host || path != tt.path {
			t.Errorf("Scope(%q) = (%q, %q), want (%q, %q)", tt.server, host, path, tt.host, tt.path)
		}
	}
}

func TestRegistryAuthCredentials(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, ".pull-token")
	if err := os.WriteFile(tokenFile, []byte("  tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxPasswordFileSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}

	user, pass, err := RegistryAuth{Server: "ghcr.io", Username: "bot", Password: "inline"}.Credentials()
	if err != nil || user != "bot" || pass != "inline" {
		t.Errorf("inline credentials = (%q, %q, %v)", user, pass, err)
	}
	user, pass, err = RegistryAuth{Server: "ghcr.io", Username: "bot", PasswordFile: tokenFile}.Credentials()
	if err != nil || user != "bot" || pass != "tok-123" {
		t.Errorf("file credentials = (%q, %q, %v), want trimmed token", user, pass, err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "nope"), "empty": empty, "oversized": big} {
		_, pass, err := RegistryAuth{Server: "ghcr.io", Username: "bot", PasswordFile: path}.Credentials()
		if err == nil {
			t.Errorf("%s password_file: expected error, got password %q", name, pass)
			continue
		}
		if strings.Contains(err.Error(), "tok-123") {
			t.Errorf("%s password_file: error leaks file content: %v", name, err)
		}
	}
}

func TestV1Defaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := decode([]byte("schedule:\n  cron: \"0 0 4 * * *\"\n  timezone: Europe/Berlin\n"), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Update.Rollback() {
		t.Error("rollback_on_failure must default to true")
	}
	if !cfg.Filters.HonorWatchtowerLabels() {
		t.Error("watchtower_labels must default to true")
	}
	if got := cfg.Schedule.CronSpec(); got != "CRON_TZ=Europe/Berlin 0 0 4 * * *" {
		t.Errorf("global cron spec = %q, want the schedule timezone applied", got)
	}
	off, err := decode([]byte("update:\n  rollback_on_failure: false\n"), "off")
	if err != nil {
		t.Fatal(err)
	}
	if off.Update.Rollback() {
		t.Error("explicit rollback_on_failure: false must disable rollback")
	}
}

func TestFetchURLRefusesPlainHTTPRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://config.example.com/dockpaum.yml", http.StatusFound)
	}))
	defer srv.Close()
	_, err := fetchURL(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "https required") {
		t.Fatalf("fetchURL followed a downgrade redirect: %v", err)
	}
}

func TestRedactedShowsEffectiveBooleans(t *testing.T) {
	isolateEnv(t)
	cfg, err := decode([]byte("hosts:\n  - name: local\n"), "defaults")
	if err != nil {
		t.Fatal(err)
	}
	red := cfg.Redacted()
	for name, v := range map[string]*bool{
		"http.enabled": red.HTTP.Enabled, "filters.watchtower_labels": red.Filters.WatchtowerLabels,
		"update.rollback_on_failure": red.Update.RollbackOnFail,
	} {
		if v == nil || !*v {
			t.Errorf("%s = %v, want effective true", name, v)
		}
	}
	if v := red.Update.SelfUpdate.Enabled; v == nil || *v {
		t.Errorf("update.self_update.enabled = %v, want effective false", v)
	}
	if cfg.HTTP.Enabled != nil {
		t.Error("Redacted must not mutate the original")
	}
}
