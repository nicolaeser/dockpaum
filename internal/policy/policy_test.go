package policy

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

func testResolver(t *testing.T, policies []config.Policy) *Resolver {
	t.Helper()
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: policies}}
	r, err := NewResolver(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func testCt(name string, labels map[string]string) docker.Container {
	return docker.Container{ID: "id-" + name, Name: name, Image: "img", Labels: labels}
}

func TestResolveDefaultFallback(t *testing.T) {
	tests := []struct {
		name     string
		policies []config.Policy
		labels   map[string]string
		wantMode string
	}{
		{
			name:     "no policies, no label: built-in default",
			policies: nil,
			labels:   nil,
			wantMode: config.ModeTagFollow,
		},
		{
			name: "label missing falls back to configured default",
			policies: []config.Policy{
				{ID: "default", Mode: config.ModeDigest},
				{ID: "critical", Mode: config.ModePatch},
			},
			labels:   nil,
			wantMode: config.ModeTagFollow,
		},
		{
			name: "explicit default label resolves default",
			policies: []config.Policy{
				{ID: "default", Mode: config.ModeDigest},
			},
			labels:   map[string]string{LabelPolicy: "default"},
			wantMode: config.ModeTagFollow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testResolver(t, tc.policies)
			got := r.Resolve(testCt("web", tc.labels), tc.labels)
			if got.Skip {
				t.Fatalf("Resolve skipped unexpectedly: %q", got.SkipReason)
			}
			if got.PolicyID != "default" {
				t.Errorf("PolicyID = %q, want %q", got.PolicyID, "default")
			}
			if got.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.Windows == nil {
				t.Error("Windows = nil, want always-allowed window set")
			} else if !got.Windows.Allows(time.Now()) {
				t.Error("default policy windows should always allow")
			}
		})
	}
}

func TestResolvePolicyNoneSkips(t *testing.T) {
	r := testResolver(t, nil)
	for label, reason := range map[string]string{
		"none": "dockpaum.policy=none", "NONE": "dockpaum.policy=none", " none ": "dockpaum.policy=none",
		"off": "dockpaum.policy=off", "Off": "dockpaum.policy=off",
	} {
		got, err := r.ResolveStrict(testCt("web", nil), map[string]string{LabelPolicy: label, LabelMode: "garbage"})
		if err != nil {
			t.Fatalf("policy label %q: %v (none/off never look at override labels)", label, err)
		}
		if !got.Skip {
			t.Fatalf("policy label %q: Skip = false, want true", label)
		}
		if got.PolicyID != NoneID {
			t.Errorf("policy label %q: PolicyID = %q, want %q", label, got.PolicyID, NoneID)
		}
		if got.SkipReason != reason {
			t.Errorf("policy label %q: SkipReason = %q, want %q", label, got.SkipReason, reason)
		}
	}
}

func TestResolveEnableFalseSkips(t *testing.T) {
	r := testResolver(t, nil)
	for _, v := range []string{"false", "FALSE", "False"} {
		labels := map[string]string{
			LabelEnable:         v,
			ComposeProjectLabel: "alpha",
		}
		got := r.Resolve(testCt("web", nil), labels)
		if !got.Skip {
			t.Fatalf("enable=%q: Skip = false, want true", v)
		}
		if got.SkipReason != "dockpaum.enable=false" {
			t.Errorf("enable=%q: SkipReason = %q, want %q", v, got.SkipReason, "dockpaum.enable=false")
		}
		if got.ComposeGroup != "alpha" {
			t.Errorf("enable=%q: ComposeGroup = %q, want %q (must survive skip)", v, got.ComposeGroup, "alpha")
		}
	}

	got := r.Resolve(testCt("web", nil), map[string]string{LabelEnable: "true"})
	if got.Skip {
		t.Fatalf("enable=true skipped: %q", got.SkipReason)
	}
}

func TestResolveNamedPolicy(t *testing.T) {
	policies := []config.Policy{
		{
			ID:            "critical",
			Mode:          config.ModePatch,
			MinImageAge:   config.Duration(2 * time.Hour),
			HealthTimeout: config.Duration(5 * time.Minute),
		},
		{ID: "lazy"},
	}
	r := testResolver(t, policies)

	t.Run("critical policy fields", func(t *testing.T) {
		got := r.Resolve(testCt("web", nil), map[string]string{LabelPolicy: "critical"})
		if got.Skip {
			t.Fatalf("skipped unexpectedly: %q", got.SkipReason)
		}
		if got.PolicyID != "critical" {
			t.Errorf("PolicyID = %q, want %q", got.PolicyID, "critical")
		}
		if got.Mode != config.ModePatch {
			t.Errorf("Mode = %q, want %q", got.Mode, config.ModePatch)
		}
		if got.MinImageAge != 2*time.Hour {
			t.Errorf("MinImageAge = %v, want %v", got.MinImageAge, 2*time.Hour)
		}
		if got.HealthTimeout != 5*time.Minute {
			t.Errorf("HealthTimeout = %v, want %v", got.HealthTimeout, 5*time.Minute)
		}
	})

	t.Run("policy id is case-insensitive", func(t *testing.T) {
		got := r.Resolve(testCt("web", nil), map[string]string{LabelPolicy: "CRITICAL"})
		if got.PolicyID != "critical" || got.Mode != config.ModePatch {
			t.Errorf("got (id=%q, mode=%q), want (critical, patch)", got.PolicyID, got.Mode)
		}
	})

	t.Run("empty mode defaults to tag-follow", func(t *testing.T) {
		got := r.Resolve(testCt("web", nil), map[string]string{LabelPolicy: "lazy"})
		if got.PolicyID != "lazy" {
			t.Errorf("PolicyID = %q, want %q", got.PolicyID, "lazy")
		}
		if got.Mode != config.ModeTagFollow {
			t.Errorf("Mode = %q, want %q", got.Mode, config.ModeTagFollow)
		}
	})
}

func TestResolveComposeGroup(t *testing.T) {
	r := testResolver(t, nil)

	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "compose_group label wins over project",
			labels: map[string]string{
				LabelComposeGroup:   "mygroup",
				ComposeProjectLabel: "alpha",
			},
			want: "mygroup",
		},
		{
			name:   "project used when no group label",
			labels: map[string]string{ComposeProjectLabel: "alpha"},
			want:   "alpha",
		},
		{
			name:   "no labels means no group",
			labels: map[string]string{},
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Resolve(testCt("web", nil), tc.labels)
			if got.ComposeGroup != tc.want {
				t.Fatalf("ComposeGroup = %q, want %q", got.ComposeGroup, tc.want)
			}
		})
	}
}

func TestResolveNilLabelsUsesContainerLabels(t *testing.T) {
	r := testResolver(t, []config.Policy{
		{ID: "critical", Mode: config.ModePatch},
	})
	ct := testCt("web", map[string]string{LabelPolicy: "critical"})
	got := r.Resolve(ct, nil)
	if got.PolicyID != "critical" {
		t.Fatalf("PolicyID = %q, want %q (must fall back to ct.Labels)", got.PolicyID, "critical")
	}
}

func TestResolveHooksAndSchedule(t *testing.T) {
	hooks := config.PolicyHooks{PreUpdate: []string{"host:/scripts/backup.sh"}}
	r := testResolver(t, []config.Policy{
		{ID: "", Mode: config.ModeTagFollow},
		{ID: "Database", Mode: config.ModeMinor, Schedule: "0 0 3 * * SUN", Timezone: "Europe/Berlin", Hooks: hooks},
		{ID: "web", Mode: config.ModePatch},
	})

	db := r.Resolve(testCt("db", map[string]string{LabelPolicy: "database"}), nil)
	if !db.Scheduled || db.PolicyID != "database" {
		t.Errorf("database: Scheduled=%v PolicyID=%q, want scheduled database", db.Scheduled, db.PolicyID)
	}
	if len(db.Hooks.PreUpdate) != 1 || db.Hooks.PreUpdate[0] != "host:/scripts/backup.sh" {
		t.Errorf("database hooks = %+v", db.Hooks)
	}
	web := r.Resolve(testCt("web", map[string]string{LabelPolicy: "web"}), nil)
	if web.Scheduled || len(web.Hooks.PreUpdate) != 0 {
		t.Errorf("web: Scheduled=%v hooks=%+v, want neither", web.Scheduled, web.Hooks)
	}

	got := r.Schedules()
	want := []Schedule{{PolicyID: "database", Spec: "CRON_TZ=Europe/Berlin 0 0 3 * * SUN"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Schedules() = %+v, want %+v", got, want)
	}
}

func TestSchedulesDefaultPolicy(t *testing.T) {
	r := testResolver(t, []config.Policy{{ID: "", Schedule: "@daily"}})
	got := r.Schedules()
	if len(got) != 1 || got[0].PolicyID != "default" || got[0].Spec != "@daily" {
		t.Fatalf("Schedules() = %+v, want the default policy", got)
	}
	if res := r.Resolve(testCt("x", nil), nil); !res.Scheduled || res.PolicyID != "default" {
		t.Errorf("unlabelled container: Scheduled=%v PolicyID=%q", res.Scheduled, res.PolicyID)
	}
}

func TestSynthesizedDefaultHasTheSafeGates(t *testing.T) {
	got := testResolver(t, nil).Resolve(testCt("web", nil), nil)
	if got.MinImageAge != config.DefaultMinImageAge || got.HealthTimeout != config.DefaultHealthTimeout {
		t.Errorf("built-in default gates = (%v, %v), want (%v, %v)",
			got.MinImageAge, got.HealthTimeout, config.DefaultMinImageAge, config.DefaultHealthTimeout)
	}
	got = testResolver(t, []config.Policy{{ID: "default"}}).Resolve(testCt("web", nil), nil)
	if got.MinImageAge != 0 || got.HealthTimeout != 0 {
		t.Errorf("configured default gates = (%v, %v), want (0, 0)", got.MinImageAge, got.HealthTimeout)
	}
}

func loadResolver(t *testing.T, yaml string) *Resolver {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); config.IsConfigEnv(name) {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "dockpaum.yml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Validate(path)
	if err != nil {
		t.Fatalf("config.Validate: %v", err)
	}
	r, err := NewResolver(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestPolicyWindowsInheritTheGlobalZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for name, yaml := range map[string]string{
		"schedule.timezone": "schedule:\n  timezone: Europe/Berlin\n",
		"top-level":         "timezone: Europe/Berlin\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := loadResolver(t, yaml+"update:\n  policies:\n    - id: \"\"\n      windows:\n        - days: [Mon-Fri]\n          times: [\"03:00-05:00\"]\n")
			w := r.Resolve(testCt("web", nil), nil).Windows
			monday := func(h, m int) time.Time { return time.Date(2026, 6, 15, h, m, 0, 0, berlin) }
			if !w.Allows(monday(3, 30)) {
				t.Error("03:30 Berlin must be inside the window")
			}
			if w.Allows(monday(5, 30)) {
				t.Error("05:30 Berlin (03:30 UTC) must be outside the window")
			}
		})
	}
}

func TestLoadedPolicySchedulesCarryTheirZone(t *testing.T) {
	r := loadResolver(t, "timezone: Europe/Berlin\nupdate:\n  policies:\n"+
		"    - id: db\n      schedule: \"30 3 * * SUN\"\n"+
		"    - id: web\n      timezone: UTC\n      schedule: \"@daily\"\n")
	want := []Schedule{
		{PolicyID: "db", Spec: "CRON_TZ=Europe/Berlin 30 3 * * SUN"},
		{PolicyID: "web", Spec: "CRON_TZ=UTC @daily"},
	}
	got := r.Schedules()
	if len(got) != len(want) {
		t.Fatalf("Schedules() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Schedules()[%d] = %+v, want %+v", i, got[i], want[i])
		}
		if _, err := config.ParseCron(got[i].Spec); err != nil {
			t.Errorf("ParseCron(%q): %v", got[i].Spec, err)
		}
	}
}
