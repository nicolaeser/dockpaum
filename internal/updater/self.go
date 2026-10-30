package updater

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/lifecycle"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/state"
)

type selfUpdatePlan struct {
	c           *docker.Client
	ct          docker.Container
	full        *docker.FullContainer
	oldDefaults *docker.ImageDefaults
	eff         policy.Resolved
	imageRef    string
	newImageID  string
	res         stepResult
	cand        *candidate
	fetched     bool
	tagAtCheck  string
	subject     approvalSubject
	moved       *pulledTag
}

func (u *Updater) applySelfUpdate(ctx context.Context, runID string, p *selfUpdatePlan) *tally {
	t := &tally{}
	res := p.res
	res.self = nil
	err := u.pointSelfTag(ctx, p, &res)
	if err == nil {
		err = u.handOver(ctx, runID, p, &res)
	}
	if err != nil || res.decision != audit.DecisionSelfUpdate {
		u.dropSelfPlan(ctx, p)
	}
	u.report(ctx, runID, p.c, t, p.ct, res, err)
	return t
}

func (u *Updater) pointSelfTag(ctx context.Context, p *selfUpdatePlan, res *stepResult) error {
	if p.cand == nil {
		return nil
	}
	moved, err := u.pointTag(ctx, nil, p.c, p.full, p.imageRef, p.newImageID, res.newTag == "", p.tagAtCheck)
	if err != nil {
		return fmt.Errorf("%w: %w", errCheckFailed, err)
	}
	p.moved = moved
	return nil
}

func (u *Updater) dropSelfPlan(ctx context.Context, p *selfUpdatePlan) {
	switch {
	case p.moved != nil:
		u.revertPull(ctx, nil, p.c, p.moved)
	case p.fetched:
		u.keepFetched(ctx, p.c, p.imageRef, p.newImageID)
	}
}

func (u *Updater) handOver(ctx context.Context, runID string, p *selfUpdatePlan, res *stepResult) error {
	logger := u.deps.Logger.With("host", p.c.Name, "run_id", runID, "container", p.ct.Name)
	configPath := selfupdate.ConfigArg(p.full)
	self := p.full
	if p.oldDefaults != nil {
		self.StripImageDefaults(p.oldDefaults)
	}
	if err := selfupdate.CheckAccess(self, u.hostEndpoint(p.c.Name)); err != nil {
		return fmt.Errorf("self-update refused: %w", err)
	}
	scope := u.deps.Config.Filters.Scope
	if err := selfupdate.ValidateConfig(ctx, p.c, self, p.newImageID, configPath, scope, logger); err != nil {
		if errors.Is(err, selfupdate.ErrConfigRejected) {
			u.state.RecordSelfUpdateFailure(p.c.Name, p.ct.Name, state.SelfUpdateFailure{Image: p.imageRef, ImageID: p.newImageID, At: u.now()})
		}
		return fmt.Errorf("self-update refused: %w", err)
	}
	if !p.eff.Windows.Allows(u.windowTime()) {
		res.decision = audit.DecisionGatedWindow
		res.reason = fmt.Sprintf("policy %q window closed during the run", p.eff.PolicyID)
		return nil
	}
	target := lifecycle.Target{
		RunID: runID, Host: p.c.Name, Container: p.ct.Name, ContainerID: p.ct.ID, Policy: p.eff.PolicyID,
		Image: p.ct.Image, NewImage: p.imageRef, PolicyHooks: p.eff.Hooks,
		LabelPreUpdate: p.eff.Labels.PreUpdate, LabelPostUpdate: p.eff.Labels.PostUpdate, LabelOnFailure: p.eff.Labels.OnFailure,
		Running: p.full.Running,
	}
	if err := u.hooks.Run(ctx, lifecycle.PhasePre, p.c, target); err != nil {
		if lifecycle.IsSkipRequest(err) {
			res.decision = audit.DecisionSkipped
			res.reason = fmt.Sprintf("a pre-update hook asked to skip this update (exit %d)", lifecycle.SkipExitCode)
			return nil
		}
		return fmt.Errorf("pre-update hook: %w", err)
	}
	if u.approval.recheck() {
		if ok, err := u.approve(ctx, runID, p.c, p.ct, p.full, p.subject, approvalPhaseFinal, p.imageRef, res); !ok {
			return err
		}
	}
	_ = u.state.Flush()
	sc := u.deps.Config.Update.SelfUpdate
	delay := sc.Delay.Std()
	if delay <= 0 {
		delay = 5 * time.Second
	}
	name, err := selfupdate.Spawn(ctx, p.c, selfupdate.SpawnOptions{
		Self: self, ConfigPath: configPath, NewImage: p.imageRef, ExpectedID: p.newImageID,
		HelperImage: sc.HelperImage, Delay: delay, Scope: scope,
	}, logger)
	if err != nil {
		return fmt.Errorf("self-update spawn: %w", err)
	}
	res.decision = audit.DecisionSelfUpdate
	res.reason = fmt.Sprintf("helper %s started: it replaces the daemon, waits for the new one to become healthy "+
		"and restores the old one otherwise; the daemon running afterwards records the outcome", name)
	return nil
}

const selfUpdateRetry = 24 * time.Hour

func (u *Updater) selfUpdateHeld(host, container, newImageID string) string {
	f, ok := u.state.LastSelfUpdateFailure(host, container)
	if !ok || f.ImageID != newImageID {
		return ""
	}
	retry := f.At.Add(selfUpdateRetry)
	if !u.now().Before(retry) {
		return ""
	}
	return fmt.Sprintf("the self-update to %s failed at %s; that image is tried again from %s, a newer one at once",
		shortID(newImageID), f.At.UTC().Format(time.RFC3339), retry.UTC().Format(time.RFC3339))
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func (u *Updater) hostEndpoint(name string) string {
	for _, h := range u.deps.Config.Hosts {
		if h.Name == name {
			return h.Endpoint
		}
	}
	return ""
}

func (u *Updater) recordSelfUpdateOutcome(ctx context.Context, runID string, c *docker.Client, t *tally, all []docker.Container, o selfupdate.Outcome) {
	logger := u.deps.Logger.With("host", c.Name, "run_id", runID)
	rec := audit.Record{RunID: runID, Host: c.Name, Container: o.Container, ContainerID: o.OldID, Image: o.Image}
	ct, found := docker.FindContainer(all, o.Container)
	found = found && o.Container != ""
	if found {
		rec.ContainerID = ct.ID
		rec.Policy = u.resolver.Resolve(ct, ct.Labels).PolicyID
	} else {
		ct = docker.Container{Name: o.Container, ID: o.OldID, Image: o.Image}
	}
	item := notify.RunItem{Host: c.Name, Container: o.Container, Image: o.Image, NewDigest: o.ImageID}
	if o.Err == nil {
		u.state.ClearSelfUpdateFailure(c.Name, o.Container)
		rec.Decision = decisionSelfUpdateCompleted
		rec.Reason = fmt.Sprintf("helper %s replaced the daemon; the new one passed its health check", o.Helper)
		item.Result = string(audit.DecisionUpdated)
		t.add(item, true, false)
		u.deps.Metrics.UpdatesTotal.WithLabelValues(c.Name, o.Container, rec.Policy).Inc()
		ev := notify.Event{
			Type: notify.EventUpdateSuccess, Host: c.Name, Container: o.Container, Image: o.Image, Policy: rec.Policy,
			NewDigest: o.ImageID,
		}
		u.notify(ctx, runID, ev)
		ev.Type, ev.Reason = notify.EventSelfUpdateSuccess, rec.Reason
		u.notify(ctx, runID, ev)
		logger.Info("self-update completed", "container", o.Container, "image", o.Image, "helper", o.Helper)
	} else {
		u.state.RecordSelfUpdateFailure(c.Name, o.Container, state.SelfUpdateFailure{Image: o.Image, ImageID: o.ImageID, At: u.now()})
		if found && o.ImageID != "" && ct.Image == o.Image && ct.ImageID != o.ImageID {
			u.revertPull(ctx, nil, c, &pulledTag{ref: o.Image, restore: ct.ImageID, candidate: o.ImageID})
		}
		rec.Decision, rec.Error = decisionSelfUpdateFailed, o.Err.Error()
		item.Result, item.Error = string(audit.DecisionFailed), o.Err.Error()
		t.add(item, false, true)
		u.deps.Metrics.UpdateFailures.WithLabelValues(c.Name, o.Container, rec.Policy).Inc()
		ev := notify.Event{
			Type: notify.EventUpdateFailure, Host: c.Name, Container: o.Container, Image: o.Image, Policy: rec.Policy, Err: o.Err,
			NewDigest: o.ImageID,
		}
		u.notify(ctx, runID, ev)
		ev.Type = notify.EventSelfUpdateFailure
		u.notify(ctx, runID, ev)
		logger.Error("self-update failed", "container", o.Container, "image", o.Image, "err", o.Err)
	}
	u.deps.Audit.Append(rec)
	u.recordSelfUpdateStatus(ct, found, rec, o.Err)
}
