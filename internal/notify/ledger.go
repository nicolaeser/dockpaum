package notify

import (
	"strings"
	"sync"
)

type NoticeKey struct {
	Notifier  string
	Event     string
	Host      string
	Container string
}

type Ledger interface {
	Mark(key NoticeKey, fingerprint string) (fresh bool)
	Commit(key NoticeKey, fingerprint string)
	Unmark(key NoticeKey, fingerprint string)
	Forget(host, container string)
}

const memoryLedgerSize = 4096

func NewMemoryLedger() Ledger {
	return &memoryLedger{subjects: map[subject]*subjectNotices{}, max: memoryLedgerSize}
}

type memoryLedger struct {
	mu       sync.Mutex
	subjects map[subject]*subjectNotices
	max      int
	tick     uint64
}

type subject struct{ host, container string }

type slot struct{ notifier, event string }

type subjectNotices struct {
	notices map[slot]string
	used    uint64
}

func (l *memoryLedger) Mark(k NoticeKey, fp string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tick++
	s := subject{k.Host, k.Container}
	e := l.subjects[s]
	if e == nil {
		if len(l.subjects) >= l.max {
			l.evictLocked()
		}
		e = &subjectNotices{notices: map[slot]string{}}
		l.subjects[s] = e
	}
	e.used = l.tick
	sl := slot{k.Notifier, k.Event}
	if prev, ok := e.notices[sl]; ok && prev == fp {
		return false
	}
	e.notices[sl] = fp
	return true
}

func (l *memoryLedger) Commit(NoticeKey, string) {}

func (l *memoryLedger) Unmark(k NoticeKey, fp string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := subject{k.Host, k.Container}
	e := l.subjects[s]
	if e == nil {
		return
	}
	sl := slot{k.Notifier, k.Event}
	if e.notices[sl] == fp {
		delete(e.notices, sl)
	}
	if len(e.notices) == 0 {
		delete(l.subjects, s)
	}
}

func (l *memoryLedger) Forget(host, container string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subjects, subject{host, container})
}

func (l *memoryLedger) evictLocked() {
	var oldest subject
	var oldestUse uint64
	first := true
	for s, e := range l.subjects {
		if first || e.used < oldestUse {
			oldest, oldestUse, first = s, e.used, false
		}
	}
	delete(l.subjects, oldest)
}

func standing(result string) bool {
	switch result {
	case "monitor-only", "dry-run", "approval-denied":
		return true
	}
	return strings.HasPrefix(result, "gated-")
}

func hasCandidate(oldTag, newTag, newDigest string) bool {
	return newDigest != "" || (newTag != "" && newTag != oldTag)
}

func standingNotice(ev Event) bool {
	switch ev.Type {
	case EventAvailable, EventNoUpdate:
		return true
	case EventDenied:
		return hasCandidate(ev.OldTag, ev.NewTag, ev.NewDigest)
	case EventCheckFailed:
		return ev.Err == nil
	}
	return false
}

func noticeFingerprint(ev Event) string {
	switch ev.Type {
	case EventAvailable, EventNoUpdate:
		return strings.Join([]string{ev.Image, ev.NewTag, ev.NewDigest}, "\x1f")
	}
	return eventFingerprint(ev)
}

func eventFingerprint(ev Event) string {
	errText := ""
	if ev.Err != nil {
		errText = ev.Err.Error()
	}
	return strings.Join([]string{ev.Image, ev.NewTag, ev.NewDigest, ev.Reason, errText}, "\x1f")
}

func itemFingerprint(it RunItem) string {
	return strings.Join([]string{it.Result, it.Image, it.NewTag, it.NewDigest, it.Error}, "\x1f")
}
