package policy

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestUnknownPolicyFailsClosed(t *testing.T) {
	var logs bytes.Buffer
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{{ID: "default"}, {ID: "database", Mode: config.ModeMinor}}}}
	r, err := NewResolver(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{LabelPolicy: "Databse", ComposeProjectLabel: "shop"}
	for range 3 {
		got, err := r.ResolveStrict(testCt("db", nil), labels)
		var rerr *ResolveError
		if !errors.As(err, &rerr) {
			t.Fatalf("err = %v, want a *ResolveError", err)
		}
		if rerr.Label != LabelPolicy || rerr.Value != "Databse" {
			t.Errorf("ResolveError = %+v", rerr)
		}
		if !got.Skip || got.PolicyID != "databse" || got.ComposeGroup != "shop" {
			t.Errorf("Resolved = %+v, want a skipped databse in group shop", got)
		}
		for _, want := range []string{`dockpaum.policy="Databse": unknown policy`, `did you mean "database"?`, "known: database, default, major, minor, monitor, none, off, patch, tag-follow"} {
			if !strings.Contains(got.SkipReason, want) {
				t.Errorf("SkipReason %q lacks %q", got.SkipReason, want)
			}
		}
		if res := r.Resolve(testCt("db", nil), labels); !res.Skip || res.SkipReason != got.SkipReason {
			t.Errorf("Resolve = %+v, want the same skip", res)
		}
	}
	if n := strings.Count(logs.String(), "do not resolve"); n != 1 {
		t.Errorf("warned %d times, want once per container and value:\n%s", n, logs.String())
	}
}

func TestBuiltinPolicyIsLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{{ID: "default"}, {ID: "minor", Mode: config.ModeMinor}}}}
	r, err := NewResolver(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for _, c := range []struct{ name, policy string }{{"pg", "major"}, {"redis", "major"}, {"web", "minor"}, {"app", ""}} {
			if res := r.Resolve(testCt(c.name, nil), map[string]string{LabelPolicy: c.policy}); res.Skip {
				t.Fatalf("%s: %+v", c.name, res)
			}
		}
	}
	out := logs.String()
	if n := strings.Count(out, "uses a built-in policy"); n != 2 {
		t.Errorf("logged %d times, want once for each container on a built-in:\n%s", n, out)
	}
	if !strings.Contains(out, "container=pg policy=major mode=major") || !strings.Contains(out, "container=redis") {
		t.Errorf("the line must name the container, the policy and its mode:\n%s", out)
	}
	if strings.Contains(out, "container=web") || strings.Contains(out, "container=app") {
		t.Errorf("configured and default policies are not logged:\n%s", out)
	}
}

func TestBuiltinPolicies(t *testing.T) {
	r := loadResolver(t, `
timezone: Europe/Berlin
update:
  policies:
    - id: default
      min_image_age: 6h
      health_timeout: 5m
      windows: [{days: [Sat], times: ["02:00-04:00"]}]
      schedule: "0 3 * * SAT"
      hooks: {pre_update: ["host:/backup.sh"]}
      tags: {exclude: "-rc"}
`)
	saturday3 := time.Date(2026, 6, 13, 3, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	tests := []struct {
		label   string
		mode    string
		monitor bool
	}{
		{"tag-follow", config.ModeTagFollow, false},
		{"patch", config.ModePatch, false},
		{"Minor", config.ModeMinor, false},
		{"major", config.ModeMajor, false},
		{"monitor", config.ModeTagFollow, true},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got, err := r.ResolveStrict(testCt("app", nil), map[string]string{LabelPolicy: tt.label})
			if err != nil {
				t.Fatal(err)
			}
			if got.Skip || !got.Builtin || got.PolicyID != strings.ToLower(tt.label) || got.Mode != tt.mode {
				t.Fatalf("Resolved = %+v", got)
			}
			if got.MonitorOnly != tt.monitor || got.Labels.MonitorOnly != tt.monitor {
				t.Errorf("MonitorOnly = %v (labels %v), want %v", got.MonitorOnly, got.Labels.MonitorOnly, tt.monitor)
			}
			if got.MinImageAge != 6*time.Hour || got.HealthTimeout != 5*time.Minute {
				t.Errorf("gates = (%v, %v), want the default policy's (6h, 5m)", got.MinImageAge, got.HealthTimeout)
			}
			if !got.Windows.Allows(saturday3) || got.Windows.Allows(saturday3.Add(2*time.Hour)) {
				t.Error("the default policy's window (Saturday 02:00-04:00 Berlin) must apply")
			}
			if got.Scheduled || len(got.Hooks.PreUpdate) != 0 || got.TagExclude != nil {
				t.Errorf("Scheduled=%v hooks=%v tags=%v, want none", got.Scheduled, got.Hooks, got.TagExclude)
			}
		})
	}
	if s := r.Schedules(); len(s) != 1 || s[0].PolicyID != "default" {
		t.Errorf("Schedules() = %+v, want only the default policy's", s)
	}
}

func TestBuiltinWithoutAnyConfig(t *testing.T) {
	got := testResolver(t, nil).Resolve(testCt("pg", nil), map[string]string{LabelPolicy: "minor"})
	if got.Skip || got.Mode != config.ModeMinor || got.MinImageAge != config.DefaultMinImageAge || got.HealthTimeout != config.DefaultHealthTimeout {
		t.Errorf("minor without config = %+v, want minor with the code default gates", got)
	}
}

func TestConfiguredPolicyReplacesBuiltin(t *testing.T) {
	r := testResolver(t, []config.Policy{{ID: "minor", Mode: config.ModePatch, MinImageAge: config.Duration(time.Hour)}, {ID: "Monitor", Mode: config.ModeMajor}})
	got := r.Resolve(testCt("pg", nil), map[string]string{LabelPolicy: "minor"})
	if got.Builtin || got.Mode != config.ModePatch {
		t.Errorf("minor = %+v, want the configured policy (patch)", got)
	}
	got = r.Resolve(testCt("pg", nil), map[string]string{LabelPolicy: "monitor"})
	if got.Builtin || got.MonitorOnly || got.Mode != config.ModeMajor {
		t.Errorf("monitor = %+v, want the configured policy (major, not monitor-only)", got)
	}
}

func TestOverrideLabels(t *testing.T) {
	r := testResolver(t, []config.Policy{
		{ID: "default", MinImageAge: config.Duration(time.Hour), HealthTimeout: config.Duration(2 * time.Minute)},
		{ID: "db", Mode: config.ModeMinor, Tags: config.TagFilter{Include: `^\d+\.\d+$`, Exclude: "beta"}},
	})
	tests := []struct {
		name   string
		labels map[string]string
		check  func(Resolved) bool
	}{
		{"no overrides", map[string]string{LabelPolicy: "db"}, func(r Resolved) bool {
			return r.Mode == config.ModeMinor && len(r.Overrides) == 0 && r.TagAllowed("16.2") && !r.TagAllowed("16") && !r.TagAllowed("16.2beta")
		}},
		{"mode on the default policy", map[string]string{LabelMode: "Major"}, func(r Resolved) bool {
			return r.PolicyID == "default" && r.Mode == config.ModeMajor
		}},
		{"digest is tag-follow", map[string]string{LabelPolicy: "db", LabelMode: "digest"}, func(r Resolved) bool { return r.Mode == config.ModeTagFollow }},
		{"mode none", map[string]string{LabelMode: "none"}, func(r Resolved) bool { return r.Mode == config.ModeNone && !r.Skip }},
		{"gates", map[string]string{LabelMinImageAge: "1d", LabelHealthTimeout: "0s"}, func(r Resolved) bool {
			return r.MinImageAge == 24*time.Hour && r.HealthTimeout == 0 && strings.Join(r.Overrides, ",") == LabelHealthTimeout+","+LabelMinImageAge
		}},
		{"bare seconds", map[string]string{LabelHealthTimeout: "300"}, func(r Resolved) bool { return r.HealthTimeout == 5*time.Minute }},
		{"short min-age spelling", map[string]string{LabelMinAge: "2h"}, func(r Resolved) bool {
			return r.MinImageAge == 2*time.Hour && strings.Join(r.Overrides, ",") == LabelMinAge
		}},
		{"min-image-age wins over min-age", map[string]string{LabelMinAge: "2h", LabelMinImageAge: "3h"}, func(r Resolved) bool {
			return r.MinImageAge == 3*time.Hour && strings.Join(r.Overrides, ",") == LabelMinImageAge
		}},
		{"tag filters replace the policy's", map[string]string{LabelPolicy: "db", LabelTagExclude: "-alpine$"}, func(r Resolved) bool {
			return r.TagAllowed("16.2beta") == false && r.TagAllowed("16.2") && !r.TagAllowed("16.2-alpine")
		}},
		{"blank values count as unset", map[string]string{LabelMode: " ", LabelMinImageAge: "", LabelTagInclude: ""}, func(r Resolved) bool {
			return r.Mode == config.ModeTagFollow && r.MinImageAge == time.Hour && r.TagInclude == nil && len(r.Overrides) == 0
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.ResolveStrict(testCt("c", nil), tt.labels)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(got) {
				t.Errorf("Resolved = %+v", got)
			}
		})
	}
}

func TestInvalidOverrideLabelsFailClosed(t *testing.T) {
	r := testResolver(t, []config.Policy{{ID: "db", Mode: config.ModeMinor}})
	tests := []struct {
		labels map[string]string
		label  string
		reason string
	}{
		{map[string]string{LabelMode: "minr"}, LabelMode, `unknown mode — did you mean "minor"?`},
		{map[string]string{LabelPolicy: "db", LabelMinImageAge: "1 day"}, LabelMinImageAge, "not a duration"},
		{map[string]string{LabelMinAge: "soon"}, LabelMinAge, "not a duration"},
		{map[string]string{LabelHealthTimeout: "-5m"}, LabelHealthTimeout, "must not be negative"},
		{map[string]string{LabelTagInclude: "(16"}, LabelTagInclude, "not a valid RE2 regular expression"},
		{map[string]string{LabelTagExclude: "(?!rc)"}, LabelTagExclude, "not a valid RE2 regular expression"},
		{map[string]string{LabelTagTransform: "^v(.*)$"}, LabelTagTransform, `want "<regex> => <template>"`},
		{map[string]string{LabelTagTransform: "^v(.*)$ => $2"}, LabelTagTransform, `template refers to unknown group "2"`},
		{map[string]string{LabelMinImageAgeSource: "firstseen"}, LabelMinImageAgeSource, `unknown source — did you mean "first_seen"?`},
		{map[string]string{LabelMonitorOnly: "maybe"}, LabelMonitorOnly, "want true or false"},
		{map[string]string{WatchtowerPrefix + "monitor-only": "perhaps"}, WatchtowerPrefix + "monitor-only", "want true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got, err := r.ResolveStrict(testCt("c", nil), tt.labels)
			var rerr *ResolveError
			if !errors.As(err, &rerr) || rerr.Label != tt.label {
				t.Fatalf("err = %v, want a *ResolveError for %s", err, tt.label)
			}
			if !got.Skip || !strings.Contains(got.SkipReason, tt.reason) || !strings.HasPrefix(got.SkipReason, tt.label+"=") {
				t.Errorf("Resolved = %+v, want a skip naming %s: %s", got, tt.label, tt.reason)
			}
		})
	}
}

func TestPolicyMonitorOnly(t *testing.T) {
	r := testResolver(t, []config.Policy{{ID: "watch", Mode: config.ModeMinor, MonitorOnly: true}})
	got := r.Resolve(testCt("c", nil), map[string]string{LabelPolicy: "watch"})
	if !got.MonitorOnly || !got.Labels.MonitorOnly || got.Mode != config.ModeMinor {
		t.Errorf("watch = %+v, want monitor-only minor", got)
	}
	got = r.Resolve(testCt("c", nil), map[string]string{LabelPolicy: "watch", LabelMonitorOnly: "false"})
	if got.MonitorOnly || got.Labels.MonitorOnly {
		t.Error("dockpaum.monitor-only=false must override the policy's monitor_only")
	}
	got = r.Resolve(testCt("c", nil), map[string]string{LabelMonitorOnly: "true"})
	if !got.MonitorOnly || got.PolicyID != "default" {
		t.Errorf("label monitor-only on the default policy = %+v", got)
	}
}

func TestComposeGroupHyphenLabel(t *testing.T) {
	r := testResolver(t, nil)
	for want, labels := range map[string]map[string]string{
		"hyphen":     {LabelComposeGroupHyphen: "hyphen", ComposeProjectLabel: "p"},
		"both-wins":  {LabelComposeGroupHyphen: "both-wins", LabelComposeGroup: "underscore"},
		"underscore": {LabelComposeGroup: "underscore"},
	} {
		if got := r.Resolve(testCt("c", nil), labels).ComposeGroup; got != want {
			t.Errorf("ComposeGroup(%v) = %q, want %q", labels, got, want)
		}
	}
}

func TestReadOverridesListsLabels(t *testing.T) {
	ov, err := ReadOverrides(map[string]string{LabelTagInclude: `^\d+$`, LabelMode: "patch", "other": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ov.Labels, ",") != LabelMode+","+LabelTagInclude || ov.Mode != config.ModePatch || ov.TagInclude == nil {
		t.Errorf("ReadOverrides = %+v", ov)
	}
	if _, err := ReadOverrides(nil); err != nil {
		t.Errorf("no labels: %v", err)
	}
}

func TestIDs(t *testing.T) {
	r := testResolver(t, []config.Policy{{ID: "DB"}, {ID: "minor", Mode: config.ModeMinor}})
	want := "db,default,major,minor,monitor,none,off,patch,tag-follow"
	if got := strings.Join(r.IDs(), ","); got != want {
		t.Errorf("IDs() = %s, want %s", got, want)
	}
}

func TestNewResolverRejectsBadTags(t *testing.T) {
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{{ID: "x", Tags: config.TagFilter{Exclude: "("}}}}}
	if _, err := NewResolver(cfg, slog.Default()); err == nil || !strings.Contains(err.Error(), "tags.exclude") {
		t.Errorf("err = %v, want a tags.exclude error", err)
	}
}

func TestTransformLabelReasonDoesNotRepeatTheValue(t *testing.T) {
	_, err := ReadOverrides(map[string]string{LabelTagTransform: "^(v => $1"})
	var rerr *ResolveError
	if !errors.As(err, &rerr) {
		t.Fatalf("err = %v, want a *ResolveError", err)
	}
	if !strings.HasPrefix(rerr.Reason, "error parsing regexp") || strings.Contains(rerr.Error(), `"^(v => $1": "`) {
		t.Errorf("ResolveError = %q (reason %q), want the value only once in front", rerr.Error(), rerr.Reason)
	}
	_, err = ReadOverrides(map[string]string{LabelTagTransform: strings.Repeat("a", 1100) + " => x"})
	if !errors.As(err, &rerr) || rerr.Reason != "pattern longer than 1024 characters" {
		t.Errorf("err = %v, want the length limit as the reason", err)
	}
}

func TestTagRules(t *testing.T) {
	r := testResolver(t, []config.Policy{
		{ID: "app", Mode: config.ModeMinor, Tags: config.TagFilter{Include: `^release-`, Exclude: `-rc`, Transform: `^release-(.+)$ => $1`}},
		{ID: "plain", Mode: config.ModeMinor},
	})
	tests := []struct {
		name   string
		labels map[string]string
		check  func(Resolved) bool
	}{
		{"policy rules", map[string]string{LabelPolicy: "app"}, func(r Resolved) bool {
			return r.Tags.Transform("release-1.2.3") == "1.2.3" && r.Tags.Allows("release-1.2.4") && !r.Tags.Allows("release-1.3.0-rc1") &&
				!r.Tags.Allows("1.2.4") && r.TagAllowed("release-1.2.4") && !r.TagAllowed("1.2.4")
		}},
		{"no rules", map[string]string{LabelPolicy: "plain"}, func(r Resolved) bool { return r.Tags.IsZero() }},
		{"a label replaces only its own rule", map[string]string{LabelPolicy: "app", LabelTagInclude: `^release-1\.`}, func(r Resolved) bool {
			return !r.Tags.Allows("release-2.0.0") && r.Tags.Allows("release-1.9.0") && !r.Tags.Allows("release-1.9.0-rc1") &&
				r.Tags.Transform("release-1.9.0") == "1.9.0" && !r.TagAllowed("release-2.0.0")
		}},
		{"transform label on a policy without tags", map[string]string{LabelPolicy: "plain", LabelTagTransform: `^v(.+)$ => $1`}, func(r Resolved) bool {
			return r.Tags.Transform("v2.1") == "2.1" && r.Tags.Allows("v2.1") && strings.Join(r.Overrides, ",") == LabelTagTransform
		}},
		{"blank labels keep the policy's rules", map[string]string{LabelPolicy: "app", LabelTagTransform: " ", LabelTagExclude: ""}, func(r Resolved) bool {
			return r.Tags.Transform("release-1.2.3") == "1.2.3" && !r.Tags.Allows("release-1.3.0-rc1") && len(r.Overrides) == 0
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.ResolveStrict(testCt("c", nil), tt.labels)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(got) {
				t.Errorf("Resolved = %+v", got)
			}
		})
	}
}

func TestResolvedMinImageAgeSource(t *testing.T) {
	r := testResolver(t, []config.Policy{
		{ID: "default", MinImageAgeSource: config.AgeSourceFirstSeen},
		{ID: "db", Mode: config.ModeMinor, MinImageAgeSource: "Created"},
		{ID: "web"},
	})
	for _, tt := range []struct {
		labels map[string]string
		want   string
	}{
		{nil, config.AgeSourceFirstSeen},
		{map[string]string{LabelPolicy: "db"}, config.AgeSourceCreated},
		{map[string]string{LabelPolicy: "web"}, config.AgeSourceAuto},
		{map[string]string{LabelPolicy: "minor"}, config.AgeSourceFirstSeen},
		{map[string]string{LabelPolicy: "db", LabelMinImageAgeSource: "AUTO"}, config.AgeSourceAuto},
		{map[string]string{LabelPolicy: "web", LabelMinImageAgeSource: "first_seen"}, config.AgeSourceFirstSeen},
	} {
		got, err := r.ResolveStrict(testCt("c", nil), tt.labels)
		if err != nil {
			t.Fatal(err)
		}
		if got.MinImageAgeSource != tt.want {
			t.Errorf("%v: MinImageAgeSource %q, want %q", tt.labels, got.MinImageAgeSource, tt.want)
		}
	}
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{{ID: "x", MinImageAgeSource: "sometimes"}}}}
	if _, err := NewResolver(cfg, slog.Default()); err == nil || !strings.Contains(err.Error(), "min_image_age_source") {
		t.Errorf("err = %v, want an unknown source rejected", err)
	}
}

func TestNewResolverRejectsBadTransform(t *testing.T) {
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{{ID: "x", Tags: config.TagFilter{Transform: "^v(.*)$"}}}}}
	if _, err := NewResolver(cfg, slog.Default()); err == nil || !strings.Contains(err.Error(), "tags: tag transform") {
		t.Errorf("err = %v, want a tags transform error", err)
	}
}
