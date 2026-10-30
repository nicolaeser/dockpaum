package cli

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/updater"
	"github.com/nicolaeser/dockpaum/internal/window"
)

const (
	actionUpdate  = "update"
	actionReport  = "report"
	actionWait    = "wait"
	actionHold    = "hold"
	actionNone    = "none"
	actionUnknown = "unknown"
	actionError   = "error"
)

type planDoc struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Instance    string           `json:"instance"`
	DryRun      bool             `json:"dry_run"`
	Schedule    string           `json:"schedule"`
	Containers  []planEntry      `json:"containers"`
	Unmanaged   []unmanagedEntry `json:"unmanaged"`
	Errors      []string         `json:"errors"`
}

type planEntry struct {
	Host         string     `json:"host"`
	Container    string     `json:"container"`
	Image        string     `json:"image"`
	Policy       string     `json:"policy"`
	PolicySource string     `json:"policy_source"`
	Overrides    []string   `json:"overrides,omitempty"`
	Mode         string     `json:"mode,omitempty"`
	Scheduled    bool       `json:"scheduled,omitempty"`
	Current      planImage  `json:"current"`
	Candidate    *planImage `json:"candidate,omitempty"`
	Update       string     `json:"update"`
	Decision     string     `json:"decision"`
	Action       string     `json:"action"`
	Reason       string     `json:"reason,omitempty"`
	Error        string     `json:"error,omitempty"`
	EligibleAt   *time.Time `json:"eligible_at,omitempty"`
	NextWindow   *time.Time `json:"next_window,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"`

	checkNote string
}

type planImage struct {
	Image   string `json:"image,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

type unmanagedEntry struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	Reason    string `json:"reason"`
}

type planClock struct {
	regular  cron.Schedule
	interval time.Duration
	global   *window.Set
	policies map[string]cron.Schedule
}

func newPlanClock(cfg *config.Config, r *policy.Resolver) planClock {
	c := planClock{policies: map[string]cron.Schedule{}}
	if cfg.Schedule.Cron != "" {
		if s, err := config.ParseCron(cfg.Schedule.CronSpec()); err == nil {
			c.regular = s
			if every, ok := s.(cron.ConstantDelaySchedule); ok {
				c.regular, c.interval = nil, every.Delay
			}
		}
	} else {
		c.interval = cfg.Schedule.Interval.Std()
		if c.interval <= 0 {
			c.interval = config.DefaultInterval
		}
	}
	specs := make([]window.Spec, len(cfg.Schedule.Windows))
	for i, w := range cfg.Schedule.Windows {
		specs[i] = window.Spec{Days: w.Days, Times: w.Times}
	}
	if ws, err := window.Compile(cfg.Schedule.Timezone, specs); err == nil {
		c.global = ws
	}
	for _, ps := range r.Schedules() {
		if s, err := config.ParseCron(ps.Spec); err == nil {
			c.policies[ps.PolicyID] = s
		}
	}
	return c
}

const maxScheduleScan = 20000

func (c planClock) nextRun(from time.Time, policyID string, sets ...*window.Set) (time.Time, bool) {
	sched := c.regular
	if own, ok := c.policies[policyID]; ok {
		sched = own
	}
	if sched == nil {
		return time.Time{}, false
	}
	sets = append(sets, c.global)
	limit := from.AddDate(1, 0, 0)
	t := from.Add(-time.Second)
	for range maxScheduleScan {
		t = sched.Next(t)
		if t.IsZero() || t.After(limit) {
			return time.Time{}, false
		}
		if allowsAll(t, sets...) {
			return t, true
		}
	}
	return time.Time{}, false
}

func allowsAll(t time.Time, sets ...*window.Set) bool {
	for _, s := range sets {
		if !s.Empty() && !s.Allows(t) {
			return false
		}
	}
	return true
}

func nextOpening(from time.Time, sets ...*window.Set) (time.Time, bool) {
	t := from
	if allowsAll(t, sets...) {
		return t, true
	}
	t = t.Truncate(time.Minute)
	for m := 1; m <= int(window.Week/time.Minute)+1; m++ {
		if c := t.Add(time.Duration(m) * time.Minute); allowsAll(c, sets...) {
			return c, true
		}
	}
	return time.Time{}, false
}

type resolved struct {
	policy.Resolved
	err    error
	source string
	label  string
}

func (ev *evaluation) resolve(ct docker.Container) resolved {
	eff, err := ev.resolver.ResolveStrict(ct, ct.Labels)
	r := resolved{Resolved: eff, err: err, source: "default", label: strings.TrimSpace(ct.Labels[policy.LabelPolicy])}
	if r.label != "" {
		r.source = "label"
	}
	return r
}

func (ev *evaluation) entries() []planEntry {
	clock := newPlanClock(ev.cfg, ev.resolver)
	out := make([]planEntry, 0, len(ev.statuses))
	for _, s := range ev.statuses {
		ct, _ := ev.container(s.Host, s.Container)
		if ct.Labels == nil {
			ct = docker.Container{Name: s.Container, Image: s.Image}
		}
		out = append(out, ev.entry(s, ev.resolve(ct), clock))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Container < out[j].Container
	})
	return out
}

func (ev *evaluation) entry(s updater.ContainerStatus, r resolved, clock planClock) planEntry {
	rec := recordedOf(s)
	e := planEntry{
		Host: s.Host, Container: s.Container, Image: s.Image,
		Policy: s.Policy, PolicySource: r.source, Overrides: r.Overrides, Mode: r.Mode, Scheduled: r.Scheduled,
		Current: planImage{Image: s.Image, Tag: s.Current.Tag, Digest: s.Current.Digest,
			Version: cmp.Or(rec.currentVersion, ev.versions[s.Host][s.Current.Digest])},
		Update: s.Update, Decision: s.Decision, Reason: s.Reason, Error: s.Error,
		checkNote: rec.checkNote,
	}
	if e.Policy == "" {
		e.Policy = r.PolicyID
	}
	if s.Candidate != nil {
		e.Candidate = &planImage{
			Image: s.Candidate.Image, Tag: s.Candidate.Tag, Digest: s.Candidate.Digest,
			Version: cmp.Or(rec.candidateVersion, ev.versions[s.Host][s.Candidate.Digest]),
		}
	}
	utc := func(t time.Time) *time.Time {
		t = t.UTC()
		return &t
	}
	if !s.EligibleAt.IsZero() {
		e.EligibleAt = utc(s.EligibleAt)
	}
	e.Action = ev.action(s, e.checkNote)
	if e.Action == actionWait && s.Decision == string(audit.DecisionGatedWindow) {
		if open, ok := nextOpening(ev.now, r.Windows); ok {
			e.NextWindow = utc(open)
		}
	}
	switch e.Action {
	case actionUpdate, actionReport, actionWait:
		from := ev.now
		if e.EligibleAt != nil && e.EligibleAt.After(from) {
			from = *e.EligibleAt
		}
		if next, ok := clock.nextRun(from, r.PolicyID, r.Windows); ok {
			e.NextRun = utc(next)
		}
	}
	e.Reason = ev.reason(s, e)
	return e
}

func (ev *evaluation) action(s updater.ContainerStatus, checkNote string) string {
	d := audit.Decision(s.Decision)
	switch {
	case d == audit.DecisionDryRun && ev.cfg.DryRun:
		return actionReport
	case d == audit.DecisionDryRun:
		return actionUpdate
	case d == audit.DecisionMonitorOnly:
		return actionReport
	case d == audit.DecisionGatedWindow && s.Update == updater.UpdateNone:
		return actionNone
	case d == audit.DecisionGatedAge, d == audit.DecisionGatedWindow:
		return actionWait
	case d.Gated():
		return actionHold
	case d == audit.DecisionSkipped:
		return actionNone
	case d.Failed():
		return actionError
	case d == audit.DecisionCheckIncomplete && checkNote != "":
		return actionError
	}
	return actionUnknown
}

func (ev *evaluation) reason(s updater.ContainerStatus, e planEntry) string {
	if s.Error != "" {
		return s.Error
	}
	if e.Action == actionNone && s.Decision == string(audit.DecisionGatedWindow) {
		return "image up-to-date (" + s.Reason + ")"
	}
	if s.Decision != string(audit.DecisionDryRun) {
		return s.Reason
	}
	what := "newer image available"
	if e.Candidate != nil && e.Candidate.Tag != "" && e.Candidate.Tag != e.Current.Tag {
		what = "newer tag " + e.Candidate.Tag
	}
	switch {
	case ev.cfg.DryRun:
		what += "; dry_run is on, so it is reported, not applied"
	case approvalConfigured(ev.cfg) && ev.cfg.Update.Approval.RecheckBeforeStop:
		what += "; the approval endpoint is asked first, and again right before the stop"
	case approvalConfigured(ev.cfg):
		what += "; the approval endpoint is asked first"
	}
	return what
}

func approvalConfigured(cfg *config.Config) bool {
	return cfg.Update.Approval != nil && strings.TrimSpace(cfg.Update.Approval.URL) != ""
}

const (
	verdictPass       = "pass"
	verdictStop       = "stop"
	verdictNotReached = "not reached"
	verdictAtUpdate   = "at update"
)

type gate struct {
	Gate    string `json:"gate"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
	Change  string `json:"change,omitempty"`
}

const (
	gateFilters    = "filters"
	gatePolicy     = "policy"
	gateContainer  = "container"
	gateMode       = "mode"
	gatePin        = "digest pin"
	gateRegistry   = "registry"
	gateWindow     = "window"
	gateSelfUpdate = "self-update"
	gateSoak       = "soak"
	gateMonitor    = "monitor-only"
	gateDryRun     = "dry-run"
	gateApproval   = "approval"
	gateUpdate     = "update"
)

type explained struct {
	planEntry
	Managed bool   `json:"managed"`
	Gates   []gate `json:"gates"`
}

type explainDoc struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Instance    string      `json:"instance"`
	DryRun      bool        `json:"dry_run"`
	Containers  []explained `json:"containers"`
	Errors      []string    `json:"errors"`
}

func (ev *evaluation) explain(name string) []explained {
	clock := newPlanClock(ev.cfg, ev.resolver)
	byHost := map[string]updater.ContainerStatus{}
	for _, s := range ev.statuses {
		if s.Container == name {
			byHost[s.Host] = s
		}
	}
	var out []explained
	for _, h := range ev.listed {
		ct, ok := ev.container(h.host, name)
		if !ok {
			continue
		}
		r := ev.resolve(ct)
		s, processed := byHost[h.host]
		if !processed {
			out = append(out, ev.explainUnmanaged(h.host, ct, r))
			continue
		}
		e := ev.entry(s, r, clock)
		out = append(out, explained{planEntry: e, Managed: true, Gates: ev.gates(s, r, e)})
	}
	return out
}

func (ev *evaluation) explainUnmanaged(host string, ct docker.Container, r resolved) explained {
	why := filterVerdict(ct, &ev.cfg.Filters)
	change := filterChange(ct, &ev.cfg.Filters)
	if why == "" {
		why = "not evaluated; the warnings above may say why"
		change = ""
	}
	e := planEntry{
		Host: host, Container: ct.Name, Image: ct.Image, Policy: r.PolicyID, PolicySource: r.source, Mode: r.Mode,
		Current: planImage{Image: ct.Image, Tag: registry.Parse(ct.Image).Tag, Digest: ct.ImageID},
		Update:  updater.UpdateUnknown, Action: actionHold, Reason: why,
	}
	gates := []gate{{Gate: gateFilters, Verdict: verdictStop, Detail: why, Change: change}}
	for _, g := range gateOrder[1:] {
		gates = append(gates, gate{Gate: g, Verdict: verdictNotReached})
	}
	return explained{planEntry: e, Gates: gates}
}

func filterChange(ct docker.Container, f *config.FilterConfig) string {
	cl := policy.ReadLabels(ct.Labels, f.HonorWatchtowerLabels())
	switch {
	case !f.IncludeStopped && ct.State != "running":
		return "start it, or set filters.include_stopped: true"
	case leftoverRE.MatchString(ct.Name):
		return ""
	case f.LabelEnable && (cl.Enable == nil || !*cl.Enable):
		return "label it " + policy.LabelEnable + "=true"
	case cl.Enable != nil && !*cl.Enable:
		return "remove the label, or set it to true"
	case f.Scope != cl.Scope:
		return "label it " + policy.LabelScope + "=" + f.Scope + " (an empty scope: no label), or run it under the instance of its scope"
	case len(f.IncludeNames) > 0 && !slices.Contains(f.IncludeNames, ct.Name):
		return "add it to filters.include_names"
	case slices.Contains(f.ExcludeNames, ct.Name):
		return "remove it from filters.exclude_names"
	}
	return "change filters.include_regex or filters.exclude_regex"
}

var gateOrder = []string{
	gateFilters, gatePolicy, gateContainer, gateMode, gatePin, gateRegistry, gateWindow,
	gateSoak, gateMonitor, gateDryRun, gateApproval, gateUpdate,
}

func stopGate(s updater.ContainerStatus, r resolved) string {
	switch audit.Decision(s.Decision) {
	case audit.DecisionGatedPolicy:
		switch {
		case r.Skip:
			return gatePolicy
		case r.Mode == config.ModeNone:
			return gateMode
		case s.Update == updater.UpdateAvailable || s.Update == updater.UpdateNone:
			return gateSelfUpdate
		}
		return gateContainer
	case audit.DecisionGatedOrchestrated, audit.DecisionGatedAutoRemove, audit.DecisionGatedMACAddress, audit.DecisionFailed:
		return gateContainer
	case audit.DecisionGatedWindow:
		if s.Update == updater.UpdateNone {
			return gateRegistry
		}
		return gateWindow
	case audit.DecisionGatedDigest:
		return gatePin
	case audit.DecisionSkipped, audit.DecisionCheckFailed, audit.DecisionCheckIncomplete:
		return gateRegistry
	case audit.DecisionGatedAge:
		return gateSoak
	case audit.DecisionMonitorOnly:
		return gateMonitor
	case audit.DecisionApprovalDeny:
		return gateApproval
	}
	return ""
}

func (ev *evaluation) gates(s updater.ContainerStatus, r resolved, e planEntry) []gate {
	stop := stopGate(s, r)
	order := gateOrder
	if stop == gateSelfUpdate {
		at := slices.Index(gateOrder, gateWindow) + 1
		order = slices.Insert(slices.Clone(gateOrder), at, gateSelfUpdate)
	}
	reached := true
	var out []gate
	for _, name := range order {
		g := gate{Gate: name}
		switch {
		case !reached:
			g.Verdict = verdictNotReached
			g.Detail = ev.gateInfo(name, r)
		case name == stop:
			g.Verdict = verdictStop
			g.Detail = e.Reason
			g.Change = ev.gateChange(name, s, r, e)
			reached = false
		default:
			g.Verdict, g.Detail, g.Change = ev.gatePassed(name, s, r, e)
			if g.Verdict == verdictStop {
				reached = false
			}
		}
		out = append(out, g)
	}
	return out
}

func (ev *evaluation) gatePassed(name string, s updater.ContainerStatus, r resolved, e planEntry) (verdict, detail, change string) {
	switch name {
	case gateFilters:
		return verdictPass, "managed by this instance", ""
	case gatePolicy:
		return verdictPass, ev.policyText(r), ""
	case gateContainer:
		return verdictPass, "not owned by an orchestrator, not started with --rm", ""
	case gateMode:
		return verdictPass, modeText(r.Mode), ""
	case gateWindow:
		if r.Windows.Empty() {
			return verdictPass, "the policy has no maintenance window", ""
		}
		return verdictPass, "the policy's window is open now", ""
	case gatePin:
		return verdictPass, "follows a tag, not a digest", ""
	case gateRegistry:
		switch {
		case e.Candidate != nil:
			return verdictPass, "newer image: " + imageText(*e.Candidate, e.Current.Tag), ""
		case s.Update == updater.UpdateNone:
			return verdictPass, "image up-to-date", ""
		case s.Decision == string(audit.DecisionGatedWindow):
			return verdictNotReached, "not established while the window is closed", ""
		}
		return verdictPass, "checked", ""
	case gateSoak:
		if r.MinImageAge <= 0 {
			return verdictPass, "no soak time (min_image_age 0)", ""
		}
		detail := "min_image_age " + shortDuration(r.MinImageAge)
		if e.EligibleAt != nil {
			detail = fmt.Sprintf("old enough since %s (%s)", e.EligibleAt.In(ev.location()).Format(timeLayout), detail)
		}
		return verdictPass, detail, ""
	case gateMonitor:
		return verdictPass, "updates are applied, not only reported", ""
	case gateDryRun:
		if ev.cfg.DryRun {
			return verdictStop, "dry_run is on: the daemon reports the update and changes nothing",
				"set dry_run: false (or DOCKPAUM_DRY_RUN=false) once the plan looks right"
		}
		return verdictPass, "dry_run is off: the daemon applies updates", ""
	case gateApproval:
		return verdictAtUpdate, ev.approvalText(), ""
	case gateUpdate:
		return verdictAtUpdate, ev.updateText(r), ""
	}
	return verdictPass, "", ""
}

func (ev *evaluation) gateInfo(name string, r resolved) string {
	if r.Skip {
		return ""
	}
	switch name {
	case gateMode:
		if r.Mode != "" {
			return modeText(r.Mode)
		}
	case gateWindow:
		if r.Windows.Empty() {
			return "the policy has no maintenance window"
		}
		if r.Windows.Allows(ev.now) {
			return "the policy's window is open now"
		}
		if open, ok := nextOpening(ev.now, r.Windows); ok {
			return "the policy's window opens " + open.In(ev.location()).Format(timeLayout)
		}
	case gateSoak:
		if r.MinImageAge > 0 {
			return "min_image_age " + shortDuration(r.MinImageAge)
		}
		if r.PolicyID != "" {
			return "no soak time"
		}
	case gateMonitor:
		if r.MonitorOnly {
			return "monitor-only: updates are reported, never applied"
		}
	case gateDryRun:
		if ev.cfg.DryRun {
			return "dry_run is on: the daemon only reports"
		}
	case gateApproval:
		return ev.approvalText()
	case gateUpdate:
		return ev.updateText(r)
	}
	return ""
}

func (ev *evaluation) gateChange(name string, s updater.ContainerStatus, r resolved, e planEntry) string {
	switch name {
	case gatePolicy:
		switch {
		case r.err != nil:
			return "fix the label the reason names"
		case r.label != "" && config.IsReservedPolicyID(strings.ToLower(r.label)):
			return "remove " + policy.LabelPolicy + "=" + r.label + " or name a policy"
		}
		return "remove the " + policy.LabelEnable + "=false label"
	case gateContainer:
		switch audit.Decision(s.Decision) {
		case audit.DecisionGatedOrchestrated:
			return "update it through its orchestrator; dockpaum never replaces such containers"
		case audit.DecisionGatedAutoRemove:
			return "recreate it without --rm (auto_remove); dockpaum never replaces such containers"
		case audit.DecisionGatedMACAddress:
			return "upgrade Docker Engine to 25 or newer, or recreate the container without --mac-address"
		case audit.DecisionFailed:
			return "check the Docker host; the next run tries again"
		}
		return "set update.self_update.enabled: true, or make sure dockpaum can find its own container on the configured hosts"
	case gateMode:
		return fmt.Sprintf("give policy %q a mode other than none, or label the container %s=tag-follow", r.PolicyID, policy.LabelMode)
	case gateWindow:
		if e.NextWindow != nil {
			return fmt.Sprintf("wait until %s, or change the windows of policy %q", e.NextWindow.In(ev.location()).Format(timeLayout), r.PolicyID)
		}
		return fmt.Sprintf("change the windows of policy %q", r.PolicyID)
	case gatePin:
		return "reference the image by tag (image:tag) instead of @sha256:..."
	case gateRegistry:
		switch audit.Decision(s.Decision) {
		case audit.DecisionCheckFailed:
			return "fix the registry access (credentials, network, rate limit); the next run tries again"
		case audit.DecisionCheckIncomplete:
			if e.checkNote != "" {
				return "let dockpaum ask the registry directly (network, proxy, credentials, rate limit), and run the image as pulled from it: " +
					"a locally built or loaded image records no registry digest (label those " + policy.LabelNoPull + "=true). " +
					"Until then the daemon pulls to compare; plan and explain never do"
			}
			return "use a tag the registry lists versions of, or another mode"
		}
		return "a newer image is published (or another mode or tag filter picks one)"
	case gateSelfUpdate:
		return "wait for the retry time, or publish a newer image"
	case gateSoak:
		at := "the time above"
		if e.EligibleAt != nil {
			at = e.EligibleAt.In(ev.location()).Format(timeLayout)
		}
		return fmt.Sprintf("wait until %s, or lower min_image_age of policy %q (or label %s)", at, r.PolicyID, policy.LabelMinImageAge)
	case gateMonitor:
		return "remove " + policy.LabelMonitorOnly + ", or use a policy without monitor_only"
	case gateApproval:
		return "the approval endpoint approves it"
	}
	return ""
}

func (ev *evaluation) policyText(r resolved) string {
	var why string
	switch {
	case r.source == "label" && r.Builtin:
		why = fmt.Sprintf("label %s=%s, a built-in policy", policy.LabelPolicy, r.label)
	case r.source == "label":
		why = fmt.Sprintf("label %s=%s", policy.LabelPolicy, r.label)
	default:
		why = "no " + policy.LabelPolicy + " label, so the default policy"
	}
	parts := []string{r.PolicyID + " (" + why + ")", "mode " + r.Mode}
	parts = append(parts, "min_image_age "+shortDuration(r.MinImageAge), "health_timeout "+shortDuration(r.HealthTimeout))
	if len(r.Overrides) > 0 {
		parts = append(parts, "overridden by "+strings.Join(r.Overrides, ", "))
	}
	if r.Scheduled {
		parts = append(parts, "updated only by its own schedule")
	}
	return strings.Join(parts, ", ")
}

func modeText(mode string) string {
	switch mode {
	case config.ModeTagFollow, config.ModeDigest:
		return mode + ": follows the image its tag points to"
	case config.ModePatch:
		return "patch: the newest tag with the same major and minor version"
	case config.ModeMinor:
		return "minor: the newest tag with the same major version"
	case config.ModeMajor:
		return "major: the newest version tag"
	case config.ModeNone:
		return "none: never checked"
	}
	return mode
}

func (ev *evaluation) approvalText() string {
	a := ev.cfg.Update.Approval
	if !approvalConfigured(ev.cfg) {
		return "no approval endpoint (update.approval)"
	}
	host := "the configured endpoint"
	if u, err := url.Parse(a.URL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	detail := "asks the approval endpoint at " + host
	if a.Timeout > 0 {
		detail += ", waiting up to " + shortDuration(a.Timeout.Std())
	}
	if a.FailOpen {
		detail += "; proceeds when it does not answer (fail_open)"
	}
	return detail
}

func (ev *evaluation) updateText(r resolved) string {
	parts := []string{}
	hooks := len(ev.cfg.Hooks.PreUpdate) + len(r.Hooks.PreUpdate)
	if r.Labels.PreUpdate != "" && ev.cfg.Hooks.LabelHooks {
		hooks++
	}
	if hooks > 0 && !ev.cfg.Hooks.Disabled {
		parts = append(parts, fmt.Sprintf("%d pre_update hook%s (one can skip the update)", hooks, plural(hooks)))
	}
	if r.HealthTimeout > 0 {
		parts = append(parts, "healthy within "+shortDuration(r.HealthTimeout))
	} else {
		parts = append(parts, "no health gate")
	}
	if ev.cfg.Update.Rollback() {
		parts = append(parts, "rolled back on failure")
	} else {
		parts = append(parts, "no rollback (rollback_on_failure: false)")
	}
	return strings.Join(parts, ", ")
}

func imageText(img planImage, sameTag string) string {
	var parts []string
	if img.Tag != "" && img.Tag != sameTag {
		parts = append(parts, img.Tag)
	}
	if img.Version != "" && img.Version != img.Tag {
		parts = append(parts, "("+img.Version+")")
	}
	if img.Digest != "" {
		parts = append(parts, shortDigest(img.Digest))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func changeText(e planEntry) string {
	cur := imageText(e.Current, "")
	if e.Candidate == nil {
		return cur
	}
	return cur + " -> " + imageText(*e.Candidate, e.Current.Tag)
}

const timeLayout = "Mon 2006-01-02 15:04 MST"

func (ev *evaluation) location() *time.Location {
	if ev.cfg.Timezone != "" {
		if loc, err := time.LoadLocation(ev.cfg.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func (ev *evaluation) whenText(e planEntry) string {
	loc := ev.location()
	switch {
	case e.NextRun != nil:
		return e.NextRun.In(loc).Format(timeLayout)
	case e.Action != actionUpdate && e.Action != actionReport && e.Action != actionWait:
		return "-"
	case e.EligibleAt != nil && e.EligibleAt.After(ev.now):
		return "after " + e.EligibleAt.In(loc).Format(timeLayout)
	case e.NextWindow != nil:
		return "window opens " + e.NextWindow.In(loc).Format(timeLayout)
	case e.Scheduled:
		return "next run of policy " + e.Policy
	}
	return "next run (" + scheduleSummary(ev.cfg.Schedule) + ")"
}

func policyLabelText(e planEntry) string {
	id := e.Policy
	if id == "" {
		id = "-"
	}
	var why []string
	if e.PolicySource == "label" {
		why = append(why, "label")
	}
	if len(e.Overrides) > 0 {
		why = append(why, "overridden")
	}
	if len(why) == 0 {
		return id
	}
	return id + " (" + strings.Join(why, ", ") + ")"
}
