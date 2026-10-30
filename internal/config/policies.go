package config

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/registry/tagrule"
)

const (
	PolicyMonitor = "monitor"
	PolicyNone    = "none"
	PolicyOff     = "off"
)

var builtinPolicyIDs = []string{ModeTagFollow, ModePatch, ModeMinor, ModeMajor, PolicyMonitor}

func BuiltinPolicyIDs() []string { return slices.Clone(builtinPolicyIDs) }

func IsReservedPolicyID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return id == PolicyNone || id == PolicyOff
}

func BuiltinPolicy(id string, base Policy) (p Policy, ok bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !slices.Contains(builtinPolicyIDs, id) {
		return Policy{}, false
	}
	p = Policy{
		ID:                id,
		Mode:              id,
		MinImageAge:       base.MinImageAge,
		MinImageAgeSource: base.MinImageAgeSource,
		HealthTimeout:     base.HealthTimeout,
		Timezone:          base.Timezone,
		Windows:           cloneWindows(base.Windows),
		minImageAgeSet:    true,
		healthTimeoutSet:  true,
	}
	if id == PolicyMonitor {
		p.Mode = ModeTagFollow
		p.MonitorOnly = true
	}
	return p, true
}

func cloneWindows(ws []WindowSpec) []WindowSpec {
	if ws == nil {
		return nil
	}
	out := make([]WindowSpec, len(ws))
	for i, w := range ws {
		out[i] = WindowSpec{Days: slices.Clone(w.Days), Times: slices.Clone(w.Times)}
	}
	return out
}

var modes = []string{ModeTagFollow, ModePatch, ModeMinor, ModeMajor, ModeNone, ModeDigest}

func IsSemverMode(mode string) bool {
	return mode == ModePatch || mode == ModeMinor || mode == ModeMajor
}

type TagFilter struct {
	Include   string `yaml:"include"`
	Exclude   string `yaml:"exclude"`
	Transform string `yaml:"transform"`
}

func (t TagFilter) IsZero() bool {
	return strings.TrimSpace(t.Include) == "" && strings.TrimSpace(t.Exclude) == "" && strings.TrimSpace(t.Transform) == ""
}

func (t TagFilter) Compile() (include, exclude *regexp.Regexp, err error) {
	if include, err = compileTagRE(t.Include); err != nil {
		return nil, nil, fmt.Errorf("include: %w", err)
	}
	if exclude, err = compileTagRE(t.Exclude); err != nil {
		return nil, nil, fmt.Errorf("exclude: %w", err)
	}
	return include, exclude, nil
}

func TagAllowed(tag string, include, exclude *regexp.Regexp) bool {
	if include != nil && !include.MatchString(tag) {
		return false
	}
	return exclude == nil || !exclude.MatchString(tag)
}

func (t TagFilter) checkTransform() error {
	if strings.TrimSpace(t.Transform) == "" {
		return nil
	}
	if _, err := tagrule.Compile("", "", t.Transform); err != nil {
		return errors.New(transformProblem(err))
	}
	return nil
}

func transformProblem(err error) string {
	msg := strings.TrimPrefix(err.Error(), "tag transform")
	return strings.TrimSpace(strings.TrimPrefix(msg, ":"))
}

func compileTagRE(expr string) (*regexp.Regexp, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	if len(expr) > tagrule.MaxLen {
		return nil, fmt.Errorf("the expression is longer than %d characters", tagrule.MaxLen)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid RE2 regular expression: %s", expr, regexpProblem(expr, err))
	}
	return re, nil
}

func regexpProblem(expr string, err error) string {
	var se *syntax.Error
	if !errors.As(err, &se) {
		return err.Error()
	}
	if se.Expr == "" || se.Expr == expr {
		return string(se.Code)
	}
	return fmt.Sprintf("%s: %s", se.Code, se.Expr)
}
