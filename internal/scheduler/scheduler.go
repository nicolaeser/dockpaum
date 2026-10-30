package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/window"
)

type Runnable interface {
	Run(ctx context.Context) error
}

type Scheduler struct {
	cfg          config.ScheduleConfig
	job          Runnable
	logger       *slog.Logger
	cron         *cron.Cron
	stopCh       chan struct{}
	stopOnce     sync.Once
	mu           sync.Mutex
	stopped      bool
	runSem       chan struct{}
	tickWG       sync.WaitGroup
	windows      *window.Set
	execCtx      context.Context
	execCancel   context.CancelFunc
	onTick       func()
	onSkip       func(SkipReason)
	policies     int
	jobs         int
	pending      map[string]bool
	loc          *time.Location
	main         cron.Schedule
	entries      []cronEntry
	runOnStart   bool
	started      atomic.Bool
	nextInterval atomic.Int64
}

type cronEntry struct {
	kind  string
	name  string
	spec  string
	sched cron.Schedule
}

type TriggerResult int

const (
	TriggerQueued TriggerResult = iota
	TriggerDuplicate
	TriggerStopped
	TriggerBusy
)

type SkipReason string

const (
	SkipBusy   SkipReason = "busy"
	SkipWindow SkipReason = "window"
)

const maxPendingTriggers = 32

type policyEntry struct {
	id      string
	run     func(context.Context) error
	pending atomic.Bool
}

type jobEntry struct {
	name    string
	run     func(context.Context) error
	running atomic.Bool
}

func New(cfg config.ScheduleConfig, job Runnable, logger *slog.Logger) (*Scheduler, error) {
	specs := make([]window.Spec, len(cfg.Windows))
	for i, w := range cfg.Windows {
		specs[i] = window.Spec{Days: w.Days, Times: w.Times}
	}
	ws, err := window.Compile(cfg.Timezone, specs)
	if err != nil {
		return nil, fmt.Errorf("schedule windows: %w", err)
	}
	s := &Scheduler{
		cfg: cfg, job: job, logger: logger, stopCh: make(chan struct{}),
		runSem: make(chan struct{}, 1), windows: ws, onTick: func() {}, onSkip: func(SkipReason) {}, pending: map[string]bool{},
		loc: ws.Location(), runOnStart: cfg.ShouldRunOnStart(false),
	}
	if cfg.Cron != "" {
		sched, err := config.ParseCron(cfg.CronSpec())
		if err != nil {
			return nil, fmt.Errorf("invalid cron %q: %w", cfg.Cron, err)
		}
		s.main = sched
		s.cronInstance().Schedule(sched, cron.FuncJob(func() {
			if s.tick(s.currentCtx()) {
				s.logNextRun()
			}
		}))
	}
	return s, nil
}

func (s *Scheduler) cronInstance() *cron.Cron {
	if s.cron == nil {
		s.cron = cron.New(cron.WithParser(config.CronParser{}), cron.WithLocation(s.loc), cron.WithLogger(cron.DiscardLogger))
	}
	return s.cron
}

func (s *Scheduler) SetRunOnStart(on bool) { s.runOnStart = on }

func (s *Scheduler) AddPolicy(policyID, spec string, run func(context.Context) error) error {
	sched, err := config.ParseCron(spec)
	if err != nil {
		return fmt.Errorf("policy %q schedule %q: %w", policyID, spec, err)
	}
	p := &policyEntry{id: policyID, run: run}
	s.cronInstance().Schedule(sched, cron.FuncJob(func() {
		if s.policyTick(s.currentCtx(), p) {
			s.logger.Info("next policy run", "policy", policyID, "next_run", s.next(sched))
		}
	}))
	s.entries = append(s.entries, cronEntry{kind: "policy", name: policyID, spec: spec, sched: sched})
	s.policies++
	return nil
}

func (s *Scheduler) AddJob(name, spec string, run func(context.Context) error) error {
	sched, err := config.ParseCron(spec)
	if err != nil {
		return fmt.Errorf("job %q cron %q: %w", name, spec, err)
	}
	j := &jobEntry{name: name, run: run}
	s.cronInstance().Schedule(sched, cron.FuncJob(func() {
		if s.jobTick(s.currentCtx(), j) {
			s.logger.Debug("next job run", "job", name, "next_run", s.next(sched))
		}
	}))
	s.entries = append(s.entries, cronEntry{kind: "job", name: name, spec: spec, sched: sched})
	s.jobs++
	return nil
}

func (s *Scheduler) Start(ctx context.Context) {
	s.execCtx, s.execCancel = context.WithCancel(context.WithoutCancel(ctx))
	if s.cron != nil {
		s.cron.Start()
	}
	for _, e := range s.entries {
		s.logger.Info("cron schedule registered", "kind", e.kind, "name", e.name, "spec", e.spec,
			"timezone", s.zone(e.sched).String(), "next_run", s.next(e.sched))
	}
	if s.cfg.Cron != "" {
		next := s.next(s.main)
		if s.runOnStart {
			next = time.Now()
		}
		s.logger.Info("scheduler started", "mode", "cron", "spec", s.cfg.Cron,
			"timezone", s.zone(s.main).String(), "run_on_start", s.runOnStart, "next_run", next,
			"policy_schedules", s.policies, "jobs", s.jobs)
		s.started.Store(true)
		if s.runOnStart {
			go s.startupRun(s.execCtx)
		}
		return
	}
	interval := s.cfg.Interval.Std()
	if interval <= 0 {
		interval = config.DefaultInterval
	}
	s.nextInterval.Store(time.Now().Add(interval).UnixNano())
	next := s.NextRun()
	if s.runOnStart {
		next = time.Now()
	}
	s.logger.Info("scheduler started", "mode", "interval", "interval", interval,
		"timezone", s.loc.String(), "run_on_start", s.runOnStart, "next_run", next,
		"policy_schedules", s.policies, "jobs", s.jobs)
	s.started.Store(true)
	go s.runInterval(s.execCtx, interval, s.runOnStart)
}

func (s *Scheduler) Started() bool { return s.started.Load() }

func (s *Scheduler) NextRun() time.Time {
	if !s.started.Load() {
		return time.Time{}
	}
	if s.main != nil {
		return s.next(s.main)
	}
	return time.Unix(0, s.nextInterval.Load())
}

func (s *Scheduler) next(sched cron.Schedule) time.Time {
	return sched.Next(time.Now().In(s.zone(sched)))
}

func (s *Scheduler) zone(sched cron.Schedule) *time.Location {
	if spec, ok := sched.(*cron.SpecSchedule); ok && spec.Location != time.Local {
		return spec.Location
	}
	return s.loc
}

func (s *Scheduler) logNextRun() {
	s.logger.Info("next run", "next_run", s.NextRun())
}

func (s *Scheduler) startupRun(ctx context.Context) {
	if !s.windows.Allows(time.Now()) {
		s.logger.Info("startup run skipped: outside the maintenance window", "next_run", s.NextRun())
		s.onSkip(SkipWindow)
		return
	}
	if s.tick(ctx) {
		s.logNextRun()
	}
}

func (s *Scheduler) runInterval(ctx context.Context, interval time.Duration, runNow bool) {
	t := time.NewTimer(interval)
	defer t.Stop()
	if runNow {
		s.startupRun(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			ran := s.tick(ctx)
			t.Reset(interval)
			s.nextInterval.Store(time.Now().Add(interval).UnixNano())
			if ran {
				s.logNextRun()
			}
		}
	}
}

func (s *Scheduler) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.tickWG.Add(1)
	return true
}

func (s *Scheduler) tick(ctx context.Context) bool {
	if !s.windows.Allows(time.Now()) {
		s.logger.Debug("tick suppressed: outside maintenance window")
		s.onSkip(SkipWindow)
		return false
	}
	if !s.begin() {
		return false
	}
	defer s.tickWG.Done()
	select {
	case s.runSem <- struct{}{}:
	default:
		s.logger.Warn("previous run still in progress, skipping tick")
		s.onSkip(SkipBusy)
		return false
	}
	defer func() { <-s.runSem }()
	if err := s.job.Run(ctx); err != nil {
		s.logger.Error("run failed", "err", err)
	}
	s.onTick()
	return true
}

func (s *Scheduler) policyTick(ctx context.Context, p *policyEntry) bool {
	if !s.windows.Allows(time.Now()) {
		s.logger.Info("policy schedule suppressed: outside the global maintenance window", "policy", p.id)
		return false
	}
	if !p.pending.CompareAndSwap(false, true) {
		s.logger.Warn("policy run already pending, skipping tick", "policy", p.id)
		return false
	}
	defer p.pending.Store(false)
	if !s.begin() {
		return false
	}
	defer s.tickWG.Done()
	select {
	case s.runSem <- struct{}{}:
	case <-s.stopCh:
		return false
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.runSem }()
	select {
	case <-s.stopCh:
		return false
	default:
	}
	if !s.windows.Allows(time.Now()) {
		s.logger.Info("policy schedule suppressed: maintenance window closed while waiting", "policy", p.id)
		return false
	}
	if err := p.run(ctx); err != nil {
		s.logger.Error("policy run failed", "policy", p.id, "err", err)
	}
	s.onTick()
	return true
}

func (s *Scheduler) Trigger(key string, run func(context.Context) error) TriggerResult {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return TriggerStopped
	}
	if s.pending[key] {
		s.mu.Unlock()
		return TriggerDuplicate
	}
	if len(s.pending) >= maxPendingTriggers {
		s.mu.Unlock()
		return TriggerBusy
	}
	s.pending[key] = true
	s.tickWG.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.tickWG.Done()
		started := false
		defer func() {
			if !started {
				s.mu.Lock()
				delete(s.pending, key)
				s.mu.Unlock()
			}
		}()
		select {
		case s.runSem <- struct{}{}:
		case <-s.stopCh:
			return
		}
		defer func() { <-s.runSem }()
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		started = true
		select {
		case <-s.stopCh:
			return
		default:
		}
		if err := run(s.currentCtx()); err != nil {
			s.logger.Error("triggered run failed", "trigger", key, "err", err)
		}
		s.onTick()
	}()
	return TriggerQueued
}

func (s *Scheduler) jobTick(ctx context.Context, j *jobEntry) bool {
	if !j.running.CompareAndSwap(false, true) {
		s.logger.Warn("previous job execution still in progress, skipping tick", "job", j.name)
		return false
	}
	defer j.running.Store(false)
	if !s.begin() {
		return false
	}
	defer s.tickWG.Done()
	if err := j.run(ctx); err != nil {
		s.logger.Error("job failed", "job", j.name, "err", err)
	}
	return true
}

func (s *Scheduler) currentCtx() context.Context {
	if s.execCtx != nil {
		return s.execCtx
	}
	return context.Background()
}

func (s *Scheduler) OnTick(fn func()) {
	if fn == nil {
		return
	}
	s.onTick = fn
}

func (s *Scheduler) OnSkip(fn func(SkipReason)) {
	if fn == nil {
		return
	}
	s.onSkip = fn
}

func (s *Scheduler) Stop(drain time.Duration) {
	s.started.Store(false)
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
	s.stopOnce.Do(func() { close(s.stopCh) })

	done := make(chan struct{})
	go func() {
		s.tickWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		if s.execCancel != nil {
			s.execCancel()
		}
		return
	case <-time.After(drain):
		s.logger.Warn("shutdown: drain deadline reached, aborting in-flight run", "drain", drain)
	}
	if s.execCancel != nil {
		s.execCancel()
	}
	const recoveryGrace = 3 * time.Minute
	select {
	case <-done:
	case <-time.After(recoveryGrace):
		s.logger.Error("shutdown: in-flight rollback did not finish; containers may be left under temp names (recovered automatically on next run)")
	}
}
