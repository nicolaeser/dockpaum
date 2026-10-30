package updater

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/state"
)

type delivered struct {
	Type      string `json:"type"`
	Container string `json:"container"`
	Image     string `json:"image"`
	NewDigest string `json:"new_digest"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
}

type webhookSink struct {
	mu  sync.Mutex
	got []delivered
}

func (s *webhookSink) count(typ, container string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.got {
		if d.Type == typ && d.Container == container {
			n++
		}
	}
	return n
}

func (h *fakeHost) deliverTo(st *state.Store, events ...string) *webhookSink {
	h.t.Helper()
	sink := &webhookSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d delivered
		_ = json.NewDecoder(r.Body).Decode(&d)
		sink.mu.Lock()
		sink.got = append(sink.got, d)
		sink.mu.Unlock()
	}))
	h.t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.u.deps.Notifier = notify.Build([]config.NotifierConfig{{Type: "webhook", URL: srv.URL, Events: events}}, logger)
	h.restart(st, "")
	return sink
}

func (h *fakeHost) runAndDeliver() error {
	h.t.Helper()
	err := h.run()
	h.u.deps.Notifier.Wait(5 * time.Second)
	return err
}

func (h *fakeHost) recordEvents() *recordedEvents {
	rec := &recordedEvents{}
	send := h.u.send
	h.u.send = func(ctx context.Context, ev notify.Event) {
		rec.send(ctx, ev)
		send(ctx, ev)
	}
	return rec
}

func (r *recordedEvents) forContainer(container string) []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.Event
	for _, ev := range r.events {
		if ev.Container == container {
			out = append(out, ev)
		}
	}
	return out
}

func (h *fakeHost) expectEvents(rec *recordedEvents, want map[string][]string) {
	h.t.Helper()
	for name, types := range want {
		evs := rec.forContainer(name)
		var got []string
		for _, ev := range evs {
			got = append(got, ev.Type)
		}
		if strings.Join(got, ",") != strings.Join(types, ",") {
			h.t.Errorf("%s: events %v, want %v (records %+v)", name, got, types, h.records(name))
			continue
		}
		recs := h.records(name)
		for _, ev := range evs {
			var match *audit.Record
			for i := range recs {
				if recs[i].RunID == ev.RunID {
					match = &recs[i]
				}
			}
			if match == nil {
				h.h(name, "event %s of run %s has no audit record", ev.Type, ev.RunID)
				continue
			}
			errText := ""
			if ev.Err != nil {
				errText = ev.Err.Error()
			}
			reason := match.Reason
			if ev.Type == notify.EventNoUpdate {
				reason = string(match.Decision) + ": " + match.Reason
			}
			if ev.Reason != reason || errText != match.Error || ev.NewDigest != match.NewDigest || ev.Image != match.Image {
				h.h(name, "event %s (reason %q, error %q, new %q) disagrees with the audit record %+v", ev.Type, ev.Reason, errText, ev.NewDigest, *match)
			}
		}
	}
}

func (h *fakeHost) h(name, format string, args ...any) {
	h.t.Helper()
	h.t.Errorf(name+": "+format, args...)
}

func TestEventsFollowTheAuditDecisions(t *testing.T) {
	t.Run("gates, failures and updates", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		rec := h.recordEvents()
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.release("example.com/same:1", dockertest.Image{ID: "s1"}, dockertest.Image{ID: "s1"})
		h.d.Run("same", &container.Config{Image: "example.com/same:1"}, nil, nil)
		h.release("example.com/mon:1", dockertest.Image{ID: "m1"}, dockertest.Image{ID: "m2"})
		h.d.Run("mon", &container.Config{Image: "example.com/mon:1", Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
		pinned := "example.com/pin@sha256:" + strings.Repeat("a", 64)
		h.d.AddImage(dockertest.Image{ID: "p1"}, pinned)
		h.d.Run("pinned", &container.Config{Image: pinned}, nil, nil)
		h.release("example.com/rm:1", dockertest.Image{ID: "r1"}, dockertest.Image{ID: "r2"})
		h.d.Run("oneshot", &container.Config{Image: "example.com/rm:1"}, &container.HostConfig{AutoRemove: true}, nil)
		h.d.Run("task", &container.Config{Image: "example.com/rm:1", Labels: map[string]string{"com.docker.swarm.task.id": "x1"}}, nil, nil)
		h.d.Run("typo", &container.Config{Image: "example.com/rm:1", Labels: map[string]string{"dockpaum.policy": "databse"}}, nil, nil)
		h.release("example.com/bad:1", dockertest.Image{ID: "b1"}, dockertest.Image{ID: "b2", CrashLoop: true})
		h.d.Run("bad", &container.Config{Image: "example.com/bad:1"}, nil, nil)
		h.d.AddImage(dockertest.Image{ID: "d1"}, "down.example.net/down:1")
		h.unreachable("down.example.net/down")
		h.d.Run("down", &container.Config{Image: "down.example.net/down:1"}, nil, nil)
		h.release("watch.example.net/watch:1", dockertest.Image{ID: "w1"}, dockertest.Image{ID: "w2"})
		h.unreachable("watch.example.net/watch")
		h.d.Run("watch", &container.Config{Image: "watch.example.net/watch:1", Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)

		if err := h.run(); err == nil {
			t.Fatal("the failed updates must fail the run")
		}
		for name, want := range map[string]audit.Decision{
			"web": audit.DecisionUpdated, "same": audit.DecisionSkipped, "mon": audit.DecisionMonitorOnly,
			"pinned": audit.DecisionGatedDigest, "oneshot": audit.DecisionGatedAutoRemove, "task": audit.DecisionGatedOrchestrated,
			"typo": audit.DecisionGatedPolicy, "bad": audit.DecisionRolledBack, "down": audit.DecisionCheckFailed,
			"watch": decisionCheckIncomplete,
		} {
			if r := h.recordFor(name); r.Decision != want {
				t.Errorf("%s: decision %s, want %s", name, r.Decision, want)
			}
		}
		h.expectEvents(rec, map[string][]string{
			"web":     {notify.EventUpdateSuccess},
			"same":    nil,
			"mon":     {notify.EventAvailable},
			"pinned":  {notify.EventNoUpdate},
			"oneshot": {notify.EventNoUpdate},
			"task":    {notify.EventNoUpdate},
			"typo":    {notify.EventNoUpdate},
			"bad":     {notify.EventUpdateFailure, notify.EventRolledBack},
			"down":    {notify.EventUpdateFailure, notify.EventCheckFailed},
			"watch":   {notify.EventCheckFailed},
		})
	})

	t.Run("maintenance window", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nightlyWindow)
		h.u.now = fixedClock(outWindow)
		rec := h.recordEvents()
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.release("example.com/same:1", dockertest.Image{ID: "s1"}, dockertest.Image{ID: "s1"})
		h.d.Run("same", &container.Config{Image: "example.com/same:1"}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if h.recordFor("web").Decision != audit.DecisionGatedWindow || h.recordFor("same").Decision != audit.DecisionGatedWindow {
			t.Fatalf("records %+v %+v", h.records("web"), h.records("same"))
		}
		h.expectEvents(rec, map[string][]string{"web": {notify.EventNoUpdate}, "same": nil})
	})

	t.Run("minimum image age", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(48 * time.Hour) })
		rec := h.recordEvents()
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: time.Now().UTC().Format(time.RFC3339)})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionGatedAge {
			t.Fatalf("record %+v", r)
		}
		h.expectEvents(rec, map[string][]string{"web": {notify.EventNoUpdate}})
	})

	t.Run("dry-run", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) { c.DryRun = true })
		rec := h.recordEvents()
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.release("example.com/same:1", dockertest.Image{ID: "s1"}, dockertest.Image{ID: "s1"})
		h.d.Run("same", &container.Config{Image: "example.com/same:1"}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionDryRun {
			t.Fatalf("record %+v", r)
		}
		h.expectEvents(rec, map[string][]string{"web": {notify.EventAvailable}, "same": nil})
	})

	t.Run("approval denied", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
		t.Cleanup(srv.Close)
		h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: srv.URL} })
		rec := h.recordEvents()
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionApprovalDeny {
			t.Fatalf("record %+v", r)
		}
		h.expectEvents(rec, map[string][]string{"web": {notify.EventDenied}})
	})
}

func TestStandingEventsAreDeliveredOncePerCandidate(t *testing.T) {
	path := state.PathIn(t.TempDir())
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(48 * time.Hour) })
	sink := h.deliverTo(durableState(t, path), notify.EventAvailable, notify.EventNoUpdate)
	young := time.Now().UTC().Format(time.RFC3339)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: young})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.release("example.com/mon:1", dockertest.Image{ID: "m1"}, dockertest.Image{ID: "m2"})
	h.d.Run("mon", &container.Config{Image: "example.com/mon:1", Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	for range 3 {
		if err := h.runAndDeliver(); err != nil {
			t.Fatal(err)
		}
	}
	check := func(step string, want int) {
		t.Helper()
		if n := sink.count(notify.EventNoUpdate, "web"); n != want {
			t.Errorf("%s: update.skipped delivered %d times, want %d", step, n, want)
		}
		if n := sink.count(notify.EventAvailable, "mon"); n != want {
			t.Errorf("%s: update.available delivered %d times, want %d", step, n, want)
		}
	}
	check("three runs", 1)

	if err := h.u.state.Close(); err != nil {
		t.Fatal(err)
	}
	h.restart(durableState(t, path), "")
	if err := h.runAndDeliver(); err != nil {
		t.Fatal(err)
	}
	check("after a restart", 1)

	h.publish(appRef, dockertest.Image{ID: "v3", Created: young})
	h.publish("example.com/mon:1", dockertest.Image{ID: "m3"})
	for range 2 {
		if err := h.runAndDeliver(); err != nil {
			t.Fatal(err)
		}
	}
	check("a newer candidate", 2)
}
