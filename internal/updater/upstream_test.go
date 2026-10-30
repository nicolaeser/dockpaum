package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

type fakeRegistry struct {
	mu      sync.Mutex
	tags    map[string][]string
	digests map[string]string
	configs map[string]registry.ImageConfig
	indexes map[string]string
	fail    map[string]error
	calls   []string
}

func repoKey(ref registry.Reference) string { return ref.Registry + "/" + ref.Repo }

func (f *fakeRegistry) ListTags(_ context.Context, ref registry.Reference) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "tags "+repoKey(ref))
	if err := f.fail[repoKey(ref)]; err != nil {
		return nil, err
	}
	tags, ok := f.tags[repoKey(ref)]
	if !ok {
		return nil, fmt.Errorf("fake registry: %s: %w", repoKey(ref), registry.ErrNotFound)
	}
	return slices.Clone(tags), nil
}

func (f *fakeRegistry) ManifestDigest(_ context.Context, ref registry.Reference) (registry.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "head "+repoKey(ref)+":"+ref.Tag)
	if err := f.fail[repoKey(ref)]; err != nil {
		return registry.Descriptor{}, err
	}
	d, ok := f.digests[repoKey(ref)+":"+ref.Tag]
	if !ok {
		return registry.Descriptor{}, &registry.HTTPError{Op: "manifest", StatusCode: 404}
	}
	return registry.Descriptor{MediaType: registry.MediaTypeOCIManifest, Digest: d}, nil
}

func (f *fakeRegistry) PlatformManifest(_ context.Context, ref registry.Reference, desc registry.Descriptor, _ registry.Platform) (registry.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "platform "+repoKey(ref)+"@"+desc.Digest)
	if err := f.fail[repoKey(ref)]; err != nil {
		return registry.Descriptor{}, err
	}
	if m, ok := f.indexes[desc.Digest]; ok {
		return registry.Descriptor{MediaType: registry.MediaTypeOCIManifest, Digest: m}, nil
	}
	return desc, nil
}

func (f *fakeRegistry) setIndex(index, manifest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexes == nil {
		f.indexes = map[string]string{}
	}
	f.indexes[index] = manifest
}

func (f *fakeRegistry) ImageConfig(_ context.Context, ref registry.Reference, desc registry.Descriptor) (registry.ImageConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "config "+repoKey(ref)+"@"+desc.Digest)
	if err := f.fail[repoKey(ref)]; err != nil {
		return registry.ImageConfig{}, err
	}
	cfg, ok := f.configs[desc.Digest]
	if !ok {
		return registry.ImageConfig{}, &registry.HTTPError{Op: "manifest", StatusCode: 404}
	}
	return cfg, nil
}

func (f *fakeRegistry) setConfig(digest string, cfg registry.ImageConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configs == nil {
		f.configs = map[string]registry.ImageConfig{}
	}
	f.configs[digest] = cfg
}

func (f *fakeRegistry) setTags(repo string, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tags == nil {
		f.tags = map[string][]string{}
	}
	f.tags[repo] = tags
}

func (f *fakeRegistry) setDigest(ref, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.digests == nil {
		f.digests = map[string]string{}
	}
	r := registry.Parse(ref)
	f.digests[repoKey(r)+":"+r.Tag] = digest
}

func (f *fakeRegistry) setFail(repo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]error{}
	}
	f.fail[repo] = err
}

func (f *fakeRegistry) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (h *fakeHost) pulls() int { return len(h.d.Calls("POST", "/images/create")) }

func (r *recordedEvents) of(typ, container string) []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.Event
	for _, ev := range r.events {
		if ev.Type == typ && ev.Container == container {
			out = append(out, ev)
		}
	}
	return out
}

func (r *recordedEvents) items(container string) []notify.RunItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.RunItem
	for _, ev := range r.events {
		if ev.Type != notify.EventRunCompleted || ev.RunStats == nil {
			continue
		}
		for _, it := range ev.RunStats.Items {
			if it.Container == container {
				out = append(out, it)
			}
		}
	}
	return out
}

func minorPolicy(tags config.TagFilter) func(*config.Config) {
	return func(c *config.Config) {
		c.Update.Policies = append(c.Update.Policies, config.Policy{ID: "minor", Mode: config.ModeMinor, Tags: tags})
	}
}

func TestUnresolvedLabelsAreGatedAndAnnouncedOnce(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	sink := h.deliverTo(nil, notify.EventNoUpdate)
	rec := h.recordEvents()
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("typo", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.policy": "databse"}}, nil, nil)
	h.d.Run("badmode", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.mode": "minr"}}, nil, nil)
	for range 2 {
		if err := h.runAndDeliver(); err != nil {
			t.Fatalf("a gated container must not fail the run: %v", err)
		}
	}
	for name, want := range map[string]string{"typo": `dockpaum.policy="databse": unknown policy`, "badmode": `dockpaum.mode="minr": unknown mode`} {
		recs := h.records(name)
		if len(recs) != 2 {
			t.Fatalf("%s: %d records, want one per run", name, len(recs))
		}
		evs := rec.of(notify.EventNoUpdate, name)
		if len(evs) != len(recs) {
			t.Fatalf("%s: %d update.skipped events for %d records", name, len(evs), len(recs))
		}
		for i, r := range recs {
			if r.Decision != audit.DecisionGatedPolicy || !strings.Contains(r.Reason, want) {
				t.Fatalf("%s: %s %q, want gated-policy with %q", name, r.Decision, r.Reason, want)
			}
			if evs[i].Reason != "gated-policy: "+r.Reason || evs[i].Err != nil || evs[i].RunID != r.RunID || evs[i].Image != appRef {
				t.Fatalf("%s: event %+v for record %+v", name, evs[i], r)
			}
		}
		if n := sink.count(notify.EventNoUpdate, name); n != 1 {
			t.Fatalf("%s: update.skipped delivered %d times over two runs, want once", name, n)
		}
	}
	if n := h.pulls(); n != 0 {
		t.Fatalf("%d pulls; a container whose labels do not resolve must not be checked", n)
	}
}

func TestPolicyTagFilterNarrowsCandidates(t *testing.T) {
	const cur = "example.com/app:1.2.0"
	h := newFakeHost(t, "1.47", minorPolicy(config.TagFilter{Exclude: `^1\.4\.`}))
	h.reg.setTags("example.com/app", "1.2.0", "1.3.0", "1.4.0", "1.4.1-rc1", "2.0.0")
	h.d.AddImage(dockertest.Image{ID: "v120"}, cur)
	h.d.Publish("example.com/app:1.3.0", dockertest.Image{ID: "v130"})
	h.d.Publish("example.com/app:1.4.0", dockertest.Image{ID: "v140"})
	h.d.Run("web", &container.Config{Image: cur, Labels: map[string]string{"dockpaum.policy": "minor"}}, nil, nil)
	h.d.Run("pinned", &container.Config{Image: cur, Labels: map[string]string{"dockpaum.policy": "minor", "dockpaum.tag.include": `^1\.2\.`}}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionUpdated || r.NewTag != "1.3.0" {
		t.Fatalf("web: %s to %q, want updated to 1.3.0 (1.4.0 is excluded)", r.Decision, r.NewTag)
	}
	if got := h.inspect("web").Config.Image; got != "example.com/app:1.3.0" {
		t.Fatalf("web runs %s", got)
	}
	r := h.recordFor("pinned")
	if r.Decision != audit.DecisionSkipped || !strings.Contains(r.Reason, "after the policy's tag filter, which excluded 4 of 5 listed tag(s)") {
		t.Fatalf("pinned: %s %q", r.Decision, r.Reason)
	}
}

func TestTagCheckWithoutCandidatesIsIncomplete(t *testing.T) {
	const bitnami = "example.com/db:16.4.0-debian-12-r12"
	h := newFakeHost(t, "1.47", minorPolicy(config.TagFilter{}))
	h.reg.setTags("example.com/db", "16.4.0-debian-12-r12", "16.4.1", "16.5.0", "latest")
	h.reg.setTags("example.com/app", "1.2.0", "1.3.0")
	h.d.AddImage(dockertest.Image{ID: "db1"}, bitnami)
	h.d.AddImage(dockertest.Image{ID: "app1"}, "example.com/app:1.2.0")
	h.d.Run("db", &container.Config{Image: bitnami, Labels: map[string]string{"dockpaum.policy": "minor"}}, nil, nil)
	h.d.Run("filtered", &container.Config{Image: "example.com/app:1.2.0",
		Labels: map[string]string{"dockpaum.policy": "minor", "dockpaum.tag.exclude": `^1\.3`}}, nil, nil)
	for range 2 {
		if err := h.run(); err != nil {
			t.Fatalf("an incomplete check must not fail the run: %v", err)
		}
	}
	recs := h.records("db")
	if len(recs) != 2 || recs[0].Decision != decisionCheckIncomplete || !strings.Contains(recs[0].Reason, "no candidates for variant") {
		t.Fatalf("db: %+v", recs)
	}
	if s := h.status("db"); s.Update != UpdateUnknown || s.Pending {
		t.Fatalf("status %+v: an incomplete check finds nothing either way", s)
	}
	if strings.Contains(scrape(t, h.u.deps.Metrics), `reason="check-incomplete"`) {
		t.Fatal("an incomplete check is not a gate and is not counted as a skipped update")
	}
	if r := h.records("filtered"); len(r) != 2 || r[0].Decision != audit.DecisionSkipped || !strings.Contains(r[0].Reason, "after the policy's tag filter") {
		t.Fatalf("filtered: %+v", r)
	}
}

func TestRateLimitedRegistryIsNotQueriedAgain(t *testing.T) {
	const cur = "example.com/app:1.2.0"
	t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	h := newFakeHost(t, "1.47", minorPolicy(config.TagFilter{}))
	h.u.now = fixedClock(t0)
	h.d.AddImage(dockertest.Image{ID: "v1"}, cur)
	for _, name := range []string{"a", "b"} {
		h.d.Run(name, &container.Config{Image: cur, Labels: map[string]string{"dockpaum.policy": "minor"}}, nil, nil)
	}
	h.reg.setFail("example.com/app", &registry.RateLimitError{Registry: "example.com", Op: "tags/list"})
	if err := h.run(); err == nil {
		t.Fatal("unchecked containers fail the run")
	}
	if n := h.reg.count("tags "); n != 1 {
		t.Fatalf("%d tag lists, want 1: a registry that answered 429 is not asked again in the run", n)
	}
	notQueried := 0
	for _, name := range []string{"a", "b"} {
		r := h.recordFor(name)
		if r.Decision != audit.DecisionCheckFailed || !strings.Contains(r.Error, "rate limit exceeded") {
			t.Fatalf("%s: %s %q", name, r.Decision, r.Error)
		}
		if strings.Contains(r.Error, "not queried") {
			notQueried++
		}
	}
	if notQueried != 1 {
		t.Fatalf("%d containers report the skipped query, want 1", notQueried)
	}

	h.reg.setFail("example.com/app", &registry.RateLimitError{Registry: "example.com", Op: "tags/list", RetryAfter: time.Hour})
	_ = h.run()
	if n := h.reg.count("tags "); n != 2 {
		t.Fatalf("%d tag lists, want the next run to ask again", n)
	}
	h.u.now = fixedClock(t0.Add(30 * time.Minute))
	_ = h.run()
	if n := h.reg.count("tags "); n != 2 {
		t.Fatalf("%d tag lists, want none before Retry-After passed", n)
	}
	h.reg.setFail("example.com/app", nil)
	h.reg.setTags("example.com/app", "1.2.0")
	h.u.now = fixedClock(t0.Add(2 * time.Hour))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := h.reg.count("tags "); n != 4 {
		t.Fatalf("%d tag lists, want both containers checked once Retry-After passed", n)
	}
}

func TestReleaseNotesLinks(t *testing.T) {
	gh := map[string]string{"org.opencontainers.image.source": "https://github.com/acme/app", "org.opencontainers.image.version": "1.2.3"}
	h := newFakeHost(t, "1.47", nil)
	rec := &recordedEvents{}
	h.u.send = rec.send
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2", Config: &container.Config{Labels: gh}})
	h.d.Run("mon", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)

	tmplRef := "example.com/tmpl:2"
	h.release(tmplRef, dockertest.Image{ID: "t1"}, dockertest.Image{ID: "t2", Config: &container.Config{Labels: map[string]string{"org.opencontainers.image.version": "2.0.1"}}})
	h.d.Run("tmpl", &container.Config{Image: tmplRef, Labels: map[string]string{linkTemplateLabel: "https://example.org/notes/${version}"}}, nil, nil)

	inhRef := "example.com/inh:1"
	h.release(inhRef,
		dockertest.Image{ID: "i1", Config: &container.Config{Labels: map[string]string{linkTemplateLabel: "https://old.example/${tag}"}}},
		dockertest.Image{ID: "i2", Config: &container.Config{Labels: map[string]string{linkTemplateLabel: "https://new.example/${tag}"}}})
	h.d.Run("inh", &container.Config{Image: inhRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}

	const want = "https://github.com/acme/app/releases/tag/v1.2.3"
	evs := rec.of(notify.EventAvailable, "mon")
	if len(evs) != 1 || evs[0].ReleaseURL != want {
		t.Fatalf("monitor-only events %+v, want release link %s", evs, want)
	}
	items := rec.items("mon")
	if len(items) != 1 || items[0].ReleaseURL != want || items[0].OldDigest != "sha256:v1" || items[0].NewDigest != "sha256:v2" {
		t.Fatalf("digest items %+v", items)
	}
	if c := h.status("mon").Candidate; c == nil || c.ReleaseURL != want {
		t.Fatalf("status candidate %+v", c)
	}
	for name, want := range map[string]string{"tmpl": "https://example.org/notes/2.0.1", "inh": "https://new.example/1"} {
		evs := rec.of(notify.EventUpdateSuccess, name)
		if len(evs) != 1 || evs[0].ReleaseURL != want {
			t.Fatalf("%s: success events %+v, want release link %s", name, evs, want)
		}
		if items := rec.items(name); len(items) != 1 || items[0].ReleaseURL != want || items[0].NewDigest == "" {
			t.Fatalf("%s: digest items %+v", name, items)
		}
	}
}

func TestDryRunItemsCarryDigests(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.DryRun = true })
	rec := &recordedEvents{}
	h.u.send = rec.send
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	items := rec.items("web")
	if len(items) != 1 || items[0].Result != string(audit.DecisionDryRun) || items[0].OldDigest != "sha256:v1" || items[0].NewDigest != "sha256:v2" {
		t.Fatalf("dry-run items %+v; the candidate digest lets the digest deduplicate the line", items)
	}
}

func TestApprovalDenialIsAReasonNotAnError(t *testing.T) {
	url, _ := approver(t, func() int { return 403 })
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Approval = &config.ApprovalConfig{URL: url} })
	rec := &recordedEvents{}
	h.u.send = rec.send
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	r := h.recordFor("web")
	evs := rec.of(notify.EventDenied, "web")
	if r.Decision != audit.DecisionApprovalDeny || len(evs) != 1 {
		t.Fatalf("decision %s, events %+v", r.Decision, evs)
	}
	if ev := evs[0]; ev.Err != nil || ev.Reason != r.Reason || !strings.Contains(ev.Reason, "denied by") || ev.NewDigest != "sha256:v2" {
		t.Fatalf("event %+v: the denial is the reason", ev)
	}
}

func TestPolicyMinImageAgeSource(t *testing.T) {
	epoch := dockertest.Image{ID: "v2", Created: "1970-01-01T00:00:00Z"}
	tests := []struct {
		name   string
		source string
		want   audit.Decision
	}{
		{name: "auto", want: audit.DecisionUpdated},
		{name: "created holds what it cannot measure", source: "created", want: audit.DecisionGatedAge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				c.Update.Policies[0].MinImageAge = config.Duration(time.Hour)
				c.Update.Policies[0].MinImageAgeSource = tc.source
			})
			h.release(appRef, dockertest.Image{ID: "v1"}, epoch)
			h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if r := h.recordFor("web"); r.Decision != tc.want {
				t.Fatalf("decision %s (%s), want %s", r.Decision, r.Reason, tc.want)
			}
		})
	}
}

func TestAgeSourceOfPolicies(t *testing.T) {
	cfg := &config.Config{Update: config.UpdatePolicy{Policies: []config.Policy{
		{ID: "Default", MinImageAgeSource: "First-Seen"},
		{ID: "db", MinImageAgeSource: "created"},
		{ID: "web"},
		{ID: "minor", MinImageAgeSource: "bogus"},
	}}}
	u := &Updater{ageSources: policyAgeSources(cfg)}
	for id, want := range map[string]string{
		"default": ageFirstSeen, "db": ageCreated, "web": ageAuto, "minor": ageAuto,
		"major": ageFirstSeen,
	} {
		if got := u.ageSourceOf(id); got != want {
			t.Errorf("ageSourceOf(%s) = %s, want %s", id, got, want)
		}
	}
	if got := (&Updater{}).ageSourceOf("default"); got != ageAuto {
		t.Errorf("without policies: %s", got)
	}
}

func TestRunNarrowedByImage(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.release("example.com/db:1", dockertest.Image{ID: "d1"}, dockertest.Image{ID: "d2"})
	h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
	h.d.Run("db", &container.Config{Image: "example.com/db:1"}, nil, nil)
	opts := RunOptions{Trigger: "api", Images: []string{"example.com/app"}}
	if err := h.u.RunWith(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if h.inspect("web").Image != "sha256:v2" || h.inspect("db").Image != "sha256:d1" || len(h.records("db")) != 0 {
		t.Fatal("only the container created from the requested image is updated")
	}
	last := h.u.LastRun()
	if last.Scanned != 1 || !slices.Equal(last.Images, opts.Images) {
		t.Fatalf("summary %+v", last)
	}
}

func TestRunOptionsKey(t *testing.T) {
	a := RunOptions{Policy: "db", Containers: []string{"b", "a"}}
	if got := a.Key(); got != "policy=db;containers=a,b" {
		t.Fatalf("key without images = %q", got)
	}
	b := RunOptions{Images: []string{"redis", "nginx"}}
	c := RunOptions{Images: []string{"nginx", "redis"}}
	if b.Key() != c.Key() || b.Key() != "policy=;containers=;images=nginx,redis" || b.Key() == (RunOptions{}).Key() {
		t.Fatalf("image keys %q %q", b.Key(), c.Key())
	}
	if (RunOptions{}).regular() != true || b.regular() || a.regular() {
		t.Fatal("only an unnarrowed run is regular")
	}
}

func TestRegistryBackoff(t *testing.T) {
	t0 := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	var b registryBackoff
	if b.note("Example.com", "r1", t0, errors.New("boom")) {
		t.Fatal("only rate limits are recorded")
	}
	if err := b.blocked("example.com", "r1", t0); err != nil {
		t.Fatalf("blocked without a refusal: %v", err)
	}
	limit := fmt.Errorf("tags: %w", &registry.RateLimitError{Registry: "example.com", RetryAfter: 30 * 24 * time.Hour})
	if !b.note("Example.com", "r1", t0, limit) || b.note("example.com", "r1", t0, limit) {
		t.Fatal("the first refusal of a run is reported once")
	}
	if err := b.blocked("example.com", "r1", t0); !errors.Is(err, registry.ErrRateLimited) || !strings.Contains(err.Error(), "this run") {
		t.Fatalf("same run: %v", err)
	}
	if err := b.blocked("example.com", "r2", t0.Add(maxRegistryBackoff-time.Minute)); err == nil {
		t.Fatal("a later run waits for Retry-After")
	}
	if err := b.blocked("example.com", "r2", t0.Add(maxRegistryBackoff)); err != nil {
		t.Fatalf("Retry-After is capped at %s: %v", maxRegistryBackoff, err)
	}
	if err := b.blocked("example.com", "r1", t0); err != nil {
		t.Fatal("an expired refusal is forgotten")
	}
}

func TestIncompleteCheckWarnsOncePerEpisode(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	var buf strings.Builder
	var mu sync.Mutex
	h.u.deps.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnings := func() int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), "tag check incomplete")
	}
	h.u.warnIncomplete("fake", "db", "db:1-x", "no candidates")
	h.u.warnIncomplete("fake", "db", "db:1-x", "no candidates")
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings for an unchanged result, want 1", n)
	}
	h.u.warnIncomplete("fake", "db", "db:1-x", "")
	h.u.warnIncomplete("fake", "db", "db:1-x", "no candidates")
	if n := warnings(); n != 2 {
		t.Fatalf("%d warnings, want a new one after the check completed in between", n)
	}
	h.u.retainLabelNotes("fake", nil)
	if len(h.u.incomplete) != 0 {
		t.Fatal("notes of removed containers are dropped")
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestOverrideLabelsApply(t *testing.T) {
	const cur = "example.com/app:1.2.0"
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].MinImageAge = config.Duration(time.Hour) })
	h.reg.setTags("example.com/app", "1.2.0", "1.3.0", "2.0.0")
	h.d.AddImage(dockertest.Image{ID: "v120"}, cur)
	h.d.Publish("example.com/app:1.3.0", dockertest.Image{ID: "v130", Created: recent})
	h.d.Run("web", &container.Config{Image: cur, Labels: map[string]string{"dockpaum.mode": "minor", "dockpaum.min-age": "0s"}}, nil, nil)
	h.d.Run("soaking", &container.Config{Image: cur, Labels: map[string]string{"dockpaum.mode": "minor"}}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if r := h.recordFor("web"); r.Decision != audit.DecisionUpdated || r.NewTag != "1.3.0" || r.Policy != "default" {
		t.Fatalf("web: %+v", r)
	}
	if r := h.recordFor("soaking"); r.Decision != audit.DecisionGatedAge {
		t.Fatalf("soaking: %s %q, want the policy's soak", r.Decision, r.Reason)
	}
}
