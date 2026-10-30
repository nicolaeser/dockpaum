package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestParseArgsFlagFormPrecedence(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, cmdRun},
		{[]string{"--config", "x.yml"}, cmdRun},
		{[]string{"--once", "--dry-run"}, cmdRun},
		{[]string{"--version", "--validate"}, cmdVersion},
		{[]string{"--self-update-helper", "abc", "--validate"}, cmdHelper},
		{[]string{"--validate", "--healthcheck"}, cmdValidate},
		{[]string{"--healthcheck", "--print-config"}, cmdHealthcheck},
		{[]string{"--print-config"}, cmdPrintConfig},
		{[]string{"--print-config", "--notify-test"}, cmdPrintConfig},
		{[]string{"--notify-test"}, cmdNotifyTest},
		{[]string{"--notify-test", "--once"}, cmdNotifyTest},
		{[]string{"run"}, cmdRun},
		{[]string{"validate"}, cmdValidate},
		{[]string{"print-config"}, cmdPrintConfig},
		{[]string{"history"}, cmdHistory},
		{[]string{"notify-test"}, cmdNotifyTest},
		{[]string{"notify", "test"}, cmdNotifyTest},
		{[]string{"plan"}, cmdPlan},
		{[]string{"explain", "web"}, cmdExplain},
		{[]string{"init"}, cmdInit},
		{[]string{"schema"}, cmdSchema},
		{[]string{"version"}, cmdVersion},
		{[]string{"help"}, cmdHelp},
	}
	for _, c := range cases {
		inv, err := parseArgs(c.args)
		if err != nil {
			t.Errorf("parseArgs(%q): %v", c.args, err)
			continue
		}
		if inv.command != c.want {
			t.Errorf("parseArgs(%q).command = %q, want %q", c.args, inv.command, c.want)
		}
	}
}

func TestParseArgsSelfUpdateHelperArgv(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "")
	inv, err := parseArgs([]string{"--self-update-helper", "0123abcd", "--new-image", "ghcr.io/x/dockpaum:1", "--delay", "7", "--expect-id", "sha256:feed"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.command != cmdHelper || inv.helper.oldID != "0123abcd" || inv.helper.newImage != "ghcr.io/x/dockpaum:1" ||
		inv.helper.delay != 7 || inv.helper.expectID != "sha256:feed" || inv.configPath != config.DefaultConfigPath {
		t.Errorf("helper invocation = %+v", inv)
	}
}

func TestParseArgsRunFlags(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "")
	inv, err := parseArgs([]string{"--config", "c.yml", "--once", "--report-only", "--container", "web", "--container", " api ", "--policy", "db", "nginx", "/redis", "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.configPath != "c.yml" || !inv.once || !inv.dryRun || inv.policy != "db" {
		t.Errorf("run flags = %+v", inv)
	}
	if !slices.Equal(inv.containers, []string{"web", "api"}) {
		t.Errorf("containers = %q", inv.containers)
	}
	if !slices.Equal(inv.names, []string{"nginx", "redis"}) {
		t.Errorf("names = %q, want nginx, redis (leading slash dropped, deduplicated)", inv.names)
	}
}

func TestParseArgsHistoryFlags(t *testing.T) {
	inv, err := parseArgs([]string{"history", "--dir", "/a", "--container", "web", "--container", "db", "--since", "7d", "--limit", "0", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	h := inv.history
	if h.dir != "/a" || !slices.Equal(h.containers, []string{"web", "db"}) || h.since != "7d" || h.limit != 0 || !inv.json {
		t.Errorf("history options = %+v json=%v", h, inv.json)
	}
	if inv, _ := parseArgs([]string{"history"}); inv.history.limit != 50 {
		t.Errorf("default limit = %d, want 50", inv.history.limit)
	}
}

func TestParseArgsRunCommandAcceptsCommandNamesAsContainers(t *testing.T) {
	inv, err := parseArgs([]string{"run", "history"})
	if err != nil || !slices.Equal(inv.names, []string{"history"}) {
		t.Errorf("run history: %+v %v", inv, err)
	}
}

func TestParseArgsExistingFileArgument(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("web", []byte("hosts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("db", 0o750); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"validate", "web"}, {"--validate", "web"}, {"print-config", "web"}} {
		_, err := parseArgs(args)
		if err == nil || !strings.Contains(err.Error(), "--config web") {
			t.Errorf("parseArgs(%q) = %v, want a --config hint", args, err)
		}
	}
	for _, args := range [][]string{{"run", "web"}, {"web"}, {"validate", "db"}} {
		if _, err := parseArgs(args); err != nil {
			t.Errorf("parseArgs(%q) = %v, want the word kept as a container name", args, err)
		}
	}
}

func TestParseArgsErrors(t *testing.T) {
	cases := [][]string{
		{"--config", "x", "web", "--once"},
		{"web", "-v"},
		{"--config", "x", "help"},
		{"a"},
		{"--json"},
		{"--healthcheck", "web"},
		{"--version", "web"},
		{"validate", "--once"},
		{"run", "--validate"},
		{"--policy", "db"},
		{"history", "--limit", "-5"},
		{"--strict"},
		{"--print-config", "--strict"},
		{"print-config", "--strict"},
		{"--notify-name", "ops"},
		{"--validate", "--notify-name", "ops"},
		{"notify-test", "ops"},
		{"notify-test", "--notify-name", "ops"},
		{"schema", "--config", "x.yml"},
		{"notify"},
		{"notify", "send"},
		{"notify", "test", "ops"},
		{"plan", "web"},
		{"plan", "--container", "bad name"},
		{"plan", "--once"},
		{"explain"},
		{"explain", "web", "db"},
		{"explain", "bad name!"},
		{"explain", "web", "--policy", "x"},
		{"init", "--config", "x.yml"},
		{"init", "observe"},
		{"init", "--force"},
		{"init", "--force", "--output", "-"},
		{"help", "notify", "send"},
		{"--config", "x", "plan"},
	}
	for _, args := range cases {
		_, err := parseArgs(args)
		var ue errUsage
		if !errors.As(err, &ue) {
			t.Errorf("parseArgs(%q) = %v, want a usage error", args, err)
		}
	}
	if _, err := parseArgs([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help = %v, want flag.ErrHelp", err)
	}
}

func TestApplyContainerNames(t *testing.T) {
	cases := []struct {
		include, names, want []string
		err                  bool
	}{
		{nil, nil, nil, false},
		{nil, []string{"nginx", "redis"}, []string{"nginx", "redis"}, false},
		{[]string{"web", "db"}, nil, []string{"web", "db"}, false},
		{[]string{"web", "db"}, []string{"db", "cache"}, []string{"db"}, false},
		{[]string{"web", "db"}, []string{"cache"}, nil, true},
	}
	for _, c := range cases {
		cfg := &config.Config{Filters: config.FilterConfig{IncludeNames: slices.Clone(c.include)}}
		err := applyContainerNames(cfg, c.names)
		if (err != nil) != c.err {
			t.Errorf("include %q names %q: err %v, want error %v", c.include, c.names, err, c.err)
			continue
		}
		if err == nil && !slices.Equal(cfg.Filters.IncludeNames, c.want) {
			t.Errorf("include %q names %q: include_names = %q, want %q", c.include, c.names, cfg.Filters.IncludeNames, c.want)
		}
	}
}

func TestHelpListsUserFlagsAndHidesInternalOnes(t *testing.T) {
	var buf bytes.Buffer
	if err := printHelp(&buf, ""); err != nil {
		t.Fatal(err)
	}
	overview := buf.String()
	for _, name := range []string{"config", "once", "dry-run", "report-only", "container", "policy", "validate", "strict", "print-config", "notify-test", "notify-name", "version", "json"} {
		if !strings.Contains(overview, "--"+name) {
			t.Errorf("help overview does not mention --%s", name)
		}
	}
	for _, hidden := range []string{"healthcheck", "self-update-helper", "new-image", "expect-id", "delay", "internal:"} {
		if strings.Contains(overview, hidden) {
			t.Errorf("help overview mentions internal %q", hidden)
		}
	}
	for _, want := range []string{"Exit codes:", docsURL, "dockpaum nginx redis", "Examples:", "notify-test", "schema", "3  the config has warnings"} {
		if !strings.Contains(overview, want) {
			t.Errorf("help overview lacks %q", want)
		}
	}

	buf.Reset()
	if err := printHelp(&buf, cmdHistory); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "dir", "container", "since", "limit", "json"} {
		if !strings.Contains(buf.String(), "--"+name) {
			t.Errorf("history help does not mention --%s", name)
		}
	}
	for _, topic := range []string{cmdRun, cmdValidate, cmdPrintConfig, cmdNotifyTest, cmdSchema, cmdVersion, cmdHelp} {
		buf.Reset()
		if err := printHelp(&buf, topic); err != nil || !strings.Contains(buf.String(), "Usage: dockpaum") {
			t.Errorf("help %s: %v\n%s", topic, err, buf.String())
		}
	}
}

func TestParseArgsDryRunAliases(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--dry-run"}, true},
		{[]string{"--report-only"}, true},
		{[]string{"--dry-run", "--report-only=false"}, true},
		{[]string{"--dry-run=false", "--report-only"}, true},
		{[]string{"run", "--report-only"}, true},
	}
	for _, c := range cases {
		inv, err := parseArgs(c.args)
		if err != nil || inv.dryRun != c.want {
			t.Errorf("parseArgs(%q): dryRun=%v err=%v, want %v", c.args, inv.dryRun, err, c.want)
		}
	}
}

func TestParseArgsConfigPath(t *testing.T) {
	cases := []struct {
		env  string
		args []string
		want string
	}{
		{"", nil, config.DefaultConfigPath},
		{"/config/env.yml", nil, "/config/env.yml"},
		{"/config/env.yml", []string{"--config", "/x/flag.yml"}, "/x/flag.yml"},
		{"/config/env.yml", []string{"--config", config.DefaultConfigPath}, config.DefaultConfigPath},
		{"/config/env.yml", []string{"validate"}, "/config/env.yml"},
		{"/config/env.yml", []string{"history"}, "/config/env.yml"},
		{"/config/env.yml", []string{"notify-test", "--config", "n.yml"}, "n.yml"},
		{"/config/env.yml", []string{"--healthcheck"}, "/config/env.yml"},
		{"  ", nil, config.DefaultConfigPath},
	}
	for _, c := range cases {
		t.Setenv(config.EnvConfigPath, c.env)
		inv, err := parseArgs(c.args)
		if err != nil {
			t.Errorf("parseArgs(%q) with %s=%q: %v", c.args, config.EnvConfigPath, c.env, err)
			continue
		}
		if inv.configPath != c.want {
			t.Errorf("parseArgs(%q) with %s=%q: config %q, want %q", c.args, config.EnvConfigPath, c.env, inv.configPath, c.want)
		}
	}
}

func TestParseArgsValidateAndNotifyFlags(t *testing.T) {
	for _, args := range [][]string{{"--validate", "--strict"}, {"validate", "--strict"}} {
		inv, err := parseArgs(args)
		if err != nil || inv.command != cmdValidate || !inv.strict {
			t.Errorf("parseArgs(%q) = %+v, %v; want validate with strict", args, inv, err)
		}
	}
	inv, err := parseArgs([]string{"--notify-test", "--notify-name", "ops", "--notify-name", " mail "})
	if err != nil || inv.command != cmdNotifyTest || !slices.Equal(inv.notifyNames, []string{"ops", "mail"}) {
		t.Errorf("--notify-test --notify-name: %+v, %v", inv, err)
	}
	inv, err = parseArgs([]string{"notify-test", "--name", "ops"})
	if err != nil || inv.command != cmdNotifyTest || !slices.Equal(inv.notifyNames, []string{"ops"}) {
		t.Errorf("notify-test --name: %+v, %v", inv, err)
	}
}

func TestParseArgsPlanExplainInit(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "")
	inv, err := parseArgs([]string{"plan", "--container", "/web", "--container", "db", "--policy", "Database", "--json", "--config", "c.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inv.plan.containers, []string{"web", "db"}) || inv.plan.policy != "Database" || !inv.json || inv.configPath != "c.yml" {
		t.Errorf("plan options = %+v json=%v config=%q", inv.plan, inv.json, inv.configPath)
	}
	for _, args := range [][]string{{"explain", "/web", "--json"}, {"explain", "--json", "web"}, {"explain", "web", "--json", "--config", "c.yml"}} {
		inv, err := parseArgs(args)
		if err != nil || inv.command != cmdExplain || inv.plan.target != "web" || !inv.json {
			t.Errorf("parseArgs(%q) = %+v, %v", args, inv, err)
		}
	}
	inv, err = parseArgs([]string{"init", "--output", "x.yml", "--force"})
	if err != nil || inv.initCfg != (initOptions{output: "x.yml", force: true}) {
		t.Errorf("init options = %+v, %v", inv.initCfg, err)
	}
	if _, err := parseArgs([]string{"init", "--preset", "homelab"}); err == nil {
		t.Error("init --preset is accepted, want a usage error")
	}
	if inv, err := parseArgs([]string{"run", "plan", "init"}); err != nil || !slices.Equal(inv.names, []string{"plan", "init"}) {
		t.Errorf("run plan init = %+v, %v", inv, err)
	}
}

func TestNotifyTestSubcommand(t *testing.T) {
	inv, err := parseArgs([]string{"notify", "test", "--name", "ops", "--name", "mail"})
	if err != nil || inv.command != cmdNotifyTest || !slices.Equal(inv.notifyNames, []string{"ops", "mail"}) {
		t.Errorf("notify test --name: %+v, %v", inv, err)
	}
	if _, err := parseArgs([]string{"notify", "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("notify --help = %v, want flag.ErrHelp", err)
	}
	code, out, _ := execute(t, "notify", "--help")
	if code != exitOK || !strings.Contains(out, "Usage: dockpaum notify test") {
		t.Errorf("notify --help: exit %d\n%s", code, out)
	}
	code, _, errOut := execute(t, "notify")
	if code != exitUsage || !strings.Contains(errOut, "dockpaum notify test") {
		t.Errorf("notify alone: exit %d %q", code, errOut)
	}
	code, out, _ = execute(t, "help", "notify", "test")
	if code != exitOK || !strings.Contains(out, "Usage: dockpaum notify test") {
		t.Errorf("help notify test: exit %d\n%s", code, out)
	}
}

func TestHelpForNewCommands(t *testing.T) {
	cases := map[string][]string{
		cmdPlan:    {"--container", "--policy", "--json", "--config", "Nothing is changed", "pulls no image", "Exit codes:"},
		cmdExplain: {"--json", "--config", "digest pin, registry check, maintenance\nwindow", "approval", "at update", "Exit codes:"},
		cmdInit:    {"--output", "--force", "docs/examples", "yaml-language-server"},
		cmdNotify:  {"notify test", "--name", "notify-test"},
		cmdHelp:    {"plan, explain, init"},
	}
	for topic, wants := range cases {
		var buf bytes.Buffer
		if err := printHelp(&buf, topic); err != nil {
			t.Errorf("help %s: %v", topic, err)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(buf.String(), w) {
				t.Errorf("help %s lacks %q:\n%s", topic, w, buf.String())
			}
		}
	}
	var buf bytes.Buffer
	if err := printHelp(&buf, ""); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"  plan ", "  explain ", "  init ", "  notify test ", "dockpaum run plan"} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("help overview lacks %q", w)
		}
	}
	if err := printHelp(&buf, "nope"); err == nil || !strings.Contains(err.Error(), "notify test") {
		t.Errorf("help nope = %v", err)
	}
}
