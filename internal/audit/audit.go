package audit

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type Decision string

const (
	DecisionUpdated             Decision = "updated"
	DecisionSkipped             Decision = "skipped"
	DecisionFailed              Decision = "failed"
	DecisionDryRun              Decision = "dry-run"
	DecisionApprovalDeny        Decision = "approval-denied"
	DecisionGatedAge            Decision = "gated-min-age"
	DecisionGatedPolicy         Decision = "gated-policy"
	DecisionGatedDigest         Decision = "gated-digest-pin"
	DecisionGatedWindow         Decision = "gated-window"
	DecisionRolledBack          Decision = "rolled-back"
	DecisionSelfUpdate          Decision = "self-update"
	DecisionRecovered           Decision = "recovered"
	DecisionCheckFailed         Decision = "check-failed"
	DecisionCheckIncomplete     Decision = "check-incomplete"
	DecisionMonitorOnly         Decision = "monitor-only"
	DecisionGatedAutoRemove     Decision = "gated-autoremove"
	DecisionGatedOrchestrated   Decision = "gated-orchestrated"
	DecisionReattached          Decision = "reattached"
	DecisionSelfUpdateCompleted Decision = "self-update-completed"
	DecisionSelfUpdateFailed    Decision = "self-update-failed"
	DecisionGatedMACAddress     Decision = "gated-mac-address"
)

type Record struct {
	At          time.Time `json:"at"`
	RunID       string    `json:"run_id"`
	Host        string    `json:"host"`
	Container   string    `json:"container"`
	ContainerID string    `json:"container_id,omitempty"`
	Image       string    `json:"image"`
	OldDigest   string    `json:"old_digest,omitempty"`
	NewDigest   string    `json:"new_digest,omitempty"`
	OldTag      string    `json:"old_tag,omitempty"`
	NewTag      string    `json:"new_tag,omitempty"`
	Decision    Decision  `json:"decision"`
	Reason      string    `json:"reason,omitempty"`
	Policy      string    `json:"policy,omitempty"`
	Group       string    `json:"compose_group,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type FailureSink interface{ Inc() }

const failureLogInterval = time.Minute

type Logger struct {
	cfg      config.AuditConfig
	logger   *slog.Logger
	failures FailureSink
	now      func() time.Time
	mu       sync.Mutex
	file     *os.File
	month    string
	pruned   time.Time

	unsunk     int
	failStreak int
	lastLogged time.Time
	suppressed int
	unwritable bool

	recentMu    sync.Mutex
	recent      map[int64]map[Decision]int
	recentFloor int64
}

func New(cfg config.AuditConfig, logger *slog.Logger) (*Logger, error) {
	if !cfg.IsEnabled() {
		return &Logger{cfg: cfg, logger: logger, now: time.Now}, nil
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("audit: enabled without a directory (audit.dir)")
	}
	l := &Logger{cfg: cfg, logger: logger, now: time.Now}
	l.mu.Lock()
	err := l.rotate(l.now())
	if err != nil {
		l.unwritable = true
		l.reportFailure("open", err)
	}
	l.mu.Unlock()
	l.seedRecent()
	if err != nil {
		logger.Warn("audit log not writable: update decisions are not recorded until it is — mount a writable volume or set audit.enabled: false",
			"dir", cfg.Dir, "err", err)
		return l, nil
	}
	l.prune()
	return l, nil
}

func (l *Logger) AttachFailureSink(s FailureSink) {
	if l == nil || s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = s
	for ; l.unsunk > 0; l.unsunk-- {
		s.Inc()
	}
}

func (l *Logger) reportFailure(op string, err error) {
	if l.failures != nil {
		l.failures.Inc()
	} else {
		l.unsunk++
	}
	l.failStreak++
	if l.unwritable {
		return
	}
	now := l.now()
	if !l.lastLogged.IsZero() && now.Sub(l.lastLogged) < failureLogInterval {
		l.suppressed++
		return
	}
	l.logger.Error("audit write failed: update decisions are not being recorded",
		"op", op, "dir", l.cfg.Dir, "err", err,
		"failed_writes", l.failStreak, "suppressed_since_last_log", l.suppressed)
	l.lastLogged = now
	l.suppressed = 0
}

func (l *Logger) reportSuccess() {
	if l.failStreak == 0 {
		return
	}
	l.logger.Info("audit writes recovered", "dir", l.cfg.Dir, "failed_writes", l.failStreak)
	l.failStreak = 0
	l.unwritable = false
	l.suppressed = 0
	l.lastLogged = time.Time{}
}

func (l *Logger) Append(rec Record) {
	if l == nil {
		return
	}
	if rec.At.IsZero() {
		rec.At = l.clock().UTC()
	}
	l.countRecent(rec.At, rec.Decision)
	if !l.cfg.IsEnabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotate(rec.At); err != nil {
		l.reportFailure("open", err)
		return
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		l.reportFailure("marshal", err)
		return
	}
	buf = append(buf, '\n')
	if _, err := l.file.Write(buf); err != nil {
		l.reportFailure("write", err)
		_ = l.file.Close()
		l.file = nil
		return
	}
	l.reportSuccess()
	if l.now().Sub(l.pruned) > 24*time.Hour {
		l.prune()
	}
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func (l *Logger) rotate(t time.Time) error {
	month := t.UTC().Format("2006-01")
	if l.file != nil && l.month == month {
		return nil
	}
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		if err != nil {
			l.reportFailure("close", err)
		}
	}
	if err := os.MkdirAll(l.cfg.Dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(fileFor(l.cfg.Dir, month), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	l.month = month
	return nil
}

func (l *Logger) prune() {
	l.pruned = l.now()
	cutoff := l.now().Add(-l.cfg.Retention.Std())
	files, err := monthFiles(l.cfg.Dir)
	if err != nil {
		l.logger.Warn("audit prune failed: cannot list the audit directory", "dir", l.cfg.Dir, "err", err)
		return
	}
	for _, f := range files {
		if !f.end.Before(cutoff) {
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.logger.Warn("audit prune failed", "file", f.path, "err", err)
		} else {
			l.logger.Info("audit pruned", "file", f.path)
		}
	}
}
