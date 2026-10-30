package updater

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/registry"
)

const maxRegistryBackoff = 6 * time.Hour

type registryBackoff struct {
	mu sync.Mutex
	m  map[string]backoffEntry
}

type backoffEntry struct {
	runID string
	until time.Time
	err   error
}

func backoffKey(host string) string { return strings.ToLower(host) }

func (b *registryBackoff) note(host, runID string, now time.Time, err error) bool {
	if !errors.Is(err, registry.ErrRateLimited) {
		return false
	}
	var wait time.Duration
	var rl *registry.RateLimitError
	if errors.As(err, &rl) {
		wait = min(rl.RetryAfter, maxRegistryBackoff)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]backoffEntry{}
	}
	k := backoffKey(host)
	prev, had := b.m[k]
	b.m[k] = backoffEntry{runID: runID, until: now.Add(wait), err: err}
	return !had || prev.runID != runID
}

func (b *registryBackoff) blocked(host, runID string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[backoffKey(host)]
	switch {
	case !ok:
		return nil
	case e.runID == runID:
		return fmt.Errorf("not queried: %s refused an earlier request of this run: %w", host, e.err)
	case now.Before(e.until):
		return fmt.Errorf("not queried before %s: %s asked to wait after refusing a request: %w",
			e.until.UTC().Format(time.RFC3339), host, e.err)
	}
	delete(b.m, backoffKey(host))
	return nil
}

func (u *Updater) noteRateLimit(host, runID string, err error) {
	if u.backoff.note(host, runID, u.now(), err) {
		u.deps.Logger.Warn("registry rate limit reached: it is not queried directly again in this run, nor before the delay it asked for",
			"registry", host, "run_id", runID, "err", err)
	}
}
