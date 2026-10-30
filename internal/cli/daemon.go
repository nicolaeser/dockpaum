package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/banner"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	dlog "github.com/nicolaeser/dockpaum/internal/log"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/scheduler"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/server"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

func runDaemon(inv *invocation, stdout, stderr io.Writer) int {
	cfg, err := config.Load(inv.configPath)
	if err == nil {
		err = applyContainerNames(cfg, inv.names)
	}
	if err != nil {
		printf(stderr, "config: %s\n", configErr(err))
		return exitUsage
	}
	if inv.dryRun {
		cfg.DryRun = true
	}
	ver := buildVersion()

	if banner.IsTerminal(stderr) || dlog.IsText(cfg.Log) {
		banner.Print(stderr, banner.Options{
			Big:     "DOCKPAUM",
			Tagline: "automated docker container updates",
			Version: ver.Version,
			Commit:  ver.Commit,
			Date:    ver.Date,
			Extra:   fmt.Sprintf("%d host%s", len(cfg.Hosts), plural(len(cfg.Hosts))),
			Stops:   [][3]int{{250, 204, 100}, {240, 180, 41}, {214, 138, 31}},
			Color:   banner.ColorFor(stderr),
		})
	}

	logger := dlog.NewTo(stdout, cfg.Log)
	for _, w := range cfg.Warnings() {
		logger.Warn("config", "warning", w)
	}
	logger.Info("dockpaum starting",
		"version", ver.Version,
		"commit", ver.Commit,
		"hosts", len(cfg.Hosts),
		"dry_run", cfg.DryRun,
		"env_overrides", len(cfg.EnvOverrides()),
	)
	if cfg.RunOnce() && !inv.once {
		inv.once = true
		logger.Info("WATCHTOWER_RUN_ONCE is set: running a single check and exiting")
	}
	if len(inv.names) > 0 {
		logger.Warn("container names given as arguments (Watchtower compatibility form): only these containers are managed",
			"containers", cfg.Filters.IncludeNames, "config_equivalent", "filters.include_names")
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := metrics.NewRegistry()
	reg.SetBuildInfo(ver.Version, ver.Commit)
	if cfg.DryRun {
		reg.DryRun.Set(1)
	}
	notifier := notify.Build(cfg.Notify, logger)

	clients, err := docker.OpenAll(rootCtx, cfg.Hosts, logger)
	if err != nil {
		logger.Error("docker connect failed", "err", err)
		if isPermissionDenied(err) {
			for _, path := range unixSockets(cfg.Hosts, os.Getenv("DOCKER_HOST")) {
				a := inspectSocket(path)
				logger.Error(a.hint(), a.logArgs()...)
			}
		}
		return exitFailure
	}
	defer clients.Close()

	if err := loadDockerConfig(cfg, logger); err != nil {
		logger.Error("registry credentials", "err", err)
		return exitUsage
	}

	auditLog, err := audit.New(cfg.Audit, logger)
	if err != nil {
		logger.Error("audit init failed", "err", err)
		return exitFailure
	}
	defer func() { _ = auditLog.Close() }()
	auditLog.AttachFailureSink(reg.AuditWriteFailures)

	st := state.Open(statePath(cfg.State), logger)
	defer func() { _ = st.Close() }()
	st.AttachFailureSink(reg.StateWriteFailures)
	notifier.AttachLedger(stateLedger{st})

	upd, err := updater.New(updater.Deps{
		Clients:  clients,
		Notifier: notifier,
		Metrics:  reg,
		Logger:   logger,
		Config:   cfg,
		Audit:    auditLog,
		State:    st,
		Instance: cfg.Instance,
	})
	if err != nil {
		logger.Error("updater init failed", "err", err)
		return exitFailure
	}
	notifier.SetInstance(upd.Instance())
	reg.SetUpdateSource(func() []metrics.UpdateAvailable { return updateAvailability(upd.Containers()) })

	if inv.once {
		opts := updater.RunOptions{Trigger: "cli", Containers: inv.containers, Policy: updater.NormalizePolicy(inv.policy)}
		if n := len(upd.PolicySchedules()); n > 0 && opts.Policy == "" && len(opts.Containers) == 0 {
			logger.Warn("--once runs the regular update only: containers whose policy has its own schedule are left out (use --policy or --container)",
				"policy_schedules", n)
		}
		stopDrain := context.AfterFunc(rootCtx, upd.Drain)
		defer stopDrain()
		err := upd.RunWith(rootCtx, opts)
		notifier.Wait(20 * time.Second)
		if err != nil {
			logger.Error("run failed", "err", err)
			return exitFailure
		}
		return exitOK
	}

	sched, err := scheduler.New(cfg.Schedule, upd, logger)
	if err != nil {
		logger.Error("scheduler init failed", "err", err)
		return exitFailure
	}
	sched.SetRunOnStart(cfg.RunOnStart())
	upd.SetNextRun(sched.NextRun)

	ready := func() error {
		if !sched.Started() {
			return errors.New("scheduler not started")
		}
		if upd.Draining() {
			return errors.New("shutting down")
		}
		if down := clients.Unreachable(); len(down) > 0 {
			return fmt.Errorf("docker host(s) unreachable: %s", strings.Join(down, ", "))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return clients.Ping(ctx)
	}
	api := server.API{
		Status:     func() any { return upd.Status(ver.Version) },
		Containers: containersHook(upd.Containers),
		Summary: summarySource{
			statuses: upd.Containers, recent: auditLog.Recent, nextRun: sched.NextRun, now: time.Now,
			dryRun: cfg.DryRun, version: ver.Version, instance: upd.Instance(),
		}.summary,
		Busy: func() server.Busy {
			r, busy := upd.Busy()
			return server.Busy{Busy: busy, RunID: r.ID, StartedAt: r.Started}
		},
		Trigger: func(req server.TriggerRequest) string {
			opts := updater.RunOptions{Trigger: "api", Containers: req.Containers, Policy: updater.NormalizePolicy(req.Policy), DryRun: req.DryRun}
			key := opts.Key()
			runFn := func(ctx context.Context) error { return upd.RunWith(ctx, opts) }
			if len(req.Images) > 0 {
				images := slices.Sorted(slices.Values(req.Images))
				key += ";images=" + strings.Join(images, ",")
				runFn = func(ctx context.Context) error {
					names, conflicts, err := imageTargets(ctx, clients, cfg.Filters.IncludeStopped, req)
					if err != nil {
						return fmt.Errorf("resolve image filter %v (nothing updated): %w", images, err)
					}
					if len(conflicts) > 0 {
						logger.Warn("image filter: these names also belong to containers running other images on another host, so they are left out",
							"images", images, "containers", conflicts)
					}
					if len(names) == 0 {
						logger.Warn("the requested images match no container: nothing to update", "images", images, "containers", req.Containers)
						return nil
					}
					o := opts
					o.Containers = names
					return upd.RunWith(ctx, o)
				}
			}
			res := sched.Trigger(key, runFn)
			result := map[scheduler.TriggerResult]string{
				scheduler.TriggerQueued:    "queued",
				scheduler.TriggerDuplicate: "already-queued",
				scheduler.TriggerStopped:   "unavailable",
				scheduler.TriggerBusy:      "too-many-queued",
			}[res]
			reg.APITriggers.WithLabelValues(strings.ReplaceAll(result, "-", "_")).Inc()
			requested := "update requested through the API"
			if req.DryRun {
				requested = "check requested through the API"
			}
			logger.Info(requested, "result", result, "endpoint", req.Endpoint,
				"containers", opts.Containers, "policy", opts.Policy, "images", req.Images, "dry_run", opts.DryRun)
			return result
		},
		WatchtowerMetrics: reg.WatchtowerHandler(func() metrics.LastRun {
			r := upd.LastRun()
			if r == nil {
				return metrics.LastRun{}
			}
			return metrics.LastRun{Scanned: r.Scanned, Updated: r.Updated, Failed: r.Failed}
		}),
		WatchtowerCompat: cfg.HTTP.WatchtowerCompat,
	}
	srv := server.New(cfg.HTTP, reg.Handler(), logger, ver, ready, api)
	for _, ps := range upd.PolicySchedules() {
		if err := sched.AddPolicy(ps.PolicyID, ps.Spec, func(ctx context.Context) error {
			return upd.RunPolicy(ctx, ps.PolicyID)
		}); err != nil {
			logger.Error("scheduler init failed", "err", err)
			return exitFailure
		}
	}
	for _, j := range cfg.Jobs {
		if err := sched.AddJob(j.Name, j.CronSpec(), func(ctx context.Context) error {
			return upd.RunJob(ctx, j.Name)
		}); err != nil {
			logger.Error("scheduler init failed", "err", err)
			return exitFailure
		}
	}
	sched.Start(rootCtx)
	notifier.Notify(rootCtx, startedEvent(cfg, ver.Version, sched.NextRun(), time.Now()))
	go func() {
		if err := srv.Run(rootCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "err", err)
		}
	}()

	<-rootCtx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	notifier.Notify(stopCtx, stoppingEvent(ver.Version, time.Now()))
	upd.Drain()
	logger.Info("shutdown signal received, draining in-flight updates")

	drainTimeout := 60 * time.Second
	sched.Stop(drainTimeout)
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sCancel()
	_ = srv.Shutdown(shutdownCtx)
	notifier.Wait(20 * time.Second)
	logger.Info("dockpaum stopped")
	return exitOK
}

func runHealthProbe(configPath string, stderr io.Writer) int {
	cfg, err := config.Validate(configPath)
	if err != nil {
		printf(stderr, "healthcheck: config: %s\n", configErr(err))
		return exitFailure
	}
	if !cfg.HTTP.IsEnabled() {
		return exitOK
	}
	host, port, err := net.SplitHostPort(cfg.HTTP.ListenAddr())
	if err != nil {
		printf(stderr, "healthcheck: bad listen addr %q: %v\n", cfg.HTTP.ListenAddr(), err)
		return exitFailure
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/internal-api/health", nil)
	if err != nil {
		printf(stderr, "healthcheck: %v\n", err)
		return exitFailure
	}
	if token := cmp.Or(cfg.HTTP.ReadToken, cfg.HTTP.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		printf(stderr, "healthcheck: %v\n", err)
		return exitFailure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		printf(stderr, "healthcheck: HTTP %d\n", resp.StatusCode)
		return exitFailure
	}
	return exitOK
}

func runHelper(configPath, oldID, newImage, expectedID string, delay time.Duration) error {
	logger := dlog.New(config.LogConfig{Level: "info", Format: "json"}).With("mode", "self-update-helper")
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	hosts := []config.HostConfig{{Name: "local"}}
	var st *state.Store
	if cfg, err := config.Validate(configPath); err != nil {
		source := configPath
		if remoteConfig() {
			source = "$" + config.EnvConfigURL
		}
		logger.Warn("self-update: config not readable, using the local Docker host", "config", source, "err", configErr(err))
	} else {
		if len(cfg.Hosts) > 0 {
			hosts = cfg.Hosts
		}
		st = state.Open(statePath(cfg.State), logger)
		defer func() { _ = st.Close() }()
	}

	var lastOpenErr error
	for _, h := range hosts {
		clients, err := docker.OpenAll(ctx, []config.HostConfig{h}, logger)
		if err != nil {
			lastOpenErr = err
			logger.Warn("self-update: host unreachable from helper (skipping)",
				"host", h.Name, "err", err)
			continue
		}
		c := clients.List()[0]
		o := selfupdate.DefaultHelperOptions(oldID, newImage, expectedID, delay)
		o.RecordedDefaults = selfupdate.RecordedDefaults(st, h.Name)
		err = selfupdate.RunHelperIfPresentWith(ctx, c, o, logger)
		clients.Close()
		if err == nil {
			return nil
		}
		if !errors.Is(err, selfupdate.ErrNotFoundOnHost) {
			return err
		}
	}
	if lastOpenErr != nil {
		return fmt.Errorf("container %s not found and at least one host was unreachable: %w", oldID, lastOpenErr)
	}
	return fmt.Errorf("container %s not found on any configured host", oldID)
}
