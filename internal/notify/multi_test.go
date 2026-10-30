package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type recorder struct {
	mu   sync.Mutex
	got  []Event
	fail atomic.Bool
}

func (r *recorder) Notify(_ context.Context, ev Event) error {
	r.mu.Lock()
	r.got = append(r.got, ev)
	r.mu.Unlock()
	if r.fail.Load() {
		return errors.New("endpoint down")
	}
	return nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.got))
	for _, ev := range r.got {
		out = append(out, ev.Type)
	}
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.got = nil
	r.mu.Unlock()
}

func recorded(t *testing.T, cfgs ...config.NotifierConfig) (*Multi, []*recorder) {
	t.Helper()
	m := Build(cfgs, discardLogger())
	recs := make([]*recorder, len(m.items))
	for i := range m.items {
		recs[i] = &recorder{}
		m.items[i].n = recs[i]
	}
	return m, recs
}

func send1(m *Multi, ev Event) {
	m.Notify(context.Background(), ev)
	m.Wait(3 * time.Second)
}

func gated(candidate string) Event {
	return Event{
		Type: EventNoUpdate, Host: "local", Container: "web", Image: "nginx:latest",
		OldDigest: "sha256:aaa", NewDigest: candidate,
		Reason: `outside policy "default" window`,
	}
}

func monitorOnly(candidate string) Event {
	return Event{
		Type: EventAvailable, Host: "local", Container: "web", Image: "nginx:latest",
		OldDigest: "sha256:aaa", NewDigest: candidate,
		Reason: "update available; the container is labelled monitor-only",
	}
}

func digest(items ...RunItem) Event {
	return Event{Type: EventRunCompleted, Host: "local", RunStats: &RunStats{Scanned: 3, Items: items}}
}

func TestSubscriptions(t *testing.T) {
	everything := []Event{
		{Type: EventUpdateSuccess, Container: "a"},
		{Type: EventUpdateFailure, Container: "b"},
		monitorOnly("sha256:bbb"),
		gated("sha256:bbb"),
		{Type: EventDenied, Container: "c", NewDigest: "sha256:bbb", Reason: "denied by https://approver.example (HTTP 403)"},
		{Type: EventRolledBack, Container: "d", Err: errors.New("unhealthy")},
		{Type: EventRecovered, Container: "e", Reason: "restored from leftover e.dockpaum-old-1"},
		{Type: EventCheckFailed, Container: "f", Err: errors.New("registry unreachable")},
		{Type: EventSelfUpdateStarted, Container: "dockpaum"},
		{Type: EventSelfUpdateSuccess, Container: "dockpaum"},
		{Type: EventSelfUpdateFailure, Container: "dockpaum", Err: errors.New("unhealthy")},
		{Type: EventRunStarted},
		{Type: EventRunCompleted, RunStats: &RunStats{Scanned: 5}},
		{Type: EventStarted, Startup: &StartupInfo{Version: "1.0.0"}},
		{Type: EventStopping},
		{Type: EventTest},
	}
	cases := []struct {
		name   string
		events []string
		want   []string
	}{
		{"default is quiet", nil, []string{EventUpdateSuccess, EventUpdateFailure}},
		{"empty list is the default", []string{}, []string{EventUpdateSuccess, EventUpdateFailure}},
		{"standing notices only when listed", []string{EventAvailable, EventNoUpdate, EventDenied}, []string{EventAvailable, EventNoUpdate, EventDenied}},
		{"explicit run.completed gets every run", []string{EventRunCompleted}, []string{EventRunCompleted}},
		{"run.started only when listed", []string{EventRunStarted, EventUpdateFailure}, []string{EventUpdateFailure, EventRunStarted}},
		{"startup message is opt-in", []string{" dockpaum.started "}, []string{EventStarted}},
		{"letter case is ignored", []string{"Update.Failure", "RUN.STARTED"}, []string{EventUpdateFailure, EventRunStarted}},
		{"star is everything", []string{"*"}, knownEvents},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: tc.events})
			for _, ev := range everything {
				send1(m, ev)
			}
			if got := recs[0].types(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("delivered %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuietRunCompleted(t *testing.T) {
	m, recs := recorded(t,
		config.NotifierConfig{Type: "slack", Name: "quiet", URL: "http://x"},
		config.NotifierConfig{Type: "slack", Name: "heartbeat", URL: "http://x", Events: []string{EventRunCompleted}},
	)
	quiet, heartbeat := recs[0], recs[1]
	steps := []struct {
		name      string
		ev        Event
		wantQuiet bool
	}{
		{"empty run", digest(), false},
		{"run error", Event{Type: EventRunCompleted, Err: errors.New("docker host(s) unreachable: nas"), RunStats: &RunStats{}}, true},
		{"no stats, no error", Event{Type: EventRunCompleted}, false},
		{"updated count", Event{Type: EventRunCompleted, RunStats: &RunStats{Updated: 1}}, true},
		{"failed count", Event{Type: EventRunCompleted, RunStats: &RunStats{Failed: 1}}, true},
		{"first monitor-only line", digest(RunItem{Host: "local", Container: "web", Result: "monitor-only", NewDigest: "sha256:b"}), true},
		{"same line again", digest(RunItem{Host: "local", Container: "web", Result: "monitor-only", NewDigest: "sha256:b"}), false},
		{"new candidate", digest(RunItem{Host: "local", Container: "web", Result: "monitor-only", NewDigest: "sha256:c"}), true},
		{"dry-run line", digest(RunItem{Host: "local", Container: "db", Result: "dry-run", NewTag: "16.5"}), true},
		{"dry-run repeated next to the old one", digest(
			RunItem{Host: "local", Container: "web", Result: "monitor-only", NewDigest: "sha256:c"},
			RunItem{Host: "local", Container: "db", Result: "dry-run", NewTag: "16.5"},
		), false},
		{"a failure is news", digest(RunItem{Host: "local", Container: "api", Result: "check-failed", Error: "registry down"}), true},
		{"so is the same failure again", digest(RunItem{Host: "local", Container: "api", Result: "check-failed", Error: "registry down"}), true},
		{"an unknown result is news", digest(RunItem{Host: "local", Container: "api", Result: "recovered"}), true},
		{"gated lines are standing", digest(RunItem{Host: "local", Container: "cache", Result: "gated-window", NewTag: "7.4"}), true},
		{"gated line repeated", digest(RunItem{Host: "local", Container: "cache", Result: "gated-window", NewTag: "7.4"}), false},
		{"line without a candidate", digest(RunItem{Host: "local", Container: "proxy", Image: "nginx:latest", OldTag: "latest", Result: "monitor-only"}), true},
		{"and again", digest(RunItem{Host: "local", Container: "proxy", Image: "nginx:latest", OldTag: "latest", Result: "monitor-only"}), true},
		{"an unchanged tag is no candidate", digest(RunItem{Host: "local", Container: "proxy", OldTag: "latest", NewTag: "latest", Result: "dry-run"}), true},
	}
	for i, s := range steps {
		quiet.reset()
		send1(m, s.ev)
		if got := len(quiet.types()) == 1; got != s.wantQuiet {
			t.Errorf("step %d (%s): quiet notifier delivered=%v, want %v", i, s.name, got, s.wantQuiet)
		}
	}
	if n := len(heartbeat.types()); n != len(steps) {
		t.Fatalf("explicit run.completed got %d of %d runs", n, len(steps))
	}
}

func TestStandingNoticesAreAnnouncedOncePerCandidate(t *testing.T) {
	for name, notice := range map[string]func(string) Event{EventAvailable: monitorOnly, EventNoUpdate: gated} {
		t.Run(name, func(t *testing.T) {
			m, recs := recorded(t,
				config.NotifierConfig{Type: "discord", URL: "http://x", Events: []string{name, EventUpdateSuccess}},
				config.NotifierConfig{Type: "discord", URL: "http://x", Events: []string{"*"}},
			)
			send1(m, notice("sha256:bbb"))
			send1(m, notice("sha256:bbb"))
			send1(m, notice("sha256:bbb"))
			for i, r := range recs {
				if n := countType(r, name); n != 1 {
					t.Fatalf("notifier %d: identical notice delivered %d times, want 1", i, n)
				}
			}

			send1(m, notice("sha256:ccc"))
			reason := notice("sha256:ccc")
			reason.Reason = "another gate"
			send1(m, reason)
			other := notice("sha256:ccc")
			other.Container = "db"
			send1(m, other)
			if n := countType(recs[0], name); n != 3 {
				t.Fatalf("notices delivered %d times, want 3: a new candidate and another container, not another reason", n)
			}

			send1(m, Event{Type: EventUpdateSuccess, Host: "local", Container: "web"})
			send1(m, reason)
			if n := countType(recs[0], name); n != 4 {
				t.Fatalf("notice after an update delivered %d times in total, want 4", n)
			}
		})
	}
}

func TestDeniedApprovalsAreAnnouncedOnce(t *testing.T) {
	m, recs := recorded(t,
		config.NotifierConfig{Type: "discord", URL: "http://x"},
		config.NotifierConfig{Type: "discord", URL: "http://x", Events: []string{EventDenied}},
	)
	denied := Event{
		Type: EventDenied, Host: "local", Container: "web", Image: "nginx:latest",
		OldDigest: "sha256:aaa", NewDigest: "sha256:bbb",
		Reason: "approval transport error from https://approver.example: connection refused",
	}
	for i := 0; i < 3; i++ {
		send1(m, denied)
		send1(m, digest())
	}
	if got := recs[0].types(); len(got) != 0 {
		t.Fatalf("default notifier got %v; update.denied is not a default event", got)
	}
	if got := recs[1].types(); strings.Join(got, ",") != EventDenied {
		t.Fatalf("subscriber got %v, want the denial once", got)
	}
	denied.Reason = "denied by https://approver.example (HTTP 403): change freeze"
	send1(m, denied)
	if n := countType(recs[1], EventDenied); n != 2 {
		t.Fatalf("a denial with another reason delivered %d times in total, want 2", n)
	}
}

func TestSkippedWithoutCandidateIsAnnouncedOnce(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "slack", URL: "http://x", Events: []string{EventNoUpdate}})
	ev := Event{Type: EventNoUpdate, Host: "local", Container: "web", Image: "nginx:latest", OldTag: "latest", Reason: "started with --rm"}
	send1(m, ev)
	send1(m, ev)
	if n := len(recs[0].types()); n != 1 {
		t.Fatalf("delivered %d times, want once", n)
	}
	ev.Image = "nginx:1.27"
	send1(m, ev)
	if n := len(recs[0].types()); n != 2 {
		t.Fatalf("delivered %d times after the image changed, want 2", n)
	}
}

func TestCheckFailedNotices(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{EventCheckFailed}})
	incomplete := Event{Type: EventCheckFailed, Host: "local", Container: "web", Image: "app:1.2", Reason: "no other tag has this tag's form"}
	failed := Event{Type: EventCheckFailed, Host: "local", Container: "db", Image: "db:16", Err: errors.New("registry unreachable")}
	for i := 0; i < 3; i++ {
		send1(m, incomplete)
		send1(m, failed)
	}
	r := recs[0]
	count := func(container string) int {
		r.mu.Lock()
		defer r.mu.Unlock()
		n := 0
		for _, ev := range r.got {
			if ev.Container == container {
				n++
			}
		}
		return n
	}
	if n := count("web"); n != 1 {
		t.Fatalf("an incomplete check delivered %d times, want once while it stays the same", n)
	}
	if n := count("db"); n != 3 {
		t.Fatalf("a failed check delivered %d times, want every time, like update.failure", n)
	}
}

func TestOnceIsConfigurable(t *testing.T) {
	yes, no := true, false
	once := EventRunCompleted
	line := RunItem{Host: "local", Container: "web", Result: "monitor-only", NewDigest: "sha256:b"}
	cases := []struct {
		name         string
		once         *bool
		wantDefault  string
		wantExplicit int
	}{
		{"unset means once", nil, once, 1},
		{"once: true", &yes, once, 1},
		{"once: false repeats", &no, strings.Join([]string{once, once, once}, ","), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, recs := recorded(t,
				config.NotifierConfig{Type: "webhook", URL: "http://x", Once: tc.once},
				config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{EventAvailable}, Once: tc.once},
				config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{"*"}, Once: tc.once},
			)
			for i := 0; i < 3; i++ {
				send1(m, monitorOnly("sha256:bbb"))
				send1(m, digest(line))
				send1(m, digest())
			}
			if got := strings.Join(recs[0].types(), ","); got != tc.wantDefault {
				t.Errorf("default subscriber got %s, want %s", got, tc.wantDefault)
			}
			if n := len(recs[1].types()); n != tc.wantExplicit {
				t.Errorf("explicit subscriber got %d notices, want %d", n, tc.wantExplicit)
			}
			if n := countType(recs[2], EventAvailable); n != tc.wantExplicit {
				t.Errorf("* subscriber got %d notices, want %d", n, tc.wantExplicit)
			}
			if n := countType(recs[2], EventRunCompleted); n != 6 {
				t.Errorf("* subscriber got %d of 6 run.completed, want every one", n)
			}
		})
	}
}

func TestFailedDeliveryIsRetried(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "ntfy", URL: "http://x", Events: []string{EventAvailable}})
	sink := &fakeSink{}
	m.AttachFailureSink(sink)
	r := recs[0]
	r.fail.Store(true)
	send1(m, monitorOnly("sha256:bbb"))
	r.fail.Store(false)
	send1(m, monitorOnly("sha256:bbb"))
	send1(m, monitorOnly("sha256:bbb"))
	if n := len(r.types()); n != 2 {
		t.Fatalf("attempts = %d, want 2 (failed, retried)", n)
	}
	if atomic.LoadInt32(&sink.n) != 1 {
		t.Fatalf("failure sink = %d, want 1", sink.n)
	}

	q, qrecs := recorded(t, config.NotifierConfig{Type: "ntfy", URL: "http://x"})
	qrecs[0].fail.Store(true)
	line := RunItem{Host: "local", Container: "web", Result: "dry-run", NewTag: "2"}
	send1(q, digest(line))
	qrecs[0].fail.Store(false)
	send1(q, digest(line))
	send1(q, digest(line))
	if n := len(qrecs[0].types()); n != 2 {
		t.Fatalf("digest attempts = %d, want 2", n)
	}
}

type panicky struct{}

func (panicky) Notify(context.Context, Event) error { panic("boom") }

func TestPanickingNotifierIsContained(t *testing.T) {
	m, _ := recorded(t, config.NotifierConfig{Type: "webhook", Name: "bad", URL: "http://x", Events: []string{EventAvailable}})
	m.items[0].n = panicky{}
	sink := &fakeSink{}
	m.AttachFailureSink(sink)
	send1(m, monitorOnly("sha256:bbb"))
	if atomic.LoadInt32(&sink.n) != 1 {
		t.Fatal("a panic must count as a failed delivery")
	}
	r := &recorder{}
	m.items[0].n = r
	send1(m, monitorOnly("sha256:bbb"))
	if len(r.types()) != 1 {
		t.Fatal("notice lost after a panicking delivery")
	}
}

func TestInstanceIsStamped(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{"*"}})
	m.SetInstance("nas")
	send1(m, Event{Type: EventRunStarted})
	send1(m, Event{Type: EventRunStarted, Instance: "explicit"})
	got := recs[0].got
	if got[0].Instance != "nas" || got[1].Instance != "explicit" {
		t.Fatalf("instances = %q, %q", got[0].Instance, got[1].Instance)
	}
	if got[0].At.IsZero() {
		t.Fatal("At not set")
	}
}

type countingLedger struct{ marks, commits, unmarks, forgets atomic.Int32 }

func (l *countingLedger) Mark(NoticeKey, string) bool { l.marks.Add(1); return true }
func (l *countingLedger) Commit(NoticeKey, string)    { l.commits.Add(1) }
func (l *countingLedger) Unmark(NoticeKey, string)    { l.unmarks.Add(1) }
func (l *countingLedger) Forget(string, string)       { l.forgets.Add(1) }

func TestAttachLedger(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{EventAvailable, EventUpdateSuccess}})
	l := &countingLedger{}
	m.AttachLedger(l)
	send1(m, monitorOnly("sha256:bbb"))
	send1(m, monitorOnly("sha256:bbb"))
	send1(m, Event{Type: EventUpdateSuccess, Host: "local", Container: "web"})
	if len(recs[0].types()) != 3 || l.marks.Load() != 2 || l.commits.Load() != 2 || l.forgets.Load() != 1 {
		t.Fatalf("attached ledger not used: delivered %v, marks %d, commits %d, forgets %d",
			recs[0].types(), l.marks.Load(), l.commits.Load(), l.forgets.Load())
	}

	recs[0].fail.Store(true)
	send1(m, monitorOnly("sha256:ccc"))
	recs[0].fail.Store(false)
	if l.commits.Load() != 2 || l.unmarks.Load() != 1 {
		t.Fatalf("after a failure: commits %d (want 2), unmarks %d (want 1)", l.commits.Load(), l.unmarks.Load())
	}
	release := make(chan struct{})
	m.items[0].n = held{release}
	m.Notify(context.Background(), monitorOnly("sha256:ddd"))
	if l.marks.Load() != 4 || l.commits.Load() != 2 {
		t.Fatalf("in-flight delivery: marks %d (want 4), commits %d (want 2)", l.marks.Load(), l.commits.Load())
	}
	close(release)
	m.Wait(3 * time.Second)
	if l.commits.Load() != 3 {
		t.Fatalf("delivered notice not committed: commits %d", l.commits.Load())
	}
	m.items[0].n = recs[0]
	m.AttachLedger(nil)
	recs[0].reset()
	send1(m, monitorOnly("sha256:bbb"))
	send1(m, monitorOnly("sha256:bbb"))
	if len(recs[0].types()) != 1 {
		t.Fatalf("restored ledger does not deduplicate: %v", recs[0].types())
	}
}

func TestNilMultiIsSafe(t *testing.T) {
	var m *Multi
	m.Notify(context.Background(), Event{Type: EventRunStarted})
	m.SetInstance("x")
	m.AttachLedger(nil)
	m.AttachFailureSink(nil)
	m.Wait(time.Millisecond)
	if m.Test(context.Background(), Event{}) != nil {
		t.Fatal("nil Multi must test nothing")
	}
}

func TestMultiTest(t *testing.T) {
	m, recs := recorded(t,
		config.NotifierConfig{Type: "slack", Name: "ops", URL: "http://x", Events: []string{EventUpdateFailure}},
		config.NotifierConfig{Type: "email", Name: "mail"},
		config.NotifierConfig{Type: "matrix", Name: "chat", URL: "http://x"},
	)
	m.SetInstance("nas")
	sink := &fakeSink{}
	m.AttachFailureSink(sink)
	recs[1].fail.Store(true)
	m.items[2].n = panicky{}

	res := m.Test(context.Background(), Event{})
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Name != "ops" || res[0].Type != "slack" || res[0].Err != nil {
		t.Errorf("ops = %+v; event filters must not apply to a test", res[0])
	}
	if res[1].Name != "mail" || res[1].Err == nil {
		t.Errorf("mail = %+v, want its delivery error", res[1])
	}
	if res[2].Err == nil || !strings.Contains(res[2].Err.Error(), "panic") {
		t.Errorf("chat = %+v, want the panic as an error", res[2])
	}
	ev := recs[0].got[0]
	if ev.Type != EventTest || ev.Instance != "nas" || ev.At.IsZero() {
		t.Errorf("test event = %+v", ev)
	}
	if atomic.LoadInt32(&sink.n) != 0 {
		t.Error("a test must not count as a delivery failure")
	}

	m.Test(context.Background(), Event{})
	if len(recs[0].got) != 2 {
		t.Fatal("second test was suppressed")
	}

	res = m.Test(context.Background(), Event{}, "mail", "nope")
	if len(res) != 2 || res[0].Name != "mail" || res[1].Name != "nope" || !errors.Is(res[1].Err, ErrUnknownNotifier) {
		t.Fatalf("named test = %+v", res)
	}
	if len(recs[0].got) != 2 {
		t.Fatal("a notifier that was not named was tested")
	}
}

func TestMultiTestHonoursContext(t *testing.T) {
	m, _ := recorded(t, config.NotifierConfig{Type: "webhook", Name: "slow", URL: "http://x"})
	m.items[0].n = blocking{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := m.Test(ctx, Event{})
	if res[0].Err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("test ignored the context: %+v after %s", res, time.Since(start))
	}
}

type held struct{ release chan struct{} }

func (h held) Notify(context.Context, Event) error {
	<-h.release
	return nil
}

type blocking struct{}

func (blocking) Notify(ctx context.Context, _ Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestLedgerIDsAreUnique(t *testing.T) {
	m := Build([]config.NotifierConfig{
		{Type: "slack", URL: "http://x"},
		{Type: "slack", URL: "http://x"},
		{Type: "slack", Name: "ops", URL: "http://x"},
		{Type: "bogus"},
		{Type: "discord", Name: "ops", URL: "http://x"},
	}, discardLogger())
	var ids []string
	for _, w := range m.items {
		ids = append(ids, w.id)
	}
	if strings.Join(ids, ",") != "slack:slack,slack:slack#2,slack:ops,discord:ops" {
		t.Fatalf("ids = %v", ids)
	}
	if m.items[1].name != "slack" {
		t.Fatalf("the metric/log name must stay the configured name, got %q", m.items[1].name)
	}
}

func ledgerIDs(items ...[2]string) []string {
	ws := make([]wrapped, len(items))
	for i, it := range items {
		ws[i] = wrapped{typ: it[0], name: it[1]}
	}
	assignLedgerIDs(ws)
	ids := make([]string, len(ws))
	for i, w := range ws {
		ids[i] = w.id
	}
	return ids
}

func TestLedgerIDs(t *testing.T) {
	slack := func(name string) [2]string { return [2]string{"slack", name} }
	discord := func(name string) [2]string { return [2]string{"discord", name} }
	cases := []struct {
		name  string
		items [][2]string
		want  string
	}{
		{"none", nil, ""},
		{"named", [][2]string{slack("ops"), {"email", "mail"}}, "slack:ops,email:mail"},
		{"one name, several notifiers", [][2]string{slack("slack"), slack("slack"), slack("slack")}, "slack:slack,slack:slack#2,slack:slack#3"},
		{"one name, two types", [][2]string{slack("ops"), discord("ops"), slack("ops")}, "slack:ops,discord:ops,slack:ops#2"},
		{"configured name looks numbered", [][2]string{slack("slack"), slack("slack"), slack("slack#2")}, "slack:slack,slack:slack#2,slack:slack%232"},
		{"escape character", [][2]string{slack("a%23"), slack("a#")}, "slack:a%2523,slack:a%23"},
	}
	for _, tc := range cases {
		if got := strings.Join(ledgerIDs(tc.items...), ","); got != tc.want {
			t.Errorf("%s: ids = %s, want %s", tc.name, got, tc.want)
		}
	}

	want := []string{"slack:slack", "slack:slack#2"}
	for _, layout := range [][][2]string{
		{slack("slack"), slack("slack")},
		{slack("ops"), slack("slack"), slack("slack")},
		{slack("slack"), discord("slack"), slack("slack"), slack("slack#2")},
		{slack("slack"), slack("slack"), discord("discord"), slack("slack")},
	} {
		var got []string
		for i, id := range ledgerIDs(layout...) {
			if layout[i] == slack("slack") && len(got) < 2 {
				got = append(got, id)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("layout %v keys the two unnamed slack notifiers %v, want %v", layout, got, want)
		}
	}

	names := []string{"a", "a#2", "a%232", "a#", "%"}
	var pairs [][2]string
	for _, typ := range []string{"slack", "discord"} {
		for _, n := range names {
			pairs = append(pairs, [2]string{typ, n})
		}
	}
	var layout [4][2]string
	var walk func(depth int)
	walk = func(depth int) {
		if depth == len(layout) {
			seen := map[string]bool{}
			for _, id := range ledgerIDs(layout[:]...) {
				if seen[id] {
					t.Fatalf("layout %v: key %s is not unique", layout, id)
				}
				seen[id] = true
			}
			return
		}
		for _, p := range pairs {
			layout[depth] = p
			walk(depth + 1)
		}
	}
	walk(0)
}

func TestNewChannelTypesAreBuilt(t *testing.T) {
	m := Build([]config.NotifierConfig{
		{Type: "pushover", Token: "t", User: "u"},
		{Type: "matrix", URL: "https://matrix.example", Token: "t", Room: "!r:example"},
		{Type: "apprise", URL: "https://apprise.example"},
	}, discardLogger())
	if len(m.items) != 3 {
		t.Fatalf("built %d of 3 notifiers", len(m.items))
	}
	for i, want := range []string{"*notify.pushover", "*notify.matrix", "*notify.apprise"} {
		if got := fmt.Sprintf("%T", m.items[i].n); got != want {
			t.Errorf("item %d is %s, want %s", i, got, want)
		}
	}
}

func schemaEnum(t *testing.T, key string) []string {
	t.Helper()
	raw, err := config.JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	prop, ok := schema.Definitions["NotifierConfig"].Properties[key]
	if !ok {
		t.Fatalf("the schema has no NotifierConfig.%s", key)
	}
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, sub := range v {
				if k != "enum" {
					walk(sub)
					continue
				}
				list, _ := sub.([]any)
				for _, e := range list {
					if s, ok := e.(string); ok {
						seen[s] = true
					}
				}
			}
		case []any:
			for _, sub := range v {
				walk(sub)
			}
		}
	}
	walk(prop)
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestVocabularyMatchesTheConfiguration(t *testing.T) {
	events := slices.DeleteFunc(schemaEnum(t, "events"), func(e string) bool { return e == "*" })
	known := slices.Sorted(slices.Values(knownEvents))
	if !slices.Equal(events, known) {
		t.Errorf("config accepts the events %v, notify knows %v", events, known)
	}

	types := schemaEnum(t, "type")
	if len(types) == 0 {
		t.Fatal("the schema lists no notifier types")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for _, typ := range types {
		if m := Build([]config.NotifierConfig{{Type: typ}}, logger); len(m.items) != 1 {
			t.Errorf("config accepts notifier type %q, but Build does not build it: %s", typ, logs.String())
		}
	}
}

func TestMemoryLedger(t *testing.T) {
	l := NewMemoryLedger()
	k := NoticeKey{Notifier: "ops", Event: EventNoUpdate, Host: "h", Container: "c"}
	if !l.Mark(k, "a") || l.Mark(k, "a") {
		t.Fatal("first mark must be fresh, the repeat not")
	}
	if !l.Mark(k, "b") {
		t.Fatal("a new fingerprint is fresh")
	}
	l.Unmark(k, "a")
	if l.Mark(k, "b") {
		t.Fatal("a stale Unmark removed the current notice")
	}
	l.Unmark(k, "b")
	if !l.Mark(k, "b") {
		t.Fatal("Unmark did not revert the notice")
	}
	other := k
	other.Notifier = "mail"
	if !l.Mark(other, "b") {
		t.Fatal("notifiers must not share notices")
	}
	l.Forget("h", "c")
	if !l.Mark(k, "b") || !l.Mark(other, "b") {
		t.Fatal("Forget must drop every notifier's notice for the container")
	}
}

func TestMemoryLedgerIsBounded(t *testing.T) {
	l := &memoryLedger{subjects: map[subject]*subjectNotices{}, max: 2}
	key := func(c string) NoticeKey {
		return NoticeKey{Notifier: "n", Event: EventNoUpdate, Host: "h", Container: c}
	}
	l.Mark(key("a"), "1")
	l.Mark(key("b"), "1")
	l.Mark(key("a"), "1")
	l.Mark(key("c"), "1")
	if len(l.subjects) != 2 {
		t.Fatalf("ledger holds %d containers, want 2", len(l.subjects))
	}
	if l.Mark(key("a"), "1") {
		t.Fatal("the recently used entry was evicted")
	}
	if !l.Mark(key("b"), "1") {
		t.Fatal("the least recently used entry was kept")
	}
}

func TestLedgerConcurrency(t *testing.T) {
	m, recs := recorded(t, config.NotifierConfig{Type: "webhook", URL: "http://x", Events: []string{"*"}})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Notify(context.Background(), monitorOnly("sha256:bbb"))
			m.Notify(context.Background(), digest(RunItem{Host: "local", Container: "web", Result: "dry-run"}))
		}()
	}
	wg.Wait()
	m.Wait(3 * time.Second)
	n := 0
	for _, ty := range recs[0].types() {
		if ty == EventAvailable {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent identical notices delivered %d times, want 1", n)
	}
}
