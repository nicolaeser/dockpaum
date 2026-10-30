package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
)

type approvalAnswerFor struct {
	status int
	header http.Header
	body   string
	hangUp bool
}

type approvalLog struct {
	mu   sync.Mutex
	reqs []approvalRequest
}

func (l *approvalLog) all() []approvalRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]approvalRequest(nil), l.reqs...)
}

func phasedApprover(t *testing.T, answer func(phase string) approvalAnswerFor) (string, *approvalLog) {
	t.Helper()
	log := &approvalLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req approvalRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		log.mu.Lock()
		log.reqs = append(log.reqs, req)
		log.mu.Unlock()
		a := answer(req.Phase)
		if a.hangUp {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		for k, v := range a.header {
			w.Header()[k] = v
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, log
}

func (h *fakeHost) scrapeMetrics() string {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.u.deps.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestRecheckBeforeStop(t *testing.T) {
	busy := approvalAnswerFor{status: http.StatusConflict, header: http.Header{"Content-Type": {"application/json"}},
		body: `{"approved":false,"reason":"workload busy","retry_after_seconds":600}`}
	ok := approvalAnswerFor{status: http.StatusNoContent}
	tests := []struct {
		name       string
		final      approvalAnswerFor
		failOpen   bool
		recheck    bool
		updated    bool
		wantPhases []string
		wantReason []string
		wantMetric string
	}{
		{name: "final denial leaves the container running", final: busy, recheck: true,
			wantPhases: []string{"initial", "final"}, wantReason: []string{"at the final check before the stop", "HTTP 409", "workload busy"},
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="denied"} 1`},
		{name: "final transport error fails closed", final: approvalAnswerFor{hangUp: true}, recheck: true,
			wantPhases: []string{"initial", "final"}, wantReason: []string{"approval transport error", "final check"},
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="error"} 1`},
		{name: "final transport error with fail_open", final: approvalAnswerFor{hangUp: true}, recheck: true, failOpen: true, updated: true,
			wantPhases: []string{"initial", "final"}},
		{name: "final approval updates", final: ok, recheck: true, updated: true, wantPhases: []string{"initial", "final"},
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="approved"} 1`},
		{name: "without recheck only the initial request", final: busy, updated: true, wantPhases: []string{"initial"},
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="initial",result="approved"} 1`},
	}
	for _, path := range checkPaths {
		for _, tc := range tests {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				hookRan := filepath.Join(t.TempDir(), "pre-update-ran")
				var mu sync.Mutex
				var hookBeforeFinal bool
				url, log := phasedApprover(t, func(phase string) approvalAnswerFor {
					if phase == approvalPhaseInitial {
						return ok
					}
					mu.Lock()
					_, err := os.Stat(hookRan)
					hookBeforeFinal = err == nil
					mu.Unlock()
					return tc.final
				})
				h := newFakeHost(t, "1.47", func(c *config.Config) {
					c.Update.Approval = &config.ApprovalConfig{URL: url, RecheckBeforeStop: tc.recheck, FailOpen: tc.failOpen}
					c.Hooks.PreUpdate = []string{"host: touch " + hookRan}
				})
				rec := &recordedEvents{}
				h.u.send = rec.send
				h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
				if path == CheckPull {
					h.unreachable("example.com/app")
				}
				oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)

				if err := h.run(); err != nil {
					t.Fatal(err)
				}
				reqs := log.all()
				var phases []string
				for _, r := range reqs {
					phases = append(phases, r.Phase)
					if r.RequestID != reqs[0].RequestID {
						t.Errorf("request IDs %s and %s: the final request must name the same update", reqs[0].RequestID, r.RequestID)
					}
				}
				if strings.Join(phases, ",") != strings.Join(tc.wantPhases, ",") {
					t.Fatalf("phases %v, want %v", phases, tc.wantPhases)
				}
				if tc.recheck {
					mu.Lock()
					if !hookBeforeFinal {
						t.Error("the final request must come after the pre-update hooks")
					}
					mu.Unlock()
				}
				r := h.recordFor("web")
				ct := h.inspect("web")
				if tc.updated {
					if r.Decision != audit.DecisionUpdated || ct.Image != "sha256:v2" {
						t.Fatalf("decision %s, image %s: the update must go through", r.Decision, ct.Image)
					}
				} else {
					if r.Decision != audit.DecisionApprovalDeny {
						t.Fatalf("decision %s (%s)", r.Decision, r.Reason)
					}
					for _, want := range tc.wantReason {
						if !strings.Contains(r.Reason, want) {
							t.Errorf("reason %q lacks %q", r.Reason, want)
						}
					}
					if ct.ID != oldID || ct.Image != "sha256:v1" || ct.State == nil || !ct.State.Running {
						t.Fatalf("container %s %s running=%v: a final denial must leave it as it was", ct.ID, ct.Image, ct.State != nil && ct.State.Running)
					}
					if n := len(h.d.Calls("POST", "/stop")) + len(h.d.Calls("POST", "/rename")); n != 0 {
						t.Fatalf("%d stop/rename call(s): nothing may be stopped", n)
					}
					if got := h.d.ImageID(appRef); got != "sha256:v1" {
						t.Fatalf("%s points at %s; the tag must go back to the running image", appRef, got)
					}
					if evs := rec.of(notify.EventDenied, "web"); len(evs) != 1 || evs[0].Reason != r.Reason {
						t.Fatalf("events %+v: the denial is announced once with its reason", evs)
					}
					st := h.status("web")
					if st.Approval == nil || st.Approval.Phase != approvalPhaseFinal || st.Approval.Approved || !st.Pending {
						t.Fatalf("status %+v approval %+v", st, st.Approval)
					}
				}
				h.noLeftovers()
				if tc.wantMetric != "" && !strings.Contains(h.scrapeMetrics(), tc.wantMetric) {
					t.Errorf("metrics lack %s", tc.wantMetric)
				}
			})
		}
	}
}

func TestApprovalRetryAfterHoldsBackRequests(t *testing.T) {
	tests := []struct {
		name   string
		answer approvalAnswerFor
	}{
		{"Retry-After header", approvalAnswerFor{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {"3600"}}}},
		{"retry_after_seconds", approvalAnswerFor{status: http.StatusConflict, body: `{"approved":false,"reason":"maintenance window closed","retry_after_seconds":3600}`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, log := phasedApprover(t, func(string) approvalAnswerFor { return tc.answer })
			h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} })
			rec := &recordedEvents{}
			h.u.send = rec.send
			t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
			h.u.now = fixedClock(t0)
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)

			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if a := h.status("web").Approval; a == nil || !a.RetryAt.Equal(t0.Add(time.Hour)) {
				t.Fatalf("approval status %+v, want retry at %s", a, t0.Add(time.Hour))
			}
			h.u.now = fixedClock(t0.Add(30 * time.Minute))
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if n := len(log.all()); n != 1 {
				t.Fatalf("%d requests; none may be sent before the retry time", n)
			}
			recs := h.records("web")
			if len(recs) != 2 || recs[1].Decision != audit.DecisionApprovalDeny || !strings.Contains(recs[1].Reason, "not asked again before 2026-03-01T13:00:00Z") {
				t.Fatalf("records %+v", recs)
			}
			if evs := rec.of(notify.EventDenied, "web"); len(evs) != 1 {
				t.Fatalf("%d notices; a run that holds back sends none", len(evs))
			}
			if m := h.scrapeMetrics(); !strings.Contains(m, `dockpaum_approval_holds_total{host="fake"} 1`) ||
				!strings.Contains(m, `dockpaum_approval_requests_total{host="fake",phase="initial",result="denied"} 1`) {
				t.Fatalf("metrics:\n%s", grepLines(m, "dockpaum_approval"))
			}
			h.u.now = fixedClock(t0.Add(61 * time.Minute))
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if n := len(log.all()); n != 2 {
				t.Fatalf("%d requests after the retry time, want 2", n)
			}
		})
	}
}

func TestApprovalHoldOutlastsOtherGatesAndRestarts(t *testing.T) {
	url, log := phasedApprover(t, func(string) approvalAnswerFor {
		return approvalAnswerFor{status: http.StatusConflict, header: http.Header{"Retry-After": {"7200"}}}
	})
	h := newFakeHost(t, "1.47", func(c *config.Config) {
		nightlyWindow(c)
		c.Update.Approval = &config.ApprovalConfig{URL: url}
	})
	h.restart(durableState(t, filepath.Join(t.TempDir(), "state.json")), "")
	t0 := time.Date(2026, time.March, 1, 3, 0, 0, 0, time.UTC)
	h.u.now = fixedClock(t0)
	h.u.windowNow = fixedClock(inWindow)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.u.windowNow = fixedClock(outWindow)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if st := h.status("web"); st.Decision != string(audit.DecisionGatedWindow) || st.Approval == nil || st.Approval.RetryAt.IsZero() {
		t.Fatalf("status %+v approval %+v", st, st.Approval)
	}
	if err := h.u.state.Flush(); err != nil {
		t.Fatal(err)
	}
	st := h.u.state
	h.restart(st, "")
	h.u.now = fixedClock(t0.Add(time.Hour))
	h.u.windowNow = fixedClock(inWindow)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(log.all()); n != 1 {
		t.Fatalf("%d requests; the hold must survive other gates and a restart", n)
	}
	if got := h.status("web").Decision; got != string(audit.DecisionApprovalDeny) {
		t.Fatalf("decision %s", got)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestRecheckBeforeSelfUpdateHandOff(t *testing.T) {
	url, log := phasedApprover(t, func(phase string) approvalAnswerFor {
		if phase == approvalPhaseInitial {
			return approvalAnswerFor{status: http.StatusNoContent}
		}
		return approvalAnswerFor{status: http.StatusConflict, body: `{"approved":false,"reason":"paused"}`}
	})
	h, selfID := selfHost(t, ok, func() (int, string) { return 0, "" })
	cfg := &config.ApprovalConfig{URL: url, RecheckBeforeStop: true}
	h.u.deps.Config.Update.Approval = cfg
	h.u.approval = newApprovalGate(cfg, discard(), func() time.Time { return h.u.now() })

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	r := h.recordFor("dockpaum")
	if r.Decision != audit.DecisionApprovalDeny || !strings.Contains(r.Reason, "final check") || !strings.Contains(r.Reason, "paused") {
		t.Fatalf("record %+v", r)
	}
	for _, c := range h.d.Calls("POST", "/containers/create") {
		if strings.HasPrefix(c.Query.Get("name"), "dockpaum-self-update-") {
			t.Fatal("no helper may start after a final denial")
		}
	}
	if ct := h.inspect("dockpaum"); ct.ID != selfID || ct.State == nil || !ct.State.Running {
		t.Fatal("the daemon's container must stay as it was")
	}
	if got := h.d.ImageID("example.com/dockpaum:latest"); got != "sha256:dp1" {
		t.Fatalf("tag at %s; it goes back to the running image", got)
	}
	if reqs := log.all(); len(reqs) != 2 || reqs[1].Phase != approvalPhaseFinal || reqs[0].RequestID != reqs[1].RequestID {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestRecheckBeforeStopWithApproveOnFailure(t *testing.T) {
	incident := approvalAnswerFor{status: http.StatusOK, header: http.Header{"Content-Type": {"application/json"}},
		body: `{"approved":true,"reason":"incident open"}`}
	quiet := approvalAnswerFor{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {"3600"}}}
	tests := []struct {
		name       string
		final      approvalAnswerFor
		updated    bool
		wantReason string
		wantMetric string
	}{
		{name: "an OK status at the final check denies", final: incident,
			wantReason: "denied: %s answered HTTP 200 at the final check before the stop (approve_on: failure): incident open",
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="denied"} 1`},
		{name: "another status at the final check approves", final: quiet, updated: true,
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="approved"} 1`},
		{name: "an unreachable endpoint at the final check approves", final: approvalAnswerFor{hangUp: true}, updated: true,
			wantMetric: `dockpaum_approval_requests_total{host="fake",phase="final",result="error"} 1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, log := phasedApprover(t, func(phase string) approvalAnswerFor {
				if phase == approvalPhaseInitial {
					return quiet
				}
				return tc.final
			})
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				c.Update.Approval = &config.ApprovalConfig{URL: url, RecheckBeforeStop: true, ApproveOn: config.ApproveOnFailure}
			})
			rec := &recordedEvents{}
			h.u.send = rec.send
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if n := len(log.all()); n != 2 {
				t.Fatalf("%d approval requests, want the initial and the final one", n)
			}
			r := h.recordFor("web")
			ct := h.inspect("web")
			if tc.updated {
				if r.Decision != audit.DecisionUpdated || ct.Image != "sha256:v2" {
					t.Fatalf("decision %s (%s), image %s: the update must go through", r.Decision, r.Reason, ct.Image)
				}
				if evs := rec.of(notify.EventDenied, "web"); len(evs) != 0 {
					t.Fatalf("denial events %+v", evs)
				}
			} else {
				want := fmt.Sprintf(tc.wantReason, url)
				if r.Decision != audit.DecisionApprovalDeny || r.Reason != want {
					t.Fatalf("decision %s reason %q, want approval-denied %q", r.Decision, r.Reason, want)
				}
				if ct.ID != oldID || ct.Image != "sha256:v1" || len(h.d.Calls("POST", "/stop")) != 0 {
					t.Fatalf("container %s %s: a final denial must leave it as it was", ct.ID, ct.Image)
				}
				if evs := rec.of(notify.EventDenied, "web"); len(evs) != 1 || evs[0].Reason != r.Reason {
					t.Fatalf("events %+v: the denial is announced with its reason", evs)
				}
				if st := h.status("web"); st.Approval == nil || st.Approval.Approved || !st.Approval.RetryAt.IsZero() {
					t.Fatalf("approval status %+v: Retry-After does not hold back approve_on: failure", st.Approval)
				}
			}
			if !strings.Contains(h.scrapeMetrics(), tc.wantMetric) {
				t.Errorf("metrics lack %s", tc.wantMetric)
			}
			h.noLeftovers()
		})
	}
}
