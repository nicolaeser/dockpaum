package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/server"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

var goldens = map[string]string{
	"v1-containers.json": `{
  "containers": [
    {
      "host": "local",
      "container": "db",
      "container_id": "c0ffee",
      "image": "postgres:16.4",
      "policy": "database",
      "current": {
        "tag": "16.4",
        "digest": "sha256:1111"
      },
      "candidate": {
        "image": "postgres:16.6",
        "tag": "16.6",
        "digest": "sha256:2222",
        "found_at": "2026-10-16T04:00:00Z",
        "first_seen": "2026-10-16T02:00:00Z"
      },
      "update": "available",
      "pending": true,
      "decision": "gated-min-age",
      "reason": "new image age 2h0m0s \u003c min 24h0m0s",
      "checked_at": "2026-10-16T04:00:00Z",
      "eligible_at": "2026-10-17T02:00:00Z",
      "run_id": "run-1"
    },
    {
      "host": "local",
      "container": "web",
      "container_id": "beef",
      "image": "nginx:latest",
      "policy": "default",
      "current": {
        "tag": "latest",
        "digest": "sha256:3333"
      },
      "update": "none",
      "pending": false,
      "decision": "skipped",
      "reason": "image up-to-date",
      "checked_at": "2026-10-16T04:00:00Z",
      "run_id": "run-1"
    },
    {
      "host": "edge",
      "container": "cache",
      "image": "redis:7",
      "policy": "default",
      "current": {
        "tag": "7",
        "digest": "sha256:4444"
      },
      "update": "unknown",
      "pending": false,
      "decision": "gated-window",
      "reason": "outside policy \"default\" window",
      "checked_at": "2026-10-16T04:00:00Z",
      "run_id": "run-1"
    }
  ]
}
`,
	"v1-containers-pending.json": `{
  "containers": [
    {
      "host": "local",
      "container": "db",
      "container_id": "c0ffee",
      "image": "postgres:16.4",
      "policy": "database",
      "current": {
        "tag": "16.4",
        "digest": "sha256:1111"
      },
      "candidate": {
        "image": "postgres:16.6",
        "tag": "16.6",
        "digest": "sha256:2222",
        "found_at": "2026-10-16T04:00:00Z",
        "first_seen": "2026-10-16T02:00:00Z"
      },
      "update": "available",
      "pending": true,
      "decision": "gated-min-age",
      "reason": "new image age 2h0m0s \u003c min 24h0m0s",
      "checked_at": "2026-10-16T04:00:00Z",
      "eligible_at": "2026-10-17T02:00:00Z",
      "run_id": "run-1"
    }
  ]
}
`,
	"v1-summary.json": `{
  "monitored": 3,
  "pending_updates": 1,
  "gated": 2,
  "updated_24h": 4,
  "failed_24h": 4,
  "dry_run": true,
  "next_run_at": "2026-10-17T04:00:00Z",
  "version": "v1.2.3",
  "instance": "edge-1"
}
`,
}

func goldenJSON(t *testing.T, name string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(body), "", "  "); err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", name, err, body)
	}
	buf.WriteByte('\n')
	if want := goldens[name]; buf.String() != want {
		t.Errorf("%s changed, a machine contract:\ngot:\n%s\nwant:\n%s", name, buf.Bytes(), want)
	}
}

func fixtureStatuses() []updater.ContainerStatus {
	checked := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	return []updater.ContainerStatus{
		{
			Host: "local", Container: "db", ContainerID: "c0ffee", Image: "postgres:16.4", Policy: "database",
			Current: updater.ImageVersion{Tag: "16.4", Digest: "sha256:1111"},
			Candidate: &updater.Candidate{
				Image: "postgres:16.6", ImageVersion: updater.ImageVersion{Tag: "16.6", Digest: "sha256:2222"},
				FoundAt: checked, FirstSeen: checked.Add(-2 * time.Hour),
			},
			Update: updater.UpdateAvailable, Pending: true, Decision: string(audit.DecisionGatedAge),
			Reason: "new image age 2h0m0s < min 24h0m0s", CheckedAt: checked,
			EligibleAt: checked.Add(22 * time.Hour), RunID: "run-1",
		},
		{
			Host: "local", Container: "web", ContainerID: "beef", Image: "nginx:latest", Policy: "default",
			Current: updater.ImageVersion{Tag: "latest", Digest: "sha256:3333"},
			Update:  updater.UpdateNone, Decision: string(audit.DecisionSkipped), Reason: "image up-to-date",
			CheckedAt: checked, RunID: "run-1",
		},
		{
			Host: "edge", Container: "cache", Image: "redis:7", Policy: "default",
			Current: updater.ImageVersion{Tag: "7", Digest: "sha256:4444"},
			Update:  updater.UpdateUnknown, Decision: string(audit.DecisionGatedWindow),
			Reason: `outside policy "default" window`, CheckedAt: checked, RunID: "run-1",
		},
	}
}

func TestReadEndpointsGolden(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	next := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	var since time.Time
	recent := func(s time.Time) map[audit.Decision]int {
		since = s
		return map[audit.Decision]int{
			audit.DecisionUpdated: 3, audit.DecisionSelfUpdateCompleted: 1,
			audit.DecisionFailed: 1, audit.DecisionRolledBack: 1, audit.DecisionCheckFailed: 2,
			audit.DecisionSkipped: 40, audit.DecisionDryRun: 5,
		}
	}
	src := summarySource{
		statuses: fixtureStatuses, recent: recent, nextRun: func() time.Time { return next },
		now: func() time.Time { return now }, dryRun: true, version: "v1.2.3", instance: "edge-1",
	}
	s := server.New(config.HTTPConfig{}, http.NotFoundHandler(), discardLogger(), server.Version{}, nil,
		server.API{Containers: containersHook(fixtureStatuses), Summary: src.summary})

	get := func(path string) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	goldenJSON(t, "v1-containers.json", get("/internal-api/v1/containers"))
	goldenJSON(t, "v1-containers-pending.json", get("/internal-api/v1/containers?pending=true"))
	goldenJSON(t, "v1-summary.json", get("/internal-api/v1/summary"))
	if !since.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("summary counted decisions since %s, want the last 24h", since)
	}
}

func TestSummaryWithoutNextRun(t *testing.T) {
	src := summarySource{
		statuses: func() []updater.ContainerStatus { return nil },
		recent:   func(time.Time) map[audit.Decision]int { return nil },
		nextRun:  func() time.Time { return time.Time{} },
		now:      time.Now,
	}
	got := src.summary()
	if got.NextRunAt != nil || got.Monitored != 0 || got.Updated24h != 0 {
		t.Errorf("summary = %+v, want zero counts and no next run", got)
	}
	src.nextRun = nil
	if got := src.summary(); got.NextRunAt != nil {
		t.Errorf("summary without a scheduler = %+v", got)
	}
}
