package notify

import "github.com/nicolaeser/dockpaum/internal/state"

func NewStateLedger(st *state.Store) Ledger {
	if st == nil {
		return NewMemoryLedger()
	}
	return stateLedger{st: st}
}

type stateLedger struct{ st *state.Store }

func (l stateLedger) Mark(k NoticeKey, fp string) bool {
	return l.st.MarkNotice(state.NoticeKey(k), fp)
}

func (l stateLedger) Commit(k NoticeKey, fp string) { l.st.CommitNotice(state.NoticeKey(k), fp) }

func (l stateLedger) Unmark(k NoticeKey, fp string) { l.st.UnmarkNotice(state.NoticeKey(k), fp) }

func (l stateLedger) Forget(host, container string) { l.st.ForgetNotices(host, container) }
