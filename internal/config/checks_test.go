package config

import (
	"strings"
	"testing"
)

func warningsOf(t *testing.T, doc string) string {
	t.Helper()
	c, err := decode([]byte(doc), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(c.Warnings(), "\n")
}

func TestPolicyAndSettingWarnings(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"dry run skips jobs and run hooks", "dry_run: true\nhooks:\n  pre_run: [\"host:/a.sh\"]\n",
			[]string{"line 1: dry_run is on: jobs and the pre_run/post_run hooks are skipped until dry_run is false"}},
		{"very short interval", "schedule:\n  interval: 30s\n",
			[]string{"line 2: schedule.interval: a check every 30s queries every registry that often"}},
		{"unknown endpoint", "hosts:\n  - name: nas\n    endpoint: http://nas:2375\n",
			[]string{`line 3: hosts[0] (nas).endpoint: "http://nas:2375" is not a Docker endpoint dockpaum knows`}},
		{"update api without a server", "http:\n  enabled: false\n  update_api: true\n  token: t\n",
			[]string{"http.update_api has no effect: http.enabled is false"}},
		{"relative docker config", "docker_config: docker/config.json\n",
			[]string{`line 1: docker_config: "docker/config.json" is a relative path`}},
		{"registry without credentials", "registry:\n  - server: ghcr.io\n",
			[]string{"line 2: registry[0] (ghcr.io): no username, password or password_file"}},
		{"monitor_only with mode none", "update:\n  policies:\n    - id: x\n      mode: none\n      monitor_only: true\n",
			[]string{`update.policies[0] (id="x").monitor_only has no effect with mode none`}},
		{"tags without a semver mode", "update:\n  policies:\n    - id: x\n      tags: {exclude: rc}\n",
			[]string{`update.policies[0] (id="x").tags has no effect with mode tag-follow`}},
		{"policy replacing a built-in differently", "update:\n  policies:\n    - id: minor\n      mode: patch\n",
			[]string{`update.policies[0] (id="minor") replaces the built-in policy minor with different behavior (mode patch, monitor_only false)`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warningsOf(t, tt.doc)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("warnings lack %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestSettingsThatDoNotWarn(t *testing.T) {
	isolateEnv(t)
	for name, doc := range map[string]string{
		"same behavior as the built-in": "update:\n  policies:\n    - id: minor\n      mode: minor\n      min_image_age: 6h\n    - id: monitor\n      monitor_only: true\n    - id: tag-follow\n      mode: digest\n",
		"tags on a semver policy":       "update:\n  policies:\n    - id: db\n      mode: minor\n      tags: {include: '^\\d+\\.\\d+$'}\n",
		"monitor only without health":   "update:\n  policies:\n    - id: watch\n      monitor_only: true\n      health_timeout: 0s\n",
		"jobs while live":               "dry_run: false\njobs:\n  - name: j\n    cron: \"@daily\"\n    commands: [x]\n",
		"known endpoints":               "hosts:\n  - name: a\n    endpoint: unix:///var/run/docker.sock\n  - name: b\n    endpoint: tcp://b:2376\n    tls: {ca_cert: /c}\n",
		"absolute docker config":        "docker_config: /root/.docker/config.json\n",
		"smtp with implicit tls":        "notify:\n  - type: email\n    to: [a@example.com]\n    smtp: {host: mail.example.com, port: 465}\n  - url: smtp://bot:pw@mail.example.com:465/?from=a@example.com&to=b@example.com\n", // #nosec G101 -- dummy credentials of a test fixture
	} {
		t.Run(name, func(t *testing.T) {
			if got := warningsOf(t, doc); got != "" {
				t.Errorf("unexpected warnings:\n%s", got)
			}
		})
	}
}

func TestTimingWarnings(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"global windows never open", "schedule:\n  windows: [{times: [\"05:00-05:00\"]}]\n",
			"schedule.windows: never open (every time range is empty), so no update run ever starts"},
		{"cron outside the global windows", "schedule:\n  cron: \"0 12 * * *\"\n  windows: [{times: [\"02:00-04:00\"]}]\n",
			"schedule.cron: never fires inside schedule.windows, so no update run ever starts — move the cron or the windows"},
		{"cron inside the global windows", "schedule:\n  cron: \"0 3 * * *\"\n  windows: [{times: [\"02:00-04:00\"]}]\n", ""},
		{"cron in another zone than the windows", "schedule:\n  cron: \"CRON_TZ=Asia/Tokyo 0 3 * * *\"\n  timezone: UTC\n  windows: [{times: [\"02:00-04:00\"]}]\n",
			"schedule.cron: never fires inside schedule.windows"},
		{"weekly cron on the wrong day", "schedule:\n  cron: \"0 3 * * MON\"\n  windows: [{days: [Sat, Sun], times: [\"02:00-04:00\"]}]\n",
			"schedule.cron: never fires inside schedule.windows"},
		{"interval longer than the windows", "schedule:\n  interval: 1h\n  windows: [{times: [\"03:00-03:30\"]}]\n",
			"schedule.interval: runs every 1h, longer than the longest opening of schedule.windows (30m), so the runs can keep missing the windows — use schedule.cron with a time inside them"},
		{"@every longer than the windows", "schedule:\n  cron: \"@every 24h\"\n  windows: [{times: [\"03:00-04:00\"]}]\n",
			"schedule.cron: runs every 1d, longer than the longest opening of schedule.windows (1h)"},
		{"interval within the windows", "schedule:\n  interval: 15m\n  windows: [{times: [\"03:00-04:00\"]}]\n", ""},
		{"policy windows never open", "update:\n  policies:\n    - id: db\n      windows: [{times: [\"04:00-04:00\"]}]\n",
			`update.policies[0] (id="db").windows: never open (every time range is empty), so containers using this policy are never updated`},
		{"policy and global windows never overlap", "schedule:\n  windows: [{days: [Sat]}]\nupdate:\n  policies:\n    - id: db\n      windows: [{days: [Mon]}]\n",
			`update.policies[0] (id="db").windows: never open while schedule.windows is open`},
		{"regular cron outside the policy windows", "schedule:\n  cron: \"0 12 * * *\"\nupdate:\n  policies:\n    - id: db\n      windows: [{times: [\"02:00-04:00\"]}]\n",
			`update.policies[0] (id="db").windows: the regular schedule (schedule.cron) never fires inside the policy windows, so containers using this policy are never updated — give the policy its own schedule inside its windows`},
		{"regular interval longer than the policy windows", "schedule:\n  interval: 1h\nupdate:\n  policies:\n    - id: db\n      windows: [{times: [\"02:00-02:30\"]}]\n",
			`update.policies[0] (id="db").windows: the regular schedule (schedule.interval) runs every 1h, longer than the longest opening of the policy windows (30m)`},
		{"policy schedule outside its windows", "timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: db\n      schedule: \"0 12 * * SUN\"\n      windows: [{days: [Sun], times: [\"02:00-04:00\"]}]\n",
			`update.policies[0] (id="db").schedule: never fires inside the policy windows, so containers using this policy are never updated`},
		{"policy schedule outside the global windows", "schedule:\n  windows: [{days: [Sat]}]\nupdate:\n  policies:\n    - id: db\n      schedule: \"0 3 * * SUN\"\n",
			`update.policies[0] (id="db").schedule: never fires inside schedule.windows (which gate policy schedules too)`},
		{"policy schedule inside its windows", "timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: db\n      schedule: \"0 3 * * SUN\"\n      windows: [{days: [Sun], times: [\"02:00-04:00\"]}]\n", ""},
		{"mode none is never updated anyway", "update:\n  policies:\n    - id: db\n      mode: none\n      windows: [{times: [\"04:00-04:00\"]}]\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warningsOf(t, tt.doc)
			if tt.want == "" {
				if got != "" {
					t.Errorf("unexpected warnings:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("warnings lack %q:\n%s", tt.want, got)
			}
		})
	}
}

func TestNotes(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte(`schedule:
  interval: 2m
  windows: [{days: [Sat]}]
update:
  policies:
    - id: db
      mode: digest
      schedule: "0 3 * * SAT"
notify:
  - type: ntfy
    url: https://ntfy.sh/dockpaum-alerts
`), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Notes(), "\n")
	for _, want := range []string{
		"line 2: schedule.interval: a check every 2m is frequent; registries such as Docker Hub rate-limit pulls (the default is 5m)",
		"http.token: not set, so /internal-api/ health, ready, busy, version, metrics, status, containers and summary are served without authentication on :8080",
		`line 7: update.policies[0] (id="db").mode: digest is another name for tag-follow`,
		`line 8: update.policies[0] (id="db").schedule: schedule.windows also gate this policy's runs`,
		"line 11: notify[0] (ntfy).url: anyone who knows a topic on ntfy.sh can read it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "interval") {
			t.Errorf("a 2m interval is a note, not a warning: %s", w)
		}
	}
	quiet, err := decode([]byte("http:\n  token: t\n"), "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if n := quiet.Notes(); len(n) != 0 {
		t.Errorf("notes = %v, want none", n)
	}
}
