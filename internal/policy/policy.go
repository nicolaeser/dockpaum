package policy

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/window"
)

const (
	LabelEnable             = "dockpaum.enable"
	LabelPolicy             = "dockpaum.policy"
	LabelComposeGroup       = "dockpaum.compose_group"
	LabelComposeGroupHyphen = "dockpaum.compose-group"

	ComposeProjectLabel = "com.docker.compose.project"
	ComposeServiceLabel = "com.docker.compose.service"
	ComposeNumberLabel  = "com.docker.compose.container-number"
	ComposeDependsLabel = "com.docker.compose.depends_on"

	NoneID = "none"
)

type Resolved struct {
	PolicyID          string
	Mode              string
	MinImageAge       time.Duration
	MinImageAgeSource string
	HealthTimeout     time.Duration
	Windows           *window.Set
	Hooks             config.PolicyHooks
	Scheduled         bool
	ComposeGroup      string
	Skip              bool
	SkipReason        string
	Labels            Labels
	MonitorOnly       bool
	TagInclude        *regexp.Regexp
	TagExclude        *regexp.Regexp
	Tags              registry.TagRules
	Builtin           bool
	Overrides         []string
}

func (r Resolved) TagAllowed(tag string) bool {
	return config.TagAllowed(tag, r.TagInclude, r.TagExclude)
}

type ResolveError struct {
	Label  string
	Value  string
	Reason string
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("%s=%q: %s", e.Label, e.Value, e.Reason)
}

type Schedule struct {
	PolicyID string
	Spec     string
}

type Resolver struct {
	policies   map[string]compiledPolicy
	def        *compiledPolicy
	schedules  []Schedule
	watchtower bool
	logger     *slog.Logger
	warned     sync.Map
}

type compiledPolicy struct {
	id                string
	mode              string
	minImageAge       time.Duration
	minImageAgeSource string
	healthTimeout     time.Duration
	windows           *window.Set
	hooks             config.PolicyHooks
	scheduled         bool
	monitorOnly       bool
	tagInclude        *regexp.Regexp
	tagExclude        *regexp.Regexp
	tagFilter         config.TagFilter
	tags              registry.TagRules
	builtin           bool
}

func NewResolver(cfg *config.Config, logger *slog.Logger) (*Resolver, error) {
	r := &Resolver{policies: map[string]compiledPolicy{}, watchtower: cfg.Filters.HonorWatchtowerLabels(), logger: logger}
	base := config.DefaultPolicy()
	for i, p := range cfg.Update.Policies {
		cp, err := compile(p)
		if err != nil {
			return nil, fmt.Errorf("update.policies[%d] (id=%q): %w", i, p.ID, err)
		}
		if p.IsDefault() {
			cp.id = "default"
			r.def = &cp
			base = p
		}
		r.policies[cp.id] = cp
		if cp.scheduled {
			r.schedules = append(r.schedules, Schedule{PolicyID: cp.id, Spec: p.CronSpec()})
		}
	}
	if r.def == nil {
		def, err := compile(base)
		if err != nil {
			return nil, err
		}
		def.id = "default"
		r.def = &def
		r.policies["default"] = def
	}
	for _, id := range config.BuiltinPolicyIDs() {
		if _, configured := r.policies[id]; configured {
			continue
		}
		bp, _ := config.BuiltinPolicy(id, base)
		cp, err := compile(bp)
		if err != nil {
			return nil, fmt.Errorf("built-in policy %s: %w", id, err)
		}
		cp.builtin = true
		r.policies[id] = cp
	}
	return r, nil
}

func compile(p config.Policy) (compiledPolicy, error) {
	ws, err := compileWindows(p.Timezone, p.Windows)
	if err != nil {
		return compiledPolicy{}, err
	}
	include, exclude, err := p.Tags.Compile()
	if err != nil {
		return compiledPolicy{}, fmt.Errorf("tags.%w", err)
	}
	rules, err := registry.NewTagRules(p.Tags.Include, p.Tags.Exclude, p.Tags.Transform)
	if err != nil {
		return compiledPolicy{}, fmt.Errorf("tags: %w", err)
	}
	source := strings.ToLower(strings.TrimSpace(p.MinImageAgeSource))
	if !config.ValidAgeSource(source) {
		return compiledPolicy{}, fmt.Errorf("min_image_age_source: unknown source %q", p.MinImageAgeSource)
	}
	return compiledPolicy{
		id:                normID(p.ID),
		mode:              normMode(p.Mode),
		minImageAge:       p.MinImageAge.Std(),
		minImageAgeSource: normAgeSource(source),
		healthTimeout:     p.HealthTimeout.Std(),
		windows:           ws,
		hooks:             p.Hooks,
		scheduled:         strings.TrimSpace(p.Schedule) != "",
		monitorOnly:       p.MonitorOnly,
		tagInclude:        include,
		tagExclude:        exclude,
		tagFilter:         p.Tags,
		tags:              rules,
	}, nil
}

func normAgeSource(s string) string {
	if s == "" {
		return config.AgeSourceAuto
	}
	return s
}

func normMode(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" || m == config.ModeDigest {
		return config.ModeTagFollow
	}
	return m
}

func (r *Resolver) Schedules() []Schedule {
	return append([]Schedule(nil), r.schedules...)
}

func (r *Resolver) IDs() []string {
	ids := make([]string, 0, len(r.policies)+2)
	for id := range r.policies {
		ids = append(ids, id)
	}
	ids = append(ids, config.PolicyNone, config.PolicyOff)
	slices.Sort(ids)
	return ids
}

func compileWindows(tz string, specs []config.WindowSpec) (*window.Set, error) {
	if len(specs) == 0 {
		return window.AlwaysAllowed(), nil
	}
	cs := make([]window.Spec, len(specs))
	for i, s := range specs {
		cs[i] = window.Spec{Days: s.Days, Times: s.Times}
	}
	return window.Compile(tz, cs)
}

func (r *Resolver) Resolve(ct docker.Container, labels map[string]string) Resolved {
	res, _ := r.ResolveStrict(ct, labels)
	return res
}

func (r *Resolver) ResolveStrict(ct docker.Container, labels map[string]string) (Resolved, error) {
	if labels == nil {
		labels = ct.Labels
	}
	cl := ReadLabels(labels, r.watchtower)
	group := composeGroup(labels)
	if cl.Enable != nil && !*cl.Enable {
		return Resolved{Skip: true, SkipReason: "dockpaum.enable=false", ComposeGroup: group, Labels: cl}, nil
	}

	fail := func(id string, e *ResolveError) (Resolved, error) {
		if _, seen := r.warned.LoadOrStore(ct.Name+"\x00"+e.Label+"\x00"+e.Value, true); !seen && r.logger != nil {
			r.logger.Warn("container labels do not resolve to a policy; the container is skipped",
				"container", ct.Name, "label", e.Label, "value", e.Value, "reason", e.Reason)
		}
		return Resolved{PolicyID: id, Skip: true, SkipReason: e.Error(), ComposeGroup: group, Labels: cl}, e
	}

	raw := labels[LabelPolicy]
	wantID := normID(raw)
	if config.IsReservedPolicyID(wantID) {
		return Resolved{
			PolicyID:     NoneID,
			Skip:         true,
			SkipReason:   LabelPolicy + "=" + wantID,
			ComposeGroup: group,
			Labels:       cl,
		}, nil
	}

	cp := *r.def
	if wantID != "" && wantID != "default" {
		p, ok := r.policies[wantID]
		if !ok {
			return fail(wantID, &ResolveError{Label: LabelPolicy, Value: raw, Reason: r.unknownPolicy(wantID)})
		}
		cp = p
	}

	res := Resolved{
		PolicyID:          cp.id,
		Mode:              cp.mode,
		MinImageAge:       cp.minImageAge,
		MinImageAgeSource: cp.minImageAgeSource,
		HealthTimeout:     cp.healthTimeout,
		Windows:           cp.windows,
		Hooks:             cp.hooks,
		Scheduled:         cp.scheduled,
		ComposeGroup:      group,
		Labels:            cl,
		MonitorOnly:       cp.monitorOnly,
		TagInclude:        cp.tagInclude,
		TagExclude:        cp.tagExclude,
		Tags:              cp.tags,
		Builtin:           cp.builtin,
	}

	ov, err := ReadOverrides(labels)
	if err != nil {
		var rerr *ResolveError
		if !errors.As(err, &rerr) {
			rerr = &ResolveError{Label: "labels", Reason: err.Error()}
		}
		return fail(cp.id, rerr)
	}
	if ov.Mode != "" {
		res.Mode = normMode(ov.Mode)
	}
	if ov.MinImageAge != nil {
		res.MinImageAge = *ov.MinImageAge
	}
	if ov.HealthTimeout != nil {
		res.HealthTimeout = *ov.HealthTimeout
	}
	if ov.MinImageAgeSource != "" {
		res.MinImageAgeSource = normAgeSource(ov.MinImageAgeSource)
	}
	if ov.TagInclude != nil {
		res.TagInclude = ov.TagInclude
	}
	if ov.TagExclude != nil {
		res.TagExclude = ov.TagExclude
	}
	if !ov.Tags.IsZero() {
		rules, rerr := labelTagRules(cp.tagFilter, ov.Tags)
		if rerr != nil {
			return fail(cp.id, rerr)
		}
		res.Tags = rules
	}
	res.Overrides = ov.Labels

	monitor, set, merr := r.monitorOnlyLabel(labels)
	if merr != nil {
		return fail(cp.id, merr)
	}
	if set {
		res.MonitorOnly = monitor
	}
	res.Labels.MonitorOnly = res.MonitorOnly
	if res.Builtin && r.logger != nil {
		if _, seen := r.warned.LoadOrStore(ct.Name+"\x00builtin\x00"+cp.id, true); !seen {
			r.logger.Info("container uses a built-in policy (define a policy with this id to change it)",
				"container", ct.Name, "policy", cp.id, "mode", res.Mode, "monitor_only", res.MonitorOnly)
		}
	}
	return res, nil
}

func labelTagRules(policy, labels config.TagFilter) (registry.TagRules, *ResolveError) {
	merged := policy
	if labels.Include != "" {
		merged.Include = labels.Include
	}
	if labels.Exclude != "" {
		merged.Exclude = labels.Exclude
	}
	if labels.Transform != "" {
		merged.Transform = labels.Transform
	}
	rules, err := registry.NewTagRules(merged.Include, merged.Exclude, merged.Transform)
	if err != nil {
		label, value := LabelTagTransform, labels.Transform
		switch {
		case labels.Include != "":
			label, value = LabelTagInclude, labels.Include
		case labels.Exclude != "":
			label, value = LabelTagExclude, labels.Exclude
		}
		return registry.TagRules{}, &ResolveError{Label: label, Value: value, Reason: err.Error()}
	}
	return rules, nil
}

func (r *Resolver) unknownPolicy(id string) string {
	ids := r.IDs()
	msg := "unknown policy"
	if s := config.Suggest(id, ids); s != "" {
		msg += fmt.Sprintf(" — did you mean %q?", s)
	}
	return msg + " (known: " + strings.Join(ids, ", ") + ")"
}

func (r *Resolver) monitorOnlyLabel(labels map[string]string) (on, set bool, err *ResolveError) {
	name := LabelMonitorOnly
	v, ok := labels[name]
	if !ok && r.watchtower {
		name = WatchtowerPrefix + "monitor-only"
		v, ok = labels[name]
	}
	if !ok || strings.TrimSpace(v) == "" {
		return false, false, nil
	}
	b, known := parseBool(v)
	if !known {
		return false, false, &ResolveError{Label: name, Value: v, Reason: "want true or false"}
	}
	return b, true, nil
}

func composeGroup(labels map[string]string) string {
	if v := labels[LabelComposeGroupHyphen]; v != "" {
		return v
	}
	if v := labels[LabelComposeGroup]; v != "" {
		return v
	}
	return labels[ComposeProjectLabel]
}

func normID(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
