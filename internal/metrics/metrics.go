package metrics

import (
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Registry struct {
	reg  *prometheus.Registry
	runs *tallyCounter

	RunsTotal            prometheus.Counter
	RunFailures          prometheus.Counter
	RunDuration          prometheus.Histogram
	UpdatesTotal         *prometheus.CounterVec
	UpdateFailures       *prometheus.CounterVec
	UpdateSkipped        *prometheus.CounterVec
	ContainersScanned    prometheus.Gauge
	LastRunTimestamp     prometheus.Gauge
	LastSuccessTimestamp prometheus.Gauge
	StartTime            prometheus.Gauge
	DryRun               prometheus.Gauge
	BuildInfo            *prometheus.GaugeVec

	ImageAgeSeconds *prometheus.GaugeVec

	RegistryCalls              *prometheus.CounterVec
	RegistryAuthCalls          *prometheus.CounterVec
	RegistryManifestCalls      *prometheus.CounterVec
	RegistryRateLimit          *prometheus.GaugeVec
	RegistryRateLimitRemaining *prometheus.GaugeVec

	AuditWriteFailures prometheus.Counter
	StateWriteFailures prometheus.Counter
	NotifierFailures   *prometheus.CounterVec

	JobRuns *prometheus.CounterVec

	HostsUnreachable prometheus.Gauge
	APITriggers      *prometheus.CounterVec

	ApprovalRequests *prometheus.CounterVec
	ApprovalHolds    *prometheus.CounterVec

	updates *updateCollector
}

var runDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

func NewRegistry() *Registry {
	r := prometheus.NewRegistry()
	runs := &tallyCounter{Counter: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dockpaum_runs_total", Help: "Total runs executed, check runs included",
	})}
	out := &Registry{
		reg:       r,
		runs:      runs,
		RunsTotal: runs,
		RunFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dockpaum_run_failures_total", Help: "Total runs that failed, check runs included",
		}),
		RunDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "dockpaum_run_duration_seconds", Help: "Duration of runs, check runs included",
			Buckets: runDurationBuckets,
		}),
		UpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_container_updates_total", Help: "Successful container updates, including completed self-updates (counted when the next daemon records them)",
		}, []string{"host", "container", "policy"}),
		UpdateFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_container_update_failures_total", Help: "Failed container updates, including failed self-updates",
		}, []string{"host", "container", "policy"}),
		UpdateSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_container_update_skipped_total", Help: "Updates skipped by a gate (reason=gated-window|gated-min-age|approval-denied|gated-digest-pin|gated-policy|monitor-only|gated-autoremove|gated-orchestrated|gated-mac-address)",
		}, []string{"host", "container", "policy", "reason"}),
		ContainersScanned: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_containers_scanned", Help: "Containers scanned in the last regular run",
		}),
		LastRunTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_last_run_timestamp_seconds", Help: "Unix time of the last completed run",
		}),
		LastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_last_successful_run_timestamp_seconds", Help: "Unix time of the last regular run that completed without errors (0 until one has)",
		}),
		StartTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_start_time_seconds", Help: "Unix time the daemon started",
		}),
		DryRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_dry_run", Help: "1 when the daemon runs in dry-run (observe-only) mode, else 0",
		}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dockpaum_build_info", Help: "Build information (always 1)",
		}, []string{"version", "commit"}),
		ImageAgeSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dockpaum_container_image_age_seconds",
			Help: "Age in seconds of the image the container is currently running",
		}, []string{"host", "container", "policy"}),
		RegistryCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_registry_calls_total",
			Help: "Registry tag-listing HTTP calls (result=ok|auth|error|rate_limited)",
		}, []string{"registry", "result"}),
		RegistryAuthCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_registry_auth_calls_total",
			Help: "Registry bearer token exchanges (result=ok|error)",
		}, []string{"registry", "result"}),
		RegistryManifestCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_registry_manifest_calls_total",
			Help: "Registry manifest HTTP calls, HEAD and GET (result=ok|auth|error|rate_limited)",
		}, []string{"registry", "result"}),
		RegistryRateLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dockpaum_registry_ratelimit_limit",
			Help: "Request quota the registry advertised last (RateLimit-Limit), per account (empty for anonymous requests)",
		}, []string{"registry", "account"}),
		RegistryRateLimitRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dockpaum_registry_ratelimit_remaining",
			Help: "Requests left in the quota the registry advertised last (RateLimit-Remaining), per account (empty for anonymous requests)",
		}, []string{"registry", "account"}),
		AuditWriteFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dockpaum_audit_write_failures_total", Help: "Audit log writes that failed",
		}),
		StateWriteFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dockpaum_state_write_failures_total", Help: "Writes of the state file that failed",
		}),
		NotifierFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_notifier_failures_total", Help: "Notifier delivery failures",
		}, []string{"name"}),
		JobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_job_runs_total", Help: "Scheduled job executions (result=ok|error|dry_run)",
		}, []string{"job", "result"}),
		HostsUnreachable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dockpaum_hosts_unreachable", Help: "Configured Docker hosts that could not be reached in the last run",
		}),
		APITriggers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_api_triggers_total", Help: "Runs requested through /internal-api/v1/update, /internal-api/v1/check or /v1/update (result=queued|already_queued|too_many_queued|unavailable)",
		}, []string{"result"}),
		ApprovalRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_approval_requests_total", Help: "Approval requests (phase=initial|final, result=approved|denied|error; error: no answer, decided by fail_open)",
		}, []string{"host", "phase", "result"}),
		ApprovalHolds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dockpaum_approval_holds_total", Help: "Containers not asked about because the approver's retry time had not passed",
		}, []string{"host"}),
		updates: newUpdateCollector(),
	}
	out.StartTime.Set(float64(time.Now().UnixNano()) / 1e9)
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	r.MustRegister(
		out.RunsTotal, out.RunFailures, out.RunDuration,
		out.UpdatesTotal, out.UpdateFailures, out.UpdateSkipped,
		out.ContainersScanned, out.ImageAgeSeconds,
		out.LastRunTimestamp, out.LastSuccessTimestamp, out.StartTime, out.DryRun, out.BuildInfo,
		out.RegistryCalls, out.RegistryAuthCalls, out.RegistryManifestCalls,
		out.RegistryRateLimit, out.RegistryRateLimitRemaining,
		out.AuditWriteFailures, out.StateWriteFailures, out.NotifierFailures,
		out.JobRuns, out.HostsUnreachable, out.APITriggers,
		out.ApprovalRequests, out.ApprovalHolds,
		out.updates,
	)
	return out
}

func (r *Registry) SetBuildInfo(version, commit string) {
	r.BuildInfo.WithLabelValues(version, commit).Set(1)
}

func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

type RegistryMetrics struct{ r *Registry }

func (r *Registry) ForRegistry() *RegistryMetrics { return &RegistryMetrics{r: r} }

func (a *RegistryMetrics) ListCall(host, result string) {
	a.r.RegistryCalls.WithLabelValues(host, result).Inc()
}
func (a *RegistryMetrics) AuthCall(host, result string) {
	a.r.RegistryAuthCalls.WithLabelValues(host, result).Inc()
}

func (a *RegistryMetrics) ManifestCall(host, result string) {
	a.r.RegistryManifestCalls.WithLabelValues(host, result).Inc()
}

func (a *RegistryMetrics) RateLimit(registry, account string, limit, remaining int) {
	a.r.RegistryRateLimit.WithLabelValues(registry, account).Set(float64(limit))
	a.r.RegistryRateLimitRemaining.WithLabelValues(registry, account).Set(float64(remaining))
}

type LastRun struct {
	Scanned, Updated, Failed int
}

func (r *Registry) WatchtowerHandler(last func() LastRun) http.Handler {
	wt := prometheus.NewRegistry()
	gauge := func(name, help string, value func(LastRun) int) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
			return float64(value(last()))
		})
	}
	wt.MustRegister(
		gauge("watchtower_containers_scanned", "Containers scanned in the most recent run (Watchtower-compatible)",
			func(l LastRun) int { return l.Scanned }),
		gauge("watchtower_containers_updated", "Containers updated in the most recent run (Watchtower-compatible)",
			func(l LastRun) int { return l.Updated }),
		gauge("watchtower_containers_failed", "Containers whose update failed in the most recent run (Watchtower-compatible)",
			func(l LastRun) int { return l.Failed }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "watchtower_scans_total", Help: "Runs since dockpaum started (Watchtower-compatible)",
		}, r.runs.value),
	)
	return promhttp.HandlerFor(prometheus.Gatherers{wt, r.reg}, promhttp.HandlerOpts{})
}

type tallyCounter struct {
	prometheus.Counter
	bits atomic.Uint64
}

func (c *tallyCounter) Inc() { c.Add(1) }

func (c *tallyCounter) Add(v float64) {
	c.Counter.Add(v)
	for {
		old := c.bits.Load()
		if c.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

func (c *tallyCounter) value() float64 { return math.Float64frombits(c.bits.Load()) }

type UpdateAvailable struct {
	Host, Container, Policy string
	Pending                 bool
}

func (r *Registry) SetUpdateSource(src func() []UpdateAvailable) {
	r.updates.src.Store(&src)
}

type updateCollector struct {
	desc *prometheus.Desc
	src  atomic.Pointer[func() []UpdateAvailable]
}

func newUpdateCollector() *updateCollector {
	return &updateCollector{desc: prometheus.NewDesc("dockpaum_container_update_available",
		"1 while a newer image is available for the container but not applied, else 0",
		[]string{"host", "container", "policy"}, nil)}
}

func (c *updateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *updateCollector) Collect(ch chan<- prometheus.Metric) {
	src := c.src.Load()
	if src == nil || *src == nil {
		return
	}
	seen := map[[3]string]bool{}
	for _, u := range (*src)() {
		key := [3]string{u.Host, u.Container, u.Policy}
		if seen[key] {
			continue
		}
		seen[key] = true
		v := 0.0
		if u.Pending {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, u.Host, u.Container, u.Policy)
	}
}
