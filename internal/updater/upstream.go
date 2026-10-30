package updater

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/state"
)

type registryAPI interface {
	ListTags(ctx context.Context, ref registry.Reference) ([]string, error)
	ManifestDigest(ctx context.Context, ref registry.Reference) (registry.Descriptor, error)
	PlatformManifest(ctx context.Context, ref registry.Reference, desc registry.Descriptor, platform registry.Platform) (registry.Descriptor, error)
	ImageConfig(ctx context.Context, ref registry.Reference, desc registry.Descriptor) (registry.ImageConfig, error)
}

const (
	digestCheckTimeout = 10 * time.Second
	resolveTimeout     = 30 * time.Second
)

const (
	CheckRegistry = "registry"
	CheckPull     = "pull"
	CheckLocal    = "local"
)

var errNoRepoDigest = errors.New("the running image records no registry digest: it was built or loaded locally")

var errNoCreated = errors.New("its config records none")

type directAccess struct {
	mu        sync.Mutex
	skip      map[string]skippedRegistry
	announced map[string]bool
}

type skippedRegistry struct {
	runID string
	err   error
}

func (a *directAccess) skipped(host, runID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.skip[strings.ToLower(host)]; ok && e.runID == runID {
		return fmt.Errorf("not asked again in this run, %s failed earlier: %w", host, e.err)
	}
	return nil
}

func (a *directAccess) markSkipped(host, runID string, err error) (first bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.skip == nil {
		a.skip, a.announced = map[string]skippedRegistry{}, map[string]bool{}
	}
	host = strings.ToLower(host)
	a.skip[host] = skippedRegistry{runID: runID, err: err}
	first = !a.announced[host]
	a.announced[host] = true
	return first
}

type candidate struct {
	pullRef  string
	imageID  string
	running  bool
	byDigest bool
	created  time.Time
	labels   map[string]string
}

func (u *Updater) checkUpstream(ctx context.Context, runID string, c *docker.Client, full *docker.FullContainer,
	imageRef string, runningDigests []string) (*candidate, error) {
	ref := registry.Parse(imageRef)
	if len(runningDigests) == 0 {
		return nil, errNoRepoDigest
	}
	if err := u.direct.skipped(ref.Registry, runID); err != nil {
		return nil, err
	}
	if err := u.backoff.blocked(ref.Registry, runID, u.now()); err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, digestCheckTimeout)
	d, err := u.reg.ManifestDigest(hctx, ref)
	cancel()
	if err != nil {
		u.noteRegistryError(ctx, ref.Registry, runID, err)
		return nil, err
	}
	cand := &candidate{pullRef: registry.DigestReference(imageRef, d.Digest)}
	if registry.HasRepoDigest(runningDigests, ref, d.Digest) {
		cand.imageID, cand.running = full.ImageID, true
		return cand, nil
	}
	ri, err := u.resolveCandidate(ctx, runID, c, full, ref, d)
	if err != nil {
		return nil, err
	}
	if manifestIDs(full.ImageID, runningDigests) {
		cand.imageID, cand.byDigest = d.Digest, true
		cand.running = full.ImageManifestDigest() != "" && full.ImageManifestDigest() == ri.Manifest
	} else {
		cand.imageID = ri.ImageID
		cand.running = ri.ImageID == full.ImageID
	}
	if cand.running {
		cand.imageID = full.ImageID
		return cand, nil
	}
	cand.created, cand.labels = ri.Created, ri.Labels
	return cand, nil
}

func manifestIDs(imageID string, repoDigests []string) bool {
	for _, rd := range repoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok && d == imageID {
			return true
		}
	}
	return false
}

func (u *Updater) resolveCandidate(ctx context.Context, runID string, c *docker.Client, full *docker.FullContainer,
	ref registry.Reference, d registry.Descriptor) (state.ResolvedImage, error) {
	ps, err := c.ImagePlatform(ctx, full)
	if err != nil {
		return state.ResolvedImage{}, fmt.Errorf("platform of the running image: %w", err)
	}
	platform, ok := registry.ParsePlatform(ps)
	if !ok {
		return state.ResolvedImage{}, fmt.Errorf("platform of the running image: unreadable %q", ps)
	}
	repo := ref.Registry + "/" + ref.Repo
	key := state.ResolvedKey(repo, d.Digest, platform.String())
	if r, ok := u.state.Resolved(c.Name, key); ok {
		return r, nil
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	m, err := u.reg.PlatformManifest(rctx, ref, d, platform)
	if err != nil {
		u.noteRegistryError(ctx, ref.Registry, runID, err)
		return state.ResolvedImage{}, fmt.Errorf("manifest for %s: %w", platform, err)
	}
	cfg, err := u.reg.ImageConfig(rctx, ref, m)
	if err != nil {
		u.noteRegistryError(ctx, ref.Registry, runID, err)
		return state.ResolvedImage{}, fmt.Errorf("image config: %w", err)
	}
	r := state.ResolvedImage{
		ImageID: cfg.Digest, Manifest: m.Digest, Created: cfg.Created,
		Labels: registry.ReleaseLabels(cfg.Labels, linkTemplateLabel),
	}
	u.state.PutResolved(c.Name, key, repo, r)
	return r, nil
}

func (u *Updater) noteRegistryError(ctx context.Context, host, runID string, err error) {
	switch {
	case ctx.Err() != nil:
	case errors.Is(err, registry.ErrRateLimited):
		u.noteRateLimit(host, runID, err)
	case !registryUnreachable(err):
	case u.direct.markSkipped(host, runID, err):
		u.deps.Logger.Warn("registry cannot be asked for digests directly: the daemon pulls to compare instead, before the gates "+
			"(the tag is pointed back when an update is refused), and dry-run and monitor-only containers are not checked; "+
			"allow dockpaum direct HTTPS access to the registry to check without pulling",
			"registry", host, "err", err)
	}
}

func registryUnreachable(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, registry.ErrUnavailable) || errors.Is(err, registry.ErrNoDigest)
}

func (u *Updater) noteFallback(host, container, image string, err error, pulled bool) {
	key := statusKey{host, container}
	kind := ""
	if err != nil {
		kind = fallbackKind(err)
		if !pulled {
			kind += "/unchecked"
		}
	}
	u.fallbackMu.Lock()
	repeated := u.fallback[key] == kind
	if kind == "" {
		delete(u.fallback, key)
	} else {
		u.fallback[key] = kind
	}
	u.fallbackMu.Unlock()
	if kind == "" {
		return
	}
	switch {
	case pulled && !repeated:
		u.deps.Logger.Info("registry not asked directly; the daemon pulls to compare, before the gates",
			"host", host, "container", container, "image", image, "reason", err)
	case !pulled && !repeated:
		u.deps.Logger.Warn("update check skipped: the registry cannot be asked directly, and the container is not pulled to compare "+
			"(it is only observed: dry-run, monitor-only; or the registry limits requests)",
			"host", host, "container", container, "image", image, "reason", err)
	default:
		u.deps.Logger.Debug("registry not asked directly", "host", host, "container", container, "image", image,
			"pulled", pulled, "reason", err)
	}
}

func fallbackKind(err error) string {
	var answer *registry.HTTPError
	switch {
	case errors.Is(err, errNoRepoDigest):
		return "no-repo-digest"
	case errors.Is(err, registry.ErrRateLimited):
		return "rate-limited"
	case errors.As(err, &answer):
		return fmt.Sprintf("http-%d", answer.StatusCode)
	case errors.Is(err, registry.ErrNoDigest):
		return "no-digest"
	case registryUnreachable(err):
		return "unreachable"
	}
	return "registry-error"
}

func (u *Updater) fetchCandidate(ctx context.Context, c *docker.Client, full *docker.FullContainer, cand *candidate,
	creds *registry.Credentials) (string, error) {
	if !cand.byDigest {
		if id, err := c.ImageID(ctx, cand.pullRef); err == nil && id == cand.imageID {
			return id, nil
		}
	}
	id, err := pullWith(ctx, c, cand.pullRef, creds, full.Platform)
	if err != nil {
		return "", fmt.Errorf("%w: pull %s: %w", errCheckFailed, cand.pullRef, err)
	}
	if id != cand.imageID {
		return id, fmt.Errorf("%w: pulling %s gave image %s, not %s, which the registry check resolved for the platform of the running image "+
			"and the gates judged; it is not deployed", errCheckFailed, cand.pullRef, shortID(id), shortID(cand.imageID))
	}
	return id, nil
}

func (u *Updater) pointTag(ctx context.Context, hr *hostRun, c *docker.Client, full *docker.FullContainer, imageRef, id string,
	followed bool, atCheck string) (*pulledTag, error) {
	cur, err := c.ImageID(ctx, imageRef)
	if err != nil {
		cur = ""
	}
	if cur == id {
		return nil, nil
	}
	if followed && cur != atCheck && (hr == nil || !hr.pointedTo(imageRef, cur)) {
		return nil, fmt.Errorf("image %s now resolves to %s, not to %s as when it was checked (the tag moved during the update); "+
			"what was pulled or built meanwhile stands", imageRef, orNone(shortID(cur)), orNone(shortID(atCheck)))
	}
	before := atCheck
	if followed && hr != nil {
		if b := hr.tagBefore(ctx, c, imageRef); b != "" {
			before = b
		}
	}
	if err := c.ImageTag(ctx, id, imageRef); err != nil {
		return nil, fmt.Errorf("tag %s: %w", imageRef, err)
	}
	if !followed {
		return nil, nil
	}
	if hr != nil {
		hr.notePointed(imageRef, id)
	}
	if before == "" {
		before = full.ImageID
	}
	return &pulledTag{ref: imageRef, restore: before, candidate: id}, nil
}

func orNone(id string) string {
	if id == "" {
		return "nothing"
	}
	return id
}

const linkTemplateLabel = "dockpaum.link.template"

func (u *Updater) releaseURL(full *docker.FullContainer, oldDefaults *docker.ImageDefaults, imageLabels map[string]string, tag string) string {
	tmpl := imageLabels[linkTemplateLabel]
	if v, ok := full.Config.Labels[linkTemplateLabel]; ok && strings.TrimSpace(v) != "" {
		inherited := oldDefaults != nil && oldDefaults.Labels[linkTemplateLabel] == v
		if !inherited {
			tmpl = v
		}
	}
	return registry.ReleaseNotesURL(imageLabels, tag, tmpl)
}
