package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	dlog "github.com/nicolaeser/dockpaum/internal/log"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

const planTrigger = "plan"

type evaluation struct {
	cfg       *config.Config
	resolver  *policy.Resolver
	instance  string
	now       time.Time
	scoped    bool
	requested []string
	listed    []hostContainers
	statuses  []updater.ContainerStatus
	versions  map[string]map[string]string
	errs      []string
}

type hostContainers struct {
	host       string
	containers []docker.Container
}

type errEvaluation struct{ code int }

func (e errEvaluation) Error() string { return fmt.Sprintf("exit %d", e.code) }

func evaluate(inv *invocation, stderr io.Writer, now time.Time) (*evaluation, error) {
	cfg, err := config.Validate(inv.configPath)
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return nil, errEvaluation{exitUsage}
	}
	resolver, err := policy.NewResolver(cfg, nil)
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return nil, errEvaluation{exitUsage}
	}
	opts := updater.RunOptions{Trigger: planTrigger, Containers: inv.plan.containers, Policy: updater.NormalizePolicy(inv.plan.policy)}
	if inv.command == cmdExplain {
		opts.Containers = []string{inv.plan.target}
	}
	if opts.Policy != "" {
		if err := checkPolicyID(resolver, opts.Policy); err != nil {
			printf(stderr, "%s: %v\n", inv.command, err)
			return nil, errEvaluation{exitUsage}
		}
	}
	ev := &evaluation{
		cfg: cfg, resolver: resolver, instance: instanceName(cfg), now: now,
		scoped: len(opts.Containers) > 0 || opts.Policy != "", requested: opts.Containers,
		versions: map[string]map[string]string{},
	}

	logger := dlog.NewTo(stderr, config.LogConfig{Level: "warn", Format: "text"})
	if err := loadDockerConfig(cfg, logger); err != nil {
		printf(stderr, "config: docker_config: %v\n", err)
		return nil, errEvaluation{exitUsage}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	conn := newConnectLog(logger.Handler())
	clients, err := docker.OpenAll(ctx, cfg.Hosts, slog.New(conn))
	if err != nil {
		for _, line := range conn.failures(cfg.Hosts, err) {
			printf(stderr, "%s: docker: %s\n", inv.command, line)
		}
		if isPermissionDenied(err) {
			for _, path := range unixSockets(cfg.Hosts, os.Getenv("DOCKER_HOST")) {
				printf(stderr, "%s\n", inspectSocket(path).hint())
			}
		}
		return nil, errEvaluation{exitFailure}
	}
	defer clients.Close()
	for _, h := range clients.Unreachable() {
		ev.errs = append(ev.errs, fmt.Sprintf("host %s: unreachable (%s), its containers are not in the plan", h, conn.failure(h)))
	}
	for _, c := range clients.List() {
		list, err := c.ListContainers(ctx, true)
		if err != nil {
			ev.errs = append(ev.errs, fmt.Sprintf("host %s: list containers: %v", c.Name, err))
			continue
		}
		ev.listed = append(ev.listed, hostContainers{host: c.Name, containers: list})
	}
	if blockers := planBlockers(ev.listed, cfg.Filters); len(blockers) > 0 {
		for _, b := range blockers {
			printf(stderr, "%s: %s\n", inv.command, b)
		}
		printf(stderr, "%s: a run would first have to change these; %s changes nothing, so it stops here. "+
			"The daemon's next run takes care of them.\n", inv.command, inv.command)
		return nil, errEvaluation{exitFailure}
	}
	if err := ev.checkRequested(clients.Unreachable()); err != nil {
		printf(stderr, "%s: %v\n", inv.command, err)
		var ue errUsage
		if errors.As(err, &ue) {
			return nil, errEvaluation{exitUsage}
		}
		return nil, errEvaluation{exitFailure}
	}

	st, cleanup := planState(cfg.State, logger)
	defer cleanup()
	run := *cfg
	run.DryRun = true
	off := false
	quiet, err := audit.New(config.AuditConfig{Enabled: &off}, logger)
	if err != nil {
		printf(stderr, "%s: %v\n", inv.command, err)
		return nil, errEvaluation{exitFailure}
	}
	upd, err := updater.New(updater.Deps{
		Clients: clients, Notifier: notify.Build(nil, logger), Metrics: metrics.NewRegistry(),
		Logger: logger, Config: &run, Audit: quiet, State: st, Instance: cfg.Instance,
	})
	if err != nil {
		printf(stderr, "%s: %v\n", inv.command, err)
		return nil, errEvaluation{exitFailure}
	}
	stopDrain := context.AfterFunc(ctx, upd.Drain)
	defer stopDrain()

	runs := []updater.RunOptions{opts}
	if !ev.scoped {
		for _, ps := range upd.PolicySchedules() {
			runs = append(runs, updater.RunOptions{Trigger: planTrigger, Policy: ps.PolicyID})
		}
	}
	ids := map[string]bool{}
	for _, o := range runs {
		_ = upd.RunWith(ctx, o)
		if last := upd.LastRun(); last != nil {
			ids[last.ID] = true
		}
		if ctx.Err() != nil {
			ev.errs = append(ev.errs, "interrupted: the plan is incomplete")
			break
		}
	}
	for _, s := range upd.Containers() {
		if ids[s.RunID] {
			ev.statuses = append(ev.statuses, s)
		}
	}
	ev.readVersions(ctx, clients)
	return ev, nil
}

type connectLog struct {
	next slog.Handler
	mu   *sync.Mutex
	errs map[string]string
}

func newConnectLog(next slog.Handler) *connectLog {
	return &connectLog{next: next, mu: &sync.Mutex{}, errs: map[string]string{}}
}

func (h *connectLog) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.next.Enabled(ctx, level)
}

func (h *connectLog) Handle(ctx context.Context, r slog.Record) error {
	var host, msg string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "host":
			host = a.Value.String()
		case "err":
			msg = a.Value.String()
		}
		return true
	})
	if host != "" && msg != "" && r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.errs[host] = msg
		h.mu.Unlock()
		return nil
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *connectLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &connectLog{next: h.next.WithAttrs(attrs), mu: h.mu, errs: h.errs}
}

func (h *connectLog) WithGroup(name string) slog.Handler {
	return &connectLog{next: h.next.WithGroup(name), mu: h.mu, errs: h.errs}
}

func (h *connectLog) failure(host string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if msg := h.errs[host]; msg != "" {
		return msg
	}
	return "no connection"
}

func (h *connectLog) failures(hosts []config.HostConfig, err error) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, hc := range hosts {
		if msg := h.errs[hc.Name]; msg != "" {
			out = append(out, "host "+hc.Name+": "+msg)
		}
	}
	if len(out) == 0 {
		out = []string{err.Error()}
	}
	return out
}

func checkPolicyID(r *policy.Resolver, id string) error {
	if config.IsReservedPolicyID(id) {
		return usagef("--policy %s: containers with policy %s are never checked; dockpaum explain <container> shows why one is", id, id)
	}
	ids := slices.DeleteFunc(r.IDs(), config.IsReservedPolicyID)
	if slices.Contains(ids, id) {
		return nil
	}
	msg := fmt.Sprintf("--policy %q: unknown policy", id)
	if s := config.Suggest(id, ids); s != "" {
		msg += fmt.Sprintf(" — did you mean %q?", s)
	}
	return usagef("%s (known: %s)", msg, strings.Join(ids, ", "))
}

func (ev *evaluation) checkRequested(unreachable []string) error {
	var names []string
	have := map[string]bool{}
	for _, h := range ev.listed {
		for _, ct := range h.containers {
			have[ct.Name] = true
			names = append(names, ct.Name)
		}
	}
	for _, want := range ev.requested {
		if have[want] {
			continue
		}
		if len(unreachable) > 0 {
			return fmt.Errorf("no container named %q on the reachable hosts (unreachable: %s)", want, strings.Join(unreachable, ", "))
		}
		msg := fmt.Sprintf("no container named %q on %s", want, hostList(ev.listed))
		if s := config.Suggest(want, names); s != "" {
			msg += fmt.Sprintf(" — did you mean %q?", s)
		}
		return usagef("%s", msg)
	}
	return nil
}

func hostList(hosts []hostContainers) string {
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.host)
	}
	if len(names) == 1 {
		return "host " + names[0]
	}
	return "hosts " + strings.Join(names, ", ")
}

func loadDockerConfig(cfg *config.Config, logger *slog.Logger) error {
	if cfg.DockerConfig == "" {
		return nil
	}
	auths, warnings, err := config.ReadDockerConfig(cfg.DockerConfig)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logger.Warn("docker_config", "path", cfg.DockerConfig, "warning", w)
	}
	cfg.Registry = append(cfg.Registry, auths...)
	logger.Info("loaded registry credentials from docker config", "path", cfg.DockerConfig, "entries", len(auths))
	return nil
}

var leftoverRE = regexp.MustCompile(`^(.+)\.(dockpaum-old|self-update-old)-\d+$`)

func planBlockers(listed []hostContainers, f config.FilterConfig) []string {
	var out []string
	for _, h := range listed {
		names := map[string]bool{}
		helperRunning := false
		for _, ct := range h.containers {
			names[ct.Name] = true
			if _, ok := ct.Labels[selfupdate.HelperLabel]; ok && ct.Labels[policy.LabelScope] == f.Scope && activeState(ct.State) {
				helperRunning = true
			}
		}
		for _, ct := range h.containers {
			if m := leftoverRE.FindStringSubmatch(ct.Name); m != nil {
				if names[m[1]] || (m[2] == "self-update-old" && helperRunning) {
					continue
				}
				if policy.ReadLabels(ct.Labels, f.HonorWatchtowerLabels()).Scope != f.Scope {
					continue
				}
				out = append(out, fmt.Sprintf("host %s: %s is left from an interrupted update and the name %s is free; "+
					"a run renames it back and starts it (or do it yourself: docker rename %s %s)", h.host, ct.Name, m[1], ct.Name, m[1]))
				continue
			}
			_, helper := ct.Labels[selfupdate.HelperLabel]
			_, check := ct.Labels[selfupdate.CheckLabel]
			if (helper || check) && ct.Labels[policy.LabelScope] == f.Scope && !activeState(ct.State) {
				what := "the finished self-update helper"
				if check {
					what = "the finished self-update configuration check"
				}
				out = append(out, fmt.Sprintf("host %s: %s %s is waiting for the daemon to record and remove it", h.host, what, ct.Name))
			}
		}
	}
	return out
}

func activeState(s string) bool {
	switch s {
	case "running", "restarting", "paused", "removing":
		return true
	}
	return false
}

func planState(sc config.StateConfig, logger *slog.Logger) (*state.Store, func()) {
	memory := func() (*state.Store, func()) {
		st := state.Open("", logger)
		return st, func() { _ = st.Close() }
	}
	src := statePath(sc)
	if src == "" {
		return memory()
	}
	dir, err := os.MkdirTemp("", "dockpaum-plan-*")
	if err != nil {
		logger.Warn("plan: no temporary directory for a copy of the state; evaluating without it", "err", err)
		return memory()
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// #nosec G703 -- the state file the administrator's configuration names.
	data, err := os.ReadFile(src)
	switch {
	case err == nil:
		// #nosec G703 -- a file in the private directory MkdirTemp just made.
		if err := os.WriteFile(state.PathIn(dir), data, 0o600); err != nil {
			logger.Warn("plan: cannot copy the state; evaluating without it", "err", err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		logger.Warn("plan: cannot read the state, so first sightings and earlier findings are not used", "path", src, "err", err)
	}
	st := state.OpenWith(state.PathIn(dir), state.Options{Logger: logger, FlushDelay: -1})
	return st, func() {
		_ = st.Close()
		cleanup()
	}
}

func (ev *evaluation) readVersions(ctx context.Context, clients *docker.Clients) {
	byHost := map[string]*docker.Client{}
	for _, c := range clients.List() {
		byHost[c.Name] = c
	}
	for _, s := range ev.statuses {
		c := byHost[s.Host]
		if c == nil {
			continue
		}
		ids := []string{s.Current.Digest}
		if s.Candidate != nil {
			ids = append(ids, s.Candidate.Digest)
		}
		for _, id := range ids {
			if id == "" || ev.versions[s.Host][id] != "" {
				continue
			}
			defs, err := c.ImageDefaults(ctx, id)
			if err != nil || defs == nil {
				continue
			}
			if v := strings.TrimSpace(defs.Labels[ociVersionLabel]); v != "" {
				if ev.versions[s.Host] == nil {
					ev.versions[s.Host] = map[string]string{}
				}
				ev.versions[s.Host][id] = v
			}
		}
	}
}

const ociVersionLabel = "org.opencontainers.image.version"

type recorded struct {
	currentVersion, candidateVersion string
	checkNote                        string
}

func recordedOf(s updater.ContainerStatus) recorded {
	b, err := json.Marshal(s)
	if err != nil {
		return recorded{}
	}
	return decodeRecorded(b)
}

func decodeRecorded(b []byte) recorded {
	var doc struct {
		Current struct {
			Version string `json:"version"`
		} `json:"current"`
		Candidate *struct {
			Version string `json:"version"`
		} `json:"candidate"`
		CheckNote string `json:"check_note"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return recorded{}
	}
	out := recorded{currentVersion: strings.TrimSpace(doc.Current.Version), checkNote: strings.TrimSpace(doc.CheckNote)}
	if doc.Candidate != nil {
		out.candidateVersion = strings.TrimSpace(doc.Candidate.Version)
	}
	return out
}

func (ev *evaluation) container(host, name string) (docker.Container, bool) {
	for _, h := range ev.listed {
		if h.host != host {
			continue
		}
		for _, ct := range h.containers {
			if ct.Name == name {
				return ct, true
			}
		}
	}
	return docker.Container{}, false
}

func (ev *evaluation) unmanaged() []unmanagedEntry {
	processed := map[[2]string]bool{}
	for _, s := range ev.statuses {
		processed[[2]string{s.Host, s.Container}] = true
	}
	out := []unmanagedEntry{}
	for _, h := range ev.listed {
		for _, ct := range h.containers {
			if processed[[2]string{h.host, ct.Name}] || leftoverRE.MatchString(ct.Name) {
				continue
			}
			if ev.scoped && !slices.Contains(ev.requested, ct.Name) {
				continue
			}
			reason := filterVerdict(ct, &ev.cfg.Filters)
			if reason == "" {
				reason = "not evaluated; the warnings above may say why"
			}
			out = append(out, unmanagedEntry{Host: h.host, Container: ct.Name, Reason: reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Container < out[j].Container
	})
	return out
}

func filterVerdict(ct docker.Container, f *config.FilterConfig) string {
	if !f.IncludeStopped && ct.State != "running" {
		return fmt.Sprintf("not running (%s) and filters.include_stopped is off", ct.State)
	}
	if leftoverRE.MatchString(ct.Name) {
		return "temporary name of an interrupted update; never updated itself"
	}
	cl := policy.ReadLabels(ct.Labels, f.HonorWatchtowerLabels())
	enableLabel := policy.LabelEnable
	if _, ok := ct.Labels[enableLabel]; !ok && f.HonorWatchtowerLabels() {
		if _, ok := ct.Labels[policy.WatchtowerPrefix+"enable"]; ok {
			enableLabel = policy.WatchtowerPrefix + "enable"
		}
	}
	switch {
	case f.LabelEnable && (cl.Enable == nil || !*cl.Enable):
		return "filters.label_enable is on and the container is not labelled " + policy.LabelEnable + "=true"
	case cl.Enable != nil && !*cl.Enable:
		return "labelled " + enableLabel + "=false"
	case f.Scope != "" && cl.Scope != f.Scope:
		if cl.Scope == "" {
			return fmt.Sprintf("no %s label, and this instance manages scope %q only", policy.LabelScope, f.Scope)
		}
		return fmt.Sprintf("labelled %s=%s, and this instance manages scope %q", policy.LabelScope, cl.Scope, f.Scope)
	case f.Scope == "" && cl.Scope != "":
		return fmt.Sprintf("labelled %s=%s: the instance with that filters.scope manages it", policy.LabelScope, cl.Scope)
	case len(f.IncludeNames) > 0 && !slices.Contains(f.IncludeNames, ct.Name):
		return "not in filters.include_names"
	case slices.Contains(f.ExcludeNames, ct.Name):
		return "in filters.exclude_names"
	case f.IncludeRegexp() != nil && !f.IncludeRegexp().MatchString(ct.Name):
		return "does not match filters.include_regex"
	case f.ExcludeRegexp() != nil && f.ExcludeRegexp().MatchString(ct.Name):
		return "matches filters.exclude_regex"
	}
	return ""
}
