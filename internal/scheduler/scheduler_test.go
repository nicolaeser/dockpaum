package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeJob struct{ ran int32 }

func (f *fakeJob) Run(context.Context) error { atomic.AddInt32(&f.ran, 1); return nil }

func TestNewRejectsBadCron(t *testing.T) {
	if _, err := New(config.ScheduleConfig{Cron: "not a cron"}, &fakeJob{}, discard()); err == nil {
		t.Fatal("expected error for invalid cron spec")
	}
}

func TestTickRunsInsideWindow(t *testing.T) {
	job := &fakeJob{}
	s, err := New(config.ScheduleConfig{
		Timezone: "UTC",
		Windows:  []config.WindowSpec{{Days: []string{"all"}, Times: []string{"00:00-00:00"}}},
	}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	var ticked int32
	s.OnTick(func() { atomic.AddInt32(&ticked, 1) })
	s.tick(context.Background())
	if atomic.LoadInt32(&job.ran) != 1 {
		t.Fatal("job should run inside the window")
	}
	if atomic.LoadInt32(&ticked) != 1 {
		t.Fatal("OnTick callback should fire after a tick")
	}
}

func TestTickSuppressedOutsideWindow(t *testing.T) {
	now := time.Now().UTC()
	h := (now.Hour() + 12) % 24
	win := fmt.Sprintf("%02d:00-%02d:30", h, h)
	job := &fakeJob{}
	s, err := New(config.ScheduleConfig{
		Timezone: "UTC",
		Windows:  []config.WindowSpec{{Days: []string{"all"}, Times: []string{win}}},
	}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	s.tick(context.Background())
	if atomic.LoadInt32(&job.ran) != 0 {
		t.Fatalf("job should be suppressed outside the window %q (now %s)", win, now.Format("15:04"))
	}
}

type blockingJob struct {
	started chan struct{}
	release chan struct{}
	ran     atomic.Int32
}

func newBlockingJob() *blockingJob {
	return &blockingJob{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingJob) Run(ctx context.Context) error {
	b.ran.Add(1)
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAddRejectsBadSpecs(t *testing.T) {
	s, err := New(config.ScheduleConfig{}, &fakeJob{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context) error { return nil }
	if err := s.AddPolicy("db", "0 3 * *", noop); err == nil {
		t.Error("AddPolicy accepted a four-field spec")
	}
	if err := s.AddJob("j", "nope", noop); err == nil {
		t.Error("AddJob accepted an invalid spec")
	}
	if err := s.AddJob("j", "CRON_TZ=Europe/Berlin", noop); err == nil {
		t.Error("AddJob accepted a zone prefix without a schedule")
	}
	for _, spec := range []string{"0 3 * * SUN", "0 0 3 * * SUN", "CRON_TZ=Europe/Berlin 0 0 3 * * SUN", "CRON_TZ=Europe/Berlin 0 3 * * SUN", "@every 1d"} {
		if err := s.AddPolicy("db", spec, noop); err != nil {
			t.Errorf("AddPolicy(%q): %v", spec, err)
		}
	}
}

func TestRegularTickSkipsWhileRunInProgress(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	go s.tick(context.Background())
	<-job.started
	s.tick(context.Background())
	close(job.release)
	waitFor(t, "first run to finish", func() bool { return len(s.runSem) == 0 })
	if got := job.ran.Load(); got != 1 {
		t.Fatalf("job ran %d times, want 1 (overlapping tick skipped)", got)
	}
}

func TestPolicyTickWaitsForInFlightRun(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	var policyRuns atomic.Int32
	p := &policyEntry{id: "database", run: func(context.Context) error { policyRuns.Add(1); return nil }}

	go s.tick(context.Background())
	<-job.started
	done := make(chan struct{})
	go func() { s.policyTick(context.Background(), p); close(done) }()
	waitFor(t, "policy tick to wait", p.pending.Load)
	s.policyTick(context.Background(), p)
	if policyRuns.Load() != 0 {
		t.Fatal("policy run must not overlap the in-flight run")
	}
	close(job.release)
	<-done
	if got := policyRuns.Load(); got != 1 {
		t.Fatalf("policy ran %d times, want 1", got)
	}
}

func TestStopAbortsWaitingPolicyTick(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	s.Start(context.Background())
	<-job.started
	var policyRuns atomic.Int32
	p := &policyEntry{id: "database", run: func(context.Context) error { policyRuns.Add(1); return nil }}
	done := make(chan struct{})
	go func() { s.policyTick(s.currentCtx(), p); close(done) }()
	waitFor(t, "policy tick to wait", p.pending.Load)

	stopped := make(chan struct{})
	go func() { s.Stop(50 * time.Millisecond); close(stopped) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting policy tick did not return on Stop")
	}
	<-stopped
	if policyRuns.Load() != 0 {
		t.Fatal("policy run must not start after Stop")
	}
}

func TestPolicyTickSuppressedOutsideGlobalWindow(t *testing.T) {
	now := time.Now().UTC()
	h := (now.Hour() + 12) % 24
	s, err := New(config.ScheduleConfig{
		Timezone: "UTC",
		Windows:  []config.WindowSpec{{Days: []string{"all"}, Times: []string{fmt.Sprintf("%02d:00-%02d:30", h, h)}}},
	}, &fakeJob{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	s.policyTick(context.Background(), &policyEntry{id: "db", run: func(context.Context) error { ran.Add(1); return nil }})
	if ran.Load() != 0 {
		t.Fatal("policy schedule must respect the global maintenance window")
	}
}

func TestJobTickIndependentOfUpdateRunsButNotOfItself(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	go s.tick(context.Background())
	<-job.started

	release := make(chan struct{})
	var jobRuns atomic.Int32
	j := &jobEntry{name: "backup", run: func(context.Context) error {
		jobRuns.Add(1)
		<-release
		return nil
	}}
	done := make(chan struct{})
	go func() { s.jobTick(context.Background(), j); close(done) }()
	waitFor(t, "job to start during the update run", func() bool { return jobRuns.Load() == 1 })
	s.jobTick(context.Background(), j)
	close(release)
	<-done
	close(job.release)
	if got := jobRuns.Load(); got != 1 {
		t.Fatalf("job ran %d times, want 1 (overlapping execution skipped)", got)
	}
}

func TestCronEntriesFireInIntervalMode(t *testing.T) {
	s, err := New(config.ScheduleConfig{Interval: config.Duration(time.Hour)}, &fakeJob{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	var policyRuns, jobRuns atomic.Int32
	if err := s.AddPolicy("db", "* * * * * *", func(context.Context) error { policyRuns.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob("j", "* * * * * *", func(context.Context) error { jobRuns.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Start(context.Background())
	defer s.Stop(time.Second)
	waitFor(t, "policy schedule and job to fire", func() bool { return policyRuns.Load() > 0 && jobRuns.Load() > 0 })
}

func TestTriggerQueuesCoalescesAndWaits(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	go s.tick(context.Background())
	<-job.started
	var runs atomic.Int32
	done := make(chan struct{}, 2)
	run := func(context.Context) error { runs.Add(1); done <- struct{}{}; return nil }
	if got := s.Trigger("web", run); got != TriggerQueued {
		t.Fatalf("first trigger = %v", got)
	}
	if got := s.Trigger("web", run); got != TriggerDuplicate {
		t.Fatalf("identical pending trigger = %v, want duplicate", got)
	}
	if got := s.Trigger("db", run); got != TriggerQueued {
		t.Fatalf("different trigger = %v", got)
	}
	if runs.Load() != 0 {
		t.Fatal("triggered runs must wait for the in-flight run")
	}
	close(job.release)
	<-done
	<-done
	if runs.Load() != 2 {
		t.Fatalf("runs = %d, want 2", runs.Load())
	}
	s.Stop(time.Second)
	if got := s.Trigger("web", run); got != TriggerStopped {
		t.Fatalf("trigger after Stop = %v", got)
	}
}

func TestTriggerQueueIsBounded(t *testing.T) {
	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	go s.tick(context.Background())
	<-job.started
	noop := func(context.Context) error { return nil }
	for i := 0; i < maxPendingTriggers; i++ {
		if got := s.Trigger(fmt.Sprintf("k%d", i), noop); got != TriggerQueued {
			t.Fatalf("trigger %d = %v", i, got)
		}
	}
	if got := s.Trigger("one-too-many", noop); got != TriggerBusy {
		t.Fatalf("trigger beyond the bound = %v, want busy", got)
	}
	close(job.release)
	s.Stop(5 * time.Second)
}

const neverSoon = "0 0 1 1 *"

func TestStartedDoesNotWaitForARun(t *testing.T) {
	now := time.Now().UTC()
	h := (now.Hour() + 12) % 24
	tests := []struct {
		name string
		cfg  config.ScheduleConfig
	}{
		{name: "cron mode before the first slot", cfg: config.ScheduleConfig{Cron: neverSoon}},
		{name: "interval mode outside the window", cfg: config.ScheduleConfig{
			Interval: config.Duration(time.Hour), Timezone: "UTC",
			Windows: []config.WindowSpec{{Days: []string{"all"}, Times: []string{fmt.Sprintf("%02d:00-%02d:30", h, h)}}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &fakeJob{}
			s, err := New(tt.cfg, job, discard())
			if err != nil {
				t.Fatal(err)
			}
			if s.Started() {
				t.Fatal("Started() before Start")
			}
			s.Start(context.Background())
			if !s.Started() {
				t.Fatal("Started() = false after Start")
			}
			if atomic.LoadInt32(&job.ran) != 0 {
				t.Fatal("no run may happen in this setup")
			}
			s.Stop(time.Second)
			if s.Started() {
				t.Error("Started() = true after Stop")
			}
		})
	}
}

func TestRunOnStart(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name     string
		cfg      config.ScheduleConfig
		override *bool
		want     bool
	}{
		{name: "interval mode runs at start", cfg: config.ScheduleConfig{Interval: config.Duration(time.Hour)}, want: true},
		{name: "cron mode waits for its slot", cfg: config.ScheduleConfig{Cron: neverSoon}, want: false},
		{name: "cron mode with run_on_start", cfg: config.ScheduleConfig{Cron: neverSoon, RunOnStart: &on}, want: true},
		{name: "cron mode switched on by the daemon (dry-run)", cfg: config.ScheduleConfig{Cron: neverSoon}, override: &on, want: true},
		{name: "interval mode with run_on_start false", cfg: config.ScheduleConfig{Interval: config.Duration(time.Hour), RunOnStart: &off}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &fakeJob{}
			s, err := New(tt.cfg, job, discard())
			if err != nil {
				t.Fatal(err)
			}
			if tt.override != nil {
				s.SetRunOnStart(*tt.override)
			}
			if s.runOnStart != tt.want {
				t.Fatalf("runOnStart = %v, want %v", s.runOnStart, tt.want)
			}
			s.Start(context.Background())
			defer s.Stop(time.Second)
			if tt.want {
				waitFor(t, "the startup run", func() bool { return atomic.LoadInt32(&job.ran) == 1 })
			}
		})
	}
}

func TestStartupRunHonorsTheWindow(t *testing.T) {
	now := time.Now().UTC()
	h := (now.Hour() + 12) % 24
	on := true
	job := &fakeJob{}
	s, err := New(config.ScheduleConfig{
		Cron: neverSoon, RunOnStart: &on, Timezone: "UTC",
		Windows: []config.WindowSpec{{Days: []string{"all"}, Times: []string{fmt.Sprintf("%02d:00-%02d:30", h, h)}}},
	}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	s.startupRun(context.Background())
	if atomic.LoadInt32(&job.ran) != 0 {
		t.Fatal("the startup run must respect the global maintenance window")
	}
}

func TestNextRun(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("cron mode, five fields in the schedule zone", func(t *testing.T) {
		s, err := New(config.ScheduleConfig{Cron: "0 4 * * *", Timezone: "Europe/Berlin"}, &fakeJob{}, discard())
		if err != nil {
			t.Fatal(err)
		}
		if !s.NextRun().IsZero() {
			t.Fatal("NextRun() before Start must be zero")
		}
		s.Start(context.Background())
		defer s.Stop(time.Second)
		next := s.NextRun().In(berlin)
		if next.Hour() != 4 || next.Minute() != 0 || next.Second() != 0 {
			t.Errorf("NextRun() = %v, want 04:00:00 Berlin", next)
		}
		if d := time.Until(next); d <= 0 || d > 25*time.Hour {
			t.Errorf("NextRun() = %v is not the next slot", next)
		}
	})
	t.Run("interval mode uses the default interval", func(t *testing.T) {
		s, err := New(config.ScheduleConfig{}, &fakeJob{}, discard())
		if err != nil {
			t.Fatal(err)
		}
		s.Start(context.Background())
		defer s.Stop(time.Second)
		if d := time.Until(s.NextRun()); d < config.DefaultInterval-time.Minute || d > config.DefaultInterval {
			t.Errorf("NextRun() is %v away, want about %v", d, config.DefaultInterval)
		}
	})
}

func TestSpecsWithoutZoneUseTheScheduleZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(config.ScheduleConfig{Timezone: "Asia/Tokyo"}, &fakeJob{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context) error { return nil }
	if err := s.AddJob("backup", "0 4 * * *", noop); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPolicy("db", "CRON_TZ=UTC 0 4 * * *", noop); err != nil {
		t.Fatal(err)
	}
	if got := s.cron.Location(); got.String() != tokyo.String() {
		t.Errorf("cron location = %v, want %v", got, tokyo)
	}
	job, pol := s.entries[0], s.entries[1]
	if z := s.zone(job.sched); z.String() != "Asia/Tokyo" {
		t.Errorf("job zone = %v, want the schedule zone", z)
	}
	if next := s.next(job.sched).In(tokyo); next.Hour() != 4 || next.Minute() != 0 {
		t.Errorf("job next = %v, want 04:00 Tokyo", next)
	}
	if z := s.zone(pol.sched); z.String() != "UTC" {
		t.Errorf("policy zone = %v, want its CRON_TZ prefix", z)
	}
	if next := s.next(pol.sched).UTC(); next.Hour() != 4 {
		t.Errorf("policy next = %v, want 04:00 UTC", next)
	}
}

func TestOnSkipReportsSkippedRegularTicks(t *testing.T) {
	var mu sync.Mutex
	var got []SkipReason
	record := func(r SkipReason) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	reasons := func() []SkipReason {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}

	job := newBlockingJob()
	s, err := New(config.ScheduleConfig{}, job, discard())
	if err != nil {
		t.Fatal(err)
	}
	s.OnSkip(nil)
	s.OnSkip(record)
	go s.tick(context.Background())
	<-job.started
	s.tick(context.Background())
	close(job.release)
	waitFor(t, "first run to finish", func() bool { return len(s.runSem) == 0 })
	s.tick(context.Background())
	if r := reasons(); !slices.Equal(r, []SkipReason{SkipBusy}) {
		t.Fatalf("reasons = %v, want [busy]", r)
	}

	h := (time.Now().UTC().Hour() + 12) % 24
	closed, err := New(config.ScheduleConfig{
		Timezone: "UTC",
		Windows:  []config.WindowSpec{{Days: []string{"all"}, Times: []string{fmt.Sprintf("%02d:00-%02d:30", h, h)}}},
	}, &fakeJob{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	closed.OnSkip(record)
	closed.tick(context.Background())
	closed.startupRun(context.Background())
	p := &policyEntry{id: "nightly", run: func(context.Context) error { return nil }}
	closed.policyTick(context.Background(), p)
	if r := reasons(); !slices.Equal(r, []SkipReason{SkipBusy, SkipWindow, SkipWindow}) {
		t.Fatalf("reasons = %v, want [busy window window]", r)
	}
}
