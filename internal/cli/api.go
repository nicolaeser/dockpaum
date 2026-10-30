package cli

import (
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/server"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

func containersHook(statuses func() []updater.ContainerStatus) func(server.ContainersQuery) any {
	return func(q server.ContainersQuery) any {
		out := []updater.ContainerStatus{}
		for _, c := range statuses() {
			if q.Matches(c.Host, c.Container, c.Pending) {
				out = append(out, c)
			}
		}
		return out
	}
}

type summarySource struct {
	statuses func() []updater.ContainerStatus
	recent   func(since time.Time) map[audit.Decision]int
	nextRun  func() time.Time
	now      func() time.Time
	dryRun   bool
	version  string
	instance string
}

func (s summarySource) summary() server.Summary {
	out := server.Summary{DryRun: s.dryRun, Version: s.version, Instance: s.instance}
	for _, c := range s.statuses() {
		out.Monitored++
		if c.Pending {
			out.PendingUpdates++
		}
		if audit.Decision(c.Decision).Gated() {
			out.Gated++
		}
	}
	for d, n := range s.recent(s.now().Add(-audit.RecentWindow)) {
		switch {
		case d.Updated():
			out.Updated24h += n
		case d.Failed():
			out.Failed24h += n
		}
	}
	if s.nextRun != nil {
		if t := s.nextRun(); !t.IsZero() {
			out.NextRunAt = &t
		}
	}
	return out
}
