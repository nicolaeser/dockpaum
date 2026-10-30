package updater

import (
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func NoticeLedger(st *state.Store) notify.Ledger { return stateLedger{st} }

type stateLedger struct{ st *state.Store }

func (l stateLedger) Mark(k notify.NoticeKey, fingerprint string) bool {
	return l.st.MarkNotice(state.NoticeKey(k), fingerprint)
}

func (l stateLedger) Commit(k notify.NoticeKey, fingerprint string) {
	l.st.CommitNotice(state.NoticeKey(k), fingerprint)
}

func (l stateLedger) Unmark(k notify.NoticeKey, fingerprint string) {
	l.st.UnmarkNotice(state.NoticeKey(k), fingerprint)
}

func (l stateLedger) Forget(host, container string) { l.st.ForgetNotices(host, container) }
