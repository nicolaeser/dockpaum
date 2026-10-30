package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type Event struct {
	Type       string
	Host       string
	Container  string
	Image      string
	OldDigest  string
	NewDigest  string
	OldTag     string
	NewTag     string
	Policy     string
	Reason     string
	Err        error
	At         time.Time
	RunStats   *RunStats
	RunID      string
	Instance   string
	ReleaseURL string
	Startup    *StartupInfo
}

type StartupInfo struct {
	Version  string    `json:"version,omitempty"`
	Hosts    []string  `json:"hosts,omitempty"`
	Schedule string    `json:"schedule,omitempty"`
	NextRun  time.Time `json:"next_run,omitzero"`
	DryRun   bool      `json:"dry_run"`
}

type RunStats struct {
	Scanned int
	Updated int
	Failed  int
	Skipped int
	Took    time.Duration
	Items   []RunItem `json:"items,omitempty"`
}

type RunItem struct {
	Host       string `json:"host"`
	Container  string `json:"container"`
	Image      string `json:"image,omitempty"`
	OldTag     string `json:"old_tag,omitempty"`
	NewTag     string `json:"new_tag,omitempty"`
	OldDigest  string `json:"old_digest,omitempty"`
	NewDigest  string `json:"new_digest,omitempty"`
	Result     string `json:"result"`
	Error      string `json:"error,omitempty"`
	ReleaseURL string `json:"release_url,omitempty"`
}

func (i RunItem) Line() string {
	s := i.Container
	if i.Host != "" {
		s = i.Host + "/" + s
	}
	switch {
	case i.OldTag != "" && i.NewTag != "" && i.OldTag != i.NewTag:
		s += ": " + i.OldTag + " → " + i.NewTag
	default:
		if i.Image != "" {
			s += ": " + i.Image
		}
		if d := digestChange(i.OldDigest, i.NewDigest); d != "" {
			s += " @ " + d
		}
	}
	return s + " (" + i.Result + ")"
}

const (
	EventUpdateSuccess     = "update.success"
	EventUpdateFailure     = "update.failure"
	EventAvailable         = "update.available"
	EventNoUpdate          = "update.skipped"
	EventDenied            = "update.denied"
	EventRolledBack        = "update.rolled_back"
	EventRecovered         = "update.recovered"
	EventCheckFailed       = "check.failed"
	EventSelfUpdateStarted = "self_update.started"
	EventSelfUpdateSuccess = "self_update.success"
	EventSelfUpdateFailure = "self_update.failure"
	EventRunStarted        = "run.started"
	EventRunCompleted      = "run.completed"
	EventStarted           = "dockpaum.started"
	EventStopping          = "dockpaum.stopping"
	EventTest              = "notify.test"
)

var knownEvents = []string{
	EventUpdateSuccess, EventUpdateFailure, EventAvailable, EventNoUpdate, EventDenied,
	EventRolledBack, EventRecovered, EventCheckFailed,
	EventSelfUpdateStarted, EventSelfUpdateSuccess, EventSelfUpdateFailure,
	EventRunStarted, EventRunCompleted, EventStarted, EventStopping, EventTest,
}

type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

func knownEvent(e string) bool {
	for _, k := range knownEvents {
		if e == k {
			return true
		}
	}
	return false
}

func defaultEvent(e string) bool {
	switch e {
	case EventUpdateSuccess, EventUpdateFailure, EventRunCompleted:
		return true
	}
	return false
}

func failureEvent(t string) bool {
	switch t {
	case EventUpdateFailure, EventDenied, EventRolledBack, EventCheckFailed, EventSelfUpdateFailure:
		return true
	}
	return false
}

type FailureSink interface {
	WithLabelValues(lvs ...string) interface{ Inc() }
}

const deliveryTimeout = 15 * time.Second

type Multi struct {
	items    []wrapped
	logger   *slog.Logger
	failures FailureSink
	wg       sync.WaitGroup

	mu       sync.RWMutex
	ledger   Ledger
	instance string
}

type wrapped struct {
	n             Notifier
	name, id, typ string
	sub           subscription
}

type subscription struct {
	all    bool
	events map[string]struct{}
	repeat bool
}

func (s subscription) explicit() bool { return s.all || s.events != nil }

func (s subscription) wants(t string) bool {
	switch {
	case s.all:
		return true
	case s.events != nil:
		_, ok := s.events[t]
		return ok
	}
	return defaultEvent(t)
}

func Build(cfgs []config.NotifierConfig, logger *slog.Logger) *Multi {
	m := &Multi{logger: logger, ledger: NewMemoryLedger()}
	for _, c := range cfgs {
		var n Notifier
		switch c.Type {
		case "webhook":
			n = newWebhook(c)
		case "slack":
			n = newSlack(c)
		case "discord":
			n = newDiscord(c)
		case "email":
			n = newEmail(c)
		case "telegram":
			n = newTelegram(c)
		case "ntfy":
			n = newNtfy(c)
		case "gotify":
			n = newGotify(c)
		case "teams":
			n = newTeams(c)
		case "pushover":
			n = newPushover(c)
		case "matrix":
			n = newMatrix(c)
		case "apprise":
			n = newApprise(c)
		default:
			logger.Warn("unknown notifier type", "type", c.Type)
			continue
		}
		var sub subscription
		if len(c.Events) > 0 {
			sub.events = map[string]struct{}{}
		}
		for _, e := range c.Events {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "*" {
				sub = subscription{all: true}
				break
			}
			if !knownEvent(e) {
				logger.Warn("unknown notify event will never fire", "event", e, "known", knownEvents)
			}
			sub.events[e] = struct{}{}
		}
		sub.repeat = c.Once != nil && !*c.Once
		name := c.Name
		if name == "" {
			name = c.Type
		}
		m.items = append(m.items, wrapped{n: n, name: name, typ: c.Type, sub: sub})
	}
	assignLedgerIDs(m.items)
	return m
}

func assignLedgerIDs(items []wrapped) {
	nth := map[[2]string]int{}
	for i := range items {
		w := &items[i]
		k := [2]string{w.typ, w.name}
		nth[k]++
		w.id = ledgerID(w.typ, w.name, nth[k])
	}
}

var ledgerName = strings.NewReplacer("%", "%25", "#", "%23")

func ledgerID(typ, name string, nth int) string {
	id := typ + ":" + ledgerName.Replace(name)
	if nth > 1 {
		id += "#" + strconv.Itoa(nth)
	}
	return id
}

func (m *Multi) AttachFailureSink(s FailureSink) {
	if m == nil {
		return
	}
	m.failures = s
}

func (m *Multi) AttachLedger(l Ledger) {
	if m == nil {
		return
	}
	if l == nil {
		l = NewMemoryLedger()
	}
	m.mu.Lock()
	m.ledger = l
	m.mu.Unlock()
}

func (m *Multi) SetInstance(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.instance = name
	m.mu.Unlock()
}

func (m *Multi) settings() (Ledger, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ledger, m.instance
}

func (m *Multi) Notify(ctx context.Context, ev Event) {
	if m == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	ledger, instance := m.settings()
	if ev.Instance == "" {
		ev.Instance = instance
	}
	if ev.Type == EventUpdateSuccess && ev.Container != "" {
		ledger.Forget(ev.Host, ev.Container)
	}
	for i := range m.items {
		w := m.items[i]
		if !w.sub.wants(ev.Type) {
			continue
		}
		marked, ok := admit(ledger, w, ev)
		if !ok {
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					marked.undo()
					m.logger.Error("notifier panic", "name", w.name, "panic", r)
					m.countFailure(w.name)
				}
			}()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
			defer cancel()
			if err := w.n.Notify(cctx, ev); err != nil {
				marked.undo()
				m.logger.Warn("notifier delivery failed", "name", w.name, "event", ev.Type, "err", err)
				m.countFailure(w.name)
				return
			}
			marked.commit()
		}()
	}
}

func (m *Multi) countFailure(name string) {
	if m.failures != nil {
		m.failures.WithLabelValues(name).Inc()
	}
}

func admit(l Ledger, w wrapped, ev Event) (claim, bool) {
	c := claim{ledger: l}
	switch {
	case standingNotice(ev):
		if w.sub.repeat {
			break
		}
		k := NoticeKey{Notifier: w.id, Event: ev.Type, Host: ev.Host, Container: ev.Container}
		fp := noticeFingerprint(ev)
		if !l.Mark(k, fp) {
			return claim{}, false
		}
		c.marks = append(c.marks, mark{k, fp})
	case ev.Type == EventRunCompleted && !w.sub.explicit():
		happened := ev.Err != nil
		if s := ev.RunStats; s != nil {
			happened = happened || s.Updated > 0 || s.Failed > 0
			for _, it := range s.Items {
				if !standing(it.Result) || w.sub.repeat || !hasCandidate(it.OldTag, it.NewTag, it.NewDigest) {
					happened = true
					continue
				}
				k := NoticeKey{Notifier: w.id, Event: ev.Type, Host: it.Host, Container: it.Container}
				fp := itemFingerprint(it)
				if l.Mark(k, fp) {
					happened = true
					c.marks = append(c.marks, mark{k, fp})
				}
			}
		}
		if !happened {
			return claim{}, false
		}
	}
	return c, true
}

type claim struct {
	ledger Ledger
	marks  []mark
}

type mark struct {
	key NoticeKey
	fp  string
}

func (c claim) commit() {
	for _, mk := range c.marks {
		c.ledger.Commit(mk.key, mk.fp)
	}
}

func (c claim) undo() {
	for _, mk := range c.marks {
		c.ledger.Unmark(mk.key, mk.fp)
	}
}

func (m *Multi) Wait(timeout time.Duration) {
	if m == nil {
		return
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		m.logger.Warn("notify: timed out flushing in-flight notifications", "timeout", timeout)
	}
}

var ErrUnknownNotifier = errors.New("no notifier with this name is configured")

type TestResult struct {
	Name string
	Type string
	Err  error
}

func (m *Multi) Test(ctx context.Context, ev Event, names ...string) []TestResult {
	if m == nil {
		return nil
	}
	if ev.Type == "" {
		ev.Type = EventTest
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if _, instance := m.settings(); ev.Instance == "" {
		ev.Instance = instance
	}
	var targets []wrapped
	var unknown []string
	if len(names) == 0 {
		targets = m.items
	} else {
		picked := make([]bool, len(m.items))
		for _, name := range names {
			found := false
			for i, w := range m.items {
				if w.name == name {
					picked[i], found = true, true
				}
			}
			if !found {
				unknown = append(unknown, name)
			}
		}
		for i, w := range m.items {
			if picked[i] {
				targets = append(targets, w)
			}
		}
	}

	results := make([]TestResult, len(targets), len(targets)+len(unknown))
	var wg sync.WaitGroup
	for i, w := range targets {
		results[i] = TestResult{Name: w.name, Type: w.typ}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i].Err = fmt.Errorf("notifier panic: %v", r)
				}
			}()
			cctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
			defer cancel()
			results[i].Err = w.n.Notify(cctx, ev)
		}()
	}
	wg.Wait()
	for _, name := range unknown {
		results = append(results, TestResult{Name: name, Err: ErrUnknownNotifier})
	}
	return results
}

func formatTitle(ev Event) string {
	c := ev.Container
	if c == "" {
		c = "container"
	}
	switch ev.Type {
	case EventUpdateSuccess:
		return "✅ dockpaum: " + c + " updated"
	case EventUpdateFailure:
		return "🔥 dockpaum: " + c + " update failed"
	case EventRunStarted:
		return "▶️ dockpaum: run started"
	case EventRunCompleted:
		if ev.Err != nil {
			return "🏁 dockpaum: run failed"
		}
		return "🏁 dockpaum: run completed"
	case EventAvailable:
		return "🆕 dockpaum: " + c + " update available"
	case EventNoUpdate:
		return "⏭ dockpaum: " + c + " skipped"
	case EventDenied:
		return "⛔ dockpaum: " + c + " update denied"
	case EventRolledBack:
		return "↩️ dockpaum: " + c + " rolled back"
	case EventRecovered:
		return "🩹 dockpaum: " + c + " recovered"
	case EventCheckFailed:
		return "⚠️ dockpaum: " + c + " check failed"
	case EventSelfUpdateStarted:
		return "🔄 dockpaum: self-update started"
	case EventSelfUpdateSuccess:
		return "✅ dockpaum: self-update completed"
	case EventSelfUpdateFailure:
		return "🔥 dockpaum: self-update failed"
	case EventStarted:
		return "🚀 dockpaum: started"
	case EventStopping:
		return "🛑 dockpaum: stopping"
	case EventTest:
		return "🔔 dockpaum: test notification"
	}
	return "dockpaum: " + ev.Type
}

func formatBody(ev Event) string { return plainBody(ev, true) }

func short(s string) string {
	const n = 12
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
