package config

import (
	"strings"
	"testing"
)

func TestWatchtowerCompat(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("dry_run: true\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.WatchtowerCompat {
		t.Error("the Watchtower routes must be off by default")
	}
	c, err = decode([]byte("http:\n  watchtower_compat: true\n  update_api: true\n  token: t\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !c.HTTP.WatchtowerCompat || !c.Redacted().HTTP.WatchtowerCompat {
		t.Error("http.watchtower_compat: true must set it, and --print-config must show it")
	}
	if got := strings.Join(append(c.Warnings(), c.Notes()...), "\n"); strings.Contains(got, "watchtower_compat") {
		t.Errorf("a complete Watchtower API setup must not be flagged:\n%s", got)
	}
}

func TestWatchtowerCompatChecks(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("http:\n  enabled: false\n  watchtower_compat: true\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "line 3: http.watchtower_compat has no effect: http.enabled is false") {
		t.Errorf("warnings = %s, want the disabled listener named", w)
	}
	c, err = decode([]byte("http:\n  watchtower_compat: true\n  read_token: r\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "watchtower_compat") {
		t.Errorf("metrics-only Watchtower routes warned: %s", w)
	}
	if n := strings.Join(c.Notes(), "\n"); !strings.Contains(n, "line 2: http.watchtower_compat: only /v1/metrics is served; /v1/update also needs http.update_api") {
		t.Errorf("notes = %s, want the missing update_api explained", n)
	}
}

func TestReadTokenMustDifferFromToken(t *testing.T) {
	isolateEnv(t)
	err := decodeErr(t, "http:\n  token: same-secret\n  read_token: same-secret\n")
	if msg := err.Error(); !strings.Contains(msg, "dockpaum.yml:3: http.read_token: must differ from http.token") || strings.Contains(msg, "same-secret") {
		t.Errorf("err = %v, want the scoping problem named without the token", err)
	}
	cfg, err := decodeEnv([]byte("dry_run: true\n"), "dockpaum.yml", envMap{envHTTPAuth: "env-secret", envHTTPReadAuth: "env-secret"})
	if err == nil || !strings.Contains(err.Error(), "http.read_token: must differ") || strings.Contains(err.Error(), "env-secret") {
		t.Errorf("tokens from the environment: cfg %v, err = %v", cfg != nil, err)
	}
	if _, err := decode([]byte("http:\n  token: a\n  read_token: b\n"), "dockpaum.yml"); err != nil {
		t.Errorf("distinct tokens: %v", err)
	}
	if _, err := decode([]byte("http:\n  read_token: b\n"), "dockpaum.yml"); err != nil {
		t.Errorf("a read token alone: %v", err)
	}
}

func TestHTTPEnabledFromTheEnvironment(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		value string
		want  bool
	}{
		{"false", false}, {"FALSE", false}, {"0", false}, {"off", false}, {"Off", false}, {"OFF", false}, {"no", false},
		{"true", true}, {"True", true}, {"1", true}, {"on", true}, {"ON", true}, {"yes", true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			c, err := decodeEnv([]byte("dry_run: true\n"), "dockpaum.yml", envMap{envHTTPEnabled: tt.value})
			if err != nil {
				t.Fatal(err)
			}
			if c.HTTP.IsEnabled() != tt.want || *c.Redacted().HTTP.Enabled != tt.want {
				t.Errorf("%s=%s: enabled %v, want %v", envHTTPEnabled, tt.value, c.HTTP.IsEnabled(), tt.want)
			}
			if got := c.EnvOverrides(); len(got) != 1 || got[0] != (EnvOverride{Var: "DOCKPAUM_HTTP_ENABLED", Key: "http.enabled"}) {
				t.Errorf("EnvOverrides() = %v", got)
			}
		})
	}
	c, err := decodeEnv([]byte("dry_run: true\n"), "dockpaum.yml", envMap{envHTTPEnabled: "  "})
	if err != nil || !c.HTTP.IsEnabled() || len(c.EnvOverrides()) != 0 {
		t.Errorf("a blank DOCKPAUM_HTTP_ENABLED must count as unset: err %v", err)
	}
	if _, err := decodeEnv([]byte("dry_run: true\n"), "dockpaum.yml", envMap{envHTTPEnabled: "maybe"}); err == nil ||
		!strings.Contains(err.Error(), `DOCKPAUM_HTTP_ENABLED="maybe": want true or false`) {
		t.Errorf("err = %v, want the variable named", err)
	}
}

func TestHTTPEnabledEnvironmentWinsOverTheFile(t *testing.T) {
	isolateEnv(t)
	c, err := decodeEnv([]byte("http:\n  enabled: false\n  update_api: true\n  token: t\n"), "dockpaum.yml", envMap{envHTTPEnabled: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HTTP.IsEnabled() {
		t.Error("DOCKPAUM_HTTP_ENABLED=on must turn on a server the file disables")
	}
	c, err = decodeEnv([]byte("http:\n  enabled: true\n  update_api: true\n  token: t\n"), "dockpaum.yml", envMap{envHTTPEnabled: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.IsEnabled() {
		t.Error("DOCKPAUM_HTTP_ENABLED=off must turn off a server the file enables")
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "http.update_api has no effect: http.enabled is false") {
		t.Errorf("warnings = %s, want the unserved update API named", w)
	}
	if n := strings.Join(c.Notes(), "\n"); strings.Contains(n, "without authentication") {
		t.Errorf("a disabled server must not be reported as open: %s", n)
	}
}

func TestHTTPEnabledMisspeltVariableIsSuggested(t *testing.T) {
	isolateEnv(t)
	c, err := decodeEnv([]byte("dry_run: true\n"), "dockpaum.yml", envMap{"DOCKPAUM_HTTP_ENABLE": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HTTP.IsEnabled() {
		t.Error("a misspelt variable must not change http.enabled")
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "DOCKPAUM_HTTP_ENABLE is not a dockpaum setting and is ignored — did you mean DOCKPAUM_HTTP_ENABLED?") {
		t.Errorf("warnings = %s, want a suggestion", w)
	}
}

func TestHTTPEnabledAcceptsYAML11Booleans(t *testing.T) {
	isolateEnv(t)
	s := loadSchema(t)
	for value, want := range map[string]bool{
		"off": false, "Off": false, "OFF": false, "no": false, "false": false, "False": false,
		"on": true, "On": true, "yes": true, "true": true,
	} {
		doc := "http:\n  enabled: " + value + "\n"
		c, err := decode([]byte(doc), "dockpaum.yml")
		if err != nil {
			t.Errorf("http.enabled: %s: %v", value, err)
			continue
		}
		if c.HTTP.IsEnabled() != want {
			t.Errorf("http.enabled: %s = %v, want %v", value, c.HTTP.IsEnabled(), want)
		}
		if errs := s.check(t, []byte(doc)); len(errs) > 0 {
			t.Errorf("the schema rejects http.enabled: %s: %v", value, errs)
		}
	}
	if _, err := decode([]byte("http:\n  enabled: maybe\n"), "dockpaum.yml"); err == nil || !strings.Contains(err.Error(), "is not true or false") {
		t.Errorf("err = %v, want a bad boolean named", err)
	}
	c, err := decode([]byte("dry_run: true\n"), "dockpaum.yml")
	if err != nil || !c.HTTP.IsEnabled() {
		t.Errorf("the HTTP server must be on by default: err %v", err)
	}
}

func TestDisabledHTTPServerNotes(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("http:\n  enabled: off\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(c.Notes(), "\n"); strings.Contains(n, "http.token") {
		t.Errorf("notes = %s, want no authentication note for a server that is off", n)
	}
	c, err = decode([]byte("http:\n  watchtower_compat: true\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := "http.token: not set, so /internal-api/ health, ready, busy, version, metrics, status, containers and summary, and /v1/metrics, are served without authentication on :8080"
	if n := strings.Join(c.Notes(), "\n"); !strings.Contains(n, want) {
		t.Errorf("notes = %s, want %q", n, want)
	}
	for _, doc := range []string{"http:\n  token: t\n", "http:\n  read_token: r\n"} {
		c, err := decode([]byte(doc), "dockpaum.yml")
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Join(c.Notes(), "\n"); strings.Contains(n, "without authentication") {
			t.Errorf("%q: notes = %s", doc, n)
		}
	}
}
