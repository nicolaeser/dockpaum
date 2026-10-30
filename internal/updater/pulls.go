package updater

import (
	"context"
	"fmt"
	"sync"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

type pulledTag struct {
	ref       string
	restore   string
	candidate string
}

type tagRecord struct {
	once sync.Once
	id   string
}

func (h *hostRun) tagBefore(ctx context.Context, c *docker.Client, ref string) string {
	key := tagKey(ref)
	h.mu.Lock()
	rec := h.tags[key]
	if rec == nil {
		rec = &tagRecord{}
		h.tags[key] = rec
	}
	h.mu.Unlock()
	rec.once.Do(func() {
		if id, err := c.ImageID(ctx, ref); err == nil {
			rec.id = id
		}
	})
	return rec.id
}

func (u *Updater) pullToCompare(ctx context.Context, hr *hostRun, c *docker.Client, full *docker.FullContainer, imageRef string,
	followed bool, creds *registry.Credentials) (string, *pulledTag, error) {
	before := ""
	if followed {
		before = hr.tagBefore(ctx, c, imageRef)
	}
	id, err := pullWith(ctx, c, imageRef, creds, full.Platform)
	if err != nil {
		return "", nil, fmt.Errorf("%w: pull %s: %w", errCheckFailed, imageRef, err)
	}
	if !followed {
		return id, nil, nil
	}
	if before == "" {
		before = full.ImageID
	}
	if id == before {
		return id, nil, nil
	}
	return id, &pulledTag{ref: imageRef, restore: before, candidate: id}, nil
}

func tagKey(ref string) string {
	if r := registry.Parse(ref); r.Repo != "" {
		return r.Registry + "/" + r.Repo + ":" + r.Tag
	}
	return ref
}

func (h *hostRun) notePointed(ref, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := tagKey(ref)
	if h.pointed[key] == nil {
		h.pointed[key] = map[string]bool{}
	}
	h.pointed[key][id] = true
}

func (h *hostRun) pointedTo(ref, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pointed[tagKey(ref)][id]
}

func (h *hostRun) markDeployed(ref, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deployed[ref] == nil {
		h.deployed[ref] = map[string]bool{}
	}
	h.deployed[ref][id] = true
}

func (h *hostRun) isDeployed(ref, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deployed[ref][id]
}

func (h *hostRun) deferRefusal(p *pulledTag) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.deferReverts {
		return false
	}
	h.refused = append(h.refused, *p)
	return true
}

func (h *hostRun) takeRefusals() []pulledTag {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	var out []pulledTag
	for _, p := range h.refused {
		if !seen[p.ref] {
			seen[p.ref] = true
			out = append(out, p)
		}
	}
	h.refused = nil
	return out
}

func (u *Updater) refusePull(ctx context.Context, hr *hostRun, c *docker.Client, p *pulledTag) {
	if hr != nil && hr.deferRefusal(p) {
		return
	}
	u.revertPull(ctx, hr, c, p)
}

func (u *Updater) revertPull(ctx context.Context, hr *hostRun, c *docker.Client, p *pulledTag) {
	if p == nil || (hr != nil && hr.isDeployed(p.ref, p.candidate)) {
		return
	}
	rctx, cancel := recoveryContext(ctx)
	defer cancel()
	if cur, err := c.ImageID(rctx, p.ref); err == nil && cur != p.candidate {
		u.deps.Logger.Debug("the tag moved again since the pull; it is left as it is",
			"host", c.Name, "image", p.ref, "now", cur, "refused", p.candidate)
	} else if err := c.ImageTag(rctx, p.restore, p.ref); err != nil {
		u.deps.Logger.Warn("could not point the tag back at the image it named before the pull; `docker compose up` would deploy the refused image",
			"host", c.Name, "image", p.ref, "before", p.restore, "refused", p.candidate, "err", err)
		return
	} else {
		u.deps.Logger.Debug("tag points at the image it named before the pull again", "host", c.Name, "image", p.ref, "before", p.restore, "refused", p.candidate)
	}
	u.keepRefused(rctx, c, p.ref, p.candidate)
}

func (u *Updater) settlePull(ctx context.Context, hr *hostRun, c *docker.Client, p *pulledTag, res *stepResult) {
	switch {
	case res.updated:
	case res.self != nil:
		res.self.moved = p
	default:
		u.refusePull(ctx, hr, c, p)
	}
}

func (u *Updater) keepRefused(ctx context.Context, c *docker.Client, ref, id string) {
	if prev := u.candidates.swap(c.Name, ref, id); prev != "" && prev != id {
		u.removeSuperseded(ctx, c, ref, prev)
	}
}

func (u *Updater) forgetCandidate(ctx context.Context, c *docker.Client, ref, id string) {
	if prev := u.candidates.take(c.Name, ref); prev != "" && prev != id {
		rctx, cancel := recoveryContext(ctx)
		defer cancel()
		u.removeSuperseded(rctx, c, ref, prev)
	}
}

func (u *Updater) removeSuperseded(ctx context.Context, c *docker.Client, ref, id string) {
	if err := c.RemoveImageIfDangling(ctx, id); err != nil {
		u.deps.Logger.Debug("superseded refused image kept", "host", c.Name, "image", ref, "id", id, "err", err)
		return
	}
	u.deps.Logger.Debug("superseded refused image removed", "host", c.Name, "image", ref, "id", id)
}

type candidateCache struct {
	mu sync.Mutex
	m  map[string]string
}

func candidateKey(host, ref string) string { return host + "\x00" + ref }

func (cc *candidateCache) swap(host, ref, id string) string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.m == nil {
		cc.m = map[string]string{}
	}
	k := candidateKey(host, ref)
	prev := cc.m[k]
	cc.m[k] = id
	return prev
}

func (cc *candidateCache) take(host, ref string) string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	k := candidateKey(host, ref)
	prev := cc.m[k]
	delete(cc.m, k)
	return prev
}
