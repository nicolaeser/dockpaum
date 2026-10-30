package updater

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/pulltoken"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
)

type hostRun struct {
	runID string

	mu       sync.Mutex
	replaced map[string]string
	busy     map[string]bool
	pending  map[string]bool
	deferred map[string]dependentResult

	deferReverts bool
	refused      []pulledTag
	deployed     map[string]map[string]bool
	tags         map[string]*tagRecord
	pointed      map[string]map[string]bool
	tokens       map[string]*pulltoken.File
}

func newHostRun(runID string, targets []docker.Container) *hostRun {
	h := &hostRun{
		runID:    runID,
		replaced: map[string]string{},
		busy:     map[string]bool{},
		pending:  map[string]bool{},
		deferred: map[string]dependentResult{},
		deployed: map[string]map[string]bool{},
		tags:     map[string]*tagRecord{},
		pointed:  map[string]map[string]bool{},
	}
	for _, ct := range targets {
		h.pending[ct.ID] = true
	}
	return h
}

func (h *hostRun) begin(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pending, id)
}

func (h *hostRun) deferFailure(d dependentResult) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pending[d.ct.ID] {
		return false
	}
	h.deferred[d.ct.ID] = d
	return true
}

func (h *hostRun) settle(id string) (dependentResult, bool) {
	h.mu.Lock()
	d, ok := h.deferred[id]
	delete(h.deferred, id)
	_, replaced := h.replaced[id]
	h.mu.Unlock()
	return d, ok && !replaced
}

func (h *hostRun) replace(oldID, newID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replaced[oldID] = newID
}

func (h *hostRun) current(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for range len(h.replaced) {
		next, ok := h.replaced[id]
		if !ok {
			break
		}
		id = next
	}
	return id
}

func (h *hostRun) claim(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busy[id] {
		return false
	}
	h.busy[id] = true
	return true
}

func (h *hostRun) release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.busy, id)
}

type dependentResult struct {
	ct     docker.Container
	policy string
	reason string
	err    error
}

func networkUnits(targets []docker.Container) [][]docker.Container {
	n := len(targets)
	index := make(map[string]int, n)
	for i, ct := range targets {
		index[ct.ID] = i
	}
	parent := make([]int, n)
	owner := make([]int, n)
	for i := range parent {
		parent[i] = i
		owner[i] = -1
	}
	root := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i, ct := range targets {
		ref := ct.NetworkOwnerRef()
		if ref == "" {
			continue
		}
		o, ok := docker.FindContainer(targets, ref)
		if !ok || o.ID == ct.ID {
			continue
		}
		j := index[o.ID]
		owner[i] = j
		parent[root(i)] = root(j)
	}
	depth := make([]int, n)
	for i := range targets {
		for j := owner[i]; j >= 0 && depth[i] < n; j = owner[j] {
			depth[i]++
		}
	}
	members := map[int][]int{}
	var order []int
	for i := range targets {
		r := root(i)
		if _, ok := members[r]; !ok {
			order = append(order, r)
		}
		members[r] = append(members[r], i)
	}
	out := make([][]docker.Container, 0, len(order))
	for _, r := range order {
		idx := members[r]
		sort.SliceStable(idx, func(a, b int) bool { return depth[idx[a]] < depth[idx[b]] })
		unit := make([]docker.Container, 0, len(idx))
		for _, i := range idx {
			unit = append(unit, targets[i])
		}
		out = append(out, unit)
	}
	return out
}

func (u *Updater) networkDependents(ctx context.Context, c *docker.Client, fc *docker.FullContainer) ([]docker.Container, error) {
	list, err := c.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	var out []docker.Container
	for _, ct := range docker.NetworkDependents(list, fc.ID) {
		if !tempNameRE.MatchString(ct.Name) {
			out = append(out, ct)
		}
	}
	return out, nil
}

func (u *Updater) cannotReattach(ct docker.Container) string {
	if u.selfID != "" && selfupdate.IsSelf(u.selfID, ct.ID) {
		return "it is dockpaum's own container"
	}
	if ok, reason := keep(ct, u.deps.Config.Filters); !ok {
		return "this instance does not manage it (" + reason + ")"
	}
	eff := u.resolver.Resolve(ct, ct.Labels)
	switch {
	case eff.Skip:
		return "it is excluded (" + eff.SkipReason + ")"
	case eff.Mode == config.ModeNone:
		return fmt.Sprintf("its policy %q has mode none", eff.PolicyID)
	case eff.Labels.MonitorOnly:
		return "it is labelled monitor-only"
	}
	if o := orchestratedBy(ct.Labels); o != "" {
		return "it is " + o
	}
	return ""
}

func (u *Updater) stopTimeoutSecs() int {
	return int(u.deps.Config.Update.StopTimeout.Std().Seconds())
}

func (u *Updater) reattach(ctx context.Context, hr *hostRun, c *docker.Client, owner *docker.FullContainer, newOwnerID string, deps []docker.Container) []dependentResult {
	var out []dependentResult
	add := func(res dependentResult) {
		if res.err != nil && hr.deferFailure(res) {
			return
		}
		out = append(out, res)
	}
	for _, dep := range deps {
		byName := isNameRef(dep.NetworkOwnerRef(), owner.Name)
		if dep.State != "running" {
			if !byName {
				u.deps.Logger.Warn("stopped container joins the network of a replaced container; recreate it before starting it again",
					"container", dep.Name, "network_owner", owner.Name)
			}
			continue
		}
		res := dependentResult{ct: dep, policy: u.resolver.Resolve(dep, dep.Labels).PolicyID}
		if reason := u.cannotReattach(dep); reason != "" {
			res.err = fmt.Errorf("it joins the network of %s, which was replaced, but %s; recreate it to restore its network", owner.Name, reason)
			add(res)
			continue
		}
		if byName {
			res.err = u.restartDependent(ctx, c, dep)
			res.reason = fmt.Sprintf("restarted to join the network of %s, which was replaced", owner.Name)
			add(res)
			continue
		}
		nested, err := u.recreateDependent(ctx, hr, c, dep, owner.Name, newOwnerID)
		res.err = err
		res.reason = fmt.Sprintf("recreated to join the network of %s, which was replaced", owner.Name)
		add(res)
		out = append(out, nested...)
	}
	return out
}

func (u *Updater) recreateDependent(ctx context.Context, hr *hostRun, c *docker.Client, dep docker.Container, ownerName, newOwnerID string) ([]dependentResult, error) {
	full, err := c.Inspect(ctx, dep.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	if full.HostConfig != nil && full.HostConfig.AutoRemove {
		return nil, fmt.Errorf("it joins the network of %s, which was replaced, but it runs with --rm; recreate it to restore its network", ownerName)
	}
	if id, err := c.ImageID(ctx, full.Image); err != nil || id != full.ImageID {
		return nil, fmt.Errorf("it joins the network of %s, which was replaced, but its image %s no longer resolves to the image it runs (%s); recreate it to restore its network",
			ownerName, full.Image, full.ImageID)
	}
	full.JoinNetworkOf(newOwnerID)
	return u.recreate(ctx, hr, c, full, recreateOpts{expectImageID: full.ImageID})
}

func (u *Updater) restartDependent(ctx context.Context, c *docker.Client, dep docker.Container) error {
	full, err := c.Inspect(ctx, dep.ID)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if full.HostConfig != nil && full.HostConfig.AutoRemove {
		return fmt.Errorf("it shares the network of a container that was restarted or replaced, but it runs with --rm; restart it by hand to restore its network")
	}
	if err := c.RestartContainer(ctx, dep.ID, full.EffectiveStopTimeout(u.stopTimeoutSecs())); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	return nil
}

func (u *Updater) rejoin(ctx context.Context, c *docker.Client, owner *docker.FullContainer, deps []docker.Container) []dependentResult {
	if !owner.Running {
		return nil
	}
	var out []dependentResult
	for _, dep := range deps {
		if dep.State != "running" {
			continue
		}
		res := dependentResult{ct: dep, policy: u.resolver.Resolve(dep, dep.Labels).PolicyID}
		if reason := u.cannotReattach(dep); reason != "" {
			res.err = fmt.Errorf("it joins the network of %s, which was restarted by a rollback, but %s; restart it to restore its network", owner.Name, reason)
			out = append(out, res)
			continue
		}
		rctx, cancel := recoveryContext(ctx)
		res.err = u.restartDependent(rctx, c, dep)
		cancel()
		res.reason = fmt.Sprintf("restarted to rejoin the network of %s after its update was rolled back", owner.Name)
		out = append(out, res)
	}
	return out
}

func isNameRef(ref, name string) bool {
	return ref == name || ref == "/"+name
}
