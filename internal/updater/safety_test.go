package updater

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
)

const appRef = "example.com/app:1"

func approver(t *testing.T, handle func() int) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(handle())
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func scriptedClock(times ...time.Time) func() time.Time {
	var mu sync.Mutex
	i := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := times[min(i, len(times)-1)]
		i++
		return t
	}
}

var (
	inWindow  = time.Date(2026, time.January, 5, 3, 0, 0, 0, time.UTC)
	outWindow = time.Date(2026, time.January, 5, 5, 0, 0, 0, time.UTC)
)

func nightlyWindow(c *config.Config) {
	c.Update.Policies[0].Timezone = "UTC"
	c.Update.Policies[0].Windows = []config.WindowSpec{{Days: []string{"daily"}, Times: []string{"02:00-04:00"}}}
}

func (h *fakeHost) recordFor(name string) audit.Record {
	h.t.Helper()
	recs := h.records(name)
	if len(recs) != 1 {
		h.t.Fatalf("%s: records %+v, want exactly one", name, recs)
	}
	return recs[0]
}

var checkPaths = []string{CheckRegistry, CheckPull}

func (h *fakeHost) unreachable(repo string) {
	h.reg.setFail(repo, dialTimeout(repo))
}

func dialTimeout(repo string) error {
	host, _, _ := strings.Cut(repo, "/")
	return &url.Error{Op: "Head", URL: "https://" + host + "/v2/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}}
}

func TestRefusedUpdateLeavesTheTagOnTheRunningImage(t *testing.T) {
	recent := dockertest.Image{ID: "v2", Created: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	tests := []struct {
		name         string
		mutate       func(*config.Config)
		labels       map[string]string
		newImg       dockertest.Image
		deny         bool
		clock        []time.Time
		want         audit.Decision
		fetched      bool
		wantFail     bool
		registryOnly bool
	}{
		{name: "monitor-only", labels: map[string]string{"dockpaum.monitor-only": "true"}, want: audit.DecisionMonitorOnly},
		{name: "still soaking", mutate: func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) },
			newImg: recent, want: audit.DecisionGatedAge},
		{name: "dry-run", mutate: func(c *config.Config) { c.DryRun = true }, want: audit.DecisionDryRun},
		{name: "approval denied", deny: true, want: audit.DecisionApprovalDeny},
		{name: "a pre-update hook asks to skip", mutate: func(c *config.Config) { c.Hooks.PreUpdate = []string{"exit 75"} },
			want: audit.DecisionSkipped, fetched: true},
		{name: "window closed before the pull", mutate: nightlyWindow, clock: []time.Time{inWindow, outWindow},
			want: audit.DecisionGatedWindow},
		{name: "window closed during the pull", mutate: nightlyWindow, clock: []time.Time{inWindow, inWindow, outWindow},
			want: audit.DecisionGatedWindow, fetched: true, registryOnly: true},
		{name: "rolled back", newImg: dockertest.Image{ID: "v2", CrashLoop: true}, want: audit.DecisionRolledBack,
			fetched: true, wantFail: true},
	}
	for _, path := range checkPaths {
		for _, tc := range tests {
			if tc.registryOnly && path != CheckRegistry {
				continue
			}
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				mutate := tc.mutate
				if tc.deny {
					url, _ := approver(t, func() int { return http.StatusForbidden })
					mutate = func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} }
				}
				h := newFakeHost(t, "1.47", mutate)
				if tc.clock != nil {
					h.u.windowNow = scriptedClock(tc.clock...)
				}
				newImg := tc.newImg
				if newImg.ID == "" {
					newImg.ID = "v2"
				}
				h.release(appRef, dockertest.Image{ID: "v1"}, newImg)
				if path == CheckPull {
					h.unreachable("example.com/app")
				}
				oldID := h.d.Run("web", &container.Config{Image: appRef, Labels: tc.labels}, nil, nil)

				if err := h.run(); (err != nil) != tc.wantFail {
					t.Fatalf("run error = %v", err)
				}
				observed := tc.want == audit.DecisionMonitorOnly || tc.want == audit.DecisionDryRun
				want := tc.want
				if path == CheckPull && observed {
					want = decisionCheckIncomplete
				}
				if got := h.recordFor("web").Decision; got != want {
					t.Fatalf("decision = %s, want %s", got, want)
				}
				if ct := h.inspect("web"); ct.ID != oldID || ct.Image != "sha256:v1" {
					t.Fatalf("container changed: %s %s", ct.ID, ct.Image)
				}
				if got := h.d.ImageID(appRef); got != "sha256:v1" {
					t.Fatalf("%s points at %s; `docker compose up` would deploy the refused image", appRef, got)
				}
				switch {
				case (path == CheckRegistry && !tc.fetched) || (path == CheckPull && observed):
					if n := h.pulls(); n != 0 || h.d.HasImage("v2") {
						t.Fatalf("%d pulls; an update refused by its gates downloads nothing", n)
					}
				default:
					if !h.d.HasImage("v2") {
						t.Fatal("the refused image is kept untagged, so the next check pulls nothing")
					}
				}
				if path == CheckRegistry {
					for _, call := range h.d.Calls("POST", "/images/create") {
						if !strings.HasPrefix(call.Query.Get("tag"), "sha256:") {
							t.Fatalf("pulled %s:%s; the candidate is pulled by its digest", call.Query.Get("fromImage"), call.Query.Get("tag"))
						}
					}
				}
			})
		}
	}
}

func TestRefusedImageIsRemovedOnceSuperseded(t *testing.T) {
	for _, path := range checkPaths {
		t.Run(path, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) { c.Hooks.PreUpdate = []string{"exit 75"} })
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			if path == CheckPull {
				h.unreachable("example.com/app")
			}
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if !h.d.HasImage("v2") {
				t.Fatal("the current refused image must be kept")
			}
			h.publish(appRef, dockertest.Image{ID: "v3"})
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if h.d.HasImage("v2") {
				t.Fatal("a superseded refused image must be removed")
			}
			if !h.d.HasImage("v3") || h.d.ImageID(appRef) != "sha256:v1" {
				t.Fatalf("v3 kept %v, tag at %s", h.d.HasImage("v3"), h.d.ImageID(appRef))
			}
		})
	}
}

func TestSharedTagFollowsTheDeployedImage(t *testing.T) {
	for _, strategy := range []string{config.StrategyRolling, config.StrategyParallel} {
		t.Run(strategy, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				c.Update.Strategy = strategy
				c.Update.Concurrency = 4
			})
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			h.d.Run("watch", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
			h.d.Run("serve", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if h.inspect("serve").Image != "sha256:v2" || h.inspect("watch").Image != "sha256:v1" {
				t.Fatal("serve must be updated and watch left alone")
			}
			if got := h.d.ImageID(appRef); got != "sha256:v2" {
				t.Fatalf("tag points at %s; it must name the image an update deployed", got)
			}
		})
	}
}

func TestComposeImageLabelFollowsTheUpdate(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("p-web-1", &container.Config{Image: appRef, Labels: map[string]string{
		docker.ComposeImageLabel: "sha256:v1", "com.docker.compose.project": "p", "com.docker.compose.service": "web",
	}}, nil, nil)
	h.d.Run("plain", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.inspect("p-web-1").Config.Labels[docker.ComposeImageLabel]; got != "sha256:v2" {
		t.Fatalf("compose image label = %q; Compose would recreate the container again", got)
	}
	if _, ok := h.inspect("plain").Config.Labels[docker.ComposeImageLabel]; ok {
		t.Fatal("a container Compose did not create gets no Compose label")
	}
}

func TestDryRunNeverAsksForApproval(t *testing.T) {
	url, calls := approver(t, func() int { return http.StatusOK })
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.DryRun = true
		c.Update.Approval = &config.ApprovalConfig{URL: url}
	})
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("approval endpoint asked %d time(s) in dry-run", n)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionDryRun || !strings.Contains(r.Reason, "not asked") {
		t.Fatalf("record = %+v", r)
	}
}

func TestWindowIsCheckedAgainBeforeTheUpdateBegins(t *testing.T) {
	tests := []struct {
		name  string
		clock []time.Time
		want  audit.Decision
	}{
		{name: "closed before the pull", clock: []time.Time{inWindow, outWindow}, want: audit.DecisionGatedWindow},
		{name: "closed before the pre-update hooks", clock: []time.Time{inWindow, inWindow, outWindow}, want: audit.DecisionGatedWindow},
		{name: "closed while the pre-update hooks ran", clock: []time.Time{inWindow, inWindow, inWindow, outWindow}, want: audit.DecisionUpdated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pre, post := filepath.Join(dir, "pre"), filepath.Join(dir, "post")
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				nightlyWindow(c)
				c.Hooks.PreUpdate = []string{"touch " + pre}
				c.Hooks.PostUpdate = []string{"touch " + post}
			})
			h.u.windowNow = scriptedClock(tc.clock...)
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			r := h.recordFor("web")
			if r.Decision != tc.want {
				t.Fatalf("record = %+v, want %s", r, tc.want)
			}
			_, preErr := os.Stat(pre)
			_, postErr := os.Stat(post)
			if (preErr == nil) != (postErr == nil) {
				t.Fatalf("pre-update hook ran %v, post-update hook ran %v: hooks must come in pairs", preErr == nil, postErr == nil)
			}
			if tc.want == audit.DecisionUpdated {
				if preErr != nil || h.inspect("web").Image != "sha256:v2" {
					t.Fatal("the update that began must be finished")
				}
				return
			}
			if !strings.Contains(r.Reason, "window closed during the run") || preErr == nil {
				t.Fatalf("record = %+v, pre-update hook ran = %v", r, preErr == nil)
			}
			if pulled := h.pulls() > 0; pulled != (len(tc.clock) > 2) {
				t.Fatalf("pulled %v; the window is checked before the pull", pulled)
			}
			if n := len(h.d.Calls("POST", "/rename")) + len(h.d.Calls("POST", "/stop")); n != 0 {
				t.Fatal("nothing may be touched once the window closed")
			}
			if h.d.ImageID(appRef) != "sha256:v1" {
				t.Fatal("tag not pointed back at the running image")
			}
		})
	}
}

func TestSoakWithoutADurableState(t *testing.T) {
	tests := []struct {
		name    string
		created string
		want    string
	}{
		{name: "unreadable", created: "not-a-time"},
		{name: "zero time", created: "0001-01-01T00:00:00Z"},
		{name: "epoch (reproducible build)", created: "1970-01-01T00:00:00Z"},
		{name: "in the future", created: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), want: "in the future"},
		{name: "too young", created: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339), want: "new image age"},
		{name: "old enough", created: "2025-01-01T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) })
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: tc.created})
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			r := h.recordFor("web")
			if tc.want == "" {
				if r.Decision != audit.DecisionUpdated {
					t.Fatalf("record = %+v, want updated", r)
				}
				return
			}
			if r.Decision != audit.DecisionGatedAge || !strings.Contains(r.Reason, tc.want) {
				t.Fatalf("record = %+v, want gated-min-age mentioning %q", r, tc.want)
			}
		})
	}
}

func TestRecreateRenamesBeforeTheStop(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	rename, stop := -1, -1
	for i, c := range h.d.Calls("POST", "") {
		switch c.Path {
		case "/containers/" + oldID + "/rename":
			rename = i
		case "/containers/" + oldID + "/stop":
			stop = i
		}
	}
	if rename < 0 || stop < 0 || rename > stop {
		t.Fatalf("the old container must be renamed before it is stopped (rename %d, stop %d)", rename, stop)
	}
}

func TestStopFailureRestoresTheRunningContainer(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.d.FailNext("POST", "/containers/"+oldID+"/stop", "stop timed out")
	if err := h.run(); err == nil {
		t.Fatal("a failed stop must fail the run")
	}
	r := h.recordFor("web")
	if r.Decision != audit.DecisionFailed || !strings.Contains(r.Error, "stop:") {
		t.Fatalf("record = %+v", r)
	}
	if ct := h.inspect("web"); ct.ID != oldID || !ct.State.Running {
		t.Fatal("the container must keep its name and keep running")
	}
	h.noLeftovers()
	if h.d.ImageID(appRef) != "sha256:v1" {
		t.Fatal("tag not pointed back at the running image")
	}
}

func TestTagMovedDuringTheUpdate(t *testing.T) {
	t.Run("before the stop: nothing is touched", func(t *testing.T) {
		var h *fakeHost
		url, _ := approver(t, func() int {
			h.d.AddImage(dockertest.Image{ID: "v3"}, appRef)
			return http.StatusOK
		})
		h = newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} })
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		_ = h.run()
		if r := h.recordFor("web"); r.Decision != audit.DecisionCheckFailed || !strings.Contains(r.Error, "tag moved") {
			t.Fatalf("record = %+v", r)
		}
		if ct := h.inspect("web"); ct.ID != oldID || len(h.d.Calls("POST", "/stop")) != 0 {
			t.Fatal("the container must not be touched")
		}
		if got := h.d.ImageID(appRef); got != "sha256:v3" {
			t.Fatalf("tag points at %s; what someone pulled after dockpaum's pull must stand", got)
		}
	})
	t.Run("during the stop: rolled back", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.AddImage(dockertest.Image{ID: "v3"})
		oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.d.RetagOnNext("POST", "/containers/"+oldID+"/stop", appRef, "v3")
		if err := h.run(); err == nil {
			t.Fatal("expected a failed run")
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionRolledBack || !strings.Contains(r.Error, "tag moved") {
			t.Fatalf("record = %+v", r)
		}
		if ct := h.inspect("web"); ct.ID != oldID || !ct.State.Running || ct.Image != "sha256:v1" {
			t.Fatal("the old container must be restored and running")
		}
		h.noLeftovers()
		if got := h.d.ImageID(appRef); got != "sha256:v3" {
			t.Fatalf("tag points at %s; what someone pulled after dockpaum's pull must stand", got)
		}
	})
}

func TestRefusedUpdateRestoresWhatTheTagNamedBefore(t *testing.T) {
	for _, path := range checkPaths {
		t.Run(path+"/an image pulled outside dockpaum stays", func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) { c.Hooks.PreUpdate = []string{"exit 75"} })
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v3"})
			if path == CheckPull {
				h.unreachable("example.com/app")
			}
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			h.d.AddImage(dockertest.Image{ID: "v2"}, appRef)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped {
				t.Fatalf("record = %+v", r)
			}
			if got := h.d.ImageID(appRef); got != "sha256:v2" {
				t.Fatalf("tag points at %s; the operator's pull must stand", got)
			}
		})
	}
	t.Run("observing leaves an operator's pull alone", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) { c.DryRun = true })
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v3"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.d.AddImage(dockertest.Image{ID: "v2"}, appRef)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionDryRun || r.NewDigest != "sha256:v3" {
			t.Fatalf("record = %+v", r)
		}
		if got := h.d.ImageID(appRef); got != "sha256:v2" || h.pulls() != 0 {
			t.Fatalf("tag points at %s after %d pulls; dry-run touches nothing", got, h.pulls())
		}
	})
	for _, strategy := range []string{config.StrategyRolling, config.StrategyParallel} {
		for _, path := range checkPaths {
			t.Run(path+"/a shared tag stays on the deployed image in later runs ("+strategy+")", func(t *testing.T) {
				h := newFakeHost(t, "1.47", func(c *config.Config) {
					c.Update.Strategy = strategy
					c.Update.Concurrency = 4
				})
				h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: time.Now().UTC().Format(time.RFC3339)})
				if path == CheckPull {
					h.unreachable("example.com/app")
				}
				h.d.Run("soak", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.min-age": "8760h"}}, nil, nil)
				h.d.Run("serve", &container.Config{Image: appRef}, nil, nil)
				for run := 1; run <= 2; run++ {
					if err := h.run(); err != nil {
						t.Fatal(err)
					}
					if got := h.d.ImageID(appRef); got != "sha256:v2" {
						t.Fatalf("run %d: tag points at %s; `compose up` would downgrade serve", run, got)
					}
				}
				if h.inspect("serve").Image != "sha256:v2" || h.inspect("soak").Image != "sha256:v1" {
					t.Fatal("serve must be updated and soak left alone")
				}
			})
		}
	}
	t.Run("refused containers that run different images", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) })
		h.release(appRef, dockertest.Image{ID: "old1"}, dockertest.Image{ID: "v3", Created: time.Now().UTC().Format(time.RFC3339)})
		h.unreachable("example.com/app")
		h.d.Run("a", &container.Config{Image: appRef}, nil, nil)
		h.d.AddImage(dockertest.Image{ID: "old2"}, appRef)
		h.d.Run("b", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if got := h.d.ImageID(appRef); got != "sha256:old2" {
			t.Fatalf("tag points at %s, want old2, which it named before the pull", got)
		}
		for _, name := range []string{"a", "b"} {
			if r := h.recordFor(name); r.Decision != audit.DecisionGatedAge || r.Image != appRef {
				t.Fatalf("%s: record = %+v (the image is the reference the container follows)", name, r)
			}
		}
	})
}

func TestDrainStopsStartingUpdates(t *testing.T) {
	t.Run("before the run", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.u.Drain()
		err := h.run()
		if err == nil || !strings.Contains(err.Error(), "shutting down: 1 container(s) left for the next run") {
			t.Fatalf("run error = %v", err)
		}
		if n := len(h.d.Calls("POST", "/images/create")); n != 0 || len(h.records("web")) != 0 {
			t.Fatal("nothing may be pulled or decided once shutdown began")
		}
	})
	t.Run("while a container waits for its approval", func(t *testing.T) {
		var h *fakeHost
		url, _ := approver(t, func() int {
			h.u.Drain()
			return http.StatusOK
		})
		h = newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} })
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.release("example.com/other:1", dockertest.Image{ID: "o1"}, dockertest.Image{ID: "o2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		h.d.Run("other", &container.Config{Image: "example.com/other:1"}, nil, nil)
		err := h.run()
		if err == nil || !strings.Contains(err.Error(), "2 container(s) left for the next run") {
			t.Fatalf("run error = %v", err)
		}
		var interrupted, untouched int
		for _, name := range []string{"web", "other"} {
			switch recs := h.records(name); {
			case len(recs) == 0:
				untouched++
			case len(recs) == 1 && recs[0].Decision == audit.DecisionSkipped && recs[0].Reason == shutdownReason:
				interrupted++
			default:
				t.Fatalf("%s records = %+v", name, recs)
			}
		}
		if interrupted != 1 || untouched != 1 || len(h.d.Calls("POST", "/stop")) != 0 {
			t.Fatalf("interrupted %d, untouched %d", interrupted, untouched)
		}
		if h.d.ImageID(appRef) != "sha256:v1" || h.d.ImageID("example.com/other:1") != "sha256:o1" {
			t.Fatal("tags must point at the running images")
		}
	})
	t.Run("only container interrupted after its approval", func(t *testing.T) {
		var h *fakeHost
		url, _ := approver(t, func() int {
			h.u.Drain()
			return http.StatusOK
		})
		h = newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} })
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		err := h.run()
		if err == nil || !strings.Contains(err.Error(), "1 container(s) left for the next run") {
			t.Fatalf("a run that left an update for the next one must not succeed: %v", err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped || r.Reason != shutdownReason {
			t.Fatalf("record = %+v", r)
		}
		if n := h.pulls(); n != 0 {
			t.Fatalf("%d pulls; nothing is pulled once shutdown began", n)
		}
	})
}

func selfHost(t *testing.T, validate, helper func() (int, string)) (*fakeHost, string) {
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		on := true
		c.Update.SelfUpdate.Enabled = &on
		c.Update.SelfUpdate.Delay = config.Duration(time.Second)
	})
	labels := &container.Config{Labels: map[string]string{"org.opencontainers.image.title": "dockpaum"}}
	h.release("example.com/dockpaum:latest", dockertest.Image{ID: "dp1", Config: labels}, dockertest.Image{ID: "dp2", Config: labels, Exit: func(cmd []string) (int, string, bool) {
		switch {
		case len(cmd) > 0 && cmd[0] == "--validate":
			code, out := validate()
			return code, out, true
		case len(cmd) > 0 && cmd[0] == "--self-update-helper":
			code, out := helper()
			return code, out, true
		}
		return 0, "", false
	}})
	selfID := h.d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest"}, nil, nil)
	h.u.selfID = selfID
	return h, selfID
}

func ok() (int, string) { return 0, "config OK\n" }

func disableSelfUpdate(h *fakeHost) {
	off := false
	h.u.deps.Config.Update.SelfUpdate.Enabled = &off
}

func TestSelfUpdateComesLastAndItsOutcomeIsRecorded(t *testing.T) {
	h, _ := selfHost(t, ok, func() (int, string) { return 0, `{"msg":"self-update complete"}` + "\n" })
	rec := h.recordEvents()
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if evs := rec.forContainer("dockpaum"); len(evs) != 1 || evs[0].Type != notify.EventSelfUpdateStarted || evs[0].Reason != h.recordFor("dockpaum").Reason {
		t.Fatalf("events %+v, want self_update.started with the hand-off's reason", evs)
	}
	if got := h.decisions("app"); !reflect.DeepEqual(got, []string{"updated"}) {
		t.Fatalf("app decisions = %v", got)
	}
	r := h.recordFor("dockpaum")
	if r.Decision != audit.DecisionSelfUpdate || !strings.Contains(r.Reason, "dockpaum-self-update-") {
		t.Fatalf("dockpaum record = %+v", r)
	}
	appCreate, helperCreate, check := -1, -1, -1
	for i, c := range h.d.Calls("POST", "/containers/create") {
		switch name := c.Query.Get("name"); {
		case name == "app":
			appCreate = i
		case strings.HasPrefix(name, "dockpaum-self-update-"):
			helperCreate = i
		case strings.HasPrefix(name, "dockpaum-config-check-"):
			check = i
		}
	}
	if appCreate < 0 || check < appCreate || helperCreate < check {
		t.Fatalf("order: app %d, config check %d, helper %d — the self-update must come after every other container", appCreate, check, helperCreate)
	}
	if h.d.ImageID("example.com/dockpaum:latest") != "sha256:dp2" {
		t.Fatal("the helper needs the tag on the new image")
	}
	if s, _ := h.u.ContainerStatusOf("fake", "dockpaum"); s.Decision != string(audit.DecisionSelfUpdate) || !s.Pending {
		t.Fatalf("status while the helper runs = %+v", s)
	}

	disableSelfUpdate(h)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.decisions("dockpaum"); !slices.Contains(got, string(decisionSelfUpdateCompleted)) {
		t.Fatalf("dockpaum decisions = %v", got)
	}
	var types []string
	for _, ev := range rec.forContainer("dockpaum") {
		types = append(types, ev.Type)
		if ev.Type == notify.EventSelfUpdateSuccess && (!strings.Contains(ev.Reason, "replaced the daemon") || ev.NewDigest != "sha256:dp2") {
			t.Fatalf("self_update.success %+v", ev)
		}
	}
	if want := []string{notify.EventSelfUpdateStarted, notify.EventUpdateSuccess, notify.EventSelfUpdateSuccess, notify.EventNoUpdate}; !reflect.DeepEqual(types, want) {
		t.Fatalf("dockpaum events %v, want %v", types, want)
	}
	if s, _ := h.u.ContainerStatusOf("fake", "dockpaum"); s.Pending || s.Update != UpdateNone {
		t.Fatalf("status after the completed self-update = %+v", s)
	}
	for _, n := range h.d.Names() {
		if strings.HasPrefix(n, "dockpaum-self-update-") || strings.HasPrefix(n, "dockpaum-config-check-") {
			t.Fatalf("one-shot container %s left behind", n)
		}
	}
}

func TestSelfUpdateRefusedWhenTheNewImageRejectsTheConfig(t *testing.T) {
	h, selfID := selfHost(t, func() (int, string) { return 2, "config: parse x: field retention not found\n" },
		func() (int, string) { t.Fatal("no helper may start"); return 0, "" })
	err := h.run()
	if err == nil {
		t.Fatal("a refused self-update must fail the run")
	}
	r := h.recordFor("dockpaum")
	if r.Decision != audit.DecisionFailed || !strings.Contains(r.Error, "rejects the configuration (exit 2)") {
		t.Fatalf("record = %+v", r)
	}
	if strings.Contains(r.Error, "config: parse x") {
		t.Fatalf("the check's output may quote secrets; it stays in the log: %+v", r)
	}
	if h.d.ImageID("example.com/dockpaum:latest") != "sha256:dp1" {
		t.Fatal("the tag must point at the running daemon's image again")
	}
	if names := h.d.Names(); !reflect.DeepEqual(names, []string{"dockpaum"}) {
		t.Fatalf("containers = %v", names)
	}
	if ct := h.inspect("dockpaum"); ct.ID != selfID || !ct.State.Running {
		t.Fatal("the daemon must not be touched")
	}
	checks := len(h.d.Calls("POST", "/containers/create"))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if recs := h.records("dockpaum"); recs[len(recs)-1].Decision != audit.DecisionGatedPolicy ||
		!strings.Contains(recs[len(recs)-1].Reason, "the self-update to dp2 failed at") {
		t.Fatalf("records = %+v", recs)
	}
	if n := len(h.d.Calls("POST", "/containers/create")); n != checks {
		t.Fatal("no configuration check may run for an image that was rejected")
	}
}

func TestFailedSelfUpdateIsReportedByTheNextRun(t *testing.T) {
	h, _ := selfHost(t, ok, func() (int, string) {
		return 1, "self-update helper: new daemon failed its health check: container is unhealthy; the old daemon was restored\n"
	})
	rec := h.recordEvents()
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	disableSelfUpdate(h)
	err := h.run()
	if err == nil || !strings.Contains(err.Error(), "1 container update(s) failed") {
		t.Fatalf("run error = %v", err)
	}
	var failed *audit.Record
	for _, r := range h.records("dockpaum") {
		if r.Decision == decisionSelfUpdateFailed {
			failed = &r
		}
	}
	if failed == nil || !strings.Contains(failed.Error, "the old daemon was restored") {
		t.Fatalf("records = %+v", h.records("dockpaum"))
	}
	fails := rec.of(notify.EventSelfUpdateFailure, "dockpaum")
	if len(fails) != 1 || fails[0].Err == nil || fails[0].Err.Error() != failed.Error || fails[0].RunID != failed.RunID {
		t.Fatalf("self_update.failure events %+v for record %+v", fails, *failed)
	}
	if n := len(rec.of(notify.EventUpdateFailure, "dockpaum")); n != 1 {
		t.Fatalf("%d update.failure events; it still fires beside self_update.failure", n)
	}
	if s, _ := h.u.ContainerStatusOf("fake", "dockpaum"); !s.Pending || s.Candidate == nil || s.Candidate.Digest != "sha256:dp2" {
		t.Fatalf("status after the failed self-update = %+v", s)
	}
}

func TestFailedSelfUpdateIsHeldBack(t *testing.T) {
	h, _ := selfHost(t, ok, func() (int, string) {
		return 1, "self-update helper: new daemon failed its health check: container is unhealthy; the old daemon was restored\n"
	})
	t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	h.u.now = fixedClock(t0)
	helpers := func() int {
		n := 0
		for _, c := range h.d.Calls("POST", "/containers/create") {
			if strings.HasPrefix(c.Query.Get("name"), "dockpaum-self-update-") {
				n++
			}
		}
		return n
	}
	last := func() audit.Record {
		recs := h.records("dockpaum")
		return recs[len(recs)-1]
	}
	if err := h.run(); err != nil || helpers() != 1 {
		t.Fatalf("first run: %v, helpers %d", err, helpers())
	}
	if err := h.run(); err == nil {
		t.Fatal("the failed self-update must fail the run that records it")
	}
	if r := last(); r.Decision != audit.DecisionGatedPolicy || !strings.Contains(r.Reason, "the self-update to dp2 failed at 2026-03-01T12:00:00Z") {
		t.Fatalf("record = %+v", r)
	}
	if helpers() != 1 {
		t.Fatal("no helper may start for the image that just failed")
	}
	if got := h.d.ImageID("example.com/dockpaum:latest"); got != "sha256:dp1" {
		t.Fatalf("tag points at %s; it must name the running daemon's image again", got)
	}
	h.u.now = fixedClock(t0.Add(selfUpdateRetry))
	if err := h.run(); err != nil || helpers() != 2 {
		t.Fatalf("retry: %v, helpers %d", err, helpers())
	}
	h.u.now = fixedClock(t0.Add(selfUpdateRetry + time.Hour))
	_ = h.run()
	labels := &container.Config{Labels: map[string]string{"org.opencontainers.image.title": "dockpaum"}}
	h.publish("example.com/dockpaum:latest", dockertest.Image{ID: "dp3", Config: labels, Exit: func(cmd []string) (int, string, bool) {
		return 0, "", len(cmd) > 0
	}})
	if err := h.run(); err != nil || helpers() != 3 {
		t.Fatalf("newer image: %v, helpers %d", err, helpers())
	}
}

func TestSelfUpdateRefusalRunsNoHooks(t *testing.T) {
	tests := []struct {
		name     string
		validate func() (int, string)
		hc       *container.HostConfig
		want     string
	}{
		{name: "the helper cannot get the daemon's Docker access", validate: ok, hc: &container.HostConfig{NetworkMode: "none"},
			want: "has no network"},
		{name: "the new image rejects the configuration", validate: func() (int, string) { return 2, "bad config\n" },
			want: "rejects the configuration"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "pre")
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				on := true
				c.Update.SelfUpdate.Enabled = &on
				c.Update.SelfUpdate.Delay = config.Duration(time.Second)
				c.Hooks.PreUpdate = []string{"touch " + marker}
			})
			labels := &container.Config{Labels: map[string]string{"org.opencontainers.image.title": "dockpaum"}}
			h.d.AddImage(dockertest.Image{ID: "dp1", Config: labels}, "example.com/dockpaum:latest")
			h.d.Publish("example.com/dockpaum:latest", dockertest.Image{ID: "dp2", Config: labels, Exit: func([]string) (int, string, bool) {
				code, out := tc.validate()
				return code, out, true
			}})
			h.u.selfID = h.d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest"}, tc.hc, nil)
			if err := h.run(); err == nil {
				t.Fatal("a refused self-update fails the run")
			}
			if r := h.recordFor("dockpaum"); r.Decision != audit.DecisionFailed || !strings.Contains(r.Error, tc.want) {
				t.Fatalf("record = %+v", r)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("pre-update hooks run only once the hand-off can happen")
			}
		})
	}
}

func TestUnidentifiedOwnContainerFailsClosed(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.u.selfUnknown = true
	labels := &container.Config{Labels: map[string]string{"org.opencontainers.image.title": "dockpaum"}}
	h.release("example.com/updater:1", dockertest.Image{ID: "dp1", Config: labels}, dockertest.Image{ID: "dp2", Config: labels})
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("maybe-me", &container.Config{Image: "example.com/updater:1"}, nil, nil)
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("maybe-me"); r.Decision != audit.DecisionGatedPolicy || r.Reason != ownContainerUnknownReason {
		t.Fatalf("record = %+v", r)
	}
	if h.inspect("web").Image != "sha256:v2" {
		t.Fatal("other containers are updated as usual")
	}
	for _, c := range h.d.Calls("POST", "/images/create") {
		if strings.Contains(c.Query.Get("fromImage"), "updater") {
			t.Fatal("a gated dockpaum container must not be pulled for")
		}
	}
}

func TestIdentifySelfOnTheFirstRun(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.d.AddImage(dockertest.Image{ID: "v1"}, appRef)
	id := h.d.Run("dockpaum", &container.Config{Image: appRef}, nil, nil)
	h.u.selfChecked, h.u.selfCands = false, []selfupdate.Candidate{{ID: id, Source: selfupdate.SourceCgroup}}
	_ = h.run()
	if h.u.selfID != id || h.u.selfUnknown {
		t.Fatalf("selfID = %q unknown = %v", h.u.selfID, h.u.selfUnknown)
	}

	h2 := newFakeHost(t, "1.47", nil)
	h2.u.selfChecked, h2.u.selfCands = false, []selfupdate.Candidate{{ID: strings.Repeat("f", 64), Source: selfupdate.SourceMountinfo}}
	_ = h2.run()
	if h2.u.selfID != "" || !h2.u.selfUnknown || !h2.u.selfChecked {
		t.Fatalf("an unidentified own container must fail closed: id %q unknown %v checked %v", h2.u.selfID, h2.u.selfUnknown, h2.u.selfChecked)
	}
}

func TestLeftoverRecovery(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*config.Config)
		leftover       string
		labels         map[string]string
		stopped        bool
		helper         bool
		self, replaced bool
		recovered      bool
	}{
		{name: "killed between the rename and the stop", leftover: "web.dockpaum-old-1", recovered: true},
		{name: "killed after the stop", leftover: "web.dockpaum-old-1", stopped: true, recovered: true},
		{name: "another instance's scope", leftover: "web.dockpaum-old-1", labels: map[string]string{"dockpaum.scope": "blue"}, stopped: true},
		{name: "excluded by this instance's filters", leftover: "web.dockpaum-old-1", stopped: true,
			mutate: func(c *config.Config) { c.Filters.ExcludeNames = []string{"web"} }},
		{name: "self-update still in progress", leftover: "dockpaum.self-update-old-1", stopped: true, helper: true},
		{name: "self-update leftover of an excluded own container", leftover: "dockpaum.self-update-old-1", stopped: true, recovered: true,
			mutate: func(c *config.Config) { c.Filters.ExcludeNames = []string{"dockpaum"} }},
		{name: "own daemon left under its temporary name, opt-in filters", leftover: "dockpaum.self-update-old-1", self: true, recovered: true,
			mutate: func(c *config.Config) { c.Filters.LabelEnable = true }},
		{name: "own daemon left under its temporary name, scoped instance", leftover: "dockpaum.self-update-old-1", self: true, recovered: true,
			mutate: func(c *config.Config) { c.Filters.Scope = "blue" }},
		{name: "daemon a finished helper replaced, opt-in filters", leftover: "dockpaum.self-update-old-1", stopped: true, replaced: true,
			recovered: true, mutate: func(c *config.Config) { c.Filters.LabelEnable = true }},
		{name: "someone else's self-update leftover, opt-in filters", leftover: "dockpaum.self-update-old-1", stopped: true,
			mutate: func(c *config.Config) { c.Filters.LabelEnable = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				c.Filters.IncludeStopped = true
				if tc.mutate != nil {
					tc.mutate(c)
				}
			})
			h.d.AddImage(dockertest.Image{ID: "v1"}, appRef)
			id := h.d.Run(tc.leftover, &container.Config{Image: appRef, Labels: tc.labels}, nil, nil)
			if tc.stopped {
				h.d.Stop(id)
			}
			if tc.helper {
				h.d.Run("dockpaum-self-update-1", &container.Config{Image: appRef, Labels: map[string]string{selfupdate.HelperLabel: id}}, nil, nil)
			}
			if tc.self {
				h.u.selfID = id
			}
			if tc.replaced {
				h.d.AddImage(dockertest.Image{ID: "helper", Exit: func([]string) (int, string, bool) {
					return 1, "self-update helper: rename the old daemon back: conflict\n", true
				}})
				labels := map[string]string{selfupdate.HelperLabel: id, selfupdate.HelperContainerLabel: "dockpaum"}
				if s := h.u.deps.Config.Filters.Scope; s != "" {
					labels["dockpaum.scope"] = s
				}
				h.d.Run("dockpaum-self-update-1", &container.Config{Image: "sha256:helper", Labels: labels}, nil, nil)
			}
			rec := h.recordEvents()
			_ = h.run()
			orig, _, _ := strings.Cut(tc.leftover, ".")
			ct, renamed := h.d.Inspect(orig)
			if renamed != tc.recovered {
				t.Fatalf("recovered = %v, want %v (containers %v)", renamed, tc.recovered, h.d.Names())
			}
			if !tc.recovered {
				return
			}
			if ct.ID != id || !ct.State.Running {
				t.Fatal("the leftover must run again under its original name")
			}
			starts := len(h.d.Calls("POST", "/containers/"+id+"/start"))
			if want := map[bool]int{true: 1, false: 0}[tc.stopped]; starts != want {
				t.Fatalf("start calls = %d, want %d", starts, want)
			}
			if got := h.decisions(orig); !slices.Contains(got, string(audit.DecisionRecovered)) {
				t.Fatalf("decisions = %v", got)
			}
			var recovered audit.Record
			for _, r := range h.records(orig) {
				if r.Decision == audit.DecisionRecovered {
					recovered = r
				}
			}
			if evs := rec.of(notify.EventRecovered, orig); len(evs) != 1 || evs[0].Reason != recovered.Reason || evs[0].RunID != recovered.RunID {
				t.Fatalf("update.recovered events %+v for record %+v", evs, recovered)
			}
		})
	}
}

func youngLeftover(h *fakeHost) string {
	h.d.AddImage(dockertest.Image{ID: "v1"}, appRef)
	return h.d.Run(fmt.Sprintf("web.dockpaum-old-%d", time.Now().UnixNano()), &container.Config{Image: appRef}, nil, nil)
}

func TestLeftoverThatMayStillBeStopping(t *testing.T) {
	t.Run("its stop ends: restored and started", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		id := youngLeftover(h)
		h.d.StopLater(id, 100*time.Millisecond)
		h.u.leftoverSettle = 10 * time.Millisecond
		_ = h.run()
		ct := h.inspect("web")
		if ct.ID != id || !ct.State.Running {
			t.Fatal("the leftover must run again under its own name once its stop ended")
		}
		if got := h.decisions("web"); !slices.Contains(got, string(audit.DecisionRecovered)) {
			t.Fatalf("decisions = %v", got)
		}
	})
	t.Run("still stopping when the wait ends: left for the next run", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		id := youngLeftover(h)
		h.u.maxStopWait = 20 * time.Millisecond
		_ = h.run()
		if _, ok := h.d.Inspect("web"); ok || len(h.d.Calls("POST", "/containers/"+id+"/rename")) != 0 {
			t.Fatal("a leftover that may still be stopping must keep its temporary name")
		}
	})
	t.Run("a replacement appears once it stopped: left to the update", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		id := youngLeftover(h)
		h.d.StopLater(id, 50*time.Millisecond)
		h.u.leftoverSettle = 300 * time.Millisecond
		time.AfterFunc(100*time.Millisecond, func() { h.d.Run("web", &container.Config{Image: appRef}, nil, nil) })
		_ = h.run()
		if ct := h.inspect("web"); ct.ID == id {
			t.Fatal("the leftover must not take the replacement's name")
		}
		if got := h.decisions("web"); slices.Contains(got, string(audit.DecisionRecovered)) {
			t.Fatalf("decisions = %v", got)
		}
	})
	t.Run("renamed long ago and running: no stop is under way", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		h.d.AddImage(dockertest.Image{ID: "v1"}, appRef)
		renamed := time.Now().Add(-time.Hour).UnixNano()
		id := h.d.Run(fmt.Sprintf("web.dockpaum-old-%d", renamed), &container.Config{Image: appRef}, nil, nil)
		_ = h.run()
		if ct := h.inspect("web"); ct.ID != id || !ct.State.Running || len(h.d.Calls("POST", "/wait")) != 0 {
			t.Fatal("an old running leftover is renamed back at once")
		}
	})
}

func TestInterruptedStopIsWaitedFor(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	begun := h.d.SlowStop(oldID, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-begun
		cancel()
	}()
	if err := h.u.RunWith(ctx, RunOptions{Trigger: "test"}); err == nil {
		t.Fatal("an interrupted update must fail the run")
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionFailed || !strings.Contains(r.Error, "stop:") {
		t.Fatalf("record = %+v", r)
	}
	if n := len(h.d.Calls("POST", "/containers/"+oldID+"/start")); n != 1 {
		t.Fatalf("the container was started %d time(s), want once after its stop ended", n)
	}
	if ct := h.inspect("web"); ct.ID != oldID || !ct.State.Running {
		t.Fatal("the container must run again under its own name")
	}
	h.noLeftovers()
}

func TestNameReferenceToTheNetworkOwnerIsKept(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.d.AddImage(dockertest.Image{ID: "vpn1"}, "example.com/vpn:1")
	h.d.Publish("example.com/vpn:1", dockertest.Image{ID: "vpn1"})
	h.d.Run("vpn", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	web := h.inspect("web")
	if web.Image != "sha256:v2" || web.HostConfig.NetworkMode != "container:vpn" {
		t.Fatalf("web image %s network %q: a reference by name must survive the update, so a restart follows a recreated owner",
			web.Image, web.HostConfig.NetworkMode)
	}
}

func TestLiveOnMarksUnknownRepositories(t *testing.T) {
	live := liveOn([]docker.Container{
		{Name: "web", Image: appRef, ImageID: "sha256:v1"},
		{Name: "web.dockpaum-old-1", Image: appRef, ImageID: "sha256:v0"},
	})
	if live.ReposUnknown || !slices.Equal(live.Repos, []string{imageRepo(appRef), imageRepo(appRef)}) ||
		!slices.Equal(live.Containers, []string{"web", "web.dockpaum-old-1", "web"}) {
		t.Fatalf("live = %+v", live)
	}
	live = liveOn([]docker.Container{{Name: "stale", Image: "sha256:v1", ImageID: "sha256:v1"}})
	if !live.ReposUnknown {
		t.Fatal("a container listed by image ID leaves its repository unknown")
	}
}
