package updater

import (
	"context"
	"sync"
	"time"
)

type RunInProgress struct {
	ID      string
	Started time.Time
}

type activeRuns struct {
	mu   sync.Mutex
	runs []RunInProgress
}

func (a *activeRuns) begin(id string, started time.Time) func() {
	a.mu.Lock()
	a.runs = append(a.runs, RunInProgress{ID: id, Started: started})
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, r := range a.runs {
			if r.ID == id {
				a.runs = append(a.runs[:i], a.runs[i+1:]...)
				return
			}
		}
	}
}

func (u *Updater) Busy() (RunInProgress, bool) {
	u.active.mu.Lock()
	defer u.active.mu.Unlock()
	if len(u.active.runs) == 0 {
		return RunInProgress{}, false
	}
	return u.active.runs[0], true
}

type dryRunKey struct{}

func withDryRun(ctx context.Context, on bool) context.Context {
	if !on {
		return ctx
	}
	return context.WithValue(ctx, dryRunKey{}, true)
}

func (u *Updater) dryRun(ctx context.Context) bool {
	if u.deps.Config.DryRun {
		return true
	}
	on, _ := ctx.Value(dryRunKey{}).(bool)
	return on
}
