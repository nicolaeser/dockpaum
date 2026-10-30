package audit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestDecisionClasses(t *testing.T) {
	cases := []struct {
		d                      Decision
		updated, failed, gated bool
	}{
		{DecisionUpdated, true, false, false},
		{DecisionSelfUpdateCompleted, true, false, false},
		{DecisionFailed, false, true, false},
		{DecisionRolledBack, false, true, false},
		{DecisionCheckFailed, false, true, false},
		{DecisionSelfUpdateFailed, false, true, false},
		{DecisionGatedWindow, false, false, true},
		{DecisionGatedAge, false, false, true},
		{DecisionGatedPolicy, false, false, true},
		{DecisionGatedDigest, false, false, true},
		{DecisionGatedAutoRemove, false, false, true},
		{DecisionGatedOrchestrated, false, false, true},
		{DecisionApprovalDeny, false, false, true},
		{DecisionMonitorOnly, false, false, false},
		{DecisionSkipped, false, false, false},
		{DecisionCheckIncomplete, false, false, false},
		{DecisionDryRun, false, false, false},
		{DecisionSelfUpdate, false, false, false},
		{DecisionRecovered, false, false, false},
		{DecisionReattached, false, false, false},
	}
	for _, c := range cases {
		if c.d.Updated() != c.updated || c.d.Failed() != c.failed || c.d.Gated() != c.gated {
			t.Errorf("%s: Updated=%v Failed=%v Gated=%v, want %v %v %v",
				c.d, c.d.Updated(), c.d.Failed(), c.d.Gated(), c.updated, c.failed, c.gated)
		}
	}
}

func TestRecentCountsTheLastDay(t *testing.T) {
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: t.TempDir()}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	l.Append(Record{At: now.Add(-25 * time.Hour), Container: "old", Decision: DecisionUpdated})
	l.Append(Record{At: now.Add(-23 * time.Hour), Container: "a", Decision: DecisionUpdated})
	l.Append(Record{At: now.Add(-time.Hour), Container: "b", Decision: DecisionUpdated})
	l.Append(Record{At: now.Add(-time.Minute), Container: "c", Decision: DecisionFailed})
	l.Append(Record{Container: "d", Decision: DecisionSkipped})

	got := l.Recent(now.Add(-24 * time.Hour))
	if got[DecisionUpdated] != 2 || got[DecisionFailed] != 1 || got[DecisionSkipped] != 1 {
		t.Errorf("Recent(24h) = %v, want updated 2, failed 1, skipped 1", got)
	}
	if got := l.Recent(now.Add(-2 * time.Hour)); got[DecisionUpdated] != 1 {
		t.Errorf("Recent(2h) = %v, want updated 1", got)
	}
	if got := l.Recent(time.Time{}); got[DecisionUpdated] != 2 {
		t.Errorf("Recent(zero) = %v, want the last day only", got)
	}

	now = now.Add(24*time.Hour + time.Minute)
	if got := l.Recent(now.Add(-24 * time.Hour)); len(got) != 0 {
		t.Errorf("Recent a day later = %v, want nothing", got)
	}
	l.recentMu.Lock()
	buckets := len(l.recent)
	l.recentMu.Unlock()
	if buckets != 0 {
		t.Errorf("%d buckets kept after the window passed", buckets)
	}
}

func TestRecentSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeHistory(t, dir,
		Record{At: now.Add(-48 * time.Hour), Container: "old", Decision: DecisionFailed},
		Record{At: now.Add(-3 * time.Hour), Container: "web", Decision: DecisionUpdated},
		Record{At: now.Add(-2 * time.Hour), Container: "db", Decision: DecisionRolledBack},
	)
	files, _ := filepath.Glob(filepath.Join(dir, "dockpaum-*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no audit file written")
	}
	f, err := os.OpenFile(files[len(files)-1], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := l.Recent(now.Add(-24 * time.Hour))
	if got[DecisionUpdated] != 1 || got[DecisionRolledBack] != 1 || got[DecisionFailed] != 0 {
		t.Errorf("Recent after a restart = %v, want updated 1 and rolled-back 1", got)
	}
}

func TestRecentWithoutAuditLog(t *testing.T) {
	l, err := New(config.AuditConfig{Enabled: boolp(false)}, discard())
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Record{Container: "web", Decision: DecisionUpdated})
	if got := l.Recent(time.Now().Add(-time.Hour)); got[DecisionUpdated] != 1 {
		t.Errorf("Recent with audit disabled = %v, want updated 1", got)
	}
	var nilLogger *Logger
	if got := nilLogger.Recent(time.Now()); len(got) != 0 {
		t.Errorf("nil logger Recent = %v", got)
	}
	var zero Logger
	zero.Append(Record{Container: "web", Decision: DecisionFailed})
	if got := zero.Recent(time.Now().Add(-time.Hour)); got[DecisionFailed] != 1 {
		t.Errorf("zero logger Recent = %v", got)
	}
}

func TestRecentIsSafeForConcurrentUse(t *testing.T) {
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: t.TempDir()}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Append(Record{Container: "c", Decision: DecisionUpdated})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = l.Recent(time.Now().Add(-time.Hour))
			}
		}()
	}
	wg.Wait()
	if got := l.Recent(time.Now().Add(-time.Hour)); got[DecisionUpdated] != 400 {
		t.Errorf("updated = %d, want 400", got[DecisionUpdated])
	}
}
