package cli

import (
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

func statePath(sc config.StateConfig) string {
	if sc.Enabled != nil && !*sc.Enabled {
		return ""
	}
	dir := strings.TrimSpace(sc.Dir)
	if dir == "" {
		dir = state.DefaultDir
	}
	return state.PathIn(dir)
}

type stateLedger struct{ st *state.Store }

var _ notify.Ledger = stateLedger{}

func (l stateLedger) Mark(k notify.NoticeKey, fp string) bool {
	return l.st.MarkNotice(state.NoticeKey(k), fp)
}

func (l stateLedger) Commit(k notify.NoticeKey, fp string) {
	l.st.CommitNotice(state.NoticeKey(k), fp)
}

func (l stateLedger) Unmark(k notify.NoticeKey, fp string) {
	l.st.UnmarkNotice(state.NoticeKey(k), fp)
}

func (l stateLedger) Forget(host, container string) {
	l.st.ForgetNotices(host, container)
}

func updateAvailability(statuses []updater.ContainerStatus) []metrics.UpdateAvailable {
	out := make([]metrics.UpdateAvailable, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, metrics.UpdateAvailable{Host: s.Host, Container: s.Container, Policy: s.Policy, Pending: s.Pending})
	}
	return out
}
