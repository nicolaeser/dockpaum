package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const (
	exitOK       = 0
	exitFailure  = 1
	exitUsage    = 2
	exitWarnings = 3
)

const (
	docsURL            = "https://github.com/nicolaeser/dockpaum/tree/main/docs"
	troubleshootingURL = "https://github.com/nicolaeser/dockpaum/blob/main/docs/troubleshooting.md"
	migrationURL       = "https://github.com/nicolaeser/dockpaum/blob/main/docs/migrating-from-watchtower.md"
)

const (
	cmdRun         = "run"
	cmdPlan        = "plan"
	cmdExplain     = "explain"
	cmdInit        = "init"
	cmdValidate    = "validate"
	cmdPrintConfig = "print-config"
	cmdHistory     = "history"
	cmdNotifyTest  = "notify-test"
	cmdSchema      = "schema"
	cmdVersion     = "version"
	cmdHelp        = "help"

	cmdNotify = "notify"

	cmdHealthcheck = "healthcheck"
	cmdHelper      = "self-update-helper"
)

const commandList = "run, plan, explain, init, validate, print-config, history, notify test, schema, version"

func isCommand(s string) bool {
	switch s {
	case cmdRun, cmdPlan, cmdExplain, cmdInit, cmdValidate, cmdPrintConfig, cmdHistory, cmdNotify, cmdNotifyTest, cmdSchema, cmdVersion, cmdHelp:
		return true
	}
	return false
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		*l = append(*l, v)
	}
	return nil
}

type invocation struct {
	command    string
	configPath string

	once       bool
	dryRun     bool
	containers stringList
	policy     string
	names      []string

	json        bool
	strict      bool
	topic       string
	history     historyOptions
	helper      helperOptions
	plan        planOptions
	initCfg     initOptions
	notifyNames stringList
}

type planOptions struct {
	containers stringList
	policy     string
	target     string
}

type historyOptions struct {
	dir        string
	containers stringList
	since      string
	limit      int
}

type helperOptions struct {
	oldID, newImage, expectID string
	delay                     int
}

type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

func usagef(format string, args ...any) error { return errUsage{fmt.Sprintf(format, args...)} }

func printf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

func parseArgs(args []string) (*invocation, error) {
	inv := &invocation{}
	form := ""
	if len(args) > 0 && isCommand(args[0]) {
		form, args = args[0], args[1:]
	}
	if form == cmdNotify {
		inv.command = cmdNotifyTest
		switch {
		case len(args) == 0:
			return inv, usagef("notify needs a subcommand: dockpaum notify test")
		case args[0] == "test":
			form, args = cmdNotifyTest, args[1:]
		case args[0] == "-h" || args[0] == "-help" || args[0] == "--help":
			return inv, flag.ErrHelp
		default:
			return inv, usagef("unknown notify subcommand %q: use dockpaum notify test", args[0])
		}
	}
	inv.command = form

	fs := flag.NewFlagSet("dockpaum", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	configFlag := func() {
		fs.StringVar(&inv.configPath, "config", config.DefaultConfigPath, "path to the YAML config (default: $DOCKPAUM_CONFIG, else "+config.DefaultConfigPath+")")
	}
	var reportOnly bool
	runFlags := func() {
		fs.BoolVar(&inv.once, "once", false, "run one update pass across all hosts, then exit")
		fs.BoolVar(&inv.dryRun, "dry-run", false, "decide and report, but change nothing")
		fs.BoolVar(&reportOnly, "report-only", false, "alias for --dry-run")
		fs.Var(&inv.containers, "container", "with --once: only this container (repeatable)")
		fs.StringVar(&inv.policy, "policy", "", "with --once: only containers of this policy")
	}
	var validate, printConfig, notifyTest, showVersion, healthcheck bool
	switch form {
	case "":
		configFlag()
		runFlags()
		fs.BoolVar(&validate, "validate", false, "validate the config file and exit")
		fs.BoolVar(&inv.strict, "strict", false, "with --validate: exit 3 when the config has warnings")
		fs.BoolVar(&printConfig, "print-config", false, "print the effective config and exit")
		fs.BoolVar(&notifyTest, "notify-test", false, "send a test notification through every notifier and exit")
		fs.Var(&inv.notifyNames, "notify-name", "with --notify-test: only this notifier (repeatable)")
		fs.BoolVar(&showVersion, "version", false, "print version and exit")
		fs.BoolVar(&inv.json, "json", false, "with --version: print JSON")
		fs.BoolVar(&healthcheck, "healthcheck", false, "internal: probe the running daemon's health endpoint and exit")
		fs.StringVar(&inv.helper.oldID, "self-update-helper", "", "internal: run as self-update helper for this container id")
		fs.StringVar(&inv.helper.newImage, "new-image", "", "internal: new image reference for self-update helper")
		fs.StringVar(&inv.helper.expectID, "expect-id", "", "internal: image id the new image reference must resolve to")
		fs.IntVar(&inv.helper.delay, "delay", 5, "internal: seconds the helper waits before swap")
	case cmdRun:
		configFlag()
		runFlags()
	case cmdValidate:
		configFlag()
		fs.BoolVar(&inv.strict, "strict", false, "exit 3 when the config has warnings")
	case cmdPrintConfig:
		configFlag()
	case cmdNotifyTest:
		configFlag()
		fs.Var(&inv.notifyNames, "name", "only this notifier (repeatable)")
	case cmdPlan:
		configFlag()
		fs.Var(&inv.plan.containers, "container", "only this container (repeatable)")
		fs.StringVar(&inv.plan.policy, "policy", "", "only containers of this policy")
		fs.BoolVar(&inv.json, "json", false, "print JSON")
	case cmdExplain:
		configFlag()
		fs.BoolVar(&inv.json, "json", false, "print JSON")
	case cmdInit:
		fs.StringVar(&inv.initCfg.output, "output", "", "write to this file instead of stdout")
		fs.BoolVar(&inv.initCfg.force, "force", false, "overwrite an existing --output file")
	case cmdVersion:
		fs.BoolVar(&inv.json, "json", false, "print JSON")
	case cmdHistory:
		configFlag()
		fs.StringVar(&inv.history.dir, "dir", "", "read this audit directory instead of the config's audit.dir")
		fs.Var(&inv.history.containers, "container", "only this container (repeatable)")
		fs.StringVar(&inv.history.since, "since", "", "only records newer than a duration (24h, 7d) or after a time")
		fs.IntVar(&inv.history.limit, "limit", 50, "show at most the newest N records (0 = all)")
		fs.BoolVar(&inv.json, "json", false, "print a JSON array")
	}
	parse := func(args []string) error {
		err := fs.Parse(args)
		if err == nil || errors.Is(err, flag.ErrHelp) {
			return err
		}
		msg := err.Error()
		if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
			if hint := watchtowerFlagHint(strings.TrimPrefix(name, "-")); hint != "" {
				msg += "\n" + hint
			}
		}
		return errUsage{msg}
	}
	if err := parse(args); err != nil {
		return inv, err
	}
	positional := fs.Args()
	if form == cmdExplain {
		positional = nil
		for rest := fs.Args(); len(rest) > 0; rest = fs.Args() {
			positional = append(positional, rest[0])
			if err := parse(rest[1:]); err != nil {
				return inv, err
			}
		}
	}
	inv.dryRun = inv.dryRun || reportOnly
	if fs.Lookup("config") != nil {
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
		inv.configPath = config.ResolvePath(inv.configPath, explicit)
	}

	if form == "" {
		switch {
		case showVersion:
			inv.command = cmdVersion
		case inv.helper.oldID != "":
			inv.command = cmdHelper
		case validate:
			inv.command = cmdValidate
		case healthcheck:
			inv.command = cmdHealthcheck
		case printConfig:
			inv.command = cmdPrintConfig
		case notifyTest:
			inv.command = cmdNotifyTest
		default:
			inv.command = cmdRun
		}
		if inv.json && inv.command != cmdVersion {
			return inv, usagef("--json applies to --version (and the version, plan, explain and history commands)")
		}
		if inv.strict && inv.command != cmdValidate {
			return inv, usagef("--strict applies to --validate (and the validate command)")
		}
		if len(inv.notifyNames) > 0 && inv.command != cmdNotifyTest {
			return inv, usagef("--notify-name applies to --notify-test")
		}
	}

	rest := positional
	switch inv.command {
	case cmdHelp:
		if len(rest) == 2 && rest[0] == cmdNotify && rest[1] == "test" {
			rest = rest[:1]
		}
		if len(rest) > 1 {
			return inv, usagef("help takes one command name, got %q", rest)
		}
		if len(rest) == 1 {
			inv.topic = rest[0]
		}
		return inv, nil
	case cmdExplain:
		if len(rest) != 1 {
			return inv, usagef("explain takes one container name, as in \"dockpaum explain web\"")
		}
		name := strings.TrimPrefix(rest[0], "/")
		if !containerNameRE.MatchString(name) {
			return inv, usagef("%q is not a valid container name", rest[0])
		}
		inv.plan.target = name
	case cmdRun, cmdValidate, cmdPrintConfig:
		names, err := containerNames(rest, form == "", inv.command != cmdRun)
		if err != nil {
			return inv, err
		}
		inv.names = names
	default:
		if len(rest) > 0 {
			return inv, usagef("unexpected argument %q", rest[0])
		}
	}

	if inv.command == cmdRun && (len(inv.containers) > 0 || inv.policy != "") && !inv.once {
		return inv, usagef("--container and --policy require --once")
	}
	if inv.command == cmdHistory && inv.history.limit < 0 {
		return inv, usagef("--limit must not be negative")
	}
	if inv.command == cmdInit && inv.initCfg.force && (inv.initCfg.output == "" || inv.initCfg.output == "-") {
		return inv, usagef("--force applies to --output FILE: it overwrites an existing file")
	}
	if inv.command == cmdPlan {
		for i, c := range inv.plan.containers {
			name := strings.TrimPrefix(c, "/")
			if !containerNameRE.MatchString(name) {
				return inv, usagef("--container %q is not a valid container name", c)
			}
			inv.plan.containers[i] = name
		}
	}
	return inv, nil
}

var watchtowerShortFlags = map[string]string{
	"H": "host", "a": "api-version", "i": "interval", "s": "schedule", "c": "cleanup",
	"d": "debug", "m": "monitor-only", "R": "run-once", "S": "include-stopped",
	"x": "disable-containers", "n": "notifications", "P": "porcelain",
}

var watchtowerLongFlags = map[string]bool{
	"interval": true, "schedule": true, "cleanup": true, "label-enable": true, "disable-containers": true,
	"scope": true, "include-stopped": true, "include-restarting": true, "revive-stopped": true,
	"rolling-restart": true, "no-pull": true, "no-restart": true, "stop-timeout": true,
	"remove-volumes": true, "debug": true, "trace": true, "host": true, "tlsverify": true,
	"api-version": true, "no-startup-message": true, "porcelain": true, "warn-on-head-failure": true,
	"enable-lifecycle-hooks": true, "label-take-precedence": true,
}

func watchtowerFlagHint(name string) string {
	shown := "--" + name
	if long, ok := watchtowerShortFlags[name]; ok {
		shown = "-" + name + " (--" + long + ")"
		name = long
	}
	switch {
	case name == "v":
		return "-v is not a dockpaum flag: use --version for the version; Watchtower's -v (--tlsverify) is hosts[].tls in the config, see " + migrationURL
	case name == "run-once":
		return "Watchtower's " + shown + " is --once in dockpaum"
	case name == "monitor-only":
		return "Watchtower's " + shown + " is --dry-run in dockpaum (dry_run: true in the config)"
	case name == "health-check":
		return "Watchtower's --health-check is --healthcheck in dockpaum; the image's HEALTHCHECK already runs it"
	case watchtowerLongFlags[name], strings.HasPrefix(name, "notification"), strings.HasPrefix(name, "http-api-"):
		return shown + " is a Watchtower flag: dockpaum takes this setting from its config file, see " + migrationURL
	}
	return ""
}

func containerNames(rest []string, flagForm, existingFiles bool) ([]string, error) {
	var out []string
	for i, a := range rest {
		if i > 0 && strings.HasPrefix(a, "-") {
			return nil, usagef("flag %s must come before the container names (it follows %q)", a, rest[0])
		}
		if i == 0 && flagForm && isCommand(a) {
			return nil, usagef("%q is a command: put it first, as in \"dockpaum %s --config FILE\"", a, a)
		}
		if looksLikeConfigFile(a, existingFiles) {
			return nil, usagef("%q looks like a config file, not a container name: pass it with --config %s", a, a)
		}
		name := strings.TrimPrefix(a, "/")
		if !containerNameRE.MatchString(name) {
			return nil, usagef("%q is not a valid container name", a)
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

func looksLikeConfigFile(arg string, existing bool) bool {
	switch strings.ToLower(filepath.Ext(arg)) {
	case ".yml", ".yaml", ".json":
		return true
	}
	if !existing {
		return false
	}
	// #nosec G703 -- only stats the administrator's own command-line argument.
	fi, err := os.Stat(arg)
	return err == nil && fi.Mode().IsRegular()
}

func applyContainerNames(cfg *config.Config, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if len(cfg.Filters.IncludeNames) == 0 {
		cfg.Filters.IncludeNames = slices.Clone(names)
		return nil
	}
	var both []string
	for _, n := range names {
		if slices.Contains(cfg.Filters.IncludeNames, n) {
			both = append(both, n)
		}
	}
	if len(both) == 0 {
		return fmt.Errorf("container names %s given as arguments are not in filters.include_names %s: no container would be managed",
			strings.Join(names, ", "), strings.Join(cfg.Filters.IncludeNames, ", "))
	}
	cfg.Filters.IncludeNames = both
	return nil
}

func helpText(topic string) (string, bool) {
	switch topic {
	case "":
		return `dockpaum - policy-driven Docker container updates

Usage:
  dockpaum [run] [flags] [container ...]
  dockpaum <command> [flags]

Commands:
  run            Watch and update containers (the default command)
  plan           Show what the next run would do for each container, and why
  explain        Walk one container through every gate
  init           Print or write the default config
  validate       Check the config file, print warnings, and exit
  print-config   Print the effective config with secrets masked
  history        Show update decisions from the audit log
  notify test    Send a test message through every notifier
  schema         Print the JSON Schema of the config file
  version        Print version and build information
  help           Show help for a command

Run flags:
{{template "runflags"}}
Container names after the flags limit dockpaum to those containers, as in
Watchtower ("dockpaum nginx redis"); filters.include_names is the config form.
A container named like a command is given after "run": dockpaum run plan.
The flag forms --validate [--strict], --print-config, --notify-test
[--notify-name NAME] and --version [--json] work as well, and notify-test
is the same as notify test.

Examples:
  dockpaum init --output ./dockpaum.yml
  dockpaum validate --strict --config ./dockpaum.yml
  dockpaum plan --config ./dockpaum.yml
  dockpaum explain web
  dockpaum history --container web --since 7d
  dockpaum notify test --name ops

{{template "exit"}}
Documentation: {{.Docs}}
`, true
	case cmdRun:
		return `Usage: dockpaum [run] [flags] [container ...]

Watch the configured Docker hosts and update containers on schedule, or run
one pass with --once.

Flags:
{{template "runflags"}}
Container names limit dockpaum to those containers, like Watchtower's
arguments; they act as filters.include_names (with include_names set in the
config too, only names in both are managed). Flags go before the names.

{{template "exit"}}`, true
	case cmdValidate:
		return `Usage: dockpaum validate [--strict] [--config FILE]

Parse and validate the config, print warnings and notes to stderr, list the
settings taken from environment variables, and exit. When the file does not
exist or is empty, the built-in default it would be created from is
validated. Nothing is written. Also available as --validate.

Flags:
  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else {{.Config}})
  --strict            Exit 3 when the config has warnings (notes never fail)

{{template "exit"}}`, true
	case cmdPrintConfig:
		return `Usage: dockpaum print-config [--config FILE]

Print the effective config (after defaults, ${VAR} expansion and environment
variables) as YAML, with secrets masked. Settings taken from environment
variables are listed in a comment at the end. Nothing is written. Also
available as --print-config.

Flags:
  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else {{.Config}})

{{template "exit"}}`, true
	case cmdNotifyTest, cmdNotify:
		return `Usage: dockpaum notify test [--config FILE] [--name NAME ...]

Validate the config, send a test message through every configured notifier
(whatever events it subscribes to) and wait for each, then print ok or the
error per notifier. Also available as notify-test and as --notify-test
[--notify-name NAME].

Flags:
  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else {{.Config}})
  --name NAME         Only the notifier with this name (repeatable)

Exit codes:
  0  every notifier accepted the message
  1  a notifier failed, a name matched none, or none is configured
  2  invalid configuration or usage
`, true
	case cmdPlan:
		return `Usage: dockpaum plan [--container NAME ...] [--policy ID] [--json] [--config FILE]

Evaluate every managed container the way the next run would, and print per
container: its policy and why it applies, the running image and the newer
one (tag, version, short image ID), the decision with its reason, and when
the update can happen (the soak time ends, a window opens, the next run).

Nothing is changed: plan runs in dry-run whatever the config says. It asks
the registries directly and pulls no image, touches no container, sends no
notification, asks no approval endpoint, runs no hook, writes no audit
record, and works on a copy of the state. A container whose registry
cannot be asked directly is not checked and shows action error. plan stops
without evaluating when a run would first have to clean up after an
interrupted update or record a finished self-update; the daemon's next run
does that.

Flags:
  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else {{.Config}})
  --container NAME    Only this container (repeatable); its policy may have
                      its own schedule
  --policy ID         Only containers of this policy
  --json              Print a JSON document instead of the table

Without --container or --policy, plan covers the regular schedule and every
policy schedule. The table uses color on a terminal unless NO_COLOR is set.

Examples:
  dockpaum plan
  docker exec dockpaum /dockpaum plan --policy database
  dockpaum plan --json | jq '.containers[] | select(.action == "update")'

Exit codes:
  0  the plan was computed (pending updates do not change this)
  1  a host failed, a container could not be checked (action error), or
     plan stopped before evaluating
  2  invalid configuration or usage, or an unknown --container or --policy
`, true
	case cmdExplain:
		return `Usage: dockpaum explain [--json] [--config FILE] CONTAINER

Evaluate one container like plan and walk it through every gate in the
order the daemon applies them: filters, policy resolution, the container
itself (orchestrator, --rm), mode, digest pin, registry check, maintenance
window, soak time (min_image_age), monitor-only, dry-run, approval and the
update itself. Each gate shows pass, stop, not reached, or at update (decided
only when an update begins), and the gate that stops the update says what
would change it. With the same name on several hosts, each one is explained.
Nothing is changed, as with plan.

Flags:
  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else {{.Config}})
  --json              Print a JSON document

Exit codes:
  0  the container was explained
  1  a host failed, the container could not be checked (action error), or
     explain stopped before evaluating
  2  invalid configuration or usage, or no container with this name
`, true
	case cmdInit:
		return `Usage: dockpaum init [--output FILE [--force]]

Print the default config, with the schema line editors read
(# yaml-language-server: $schema=...), or write it to a file. More example
configs are in docs/examples.

Flags:
  --output FILE       Write the config to FILE (mode 0600) instead of stdout
  --force             Overwrite FILE when it exists

Examples:
  dockpaum init > dockpaum.yml
  dockpaum init --output /etc/dockpaum/dockpaum.yml

Exit codes:
  0  the config was written
  1  FILE exists (without --force) or cannot be written
  2  invalid usage
`, true
	case cmdSchema:
		return `Usage: dockpaum schema

Print the JSON Schema of the config file, matching this version. Editors
read it through a "# yaml-language-server: $schema=" first line.

{{template "exit"}}`, true
	case cmdHistory:
		return `Usage: dockpaum history [flags]

Show update decisions recorded in the audit log (audit.dir), newest last.

Flags:
  --config FILE       Config file naming audit.dir (default: $DOCKPAUM_CONFIG, else {{.Config}})
  --dir DIR           Read this audit directory instead (ignores --config)
  --container NAME    Only this container (repeatable)
  --since WHEN        Only newer records: a duration (24h, 7d, 2w) or a time
                      (2026-10-01, 2026-10-01T08:00:00Z)
  --limit N           At most the newest N records (default 50, 0 = all)
  --json              Print a JSON array of audit records

Examples:
  dockpaum history --since 24h
  docker exec dockpaum /dockpaum history --container web --json

{{template "exit"}}`, true
	case cmdVersion:
		return `Usage: dockpaum version [--json]

Print the version, commit, build date, Go version and platform. Also
available as --version.

Flags:
  --json              Print a JSON object
`, true
	case cmdHelp:
		return `Usage: dockpaum help [command]

Show help for dockpaum or one of its commands: {{.Commands}}.
`, true
	}
	return "", false
}

const helpPartials = `{{define "runflags"}}  --config FILE       Config file (default: $DOCKPAUM_CONFIG, else /etc/dockpaum/dockpaum.yml)
  --once              Run one update pass across all hosts, then exit
  --dry-run           Decide and report, but change nothing (alias: --report-only)
  --container NAME    With --once: only this container (repeatable)
  --policy ID         With --once: only containers of this policy
{{end}}{{define "exit"}}Exit codes:
  0  success
  1  a run or command failed
  2  invalid configuration or usage
  3  the config has warnings (validate --strict)
{{end}}`

func printHelp(w io.Writer, topic string) error {
	text, ok := helpText(topic)
	if !ok {
		return usagef("no help for %q: commands are %s", topic, commandList)
	}
	t := template.Must(template.Must(template.New("help").Parse(helpPartials)).Parse(text))
	return t.Execute(w, struct {
		Docs, Config, Commands string
	}{docsURL, config.DefaultConfigPath, commandList})
}
