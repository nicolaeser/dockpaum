package cli

import (
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func Run(args []string, stdout, stderr io.Writer) int {
	inv, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		if err := printHelp(stdout, inv.command); err != nil {
			printf(stderr, "dockpaum: %v\n", err)
			return exitUsage
		}
		return exitOK
	}
	if err != nil {
		printf(stderr, "dockpaum: %v\nRun 'dockpaum help' for usage.\n", err)
		return exitUsage
	}

	switch inv.command {
	case cmdHelp:
		if err := printHelp(stdout, inv.topic); err != nil {
			printf(stderr, "dockpaum: %v\n", err)
			return exitUsage
		}
		return exitOK
	case cmdVersion:
		return runVersion(inv, stdout)
	case cmdHelper:
		h := inv.helper
		if err := runHelper(inv.configPath, h.oldID, h.newImage, h.expectID, time.Duration(h.delay)*time.Second); err != nil {
			printf(stderr, "self-update helper: %v\n", err)
			return exitFailure
		}
		return exitOK
	case cmdValidate:
		return runValidate(inv, stdout, stderr)
	case cmdHealthcheck:
		return runHealthProbe(inv.configPath, stderr)
	case cmdPrintConfig:
		return runPrintConfig(inv, stdout, stderr)
	case cmdHistory:
		return runHistory(inv, stdout, stderr, time.Now())
	case cmdNotifyTest:
		return runNotifyTest(inv, stdout, stderr)
	case cmdPlan:
		return runPlan(inv, stdout, stderr, time.Now())
	case cmdExplain:
		return runExplain(inv, stdout, stderr, time.Now())
	case cmdInit:
		return runInit(inv, stdout, stderr)
	case cmdSchema:
		return runSchema(stdout, stderr)
	}
	return runDaemon(inv, stdout, stderr)
}

const embeddedSource = "embedded default"

func configSource(cfg *config.Config) string {
	if remoteConfig() {
		return "$" + config.EnvConfigURL
	}
	return cfg.Source()
}

func remoteConfig() bool {
	return strings.TrimSpace(os.Getenv(config.EnvConfigURL)) != ""
}

func configErr(err error) string {
	msg := err.Error()
	if !remoteConfig() {
		return msg
	}
	raw := strings.TrimSpace(os.Getenv(config.EnvConfigURL))
	forms := []string{raw}
	if u, perr := url.Parse(raw); perr == nil {
		forms = append(forms, u.String(), u.Redacted())
		if _, ok := u.User.Password(); ok {
			forms = append(forms, strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1))
		}
	}
	slices.SortFunc(forms, func(a, b string) int { return len(b) - len(a) })
	for _, f := range forms {
		if f != "" {
			msg = strings.ReplaceAll(msg, f, "$"+config.EnvConfigURL)
		}
	}
	return msg
}
