package notify

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func openState(t *testing.T, dir string) *state.Store {
	t.Helper()
	st := state.OpenWith(state.PathIn(dir), state.Options{Logger: discardLogger(), FlushDelay: -1})
	if !st.Durable() {
		t.Fatal("the state in a temporary directory is not durable")
	}
	return st
}

func closeState(t *testing.T, st *state.Store) {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatalf("closing the state: %v", err)
	}
}

func countType(r *recorder, typ string) int {
	n := 0
	for _, ty := range r.types() {
		if ty == typ {
			n++
		}
	}
	return n
}

func TestStateLedgerKeepsNoticesAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	no := false
	cfgs := []config.NotifierConfig{
		{Type: "discord", Name: "ops", URL: "http://x"},
		{Type: "webhook", Name: "audit", URL: "http://x", Events: []string{EventAvailable}},
		{Type: "slack", Name: "loud", URL: "http://x", Events: []string{EventAvailable}, Once: &no},
	}
	line := func(candidate string) RunItem {
		return RunItem{Host: "local", Container: "web", Image: "nginx:latest", OldTag: "latest", Result: "monitor-only", NewDigest: candidate}
	}
	lifetime := func(evs ...Event) (ops, audit, loud *recorder) {
		t.Helper()
		st := openState(t, dir)
		m, recs := recorded(t, cfgs...)
		m.AttachLedger(NewStateLedger(st))
		for _, ev := range evs {
			send1(m, ev)
		}
		closeState(t, st)
		return recs[0], recs[1], recs[2]
	}
	check := func(step string, r *recorder, typ string, want int) {
		t.Helper()
		if n := countType(r, typ); n != want {
			t.Errorf("%s: %s delivered %d times, want %d", step, typ, n, want)
		}
	}

	ops, audit, loud := lifetime(monitorOnly("sha256:bbb"), digest(line("sha256:bbb")), monitorOnly("sha256:bbb"), digest(line("sha256:bbb")))
	check("first start: ops", ops, EventAvailable, 0)
	check("first start: ops", ops, EventRunCompleted, 1)
	check("first start: audit", audit, EventAvailable, 1)
	check("first start: loud (once: false)", loud, EventAvailable, 2)

	ops, audit, loud = lifetime(monitorOnly("sha256:bbb"), digest(line("sha256:bbb")))
	check("after a restart: ops", ops, EventRunCompleted, 0)
	check("after a restart: audit", audit, EventAvailable, 0)
	check("after a restart: loud", loud, EventAvailable, 1)

	ops, audit, _ = lifetime(monitorOnly("sha256:ccc"), digest(line("sha256:ccc")))
	check("a newer candidate: ops", ops, EventRunCompleted, 1)
	check("a newer candidate: audit", audit, EventAvailable, 1)

	reason := monitorOnly("sha256:ccc")
	reason.Reason = "dry-run"
	_, audit, _ = lifetime(reason, reason)
	check("another reason for the same candidate", audit, EventAvailable, 0)

	other := reason
	other.Host = "nas"
	_, audit, _ = lifetime(reason, other)
	check("the same container on another host", audit, EventAvailable, 1)

	lifetime(Event{Type: EventUpdateSuccess, Host: "local", Container: "web"})
	ops, audit, _ = lifetime(reason, digest(line("sha256:ccc")))
	check("after an update: ops", ops, EventRunCompleted, 1)
	check("after an update: audit", audit, EventAvailable, 1)

	st := openState(t, dir)
	defer closeState(t, st)
	raw, err := st.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hosts map[string]struct {
			Notices map[string]map[string]map[string]json.RawMessage `json:"notices"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	notifiers := slices.Sorted(maps.Keys(doc.Hosts["local"].Notices["web"]))
	if strings.Join(notifiers, ",") != "discord:ops,webhook:audit" {
		t.Fatalf("notices recorded for %v, want ops and audit", notifiers)
	}
}

func TestStateLedgerFollowsTypeAndName(t *testing.T) {
	dir := t.TempDir()
	deliver := func(cfgs ...config.NotifierConfig) []int {
		t.Helper()
		st := openState(t, dir)
		defer closeState(t, st)
		m, recs := recorded(t, cfgs...)
		m.AttachLedger(NewStateLedger(st))
		send1(m, monitorOnly("sha256:bbb"))
		got := make([]int, len(recs))
		for i, r := range recs {
			got[i] = countType(r, EventAvailable)
		}
		return got
	}
	ops := config.NotifierConfig{Type: "discord", Name: "ops", URL: "http://x", Events: []string{EventAvailable}}
	moved, renamed := ops, ops
	moved.Type = "slack"
	renamed.Name = "team"
	if got := deliver(ops); !slices.Equal(got, []int{1}) {
		t.Fatalf("first start: deliveries = %v, want [1]", got)
	}
	if got := deliver(ops, moved, renamed); !slices.Equal(got, []int{0, 1, 1}) {
		t.Fatalf("deliveries = %v, want [0 1 1]: only the unchanged notifier keeps its record", got)
	}
}

type stalled struct{ began, release chan struct{} }

func (s stalled) Notify(context.Context, Event) error {
	s.began <- struct{}{}
	<-s.release
	return nil
}

func TestStateLedgerRepeatsUndeliveredNotices(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NotifierConfig{Type: "ntfy", Name: "phone", URL: "http://x", Events: []string{EventAvailable}}
	failed := monitorOnly("sha256:bbb")
	interrupted := monitorOnly("sha256:bbb")
	interrupted.Container = "db"

	st := openState(t, dir)
	m, recs := recorded(t, cfg)
	m.AttachLedger(NewStateLedger(st))
	recs[0].fail.Store(true)
	send1(m, failed)
	s := stalled{began: make(chan struct{}, 1), release: make(chan struct{})}
	m.items[0].n = s
	m.Notify(context.Background(), interrupted)
	<-s.began
	closeState(t, st)

	next := openState(t, dir)
	m2, recs2 := recorded(t, cfg)
	m2.AttachLedger(NewStateLedger(next))
	for i := 0; i < 2; i++ {
		send1(m2, failed)
		send1(m2, interrupted)
	}
	closeState(t, next)
	if got := recs2[0].types(); len(got) != 2 {
		t.Fatalf("after the restart the undelivered notices were sent %d times, want once each", len(got))
	}
	close(s.release)
	m.Wait(3 * time.Second)

	last := openState(t, dir)
	defer closeState(t, last)
	m3, recs3 := recorded(t, cfg)
	m3.AttachLedger(NewStateLedger(last))
	send1(m3, failed)
	send1(m3, interrupted)
	if n := len(recs3[0].types()); n != 0 {
		t.Fatalf("delivered notices were repeated after the restart (%d times)", n)
	}
}

func TestStateLedgerSharesNothingBetweenNotifiers(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NotifierConfig{Type: "gotify", URL: "http://x", Token: "t", Events: []string{EventAvailable}}

	st := openState(t, dir)
	m, recs := recorded(t, cfg, cfg)
	m.AttachLedger(NewStateLedger(st))
	recs[1].fail.Store(true)
	send1(m, monitorOnly("sha256:bbb"))
	closeState(t, st)
	if len(recs[0].types()) != 1 || len(recs[1].types()) != 1 {
		t.Fatalf("deliveries = %v, %v", recs[0].types(), recs[1].types())
	}

	st = openState(t, dir)
	defer closeState(t, st)
	m, recs = recorded(t, cfg, cfg, cfg)
	m.AttachLedger(NewStateLedger(st))
	send1(m, monitorOnly("sha256:bbb"))
	got := []int{len(recs[0].types()), len(recs[1].types()), len(recs[2].types())}
	if !slices.Equal(got, []int{0, 1, 1}) {
		t.Fatalf("deliveries after adding a notifier = %v, want [0 1 1]", got)
	}
}

func TestStateLedgerInMemory(t *testing.T) {
	for name, l := range map[string]Ledger{
		"in-memory store": NewStateLedger(state.Open("", discardLogger())),
		"nil store":       NewStateLedger(nil),
	} {
		t.Run(name, func(t *testing.T) {
			m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{EventAvailable, EventUpdateSuccess}})
			m.AttachLedger(l)
			send1(m, monitorOnly("sha256:bbb"))
			send1(m, monitorOnly("sha256:bbb"))
			send1(m, Event{Type: EventUpdateSuccess, Host: "local", Container: "web"})
			send1(m, monitorOnly("sha256:bbb"))
			if got := countType(recs[0], EventAvailable); got != 2 {
				t.Fatalf("update.skipped delivered %d times, want 2 (once, and again after the update)", got)
			}
		})
	}
}

func TestStateLedgerContract(t *testing.T) {
	l := NewStateLedger(state.Open("", discardLogger()))
	k := NoticeKey{Notifier: "ops", Event: EventNoUpdate, Host: "h", Container: "c"}
	if !l.Mark(k, "a") || l.Mark(k, "a") {
		t.Fatal("first mark must be fresh, a concurrent repeat not")
	}
	l.Commit(k, "a")
	if l.Mark(k, "a") {
		t.Fatal("a committed notice is not fresh")
	}
	if !l.Mark(k, "b") {
		t.Fatal("a new fingerprint is fresh")
	}
	l.Unmark(k, "b")
	if l.Mark(k, "a") {
		t.Fatal("a failed delivery must keep the notice delivered before")
	}
	if !l.Mark(k, "b") {
		t.Fatal("Unmark did not release the claim")
	}
	l.Unmark(k, "a")
	if l.Mark(k, "b") {
		t.Fatal("a stale Unmark released the current claim")
	}
	l.Forget("h", "c")
	if !l.Mark(k, "a") {
		t.Fatal("Forget must drop the container's notices")
	}
}
