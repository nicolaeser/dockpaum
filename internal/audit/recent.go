package audit

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

func (d Decision) Updated() bool {
	return d == DecisionUpdated || d == DecisionSelfUpdateCompleted
}

func (d Decision) Failed() bool {
	switch d {
	case DecisionFailed, DecisionRolledBack, DecisionCheckFailed, DecisionSelfUpdateFailed:
		return true
	}
	return false
}

func (d Decision) Gated() bool {
	return strings.HasPrefix(string(d), "gated-") || d == DecisionApprovalDeny
}

const RecentWindow = 24 * time.Hour

func (l *Logger) Recent(since time.Time) map[Decision]int {
	out := map[Decision]int{}
	if l == nil {
		return out
	}
	now := l.clock()
	if floor := now.Add(-RecentWindow); since.Before(floor) {
		since = floor
	}
	from := minuteOf(since)
	l.recentMu.Lock()
	defer l.recentMu.Unlock()
	l.pruneRecentLocked(now)
	for m, counts := range l.recent {
		if m < from {
			continue
		}
		for d, n := range counts {
			out[d] += n
		}
	}
	return out
}

func (l *Logger) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

func (l *Logger) countRecent(at time.Time, d Decision) {
	if d == "" {
		return
	}
	now := l.clock()
	if at.Before(now.Add(-RecentWindow)) {
		return
	}
	l.recentMu.Lock()
	defer l.recentMu.Unlock()
	if l.recent == nil {
		l.recent = map[int64]map[Decision]int{}
	}
	m := minuteOf(at)
	if l.recent[m] == nil {
		l.recent[m] = map[Decision]int{}
	}
	l.recent[m][d]++
	l.pruneRecentLocked(now)
}

func (l *Logger) pruneRecentLocked(now time.Time) {
	oldest := minuteOf(now.Add(-RecentWindow))
	if oldest == l.recentFloor {
		return
	}
	l.recentFloor = oldest
	for m := range l.recent {
		if m < oldest {
			delete(l.recent, m)
		}
	}
}

func (l *Logger) seedRecent() {
	since := l.clock().Add(-RecentWindow)
	files, err := monthFiles(l.cfg.Dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if !f.end.After(since) {
			continue
		}
		fh, err := os.Open(f.path)
		if err != nil {
			l.logger.Debug("audit: recent decisions not read", "file", f.path, "err", err)
			continue
		}
		_, err = scanLines(fh, func(line []byte) {
			var rec struct {
				At       time.Time `json:"at"`
				Decision Decision  `json:"decision"`
			}
			if json.Unmarshal(line, &rec) != nil || rec.At.Before(since) {
				return
			}
			l.countRecent(rec.At, rec.Decision)
		})
		_ = fh.Close()
		if err != nil {
			l.logger.Debug("audit: recent decisions not read", "file", f.path, "err", err)
		}
	}
}
