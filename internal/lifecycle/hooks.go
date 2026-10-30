package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

type Phase string

const (
	PhasePreRun  Phase = "pre_run"
	PhasePostRun Phase = "post_run"
	PhasePre     Phase = "pre_update"
	PhasePost    Phase = "post_update"
	PhaseFailure Phase = "on_failure"
	PhaseJob     Phase = "job"
)

type Runner struct {
	cfg    config.HookConfig
	logger *slog.Logger
}

func NewRunner(cfg config.HookConfig, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, logger: logger}
}

type Target struct {
	RunID           string
	Host            string
	Container       string
	ContainerID     string
	Policy          string
	Image           string
	NewImage        string
	PolicyHooks     config.PolicyHooks
	LabelPreUpdate  string
	LabelPostUpdate string
	LabelOnFailure  string
	Running         bool
}

const SkipExitCode = 75

func IsSkipRequest(err error) bool {
	var dockerExit *docker.ExitError
	if errors.As(err, &dockerExit) {
		return dockerExit.Code == SkipExitCode
	}
	var procExit *exec.ExitError
	if errors.As(err, &procExit) {
		return procExit.ExitCode() == SkipExitCode
	}
	return false
}

func (r *Runner) labelHook(phase Phase, t Target) string {
	if !r.cfg.LabelHooks || !t.Running {
		return ""
	}
	switch phase {
	case PhasePre:
		if t.LabelPreUpdate != "" && t.ContainerID != "" {
			return "exec:" + t.ContainerID + ":" + t.LabelPreUpdate
		}
	case PhasePost:
		if t.LabelPostUpdate != "" && t.Container != "" {
			return "exec:" + t.Container + ":" + t.LabelPostUpdate
		}
	case PhaseFailure:
		if t.LabelOnFailure != "" && t.ContainerID != "" {
			return "exec:" + t.ContainerID + ":" + t.LabelOnFailure
		}
	}
	return ""
}

func (t Target) env(phase Phase) []string {
	return []string{
		"DOCKPAUM_PHASE=" + string(phase),
		"DOCKPAUM_TARGET=" + t.Container,
		"DOCKPAUM_HOST=" + t.Host,
		"DOCKPAUM_RUN_ID=" + t.RunID,
		"DOCKPAUM_POLICY=" + t.Policy,
		"DOCKPAUM_IMAGE=" + t.Image,
		"DOCKPAUM_NEW_IMAGE=" + t.NewImage,
	}
}

type RunInfo struct {
	RunID   string
	Policy  string
	Scanned int
	Updated int
	Failed  int
}

func (i RunInfo) env(phase Phase) []string {
	return []string{
		"DOCKPAUM_PHASE=" + string(phase),
		"DOCKPAUM_RUN_ID=" + i.RunID,
		"DOCKPAUM_POLICY=" + i.Policy,
		"DOCKPAUM_SCANNED=" + strconv.Itoa(i.Scanned),
		"DOCKPAUM_UPDATED=" + strconv.Itoa(i.Updated),
		"DOCKPAUM_FAILED=" + strconv.Itoa(i.Failed),
	}
}

func (r *Runner) Run(ctx context.Context, phase Phase, dc *docker.Client, t Target) error {
	var hooks []string
	switch phase {
	case PhasePre:
		hooks = append(hooks, r.cfg.PreUpdate...)
		hooks = append(hooks, t.PolicyHooks.PreUpdate...)
	case PhasePost:
		hooks = append(hooks, r.cfg.PostUpdate...)
		hooks = append(hooks, t.PolicyHooks.PostUpdate...)
	case PhaseFailure:
		hooks = append(hooks, r.cfg.OnFailure...)
		hooks = append(hooks, t.PolicyHooks.OnFailure...)
	}
	if h := r.labelHook(phase, t); h != "" {
		hooks = append(hooks, h)
	}
	return r.runAll(ctx, phase, hooks, dc, t.Container, t.env(phase), phase != PhaseFailure)
}

func (r *Runner) RunStage(ctx context.Context, phase Phase, info RunInfo) error {
	var hooks []string
	switch phase {
	case PhasePreRun:
		hooks = r.cfg.PreRun
	case PhasePostRun:
		hooks = r.cfg.PostRun
	}
	return r.runAll(ctx, phase, hooks, nil, "", info.env(phase), true)
}

func (r *Runner) runAll(ctx context.Context, phase Phase, hooks []string, dc *docker.Client, target string, env []string, stopOnError bool) error {
	if len(hooks) == 0 {
		return nil
	}
	if r.cfg.Disabled {
		r.logger.Debug("hooks disabled by config", "phase", string(phase))
		return nil
	}
	timeout := r.cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	for i, h := range hooks {
		hctx, cancel := context.WithTimeout(ctx, timeout)
		err := r.runOne(hctx, h, dc, target, env)
		cancel()
		if err != nil {
			r.logger.Warn("hook failed", "phase", string(phase), "index", i, "target", target, "err", err)
			if stopOnError {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) RunJob(ctx context.Context, job config.JobConfig, dc *docker.Client) error {
	timeout := job.Timeout.Std()
	if timeout <= 0 {
		timeout = r.cfg.Timeout.Std()
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	env := []string{"DOCKPAUM_PHASE=" + string(PhaseJob), "DOCKPAUM_JOB=" + job.Name}
	for i, cmd := range job.Commands {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := r.runOne(cctx, cmd, dc, "", env)
		cancel()
		if err != nil {
			r.logger.Warn("job command failed", "job", job.Name, "index", i, "err", err)
			return fmt.Errorf("job %s: command %d: %w", job.Name, i, err)
		}
	}
	return nil
}

func (r *Runner) runOne(ctx context.Context, hook string, dc *docker.Client, target string, env []string) error {
	hook = strings.TrimSpace(hook)
	if strings.HasPrefix(hook, "exec:") {
		rest := strings.TrimPrefix(hook, "exec:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid exec hook: %s", hook)
		}
		container := strings.TrimSpace(parts[0])
		if container == "" || container == "@self" {
			container = target
		}
		if container == "" {
			return fmt.Errorf("exec hook names no container: %s", hook)
		}
		if dc == nil {
			return fmt.Errorf("exec hook has no connected docker host: %s", hook)
		}
		cmd := strings.TrimSpace(parts[1])
		out, err := dc.ExecInContainer(ctx, container, []string{"sh", "-c", cmd}, env)
		r.logger.Debug("hook exec", "container", container, "cmd", cmd, "out", out)
		return err
	}
	hook = strings.TrimPrefix(hook, "host:")
	hook = strings.TrimSpace(hook)
	cmd := exec.CommandContext(ctx, "sh", "-c", hook) // #nosec G204 -- hooks are admin-defined commands by design
	cmd.Env = append(os.Environ(), env...)
	setupProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	r.logger.Debug("hook host", "cmd", hook, "out", string(out))
	return err
}
