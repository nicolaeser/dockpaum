package updater

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
)

func TestCheckRunAppliesNothing(t *testing.T) {
	dir := t.TempDir()
	preRun, preUpdate := filepath.Join(dir, "pre-run"), filepath.Join(dir, "pre-update")
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.Hooks.PreRun = []string{"touch " + preRun}
		c.Hooks.PreUpdate = []string{"touch " + preUpdate}
	})
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)
	var mu sync.Mutex
	var events []string
	send := h.u.send
	h.u.send = func(ctx context.Context, ev notify.Event) {
		if ev.Container == "app" {
			mu.Lock()
			events = append(events, ev.Type)
			mu.Unlock()
		}
		send(ctx, ev)
	}

	if err := h.u.RunWith(context.Background(), RunOptions{Trigger: "api", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if got := h.decisions("app"); !reflect.DeepEqual(got, []string{"dry-run"}) {
		t.Fatalf("decisions = %v, want a dry-run", got)
	}
	mu.Lock()
	if !reflect.DeepEqual(events, []string{notify.EventAvailable}) {
		t.Errorf("events = %v, want the update announced as available", events)
	}
	mu.Unlock()
	if img := h.inspect("app").Image; img != "sha256:v1" {
		t.Fatalf("a check replaced the container: it runs %s", img)
	}
	if n := h.pulls(); n != 0 {
		t.Fatalf("a check pulled %d times", n)
	}
	for _, marker := range []string{preRun, preUpdate} {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("a check ran the hook that writes %s", filepath.Base(marker))
		}
	}
	s, ok := h.u.ContainerStatusOf("fake", "app")
	if !ok || !s.Pending || s.Update != UpdateAvailable || s.Candidate == nil || s.Candidate.Digest != "sha256:v2" {
		t.Fatalf("status = %+v, want the update reported as available", s)
	}
	if h.u.deps.Config.DryRun || h.u.Status("test").DryRun {
		t.Fatal("a check must leave the global dry_run as it is")
	}

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if img := h.inspect("app").Image; img != "sha256:v2" {
		t.Fatalf("the next regular run must apply the update: the container runs %s", img)
	}
	if _, err := os.Stat(preRun); err != nil {
		t.Fatal("the regular run runs its hooks")
	}
}

func TestCheckRunDoesNotPullToCompare(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)
	h.unreachable("example.com/app")

	if err := h.u.RunWith(context.Background(), RunOptions{Trigger: "api", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	r := h.recordFor("app")
	if r.Decision != decisionCheckIncomplete || !strings.Contains(r.Reason, "dry-run does not pull to compare") {
		t.Fatalf("record = %+v", r)
	}
	if n := h.pulls(); n != 0 || h.inspect("app").Image != "sha256:v1" {
		t.Fatalf("a check pulled %d times or replaced the container", n)
	}
}

func TestCheckRunIsTargeted(t *testing.T) {
	const otherRef = "example.com/other:1"
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.release(otherRef, dockertest.Image{ID: "o1"}, dockertest.Image{ID: "o2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)
	h.d.Run("other", &container.Config{Image: otherRef}, nil, nil)

	if err := h.u.RunWith(context.Background(), RunOptions{Trigger: "api", DryRun: true, Containers: []string{"app"}}); err != nil {
		t.Fatal(err)
	}
	if got := h.decisions("app"); !reflect.DeepEqual(got, []string{"dry-run"}) {
		t.Fatalf("app decisions = %v", got)
	}
	if got := h.decisions("other"); len(got) != 0 {
		t.Fatalf("a check of app processed other: %v", got)
	}
	if err := h.u.RunWith(context.Background(), RunOptions{Trigger: "api", DryRun: true, Images: []string{otherRef}}); err != nil {
		t.Fatal(err)
	}
	if got := h.decisions("other"); !reflect.DeepEqual(got, []string{"dry-run"}) {
		t.Fatalf("other decisions = %v", got)
	}
	if got := h.decisions("app"); len(got) != 1 {
		t.Fatalf("a check of %s processed app again: %v", otherRef, got)
	}
	if h.inspect("app").Image != "sha256:v1" || h.inspect("other").Image != "sha256:o1" || h.pulls() != 0 {
		t.Fatal("a targeted check applied an update")
	}
}

func TestCheckRunKeyDiffersFromTheUpdate(t *testing.T) {
	update := RunOptions{Containers: []string{"web"}, Policy: "db"}
	check := update
	check.DryRun = true
	if update.Key() == check.Key() {
		t.Fatalf("a check and an update share the key %q: one would swallow the other", update.Key())
	}
	if got := check.Key(); got != "policy=db;containers=web;dry_run" {
		t.Errorf("key = %q", got)
	}
	if !(RunOptions{DryRun: true}).regular() {
		t.Error("a check of everything covers what a regular run covers")
	}
}

type busySample struct {
	event string
	runID string
	run   RunInProgress
	busy  bool
}

func TestBusyDuringARun(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)
	if r, busy := h.u.Busy(); busy || r != (RunInProgress{}) {
		t.Fatalf("idle updater: Busy() = %+v, %v", r, busy)
	}
	var mu sync.Mutex
	var samples []busySample
	send := h.u.send
	h.u.send = func(ctx context.Context, ev notify.Event) {
		r, busy := h.u.Busy()
		mu.Lock()
		samples = append(samples, busySample{event: ev.Type, runID: ev.RunID, run: r, busy: busy})
		mu.Unlock()
		send(ctx, ev)
	}
	before := time.Now()
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	mu.Lock()
	defer mu.Unlock()
	var started, completed bool
	for _, s := range samples {
		if !s.busy || s.run.ID != s.runID || s.run.Started.Before(before) || s.run.Started.After(after) {
			t.Errorf("%s: Busy() = %+v, %v during run %s", s.event, s.run, s.busy, s.runID)
		}
		started = started || s.event == notify.EventRunStarted
		completed = completed || s.event == notify.EventRunCompleted
	}
	if !started || !completed {
		t.Fatalf("events seen: %+v", samples)
	}
	if r, busy := h.u.Busy(); busy {
		t.Fatalf("Busy() after the run = %+v", r)
	}
	if last := h.u.LastRun(); last == nil || last.ID != samples[0].run.ID || !last.Started.Equal(samples[0].run.Started) {
		t.Fatalf("last run %+v, busy run %+v", last, samples[0].run)
	}
}

func TestBusyReportsTheOldestRun(t *testing.T) {
	u := &Updater{}
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	endFirst := u.active.begin("first", t0)
	endSecond := u.active.begin("second", t0.Add(time.Minute))
	if r, busy := u.Busy(); !busy || r.ID != "first" || !r.Started.Equal(t0) {
		t.Fatalf("Busy() = %+v, %v, want the first run", r, busy)
	}
	endFirst()
	if r, busy := u.Busy(); !busy || r.ID != "second" {
		t.Fatalf("after the first ended: Busy() = %+v, %v", r, busy)
	}
	endSecond()
	if r, busy := u.Busy(); busy {
		t.Fatalf("after both ended: Busy() = %+v", r)
	}
}
