package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/server"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); config.IsConfigEnv(name) {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPlural(t *testing.T) {
	if plural(1) != "" {
		t.Errorf("plural(1) = %q, want empty", plural(1))
	}
	for _, n := range []int{0, 2, 5, 100} {
		if plural(n) != "s" {
			t.Errorf("plural(%d) = %q, want \"s\"", n, plural(n))
		}
	}
}

func TestRunHealthProbeHTTPDisabled(t *testing.T) {
	clearConfigEnv(t)
	p := writeConfig(t, "http: {enabled: false}\n")
	if code := runHealthProbe(p, io.Discard); code != 0 {
		t.Fatalf("runHealthProbe with http disabled = %d, want 0", code)
	}
}

func TestRunHealthProbeBadConfig(t *testing.T) {
	clearConfigEnv(t)
	p := writeConfig(t, "schedule: {cron: \"nonsense\"}\n")
	if code := runHealthProbe(p, io.Discard); code == 0 {
		t.Fatal("runHealthProbe with invalid config should return nonzero")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dockpaum.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func execute(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunCommands(t *testing.T) {
	clearConfigEnv(t)
	cfg := writeConfig(t, "hosts: [{name: local}]\nfilters: {include_names: [web, db]}\n")
	bad := writeConfig(t, "schedule: {cron: \"nonsense\"}\n")
	empty := writeConfig(t, "\n")
	warn := writeConfig(t, "notify: [{type: webhook, url: \"http://example.com/hook\"}]\n")
	cases := []struct {
		name      string
		args      []string
		code      int
		stdout    []string
		stderr    []string
		notStdout []string
		notStderr []string
	}{
		{name: "version flag", args: []string{"--version"}, code: 0, stdout: []string{"dockpaum ", "commit ", "go1."}},
		{name: "version command", args: []string{"version"}, code: 0, stdout: []string{"dockpaum "}},
		{name: "version json flag", args: []string{"--version", "--json"}, code: 0, stdout: []string{`"version"`, `"go"`, `"platform"`}},
		{name: "validate flag", args: []string{"--validate", "--config", cfg}, code: 0, stdout: []string{"config OK: " + cfg}},
		{name: "validate command", args: []string{"validate", "--config", cfg}, code: 0, stdout: []string{"config OK: " + cfg}},
		{name: "validate invalid", args: []string{"validate", "--config", bad}, code: 2, stderr: []string{"schedule.cron"}},
		{name: "validate missing file uses default", args: []string{"validate", "--config", filepath.Join(t.TempDir(), "absent.yml")}, code: 0, stdout: []string{"embedded default is valid"}},
		{name: "validate empty file uses default", args: []string{"validate", "--config", empty}, code: 0, stdout: []string{"config OK: " + empty + " not present or empty; embedded default is valid"}},
		{name: "validate prints notes", args: []string{"validate", "--config", cfg}, code: 0, stderr: []string{"note: http.token: not set"}},
		{name: "validate strict with warnings", args: []string{"validate", "--strict", "--config", warn}, code: 3, stderr: []string{"warning: ", "--strict"}, notStdout: []string{"config OK"}},
		{name: "validate flag strict with warnings", args: []string{"--validate", "--strict", "--config", warn}, code: 3},
		{name: "validate warnings without strict", args: []string{"validate", "--config", warn}, code: 0, stdout: []string{"config OK"}, stderr: []string{"warning: "}},
		{name: "validate strict ignores notes", args: []string{"validate", "--strict", "--config", cfg}, code: 0, stdout: []string{"config OK: " + cfg}, stderr: []string{"note: "}},
		{name: "strict without validate", args: []string{"--strict", "--config", cfg}, code: 2, stderr: []string{"--strict applies to --validate"}},
		{name: "print-config shows watchtower_compat", args: []string{"print-config", "--config", cfg}, code: 0, stdout: []string{"watchtower_compat: false"}},
		{name: "schema", args: []string{"schema"}, code: 0, stdout: []string{`"$schema"`, `"watchtower_compat"`}},
		{name: "schema takes no arguments", args: []string{"schema", "web"}, code: 2, stderr: []string{`unexpected argument "web"`}},
		{name: "notify-test without notifiers", args: []string{"notify-test", "--config", cfg}, code: 1, stderr: []string{"no notifiers configured"}},
		{name: "notify-test invalid config", args: []string{"notify-test", "--config", bad}, code: 2, stderr: []string{"schedule.cron"}},
		{name: "notify-name without notify-test", args: []string{"--notify-name", "ops", "--config", cfg}, code: 2, stderr: []string{"--notify-name applies to --notify-test"}},
		{name: "validate with names", args: []string{"--validate", "--config", cfg, "web", "redis"}, code: 0, stdout: []string{"config OK", "managing only web"}, notStdout: []string{"redis"}},
		{name: "validate names outside include_names", args: []string{"--validate", "--config", cfg, "redis"}, code: 2, stderr: []string{"not in filters.include_names"}},
		{name: "print-config flag", args: []string{"--print-config", "--config", cfg}, code: 0, stdout: []string{"include_names:", "- web", "- db"}},
		{name: "print-config command with names", args: []string{"print-config", "--config", cfg, "db"}, code: 0, stdout: []string{"include_names:", "- db"}, notStdout: []string{"- web"}},
		{name: "flag after a name", args: []string{"--config", cfg, "nginx", "--once"}, code: 2, stderr: []string{"flag --once must come before the container names"}},
		{name: "flag after names in run", args: []string{"run", "nginx", "redis", "-dry-run"}, code: 2, stderr: []string{"flag -dry-run must come before"}},
		{name: "command after flags", args: []string{"--config", cfg, "validate"}, code: 2, stderr: []string{`"validate" is a command: put it first`}},
		{name: "invalid container name", args: []string{"--validate", "--config", cfg, "bad name!"}, code: 2, stderr: []string{"not a valid container name"}},
		{name: "validate with a file argument", args: []string{"validate", bad}, code: 2, stderr: []string{"looks like a config file", "--config " + bad}, notStdout: []string{"config OK"}},
		{name: "validate flag with a file name", args: []string{"--validate", "dockpaum.yml"}, code: 2, stderr: []string{`"dockpaum.yml" looks like a config file`}, notStdout: []string{"config OK"}},
		{name: "print-config with a file name", args: []string{"print-config", "prod.YAML"}, code: 2, stderr: []string{"looks like a config file"}, notStdout: []string{"include_names"}},
		{name: "daemon with a file name", args: []string{"--once", "config.json"}, code: 2, stderr: []string{"--config config.json"}},
		{name: "name after terminator", args: []string{"--validate", "--config", cfg, "--", "-x"}, code: 2, stderr: []string{"not a valid container name"}},
		{name: "unknown flag", args: []string{"--nope"}, code: 2, stderr: []string{"flag provided but not defined: -nope", "dockpaum help"}},
		{name: "watchtower flag", args: []string{"--interval", "30", "nginx"}, code: 2, stderr: []string{"--interval is a Watchtower flag", "migrating-from-watchtower.md"}},
		{name: "watchtower run-once", args: []string{"-run-once"}, code: 2, stderr: []string{"--run-once is --once"}},
		{name: "watchtower monitor-only", args: []string{"--monitor-only"}, code: 2, stderr: []string{"--dry-run"}},
		{name: "watchtower short flag", args: []string{"-i", "30"}, code: 2, stderr: []string{"-i (--interval) is a Watchtower flag", "migrating-from-watchtower.md"}},
		{name: "watchtower short run-once", args: []string{"-R"}, code: 2, stderr: []string{"Watchtower's -R (--run-once) is --once"}},
		{name: "watchtower short monitor-only", args: []string{"-m"}, code: 2, stderr: []string{"-m (--monitor-only) is --dry-run"}},
		{name: "watchtower notification family", args: []string{"--notification-email-from", "a@b"}, code: 2, stderr: []string{"--notification-email-from is a Watchtower flag"}},
		{name: "watchtower health-check", args: []string{"--health-check"}, code: 2, stderr: []string{"--healthcheck in dockpaum"}},
		{name: "v is not version", args: []string{"-v"}, code: 2, stderr: []string{"use --version", "--tlsverify"}},
		{name: "unknown short flag", args: []string{"-q"}, code: 2, stderr: []string{"flag provided but not defined: -q"}, notStderr: []string{"Watchtower"}},
		{name: "container without once", args: []string{"--config", cfg, "--container", "web"}, code: 2, stderr: []string{"require --once"}},
		{name: "json without version", args: []string{"--json", "--config", cfg}, code: 2, stderr: []string{"--json applies to --version"}},
		{name: "version takes no names", args: []string{"version", "web"}, code: 2, stderr: []string{`unexpected argument "web"`}},
		{name: "history takes no names", args: []string{"history", "web"}, code: 2, stderr: []string{`unexpected argument "web"`}},
		{name: "help", args: []string{"help"}, code: 0, stdout: []string{"Usage:", "Commands:", "Exit codes:"}},
		{name: "help flag", args: []string{"--help"}, code: 0, stdout: []string{"Usage:"}},
		{name: "short help flag", args: []string{"-h"}, code: 0, stdout: []string{"Usage:"}},
		{name: "help topic", args: []string{"help", "history"}, code: 0, stdout: []string{"dockpaum history", "--since"}},
		{name: "command help flag", args: []string{"history", "--help"}, code: 0, stdout: []string{"dockpaum history", "--limit"}},
		{name: "help unknown topic", args: []string{"help", "nope"}, code: 2, stderr: []string{`no help for "nope"`}},
		{name: "help too many", args: []string{"help", "run", "validate"}, code: 2},
		{name: "negative history limit", args: []string{"history", "--limit", "-1"}, code: 2, stderr: []string{"--limit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := execute(t, c.args...)
			if code != c.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, c.code, out, errOut)
			}
			for _, want := range c.stdout {
				if !strings.Contains(out, want) {
					t.Errorf("stdout lacks %q:\n%s", want, out)
				}
			}
			for _, bad := range c.notStdout {
				if strings.Contains(out, bad) {
					t.Errorf("stdout contains %q:\n%s", bad, out)
				}
			}
			for _, want := range c.stderr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr lacks %q:\n%s", want, errOut)
				}
			}
			for _, bad := range c.notStderr {
				if strings.Contains(errOut, bad) {
					t.Errorf("stderr contains %q:\n%s", bad, errOut)
				}
			}
		})
	}
}

func TestVersionJSONDocument(t *testing.T) {
	code, out, _ := execute(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var v server.Version
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, out)
	}
	if v.Version == "" || v.Commit == "" || v.Date == "" || !strings.HasPrefix(v.Go, "go") || !strings.Contains(v.Platform, "/") {
		t.Errorf("version document incomplete: %+v", v)
	}
}

func TestConfigPathFromEnvironment(t *testing.T) {
	clearConfigEnv(t)
	cfg := writeConfig(t, "hosts: [{name: local}]\n")
	t.Setenv(config.EnvConfigPath, cfg)
	code, out, errOut := execute(t, "validate")
	if code != 0 || !strings.Contains(out, "config OK: "+cfg) {
		t.Fatalf("validate with %s set: exit %d\n%s%s", config.EnvConfigPath, code, out, errOut)
	}
	other := writeConfig(t, "hosts: [{name: other}]\n")
	if code, out, _ := execute(t, "validate", "--config", other); code != 0 || !strings.Contains(out, other) {
		t.Fatalf("--config did not win over %s: %s", config.EnvConfigPath, out)
	}
}

func TestDaemonDockerUnreachable(t *testing.T) {
	clearConfigEnv(t)
	sock := filepath.Join(t.TempDir(), "missing.sock")
	cfg := writeConfig(t, "log: {format: json}\nhosts: [{name: local, endpoint: \"unix://"+sock+"\"}]\naudit: {enabled: false}\nhttp: {enabled: false}\n")
	code, out, errOut := execute(t, "--once", "--config", cfg, "web")
	if code != exitFailure {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, exitFailure, out, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Errorf("stdout line is not JSON: %q", line)
		}
		if msg, _ := doc["msg"].(string); strings.Contains(msg, "Watchtower compatibility form") && doc["level"] != "WARN" {
			t.Errorf("container names logged at %v, want WARN", doc["level"])
		}
	}
	for _, want := range []string{"docker connect failed", "Watchtower compatibility form"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(errOut, "█") {
		t.Errorf("banner printed to a non-terminal with JSON logs:\n%s", errOut)
	}
}

func TestDaemonTextLogsShowPlainBanner(t *testing.T) {
	clearConfigEnv(t)
	sock := filepath.Join(t.TempDir(), "missing.sock")
	cfg := writeConfig(t, "log: {format: text}\nhosts: [{name: local, endpoint: \"unix://"+sock+"\"}]\naudit: {enabled: false}\nhttp: {enabled: false}\n")
	code, out, errOut := execute(t, "run", "--once", "--config", cfg)
	if code != exitFailure {
		t.Fatalf("exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errOut, "█") || !strings.Contains(errOut, "1 host") {
		t.Errorf("text logs should come with the banner on stderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "\x1b[") {
		t.Error("banner on a non-terminal must not contain ANSI escapes")
	}
	if strings.Contains(out, "█") {
		t.Error("banner leaked into stdout")
	}
}

func TestReadOnlyCommandsWriteNoConfig(t *testing.T) {
	clearConfigEnv(t)
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal-api/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer health.Close()
	_, port, err := net.SplitHostPort(health.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKPAUM_HTTP_PORT", port)

	absent := filepath.Join(t.TempDir(), "conf", "dockpaum.yml")
	for _, args := range [][]string{
		{"validate", "--config", absent},
		{"print-config", "--config", absent},
		{"--healthcheck", "--config", absent},
	} {
		code, out, errOut := execute(t, args...)
		if code != 0 {
			t.Errorf("%q: exit %d\n%s%s", args, code, out, errOut)
		}
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("%q created the config file (stat: %v)", args, err)
		}
		if _, err := os.Stat(filepath.Dir(absent)); !os.IsNotExist(err) {
			t.Fatalf("%q created the config directory (stat: %v)", args, err)
		}
	}
}

func TestEnvironmentOverridesAreListed(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DOCKPAUM_DRY_RUN", "true")
	cfg := writeConfig(t, "dry_run: false\n")

	code, out, errOut := execute(t, "validate", "--config", cfg)
	if code != 0 {
		t.Fatalf("validate: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "set from the environment") || !strings.Contains(out, "  dry_run <- DOCKPAUM_DRY_RUN") {
		t.Errorf("validate does not list the environment override:\n%s", out)
	}

	code, out, errOut = execute(t, "print-config", "--config", cfg)
	if code != 0 {
		t.Fatalf("print-config: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "dry_run: true") || !strings.Contains(out, "#   dry_run <- DOCKPAUM_DRY_RUN") {
		t.Errorf("print-config does not mark the environment override:\n%s", out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Errorf("print-config output is not YAML: %v", err)
	}
}

func TestValidateRemoteConfigHidesTheURL(t *testing.T) {
	clearConfigEnv(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dry_run: true\n"))
	}))
	defer remote.Close()
	t.Setenv(config.EnvConfigURL, remote.URL+"/config.yml?token=s3cret")
	code, out, errOut := execute(t, "validate")
	if code != 0 {
		t.Fatalf("validate: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "config OK: $"+config.EnvConfigURL) || strings.Contains(out+errOut, "s3cret") {
		t.Errorf("remote config reported as %q (stderr %q)", out, errOut)
	}
}

func TestConfigErrorsHideTheRemoteURL(t *testing.T) {
	clearConfigEnv(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer broken.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("schedule: {cron: \"nonsense\"}\n"))
	}))
	defer invalid.Close()

	for _, srv := range []string{broken.URL, gone.URL, invalid.URL} {
		t.Setenv(config.EnvConfigURL, srv+"/config.yml?token=s3cret")
		for _, args := range [][]string{{"validate"}, {"print-config"}, {"notify", "test"}, {"plan"}, {"--healthcheck"}} {
			code, out, errOut := execute(t, args...)
			if code == 0 || strings.Contains(out+errOut, "s3cret") || !strings.Contains(errOut, "$"+config.EnvConfigURL) {
				t.Errorf("%s with %s: exit %d, stderr %q", args, srv, code, errOut)
			}
		}
	}
}

func TestConfigErrRedactsEverySpelling(t *testing.T) {
	clearConfigEnv(t)
	u := &url.URL{Scheme: "https", User: url.UserPassword("ops", "pa55"), Host: "config.example", Path: "/dockpaum.yml", RawQuery: "token=s3cret"}
	raw := u.String()
	t.Setenv(config.EnvConfigURL, " "+raw+" ")
	for _, msg := range []string{
		"fetch " + raw + ": HTTP 500",
		`Get "` + strings.Replace(raw, "pa55@", "***@", 1) + `": dial tcp: lookup config.example: no such host`,
		"source " + u.Redacted() + ": yaml: line 1",
	} {
		got := configErr(errors.New(msg))
		if strings.Contains(got, "s3cret") || strings.Contains(got, "pa55") || !strings.Contains(got, "$"+config.EnvConfigURL) {
			t.Errorf("configErr(%q) = %q", msg, got)
		}
	}
	t.Setenv(config.EnvConfigURL, "")
	if got := configErr(errors.New("read /etc/dockpaum/dockpaum.yml: permission denied")); got != "read /etc/dockpaum/dockpaum.yml: permission denied" {
		t.Errorf("a local config's error changed: %q", got)
	}
}

func TestSchemaCommandPrintsTheConfigSchema(t *testing.T) {
	code, out, _ := execute(t, "schema")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want, err := config.JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Error("schema output differs from config.JSONSchema")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
}

func TestNotifyTest(t *testing.T) {
	clearConfigEnv(t)
	var mu sync.Mutex
	var events []string
	hook := func(code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type     string `json:"type"`
				Instance string `json:"instance"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			events = append(events, body.Type+"@"+body.Instance)
			mu.Unlock()
			w.WriteHeader(code)
		}))
	}
	good, broken := hook(http.StatusNoContent), hook(http.StatusInternalServerError)
	defer good.Close()
	defer broken.Close()
	cfg := writeConfig(t, "instance: edge-1\nnotify:\n"+
		"  - {type: webhook, name: ops, url: \""+good.URL+"/hook\", events: [update.failure]}\n"+
		"  - {type: webhook, name: broken, url: \""+broken.URL+"/hook\"}\n")

	code, out, errOut := execute(t, "notify", "test", "--config", cfg, "--name", "ops")
	if code != 0 || !strings.Contains(out, "ok    ops (webhook)") || strings.Contains(out, "broken") {
		t.Fatalf("notify test --name ops: exit %d\n%s%s", code, out, errOut)
	}
	mu.Lock()
	got := strings.Join(events, ",")
	mu.Unlock()
	if got != "notify.test@edge-1" {
		t.Errorf("delivered events = %q, want one notify.test from instance edge-1", got)
	}

	code, out, errOut = execute(t, "--notify-test", "--config", cfg)
	if code != exitFailure || !strings.Contains(out, "ok    ops") || !strings.Contains(out, "FAIL  broken (webhook): HTTP 500") {
		t.Errorf("notify-test with a failing notifier: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "1 notifier failed") {
		t.Errorf("stderr lacks the failure count: %q", errOut)
	}

	code, out, _ = execute(t, "--notify-test", "--notify-name", "nope", "--config", cfg)
	if code != exitFailure || !strings.Contains(out, "FAIL  nope: no notifier with this name is configured") {
		t.Errorf("unknown notifier name: exit %d\n%s", code, out)
	}
}

func TestDaemonWatchtowerRunOnce(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DOCKPAUM_WATCHTOWER_ENV", "true")
	t.Setenv("WATCHTOWER_RUN_ONCE", "true")
	sock := filepath.Join(t.TempDir(), "missing.sock")
	cfg := writeConfig(t, "log: {format: json}\nhosts: [{name: local, endpoint: \"unix://"+sock+"\"}]\naudit: {enabled: false}\nhttp: {enabled: false}\n")
	code, out, errOut := execute(t, "--config", cfg)
	if code != exitFailure {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, exitFailure, out, errOut)
	}
	var sawOnce, sawOverrides bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		switch doc["msg"] {
		case "WATCHTOWER_RUN_ONCE is set: running a single check and exiting":
			sawOnce = true
		case "dockpaum starting":
			n, _ := doc["env_overrides"].(float64)
			sawOverrides = n >= 1
		}
	}
	if !sawOnce || !sawOverrides {
		t.Errorf("run-once log %v, env_overrides counted %v\n%s", sawOnce, sawOverrides, out)
	}
}
