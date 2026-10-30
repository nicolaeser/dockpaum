package metrics

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/registry"
)

func TestRegistryExposesMetrics(t *testing.T) {
	r := NewRegistry()
	r.SetBuildInfo("v1.2.3", "abc123")
	r.RunsTotal.Inc()
	r.UpdatesTotal.WithLabelValues("local", "web", "default").Inc()
	r.ForRegistry().ListCall("docker.io", "ok")
	r.ForRegistry().AuthCall("docker.io", "ok")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"dockpaum_runs_total 1",
		`dockpaum_build_info{commit="abc123",version="v1.2.3"} 1`,
		`dockpaum_container_updates_total{container="web",host="local",policy="default"} 1`,
		`dockpaum_registry_calls_total{registry="docker.io",result="ok"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestRuntimeCollectorsExposed(t *testing.T) {
	r := NewRegistry()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing runtime metric %q", want)
		}
	}
	if runtime.GOOS == "linux" && !strings.Contains(body, "process_resident_memory_bytes") {
		t.Error("metrics output missing process_resident_memory_bytes on linux")
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestRunDurationBucketsCoverRealRuns(t *testing.T) {
	r := NewRegistry()
	r.RunDuration.Observe(42)
	r.RunDuration.Observe(1500)
	body := scrape(t, r.Handler())
	for _, want := range []string{
		`dockpaum_run_duration_seconds_bucket{le="30"} 0`,
		`dockpaum_run_duration_seconds_bucket{le="60"} 1`,
		`dockpaum_run_duration_seconds_bucket{le="1800"} 2`,
		`dockpaum_run_duration_seconds_bucket{le="3600"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if strings.Contains(body, `le="0.005"`) {
		t.Error("run duration still uses the default sub-second buckets")
	}
}

func TestStartTimeAndDryRun(t *testing.T) {
	before := float64(time.Now().Unix())
	r := NewRegistry()
	r.DryRun.Set(1)
	body := scrape(t, r.Handler())
	if !strings.Contains(body, "dockpaum_dry_run 1") {
		t.Error("metrics output missing dockpaum_dry_run 1")
	}
	if !strings.Contains(body, "dockpaum_last_successful_run_timestamp_seconds 0") {
		t.Error("last successful run must start at 0")
	}
	var start float64
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, "dockpaum_start_time_seconds "); ok {
			var err error
			if start, err = strconv.ParseFloat(v, 64); err != nil {
				t.Fatal(err)
			}
		}
	}
	if start < before || start > float64(time.Now().Unix())+1 {
		t.Errorf("dockpaum_start_time_seconds = %v, want about %v", start, before)
	}
}

func TestWatchtowerHandler(t *testing.T) {
	r := NewRegistry()
	var mu sync.Mutex
	last := LastRun{}
	h := r.WatchtowerHandler(func() LastRun { mu.Lock(); defer mu.Unlock(); return last })

	body := scrape(t, h)
	for _, want := range []string{
		"watchtower_containers_scanned 0",
		"watchtower_containers_updated 0",
		"watchtower_containers_failed 0",
		"watchtower_scans_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("before the first run: output missing %q", want)
		}
	}

	r.RunsTotal.Inc()
	r.RunsTotal.Inc()
	r.RunsTotal.Add(1)
	mu.Lock()
	last = LastRun{Scanned: 14, Updated: 2, Failed: 1}
	mu.Unlock()
	body = scrape(t, h)
	for _, want := range []string{
		"watchtower_containers_scanned 14",
		"watchtower_containers_updated 2",
		"watchtower_containers_failed 1",
		"watchtower_scans_total 3",
		"dockpaum_runs_total 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q", want)
		}
	}
	values := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "watchtower") {
			parts := strings.Split(line, " ")
			values[parts[0]] = parts[1]
		}
	}
	for name, want := range map[string]string{
		"watchtower_containers_scanned": "14",
		"watchtower_containers_updated": "2",
		"watchtower_containers_failed":  "1",
	} {
		if values[name] != want {
			t.Errorf("Homepage-style parse: %s = %q, want %q", name, values[name], want)
		}
	}
}

func TestTallyCounterRejectsNegative(t *testing.T) {
	r := NewRegistry()
	r.RunsTotal.Inc()
	func() {
		defer func() { _ = recover() }()
		r.RunsTotal.Add(-1)
	}()
	if got := r.runs.value(); got != 1 {
		t.Errorf("tally after a rejected negative Add = %v, want 1", got)
	}
}

var (
	_ registry.Metrics          = (*RegistryMetrics)(nil)
	_ registry.ManifestMetrics  = (*RegistryMetrics)(nil)
	_ registry.RateLimitMetrics = (*RegistryMetrics)(nil)
)

func TestRegistryManifestAndRateLimitMetrics(t *testing.T) {
	r := NewRegistry()
	m := r.ForRegistry()
	m.ManifestCall("registry-1.docker.io", "ok")
	m.ManifestCall("registry-1.docker.io", "ok")
	m.ManifestCall("ghcr.io", "rate_limited")
	m.RateLimit("registry-1.docker.io", "", 100, 97)
	m.RateLimit("registry-1.docker.io", "alice", 200, 150)
	m.RateLimit("registry-1.docker.io", "", 100, 95)
	body := scrape(t, r.Handler())
	for _, want := range []string{
		`dockpaum_registry_manifest_calls_total{registry="registry-1.docker.io",result="ok"} 2`,
		`dockpaum_registry_manifest_calls_total{registry="ghcr.io",result="rate_limited"} 1`,
		`dockpaum_registry_ratelimit_limit{account="",registry="registry-1.docker.io"} 100`,
		`dockpaum_registry_ratelimit_remaining{account="",registry="registry-1.docker.io"} 95`,
		`dockpaum_registry_ratelimit_limit{account="alice",registry="registry-1.docker.io"} 200`,
		`dockpaum_registry_ratelimit_remaining{account="alice",registry="registry-1.docker.io"} 150`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if strings.Contains(body, "dockpaum_registry_calls_total{") {
		t.Error("manifest calls were counted as tag-listing calls")
	}
}

func TestStateWriteFailures(t *testing.T) {
	r := NewRegistry()
	if body := scrape(t, r.Handler()); !strings.Contains(body, "dockpaum_state_write_failures_total 0") {
		t.Error("dockpaum_state_write_failures_total must be exported from the start")
	}
	r.StateWriteFailures.Inc()
	if body := scrape(t, r.Handler()); !strings.Contains(body, "dockpaum_state_write_failures_total 1") {
		t.Error("dockpaum_state_write_failures_total did not count")
	}
}

func TestUpdateAvailableFollowsTheSource(t *testing.T) {
	r := NewRegistry()
	if body := scrape(t, r.Handler()); strings.Contains(body, "dockpaum_container_update_available{") {
		t.Error("update_available reported before a source was set")
	}
	var mu sync.Mutex
	current := []UpdateAvailable{
		{Host: "local", Container: "web", Policy: "default", Pending: true},
		{Host: "local", Container: "db", Policy: "pinned"},
		{Host: "local", Container: "web", Policy: "default"},
	}
	r.SetUpdateSource(func() []UpdateAvailable { mu.Lock(); defer mu.Unlock(); return current })
	body := scrape(t, r.Handler())
	for _, want := range []string{
		`dockpaum_container_update_available{container="web",host="local",policy="default"} 1`,
		`dockpaum_container_update_available{container="db",host="local",policy="pinned"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
	mu.Lock()
	current = current[1:2]
	mu.Unlock()
	body = scrape(t, r.Handler())
	if strings.Contains(body, `container="web"`) {
		t.Error("a removed container is still reported")
	}
	if !strings.Contains(body, `dockpaum_container_update_available{container="db",host="local",policy="pinned"} 0`) {
		t.Error("the remaining container is missing")
	}
}
