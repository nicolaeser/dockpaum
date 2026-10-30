package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/lifecycle"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/pulltoken"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/window"
)

type Deps struct {
	Clients  *docker.Clients
	Notifier *notify.Multi
	Metrics  *metrics.Registry
	Logger   *slog.Logger
	Config   *config.Config
	Audit    *audit.Logger
	State    *state.Store
	Instance string
}

type Updater struct {
	deps         Deps
	hooks        *lifecycle.Runner
	reg          registryAPI
	resolver     *policy.Resolver
	approval     *approvalGate
	backoff      registryBackoff
	direct       directAccess
	ageSources   map[string]string
	incompleteMu sync.Mutex
	incomplete   map[statusKey]string
	fallbackMu   sync.Mutex
	fallback     map[statusKey]string

	selfMu      sync.Mutex
	selfID      string
	selfChecked bool
	selfUnknown bool
	selfCands   []selfupdate.Candidate

	draining atomic.Bool

	lastMu  sync.Mutex
	last    *RunSummary
	nextRun func() time.Time
	running atomic.Int32
	active  activeRuns

	state      *state.Store
	instance   string
	candidates candidateCache
	send       func(context.Context, notify.Event)

	statusMu         sync.RWMutex
	statuses         map[statusKey]ContainerStatus
	ageSourceWarning sync.Once

	healthGraceCap time.Duration
	healthPoll     time.Duration
	stopGrace      time.Duration
	maxStopWait    time.Duration
	leftoverSettle time.Duration
	now            func() time.Time
	windowNow      func() time.Time
	nextOpenMu     sync.Mutex
	nextOpen       map[*window.Set]windowOpening

	tokens  *pulltoken.Store
	keyring *registry.Keyring
	central []registry.Credential
}

func New(d Deps) (*Updater, error) {
	r, err := policy.NewResolver(d.Config, d.Logger)
	if err != nil {
		return nil, err
	}
	reg := registry.New(d.Config.Registry)
	if d.Metrics != nil {
		reg.AttachMetrics(d.Metrics.ForRegistry())
		d.Audit.AttachFailureSink(d.Metrics.AuditWriteFailures)
		d.Notifier.AttachFailureSink(notifyAdapter{d.Metrics.NotifierFailures})
	}
	st := d.State
	if st == nil {
		st = state.Open("", d.Logger)
	}
	hosts := make([]string, 0, len(d.Config.Hosts))
	for _, h := range d.Config.Hosts {
		hosts = append(hosts, h.Name)
	}
	st.RetainHosts(hosts)
	if d.State != nil && d.Notifier != nil {
		d.Notifier.AttachLedger(NoticeLedger(d.State))
	}
	instance := strings.TrimSpace(d.Instance)
	if instance == "" {
		instance = DefaultInstance()
	}
	u := &Updater{
		deps:           d,
		hooks:          lifecycle.NewRunner(d.Config.Hooks, d.Logger),
		reg:            reg,
		resolver:       r,
		healthGraceCap: healthGraceCap,
		healthPoll:     healthPollInterval,
		stopGrace:      stopGrace,
		maxStopWait:    maxStopWait,
		leftoverSettle: leftoverSettle,
		now:            time.Now,
		state:          st,
		instance:       instance,
		statuses:       map[statusKey]ContainerStatus{},
		send:           d.Notifier.Notify,
		ageSources:     policyAgeSources(d.Config),
		incomplete:     map[statusKey]string{},
		fallback:       map[statusKey]string{},
		nextOpen:       map[*window.Set]windowOpening{},
	}
	u.approval = newApprovalGate(d.Config.Update.Approval, d.Logger, func() time.Time { return u.now() })
	u.loadStatuses()
	inContainer := selfupdate.InContainer()
	u.tokens = pulltoken.New(pulltoken.Options{Logger: d.Logger, Instance: instance, InContainer: inContainer, Local: localHosts(d.Config.Hosts)})
	u.keyring = registry.NewKeyring(d.Logger)
	u.central = registry.CentralCredentials(d.Config.Registry)
	if inContainer {
		u.selfCands = selfupdate.Candidates()
	} else {
		u.selfChecked = true
	}
	return u, nil
}

func (u *Updater) Drain() { u.draining.Store(true) }

func (u *Updater) Draining() bool { return u.draining.Load() }

func (u *Updater) identifySelf(ctx context.Context, clients []*docker.Client) {
	u.selfMu.Lock()
	defer u.selfMu.Unlock()
	if u.selfChecked {
		return
	}
	if id, ok := selfupdate.Identify(ctx, clients, u.selfCands); ok {
		u.selfID, u.selfChecked, u.selfUnknown = id.ID, true, false
		u.deps.Logger.Info("self-update: identified own container", "id", id.ID, "host", id.Host, "via", id.Source)
		return
	}
	u.selfUnknown = true
	if len(u.deps.Clients.Unreachable()) > 0 || len(clients) == 0 {
		return
	}
	u.selfChecked = true
	ids := make([]string, 0, len(u.selfCands))
	for _, c := range u.selfCands {
		ids = append(ids, c.Source+":"+c.ID)
	}
	u.deps.Logger.Warn("self-update: dockpaum runs in a container but could not find it on the configured Docker hosts; "+
		"self-update is unavailable, and containers running the dockpaum image are not recreated",
		"candidates", ids)
}

type notifyAdapter struct {
	vec *prometheus.CounterVec
}

func (a notifyAdapter) WithLabelValues(lvs ...string) interface{ Inc() } {
	return a.vec.WithLabelValues(lvs...)
}

type RunOptions struct {
	Policy     string
	Containers []string
	Images     []string
	Trigger    string
	DryRun     bool
}

func (o RunOptions) regular() bool {
	return o.Policy == "" && len(o.Containers) == 0 && len(o.Images) == 0
}

func (u *Updater) Run(ctx context.Context) error {
	return u.RunWith(ctx, RunOptions{Trigger: "schedule"})
}

func (u *Updater) RunPolicy(ctx context.Context, policyID string) error {
	return u.RunWith(ctx, RunOptions{Policy: policyID, Trigger: "policy-schedule"})
}

func (u *Updater) PolicySchedules() []policy.Schedule { return u.resolver.Schedules() }

func NormalizePolicy(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return ""
	}
	return id
}

func inScope(eff policy.Resolved, name, image string, opts RunOptions) bool {
	if len(opts.Containers) > 0 && !slices.Contains(opts.Containers, name) {
		return false
	}
	if len(opts.Images) > 0 && !registry.SelectsAnyImage(opts.Images, image) {
		return false
	}
	if opts.Policy == "" {
		return len(opts.Containers) > 0 || len(opts.Images) > 0 || !eff.Scheduled
	}
	return !eff.Skip && eff.PolicyID == opts.Policy
}

type RunSummary struct {
	ID         string    `json:"id"`
	Trigger    string    `json:"trigger"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Policy     string    `json:"policy,omitempty"`
	Containers []string  `json:"containers,omitempty"`
	Images     []string  `json:"images,omitempty"`
	Scanned    int       `json:"scanned"`
	Updated    int       `json:"updated"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	Error      string    `json:"error,omitempty"`
}

func (o RunOptions) Key() string {
	names := slices.Clone(o.Containers)
	slices.Sort(names)
	key := "policy=" + o.Policy + ";containers=" + strings.Join(names, ",")
	if len(o.Images) > 0 {
		images := slices.Clone(o.Images)
		slices.Sort(images)
		key += ";images=" + strings.Join(images, ",")
	}
	if o.DryRun {
		key += ";dry_run"
	}
	return key
}

type HostStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

type Status struct {
	Version         string       `json:"version"`
	Hosts           []HostStatus `json:"hosts"`
	LastRun         *RunSummary  `json:"last_run"`
	Instance        string       `json:"instance"`
	DryRun          bool         `json:"dry_run"`
	Running         bool         `json:"running"`
	FirstRunPending bool         `json:"first_run_pending"`
	NextRunAt       *time.Time   `json:"next_run_at,omitempty"`
	PendingUpdates  int          `json:"pending_updates"`
}

func (u *Updater) Status(version string) Status {
	connected := map[string]bool{}
	for _, c := range u.deps.Clients.List() {
		connected[c.Name] = true
	}
	last := u.LastRun()
	st := Status{
		Version: version, LastRun: last, Instance: u.instance,
		DryRun: u.deps.Config.DryRun, Running: u.running.Load() > 0, FirstRunPending: last == nil,
		PendingUpdates: u.pendingUpdates(),
	}
	u.lastMu.Lock()
	next := u.nextRun
	u.lastMu.Unlock()
	if next != nil {
		if t := next(); !t.IsZero() {
			st.NextRunAt = &t
		}
	}
	for _, h := range u.deps.Config.Hosts {
		st.Hosts = append(st.Hosts, HostStatus{Name: h.Name, Connected: connected[h.Name]})
	}
	return st
}

func (u *Updater) SetNextRun(next func() time.Time) {
	u.lastMu.Lock()
	defer u.lastMu.Unlock()
	u.nextRun = next
}

func (u *Updater) Instance() string { return u.instance }

func (u *Updater) notify(ctx context.Context, runID string, ev notify.Event) {
	ev.RunID = runID
	if ev.Instance == "" {
		ev.Instance = u.instance
	}
	u.send(ctx, ev)
}

func (u *Updater) LastRun() *RunSummary {
	u.lastMu.Lock()
	defer u.lastMu.Unlock()
	if u.last == nil {
		return nil
	}
	cp := *u.last
	return &cp
}

func runError(errs []error, failedUpdates int) error {
	if failedUpdates > 0 {
		errs = append(errs, fmt.Errorf("%d container update(s) failed", failedUpdates))
	}
	return errors.Join(errs...)
}

func (u *Updater) RunWith(ctx context.Context, opts RunOptions) error {
	start := time.Now()
	runID := newRunID()
	u.running.Add(1)
	defer u.running.Add(-1)
	defer u.active.begin(runID, start)()
	ctx = withDryRun(ctx, opts.DryRun)
	u.deps.Metrics.RunsTotal.Inc()

	u.deps.Clients.Reconnect(ctx)
	clients := u.deps.Clients.List()
	u.identifySelf(ctx, clients)
	unreachable := u.deps.Clients.Unreachable()
	u.deps.Metrics.HostsUnreachable.Set(float64(len(unreachable)))
	hostList := make([]string, 0, len(clients))
	for _, c := range clients {
		hostList = append(hostList, c.Name)
	}
	hosts := strings.Join(hostList, ", ")
	u.notify(ctx, runID, notify.Event{Type: notify.EventRunStarted, Host: hosts, Policy: opts.Policy})

	var stats notify.RunStats
	var errs []error
	if len(unreachable) > 0 {
		errs = append(errs, fmt.Errorf("docker host(s) unreachable: %s", strings.Join(unreachable, ", ")))
	}
	if err := u.runStage(ctx, lifecycle.PhasePreRun, lifecycle.RunInfo{RunID: runID, Policy: opts.Policy}); err != nil {
		errs = append(errs, fmt.Errorf("pre-run hook: %w", err))
		u.deps.Logger.Error("pre-run hook failed, no container is updated in this run", "run_id", runID, "err", err)
	} else {
		var selfUpdates []*selfUpdatePlan
		drained := 0
		for _, c := range clients {
			hs, err := u.runHost(ctx, runID, c, opts)
			stats.Scanned += hs.Scanned
			stats.Updated += hs.Updated
			stats.Failed += hs.Failed
			stats.Skipped += hs.Skipped
			stats.Items = append(stats.Items, hs.Items...)
			selfUpdates = append(selfUpdates, hs.selfUpdates...)
			drained += hs.Drained
			if err != nil {
				errs = append(errs, fmt.Errorf("host %s: %w", c.Name, err))
			}
		}
		for _, p := range selfUpdates {
			if u.Draining() {
				drained++
				u.dropSelfPlan(ctx, p)
				u.deps.Logger.Info("shutting down: self-update left for the next run", "run_id", runID, "container", p.ct.Name)
				continue
			}
			t := u.applySelfUpdate(ctx, runID, p)
			stats.Updated += t.updated
			stats.Failed += t.failed
			stats.Items = append(stats.Items, t.items...)
		}
		if drained > 0 {
			errs = append(errs, fmt.Errorf("shutting down: %d container(s) left for the next run", drained))
		}
		info := lifecycle.RunInfo{RunID: runID, Policy: opts.Policy, Scanned: stats.Scanned, Updated: stats.Updated, Failed: stats.Failed}
		if err := u.runStage(ctx, lifecycle.PhasePostRun, info); err != nil {
			u.deps.Logger.Warn("post-run hook failed", "run_id", runID, "err", err)
		}
	}
	if !opts.regular() && stats.Scanned == 0 {
		u.deps.Logger.Warn("the requested run matched no container — check the container names, images or policy id",
			"run_id", runID, "containers", opts.Containers, "images", opts.Images, "policy", opts.Policy)
	}
	runErr := runError(errs, stats.Failed)

	stats.Took = time.Since(start)
	u.deps.Metrics.RunDuration.Observe(stats.Took.Seconds())
	u.deps.Metrics.LastRunTimestamp.SetToCurrentTime()
	regular := opts.regular()
	if regular {
		u.deps.Metrics.ContainersScanned.Set(float64(stats.Scanned))
	}
	if runErr != nil {
		u.deps.Metrics.RunFailures.Inc()
	} else if regular {
		u.deps.Metrics.LastSuccessTimestamp.SetToCurrentTime()
	}
	summary := &RunSummary{
		ID: runID, Trigger: opts.Trigger, Started: start, Finished: time.Now(),
		Policy: opts.Policy, Containers: opts.Containers, Images: opts.Images,
		Scanned: stats.Scanned, Updated: stats.Updated, Failed: stats.Failed, Skipped: stats.Skipped,
	}
	if runErr != nil {
		summary.Error = runErr.Error()
	}
	u.lastMu.Lock()
	u.last = summary
	u.lastMu.Unlock()
	_ = u.state.Flush()

	u.deps.Logger.Info("run completed",
		"run_id", runID, "trigger", opts.Trigger, "policy", opts.Policy, "images", opts.Images,
		"scanned", stats.Scanned, "updated", stats.Updated,
		"failed", stats.Failed, "skipped", stats.Skipped,
		"took", stats.Took, "err", runErr)
	u.notify(ctx, runID, notify.Event{
		Type:     notify.EventRunCompleted,
		Host:     hosts,
		Policy:   opts.Policy,
		Err:      runErr,
		RunStats: &stats,
	})
	return runErr
}

func (u *Updater) runStage(ctx context.Context, phase lifecycle.Phase, info lifecycle.RunInfo) error {
	if u.dryRun(ctx) {
		if (phase == lifecycle.PhasePreRun && len(u.deps.Config.Hooks.PreRun) > 0) ||
			(phase == lifecycle.PhasePostRun && len(u.deps.Config.Hooks.PostRun) > 0) {
			u.deps.Logger.Info("dry-run: run hooks skipped", "phase", string(phase), "run_id", info.RunID)
		}
		return nil
	}
	return u.hooks.RunStage(ctx, phase, info)
}

func (u *Updater) RunJob(ctx context.Context, name string) error {
	var job *config.JobConfig
	for i := range u.deps.Config.Jobs {
		if u.deps.Config.Jobs[i].Name == name {
			job = &u.deps.Config.Jobs[i]
			break
		}
	}
	if job == nil {
		return fmt.Errorf("job %q is not configured", name)
	}
	logger := u.deps.Logger.With("job", name)
	if u.deps.Config.DryRun {
		u.deps.Metrics.JobRuns.WithLabelValues(name, "dry_run").Inc()
		logger.Info("dry-run: scheduled job skipped")
		return nil
	}
	start := time.Now()
	err := u.hooks.RunJob(ctx, *job, u.jobClient(job.Host))
	if err != nil {
		u.deps.Metrics.JobRuns.WithLabelValues(name, "error").Inc()
		logger.Error("scheduled job failed", "took", time.Since(start), "err", err)
		return err
	}
	u.deps.Metrics.JobRuns.WithLabelValues(name, "ok").Inc()
	logger.Info("scheduled job completed", "took", time.Since(start))
	return nil
}

func (u *Updater) jobClient(host string) *docker.Client {
	clients := u.deps.Clients.List()
	for _, c := range clients {
		if host == "" || c.Name == host {
			return c
		}
	}
	return nil
}

type hostStats struct {
	Scanned, Updated, Failed, Skipped int
	Items                             []notify.RunItem
	Drained                           int
	selfUpdates                       []*selfUpdatePlan
}

func (u *Updater) runHost(ctx context.Context, runID string, c *docker.Client, opts RunOptions) (hostStats, error) {
	logger := u.deps.Logger.With("host", c.Name, "run_id", runID)
	observing := u.observing() || u.dryRun(ctx)
	collect := selfupdate.Collect
	if observing {
		collect = selfupdate.Peek
	}
	outcomes, helpers, err := collect(ctx, c, u.deps.Config.Filters.Scope, logger)
	if err != nil {
		logger.Warn("self-update: looking for finished helpers failed", "err", err)
	}
	all, err := c.ListContainers(ctx, true)
	if err != nil {
		logger.Error("list containers failed", "err", err)
		return hostStats{}, err
	}
	u.state.PruneHost(c.Name, liveOn(all))
	u.retainLabelNotes(c.Name, all)
	t := &tally{}
	own := map[string]bool{}
	for _, o := range outcomes {
		if observing {
			logger.Info("dry-run: a finished self-update helper is left for a run that is not a dry-run to record",
				"helper", o.Helper, "container", o.Container, "exit_code", o.ExitCode)
		} else {
			u.recordSelfUpdateOutcome(ctx, runID, c, t, all, o)
		}
		if o.OldID != "" {
			own[o.OldID] = true
		}
	}
	u.recoverLeftovers(ctx, runID, c, all, helpers > 0, own, observing, logger)

	containers := all
	if !u.deps.Config.Filters.IncludeStopped {
		containers = containers[:0:0]
		for _, ct := range all {
			if ct.State == "running" {
				containers = append(containers, ct)
			}
		}
	}
	regular := opts.regular()
	if regular {
		u.deps.Metrics.ImageAgeSeconds.DeletePartialMatch(prometheus.Labels{"host": c.Name})
	}

	var targets []docker.Container
	skipped, scanned := 0, 0
	managed := map[string]bool{}
	for _, ct := range containers {
		eff := u.resolver.Resolve(ct, ct.Labels)
		if !inScope(eff, ct.Name, ct.Image, opts) {
			logger.Debug("not in this run's scope", "container", ct.Name)
			if ok, _ := keep(ct, u.deps.Config.Filters); ok || (u.selfID != "" && selfupdate.IsSelf(u.selfID, ct.ID)) {
				managed[ct.Name] = true
			}
			if regular && !eff.Skip {
				if created, err := c.ImageCreated(ctx, ct.ImageID); err == nil {
					u.deps.Metrics.ImageAgeSeconds.WithLabelValues(c.Name, ct.Name, eff.PolicyID).Set(time.Since(created).Seconds())
				}
			}
			continue
		}
		scanned++
		ok, reason := keep(ct, u.deps.Config.Filters)
		if !ok {
			if reason != tempContainerReason && u.selfID != "" && u.deps.Config.Update.SelfUpdate.IsEnabled() && selfupdate.IsSelf(u.selfID, ct.ID) {
				logger.Debug("self container kept despite filter", "container", ct.Name, "reason", reason)
				targets = append(targets, ct)
				managed[ct.Name] = true
				continue
			}
			skipped++
			logger.Debug("skipped", "container", ct.Name, "reason", reason)
			continue
		}
		targets = append(targets, ct)
		managed[ct.Name] = true
	}
	u.retainStatuses(c.Name, managed)

	hr := newHostRun(runID, targets)
	hr.tokens = u.loadTokens(ctx, c, all, targets)
	parallel := !u.deps.Config.Update.ComposeGroup && u.deps.Config.Update.Strategy != config.StrategyRolling &&
		u.deps.Config.Update.Concurrency > 1
	hr.deferReverts = parallel
	var mu sync.Mutex
	var drained []string
	var selfUpdates []*selfUpdatePlan
	recordDependent := func(gctx context.Context, d dependentResult) {
		rec := audit.Record{
			RunID: runID, Host: c.Name, Container: d.ct.Name, ContainerID: d.ct.ID,
			Image: d.ct.Image, Policy: d.policy,
		}
		if d.err != nil {
			t.add(runItem(c, d.ct, stepResult{}, string(audit.DecisionFailed), d.err), false, true)
			u.deps.Metrics.UpdateFailures.WithLabelValues(c.Name, d.ct.Name, d.policy).Inc()
			u.notify(gctx, runID, notify.Event{
				Type: notify.EventUpdateFailure, Host: c.Name,
				Container: d.ct.Name, Image: d.ct.Image, Policy: d.policy, Err: d.err,
			})
			rec.Decision, rec.Error = audit.DecisionFailed, d.err.Error()
			u.deps.Audit.Append(rec)
			logger.Error("container sharing a network was not reattached", "container", d.ct.Name, "err", d.err)
			return
		}
		t.add(runItem(c, d.ct, stepResult{}, string(decisionReattached), nil), false, false)
		rec.Decision, rec.Reason = decisionReattached, d.reason
		u.deps.Audit.Append(rec)
		u.notify(gctx, runID, notify.Event{
			Type: notify.EventRecovered, Host: c.Name,
			Container: d.ct.Name, Image: d.ct.Image, Policy: d.policy, Reason: d.reason,
		})
		logger.Info("container sharing a network reattached", "container", d.ct.Name, "action", d.reason)
	}
	process := func(gctx context.Context, ct docker.Container) {
		listedID := ct.ID
		hr.begin(listedID)
		ct.ID = hr.current(ct.ID)
		var res stepResult
		defer func() {
			for _, d := range res.dependents {
				recordDependent(gctx, d)
			}
			if d, ok := hr.settle(listedID); ok {
				recordDependent(gctx, d)
			}
		}()
		if u.Draining() {
			mu.Lock()
			drained = append(drained, ct.Name)
			mu.Unlock()
			return
		}
		var err error
		res, err = u.processContainer(gctx, hr, c, ct)
		if res.drained {
			mu.Lock()
			drained = append(drained, ct.Name)
			mu.Unlock()
		}
		if res.self != nil && err == nil {
			mu.Lock()
			selfUpdates = append(selfUpdates, res.self)
			mu.Unlock()
			return
		}
		u.report(gctx, runID, c, t, ct, res, err)
	}

	if u.deps.Config.Update.ComposeGroup {
		groups := groupByCompose(targets, u.resolver)
		for _, g := range groups {
			for _, ct := range g.containers {
				process(ctx, ct)
			}
		}
	} else {
		concurrency := u.deps.Config.Update.Concurrency
		if u.deps.Config.Update.Strategy == config.StrategyRolling {
			concurrency = 1
		}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(concurrency)
		for _, unit := range networkUnits(targets) {
			unit := unit
			g.Go(func() error {
				for _, ct := range unit {
					process(gctx, ct)
				}
				return nil
			})
		}
		_ = g.Wait()
	}
	for _, p := range hr.takeRefusals() {
		u.revertPull(ctx, hr, c, &p)
	}
	if len(drained) > 0 {
		sort.Strings(drained)
		logger.Warn("shutting down: container updates left for the next run", "containers", drained)
	}

	sort.SliceStable(t.items, func(i, j int) bool { return t.items[i].Container < t.items[j].Container })
	return hostStats{
		Scanned:     scanned,
		Updated:     t.updated,
		Failed:      t.failed,
		Skipped:     skipped,
		Items:       t.items,
		Drained:     len(drained),
		selfUpdates: selfUpdates,
	}, nil
}

type tally struct {
	mu              sync.Mutex
	updated, failed int
	items           []notify.RunItem
}

func (t *tally) add(it notify.RunItem, updated, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = append(t.items, it)
	if updated {
		t.updated++
	}
	if failed {
		t.failed++
	}
}

func runItem(c *docker.Client, ct docker.Container, res stepResult, result string, err error) notify.RunItem {
	it := notify.RunItem{
		Host: c.Name, Container: ct.Name, Image: ct.Image, OldTag: res.oldTag, NewTag: res.newTag,
		OldDigest: res.oldDigest, NewDigest: res.newDigest, ReleaseURL: res.releaseURL, Result: result,
	}
	if err != nil {
		it.Error = err.Error()
	}
	return it
}

func (u *Updater) report(ctx context.Context, runID string, c *docker.Client, t *tally, ct docker.Container, res stepResult, err error) {
	logger := u.deps.Logger.With("host", c.Name, "run_id", runID)
	target := lifecycle.Target{
		RunID: runID, Host: c.Name, Container: ct.Name, ContainerID: ct.ID, Policy: res.policyMode,
		Image: ct.Image, NewImage: res.newImage, PolicyHooks: res.hooks,
		LabelPreUpdate: res.labels.PreUpdate, LabelPostUpdate: res.labels.PostUpdate, LabelOnFailure: res.labels.OnFailure,
		Running: ct.State == "running",
	}
	switch {
	case err != nil:
		decision := audit.DecisionFailed
		switch {
		case errors.Is(err, errRolledBack):
			decision = audit.DecisionRolledBack
		case errors.Is(err, errCheckFailed):
			decision = audit.DecisionCheckFailed
		}
		t.add(runItem(c, ct, res, string(decision), err), false, true)
		u.deps.Metrics.UpdateFailures.WithLabelValues(c.Name, ct.Name, res.policyMode).Inc()
		ev := notify.Event{
			Type: notify.EventUpdateFailure, Host: c.Name,
			Container: ct.Name, Image: ct.Image,
			OldDigest: res.oldDigest, NewDigest: res.newDigest,
			OldTag: res.oldTag, NewTag: res.newTag,
			Policy: res.policyMode, Err: err, ReleaseURL: res.releaseURL,
		}
		u.notify(ctx, runID, ev)
		switch decision {
		case audit.DecisionRolledBack:
			ev.Type = notify.EventRolledBack
			u.notify(ctx, runID, ev)
		case audit.DecisionCheckFailed:
			ev.Type = notify.EventCheckFailed
			u.notify(ctx, runID, ev)
		}
		if !u.dryRun(ctx) {
			_ = u.hooks.Run(ctx, lifecycle.PhaseFailure, c, target)
		}
		u.deps.Audit.Append(audit.Record{
			RunID: runID, Host: c.Name, Container: ct.Name, ContainerID: ct.ID,
			Image: ct.Image, Decision: decision, Error: err.Error(),
			OldTag: res.oldTag, NewTag: res.newTag, Policy: res.policyMode,
			OldDigest: res.oldDigest, NewDigest: res.newDigest,
		})
		u.recordStatus(runID, c, ct, res, decision, err)
		logger.Error("update failed", "container", ct.Name, "err", err, "decision", decision)
	case res.updated:
		u.deps.Metrics.UpdatesTotal.WithLabelValues(c.Name, ct.Name, res.policyMode).Inc()
		u.notify(ctx, runID, notify.Event{
			Type: notify.EventUpdateSuccess, Host: c.Name,
			Container: ct.Name, Image: ct.Image,
			OldDigest: res.oldDigest, NewDigest: res.newDigest,
			OldTag: res.oldTag, NewTag: res.newTag,
			Policy: res.policyMode, ReleaseURL: res.releaseURL,
		})
		u.deps.Audit.Append(audit.Record{
			RunID: runID, Host: c.Name, Container: ct.Name, ContainerID: ct.ID,
			Image:     ct.Image,
			OldDigest: res.oldDigest, NewDigest: res.newDigest,
			OldTag: res.oldTag, NewTag: res.newTag,
			Decision: audit.DecisionUpdated, Policy: res.policyMode, Group: res.group,
		})
		t.add(runItem(c, ct, res, string(audit.DecisionUpdated), nil), true, false)
		u.recordStatus(runID, c, ct, res, audit.DecisionUpdated, nil)
		logger.Info("updated", "container", ct.Name, "image", ct.Image,
			"old", res.oldDigest, "new", res.newDigest, "tag_old", res.oldTag, "tag_new", res.newTag)
	default:
		if res.decision != "" {
			u.deps.Audit.Append(audit.Record{
				RunID: runID, Host: c.Name, Container: ct.Name, ContainerID: ct.ID,
				Image: ct.Image, Decision: res.decision, Reason: res.reason,
				Policy: res.policyMode, Group: res.group,
				OldTag: res.oldTag, NewTag: res.newTag,
				OldDigest: res.oldDigest, NewDigest: res.newDigest,
			})
			switch res.decision {
			case audit.DecisionSkipped, audit.DecisionDryRun, audit.DecisionSelfUpdate, decisionCheckIncomplete:
			default:
				u.deps.Metrics.UpdateSkipped.WithLabelValues(c.Name, ct.Name, res.policyMode, string(res.decision)).Inc()
			}
			u.recordStatus(runID, c, ct, res, res.decision, nil)
			switch res.decision {
			case audit.DecisionMonitorOnly, audit.DecisionDryRun:
				t.add(runItem(c, ct, res, string(res.decision), nil), false, false)
			}
			if typ := decisionEvent(res); typ != "" {
				reason := res.reason
				if typ == notify.EventNoUpdate {
					reason = string(res.decision) + ": " + res.reason
				}
				u.notify(ctx, runID, notify.Event{
					Type: typ, Host: c.Name,
					Container: ct.Name, Image: ct.Image,
					OldDigest: res.oldDigest, NewDigest: res.newDigest,
					OldTag: res.oldTag, NewTag: res.newTag,
					Policy: res.policyMode, Reason: reason, ReleaseURL: res.releaseURL,
				})
			}
		}
		logger.Debug("no-op", "container", ct.Name, "reason", res.reason)
	}
}

var tempSuffixRE = regexp.MustCompile(`^(.+)\.(?:dockpaum-old|self-update-old)-(\d+)$`)

const (
	stopGrace      = 30 * time.Second
	maxStopWait    = 2 * time.Minute
	leftoverSettle = 3 * time.Second
)

func (u *Updater) recoverLeftovers(ctx context.Context, runID string, c *docker.Client, all []docker.Container, helperRunning bool,
	own map[string]bool, observing bool, logger *slog.Logger) {
	taken := make(map[string]struct{}, len(all))
	for _, ct := range all {
		taken[ct.Name] = struct{}{}
	}
	for _, ct := range all {
		m := tempSuffixRE.FindStringSubmatch(ct.Name)
		if m == nil {
			continue
		}
		orig := m[1]
		selfLeftover := strings.Contains(ct.Name, ".self-update-old-")
		if selfLeftover && helperRunning {
			logger.Debug("self-update in progress, its leftover is not touched", "leftover", ct.Name)
			continue
		}
		if !u.ownsLeftover(ct, orig, selfLeftover, own) {
			logger.Debug("leftover container of another instance or of an excluded container, not touched",
				"leftover", ct.Name, "original", orig)
			continue
		}
		if _, exists := taken[orig]; exists {
			logger.Warn("leftover container from an interrupted update; replacement exists, leaving it for manual review",
				"leftover", ct.Name, "original", orig)
			continue
		}
		if observing {
			logger.Warn("dry-run: leftover container from an interrupted update is not restored; a run that is not a dry-run restores it",
				"leftover", ct.Name, "original", orig, "state", ct.State)
			continue
		}
		running := ct.State == "running"
		if running && !selfLeftover {
			var leave bool
			if running, leave = u.awaitLeftoverStop(ctx, c, ct, m[2], orig, logger); leave {
				continue
			}
		}
		if err := c.RenameContainer(ctx, ct.ID, orig); err != nil {
			logger.Error("recover: rename leftover back failed", "leftover", ct.Name, "original", orig, "err", err)
			continue
		}
		taken[orig] = struct{}{}
		if !running {
			if err := c.StartContainer(ctx, ct.ID); err != nil {
				logger.Error("recover: start of restored container failed", "container", orig, "err", err)
			}
		}
		logger.Warn("recovered container stranded by an interrupted update", "container", orig, "was", ct.Name)
		rec := audit.Record{
			RunID: runID, Host: c.Name, Container: orig, ContainerID: ct.ID,
			Image: ct.Image, Decision: audit.DecisionRecovered,
			Reason: "restored from leftover " + ct.Name,
		}
		u.deps.Audit.Append(rec)
		u.notify(ctx, runID, notify.Event{
			Type: notify.EventRecovered, Host: c.Name, Container: orig, Image: ct.Image, Reason: rec.Reason,
		})
	}
}

func (u *Updater) awaitLeftoverStop(ctx context.Context, c *docker.Client, ct docker.Container, suffix, orig string,
	logger *slog.Logger) (running, leave bool) {
	nanos, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		return true, false
	}
	full, err := c.Inspect(ctx, ct.ID)
	if err != nil {
		logger.Warn("recover: inspecting a running leftover failed; it is left for the next run", "leftover", ct.Name, "err", err)
		return true, true
	}
	stopSecs := full.EffectiveStopTimeout(u.stopTimeoutSecs())
	remaining := time.Until(time.Unix(0, nanos).Add(time.Duration(stopSecs)*time.Second + u.stopGrace))
	if remaining <= 0 {
		return true, false
	}
	logger.Info("leftover of an interrupted update still runs and may be stopping; waiting for its stop to end",
		"leftover", ct.Name, "wait", min(remaining, u.maxStopWait).Round(time.Second))
	if _, err := c.WaitExit(ctx, ct.ID, min(remaining, u.maxStopWait)); err != nil {
		if errors.Is(err, docker.ErrWaitTimeout) && remaining <= u.maxStopWait {
			return true, false
		}
		logger.Warn("leftover of an interrupted update may still be stopping; it is left for the next run",
			"leftover", ct.Name, "err", err)
		return true, true
	}
	select {
	case <-ctx.Done():
		return false, true
	case <-time.After(u.leftoverSettle):
	}
	if _, err := c.ContainerState(ctx, orig); err == nil {
		logger.Info("a replacement appeared after the leftover stopped; it is left to the update that made it",
			"leftover", ct.Name, "original", orig)
		return false, true
	}
	return false, false
}

func (u *Updater) ownsLeftover(ct docker.Container, orig string, selfLeftover bool, own map[string]bool) bool {
	if selfLeftover && (own[ct.ID] || (u.selfID != "" && selfupdate.IsSelf(u.selfID, ct.ID))) {
		return true
	}
	probe := ct
	probe.Name = orig
	ok, reason := keep(probe, u.deps.Config.Filters)
	if ok {
		return true
	}
	switch reason {
	case "not-in-include-list", "in-exclude-list", "include-regex-no-match", "exclude-regex-match":
		return selfLeftover
	}
	return false
}

type stepResult struct {
	updated    bool
	oldDigest  string
	newDigest  string
	oldTag     string
	newTag     string
	decision   audit.Decision
	reason     string
	policyMode string
	group      string
	hooks      config.PolicyHooks
	newImage   string
	labels     policy.Labels
	dependents []dependentResult
	drained    bool
	self       *selfUpdatePlan

	image             string
	currentID         string
	checked           bool
	firstSeen         time.Time
	eligibleAt        time.Time
	releaseURL        string
	mode              string
	policySource      string
	overrides         []string
	monitorOnly       bool
	windows           *window.Set
	check             string
	checkNote         string
	approval          *ApprovalStatus
	currentRepoDigest string
	newRepoDigest     string
	currentVersion    string
	newVersion        string
	newCreated        time.Time
}

func (u *Updater) processContainer(ctx context.Context, hr *hostRun, c *docker.Client, ct docker.Container) (res stepResult, err error) {
	runID := hr.runID
	full, err := c.Inspect(ctx, ct.ID)
	if err != nil {
		eff := u.resolver.Resolve(ct, ct.Labels)
		return stepResult{policyMode: eff.PolicyID, group: eff.ComposeGroup, hooks: eff.Hooks, labels: eff.Labels}, fmt.Errorf("inspect: %w", err)
	}
	eff, _ := u.resolver.ResolveStrict(ct, full.Config.Labels)
	res = stepResult{policyMode: eff.PolicyID, group: eff.ComposeGroup, hooks: eff.Hooks, labels: eff.Labels,
		image: full.Image, currentID: full.ImageID,
		mode: eff.Mode, policySource: policySource(eff, full.Config.Labels), overrides: eff.Overrides, monitorOnly: eff.MonitorOnly}
	oldDefaults, fromCache, derr := u.imageDefaults(ctx, c, full.ImageID)
	var runningLabels map[string]string
	if oldDefaults != nil {
		runningLabels = oldDefaults.Labels
	}
	res.currentVersion = registry.VersionOf(runningLabels, registry.Parse(full.Image).Tag)

	if t, errAge := c.ImageCreated(ctx, full.Image); errAge == nil {
		u.deps.Metrics.ImageAgeSeconds.WithLabelValues(c.Name, ct.Name, eff.PolicyID).Set(time.Since(t).Seconds())
	}

	if eff.Skip {
		res.decision = audit.DecisionGatedPolicy
		res.reason = eff.SkipReason
		return res, nil
	}
	isSelf := u.selfID != "" && selfupdate.IsSelf(u.selfID, ct.ID)
	if isSelf && !u.deps.Config.Update.SelfUpdate.IsEnabled() {
		res.decision = audit.DecisionGatedPolicy
		res.reason = "own container: self_update is disabled"
		return res, nil
	}
	if !isSelf && u.selfUnknown && selfupdate.IsDockpaum(full.Config.Labels, full.Image) {
		res.decision = audit.DecisionGatedPolicy
		res.reason = ownContainerUnknownReason
		return res, nil
	}
	if reason := orchestratedBy(full.Config.Labels); reason != "" {
		res.decision = decisionGatedOrchestrated
		res.reason = reason + "; update it through its orchestrator"
		return res, nil
	}
	if full.HostConfig != nil && full.HostConfig.AutoRemove {
		res.decision = decisionGatedAutoRemove
		res.reason = autoRemoveReason
		return res, nil
	}
	if mac := full.MACAddress(); mac != "" {
		res.decision = audit.DecisionGatedMACAddress
		res.reason = macAddressReason(mac, c.APIVersion())
		return res, nil
	}
	if eff.Mode == config.ModeNone {
		res.decision = audit.DecisionGatedPolicy
		res.reason = fmt.Sprintf("policy %q mode=none", eff.PolicyID)
		return res, nil
	}
	if strings.Contains(full.Image, "@sha256:") {
		res.decision = audit.DecisionGatedDigest
		res.reason = "image is digest-pinned"
		return res, nil
	}

	res.windows = eff.Windows
	windowOpen := eff.Windows.Allows(u.windowTime())
	outsideWindow := func() (stepResult, error) {
		res.decision = audit.DecisionGatedWindow
		res.reason = fmt.Sprintf("outside policy %q window", eff.PolicyID)
		return res, nil
	}

	imageRef := full.Image
	res.oldTag = registry.Parse(imageRef).Tag
	creds := u.credentialsFor(hr, c.Name, ct.Labels, imageRef)
	ctx = registry.WithCredentials(ctx, creds)

	semver := eff.Mode == config.ModePatch || eff.Mode == config.ModeMinor || eff.Mode == config.ModeMajor
	if semver && eff.Labels.NoPull {
		semver = false
	}
	if semver {
		rules := registry.CompiledTagRules(eff.TagInclude, eff.TagExclude)
		newRef, newTag, outcome, reason, err := u.findNewerTag(ctx, runID, imageRef, eff.Mode, rules)
		if err == nil && outcome != tagNoCandidates {
			u.warnIncomplete(c.Name, ct.Name, imageRef, "")
		}
		switch {
		case err != nil && !windowOpen:
			u.deps.Logger.Debug("registry check failed outside the maintenance window",
				"host", c.Name, "container", ct.Name, "image", imageRef, "err", err)
			return outsideWindow()
		case err != nil:
			return res, fmt.Errorf("%w: %w", errCheckFailed, err)
		case outcome == tagNone:
			res.checked, res.check = true, CheckRegistry
			if !windowOpen {
				return outsideWindow()
			}
			res.decision = audit.DecisionSkipped
			res.reason = reason
			return res, nil
		case outcome == tagNoCandidates:
			u.warnIncomplete(c.Name, ct.Name, imageRef, reason)
			if !windowOpen {
				return outsideWindow()
			}
			res.decision = decisionCheckIncomplete
			res.reason = reason
			return res, nil
		case outcome == tagFallback:
			u.deps.Logger.Info("tag is not a version, following its digest instead",
				"container", ct.Name, "image", imageRef, "mode", eff.Mode, "reason", reason)
		default:
			imageRef = newRef
			res.newTag = newTag
			full.Image = newRef
			full.Config.Image = newRef
		}
	}
	res.newImage = imageRef

	runningDigests, digestsErr := c.ImageRepoDigests(ctx, full.ImageID)
	if digestsErr == nil {
		res.currentRepoDigest = registry.RepoDigestFor(runningDigests, registry.Parse(res.image))
	}
	var cand *candidate
	var checkErr error
	switch {
	case eff.Labels.NoPull:
	case digestsErr != nil:
		checkErr = fmt.Errorf("the running image cannot be inspected: %w", digestsErr)
	default:
		cand, checkErr = u.checkUpstream(ctx, runID, c, full, imageRef, runningDigests)
	}

	var newImageID, tagAtCheck string
	switch {
	case eff.Labels.NoPull:
		res.check = CheckLocal
		newImageID, err = c.ImageID(ctx, imageRef)
		if err != nil {
			if !windowOpen {
				return outsideWindow()
			}
			return res, fmt.Errorf("%w: no-pull: local image %s: %w", errCheckFailed, imageRef, err)
		}
	case checkErr == nil:
		u.noteFallback(c.Name, ct.Name, imageRef, nil, false)
		res.check = CheckRegistry
		newImageID = cand.imageID
		res.newRepoDigest = cand.pullRef
		if res.newTag == "" {
			hr.tagBefore(ctx, c, imageRef)
			tagAtCheck, _ = c.ImageID(ctx, imageRef)
		}
	default:
		res.checkNote = checkErr.Error()
		rateLimited := errors.Is(checkErr, registry.ErrRateLimited)
		switch {
		case !windowOpen:
			return outsideWindow()
		case eff.Labels.MonitorOnly || u.dryRun(ctx) || rateLimited:
			u.noteFallback(c.Name, ct.Name, imageRef, checkErr, false)
			var why string
			switch {
			case eff.Labels.MonitorOnly:
				why = "a monitor-only container is not pulled to compare"
			case u.dryRun(ctx):
				why = "dry-run does not pull to compare"
			default:
				why = "no container is pulled to compare while the registry limits requests: the pull would count against the same limit"
			}
			res.decision = decisionCheckIncomplete
			res.reason = fmt.Sprintf("not checked: the registry was not asked directly (%v), and %s", checkErr, why)
			return res, nil
		}
		u.noteFallback(c.Name, ct.Name, imageRef, checkErr, true)
		res.check = CheckPull
		full.Platform = u.keepPlatform(ctx, c, ct.Name, full)
		var moved *pulledTag
		newImageID, moved, err = u.pullToCompare(ctx, hr, c, full, imageRef, res.newTag == "", creds)
		if err != nil {
			return res, err
		}
		if moved != nil {
			defer u.settlePull(ctx, hr, c, moved, &res)
		}
		if digests, err := c.ImageRepoDigests(ctx, newImageID); err == nil {
			res.newRepoDigest = registry.RepoDigestFor(digests, registry.Parse(imageRef))
		}
	}
	res.oldDigest = full.ImageID
	res.newDigest = newImageID
	res.checked = true

	unchanged := newImageID == full.ImageID && res.newTag == ""
	var soakHeld string
	if !unchanged {
		var labels map[string]string
		var created time.Time
		var createdErr error
		if cand != nil && !cand.running {
			labels, created = cand.labels, cand.created
			if created.IsZero() {
				createdErr = errNoCreated
			}
		} else {
			if d, err := c.ImageDefaults(ctx, newImageID); err == nil {
				labels = d.Labels
			} else {
				u.deps.Logger.Debug("could not read the labels of the new image; no release-notes link", "host", c.Name, "image_id", newImageID, "err", err)
			}
			created, createdErr = c.ImageCreated(ctx, newImageID)
		}
		tag := res.newTag
		if tag == "" {
			tag = res.oldTag
		}
		res.newVersion = registry.VersionOf(labels, tag)
		res.releaseURL = u.releaseURL(full, oldDefaults, labels, tag)
		if createdErr == nil {
			res.newCreated = created
		}
		if eff.MinImageAge > 0 {
			sk := u.soakHold(c.Name, imageRef, newImageID, created, createdErr, eff.MinImageAge, u.ageSourceOf(eff.PolicyID))
			res.firstSeen, res.eligibleAt, soakHeld = sk.firstSeen, sk.eligibleAt, sk.hold
		}
	}

	if unchanged && !u.deps.Config.Update.IncludeRestart {
		if !windowOpen {
			return outsideWindow()
		}
		res.decision = audit.DecisionSkipped
		res.reason = "image up-to-date"
		return res, nil
	}
	if !windowOpen {
		return outsideWindow()
	}

	if isSelf {
		if reason := u.selfUpdateHeld(c.Name, ct.Name, newImageID); reason != "" {
			res.decision = audit.DecisionGatedPolicy
			res.reason = reason
			return res, nil
		}
	}
	if soakHeld != "" {
		res.decision = audit.DecisionGatedAge
		res.reason = soakHeld
		return res, nil
	}
	if eff.Labels.MonitorOnly {
		if unchanged {
			res.decision = audit.DecisionSkipped
			res.reason = "image up-to-date"
			return res, nil
		}
		res.decision = audit.DecisionMonitorOnly
		res.reason = "update available; the container is labelled monitor-only"
		return res, nil
	}

	if u.dryRun(ctx) {
		res.decision = audit.DecisionDryRun
		res.reason = "dry-run"
		if a := u.deps.Config.Update.Approval; a != nil && a.URL != "" {
			res.reason = "dry-run; the approval endpoint is not asked in dry-run"
		}
		return res, nil
	}

	if last, why := u.approvalHold(c.Name, ct.Name); last != nil {
		u.deps.Metrics.ApprovalHolds.WithLabelValues(c.Name).Inc()
		res.decision, res.reason, res.approval = audit.DecisionApprovalDeny, why, last
		return res, nil
	}
	subject := approvalSubject{
		OldDigest: full.ImageID, NewDigest: newImageID, OldRepoDigest: res.currentRepoDigest, NewRepoDigest: res.newRepoDigest,
		Labels: maps.Clone(full.Config.Labels),
	}
	if ok, err := u.approve(ctx, runID, c, ct, full, subject, approvalPhaseInitial, imageRef, &res); !ok {
		return res, err
	}

	stillDue := func() bool {
		switch {
		case !eff.Windows.Allows(u.windowTime()):
			res.decision = audit.DecisionGatedWindow
			res.reason = fmt.Sprintf("policy %q window closed during the run", eff.PolicyID)
		case u.Draining():
			res.decision = audit.DecisionSkipped
			res.reason = shutdownReason
			res.drained = true
		default:
			return true
		}
		return false
	}

	fetched := false
	if cand != nil && !cand.running {
		if !stillDue() {
			return res, nil
		}
		full.Platform = u.keepPlatform(ctx, c, ct.Name, full)
		id, err := u.fetchCandidate(ctx, c, full, cand, creds)
		if err != nil {
			if id != "" {
				u.keepFetched(ctx, c, imageRef, id)
			}
			return res, err
		}
		fetched = true
	}
	if !stillDue() {
		if fetched {
			u.keepFetched(ctx, c, imageRef, newImageID)
		}
		return res, nil
	}

	switch {
	case derr != nil:
		u.deps.Logger.Warn("could not read old image defaults (the image is gone and was not seen before); "+
			"the recreated container keeps its current cmd, env and healthcheck instead of the new image's",
			"container", ct.Name, "image_id", full.ImageID, "err", derr)
		oldDefaults = nil
	case fromCache:
		u.deps.Logger.Info("old image is no longer in the image store; using its defaults recorded in an earlier run",
			"container", ct.Name, "image_id", full.ImageID)
	}

	if isSelf {
		res.self = &selfUpdatePlan{
			c: c, ct: ct, full: full, oldDefaults: oldDefaults, eff: eff,
			imageRef: imageRef, newImageID: newImageID, cand: cand, fetched: fetched, tagAtCheck: tagAtCheck,
			subject: subject,
		}
		res.self.res = res
		return res, nil
	}

	if cand != nil {
		moved, err := u.pointTag(ctx, hr, c, full, imageRef, newImageID, res.newTag == "", tagAtCheck)
		if err != nil {
			if fetched {
				u.keepFetched(ctx, c, imageRef, newImageID)
			}
			return res, fmt.Errorf("%w: %w", errCheckFailed, err)
		}
		if moved != nil {
			defer u.settlePull(ctx, hr, c, moved, &res)
		}
	}

	if res.newTag == "" {
		full.PinComposeImage(newImageID)
	}

	target := lifecycle.Target{
		RunID: runID, Host: c.Name, Container: ct.Name, ContainerID: ct.ID, Policy: eff.PolicyID,
		Image: ct.Image, NewImage: imageRef, PolicyHooks: eff.Hooks,
		LabelPreUpdate: eff.Labels.PreUpdate, LabelPostUpdate: eff.Labels.PostUpdate, LabelOnFailure: eff.Labels.OnFailure,
		Running: full.Running,
	}
	if err := u.hooks.Run(ctx, lifecycle.PhasePre, c, target); err != nil {
		if lifecycle.IsSkipRequest(err) {
			res.decision = audit.DecisionSkipped
			res.reason = fmt.Sprintf("a pre-update hook asked to skip this update (exit %d)", lifecycle.SkipExitCode)
			return res, nil
		}
		return res, fmt.Errorf("pre-update hook: %w", err)
	}

	previous := state.PreviousImage{Image: ct.Image, ImageID: full.ImageID, NewImage: imageRef, NewImageID: newImageID, RunID: runID}
	if digestsErr == nil {
		previous.RepoDigests = runningDigests
	} else {
		u.deps.Logger.Debug("could not read the registry digests of the old image", "container", ct.Name, "image_id", full.ImageID, "err", digestsErr)
	}

	if !eff.Windows.Allows(u.windowTime()) {
		u.deps.Logger.Warn("maintenance window closed while the pre-update hooks ran; the update that began is finished",
			"container", ct.Name, "policy", eff.PolicyID)
	}
	if u.approval.recheck() {
		if ok, err := u.approve(ctx, runID, c, ct, full, subject, approvalPhaseFinal, imageRef, &res); !ok {
			return res, err
		}
	}
	dependents, err := u.recreate(ctx, hr, c, full, recreateOpts{
		oldDefaults:   oldDefaults,
		healthTimeout: eff.HealthTimeout,
		expectImageID: newImageID,
	})
	res.dependents = dependents
	if err != nil {
		return res, err
	}
	hr.markDeployed(imageRef, newImageID)
	u.forgetCandidate(ctx, c, imageRef, newImageID)
	previous.ContainerID = hr.current(ct.ID)
	u.state.RecordPrevious(c.Name, ct.Name, previous)

	if u.deps.Config.Update.CleanupImages {
		_ = c.RemoveImageIfDangling(ctx, full.ImageID)
	}
	if err := u.hooks.Run(ctx, lifecycle.PhasePost, c, target); err != nil {
		u.deps.Logger.Warn("post-update hook failed", "container", ct.Name, "err", err)
	}
	res.updated = true
	return res, nil
}

func (u *Updater) observing() bool {
	return u.deps.Config.DryRun || u.state.Snapshot()
}

func (u *Updater) windowTime() time.Time {
	if u.windowNow != nil {
		return u.windowNow()
	}
	return u.now()
}

func policySource(eff policy.Resolved, labels map[string]string) string {
	if eff.Skip {
		return eff.SkipReason
	}
	raw := strings.TrimSpace(labels[policy.LabelPolicy])
	switch {
	case raw == "" || strings.EqualFold(raw, "default"):
		return "default policy"
	case eff.Builtin:
		return fmt.Sprintf("label %s=%s (built-in policy)", policy.LabelPolicy, raw)
	}
	return fmt.Sprintf("label %s=%s", policy.LabelPolicy, raw)
}

func (u *Updater) keepPlatform(ctx context.Context, c *docker.Client, container string, full *docker.FullContainer) string {
	platform, err := c.ForeignPlatform(ctx, full)
	if err != nil {
		u.deps.Logger.Debug("could not determine the platform of the running image; pulling for the daemon's default platform",
			"container", container, "image_id", full.ImageID, "err", err)
		return ""
	}
	if platform != "" {
		u.deps.Logger.Debug("image runs under emulation, keeping its platform", "container", container, "platform", platform)
	}
	return platform
}

func (u *Updater) keepFetched(ctx context.Context, c *docker.Client, ref, id string) {
	rctx, cancel := recoveryContext(ctx)
	defer cancel()
	u.keepRefused(rctx, c, ref, id)
}

const (
	shutdownReason            = "dockpaum is shutting down; the update is left for the next run"
	ownContainerUnknownReason = "own container not identified: dockpaum runs in a container it could not find on the Docker hosts, " +
		"so containers running the dockpaum image are not recreated (one of them may be dockpaum itself)"
)

type tagOutcome int

const (
	tagFound tagOutcome = iota
	tagNone
	tagFallback
	tagNoCandidates
)

var errCheckFailed = errors.New("check failed")

func (u *Updater) findNewerTag(ctx context.Context, runID, imageRef, mode string, rules registry.TagRules) (newRef, newTag string, outcome tagOutcome, reason string, err error) {
	ref := registry.Parse(imageRef)
	if ref.Tag == "" {
		return "", "", tagFallback, "image has no tag", nil
	}
	cur, parsed := rules.ParseTag(ref.Tag)
	if !parsed {
		return "", "", tagFallback, fmt.Sprintf("tag %q is not a version", ref.Tag), nil
	}
	if need := registry.MinComponents(mode); len(cur.Components) < need {
		return "", "", tagNone, fmt.Sprintf("tag %q has %d version part(s); %s needs at least %d — use minor or major for this image", ref.Tag, len(cur.Components), mode, need), nil
	}
	if err := u.backoff.blocked(ref.Registry, runID, u.now()); err != nil {
		return "", "", tagNone, "", fmt.Errorf("registry tag list: %w", err)
	}
	tags, err := u.reg.ListTags(ctx, ref)
	if err != nil {
		u.noteRateLimit(ref.Registry, runID, err)
		return "", "", tagNone, "", fmt.Errorf("registry tag list: %w", err)
	}
	p := rules.Pick(cur, tags, mode)
	switch {
	case p.Outcome == registry.PickFound && p.Tag != ref.Tag:
		return replaceTag(imageRef, p.Tag), p.Tag, tagFound, "", nil
	case p.Outcome == registry.PickNoCandidates &&
		(rules.IsZero() || (registry.TagRules{}).Pick(cur, tags, mode).Outcome == registry.PickNoCandidates):
		return "", "", tagNoCandidates, p.Reason(mode), nil
	case !rules.IsZero():
		return "", "", tagNone, fmt.Sprintf("no newer %s-eligible tag found (after the policy's tag filter, which excluded %d of %d listed tag(s))",
			mode, p.Excluded, len(tags)), nil
	}
	return "", "", tagNone, p.Reason(mode), nil
}

func replaceTag(imageRef, newTag string) string {
	if i := strings.Index(imageRef, "@"); i >= 0 {
		imageRef = imageRef[:i]
	}
	slash := strings.LastIndex(imageRef, "/")
	if colon := strings.LastIndex(imageRef, ":"); colon > slash {
		return imageRef[:colon+1] + newTag
	}
	return imageRef + ":" + newTag
}

var errRolledBack = errors.New("rolled back")

func recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
}

type recreateOpts struct {
	oldDefaults   *docker.ImageDefaults
	healthTimeout time.Duration
	expectImageID string
}

func (u *Updater) recreate(ctx context.Context, hr *hostRun, c *docker.Client, fc *docker.FullContainer, o recreateOpts) ([]dependentResult, error) {
	if !hr.claim(fc.ID) {
		return nil, fmt.Errorf("network_mode references of %s form a loop", fc.Name)
	}
	defer hr.release(fc.ID)
	if o.oldDefaults != nil {
		fc.StripImageDefaults(o.oldDefaults)
	}
	if ref := fc.NetworkOwnerRef(); ref != "" {
		if cur := hr.current(ref); cur != ref {
			fc.JoinNetworkOf(cur)
		}
	}
	if err := c.ResolveNetworkOwner(ctx, fc); err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	dependents, err := u.networkDependents(ctx, c, fc)
	if err != nil {
		return nil, fmt.Errorf("list containers sharing its network: %w", err)
	}
	if o.expectImageID != "" {
		if err := checkImage(ctx, c, fc.Image, o.expectImageID); err != nil {
			return nil, fmt.Errorf("%w: %w", errCheckFailed, err)
		}
	}
	stopSecs := fc.EffectiveStopTimeout(u.stopTimeoutSecs())
	tempName := fc.Name + ".dockpaum-old-" + nowSuffix()
	if err := c.RenameContainer(ctx, fc.ID, tempName); err != nil {
		return nil, fmt.Errorf("rename old: %w", err)
	}
	if fc.Running {
		if err := c.StopContainer(ctx, fc.ID, stopSecs); err != nil {
			var results []dependentResult
			if u.undoStop(ctx, c, fc, stopSecs, docker.StopMayContinue(ctx, err)) {
				results = u.rejoin(ctx, c, fc, dependents)
			}
			return results, fmt.Errorf("stop: %w", err)
		}
	}
	if o.expectImageID != "" {
		if err := checkImage(ctx, c, fc.Image, o.expectImageID); err != nil {
			u.restoreOld(ctx, c, fc, "")
			return u.rejoin(ctx, c, fc, dependents), fmt.Errorf("%w: %w", errRolledBack, err)
		}
	}
	newID, err := c.CreateContainer(ctx, fc.Name, fc)
	if err != nil {
		u.restoreOld(ctx, c, fc, newID)
		return u.rejoin(ctx, c, fc, dependents), fmt.Errorf("create: %w", err)
	}
	if fc.Running {
		if err := c.StartContainer(ctx, newID); err != nil {
			if u.deps.Config.Update.Rollback() {
				u.restoreOld(ctx, c, fc, newID)
				return u.rejoin(ctx, c, fc, dependents), fmt.Errorf("%w: start new: %w", errRolledBack, err)
			}
			u.deps.Logger.Error("start of replacement failed and rollback is disabled — old container remains stopped",
				"container", fc.Name, "old_id", fc.ID, "old_name", tempName)
			return nil, fmt.Errorf("start new: %w", err)
		}
		if err := u.healthCheck(ctx, c, newID, o.healthTimeout); err != nil {
			if u.deps.Config.Update.Rollback() {
				u.deps.Logger.Warn("health check failed, rolling back",
					"container", fc.Name, "err", err)
				u.removeNew(ctx, c, newID, stopSecs)
				u.restoreOld(ctx, c, fc, "")
				return u.rejoin(ctx, c, fc, dependents), fmt.Errorf("%w: health check: %w", errRolledBack, err)
			}
			return nil, fmt.Errorf("health check: %w", err)
		}
	}
	hr.replace(fc.ID, newID)
	results := u.reattach(ctx, hr, c, fc, newID, dependents)
	rctx, cancel := recoveryContext(ctx)
	defer cancel()
	if err := c.RemoveContainer(rctx, fc.ID); err != nil {
		u.deps.Logger.Warn("remove old container failed", "id", fc.ID, "err", err)
	}
	return results, nil
}

func checkImage(ctx context.Context, c *docker.Client, ref, want string) error {
	got, err := c.ImageID(ctx, ref)
	if err != nil {
		return fmt.Errorf("verify image %s: %w", ref, err)
	}
	if got != want {
		return fmt.Errorf("image %s now resolves to %s, not to the approved %s (the tag moved during the update)", ref, got, want)
	}
	return nil
}

func (u *Updater) undoStop(ctx context.Context, c *docker.Client, fc *docker.FullContainer, stopSecs int, stopping bool) bool {
	wait := min(time.Duration(stopSecs)*time.Second+u.stopGrace, u.maxStopWait)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait+2*time.Minute)
	defer cancel()
	if stopping {
		if _, err := c.WaitExit(rctx, fc.ID, wait); err != nil {
			u.deps.Logger.Warn("the interrupted stop was not seen to end; the container keeps its temporary name until the next run recovers it",
				"id", fc.ID, "name", fc.Name, "err", err)
			return false
		}
	}
	if err := c.RenameContainer(rctx, fc.ID, fc.Name); err != nil {
		u.deps.Logger.Error("stop failed and renaming the container back failed — it remains under its temporary name until the next run recovers it",
			"id", fc.ID, "name", fc.Name, "err", err)
	}
	if st, err := c.ContainerState(rctx, fc.ID); err == nil && st.Running {
		return false
	}
	if err := c.StartContainer(rctx, fc.ID); err != nil {
		u.deps.Logger.Error("stop failed and restarting the container failed — service is down",
			"id", fc.ID, "name", fc.Name, "err", err)
		return false
	}
	return true
}

func (u *Updater) healthCheck(ctx context.Context, c *docker.Client, id string, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	started, err := c.ContainerState(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect after start: %w", err)
	}
	g := healthGate{timeout: timeout, graceCap: u.healthGraceCap, poll: u.healthPoll}
	return g.wait(ctx, c, id, started)
}

func (u *Updater) removeNew(ctx context.Context, c *docker.Client, newID string, stopSecs int) {
	rctx, cancel := recoveryContext(ctx)
	defer cancel()
	if err := c.StopContainer(rctx, newID, stopSecs); err != nil {
		u.deps.Logger.Warn("rollback: stop new container failed", "id", newID, "err", err)
	}
	if err := c.RemoveContainer(rctx, newID); err != nil {
		u.deps.Logger.Warn("rollback: remove new container failed", "id", newID, "err", err)
	}
}

func (u *Updater) restoreOld(ctx context.Context, c *docker.Client, fc *docker.FullContainer, removeNewID string) {
	rctx, cancel := recoveryContext(ctx)
	defer cancel()
	if removeNewID != "" {
		if err := c.RemoveContainer(rctx, removeNewID); err != nil {
			u.deps.Logger.Warn("rollback: remove new container failed", "id", removeNewID, "err", err)
		}
	}
	if err := c.RenameContainer(rctx, fc.ID, fc.Name); err != nil {
		u.deps.Logger.Error("rollback: rename old container back failed — container remains under temp name",
			"id", fc.ID, "name", fc.Name, "err", err)
	}
	if fc.Running {
		if err := c.StartContainer(rctx, fc.ID); err != nil {
			u.deps.Logger.Error("rollback: restart old container failed — service is down",
				"id", fc.ID, "name", fc.Name, "err", err)
		}
	}
}

type composeGroup struct {
	name       string
	containers []docker.Container
}

func groupByCompose(targets []docker.Container, resolver *policy.Resolver) []composeGroup {
	byName := map[string][]docker.Container{}
	order := []string{}
	for _, ct := range targets {
		eff := resolver.Resolve(ct, ct.Labels)
		key := eff.ComposeGroup
		if key == "" {
			key = "__ungrouped__:" + ct.Name
		}
		if _, ok := byName[key]; !ok {
			order = append(order, key)
		}
		byName[key] = append(byName[key], ct)
	}
	out := make([]composeGroup, 0, len(order))
	for _, k := range order {
		out = append(out, composeGroup{name: k, containers: sortByDependencies(byName[k])})
	}
	return out
}

func sortByDependencies(containers []docker.Container) []docker.Container {
	base := append([]docker.Container(nil), containers...)
	sort.SliceStable(base, func(i, j int) bool {
		an := base[i].Labels[policy.ComposeNumberLabel]
		bn := base[j].Labels[policy.ComposeNumberLabel]
		if an != bn && an != "" && bn != "" {
			ai, aerr := strconv.Atoi(an)
			bi, berr := strconv.Atoi(bn)
			if aerr == nil && berr == nil {
				return ai < bi
			}
			return an < bn
		}
		return base[i].Name < base[j].Name
	})

	bySvc := map[string][]docker.Container{}
	var svcOrder []string
	for _, ct := range base {
		svc := ct.Labels[policy.ComposeServiceLabel]
		if svc == "" {
			continue
		}
		if _, ok := bySvc[svc]; !ok {
			svcOrder = append(svcOrder, svc)
		}
		bySvc[svc] = append(bySvc[svc], ct)
	}

	deps := map[string][]string{}
	for _, svc := range svcOrder {
		seen := map[string]struct{}{}
		for _, ct := range bySvc[svc] {
			for _, d := range dependencyList(ct.Labels[policy.ComposeDependsLabel]) {
				if d == svc {
					continue
				}
				if _, present := bySvc[d]; !present {
					continue
				}
				if _, dup := seen[d]; dup {
					continue
				}
				seen[d] = struct{}{}
				deps[svc] = append(deps[svc], d)
			}
		}
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[string]int{}
	var sorted []string
	var visit func(svc string)
	visit = func(svc string) {
		if state[svc] != unvisited {
			return
		}
		state[svc] = visiting
		for _, d := range deps[svc] {
			visit(d)
		}
		state[svc] = done
		sorted = append(sorted, svc)
	}
	for _, svc := range svcOrder {
		visit(svc)
	}

	out := make([]docker.Container, 0, len(base))
	for _, svc := range sorted {
		out = append(out, bySvc[svc]...)
	}
	for _, ct := range base {
		if ct.Labels[policy.ComposeServiceLabel] == "" {
			out = append(out, ct)
		}
	}
	return out
}

func dependencyList(label string) []string {
	if label == "" {
		return nil
	}
	var out []string
	for _, entry := range strings.FieldsFunc(label, func(r rune) bool { return r == ',' || r == ' ' }) {
		entry = strings.TrimSpace(entry)
		if i := strings.IndexByte(entry, ':'); i >= 0 {
			entry = entry[:i]
		}
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

func nowSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
