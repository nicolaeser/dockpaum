package config

import (
	"errors"
	"strings"
	"testing"
)

func TestExpandEnvOperators(t *testing.T) {
	env := map[string]string{"SET": "value", "EMPTY": "", "HOST": "db"}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	tests := []struct {
		in, want string
		missing  string
	}{
		{in: "${SET:-d}", want: "value"},
		{in: "${UNSET:-24h}", want: "24h"},
		{in: "${EMPTY:-d}", want: "d"},
		{in: "${EMPTY-d}", want: ""},
		{in: "${UNSET-d}", want: "d"},
		{in: "${UNSET:-}", want: ""},
		{in: "${UNSET:-${HOST}:5432}", want: "db:5432"},
		{in: "${UNSET:-${ALSO_UNSET:-x}}", want: "x"},
		{in: "${SET:+on}", want: "on"},
		{in: "${EMPTY:+on}", want: ""},
		{in: "${EMPTY+on}", want: "on"},
		{in: "${UNSET+on}", want: ""},
		{in: "${SET:?needed}", want: "value"},
		{in: "${EMPTY?needed}", want: ""},
		{in: "t: ${UNSET:?set the API token}", want: "t: ", missing: "UNSET"},
		{in: "t: ${EMPTY:?}", want: "t: ", missing: "EMPTY"},
		{in: "t: ${UNSET?}", want: "t: ", missing: "UNSET"},
		{in: "${DOCKPAUM_TARGET:-x}", want: "${DOCKPAUM_TARGET:-x}"},
		{in: "${UNSET:-$$5}", want: "$5"},
		{in: "${1BAD}", want: "${1BAD}"},
		{in: "${SET:x}", want: "${SET:x}"},
		{in: "${UNCLOSED\nnext: ${SET}", want: "${UNCLOSED\nnext: value"},
		{in: "# t: ${UNSET:?commented out}\nx: 1", want: "# t: \nx: 1"},
		{in: "x: 1 # ${UNSET:?inline comment}", want: "x: 1 # "},
		{in: "x: \"a # ${UNSET:?in quotes}\"", want: "x: \"a # \"", missing: "UNSET"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := expandEnv(tt.in, lookup)
			if got != tt.want {
				t.Errorf("expandEnv(%q) = %q, want %q", tt.in, got, tt.want)
			}
			var rerr *requiredVarError
			switch {
			case tt.missing == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.missing != "" && (!errors.As(err, &rerr) || rerr.name != tt.missing):
				t.Errorf("err = %v, want a required-variable error for %s", err, tt.missing)
			}
		})
	}
}

func TestRequiredVariableFailsTheLoadWithItsLine(t *testing.T) {
	isolateEnv(t)
	_, err := decode([]byte("dry_run: false\nhttp:\n  token: ${DPTEST_TOKEN:?set the API token}\n"), "dockpaum.yml")
	if err == nil || err.Error() != "dockpaum.yml:3: ${DPTEST_TOKEN} is not set: set the API token" {
		t.Errorf("err = %v", err)
	}
	t.Setenv("DPTEST_TOKEN", "s3cret")
	c, err := decode([]byte("http:\n  token: ${DPTEST_TOKEN:?set the API token}\nschedule:\n  interval: ${DPTEST_INTERVAL:-24h}\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Token != "s3cret" || c.Schedule.Interval.Std().Hours() != 24 {
		t.Errorf("token %q, interval %v", c.HTTP.Token, c.Schedule.Interval)
	}
}

func TestEnvWarnings(t *testing.T) {
	isolateEnv(t)
	t.Setenv("DPTEST_SET", "x")
	c, err := decode([]byte(`dry_run: false
http:
  token: ${DPTEST_UNSET_TOKEN}
  read_token: ${DPTEST_UNSET_READ:-}
filters:
  scope: ${DOCKPAUM_SCOPE}
hooks:
  pre_update: ["echo $DPTEST_SET", "echo $$HOME ${DOCKPAUM_TARGET}"]
  post_run: echo $DPTEST_UNSET_HOOK
jobs:
  - name: j
    cron: "@daily"
    commands: ["echo ${DPTEST_SET}"]
`), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{
		"line 3: http.token: ${DPTEST_UNSET_TOKEN} is not set, so the value is empty — set it, or write ${DPTEST_UNSET_TOKEN:-} if an empty value is intended",
		"line 6: filters.scope: ${DOCKPAUM_SCOPE} stays literal here — ${DOCKPAUM_*} references are expanded only in hooks and jobs; DOCKPAUM_SCOPE is read from the environment by itself",
		"line 8: hooks.pre_update[0]: $DPTEST_SET is replaced when the configuration is loaded, not when the command runs — write $$DPTEST_SET to leave it to the shell",
		"line 9: hooks.post_run: $DPTEST_UNSET_HOOK is replaced when the configuration is loaded, not when the command runs — write $$DPTEST_UNSET_HOOK to leave it to the shell (it is not set, so the command gets an empty value)",
		"line 13: jobs[0].commands[0]: $DPTEST_SET is replaced",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings lack %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"DPTEST_UNSET_READ", "HOME", "DOCKPAUM_TARGET"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("warnings mention %s, which is fine as written:\n%s", unwanted, got)
		}
	}
}
