package cli

import (
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func runValidate(inv *invocation, stdout, stderr io.Writer) int {
	vcfg, err := config.Validate(inv.configPath)
	if err == nil {
		err = applyContainerNames(vcfg, inv.names)
	}
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return exitUsage
	}
	warnings := vcfg.Warnings()
	for _, w := range warnings {
		printf(stderr, "warning: %s\n", w)
	}
	for _, n := range vcfg.Notes() {
		printf(stderr, "note: %s\n", n)
	}
	if inv.strict && len(warnings) > 0 {
		printf(stderr, "config: %d warning%s, failing as --strict asks\n", len(warnings), plural(len(warnings)))
		return exitWarnings
	}
	if vcfg.Source() == embeddedSource {
		printf(stdout, "config OK: %s not present or empty; embedded default is valid (created on first run)\n", inv.configPath)
	} else {
		printf(stdout, "config OK: %s\n", configSource(vcfg))
	}
	if o := vcfg.EnvOverrides(); len(o) > 0 {
		printf(stdout, "set from the environment (overrides the config file):\n")
		for _, e := range o {
			printf(stdout, "  %s <- %s\n", e.Key, e.Var)
		}
	}
	if len(inv.names) > 0 {
		printf(stdout, "managing only %s (container names as arguments, the Watchtower form of filters.include_names)\n",
			strings.Join(vcfg.Filters.IncludeNames, ", "))
	}
	return exitOK
}

func runPrintConfig(inv *invocation, stdout, stderr io.Writer) int {
	cfg, err := config.Validate(inv.configPath)
	if err == nil {
		err = applyContainerNames(cfg, inv.names)
	}
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return exitUsage
	}
	enc := yaml.NewEncoder(stdout)
	enc.SetIndent(2)
	if err := enc.Encode(cfg.Redacted()); err != nil {
		printf(stderr, "print-config: %v\n", err)
		return exitFailure
	}
	_ = enc.Close()
	if o := cfg.EnvOverrides(); len(o) > 0 {
		printf(stdout, "# set from the environment (overrides the config file):\n")
		for _, e := range o {
			printf(stdout, "#   %s <- %s\n", e.Key, e.Var)
		}
	}
	return exitOK
}

func runSchema(stdout, stderr io.Writer) int {
	b, err := config.JSONSchema()
	if err != nil {
		printf(stderr, "schema: %v\n", err)
		return exitFailure
	}
	if _, err := stdout.Write(b); err != nil {
		printf(stderr, "schema: %v\n", err)
		return exitFailure
	}
	return exitOK
}
