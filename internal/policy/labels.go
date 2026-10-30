package policy

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

const (
	LabelScope        = "dockpaum.scope"
	LabelMonitorOnly  = "dockpaum.monitor-only"
	LabelNoPull       = "dockpaum.no-pull"
	LabelHookPre      = "dockpaum.hook.pre-update"
	LabelHookPost     = "dockpaum.hook.post-update"
	LabelHookFailure  = "dockpaum.hook.on-failure"
	LabelLinkTemplate = "dockpaum.link.template"

	WatchtowerPrefix = "com.centurylinklabs.watchtower."
)

const (
	LabelMode              = "dockpaum.mode"
	LabelMinImageAge       = "dockpaum.min-image-age"
	LabelMinAge            = "dockpaum.min-age"
	LabelHealthTimeout     = "dockpaum.health-timeout"
	LabelMinImageAgeSource = "dockpaum.min-image-age-source"
	LabelTagInclude        = "dockpaum.tag.include"
	LabelTagExclude        = "dockpaum.tag.exclude"
	LabelTagTransform      = "dockpaum.tag.transform"
)

type Overrides struct {
	Mode              string
	MinImageAge       *time.Duration
	HealthTimeout     *time.Duration
	MinImageAgeSource string
	TagInclude        *regexp.Regexp
	TagExclude        *regexp.Regexp
	Tags              config.TagFilter
	Labels            []string
}

func ReadOverrides(labels map[string]string) (Overrides, error) {
	var out Overrides
	get := func(name string) (string, bool) {
		v := strings.TrimSpace(labels[name])
		if v != "" {
			out.Labels = append(out.Labels, name)
		}
		return v, v != ""
	}
	if v, ok := get(LabelMode); ok {
		m := strings.ToLower(v)
		if !config.ValidMode(m) {
			allowed := []string{config.ModeTagFollow, config.ModePatch, config.ModeMinor, config.ModeMajor, config.ModeNone}
			reason := "unknown mode"
			if s := config.Suggest(m, allowed); s != "" {
				reason += fmt.Sprintf(" — did you mean %q?", s)
			}
			return Overrides{}, &ResolveError{Label: LabelMode, Value: labels[LabelMode], Reason: reason + " (want " + strings.Join(allowed, ", ") + ")"}
		}
		out.Mode = m
	}
	minAge := LabelMinImageAge
	if strings.TrimSpace(labels[LabelMinImageAge]) == "" && strings.TrimSpace(labels[LabelMinAge]) != "" {
		minAge = LabelMinAge
	}
	for _, d := range []struct {
		label string
		dst   **time.Duration
	}{{minAge, &out.MinImageAge}, {LabelHealthTimeout, &out.HealthTimeout}} {
		v, ok := get(d.label)
		if !ok {
			continue
		}
		parsed, err := config.ParseDuration(v)
		switch {
		case err != nil:
			return Overrides{}, &ResolveError{Label: d.label, Value: labels[d.label], Reason: fmt.Sprintf("not a duration (%v) — write it like 30m, 2h or 1d; 0s turns the gate off", err)}
		case parsed < 0:
			return Overrides{}, &ResolveError{Label: d.label, Value: labels[d.label], Reason: "must not be negative"}
		}
		*d.dst = &parsed
	}
	if v, ok := get(LabelMinImageAgeSource); ok {
		s := strings.ToLower(v)
		if !config.ValidAgeSource(s) {
			allowed := []string{config.AgeSourceAuto, config.AgeSourceFirstSeen, config.AgeSourceCreated}
			reason := "unknown source"
			if sg := config.Suggest(s, allowed); sg != "" {
				reason += fmt.Sprintf(" — did you mean %q?", sg)
			}
			return Overrides{}, &ResolveError{Label: LabelMinImageAgeSource, Value: labels[LabelMinImageAgeSource], Reason: reason + " (want " + strings.Join(allowed, ", ") + ")"}
		}
		out.MinImageAgeSource = s
	}
	for _, re := range []struct {
		label string
		raw   *string
		dst   **regexp.Regexp
	}{{LabelTagInclude, &out.Tags.Include, &out.TagInclude}, {LabelTagExclude, &out.Tags.Exclude, &out.TagExclude}} {
		v, ok := get(re.label)
		if !ok {
			continue
		}
		compiled, _, err := config.TagFilter{Include: v}.Compile()
		if err != nil {
			return Overrides{}, &ResolveError{Label: re.label, Value: labels[re.label], Reason: strings.TrimPrefix(err.Error(), "include: ")}
		}
		*re.raw, *re.dst = v, compiled
	}
	if v, ok := get(LabelTagTransform); ok {
		if _, err := registry.NewTagRules("", "", v); err != nil {
			return Overrides{}, &ResolveError{Label: LabelTagTransform, Value: labels[LabelTagTransform], Reason: transformReason(err)}
		}
		out.Tags.Transform = v
	}
	slices.Sort(out.Labels)
	return out, nil
}

func transformReason(err error) string {
	msg := strings.TrimSpace(strings.TrimPrefix(err.Error(), "tag transform"))
	if q, qerr := strconv.QuotedPrefix(msg); qerr == nil {
		msg = msg[len(q):]
	}
	return strings.TrimSpace(strings.TrimPrefix(msg, ":"))
}

type Labels struct {
	Enable       *bool
	Scope        string
	MonitorOnly  bool
	NoPull       bool
	PreUpdate    string
	PostUpdate   string
	OnFailure    string
	LinkTemplate string
}

func ReadLabels(labels map[string]string, watchtower bool) Labels {
	get := func(native, wt string) (string, bool) {
		if v, ok := labels[native]; ok {
			return strings.TrimSpace(v), true
		}
		if watchtower && wt != "" {
			if v, ok := labels[WatchtowerPrefix+wt]; ok {
				return strings.TrimSpace(v), true
			}
		}
		return "", false
	}
	var out Labels
	if v, ok := get(LabelEnable, "enable"); ok {
		on, known := parseBool(v)
		on = on || !known
		out.Enable = &on
	}
	out.Scope, _ = get(LabelScope, "scope")
	if v, ok := get(LabelMonitorOnly, "monitor-only"); ok {
		out.MonitorOnly, _ = parseBool(v)
	}
	if v, ok := get(LabelNoPull, "no-pull"); ok {
		out.NoPull, _ = parseBool(v)
	}
	out.PreUpdate, _ = get(LabelHookPre, "lifecycle.pre-update")
	out.PostUpdate, _ = get(LabelHookPost, "lifecycle.post-update")
	out.OnFailure, _ = get(LabelHookFailure, "")
	out.LinkTemplate, _ = get(LabelLinkTemplate, "")
	return out
}

func parseBool(v string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "yes", "y", "on":
		return true, true
	case "0", "f", "false", "no", "n", "off":
		return false, true
	}
	return false, false
}
