package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOneLineConfigKeepsTheGuardrails(t *testing.T) {
	isolateEnv(t)
	t.Setenv("TZ", "")
	c, err := decode([]byte("dry_run: false\n"), "one-line")
	if err != nil {
		t.Fatal(err)
	}
	def := policyByID(t, c, "default")
	if def.Mode != ModeTagFollow || def.MinImageAge.Std() != time.Hour || def.HealthTimeout.Std() != 2*time.Minute {
		t.Errorf("default policy = %+v, want tag-follow with a 1h soak and a 2m health gate", def)
	}
	if !c.Audit.IsEnabled() || c.Audit.Dir != DefaultAuditDir {
		t.Errorf("audit = %+v, want enabled at %s", c.Audit, DefaultAuditDir)
	}
	if got := c.Schedule.Interval.Std(); got != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", got)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("the code defaults warned: %v", w)
	}
}

func TestPolicyGateDefaults(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name           string
		policy         string
		minAge, health time.Duration
	}{
		{name: "unset gets the code defaults", policy: "- id: web\n", minAge: time.Hour, health: 2 * time.Minute},
		{name: "explicit 0s turns the gates off", policy: "- id: web\n  min_image_age: 0s\n  health_timeout: 0s\n"},
		{name: "explicit integer 0 turns the gates off", policy: "- id: web\n  min_image_age: 0\n  health_timeout: 0\n"},
		{name: "null counts as unset", policy: "- id: web\n  min_image_age:\n  health_timeout: ~\n", minAge: time.Hour, health: 2 * time.Minute},
		{name: "explicit values are kept", policy: "- id: web\n  min_image_age: 30m\n  health_timeout: 5m\n", minAge: 30 * time.Minute, health: 5 * time.Minute},
		{name: "one gate set, the other defaulted", policy: "- id: web\n  min_image_age: 0s\n", health: 2 * time.Minute},
		{
			name:   "a merge key carries the explicit 0s",
			policy: "- &base\n  id: base\n  min_image_age: 0s\n- <<: *base\n  id: web\n",
			health: 2 * time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "update:\n  policies:\n" + indent(tt.policy, "    ")
			c, err := decode([]byte(yaml), tt.name)
			if err != nil {
				t.Fatal(err)
			}
			web := policyByID(t, c, "web")
			if web.MinImageAge.Std() != tt.minAge || web.HealthTimeout.Std() != tt.health {
				t.Errorf("web gates = (%v, %v), want (%v, %v)", web.MinImageAge, web.HealthTimeout, tt.minAge, tt.health)
			}
		})
	}
}

func indent(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "")
}

func TestDefaultPolicyIsListedOnce(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - id: critical\n      mode: patch\n"), "named")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Update.Policies) != 2 || !c.Update.Policies[1].IsDefault() {
		t.Fatalf("policies = %+v, want critical plus the appended default", c.Update.Policies)
	}
	own, err := decode([]byte("update:\n  policies:\n    - id: \"\"\n      min_image_age: 6h\n"), "own default")
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Update.Policies) != 1 || own.Update.Policies[0].MinImageAge.Std() != 6*time.Hour {
		t.Errorf("policies = %+v, want only the configured default", own.Update.Policies)
	}
}

func TestDisabledHealthGateWarns(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - id: fast\n      health_timeout: 0s\n    - id: frozen\n      mode: none\n      health_timeout: 0s\n"), "warn")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(got, `update.policies[0] (id="fast").health_timeout is 0s`) {
		t.Errorf("no warning for the disabled health gate:\n%s", got)
	}
	if strings.Contains(got, "frozen") {
		t.Errorf("a mode none policy never recreates and must not warn:\n%s", got)
	}
}

func TestAuditDefaults(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		yaml    string
		enabled bool
		dir     string
	}{
		{yaml: "{}\n", enabled: true, dir: DefaultAuditDir},
		{yaml: "audit:\n  enabled: true\n", enabled: true, dir: DefaultAuditDir},
		{yaml: "audit:\n  dir: /data/audit\n", enabled: true, dir: "/data/audit"},
		{yaml: "audit:\n  enabled: false\n", enabled: false, dir: ""},
		{yaml: "audit:\n  enabled: false\n  dir: /data/audit\n", enabled: false, dir: "/data/audit"},
	}
	for _, tt := range tests {
		c, err := decode([]byte(tt.yaml), "audit")
		if err != nil {
			t.Fatal(err)
		}
		if c.Audit.IsEnabled() != tt.enabled || c.Audit.Dir != tt.dir {
			t.Errorf("decode(%q): audit enabled=%v dir=%q, want %v %q", tt.yaml, c.Audit.IsEnabled(), c.Audit.Dir, tt.enabled, tt.dir)
		}
	}
	if (AuditConfig{}).IsEnabled() {
		t.Error("a hand-built zero AuditConfig must stay disabled")
	}
}

func TestRunOnStart(t *testing.T) {
	isolateEnv(t)
	on, off := true, false
	tests := []struct {
		name     string
		schedule ScheduleConfig
		dryRun   bool
		want     bool
	}{
		{name: "interval mode", schedule: ScheduleConfig{Interval: Duration(time.Hour)}, want: true},
		{name: "cron mode", schedule: ScheduleConfig{Cron: "0 4 * * *"}, want: false},
		{name: "cron mode in dry-run", schedule: ScheduleConfig{Cron: "0 4 * * *"}, dryRun: true, want: true},
		{name: "cron mode, explicit on", schedule: ScheduleConfig{Cron: "0 4 * * *", RunOnStart: &on}, want: true},
		{name: "cron mode in dry-run, explicit off", schedule: ScheduleConfig{Cron: "0 4 * * *", RunOnStart: &off}, dryRun: true, want: false},
		{name: "interval mode, explicit off", schedule: ScheduleConfig{Interval: Duration(time.Hour), RunOnStart: &off}, want: false},
	}
	for _, tt := range tests {
		c := Config{Schedule: tt.schedule, DryRun: tt.dryRun}
		if got := c.RunOnStart(); got != tt.want {
			t.Errorf("%s: RunOnStart() = %v, want %v", tt.name, got, tt.want)
		}
	}
	c, err := decode([]byte("dry_run: true\nschedule:\n  cron: \"0 4 * * *\"\n"), "redacted")
	if err != nil {
		t.Fatal(err)
	}
	if red := c.Redacted(); red.Schedule.RunOnStart == nil || !*red.Schedule.RunOnStart {
		t.Errorf("Redacted().Schedule.RunOnStart = %v, want the effective true", red.Schedule.RunOnStart)
	}
	if c.Schedule.RunOnStart != nil {
		t.Error("Redacted must not mutate the original")
	}
}

func TestEmbeddedDefaultBehavesLikeWatchtower(t *testing.T) {
	isolateEnv(t)
	t.Setenv("TZ", "")
	c, err := decode(DefaultConfig(), embeddedSource)
	if err != nil {
		t.Fatal(err)
	}
	def := policyByID(t, c, "default")
	if c.DryRun || c.Schedule.Interval.Std() != DefaultInterval || c.Schedule.Cron != "" {
		t.Errorf("dry_run %v, interval %v, cron %q: want updates applied every %v", c.DryRun, c.Schedule.Interval, c.Schedule.Cron, DefaultInterval)
	}
	if def.Mode != ModeTagFollow || def.MinImageAge != 0 || len(c.Update.Policies) != 1 {
		t.Errorf("policies = %+v, want one tag-follow default policy without soak", c.Update.Policies)
	}
	if !c.Update.Rollback() || !c.Filters.HonorWatchtowerLabels() || c.Filters.LabelEnable {
		t.Errorf("rollback %v, watchtower labels %v, label_enable %v", c.Update.Rollback(), c.Filters.HonorWatchtowerLabels(), c.Filters.LabelEnable)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("the embedded default warned: %v", w)
	}
}

func TestSelfUpdateIsOffByDefault(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name string
		yaml string
		env  envMap
		want bool
	}{
		{"embedded default", string(DefaultConfig()), nil, false},
		{"key unset", "dry_run: false\n", nil, false},
		{"null counts as unset", "update:\n  self_update:\n    enabled:\n", nil, false},
		{"enabled in the file", "update:\n  self_update:\n    enabled: true\n", nil, true},
		{"YAML 1.1 on", "update:\n  self_update:\n    enabled: on\n", nil, true},
		{"disabled in the file", "update:\n  self_update:\n    enabled: false\n", nil, false},
		{"environment turns it on", "dry_run: false\n", envMap{envSelfUpdate: "true"}, true},
		{"environment wins over the file", "update:\n  self_update:\n    enabled: true\n", envMap{envSelfUpdate: "false"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := decodeEnv([]byte(tt.yaml), "dockpaum.yml", tt.env)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Update.SelfUpdate.IsEnabled(); got != tt.want {
				t.Errorf("self-update enabled = %v, want %v", got, tt.want)
			}
			if got := *c.Redacted().Update.SelfUpdate.Enabled; got != tt.want {
				t.Errorf("--print-config shows enabled: %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateTreatsEmptyFileAsMissing(t *testing.T) {
	isolateEnv(t)
	t.Setenv(EnvConfigURL, "")
	dir := t.TempDir()
	for name, content := range map[string]string{"empty": "", "blank": " \n\t\n"} {
		path := filepath.Join(dir, name+".yml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Validate(path)
		if err != nil {
			t.Fatalf("%s file: %v", name, err)
		}
		if c.Source() != embeddedSource || c.DryRun {
			t.Errorf("%s file: source %q dry_run %v, want the embedded default", name, c.Source(), c.DryRun)
		}
		if b, _ := os.ReadFile(path); string(b) != content {
			t.Errorf("%s file: Validate wrote to it", name)
		}
	}
	missing := filepath.Join(dir, "missing.yml")
	if c, err := Validate(missing); err != nil || c.Source() != embeddedSource {
		t.Fatalf("missing file: %v, %v", c, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("Validate created %s", missing)
	}
}

func TestCommentOnlyConfigIsRejected(t *testing.T) {
	isolateEnv(t)
	t.Setenv(EnvConfigURL, "")
	path := filepath.Join(t.TempDir(), "dockpaum.yml")
	content := []byte("# dry_run: false\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, load := range map[string]func(string) (*Config, error){"Validate": Validate, "Load": Load} {
		if _, err := load(path); err == nil || !strings.Contains(err.Error(), "no settings") {
			t.Errorf("%s: err = %v, want a no-settings error", name, err)
		}
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, content) {
		t.Error("Load replaced a file that holds comments")
	}
}

func TestLoadMaterializesDefault(t *testing.T) {
	isolateEnv(t)
	t.Setenv(EnvConfigURL, "")
	dir := t.TempDir()
	for name, existing := range map[string]*string{"missing": nil, "empty": new(string)} {
		path := filepath.Join(dir, name, "dockpaum.yml")
		if existing != nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(*existing), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Source() != path || c.DryRun || len(c.Warnings()) != 0 {
			t.Errorf("%s: source %q dry_run %v warnings %v", name, c.Source(), c.DryRun, c.Warnings())
		}
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, DefaultConfig()) {
			t.Errorf("%s: file not replaced by the embedded default (%v)", name, err)
		}
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestLoadRunsOnEmbeddedDefaultWhenItCannotWrite(t *testing.T) {
	isolateEnv(t)
	t.Setenv(EnvConfigURL, "")
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	readOnly := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	c, err := Load(filepath.Join(readOnly, "etc", "dockpaum.yml"))
	if err != nil {
		t.Fatalf("Load must not fail when the default cannot be written: %v", err)
	}
	if c.Source() != embeddedSource || c.DryRun {
		t.Errorf("source %q dry_run %v, want the embedded default", c.Source(), c.DryRun)
	}
	if got := strings.Join(c.Warnings(), "\n"); !strings.Contains(got, "running on the embedded default") {
		t.Errorf("warnings = %q, want the write failure explained", got)
	}
}

func TestWriteDefaultNamesTheDirectoryOnce(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeDefault(filepath.Join(blocker, "etc", "dockpaum.yml"))
	if err == nil {
		t.Fatal("writing below a regular file must fail")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "create config directory: ") || strings.Count(msg, blocker) != 1 {
		t.Errorf("err = %q, want the directory named once", msg)
	}
}
