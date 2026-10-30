package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func decodeErr(t *testing.T, doc string) error {
	t.Helper()
	_, err := decode([]byte(doc), "dockpaum.yml")
	if err == nil {
		t.Fatalf("decode(%q) succeeded, want an error", doc)
	}
	return err
}

func TestStructureProblemsNameKeyAndLine(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"misspelt key", "dryrun: true\n", `dockpaum.yml:1: dryrun: unknown key — did you mean "dry_run"?`},
		{"hyphen for underscore", "dry-run: true\n", `did you mean "dry_run"?`},
		{"other letter case", "DryRun: true\n", `did you mean "dry_run"?`},
		{"key at the wrong level", "policies: []\n", "dockpaum.yml:1: policies: unknown key — did you mean update.policies?"},
		{"key valid in several places", "cron: \"0 4 * * *\"\n", "did you mean schedule.cron or jobs[].cron?"},
		{"nothing close lists the keys", "x:\n  y: 1\n", "x: unknown key (known here: log, http, timezone, schedule,"},
		{"nested key with the policy id", "update:\n  policies:\n    - id: db\n      windos: []\n", `dockpaum.yml:4: update.policies[0] (id="db").windos: unknown key — did you mean "windows"?`},
		{"duplicate key", "schedule:\n  cron: \"@daily\"\n  cron: \"@hourly\"\n", "dockpaum.yml:3: schedule.cron: set twice (first on line 2)"},
		{"scalar where a list of entries is expected", "hosts: local\n", `dockpaum.yml:1: hosts: expected a list, got "local"`},
		{"mapping where a list is expected", "notify:\n  type: slack\n", `dockpaum.yml:2: notify: expected a list, got a mapping — start each entry with "- "`},
		{"scalar where a mapping is expected", "log: debug\n", `dockpaum.yml:1: log: expected a mapping (key: value), got "debug"`},
		{"list where text is expected", "timezone: [UTC]\n", "timezone: expected a text value, got a list"},
		{"quoted boolean", "dry_run: \"false\"\n", `dockpaum.yml:1: dry_run: "false" is quoted, which makes it text; write it without quotes`},
		{"not a boolean", "dry_run: maybe\n", `dry_run: "maybe" is not true or false`},
		{"not a number", "http:\n  port: eighty\n", `dockpaum.yml:2: http.port: "eighty" is not a whole number`},
		{"duration with a path and a hint", "update:\n  policies:\n    - id: db\n      min_image_age: 1 day\n",
			`dockpaum.yml:4: update.policies[0] (id="db").min_image_age: "1 day" is not a duration (missing unit after "1") — write it like 30s, 5m, 1h30m, 1d or 2w`},
		{"duration in a job", "jobs:\n  - name: backup\n    cron: \"@daily\"\n    commands: [x]\n    timeout: soon\n", `jobs[0] (backup).timeout: "soon" is not a duration`},
		{"invalid YAML", "a: [1\n", "dockpaum.yml:1: invalid YAML: "},
		{"comments only", "# nothing\n", "no settings (the document is empty or only comments)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeErr(t, tt.doc)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q\nwant it to contain %q", err, tt.want)
			}
			for _, leak := range []string{"config.", "!!str", "!!map", "!!seq", "not found in type"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks %q", err, leak)
				}
			}
		})
	}
}

func TestAllStructureProblemsAreReported(t *testing.T) {
	isolateEnv(t)
	err := decodeErr(t, "dryrun: true\nhttp:\n  port: x\nupdate:\n  policies:\n    - mode: minor\n      soak: 1h\n")
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %T, want *Error", err)
	}
	want := []Problem{
		{Path: "dryrun", Line: 1},
		{Path: "http.port", Line: 3},
		{Path: "update.policies[0].soak", Line: 7},
	}
	if len(cerr.Problems) != len(want) {
		t.Fatalf("problems = %+v, want %d", cerr.Problems, len(want))
	}
	for i, w := range want {
		if p := cerr.Problems[i]; p.Path != w.Path || p.Line != w.Line {
			t.Errorf("problem %d = %+v, want path %s line %d", i, p, w.Path, w.Line)
		}
	}
	if !strings.HasPrefix(err.Error(), "dockpaum.yml: 3 problems\n  line 1: dryrun: unknown key") {
		t.Errorf("error = %q", err)
	}
}

func TestValidationErrorsCarryTheirLine(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"mode with a suggestion", "update:\n  policies:\n    - id: db\n      mode: minr\n",
			`dockpaum.yml:4: update.policies[0] (id="db").mode: unknown mode "minr" — did you mean "minor"? (want tag-follow, patch, minor, major, none or digest)`},
		{"job cron", "jobs:\n  - name: backup\n    cron: \"0 4 * *\"\n    commands: [x]\n", "dockpaum.yml:3: jobs[0] (backup).cron: expected 5 fields"},
		{"window", "schedule:\n  windows:\n    - days: [Moonday]\n", `dockpaum.yml:3: schedule.windows[0].days: unknown day "Moonday"`},
		{"policy window", "update:\n  policies:\n    - id: db\n      windows:\n        - times: [\"3am\"]\n", `dockpaum.yml:5: update.policies[0] (id="db").windows[0].times: expected HH:MM-HH:MM`},
		{"window list in one string", "schedule:\n  windows:\n    - days: \"Mon, Tue\"\n", "list several as [Mon, Tue]"},
		{"a key the file leaves out has no line", "http:\n  update_api: true\n", "dockpaum.yml:2: http.update_api: requires http.token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := decodeErr(t, tt.doc); !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q\nwant it to contain %q", err, tt.want)
			}
		})
	}
	var cerr *Error
	if err := decodeErr(t, "update:\n  policies:\n    - id: db\n      mode: minr\n"); !errors.As(err, &cerr) || cerr.Problems[0].Path != "update.policies[0].mode" || cerr.Problems[0].Line != 4 {
		t.Errorf("err = %#v, want path update.policies[0].mode at line 4", err)
	}
}

func TestUnsetVariablesAreNamedInErrors(t *testing.T) {
	isolateEnv(t)
	err := decodeErr(t, "notify:\n  - type: discord\n    url: ${DPTEST_UNSET_WEBHOOK}\n")
	if want := "dockpaum.yml:2: notify[0] (discord): url is required (${DPTEST_UNSET_WEBHOOK} is not set)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSecretFileErrorsUnwrap(t *testing.T) {
	isolateEnv(t)
	err := decodeErr(t, "http:\n  token_file: "+filepath.Join(t.TempDir(), "missing")+"\n")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(%v, fs.ErrNotExist) = false", err)
	}
}

func TestCaseInsensitiveValuesAreNormalized(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte(`
log: {level: DEBUG, format: Text}
filters: {scope: " prod "}
update:
  strategy: Parallel
  approval: {url: "https://approve.example.com", method: post}
  policies:
    - id: db
      mode: Minor
notify:
  - type: Slack
    url: https://hooks.slack.com/services/T0/B0/X
    events: [Update.Failure, RUN.COMPLETED]
`), "case")
	if err != nil {
		t.Fatal(err)
	}
	if c.Log.Level != "debug" || c.Log.Format != "text" || c.Update.Strategy != StrategyParallel || c.Filters.Scope != "prod" {
		t.Errorf("log/strategy/scope = %+v %q %q", c.Log, c.Update.Strategy, c.Filters.Scope)
	}
	if c.Update.Approval.Method != "POST" || policyByID(t, c, "db").Mode != ModeMinor {
		t.Errorf("method %q, mode %q", c.Update.Approval.Method, policyByID(t, c, "db").Mode)
	}
	if n := c.Notify[0]; n.Type != "slack" || !slices.Equal(n.Events, []string{"update.failure", "run.completed"}) {
		t.Errorf("notifier = %+v", n)
	}
}

func TestNewValidationErrors(t *testing.T) {
	isolateEnv(t)
	slack := "notify:\n  - type: slack\n    url: https://hooks.slack.com/services/T0/B0/X\n"
	tests := []struct {
		name, doc, want string
	}{
		{"log level", "log:\n  level: verbose\n", `log.level: unknown level "verbose" (want debug, info, warn or error)`},
		{"log format", "log:\n  format: pretty\n", `log.format: unknown format "pretty" (want json or text)`},
		{"strategy", "update:\n  strategy: rollin\n", `update.strategy: unknown strategy "rollin" — did you mean "rolling"?`},
		{"approval method", "update:\n  approval:\n    url: https://a.example.com\n    method: PSOT\n", `update.approval.method: unknown method "PSOT" — did you mean "POST"?`},
		{"approval url scheme", "update:\n  approval:\n    url: ftp://a.example.com/x?token=s3cret\n", "update.approval.url: must be an http:// or https:// URL"},
		{"duplicate host name", "hosts:\n  - name: nas\n  - name: nas\n    endpoint: tcp://nas:2376\n", `dockpaum.yml:3: hosts[1].name: "nas" is already the name of hosts[0]`},
		{"duplicate default host names", "hosts:\n  - name: host-1\n  - endpoint: tcp://b:2376\n", `"host-1" is already the name of hosts[0]`},
		{"event typo", slack + "    events: [update.sucess]\n", `notify[0].events[0]: unknown event "update.sucess" — did you mean "update.success"?`},
		{"events in one string", slack + "    events: \"update.failure, run.completed\"\n", "list several events as [update.failure, run.completed]"},
		{"reserved off", "update:\n  policies:\n    - id: \"OFF\"\n", `update.policies[0].id: "OFF" is reserved`},
		{"policy tag regex", "update:\n  policies:\n    - id: db\n      mode: minor\n      tags: {exclude: \"(rc\"}\n", `update.policies[0] (id="db").tags.exclude: "(rc" is not a valid RE2 regular expression: missing closing )`},
		{"lookahead is not RE2", "update:\n  policies:\n    - mode: minor\n      tags: {include: \"^(?!rc)\"}\n", "invalid or unsupported Perl syntax"},
		{"never-firing cron", "schedule:\n  cron: \"0 0 30 2 *\"\n", `schedule.cron: "0 0 30 2 *" never fires`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeErr(t, tt.doc)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q\nwant it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q echoes the approval URL", err)
			}
		})
	}
}

func TestShorthands(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name      string
		shorthand string
		canonical string
	}{
		{"one window day and time", "schedule:\n  windows:\n    - days: Mon-Fri\n      times: \"03:00-05:00\"\n",
			"schedule:\n  windows:\n    - days: [Mon-Fri]\n      times: [\"03:00-05:00\"]\n"},
		{"one event and recipient", "notify:\n  - type: email\n    to: ops@example.com\n    events: update.failure\n    smtp: {host: mail.example.com, starttls: true}\n",
			"notify:\n  - type: email\n    to: [ops@example.com]\n    events: [update.failure]\n    smtp: {host: mail.example.com, starttls: true}\n"},
		{"one command, name and status", "hooks:\n  post_update: \"host:/notify.sh\"\nfilters:\n  exclude_names: db\nupdate:\n  approval: {url: \"https://a.example.com\", ok_statuses: 202}\njobs:\n  - name: j\n    cron: \"@daily\"\n    commands: \"host:/prune.sh\"\n",
			"hooks:\n  post_update: [\"host:/notify.sh\"]\nfilters:\n  exclude_names: [db]\nupdate:\n  approval: {url: \"https://a.example.com\", ok_statuses: [202]}\njobs:\n  - name: j\n    cron: \"@daily\"\n    commands: [\"host:/prune.sh\"]\n"},
		{"schedule as a cron", "schedule: \"0 4 * * *\"\n", "schedule:\n  cron: \"0 4 * * *\"\n"},
		{"schedule as an interval", "schedule: 6h\n", "schedule:\n  interval: 6h\n"},
		{"schedule as seconds", "schedule: 3600\n", "schedule:\n  interval: 1h\n"},
		{"schedule as a descriptor", "schedule: \"@daily\"\n", "schedule:\n  cron: \"@daily\"\n"},
		{"policies as a map", "update:\n  policies:\n    default:\n      min_image_age: 2h\n    db:\n      mode: minor\n      windows: [{days: Sat}]\n    web:\n",
			"update:\n  policies:\n    - id: default\n      min_image_age: 2h\n    - id: db\n      mode: minor\n      windows: [{days: [Sat]}]\n    - id: web\n"},
		{"map form with a matching id", "update:\n  policies:\n    db: {id: DB, mode: patch}\n", "update:\n  policies:\n    - id: DB\n      mode: patch\n"},
		{"map form with an anchor", "update:\n  policies:\n    base: &base {mode: minor, min_image_age: 3h}\n    db: *base\n",
			"update:\n  policies:\n    - id: base\n      mode: minor\n      min_image_age: 3h\n    - id: db\n      mode: minor\n      min_image_age: 3h\n"},
		{"anchors and merge keys in the list form", "update:\n  policies:\n    - &base {id: base, mode: minor, health_timeout: 0s}\n    - <<: *base\n      id: db\n",
			"update:\n  policies:\n    - {id: base, mode: minor, health_timeout: 0s}\n    - {id: db, mode: minor, health_timeout: 0s}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			short, err := decode([]byte(tt.shorthand), "short")
			if err != nil {
				t.Fatalf("shorthand: %v", err)
			}
			long, err := decode([]byte(tt.canonical), "long")
			if err != nil {
				t.Fatalf("canonical form: %v", err)
			}
			a, _ := yaml.Marshal(short.Redacted())
			b, _ := yaml.Marshal(long.Redacted())
			if string(a) != string(b) {
				t.Errorf("shorthand decodes to\n%s\nthe canonical form to\n%s", a, b)
			}
		})
	}
}

func TestShorthandErrors(t *testing.T) {
	isolateEnv(t)
	tests := []struct{ name, doc, want string }{
		{"schedule neither", "schedule: \"every day\"\n", `dockpaum.yml:1: schedule: "every day": neither an interval (6h, 1d) nor a cron spec`},
		{"schedule negative", "schedule: -5m\n", "an interval must be positive"},
		{"map id mismatch", "update:\n  policies:\n    web:\n      id: other\n", `dockpaum.yml:4: update.policies.web.id: "other" does not match its key "web"`},
		{"map value not a mapping", "update:\n  policies:\n    web: minor\n", `update.policies.web: expected the settings of policy "web"`},
		{"map form merge", "update:\n  policies:\n    <<: {db: {}}\n", "<< merges are not supported in the map form"},
		{"list of mappings is not a scalar", "hosts: [local]\n", `hosts[0]: expected a mapping (key: value), got "local"`},
		{"map form key errors keep the policy", "update:\n  policies:\n    db:\n      mode: minor\n      soak: 1h\n", `dockpaum.yml:5: update.policies[0] (id="db").soak: unknown key`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := decodeErr(t, tt.doc); !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q\nwant it to contain %q", err, tt.want)
			}
		})
	}
}

func TestMapFormPoliciesKeepTheirGates(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    db:\n      health_timeout: 0s\n    web:\n"), "map")
	if err != nil {
		t.Fatal(err)
	}
	if db := policyByID(t, c, "db"); db.HealthTimeout != 0 || db.MinImageAge.Std() != DefaultMinImageAge {
		t.Errorf("db gates = (%v, %v), want an explicit 0s health gate and the 1h default soak", db.MinImageAge, db.HealthTimeout)
	}
	if web := policyByID(t, c, "web"); web.HealthTimeout.Std() != DefaultHealthTimeout {
		t.Errorf("web health_timeout = %v, want the default", web.HealthTimeout)
	}
}

func TestPrintConfigDurationsReadLikeTheConfig(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - min_image_age: 90m\n      health_timeout: 2m\n"), "print")
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(c.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interval: 5m\n", "min_image_age: 1h30m\n", "health_timeout: 2m\n", "retention: 31d\n", "stop_timeout: 30s\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("--print-config output lacks %q:\n%s", want, out)
		}
	}
	back, err := decode(out, "printed")
	if err != nil {
		t.Fatalf("the printed config does not load: %v", err)
	}
	if back.Update.Policies[0].MinImageAge.Std() != 90*time.Minute || back.Audit.Retention.Std() != 31*24*time.Hour {
		t.Errorf("round trip: %+v %v", back.Update.Policies[0], back.Audit.Retention)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", time.Second: "1s", 90 * time.Second: "1m30s", 2 * time.Minute: "2m", time.Hour: "1h",
		90 * time.Minute: "1h30m", 26 * time.Hour: "26h", 24 * time.Hour: "1d", 31 * 24 * time.Hour: "31d",
		-time.Hour: "-1h", 1500 * time.Millisecond: "1.5s", 10*time.Hour + 5*time.Second: "10h0m5s",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
		if parsed, err := ParseDuration(formatDuration(d)); err != nil || parsed != d {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", formatDuration(d), parsed, err, d)
		}
	}
}

func TestDirectoryErrorNamesThePathOnce(t *testing.T) {
	dir := t.TempDir()
	_, err := Validate(dir)
	if err == nil {
		t.Fatal("Validate(dir) succeeded")
	}
	if n := strings.Count(err.Error(), dir); n != 1 || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("error = %q, want the path once and 'is a directory'", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dockpaum.yml")); err == nil {
		t.Error("Validate wrote into the directory")
	}
}

func TestMessagePath(t *testing.T) {
	for msg, want := range map[string]string{
		`update.policies[1] (id="db").mode: unknown`:      "update.policies[1].mode",
		`update.policies[1] (id="a) b").windows: x`:       "update.policies[1].windows",
		`notify[0] (apprise).to[1]: not an Apprise URL`:   "notify[0].to[1]",
		`jobs[2] (backup): name is required`:              "jobs[2]",
		`schedule: set either cron or interval, not both`: "schedule",
		"dry_run is on: jobs are skipped":                 "dry_run",
		"DOCKPAUM_NOTIFY_URLS[0] (discord): bad":          "",
		"WATCHTOWER_SCHEDULE is set":                      "",
		"unknown: something":                              "",
	} {
		if got := messagePath(msg); got != want {
			t.Errorf("messagePath(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestProblemsDoNotEchoSecrets(t *testing.T) {
	isolateEnv(t)
	for doc, want := range map[string]string{ // #nosec G101 -- dummy credentials of a test fixture
		"notify: https://hooks.slack.com/services/T0/B0/privatepart\n":                       `notify: expected a list, got a URL (not shown)`,
		"http:\n  port: \"https://example.com/?token=privatepart\"\n":                        "http.port: a URL (not shown) is not a whole number",
		"update:\n  self_update:\n    enabled: privatepart0123456789012345678901234567890\n": "a long text value (not shown) is not true or false",
	} {
		err := decodeErr(t, doc)
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "privatepart") {
			t.Errorf("error = %q, want %q and no secret", err, want)
		}
	}
}
