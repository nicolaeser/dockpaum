package updater

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func (h *fakeHost) restart(st *state.Store, instance string) {
	h.t.Helper()
	d := h.u.deps
	d.State, d.Instance = st, instance
	u, err := New(d)
	if err != nil {
		h.t.Fatal(err)
	}
	u.selfID, u.selfChecked, u.selfUnknown, u.selfCands = "", true, false, nil
	u.healthGraceCap, u.healthPoll = h.u.healthGraceCap, h.u.healthPoll
	u.reg = h.reg
	h.u = u
}

func durableState(t *testing.T, path string) *state.Store {
	t.Helper()
	st := state.OpenWith(path, state.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), FlushDelay: -1,
	})
	if !st.Durable() {
		t.Fatal("test state must be durable")
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func (h *fakeHost) status(name string) ContainerStatus {
	h.t.Helper()
	s, ok := h.u.ContainerStatusOf("fake", name)
	if !ok {
		h.t.Fatalf("no status for %s (have %+v)", name, h.u.Containers())
	}
	return s
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestImageDefaultsSurviveARestart(t *testing.T) {
	tests := []struct {
		name    string
		durable bool
		wantCmd []string
	}{
		{name: "durable state", durable: true, wantCmd: []string{"serve", "--new"}},
		{name: "in-memory state", wantCmd: []string{"serve", "--old"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", nil)
			path := state.PathIn(t.TempDir())
			var st *state.Store
			if tc.durable {
				st = durableState(t, path)
			}
			h.restart(st, "")
			old := dockertest.Image{ID: "v1", Config: &container.Config{Cmd: []string{"serve", "--old"}, Env: []string{"MODE=old"}}}
			h.d.AddImage(old, appRef)
			h.d.Publish(appRef, old)
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}

			h.d.DropImage("v1")
			h.d.Publish(appRef, dockertest.Image{ID: "v2", Config: &container.Config{Cmd: []string{"serve", "--new"}, Env: []string{"MODE=new"}}})
			var next *state.Store
			if tc.durable {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				next = durableState(t, path)
			}
			h.restart(next, "")
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			ct := h.inspect("web")
			if ct.Image != "sha256:v2" {
				t.Fatalf("not updated: %s", ct.Image)
			}
			if !reflect.DeepEqual(ct.Config.Cmd, tc.wantCmd) {
				t.Fatalf("cmd = %q, want %q", ct.Config.Cmd, tc.wantCmd)
			}
		})
	}
}

func TestContainerStatus(t *testing.T) {
	created := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	oldPulledAs := "example.com/app@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name     string
		mutate   func(*config.Config)
		labels   map[string]string
		publish  *dockertest.Image
		decision audit.Decision
		update   string
		pending  bool
		check    func(t *testing.T, s ContainerStatus)
	}{
		{name: "up to date", publish: &dockertest.Image{ID: "v1"}, decision: audit.DecisionSkipped, update: UpdateNone},
		{name: "monitor-only", labels: map[string]string{"dockpaum.monitor-only": "true"}, publish: &dockertest.Image{ID: "v2"},
			decision: audit.DecisionMonitorOnly, update: UpdateAvailable, pending: true},
		{name: "dry-run", mutate: func(c *config.Config) { c.DryRun = true }, publish: &dockertest.Image{ID: "v2"},
			decision: audit.DecisionDryRun, update: UpdateAvailable, pending: true},
		{name: "soaking", mutate: func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) },
			publish: &dockertest.Image{ID: "v2", Created: created.Format(time.RFC3339)}, decision: audit.DecisionGatedAge,
			update: UpdateAvailable, pending: true, check: func(t *testing.T, s ContainerStatus) {
				if !s.EligibleAt.Equal(created.Add(time.Hour)) {
					t.Errorf("eligible at %v, want %v", s.EligibleAt, created.Add(time.Hour))
				}
				if s.Candidate.FirstSeen.IsZero() {
					t.Error("the first sighting of a soaking candidate must be recorded")
				}
			}},
		{name: "updated", publish: &dockertest.Image{ID: "v2"}, decision: audit.DecisionUpdated, update: UpdateNone,
			check: func(t *testing.T, s ContainerStatus) {
				if s.Current.Digest != "sha256:v2" || s.Candidate != nil {
					t.Errorf("current %+v candidate %+v", s.Current, s.Candidate)
				}
			}},
		{name: "rolled back", publish: &dockertest.Image{ID: "v2", CrashLoop: true}, decision: audit.DecisionRolledBack,
			update: UpdateAvailable, pending: true, check: func(t *testing.T, s ContainerStatus) {
				if s.Error == "" || s.Current.Digest != "sha256:v1" {
					t.Errorf("error %q current %+v", s.Error, s.Current)
				}
			}},
		{name: "check failed", decision: audit.DecisionCheckFailed, update: UpdateUnknown,
			check: func(t *testing.T, s ContainerStatus) {
				if s.Error == "" {
					t.Error("the check error must be kept")
				}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", tc.mutate)
			if tc.publish != nil {
				h.release(appRef, dockertest.Image{ID: "v1"}, *tc.publish)
			} else {
				h.d.AddImage(dockertest.Image{ID: "v1", RepoDigests: []string{oldPulledAs}}, appRef)
			}
			id := h.d.Run("web", &container.Config{Image: appRef, Labels: tc.labels}, nil, nil)
			_ = h.run()
			s := h.status("web")
			rec := h.recordFor("web")
			if s.Decision != string(tc.decision) || s.Update != tc.update || s.Pending != tc.pending {
				t.Fatalf("status = %+v, want decision %s update %s pending %v", s, tc.decision, tc.update, tc.pending)
			}
			if s.Host != "fake" || s.Image != appRef || s.Policy != "default" || s.Current.Tag != "1" || s.RunID != rec.RunID || s.CheckedAt.IsZero() {
				t.Fatalf("status = %+v", s)
			}
			if tc.decision != audit.DecisionUpdated && s.ContainerID != id {
				t.Fatalf("container id = %s, want %s", s.ContainerID, id)
			}
			if s.PolicySource != "default policy" || s.Mode != config.ModeTagFollow {
				t.Fatalf("policy source %q mode %q", s.PolicySource, s.Mode)
			}
			oldRepoDigest := registry.DigestReference(appRef, fakeDigest(appRef, "v1"))
			newRepoDigest := registry.DigestReference(appRef, fakeDigest(appRef, "v2"))
			switch {
			case tc.publish == nil:
				if s.Check != CheckPull || !strings.Contains(s.CheckNote, "404") || s.Current.RepoDigest != oldPulledAs {
					t.Fatalf("check %q note %q current %+v", s.Check, s.CheckNote, s.Current)
				}
			case tc.decision == audit.DecisionUpdated:
				if s.Check != CheckRegistry || s.Current.RepoDigest != newRepoDigest {
					t.Fatalf("check %q current %+v", s.Check, s.Current)
				}
			default:
				if s.Check != CheckRegistry || s.CheckNote != "" || s.Current.RepoDigest != oldRepoDigest || s.Current.Version != "1" {
					t.Fatalf("check %q note %q current %+v", s.Check, s.CheckNote, s.Current)
				}
			}
			if tc.pending {
				if s.Candidate == nil || s.Candidate.Digest != "sha256:v2" || s.Candidate.Image != appRef || s.Candidate.FoundAt.IsZero() ||
					s.Candidate.RepoDigest != newRepoDigest || s.Candidate.Version != "1" || s.Candidate.Created.IsZero() {
					t.Fatalf("candidate = %+v", s.Candidate)
				}
			} else if s.Candidate != nil {
				t.Fatalf("unexpected candidate %+v", s.Candidate)
			}
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func TestStatusKeepsTheCandidateWhileGatedBeforeTheCheck(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		nightlyWindow(c)
		c.Update.Policies[0].MinImageAge = config.Duration(time.Hour)
	})
	created := inWindow.Add(-10 * time.Minute)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: created.Format(time.RFC3339)})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.u.now = fixedClock(inWindow)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	first := h.status("web")
	if first.Decision != string(audit.DecisionGatedAge) || !first.Pending {
		t.Fatalf("first run: %+v", first)
	}

	h.u.now = fixedClock(outWindow)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	s := h.status("web")
	if s.Decision != string(audit.DecisionGatedWindow) {
		t.Fatalf("decision = %s", s.Decision)
	}
	if s.Update != UpdateAvailable || !s.Pending || s.Candidate == nil || s.Candidate.Digest != "sha256:v2" {
		t.Fatalf("the candidate found earlier must be kept: %+v", s)
	}
	if !s.Candidate.FoundAt.Equal(first.Candidate.FoundAt) || !s.EligibleAt.Equal(first.EligibleAt) {
		t.Fatalf("carried candidate changed: %+v vs %+v", s.Candidate, first.Candidate)
	}
	if !s.CheckedAt.After(first.CheckedAt) && !s.CheckedAt.Equal(first.CheckedAt) {
		t.Fatal("checked_at must advance")
	}
}

func TestStatusIsUnknownWithoutACheck(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.policy": "none"}}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if s := h.status("web"); s.Update != UpdateUnknown || s.Pending || s.Decision != string(audit.DecisionGatedPolicy) {
		t.Fatalf("status = %+v", s)
	}
}

func TestStatusesOfContainersNoLongerManagedAreDropped(t *testing.T) {
	path := state.PathIn(t.TempDir())
	h := newFakeHost(t, "1.47", nil)
	st := durableState(t, path)
	h.restart(st, "")
	for _, name := range []string{"web", "db", "cache"} {
		ref := "example.com/" + name + ":1"
		h.d.AddImage(dockertest.Image{ID: name + "-v1"}, ref)
		h.d.Publish(ref, dockertest.Image{ID: name + "-v1"})
		h.d.Run(name, &container.Config{Image: ref}, nil, nil)
	}
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.u.Containers()); n != 3 {
		t.Fatalf("statuses = %d", n)
	}
	h.u.deps.Config.Filters.ExcludeNames = []string{"db"}
	h.d.Stop("cache")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	got := h.u.Containers()
	if len(got) != 1 || got[0].Container != "web" {
		t.Fatalf("statuses = %+v, want only web", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if stored := durableState(t, path).ContainerStatuses(); len(stored) != 1 || stored[0].Container != "web" {
		t.Fatalf("stored statuses = %+v", stored)
	}
}

func TestStatusSurvivesARestart(t *testing.T) {
	path := state.PathIn(t.TempDir())
	h := newFakeHost(t, "1.47", nil)
	st := durableState(t, path)
	h.restart(st, "")
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	before := h.status("web")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	h.restart(durableState(t, path), "")
	after, ok := h.u.ContainerStatusOf("fake", "web")
	if !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("status after restart = %+v (%v), want %+v", after, ok, before)
	}
	if got := h.u.Status("test").PendingUpdates; got != 1 {
		t.Fatalf("pending updates = %d", got)
	}
}

func TestStatusReadsDuringARun(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.Update.Strategy = config.StrategyParallel
		c.Update.Concurrency = 4
	})
	for i := 0; i < 8; i++ {
		name := "app" + string(rune('a'+i))
		ref := "example.com/" + name + ":1"
		h.release(ref, dockertest.Image{ID: name + "-v1"}, dockertest.Image{ID: name + "-v2"})
		labels := map[string]string{}
		if i%2 == 0 {
			labels["dockpaum.monitor-only"] = "true"
		}
		h.d.Run(name, &container.Config{Image: ref, Labels: labels}, nil, nil)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for _, s := range h.u.Containers() {
					if s.Candidate != nil {
						s.Candidate.Tag = "mutated"
					}
				}
				_ = h.u.Status("test")
				_, _ = h.u.ContainerStatusOf("fake", "appa")
			}
		}()
	}
	for i := 0; i < 2; i++ {
		_ = h.run()
	}
	stop.Store(true)
	wg.Wait()
	got := h.u.Containers()
	if len(got) != 8 {
		t.Fatalf("statuses = %d", len(got))
	}
	pending := 0
	for _, s := range got {
		if s.Pending {
			pending++
			if s.Candidate.Tag == "mutated" {
				t.Fatal("Containers must return copies")
			}
		}
	}
	if pending != 4 || h.u.Status("test").PendingUpdates != 4 {
		t.Fatalf("pending = %d / %d, want the 4 monitor-only containers", pending, h.u.Status("test").PendingUpdates)
	}
}

func TestStatusDocument(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.DryRun = true
		c.Hooks.PreRun = []string{"sleep 0.3"}
	})
	h.restart(nil, "edge-1")
	next := time.Date(2026, time.May, 1, 4, 0, 0, 0, time.UTC)
	h.u.SetNextRun(func() time.Time { return next })
	st := h.u.Status("1.2.3")
	if !st.FirstRunPending || st.Running || !st.DryRun || st.Instance != "edge-1" || st.NextRunAt == nil || !st.NextRunAt.Equal(next) {
		t.Fatalf("status before the first run = %+v", st)
	}
	h.u.SetNextRun(func() time.Time { return time.Time{} })
	if st := h.u.Status("1.2.3"); st.NextRunAt != nil {
		t.Fatal("an unknown next run must be omitted")
	}

	h.u.deps.Config.DryRun = false
	done := make(chan error, 1)
	go func() { done <- h.run() }()
	deadline := time.Now().Add(5 * time.Second)
	for !h.u.Status("1.2.3").Running {
		if time.Now().After(deadline) {
			t.Fatal("a run in progress was never reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := h.u.Status("1.2.3"); st.Running || st.FirstRunPending || st.LastRun == nil {
		t.Fatalf("status after the run = %+v", st)
	}
}

type recordedEvents struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordedEvents) send(_ context.Context, ev notify.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func TestEventsCarryTheRunAndTheInstance(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.restart(nil, "edge-1")
	rec := &recordedEvents{}
	h.u.send = rec.send
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", CrashLoop: true})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.release("example.com/watch:1", dockertest.Image{ID: "w1"}, dockertest.Image{ID: "w2"})
	h.d.Run("watch", &container.Config{Image: "example.com/watch:1", Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	if err := h.run(); err == nil {
		t.Fatal("the rolled-back update must fail the run")
	}
	runID := h.recordFor("web").RunID
	types := map[string]bool{}
	for _, ev := range rec.events {
		types[ev.Type] = true
		if ev.RunID != runID || ev.Instance != "edge-1" {
			t.Errorf("%s: run %q instance %q, want %q edge-1", ev.Type, ev.RunID, ev.Instance, runID)
		}
		if ev.Type == notify.EventUpdateFailure && (ev.OldDigest != "sha256:v1" || ev.NewDigest != "sha256:v2") {
			t.Errorf("failure event digests = %q %q", ev.OldDigest, ev.NewDigest)
		}
	}
	for _, want := range []string{notify.EventRunStarted, notify.EventUpdateFailure, notify.EventRolledBack, notify.EventAvailable, notify.EventRunCompleted} {
		if !types[want] {
			t.Errorf("no %s event (got %v)", want, types)
		}
	}
}

func TestDefaultInstance(t *testing.T) {
	want, err := os.Hostname()
	if err != nil || want == "" {
		want = "dockpaum"
	}
	if got := DefaultInstance(); got != want {
		t.Fatalf("DefaultInstance() = %q, want %q", got, want)
	}
	h := newFakeHost(t, "1.47", nil)
	if h.u.Instance() != want {
		t.Fatalf("instance without configuration = %q", h.u.Instance())
	}
}

func TestPreviousImageIsRecorded(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1", RepoDigests: []string{"example.com/app@sha256:aaa"}}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	p, ok := h.u.state.Previous("fake", "web")
	if !ok {
		t.Fatal("no previous image recorded")
	}
	want := state.PreviousImage{
		Image: appRef, ImageID: "sha256:v1",
		RepoDigests: []string{"example.com/app@sha256:aaa", registry.DigestReference(appRef, fakeDigest(appRef, "v1"))},
		NewImage:    appRef, NewImageID: "sha256:v2", ContainerID: h.inspect("web").ID,
		RunID: h.recordFor("web").RunID, UpdatedAt: p.UpdatedAt,
	}
	if !reflect.DeepEqual(p, want) || p.UpdatedAt.IsZero() {
		t.Fatalf("previous = %+v\nwant %+v", p, want)
	}
}

func TestSoakIsMeasuredFromTheFirstSightingWhenTheBuildTimeIsUnusable(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name    string
		restart bool
		want    audit.Decision
	}{
		{name: "same process", want: audit.DecisionUpdated},
		{name: "restart", restart: true, want: audit.DecisionUpdated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := state.PathIn(t.TempDir())
			h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) })
			st := durableState(t, path)
			h.restart(st, "")
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: "1970-01-01T00:00:00Z"})
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			h.u.now = fixedClock(t0)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			first := h.recordFor("web")
			if first.Decision != audit.DecisionGatedAge || !strings.Contains(first.Reason, "implausible creation time") || !strings.Contains(first.Reason, "first saw it") {
				t.Fatalf("first run: %+v", first)
			}
			if s := h.status("web"); !s.EligibleAt.Equal(t0.Add(time.Hour)) {
				t.Fatalf("eligible at %v, want %v", s.EligibleAt, t0.Add(time.Hour))
			}

			if tc.restart {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				h.restart(durableState(t, path), "")
			}
			h.u.now = fixedClock(t0.Add(2 * time.Hour))
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			recs := h.records("web")
			if got := recs[len(recs)-1].Decision; got != tc.want {
				t.Fatalf("second run: %s (%s), want %s", got, recs[len(recs)-1].Reason, tc.want)
			}
		})
	}
}

func TestSoakSources(t *testing.T) {
	now := time.Date(2026, time.April, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		source   string
		durable  bool
		created  string
		seenAgo  time.Duration
		hold     string
		eligible time.Duration
	}{
		{name: "auto: old enough", created: now.Add(-2 * time.Hour).Format(time.RFC3339), eligible: -time.Hour},
		{name: "auto: too young", created: now.Add(-10 * time.Minute).Format(time.RFC3339), hold: "new image age 10m0s < min 1h0m0s", eligible: 50 * time.Minute},
		{name: "auto: build long ago, seen just now", created: now.Add(-48 * time.Hour).Format(time.RFC3339), durable: true},
		{name: "auto: epoch, first sighting", created: "1970-01-01T00:00:00Z", durable: true,
			hold: "implausible creation time 1970-01-01T00:00:00Z, as reproducible builds do; new image age 0s since dockpaum first saw it", eligible: time.Hour},
		{name: "auto: epoch, seen two hours ago", created: "1970-01-01T00:00:00Z", durable: true, seenAgo: 2 * time.Hour, eligible: -time.Hour},
		{name: "auto: unreadable, in memory", created: "bogus"},
		{name: "auto: epoch, in memory", created: "1970-01-01T00:00:00Z"},
		{name: "auto: future, in memory", created: now.Add(time.Hour).Format(time.RFC3339),
			hold: "creation time in the future (2026-04-01T11:00:00Z); new image age -1h0m0s < min 1h0m0s", eligible: 2 * time.Hour},
		{name: "auto: future, first sighting", created: now.Add(time.Hour).Format(time.RFC3339), durable: true,
			hold: "creation time in the future", eligible: time.Hour},
		{name: "first_seen: promoted old build waits", source: ageFirstSeen, durable: true, created: now.Add(-48 * time.Hour).Format(time.RFC3339),
			seenAgo: 10 * time.Minute, hold: "new image age 10m0s since dockpaum first saw it", eligible: 50 * time.Minute},
		{name: "first_seen: seen long enough", source: ageFirstSeen, durable: true, created: now.Add(-48 * time.Hour).Format(time.RFC3339),
			seenAgo: 2 * time.Hour, eligible: -time.Hour},
		{name: "first_seen: young build seen earlier", source: ageFirstSeen, durable: true, created: now.Add(-10 * time.Minute).Format(time.RFC3339),
			seenAgo: 2 * time.Hour, hold: "new image age 10m0s < min", eligible: 50 * time.Minute},
		{name: "first_seen without a durable state uses the build time", source: ageFirstSeen, created: now.Add(-48 * time.Hour).Format(time.RFC3339)},
		{name: "created: epoch is held", source: ageCreated, durable: true, created: "1970-01-01T00:00:00Z", seenAgo: 2 * time.Hour,
			hold: "its age cannot be measured, so it is held"},
		{name: "created: future", source: ageCreated, created: now.Add(time.Hour).Format(time.RFC3339), hold: "creation time in the future"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", nil)
			var st *state.Store
			if tc.durable {
				st = durableState(t, state.PathIn(t.TempDir()))
			}
			h.restart(st, "")
			h.u.now = fixedClock(now)
			h.d.AddImage(dockertest.Image{ID: "v2", Created: tc.created}, appRef)
			if tc.seenAgo > 0 {
				h.u.state.FirstSeen("fake", "sha256:v2", imageRepo(appRef), now.Add(-tc.seenAgo))
			}
			c := h.u.deps.Clients.List()[0]
			created, err := c.ImageCreated(context.Background(), appRef)
			got := h.u.soakHold("fake", appRef, "sha256:v2", created, err, time.Hour, tc.source)
			if tc.hold == "" && got.hold != "" {
				t.Fatalf("held: %s", got.hold)
			}
			if tc.hold != "" && !strings.Contains(got.hold, tc.hold) {
				t.Fatalf("hold = %q, want it to contain %q", got.hold, tc.hold)
			}
			if tc.eligible != 0 && !got.eligibleAt.Equal(now.Add(tc.eligible)) {
				t.Fatalf("eligible at %v, want %v", got.eligibleAt, now.Add(tc.eligible))
			}
			if got.firstSeen.IsZero() {
				t.Fatal("the sighting must be recorded")
			}
		})
	}
}
