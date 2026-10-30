package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/state"
)

const (
	HelperLabel          = "dockpaum.self-update.helper"
	HelperContainerLabel = "dockpaum.self-update.container"
	HelperImageLabel     = "dockpaum.self-update.image"
	HelperImageIDLabel   = "dockpaum.self-update.image-id"
	CheckLabel           = "dockpaum.self-update.check"
)

const (
	helperStopTimeout      = 300
	helperRevertGrace      = 120
	interruptedStopTimeout = 60
	stopGrace              = 30 * time.Second
	minHelperHealthTimeout = 3 * time.Minute
	maxHelperHealthTimeout = 15 * time.Minute
	helperHealthGrace      = 30 * time.Second
	helperHealthPoll       = time.Second
	configCheckTimeout     = 2 * time.Minute
	maxReportedOutput      = 600
)

func recoveryContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

func ConfigArg(fc *docker.FullContainer) string {
	if fc == nil || fc.Config == nil {
		return ""
	}
	args := append(append([]string(nil), fc.Config.Entrypoint...), fc.Config.Cmd...)
	for i, a := range args {
		for _, name := range []string{"--config", "-config"} {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
	}
	return ""
}

func CheckAccess(self *docker.FullContainer, endpoint string) error {
	if self == nil || self.Config == nil {
		return errors.New("own container not inspected")
	}
	if endpoint == "" {
		endpoint = envValue(self.Config.Env, "DOCKER_HOST")
	}
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		sock := strings.TrimPrefix(endpoint, "unix://")
		if !mountedAt(self, sock) {
			return fmt.Errorf("the Docker socket %s is not mounted into dockpaum's container, so a helper cannot be given the same Docker access", sock)
		}
	case strings.HasPrefix(endpoint, "tcp://"), strings.HasPrefix(endpoint, "http://"), strings.HasPrefix(endpoint, "https://"):
		if self.HostConfig != nil && self.HostConfig.NetworkMode.IsNone() {
			return fmt.Errorf("dockpaum reaches Docker at %s but its container has no network", endpoint)
		}
	}
	return nil
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func mountedAt(fc *docker.FullContainer, p string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(s, "/")
		if s == "/var/run" {
			return "/run"
		}
		if rest, ok := strings.CutPrefix(s, "/var/run/"); ok {
			return "/run/" + rest
		}
		return s
	}
	target := norm(p)
	covers := func(dst string) bool {
		dst = norm(dst)
		return dst != "" && (target == dst || strings.HasPrefix(target, dst+"/"))
	}
	if fc.HostConfig == nil {
		return false
	}
	for _, b := range fc.HostConfig.Binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 && covers(parts[1]) {
			return true
		}
	}
	for _, m := range fc.HostConfig.Mounts {
		if covers(m.Target) {
			return true
		}
	}
	return false
}

func oneShotLabels(key, value, scope string) map[string]string {
	labels := map[string]string{key: value, policy.LabelEnable: "false"}
	if scope != "" {
		labels[policy.LabelScope] = scope
	}
	return labels
}

var ErrConfigRejected = errors.New("the new image rejects the configuration")

func ValidateConfig(ctx context.Context, c *docker.Client, self *docker.FullContainer, image, configPath, scope string, logger *slog.Logger) error {
	cmd := []string{"--validate"}
	if configPath != "" {
		cmd = append(cmd, "--config", configPath)
	}
	name := fmt.Sprintf("dockpaum-config-check-%d", time.Now().UnixNano())
	id, err := c.CreateOneShotLike(ctx, docker.OneShot{
		Name: name, Image: image, Cmd: cmd, Labels: oneShotLabels(CheckLabel, self.ID, scope), Like: self,
	})
	defer func() {
		if id == "" {
			return
		}
		rctx, cancel := recoveryContext(ctx, time.Minute)
		defer cancel()
		if err := c.RemoveOneShot(rctx, id); err != nil {
			logger.Warn("self-update: remove the configuration check container failed", "container", name, "err", err)
		}
	}()
	if err != nil {
		return fmt.Errorf("create configuration check: %w", err)
	}
	if err := c.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start configuration check: %w", err)
	}
	code, err := c.WaitExit(ctx, id, configCheckTimeout)
	if err != nil {
		return fmt.Errorf("configuration check: %w", err)
	}
	if code == 0 {
		return nil
	}
	out, lerr := c.LogsTail(ctx, id, 20)
	if lerr != nil {
		logger.Warn("self-update: read the configuration check output failed", "err", lerr)
	}
	logger.Warn("self-update: the new image rejects the configuration", "image", image, "exit_code", code,
		"output", Redact(summarize(out)))
	return fmt.Errorf("%w (exit %d); its output is in the dockpaum log, or run the new image with --validate to see it",
		ErrConfigRejected, code)
}

var (
	urlRE      = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>` + "`" + `]+`)
	envRE      = regexp.MustCompile(`\b([A-Z][A-Z0-9_]*)=[^\s"']+`)
	backtickRE = regexp.MustCompile("`[^`]*`")
)

func Redact(out string) string {
	out = urlRE.ReplaceAllStringFunc(out, func(match string) string {
		raw := strings.TrimRight(match, ":,.;)]}")
		tail := match[len(raw):]
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			scheme, _, _ := strings.Cut(raw, "://")
			return scheme + "://***" + tail
		}
		masked := u.Scheme + "://" + u.Host
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			masked += "/***"
		}
		return masked + tail
	})
	out = envRE.ReplaceAllString(out, "$1=***")
	return backtickRE.ReplaceAllString(out, "`***`")
}

func summarize(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "no output"
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > maxReportedOutput {
		out = "…" + out[len(out)-maxReportedOutput:]
	}
	return out
}

type SpawnOptions struct {
	Self        *docker.FullContainer
	ConfigPath  string
	NewImage    string
	ExpectedID  string
	HelperImage string
	Delay       time.Duration
	Scope       string
}

func Spawn(ctx context.Context, c *docker.Client, o SpawnOptions, logger *slog.Logger) (string, error) {
	if o.Self == nil {
		return "", errors.New("own container not inspected")
	}
	if running, err := Running(ctx, c, o.Scope); err != nil {
		return "", fmt.Errorf("look for a running helper: %w", err)
	} else if running > 0 {
		return "", errors.New("a self-update helper is still running")
	}
	helperImage := o.HelperImage
	if helperImage == "" {
		helperImage = o.NewImage
	}
	delaySecs := int(o.Delay.Seconds())
	if delaySecs < 1 {
		delaySecs = 5
	}
	cmd := []string{
		"--self-update-helper", o.Self.ID,
		"--new-image", o.NewImage,
		"--delay", strconv.Itoa(delaySecs),
	}
	if o.ExpectedID != "" {
		cmd = append(cmd, "--expect-id", o.ExpectedID)
	}
	if o.ConfigPath != "" {
		cmd = append(cmd, "--config", o.ConfigPath)
	}
	labels := oneShotLabels(HelperLabel, o.Self.ID, o.Scope)
	labels[HelperContainerLabel] = o.Self.Name
	labels[HelperImageLabel] = o.NewImage
	if o.ExpectedID != "" {
		labels[HelperImageIDLabel] = o.ExpectedID
	}
	name := fmt.Sprintf("dockpaum-self-update-%d", time.Now().UnixNano())
	logger.Info("spawning self-update helper",
		"helper", name, "old_id", o.Self.ID, "new_image", o.NewImage, "helper_image", helperImage, "delay_s", delaySecs)
	stop := o.Self.EffectiveStopTimeout(helperStopTimeout) + helperRevertGrace
	id, err := c.CreateOneShotLike(ctx, docker.OneShot{
		Name: name, Image: helperImage, Cmd: cmd, Labels: labels, Like: o.Self, StopTimeout: stop,
	})
	if err != nil {
		removeQuietly(ctx, c, id, logger)
		return "", fmt.Errorf("create helper: %w", err)
	}
	if err := c.StartContainer(ctx, id); err != nil {
		removeQuietly(ctx, c, id, logger)
		return "", fmt.Errorf("start helper: %w", err)
	}
	return name, nil
}

func removeQuietly(ctx context.Context, c *docker.Client, id string, logger *slog.Logger) {
	if id == "" {
		return
	}
	rctx, cancel := recoveryContext(ctx, time.Minute)
	defer cancel()
	if err := c.RemoveOneShot(rctx, id); err != nil {
		logger.Warn("self-update: remove the failed helper container failed", "id", id, "err", err)
	}
}

func SpawnHelper(ctx context.Context, c *docker.Client, oldID, newImage, expectedID, helperImage string, delay time.Duration, logger *slog.Logger) error {
	self, err := c.Inspect(ctx, oldID)
	if err != nil {
		return fmt.Errorf("inspect own container: %w", err)
	}
	_, err = Spawn(ctx, c, SpawnOptions{
		Self: self, ConfigPath: ConfigArg(self), NewImage: newImage, ExpectedID: expectedID,
		HelperImage: helperImage, Delay: delay,
	}, logger)
	return err
}

type Outcome struct {
	Helper    string
	Container string
	OldID     string
	Image     string
	ImageID   string
	ExitCode  int
	Err       error
}

const helperErrorPrefix = "self-update helper: "

func helperActive(state string) bool {
	switch state {
	case "running", "restarting", "paused", "removing":
		return true
	}
	return false
}

func isHelper(ct docker.Container, scope string) bool {
	_, ok := ct.Labels[HelperLabel]
	return ok && ct.Labels[policy.LabelScope] == scope
}

func isCheck(ct docker.Container, scope string) bool {
	_, ok := ct.Labels[CheckLabel]
	return ok && ct.Labels[policy.LabelScope] == scope
}

func Running(ctx context.Context, c *docker.Client, scope string) (int, error) {
	list, err := c.ListContainers(ctx, true)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ct := range list {
		if isHelper(ct, scope) && helperActive(ct.State) {
			n++
		}
	}
	return n, nil
}

func Collect(ctx context.Context, c *docker.Client, scope string, logger *slog.Logger) (outcomes []Outcome, running int, err error) {
	return collect(ctx, c, scope, logger, true)
}

func Peek(ctx context.Context, c *docker.Client, scope string, logger *slog.Logger) (outcomes []Outcome, running int, err error) {
	return collect(ctx, c, scope, logger, false)
}

func collect(ctx context.Context, c *docker.Client, scope string, logger *slog.Logger, remove bool) (outcomes []Outcome, running int, err error) {
	list, err := c.ListContainers(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	for _, ct := range list {
		if isCheck(ct, scope) && !helperActive(ct.State) {
			if !remove {
				continue
			}
			if err := c.RemoveOneShot(ctx, ct.ID); err != nil {
				logger.Warn("self-update: remove a stale configuration check failed", "container", ct.Name, "err", err)
			}
			continue
		}
		if !isHelper(ct, scope) {
			continue
		}
		if helperActive(ct.State) {
			running++
			continue
		}
		if ct.State == "created" {
			if !remove {
				continue
			}
			if err := c.RemoveOneShot(ctx, ct.ID); err != nil {
				logger.Warn("self-update: remove a helper that never started failed", "helper", ct.Name, "err", err)
			}
			continue
		}
		o := Outcome{
			Helper: ct.Name, Container: ct.Labels[HelperContainerLabel],
			OldID: ct.Labels[HelperLabel], Image: ct.Labels[HelperImageLabel], ImageID: ct.Labels[HelperImageIDLabel],
		}
		st, serr := c.ContainerState(ctx, ct.ID)
		if serr != nil {
			logger.Warn("self-update: inspect a finished helper failed; it is kept for review", "helper", ct.Name, "err", serr)
			continue
		}
		o.ExitCode = st.ExitCode
		if st.ExitCode != 0 {
			out, lerr := c.LogsTail(ctx, ct.ID, 20)
			if lerr != nil {
				logger.Warn("self-update: read the helper output failed", "helper", ct.Name, "err", lerr)
			}
			o.Err = fmt.Errorf("helper %s exited %d: %s", ct.Name, st.ExitCode, helperError(out))
		}
		if remove {
			if err := c.RemoveOneShot(ctx, ct.ID); err != nil {
				logger.Warn("self-update: remove a finished helper failed; its outcome is recorded once it is removed", "helper", ct.Name, "err", err)
				continue
			}
		}
		outcomes = append(outcomes, o)
	}
	return outcomes, running, nil
}

func helperError(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), helperErrorPrefix); ok {
			return Redact(summarize(msg))
		}
	}
	return Redact(summarize(lines[len(lines)-1]))
}

type HelperOptions struct {
	OldID            string
	NewImage         string
	ExpectedID       string
	Delay            time.Duration
	StopTimeout      int
	HealthTimeout    time.Duration
	HealthGrace      time.Duration
	HealthPoll       time.Duration
	RecordedDefaults func(imageID string) (*docker.ImageDefaults, bool)
}

func DefaultHelperOptions(oldID, newImage, expectedID string, delay time.Duration) HelperOptions {
	return HelperOptions{
		OldID: oldID, NewImage: newImage, ExpectedID: expectedID, Delay: delay,
		StopTimeout: helperStopTimeout, HealthGrace: helperHealthGrace, HealthPoll: helperHealthPoll,
	}
}

func helperHealthTimeout(hc *container.HealthConfig) time.Duration {
	if hc == nil || len(hc.Test) == 0 || hc.Test[0] == "NONE" {
		return minHelperHealthTimeout
	}
	interval := hc.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	retries := hc.Retries
	if retries <= 0 {
		retries = 3
	}
	d := hc.StartPeriod + interval*time.Duration(retries+1) + 30*time.Second
	return min(max(d, minHelperHealthTimeout), maxHelperHealthTimeout)
}

func RunHelper(ctx context.Context, c *docker.Client, oldID, newImage, expectedID string, delay time.Duration, logger *slog.Logger) error {
	return RunHelperWith(ctx, c, DefaultHelperOptions(oldID, newImage, expectedID, delay), logger)
}

func RunHelperWith(ctx context.Context, c *docker.Client, o HelperOptions, logger *slog.Logger) error {
	if o.Delay > 0 {
		logger.Info("helper waiting before swap", "delay", o.Delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Delay):
		}
	}

	if o.ExpectedID != "" && o.NewImage != "" {
		gotID, err := c.ImageID(ctx, o.NewImage)
		if err != nil {
			return fmt.Errorf("verify new image: %w", err)
		}
		if gotID != o.ExpectedID {
			return fmt.Errorf("verify new image: local %s resolves to %s, expected %s — refusing self-update", o.NewImage, gotID, o.ExpectedID)
		}
	}

	full, err := c.Inspect(ctx, o.OldID)
	if err != nil {
		return fmt.Errorf("inspect old: %w", err)
	}
	oldRef, oldImageID := full.Image, full.ImageID
	if defaults, err := c.ImageDefaults(ctx, full.ImageID); err == nil {
		full.StripImageDefaults(defaults)
	} else if recorded, ok := lookupDefaults(o.RecordedDefaults, full.ImageID); ok {
		logger.Info("old image is no longer in the image store; using its recorded defaults", "image_id", full.ImageID)
		full.StripImageDefaults(recorded)
	} else {
		logger.Warn("could not read the old image's defaults; the replacement keeps the current command, environment and healthcheck",
			"image_id", full.ImageID, "err", err)
	}
	if platform, err := c.ForeignPlatform(ctx, full); err != nil {
		logger.Warn("could not determine the platform of the running image; creating for the daemon's default platform", "err", err)
	} else {
		full.Platform = platform
	}
	if o.NewImage != "" {
		full.Image = o.NewImage
		if full.Config != nil {
			full.Config.Image = o.NewImage
		}
	}
	retag := ""
	if o.NewImage != "" && o.NewImage == oldRef && o.ExpectedID != "" {
		full.PinComposeImage(o.ExpectedID)
		retag = oldRef
	}
	if err := c.ResolveNetworkOwner(ctx, full); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	sw := swap{c: c, full: full, oldImageID: oldImageID, retag: retag, logger: logger,
		stopSecs: full.EffectiveStopTimeout(o.StopTimeout)}

	tempName := full.Name + ".self-update-old-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := c.RenameContainer(ctx, o.OldID, tempName); err != nil {
		return fmt.Errorf("rename old: %w (nothing was changed; the old daemon keeps running)", err)
	}
	logger.Info("stopping old daemon", "id", o.OldID, "name", full.Name, "timeout_s", sw.stopSecs)
	if err := c.StopContainer(ctx, o.OldID, sw.stopSecs); err != nil {
		sw.stopInterrupted = docker.StopMayContinue(ctx, err)
		return sw.revert(ctx, "", fmt.Errorf("stop old: %w", err))
	}
	newID, err := c.CreateContainer(ctx, full.Name, full)
	if err != nil {
		return sw.revert(ctx, newID, fmt.Errorf("create replacement: %w", err))
	}
	if err := c.StartContainer(ctx, newID); err != nil {
		return sw.revert(ctx, newID, fmt.Errorf("start replacement: %w", err))
	}
	started, err := c.ContainerState(ctx, newID)
	if err != nil {
		return sw.revert(ctx, newID, fmt.Errorf("inspect replacement: %w", err))
	}
	gate := docker.HealthGate{Timeout: o.HealthTimeout, GraceCap: o.HealthGrace, Poll: o.HealthPoll}
	if gate.Timeout <= 0 {
		var hc *container.HealthConfig
		if repl, err := c.Inspect(ctx, newID); err == nil && repl.Config != nil {
			hc = repl.Config.Healthcheck
		}
		gate.Timeout = helperHealthTimeout(hc)
	}
	logger.Info("waiting for the new daemon to become healthy", "id", newID, "timeout", gate.Timeout)
	if err := gate.Wait(ctx, c, newID, started); err != nil {
		return sw.revert(ctx, newID, fmt.Errorf("new daemon failed its health check: %w", err))
	}
	rctx, cancel := recoveryContext(ctx, 2*time.Minute)
	defer cancel()
	if err := c.RemoveContainer(rctx, o.OldID); err != nil {
		logger.Warn("remove old container failed", "id", o.OldID, "err", err)
	}
	logger.Info("self-update complete", "new_id", newID, "name", full.Name)
	return nil
}

func lookupDefaults(f func(string) (*docker.ImageDefaults, bool), imageID string) (*docker.ImageDefaults, bool) {
	if f == nil {
		return nil, false
	}
	return f(imageID)
}

func RecordedDefaults(st *state.Store, host string) func(imageID string) (*docker.ImageDefaults, bool) {
	return func(imageID string) (*docker.ImageDefaults, bool) {
		if st == nil {
			return nil, false
		}
		raw, ok := st.ImageDefaults(host, imageID)
		if !ok {
			return nil, false
		}
		var d docker.ImageDefaults
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false
		}
		return &d, true
	}
}

type swap struct {
	c               *docker.Client
	full            *docker.FullContainer
	oldImageID      string
	retag           string
	stopSecs        int
	logger          *slog.Logger
	stopInterrupted bool
}

func (s swap) revert(ctx context.Context, newID string, cause error) error {
	rctx, cancel := recoveryContext(ctx, time.Duration(s.stopSecs)*time.Second+stopGrace+2*time.Minute)
	defer cancel()
	s.logger.Error("self-update failed, restoring the old daemon", "err", cause)
	var errs []error
	if newID != "" {
		stop := s.stopSecs
		if ctx.Err() != nil {
			stop = min(stop, interruptedStopTimeout)
		}
		if err := s.c.StopContainer(rctx, newID, stop); err != nil {
			s.logger.Warn("stop the replacement failed", "id", newID, "err", err)
		}
		if err := s.c.RemoveContainer(rctx, newID); err != nil {
			errs = append(errs, fmt.Errorf("remove the replacement: %w", err))
		}
	}
	if s.stopInterrupted {
		if _, err := s.c.WaitExit(rctx, s.full.ID, time.Duration(s.stopSecs)*time.Second+stopGrace); err != nil {
			s.logger.Warn("the interrupted stop of the old daemon did not end in time", "id", s.full.ID, "err", err)
		}
	}
	if err := s.c.RenameContainer(rctx, s.full.ID, s.full.Name); err != nil {
		errs = append(errs, fmt.Errorf("rename the old daemon back: %w", err))
	}
	if st, err := s.c.ContainerState(rctx, s.full.ID); err != nil || !st.Running {
		if err := s.c.StartContainer(rctx, s.full.ID); err != nil {
			errs = append(errs, fmt.Errorf("start the old daemon: %w", err))
		}
	}
	if s.retag != "" {
		if err := s.c.ImageTag(rctx, s.oldImageID, s.retag); err != nil {
			s.logger.Warn("could not point the tag back at the old image", "ref", s.retag, "err", err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w; restoring the old daemon failed: %w", cause, errors.Join(errs...))
	}
	s.logger.Info("old daemon restored", "id", s.full.ID, "name", s.full.Name)
	return fmt.Errorf("%w; the old daemon was restored", cause)
}

var ErrNotInContainer = errors.New("self-update: not running inside a container")

var ErrNotFoundOnHost = errors.New("self-update: container not found on host")

func RunHelperIfPresent(ctx context.Context, c *docker.Client, oldID, newImage, expectedID string, delay time.Duration, logger *slog.Logger) error {
	return RunHelperIfPresentWith(ctx, c, DefaultHelperOptions(oldID, newImage, expectedID, delay), logger)
}

func RunHelperIfPresentWith(ctx context.Context, c *docker.Client, o HelperOptions, logger *slog.Logger) error {
	if _, err := c.Inspect(ctx, o.OldID); err != nil {
		return ErrNotFoundOnHost
	}
	return RunHelperWith(ctx, c, o, logger)
}
