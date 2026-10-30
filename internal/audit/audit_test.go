package audit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func boolp(b bool) *bool    { return &b }

func TestAppendWritesRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir, Retention: config.Duration(31 * 24 * time.Hour)}, discard())
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Record{Container: "c1", Image: "img:1", Decision: DecisionUpdated, OldTag: "1.0.0", NewTag: "1.0.1"})
	_ = l.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "dockpaum-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("expected 1 audit file, got %d", len(files))
	}
	data, _ := os.ReadFile(files[0])
	var rec Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Container != "c1" || rec.Decision != DecisionUpdated || rec.NewTag != "1.0.1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.At.IsZero() {
		t.Error("timestamp should be set automatically")
	}
}

func TestDisabledIsNoOp(t *testing.T) {
	dir := t.TempDir()
	l, err := New(config.AuditConfig{Enabled: boolp(false), Dir: dir}, discard())
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Record{Container: "c1", Decision: DecisionUpdated})
	files, _ := filepath.Glob(filepath.Join(dir, "dockpaum-*.jsonl"))
	if len(files) != 0 {
		t.Fatalf("disabled audit must not write files, got %d", len(files))
	}
}

func TestPruneOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "dockpaum-2000-01.jsonl")
	recent := filepath.Join(dir, "dockpaum-"+time.Now().UTC().Format("2006-01")+".jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recent, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir, Retention: config.Duration(31 * 24 * time.Hour)}, discard()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old audit file should have been pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent audit file should remain")
	}
}

func TestIsEnabled(t *testing.T) {
	cases := []struct {
		cfg  config.AuditConfig
		want bool
	}{
		{config.AuditConfig{Enabled: boolp(true)}, true},
		{config.AuditConfig{Enabled: boolp(false), Dir: "/x"}, false},
		{config.AuditConfig{Dir: "/x"}, true},
		{config.AuditConfig{}, false},
	}
	for i, c := range cases {
		if got := c.cfg.IsEnabled(); got != c.want {
			t.Errorf("case %d: IsEnabled()=%v want %v", i, got, c.want)
		}
	}
}

type countSink struct{ n int }

func (c *countSink) Inc() { c.n++ }

type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) messages(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

func (h *recordingHandler) attrs(level slog.Level) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.records {
		if r.Level != level {
			continue
		}
		m := map[string]string{"msg": r.Message}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.String()
			return true
		})
		out = append(out, m)
	}
	return out
}

func TestNewWarnsOnceWhenTheDirectoryCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &recordingHandler{}
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: filepath.Join(blocker, "audit")}, slog.New(h))
	if err != nil {
		t.Fatalf("an audit directory that cannot be created must not stop startup: %v", err)
	}
	warns := h.attrs(slog.LevelWarn)
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1", warns)
	}
	if w := warns[0]; !strings.Contains(w["msg"], "audit.enabled: false") || strings.Count(w["err"], blocker) != 1 {
		t.Errorf("warning = %v, want the fix and the path once", w)
	}
	sink := &countSink{}
	l.AttachFailureSink(sink)
	for range 3 {
		l.Append(Record{Container: "web", Decision: DecisionUpdated})
	}
	if sink.n != 4 {
		t.Errorf("failures counted = %d, want 4 (startup and every lost record)", sink.n)
	}
	if got := h.messages(slog.LevelError); len(got) != 0 {
		t.Errorf("errors logged = %q, want none after the startup warning", got)
	}
	if got := h.messages(slog.LevelWarn); len(got) != 1 {
		t.Errorf("warnings logged = %q, want only the startup one", got)
	}
}

func TestNewRejectsEnabledWithoutDirectory(t *testing.T) {
	if _, err := New(config.AuditConfig{Enabled: boolp(true)}, discard()); err == nil {
		t.Fatal("an enabled audit log without a directory must be rejected")
	}
}

func TestUnwritableAtStartupRecovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- a test directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- a test directory

	h := &recordingHandler{}
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir, Retention: config.Duration(31 * 24 * time.Hour)}, slog.New(h))
	if err != nil {
		t.Fatalf("an unwritable directory must not stop startup: %v", err)
	}
	if got := h.messages(slog.LevelWarn); len(got) != 1 {
		t.Fatalf("startup warnings = %q, want 1", got)
	}
	sink := &countSink{}
	l.AttachFailureSink(sink)
	if sink.n != 1 {
		t.Fatalf("failures counted at attach = %d, want 1", sink.n)
	}
	for range 5 {
		l.Append(Record{Container: "web", Decision: DecisionUpdated})
	}
	if sink.n != 6 {
		t.Errorf("failures counted = %d, want 6 (every lost record)", sink.n)
	}
	if got := h.messages(slog.LevelError); len(got) != 0 {
		t.Errorf("errors logged = %q, want none: the startup warning covers them", got)
	}

	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a test directory
		t.Fatal(err)
	}
	l.Append(Record{Container: "web", Decision: DecisionUpdated})
	l.Append(Record{Container: "api", Decision: DecisionSkipped})
	if got := h.messages(slog.LevelInfo); !slices.Contains(got, "audit writes recovered") {
		t.Errorf("info logs = %q, want a recovery message", got)
	}
	if sink.n != 6 {
		t.Errorf("failures counted after recovery = %d, want 6", sink.n)
	}
	_ = l.Close()
	res, err := Read(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 {
		t.Errorf("records after recovery = %d, want 2", len(res.Records))
	}
}

func TestWriteFailuresAreCountedAndRateLimited(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	h := &recordingHandler{}
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir, Retention: config.Duration(31 * 24 * time.Hour)}, slog.New(h))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	l.now = func() time.Time { return clock }
	sink := &countSink{}
	l.AttachFailureSink(sink)

	files, _ := filepath.Glob(filepath.Join(dir, "dockpaum-*.jsonl"))
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- a test directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- a test directory
	l.mu.Lock()
	_ = l.file.Close()
	l.mu.Unlock()

	for range 5 {
		l.Append(Record{At: clock, Container: "web", Decision: DecisionUpdated})
	}
	if sink.n != 5 {
		t.Errorf("failures counted = %d, want 5 (every lost record)", sink.n)
	}
	if got := h.messages(slog.LevelError); len(got) != 1 {
		t.Errorf("errors logged within the interval = %d, want 1 (rate limited)", len(got))
	}
	clock = clock.Add(failureLogInterval)
	l.Append(Record{At: clock, Container: "web", Decision: DecisionUpdated})
	if got := h.messages(slog.LevelError); len(got) != 2 {
		t.Errorf("errors logged after the interval = %d, want 2", len(got))
	}

	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a test directory
		t.Fatal(err)
	}
	l.Append(Record{At: clock, Container: "web", Decision: DecisionUpdated})
	if got := h.messages(slog.LevelInfo); !slices.Contains(got, "audit writes recovered") {
		t.Errorf("info logs = %q, want a recovery message", got)
	}
	_ = l.Close()
}

func TestAttachFailureSinkNil(t *testing.T) {
	var l *Logger
	l.AttachFailureSink(&countSink{})
	l2, _ := New(config.AuditConfig{Enabled: boolp(false)}, discard())
	l2.AttachFailureSink(nil)
	l2.Append(Record{Container: "x"})
}

func TestDecisionValues(t *testing.T) {
	for d, want := range map[Decision]string{
		DecisionUpdated:             "updated",
		DecisionSkipped:             "skipped",
		DecisionFailed:              "failed",
		DecisionDryRun:              "dry-run",
		DecisionApprovalDeny:        "approval-denied",
		DecisionGatedAge:            "gated-min-age",
		DecisionGatedPolicy:         "gated-policy",
		DecisionGatedDigest:         "gated-digest-pin",
		DecisionGatedWindow:         "gated-window",
		DecisionRolledBack:          "rolled-back",
		DecisionSelfUpdate:          "self-update",
		DecisionRecovered:           "recovered",
		DecisionCheckFailed:         "check-failed",
		DecisionCheckIncomplete:     "check-incomplete",
		DecisionMonitorOnly:         "monitor-only",
		DecisionGatedAutoRemove:     "gated-autoremove",
		DecisionGatedOrchestrated:   "gated-orchestrated",
		DecisionReattached:          "reattached",
		DecisionSelfUpdateCompleted: "self-update-completed",
		DecisionSelfUpdateFailed:    "self-update-failed",
	} {
		if string(d) != want {
			t.Errorf("decision %q, want %q", d, want)
		}
	}
}
