package registry

import (
	"regexp"

	"github.com/nicolaeser/dockpaum/internal/registry/tagrule"
)

type TagRules struct {
	rules tagrule.Rules
}

func NewTagRules(include, exclude, transform string) (TagRules, error) {
	r, err := tagrule.Compile(include, exclude, transform)
	if err != nil {
		return TagRules{}, err
	}
	return TagRules{rules: r}, nil
}

func CompiledTagRules(include, exclude *regexp.Regexp) TagRules {
	return TagRules{rules: tagrule.FromRegexps(include, exclude)}
}

func (r TagRules) IsZero() bool { return r.rules.IsZero() }

func (r TagRules) Allows(tag string) bool { return r.rules.Allows(tag) }

func (r TagRules) Transform(tag string) string { return r.rules.Transform(tag) }

func (r TagRules) ParseTag(tag string) (Tag, bool) {
	t, ok := ParseTag(r.Transform(tag))
	t.Raw = tag
	return t, ok
}
