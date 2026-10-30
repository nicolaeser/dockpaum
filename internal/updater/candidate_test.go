package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/registry/registrytest"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func (h *fakeHost) digestPulls() []string {
	var out []string
	for _, c := range h.d.Calls("POST", "/images/create") {
		if tag := c.Query.Get("tag"); strings.HasPrefix(tag, "sha256:") {
			out = append(out, c.Query.Get("fromImage")+"@"+tag)
		}
	}
	return out
}

func TestUnchangedImageIsNotPulledAndAChangedOneIsPulledByDigest(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v1"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped || r.Reason != "image up-to-date" || r.NewDigest != "sha256:v1" {
		t.Fatalf("record %+v", r)
	}
	if n := h.pulls(); n != 0 || h.reg.count("config ") != 0 {
		t.Fatalf("%d pulls, %d config reads; the registry serves the running image", n, h.reg.count("config "))
	}
	if s := h.status("web"); s.Update != UpdateNone || s.Check != CheckRegistry {
		t.Fatalf("status %+v", s)
	}

	d2 := h.publish(appRef, dockertest.Image{ID: "v2"})
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.digestPulls(); len(got) != 1 || got[0] != "example.com/app@"+d2 || h.pulls() != 1 {
		t.Fatalf("pulls by digest %v of %d; want exactly the new digest", got, h.pulls())
	}
	if h.inspect("web").Image != "sha256:v2" || h.d.ImageID(appRef) != "sha256:v2" {
		t.Fatal("the container runs the new release and the tag names it")
	}
	recs := h.records("web")
	if r := recs[len(recs)-1]; r.Decision != audit.DecisionUpdated || r.OldDigest != "sha256:v1" || r.NewDigest != "sha256:v2" {
		t.Fatalf("record %+v", r)
	}
	if s := h.status("web"); s.Current.RepoDigest != "example.com/app@"+d2 || s.Current.Digest != "sha256:v2" {
		t.Fatalf("status current %+v", s.Current)
	}

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := h.pulls(); n != 1 {
		t.Fatalf("%d pulls; an unchanged image is not pulled again", n)
	}
}

func TestCandidateIsResolvedOncePerDigest(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) })
	st := durableState(t, state.PathIn(t.TempDir()))
	h.restart(st, "")
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: time.Now().UTC().Format(time.RFC3339)})
	h.d.Run("a", &container.Config{Image: appRef}, nil, nil)
	h.d.Run("b", &container.Config{Image: appRef}, nil, nil)
	for range 2 {
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
	}
	h.restart(st, "")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := h.reg.count("config "); n != 1 {
		t.Fatalf("the config of one candidate was read %d times; Docker Hub counts each manifest request", n)
	}
	if n := h.reg.count("head "); n != 6 {
		t.Fatalf("%d digest requests, want one per container and run", n)
	}
	if n := h.pulls(); n != 0 {
		t.Fatalf("%d pulls of a soaking image", n)
	}
	for _, name := range []string{"a", "b"} {
		s := h.status(name)
		if s.Decision != string(audit.DecisionGatedAge) || !s.Pending || s.EligibleAt.IsZero() || s.Candidate.Created.IsZero() {
			t.Fatalf("%s: status %+v", name, s)
		}
	}
}

func TestSamePlatformImageInAChangedIndexIsNotPulled(t *testing.T) {
	const otherIndex = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	t.Run("classic image store", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nil)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v1"})
		h.reg.setDigest(appRef, otherIndex)
		h.reg.setConfig(otherIndex, registry.ImageConfig{Digest: "sha256:v1"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped || h.pulls() != 0 {
			t.Fatalf("record %+v after %d pulls", r, h.pulls())
		}
	})
	t.Run("containerd image store", func(t *testing.T) {
		const (
			oldIndex = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			manifest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		)
		h := newFakeHost(t, "1.48", nil)
		h.d.UseContainerdStore()
		h.d.AddImage(dockertest.Image{ID: oldIndex, RepoDigests: []string{"example.com/app@" + oldIndex}, Manifest: manifest}, appRef)
		h.reg.setDigest(appRef, otherIndex)
		h.reg.setIndex(otherIndex, manifest)
		h.reg.setConfig(manifest, registry.ImageConfig{Digest: "sha256:cfg"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped || h.pulls() != 0 {
			t.Fatalf("record %+v after %d pulls; identical bits must not be recreated", r, h.pulls())
		}
	})
}

func TestContainerdImageStoreUpdate(t *testing.T) {
	const (
		index1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		index2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		index3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	)
	h := newFakeHost(t, "1.48", func(c *config.Config) { c.Update.CleanupImages = true })
	h.d.UseContainerdStore()
	h.d.AddImage(dockertest.Image{ID: index1, RepoDigests: []string{"example.com/app@" + index1}}, appRef)
	push := func(index, cfg string) {
		h.d.Publish(appRef, dockertest.Image{ID: index, RepoDigests: []string{"example.com/app@" + index}})
		h.reg.setDigest(appRef, index)
		h.reg.setConfig(index, registry.ImageConfig{Digest: cfg})
	}
	push(index2, "sha256:cfg2")
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if h.inspect("web").Image != index2 || h.d.ImageID(appRef) != index2 {
		t.Fatalf("web runs %s, tag at %s; the new image is identified by its index", h.inspect("web").Image, h.d.ImageID(appRef))
	}
	if r := h.recordFor("web"); r.NewDigest != index2 {
		t.Fatalf("record %+v: the predicted image ID must be the one deployed", r)
	}
	push(index3, "sha256:cfg3")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if h.d.HasImage(index2) || h.d.HasImage(index1) || !h.d.HasImage(index3) {
		t.Fatalf("images kept: index1 %v index2 %v index3 %v", h.d.HasImage(index1), h.d.HasImage(index2), h.d.HasImage(index3))
	}
}

func TestRegistryFallbacks(t *testing.T) {
	tests := []struct {
		name      string
		old       dockertest.Image
		published bool
		fail      error
		wantHeads int
		note      string
	}{
		{name: "local build without registry digest", old: dockertest.Image{ID: "v1"}, published: true, wantHeads: 0, note: "no registry digest"},
		{name: "manifest not found", wantHeads: 2, note: "404"},
		{name: "registry unreachable", published: true, fail: dialTimeout("example.com/app"), wantHeads: 1, note: "i/o timeout"},
		{name: "registry not serving", published: true, fail: fmt.Errorf("after retries: %w", registry.ErrUnavailable), wantHeads: 1,
			note: "unavailable"},
		{name: "no digest in the answer", published: true, fail: fmt.Errorf("%w (example.com/app:1)", registry.ErrNoDigest), wantHeads: 1, note: "Docker-Content-Digest"},
		{name: "unusable answer", published: true, fail: errors.New("registry: decode manifest: unexpected end of JSON input"), wantHeads: 2,
			note: "decode manifest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", nil)
			if tc.old.ID != "" {
				h.d.AddImage(tc.old, appRef)
				h.d.Publish(appRef, dockertest.Image{ID: "v2"})
			} else {
				h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
				if !tc.published {
					h.reg.mu.Lock()
					h.reg.digests = nil
					h.reg.mu.Unlock()
				}
			}
			if tc.fail != nil {
				h.reg.setFail("example.com/app", tc.fail)
			}
			h.d.Run("a", &container.Config{Image: appRef}, nil, nil)
			h.d.Run("b", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if n := h.reg.count("head "); n != tc.wantHeads {
				t.Fatalf("%d digest requests, want %d", n, tc.wantHeads)
			}
			if h.pulls() == 0 || len(h.digestPulls()) != 0 {
				t.Fatalf("pulls %d, by digest %v; without the registry the daemon pulls the tag to compare", h.pulls(), h.digestPulls())
			}
			for _, name := range []string{"a", "b"} {
				if r := h.recordFor(name); r.Decision != audit.DecisionUpdated {
					t.Fatalf("%s: %s %q %q", name, r.Decision, r.Reason, r.Error)
				}
				if s := h.status(name); s.Check != CheckPull || !strings.Contains(s.CheckNote, tc.note) {
					t.Fatalf("%s: check %q note %q", name, s.Check, s.CheckNote)
				}
			}
		})
	}
}

func TestRateLimitedRegistryIsNotPulledToCompare(t *testing.T) {
	t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	h := newFakeHost(t, "1.47", nil)
	h.u.now = fixedClock(t0)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("a", &container.Config{Image: appRef}, nil, nil)
	h.d.Run("b", &container.Config{Image: appRef}, nil, nil)
	h.reg.setFail("example.com/app", &registry.RateLimitError{Registry: "example.com", Op: "manifest", RetryAfter: time.Hour})
	if err := h.run(); err != nil {
		t.Fatalf("an unchecked container does not fail the run: %v", err)
	}
	if n, heads := h.pulls(), h.reg.count("head "); n != 0 || heads != 1 {
		t.Fatalf("%d pulls, %d digest requests", n, heads)
	}
	for _, name := range []string{"a", "b"} {
		r := h.recordFor(name)
		if r.Decision != decisionCheckIncomplete || !strings.Contains(r.Reason, "rate limit exceeded") ||
			!strings.Contains(r.Reason, "no container is pulled to compare while the registry limits requests") {
			t.Fatalf("%s: record %+v", name, r)
		}
	}
	h.reg.setFail("example.com/app", nil)
	h.u.now = fixedClock(t0.Add(2 * time.Hour))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if h.inspect(name).Image != "sha256:v2" {
			t.Fatalf("%s was not updated", name)
		}
	}
}

func TestRateLimitedRegistryIsNotAskedBeforeRetryAfter(t *testing.T) {
	t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	h := newFakeHost(t, "1.47", nil)
	h.u.now = fixedClock(t0)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v1"})
	h.d.Run("web", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	h.reg.setFail("example.com/app", &registry.RateLimitError{Registry: "example.com", Op: "manifest", RetryAfter: time.Hour})
	if err := h.run(); err != nil {
		t.Fatalf("an unchecked monitor-only container does not fail the run: %v", err)
	}
	r := h.recordFor("web")
	if r.Decision != decisionCheckIncomplete || !strings.Contains(r.Reason, "rate limit exceeded, retry after 1h0m0s") ||
		!strings.Contains(r.Reason, "monitor-only container is not pulled") {
		t.Fatalf("record %+v", r)
	}
	h.reg.setFail("example.com/app", nil)
	h.u.now = fixedClock(t0.Add(30 * time.Minute))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	recs := h.records("web")
	if n := h.reg.count("head "); n != 1 || !strings.Contains(recs[1].Reason, "not queried before 2026-03-01T13:00:00Z") {
		t.Fatalf("%d digest requests, record %+v; the registry asked to wait", n, recs[1])
	}
	h.u.now = fixedClock(t0.Add(2 * time.Hour))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	recs = h.records("web")
	if n := h.reg.count("head "); n != 2 || recs[2].Decision != audit.DecisionSkipped || h.pulls() != 0 {
		t.Fatalf("%d digest requests, record %+v, %d pulls", n, recs[2], h.pulls())
	}
}

func TestObservedContainersAreNotPulledWithoutTheRegistry(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.DryRun = true })
	var buf strings.Builder
	var mu sync.Mutex
	h.u.deps.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.unreachable("example.com/app")
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	for range 2 {
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range h.records("web") {
		if r.Decision != decisionCheckIncomplete || !strings.Contains(r.Reason, "dry-run does not pull to compare") {
			t.Fatalf("record %+v", r)
		}
	}
	if n := h.pulls(); n != 0 {
		t.Fatalf("%d pulls in dry-run", n)
	}
	if s := h.status("web"); s.Update != UpdateUnknown || s.Pending || s.Check != "" || s.CheckNote == "" {
		t.Fatalf("status %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(buf.String(), "update check skipped"); n != 1 {
		t.Fatalf("%d warnings over two runs, want one:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "registry cannot be asked for digests directly"); n != 1 {
		t.Fatalf("%d registry warnings, want one in the life of the process:\n%s", n, buf.String())
	}
}

func TestApprovalIsAskedBeforeAnythingIsPulled(t *testing.T) {
	var body approvalRequest
	var pullsAtApproval int
	var h *fakeHost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		pullsAtApproval = h.pulls()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h = newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: srv.URL} })
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if pullsAtApproval != 0 || h.pulls() != 1 {
		t.Fatalf("pulls at approval %d, after %d; the approved image is pulled once approved", pullsAtApproval, h.pulls())
	}
	want := approvalRequest{
		OldDigest: "sha256:v1", NewDigest: "sha256:v2",
		OldRepoDigest: registry.DigestReference(appRef, fakeDigest(appRef, "v1")),
		NewRepoDigest: registry.DigestReference(appRef, fakeDigest(appRef, "v2")),
		RequestID:     approvalRequestID("fake", "web", "sha256:v2"),
	}
	if body.OldDigest != want.OldDigest || body.NewDigest != want.NewDigest || body.OldRepoDigest != want.OldRepoDigest ||
		body.NewRepoDigest != want.NewRepoDigest || body.RequestID != want.RequestID {
		t.Fatalf("approval request %+v, want %+v", body, want)
	}
	if got := h.digestPulls(); len(got) != 1 || got[0] != want.NewRepoDigest {
		t.Fatalf("pulled %v, want the approved digest", got)
	}
}

func TestWindowClosedStillChecksWithoutPulling(t *testing.T) {
	nextOpening := time.Date(2026, time.January, 6, 2, 0, 0, 0, time.UTC)
	t.Run("candidate waits for the window", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nightlyWindow)
		h.u.now = fixedClock(outWindow)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionGatedWindow || r.NewDigest != "sha256:v2" {
			t.Fatalf("record %+v", r)
		}
		s := h.status("web")
		if !s.Pending || s.Candidate == nil || s.Candidate.Digest != "sha256:v2" || !s.NextWindow.Equal(nextOpening) {
			t.Fatalf("status %+v; the plan shows the candidate and when it can be applied", s)
		}
		if h.pulls() != 0 {
			t.Fatalf("%d pulls outside the window", h.pulls())
		}
	})
	t.Run("the window after the soak", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) {
			nightlyWindow(c)
			c.Update.Policies[0].MinImageAge = config.Duration(48 * time.Hour)
		})
		h.u.now = fixedClock(outWindow)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Created: outWindow.Format(time.RFC3339)})
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		s := h.status("web")
		eligible := outWindow.Add(48 * time.Hour)
		if !s.EligibleAt.Equal(eligible) || !s.NextWindow.Equal(time.Date(2026, time.January, 8, 2, 0, 0, 0, time.UTC)) {
			t.Fatalf("eligible %v, next window %v; the window opening after the soak", s.EligibleAt, s.NextWindow)
		}
	})
	t.Run("without the registry nothing is pulled", func(t *testing.T) {
		h := newFakeHost(t, "1.47", nightlyWindow)
		h.u.now = fixedClock(outWindow)
		h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
		h.unreachable("example.com/app")
		h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionGatedWindow || h.pulls() != 0 {
			t.Fatalf("record %+v after %d pulls", r, h.pulls())
		}
		if s := h.status("web"); s.Update != UpdateUnknown || !s.NextWindow.Equal(nextOpening) {
			t.Fatalf("status %+v", s)
		}
	})
	t.Run("a failed tag list does not fail the run", func(t *testing.T) {
		h := newFakeHost(t, "1.47", func(c *config.Config) {
			nightlyWindow(c)
			c.Update.Policies[0].Mode = config.ModeMinor
		})
		h.u.now = fixedClock(outWindow)
		h.d.AddImage(dockertest.Image{ID: "v1"}, "example.com/app:1.2.0")
		h.reg.setFail("example.com/app", errors.New("dial tcp: i/o timeout"))
		h.d.Run("web", &container.Config{Image: "example.com/app:1.2.0"}, nil, nil)
		if err := h.run(); err != nil {
			t.Fatalf("outside its window a failed check is not a failure: %v", err)
		}
		if r := h.recordFor("web"); r.Decision != audit.DecisionGatedWindow {
			t.Fatalf("record %+v", r)
		}
	})
}

func TestNextWindow(t *testing.T) {
	h := newFakeHost(t, "1.47", nightlyWindow)
	eff := h.u.resolver.Resolve(docker.Container{Name: "web"}, map[string]string{})
	h.u.now = fixedClock(outWindow)
	if got := h.u.nextWindow(eff.Windows, time.Time{}); !got.Equal(time.Date(2026, time.January, 6, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next window %v", got)
	}
	h.u.now = fixedClock(outWindow.Add(time.Hour))
	if got := h.u.nextWindow(eff.Windows, time.Time{}); !got.Equal(time.Date(2026, time.January, 6, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next window %v", got)
	}
	h.u.now = fixedClock(inWindow)
	if got := h.u.nextWindow(eff.Windows, time.Time{}); !got.IsZero() {
		t.Fatalf("open now, got %v", got)
	}
	if got := h.u.nextWindow(nil, time.Time{}); !got.IsZero() {
		t.Fatalf("no windows, got %v", got)
	}
}

func TestDryRunIsAPlanWithoutSideEffects(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.DryRun = true
		c.Update.Policies = append(c.Update.Policies,
			config.Policy{ID: "minor", Mode: config.ModeMinor},
			config.Policy{ID: "soak", Mode: config.ModeTagFollow, MinImageAge: config.Duration(time.Hour)})
	})
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.release("example.com/same:1", dockertest.Image{ID: "s1"}, dockertest.Image{ID: "s1"})
	h.release("example.com/fresh:1", dockertest.Image{ID: "f1"}, dockertest.Image{ID: "f2", Created: time.Now().UTC().Format(time.RFC3339)})
	h.release("example.com/lib:1.2.0", dockertest.Image{ID: "l120"}, dockertest.Image{ID: "l120"})
	h.publish("example.com/lib:1.3.0", dockertest.Image{ID: "l130", Config: &container.Config{Labels: map[string]string{
		"org.opencontainers.image.source": "https://github.com/acme/lib",
	}}})
	h.reg.setTags("example.com/lib", "1.2.0", "1.3.0", "2.0.0")
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.d.Run("same", &container.Config{Image: "example.com/same:1"}, nil, nil)
	h.d.Run("fresh", &container.Config{Image: "example.com/fresh:1", Labels: map[string]string{"dockpaum.policy": "soak"}}, nil, nil)
	h.d.Run("lib", &container.Config{Image: "example.com/lib:1.2.0", Labels: map[string]string{"dockpaum.policy": "minor"}}, nil, nil)
	h.d.Run("mon", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	leftover := h.d.Run("old.dockpaum-old-1", &container.Config{Image: appRef}, nil, nil)
	h.d.Stop(leftover)
	h.d.AddImage(dockertest.Image{ID: "helper", Exit: func([]string) (int, string, bool) {
		return 1, "self-update helper: start replacement: boom\n", true
	}})
	h.d.Run("dockpaum-self-update-1", &container.Config{Image: "sha256:helper", Labels: map[string]string{
		selfupdate.HelperLabel: "gone", selfupdate.HelperContainerLabel: "dockpaum",
	}}, nil, nil)
	h.d.Run("dockpaum-check-1", &container.Config{Image: "sha256:helper", Labels: map[string]string{selfupdate.CheckLabel: "gone"}}, nil, nil)
	before := len(h.d.Calls("", ""))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, call := range h.d.Calls("", "")[before:] {
		if call.Method != http.MethodGet {
			t.Fatalf("dry-run changed Docker: %s %s", call.Method, call.Path)
		}
	}
	for _, name := range []string{"old.dockpaum-old-1", "dockpaum-self-update-1", "dockpaum-check-1"} {
		if _, ok := h.d.Inspect(name); !ok {
			t.Fatalf("%s is gone after a dry-run (containers %v)", name, h.d.Names())
		}
	}
	if recs := append(h.records("old"), h.records("dockpaum")...); len(recs) != 0 {
		t.Fatalf("a dry-run recorded %+v", recs)
	}
	want := map[string]struct {
		decision audit.Decision
		pending  bool
	}{
		"web":   {audit.DecisionDryRun, true},
		"same":  {audit.DecisionSkipped, false},
		"fresh": {audit.DecisionGatedAge, true},
		"lib":   {audit.DecisionDryRun, true},
		"mon":   {audit.DecisionMonitorOnly, true},
	}
	for name, w := range want {
		s := h.status(name)
		if s.Decision != string(w.decision) || s.Pending != w.pending || s.Check != CheckRegistry {
			t.Fatalf("%s: status %+v, want %s pending %v", name, s, w.decision, w.pending)
		}
	}
	lib := h.status("lib")
	if lib.Candidate.Tag != "1.3.0" || lib.Candidate.Version != "1.3.0" || lib.Candidate.Digest != "sha256:l130" ||
		lib.Candidate.ReleaseURL != "https://github.com/acme/lib/releases/tag/v1.3.0" || lib.PolicySource != "label dockpaum.policy=minor" || lib.Mode != config.ModeMinor {
		t.Fatalf("lib: %+v %+v", lib, lib.Candidate)
	}
	if fresh := h.status("fresh"); fresh.EligibleAt.IsZero() {
		t.Fatalf("fresh: %+v", fresh)
	}
	if mon := h.status("mon"); !mon.MonitorOnly {
		t.Fatalf("mon: %+v", mon)
	}

	h.u.deps.Config.DryRun = false
	_ = h.run()
	if ct, ok := h.d.Inspect("old"); !ok || ct.ID != leftover {
		t.Fatalf("leftover not restored (containers %v)", h.d.Names())
	}
	if got := h.decisions("dockpaum"); !slices.Equal(got, []string{string(decisionSelfUpdateFailed)}) {
		t.Fatalf("self-update outcome %v", got)
	}
}

func TestPulledImageThatDiffersFromTheResolvedOneIsNotDeployed(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	d := h.publish(appRef, dockertest.Image{ID: "v2"})
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.reg.setConfig(d, registry.ImageConfig{Digest: "sha256:elsewhere"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	err := h.run()
	if err == nil {
		t.Fatal("the run must fail")
	}
	r := h.recordFor("web")
	if r.Decision != audit.DecisionCheckFailed || !strings.Contains(r.Error, "not elsewhere") || !strings.Contains(r.Error, "not deployed") {
		t.Fatalf("record %+v", r)
	}
	if h.inspect("web").Image != "sha256:v1" || h.d.ImageID(appRef) != "sha256:v1" {
		t.Fatal("the container and its tag must stay on the running image")
	}
	if !h.d.HasImage("v2") {
		t.Fatal("the pulled image is kept untagged until superseded")
	}
}

func TestRegistryCheckAgainstAnHTTPRegistry(t *testing.T) {
	reg := registrytest.New(t)
	build := func(arch, b string) registrytest.Image {
		return registrytest.Image{Architecture: arch, Build: b, Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Labels: map[string]string{"org.opencontainers.image.version": "1.0." + b}}
	}
	ref := reg.Host() + "/acme/app:1"
	v1 := reg.Push("acme/app", "1", build("amd64", "1"), build("arm64", "1"))

	h := newFakeHost(t, "1.47", nil)
	h.u.reg = registry.New(nil)
	h.d.AddImage(dockertest.Image{ID: v1.Configs[0], RepoDigests: []string{reg.Host() + "/acme/app@" + v1.Digest}}, ref)
	h.d.Run("web", &container.Config{Image: ref}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionSkipped || h.pulls() != 0 {
		t.Fatalf("record %+v after %d pulls", r, h.pulls())
	}
	if n := len(reg.Calls(http.MethodGet, "/manifests/")); n != 0 {
		t.Fatalf("%d manifest GETs for an unchanged image; Docker Hub counts each", n)
	}

	reg.Push("acme/app", "1", build("amd64", "1"), build("arm64", "1b"))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	recs := h.records("web")
	if r := recs[len(recs)-1]; r.Decision != audit.DecisionSkipped || h.pulls() != 0 {
		t.Fatalf("record %+v after %d pulls; identical bits are not recreated", r, h.pulls())
	}

	v2 := reg.Push("acme/app", "1", build("amd64", "2"), build("arm64", "2"))
	h.d.Publish(ref, dockertest.Image{ID: v2.Configs[0], RepoDigests: []string{reg.Host() + "/acme/app@" + v2.Digest}})
	gets := len(reg.Calls(http.MethodGet, ""))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	recs = h.records("web")
	if r := recs[len(recs)-1]; r.Decision != audit.DecisionUpdated || r.NewDigest != v2.Configs[0] {
		t.Fatalf("record %+v", r)
	}
	if got := h.digestPulls(); len(got) != 1 || got[0] != reg.Host()+"/acme/app@"+v2.Digest {
		t.Fatalf("pulls by digest %v", got)
	}
	if n := len(reg.Calls(http.MethodGet, "")) - gets; n != 3 {
		t.Fatalf("%d GETs to resolve the release, want 3", n)
	}
	if s := h.status("web"); s.Current.Version != "1.0.2" || s.Current.RepoDigest != reg.Host()+"/acme/app@"+v2.Digest {
		t.Fatalf("status %+v", s.Current)
	}
}

func TestEmulatedContainerKeepsItsPlatformThroughTheRegistry(t *testing.T) {
	reg := registrytest.New(t)
	ref := reg.Host() + "/acme/legacy:1"
	v1 := reg.Push("acme/legacy", "1", registrytest.Image{Architecture: "arm64", Build: "1"}, registrytest.Image{Architecture: "amd64", Build: "1"})
	h := newFakeHost(t, "1.47", nil)
	h.d.SetArch("arm64")
	h.u.reg = registry.New(nil)
	h.d.AddImage(dockertest.Image{ID: v1.Configs[1], Architecture: "amd64", RepoDigests: []string{reg.Host() + "/acme/legacy@" + v1.Digest}}, ref)
	h.d.Run("legacy", &container.Config{Image: ref}, nil, nil)
	v2 := reg.Push("acme/legacy", "1", registrytest.Image{Architecture: "arm64", Build: "2"}, registrytest.Image{Architecture: "amd64", Build: "2"})
	for i, arch := range []string{"arm64", "amd64"} {
		h.d.Publish(ref, dockertest.Image{ID: v2.Configs[i], Architecture: arch, RepoDigests: []string{reg.Host() + "/acme/legacy@" + v2.Digest}})
	}
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.inspect("legacy").Image; got != v2.Configs[1] {
		t.Fatalf("legacy runs %s, want the amd64 build %s", got, v2.Configs[1])
	}
	pull := h.d.Calls("POST", "/images/create")[0]
	if pull.Query.Get("platform") != "linux/amd64" || !strings.HasPrefix(pull.Query.Get("tag"), "sha256:") {
		t.Fatalf("pull %v", pull.Query)
	}
	if r := h.recordFor("legacy"); r.NewDigest != v2.Configs[1] {
		t.Fatalf("record %+v: the amd64 config was resolved", r)
	}
}

func TestContainersOfOneTagKeepTheirPlatforms(t *testing.T) {
	type fixture struct {
		h       *fakeHost
		ref     string
		pullRef string
		v1, v2  registrytest.Published
		run     func(name string, arch int)
	}
	setup := func(t *testing.T) fixture {
		reg := registrytest.New(t)
		f := fixture{ref: reg.Host() + "/acme/app:1"}
		build := func(arch, b string) registrytest.Image { return registrytest.Image{Architecture: arch, Build: b} }
		archs := []string{"arm64", "amd64"}
		f.v1 = reg.Push("acme/app", "1", build(archs[0], "1"), build(archs[1], "1"))
		f.h = newFakeHost(t, "1.47", nil)
		f.h.d.SetArch("arm64")
		f.h.u.reg = registry.New(nil)
		f.run = func(name string, arch int) {
			f.h.d.AddImage(dockertest.Image{ID: f.v1.Configs[arch], Architecture: archs[arch],
				RepoDigests: []string{reg.Host() + "/acme/app@" + f.v1.Digest}}, f.ref)
			f.h.d.Run(name, &container.Config{Image: f.ref}, nil, nil)
		}
		f.v2 = reg.Push("acme/app", "1", build(archs[0], "2"), build(archs[1], "2"))
		f.pullRef = reg.Host() + "/acme/app@" + f.v2.Digest
		for i, arch := range archs {
			f.h.d.Publish(f.ref, dockertest.Image{ID: f.v2.Configs[i], Architecture: arch, RepoDigests: []string{f.pullRef}})
		}
		return f
	}
	check := func(t *testing.T, f fixture, names map[string]int) {
		t.Helper()
		for name, arch := range names {
			if got := f.h.inspect(name).Image; got != f.v2.Configs[arch] {
				t.Fatalf("%s runs %s, want %s: its platform's build", name, got, f.v2.Configs[arch])
			}
			if r := f.h.records(name); len(r) != 1 || r[0].Decision != audit.DecisionUpdated || r[0].NewDigest != f.v2.Configs[arch] {
				t.Fatalf("%s: records %+v", name, r)
			}
		}
	}

	emulatedFirst := 0
	for _, order := range [][]string{{"legacy", "native"}, {"native", "legacy"}} {
		t.Run(order[0]+" created first", func(t *testing.T) {
			f := setup(t)
			arch := map[string]int{"native": 0, "legacy": 1}
			for _, name := range order {
				f.run(name, arch[name])
			}
			if err := f.h.run(); err != nil {
				t.Fatal(err)
			}
			check(t, f, arch)
			if f.h.d.Calls("POST", "/images/create")[0].Query.Get("platform") == "linux/amd64" {
				emulatedFirst++
			}
		})
	}
	t.Run("another platform pulled as the digest meanwhile", func(t *testing.T) {
		f := setup(t)
		f.run("native", 0)
		f.run("native2", 0)
		c := f.h.u.deps.Clients.List()[0]
		if _, err := c.PullImagePlatform(context.Background(), f.pullRef, nil, "linux/amd64"); err != nil {
			t.Fatal(err)
		}
		if err := f.h.run(); err != nil {
			t.Fatal(err)
		}
		check(t, f, map[string]int{"native": 0, "native2": 0})
		if n := len(f.h.digestPulls()); n != 2 {
			t.Fatalf("%d pulls by digest, want the amd64 one and one native", n)
		}
	})
	if emulatedFirst != 1 {
		t.Fatalf("the emulated container came first in %d of the two orders, want one", emulatedFirst)
	}
}

func TestSelfUpdateWithoutTheRegistryPullsToCompare(t *testing.T) {
	h, _ := selfHost(t, ok, func() (int, string) { return 0, "" })
	h.unreachable("example.com/dockpaum")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("dockpaum"); r.Decision != audit.DecisionSelfUpdate {
		t.Fatalf("record %+v", r)
	}
	if h.d.ImageID("example.com/dockpaum:latest") != "sha256:dp2" || len(h.digestPulls()) != 0 {
		t.Fatal("the helper needs the tag on the new image, pulled by tag")
	}
}

func TestSelfUpdateTagMovesRightBeforeTheHandOver(t *testing.T) {
	h, _ := selfHost(t, ok, func() (int, string) { return 0, "" })
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("app", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	appCreate, selfTag := -1, -1
	for i, c := range h.d.Calls("POST", "") {
		switch {
		case strings.HasSuffix(c.Path, "/containers/create") && c.Query.Get("name") == "app":
			appCreate = i
		case strings.HasSuffix(c.Path, "/tag") && strings.Contains(c.Query.Get("repo"), "dockpaum"):
			selfTag = i
		}
	}
	if appCreate < 0 || selfTag < appCreate {
		t.Fatalf("app create %d, daemon tag %d", appCreate, selfTag)
	}
	if got := h.digestPulls(); len(got) != 2 {
		t.Fatalf("pulls by digest %v", got)
	}
}

func TestPlanFromAStateSnapshotWritesNoState(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	path := state.PathIn(t.TempDir())
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		c.Update.Policies[0].MinImageAge = config.Duration(time.Hour)
		c.Update.Policies[0].MinImageAgeSource = "first_seen"
	})
	st := durableState(t, path)
	h.restart(st, "")
	h.u.now = fixedClock(t0)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionGatedAge {
		t.Fatalf("record %+v", r)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	h.u.deps.Config.DryRun = true
	h.restart(state.OpenSnapshot(path, nil), "")
	h.u.now = fixedClock(t0.Add(2 * time.Hour))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	s := h.status("web")
	if s.Decision != string(audit.DecisionDryRun) || s.Candidate == nil || !s.Candidate.FirstSeen.Equal(t0) {
		t.Fatalf("status %+v", s)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the plan changed the state file (%v)", err)
	}
	if h.pulls() != 0 {
		t.Fatalf("%d pulls", h.pulls())
	}
}
