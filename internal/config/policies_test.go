package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuiltinPolicy(t *testing.T) {
	base := Policy{
		ID: "default", Mode: ModeMinor, MinImageAge: Duration(6 * time.Hour), HealthTimeout: 0,
		MinImageAgeSource: AgeSourceCreated,
		Timezone:          "Europe/Berlin", Windows: []WindowSpec{{Days: []string{"sat"}}},
		Schedule: "@daily", Hooks: PolicyHooks{PreUpdate: []string{"x"}}, MonitorOnly: true,
		Tags: TagFilter{Exclude: "rc"},
	}
	for _, id := range BuiltinPolicyIDs() {
		p, ok := BuiltinPolicy(strings.ToUpper(id), base)
		if !ok || p.ID != id {
			t.Fatalf("BuiltinPolicy(%q) = %+v, %v", id, p, ok)
		}
		wantMode, wantMonitor := id, false
		if id == PolicyMonitor {
			wantMode, wantMonitor = ModeTagFollow, true
		}
		if p.Mode != wantMode || p.MonitorOnly != wantMonitor {
			t.Errorf("%s: mode %q monitor_only %v, want %q %v", id, p.Mode, p.MonitorOnly, wantMode, wantMonitor)
		}
		if p.MinImageAge != base.MinImageAge || p.MinImageAgeSource != AgeSourceCreated || p.HealthTimeout != 0 || p.Timezone != base.Timezone || len(p.Windows) != 1 {
			t.Errorf("%s does not carry the default policy's safety settings: %+v", id, p)
		}
		if p.Schedule != "" || len(p.Hooks.PreUpdate) != 0 || !p.Tags.IsZero() {
			t.Errorf("%s carries the default policy's schedule, hooks or tags: %+v", id, p)
		}
		if !p.minImageAgeSet || !p.healthTimeoutSet {
			t.Errorf("%s: an explicit 0s health gate must stay off", id)
		}
	}
	p, _ := BuiltinPolicy("minor", base)
	p.Windows[0].Days[0] = "sun"
	if base.Windows[0].Days[0] != "sat" {
		t.Error("BuiltinPolicy shares the windows slice with the default policy")
	}
	for _, id := range []string{"default", "none", "off", "digest", "db", ""} {
		if _, ok := BuiltinPolicy(id, base); ok {
			t.Errorf("BuiltinPolicy(%q) is a built-in", id)
		}
	}
	if !slices.Equal(BuiltinPolicyIDs(), []string{"tag-follow", "patch", "minor", "major", "monitor"}) {
		t.Errorf("BuiltinPolicyIDs() = %v", BuiltinPolicyIDs())
	}
}

func TestIsReservedPolicyID(t *testing.T) {
	for id, want := range map[string]bool{"none": true, " OFF ": true, "Off": true, "default": false, "": false, "monitor": false} {
		if got := IsReservedPolicyID(id); got != want {
			t.Errorf("IsReservedPolicyID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestTagFilter(t *testing.T) {
	f := TagFilter{Include: `^\d+\.\d+(\.\d+)?$`, Exclude: `^1\.`}
	include, exclude, err := f.Compile()
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[string]bool{"2.4": true, "2.4.1": true, "1.9": false, "2.4-rc1": false, "latest": false} {
		if got := TagAllowed(tag, include, exclude); got != want {
			t.Errorf("TagAllowed(%q) = %v, want %v", tag, got, want)
		}
	}
	if !TagAllowed("anything", nil, nil) || !(TagFilter{}).IsZero() || f.IsZero() {
		t.Error("an empty filter must let every tag through")
	}
	if inc, exc, err := (TagFilter{Include: "  "}).Compile(); err != nil || inc != nil || exc != nil {
		t.Errorf("blank expression = %v, %v, %v; want nil filters", inc, exc, err)
	}
	if _, _, err := (TagFilter{Exclude: "[z-a]"}).Compile(); err == nil || !strings.HasPrefix(err.Error(), `exclude: "[z-a]" is not a valid RE2 regular expression: invalid character class range: z-a`) {
		t.Errorf("err = %v", err)
	}
}

func TestPolicyFieldsDecode(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - id: db\n      mode: minor\n      monitor_only: true\n      tags:\n        include: '^\\d+\\.\\d+$'\n        exclude: beta\n"), "fields")
	if err != nil {
		t.Fatal(err)
	}
	db := policyByID(t, c, "db")
	if !db.MonitorOnly || db.Tags.Include != `^\d+\.\d+$` || db.Tags.Exclude != "beta" {
		t.Errorf("policy = %+v", db)
	}
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "health_timeout") {
		t.Errorf("a monitor-only policy never recreates, so its health gate is moot: %s", w)
	}
}

func TestTagFilterTransform(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - id: app\n      mode: minor\n      tags:\n        include: ' ^release-'\n        transform: '^release-(.+)$ => $1'\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	app := policyByID(t, c, "app")
	if app.Tags.Transform != "^release-(.+)$ => $1" || app.Tags.IsZero() {
		t.Errorf("tags = %+v", app.Tags)
	}
	if (TagFilter{Transform: "^v(.*)$ => $1"}).IsZero() {
		t.Error("a transform alone changes what is compared, so the filter is not zero")
	}
	include, _, err := app.Tags.Compile()
	if err != nil || include.String() != "^release-" {
		t.Errorf("include = %v, %v; want the expression without surrounding blanks, as the registry compiles it", include, err)
	}
	if w := warningsOf(t, "update:\n  policies:\n    - id: app\n      tags: {transform: '^v(.*)$ => $1'}\n"); !strings.Contains(w, `update.policies[0] (id="app").tags has no effect with mode tag-follow`) {
		t.Errorf("warnings = %s, want the transform on tag-follow flagged", w)
	}
	for transform, want := range map[string]string{
		"^v(.*)$":                  `tags.transform: "^v(.*)$": want "<regex> => <template>"`,
		"^v(.*)$ => $2":            `tags.transform: "^v(.*)$ => $2": template refers to unknown group "2"`,
		"^(v => $1":                `tags.transform: "^(v": error parsing regexp`,
		"^(\\d+)-(\\d+)$ => $1_$2": `template refers to unknown group "1_"`,
	} {
		err := decodeErr(t, "update:\n  policies:\n    - id: app\n      mode: minor\n      tags:\n        transform: '"+transform+"'\n")
		if !strings.Contains(err.Error(), `update.policies[0] (id="app").tags.transform: `) || !strings.Contains(err.Error(), want) {
			t.Errorf("transform %q: err = %v, want %q", transform, err, want)
		}
	}
	c, err = decode([]byte("update:\n  policies:\n    - id: app\n      mode: minor\n      tags:\n        transform: '^(?P<ver>[0-9.]+)-.*$ => $${ver}'\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := policyByID(t, c, "app").Tags.Transform; got != "^(?P<ver>[0-9.]+)-.*$ => ${ver}" {
		t.Errorf("transform = %q, want the named group kept", got)
	}
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "ver") {
		t.Errorf("an escaped group warned: %s", w)
	}
	long := strings.Repeat("a", 1025)
	if err := decodeErr(t, "update:\n  policies:\n    - id: app\n      tags:\n        exclude: "+long+"\n"); !strings.Contains(err.Error(), "tags.exclude: the expression is longer than 1024 characters") {
		t.Errorf("err = %v, want the registry's length limit", err)
	}
	err = decodeErr(t, "update:\n  policies:\n    - id: app\n      mode: minor\n      tags:\n        transform: '"+long+" => $1'\n")
	if want := `update.policies[0] (id="app").tags.transform: pattern longer than 1024 characters`; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), "tag transform") {
		t.Errorf("err = %v repeats the rule name the key already gives", err)
	}
}

func TestMinImageAgeSource(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  policies:\n    - id: db\n      min_image_age_source: ' First_Seen '\n    - id: web\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"db": AgeSourceFirstSeen, "web": AgeSourceAuto, "default": AgeSourceAuto} {
		if got := policyByID(t, c, id).MinImageAgeSource; got != want {
			t.Errorf("%s: min_image_age_source %q, want %q", id, got, want)
		}
	}
	if DefaultPolicy().MinImageAgeSource != AgeSourceAuto {
		t.Error("the fallback policy must show its clock")
	}
	err = decodeErr(t, "update:\n  policies:\n    - id: db\n      min_image_age_source: firstseen\n")
	if want := `dockpaum.yml:4: update.policies[0] (id="db").min_image_age_source: unknown source "firstseen" — did you mean "first_seen"?`; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	for _, s := range []string{"", AgeSourceAuto, AgeSourceFirstSeen, AgeSourceCreated} {
		if !ValidAgeSource(s) {
			t.Errorf("ValidAgeSource(%q) = false", s)
		}
	}
	if ValidAgeSource("Created") {
		t.Error("ValidAgeSource takes the lowercased value")
	}
}

func TestFirstSeenWithoutADurableState(t *testing.T) {
	isolateEnv(t)
	const warning = "min_image_age_source: first_seen needs the durable state, but state.enabled is false"
	w := warningsOf(t, "state:\n  enabled: false\nupdate:\n  policies:\n    - id: db\n      min_image_age_source: first_seen\n    - id: web\n      min_image_age_source: created\n")
	if want := `line 6: update.policies[0] (id="db").` + warning; !strings.Contains(w, want) {
		t.Errorf("warnings = %s, want %q", w, want)
	}
	if strings.Count(w, warning) != 1 {
		t.Errorf("warnings = %s, want only the first_seen policy flagged", w)
	}
	for name, doc := range map[string]string{
		"durable state":      "update:\n  policies:\n    - id: db\n      min_image_age_source: first_seen\n",
		"no soak":            "state:\n  enabled: false\nupdate:\n  policies:\n    - id: db\n      min_image_age: 0s\n      min_image_age_source: first_seen\n",
		"no updates":         "state:\n  enabled: false\nupdate:\n  policies:\n    - id: db\n      mode: none\n      min_image_age_source: first_seen\n",
		"the default clock":  "state:\n  enabled: false\n",
		"the creation clock": "state:\n  enabled: false\nupdate:\n  policies:\n    - id: db\n      min_image_age_source: created\n",
	} {
		if w := warningsOf(t, doc); strings.Contains(w, "first_seen") {
			t.Errorf("%s: unexpected warning:\n%s", name, w)
		}
	}
}
