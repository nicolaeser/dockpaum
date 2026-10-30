package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion     = 1
	DefaultDir        = "/var/lib/dockpaum/state"
	FileName          = "state.json"
	DefaultFlushDelay = 2 * time.Second
)

const (
	maxFileSize           = 32 << 20
	maxHosts              = 64
	maxImageDefaults      = 1024
	maxFirstSeen          = 2048
	maxResolved           = 1024
	maxPrevious           = 2048
	maxStatuses           = 4096
	maxNoticeContainers   = 4096
	maxSelfUpdateFailures = 64
	maxPendingOps         = 10000
	keepBackups           = 3
	lockTimeout           = 10 * time.Second
	lockPoll              = 25 * time.Millisecond
	firstSeenRetention    = 30 * 24 * time.Hour
	lastSeenResolution    = time.Hour
	corruptBackupMarker   = ".corrupt-"
	temporaryFileMarker   = ".tmp-"
	lockFileSuffix        = ".lock"
	backupTimeLayout      = "20060102T150405.000000000Z"
)

func PathIn(dir string) string { return filepath.Join(dir, FileName) }

type FailureSink interface{ Inc() }

type Options struct {
	Logger     *slog.Logger
	FlushDelay time.Duration
}

type Store struct {
	path       string
	logger     *slog.Logger
	durable    bool
	snapshot   bool
	flushDelay time.Duration
	now        func() time.Time

	flushMu  sync.Mutex
	mu       sync.Mutex
	doc      *document
	ops      []op
	opsGen   uint64
	claims   map[NoticeKey]string
	timer    *time.Timer
	closed   bool
	failing  bool
	failures FailureSink
}

type op func(*document)

func Open(path string, logger *slog.Logger) *Store {
	return OpenWith(path, Options{Logger: logger})
}

func OpenWith(path string, o Options) *Store {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	delay := o.FlushDelay
	if delay == 0 {
		delay = DefaultFlushDelay
	}
	s := &Store{
		path: path, logger: logger, flushDelay: delay, now: time.Now,
		doc: newDocument(), claims: map[NoticeKey]string{},
	}
	if path == "" {
		return s
	}
	doc, err := s.openFile()
	if doc != nil {
		s.doc = doc
	}
	if err != nil {
		s.logger.Warn("state: cannot write the state file; the state is kept in memory only and is lost on restart",
			"path", path, "err", err)
		return s
	}
	s.durable = true
	return s
}

func OpenSnapshot(path string, logger *slog.Logger) *Store {
	s := OpenWith("", Options{Logger: logger})
	if path == "" {
		return s
	}
	s.path, s.snapshot = path, true
	doc, err := readDocument(path)
	switch {
	case err == nil:
		s.doc = doc
	case !errors.Is(err, os.ErrNotExist):
		s.logger.Warn("state: the state file cannot be read; the snapshot starts empty", "path", path, "err", err)
	}
	return s
}

func (s *Store) openFile() (*document, error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return s.readOnlyLoad(), fmt.Errorf("create directory: %w", err)
	}
	unlock, err := lockFile(s.path+lockFileSuffix, lockTimeout)
	if err != nil {
		return s.readOnlyLoad(), fmt.Errorf("lock: %w", err)
	}
	defer unlock()
	s.removeTemporaryFiles()
	doc, err := s.loadLocked(nil)
	if err != nil {
		return nil, err
	}
	if err := probeWritable(dir); err != nil {
		return doc, err
	}
	return doc, nil
}

func (s *Store) readOnlyLoad() *document {
	doc, err := readDocument(s.path)
	if err != nil {
		return nil
	}
	return doc
}

func (s *Store) loadLocked(fallback func() *document) (*document, error) {
	doc, err := readDocument(s.path)
	switch {
	case err == nil:
		return doc, nil
	case errors.Is(err, os.ErrNotExist):
		return newDocument(), nil
	case errors.Is(err, errUnusable):
		backup := s.backupCorrupt()
		s.logger.Warn("state: the state file is unusable; it was moved aside and the state starts afresh",
			"path", s.path, "backup", backup, "err", err)
		if fallback != nil {
			return fallback(), nil
		}
		return newDocument(), nil
	default:
		return nil, fmt.Errorf("read: %w", err)
	}
}

var errUnusable = errors.New("unusable state file")

func readDocument(path string) (*document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > maxFileSize {
		return nil, fmt.Errorf("%w: larger than %d bytes", errUnusable, maxFileSize)
	}
	var doc document
	if err := json.Unmarshal(buf, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnusable, err)
	}
	switch {
	case doc.Version == 0:
		return nil, fmt.Errorf("%w: no schema version", errUnusable)
	case doc.Version > SchemaVersion:
		return nil, fmt.Errorf("%w: schema version %d is newer than %d (written by a newer dockpaum)", errUnusable, doc.Version, SchemaVersion)
	}
	doc.normalize()
	return &doc, nil
}

func (s *Store) backupCorrupt() string {
	backup := s.path + corruptBackupMarker + s.now().UTC().Format(backupTimeLayout)
	if err := os.Rename(s.path, backup); err != nil {
		s.logger.Warn("state: moving the unusable state file aside failed", "path", s.path, "err", err)
		return ""
	}
	matches, err := filepath.Glob(s.path + corruptBackupMarker + "*")
	if err != nil || len(matches) <= keepBackups {
		return backup
	}
	sort.Strings(matches)
	for _, old := range matches[:len(matches)-keepBackups] {
		if err := os.Remove(old); err != nil {
			s.logger.Debug("state: removing an old backup failed", "path", old, "err", err)
		}
	}
	return backup
}

func (s *Store) removeTemporaryFiles() {
	matches, err := filepath.Glob(s.path + temporaryFileMarker + "*")
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			s.logger.Debug("state: removing a leftover temporary file failed", "path", m, "err", err)
		}
	}
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func (s *Store) Path() string { return s.path }

func (s *Store) Durable() bool { return s.durable || s.snapshot }

func (s *Store) Snapshot() bool { return s.snapshot }

func (s *Store) AttachFailureSink(f FailureSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = f
}

func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if !s.durable || len(s.ops) == 0 {
		s.mu.Unlock()
		return nil
	}
	ops := slices.Clone(s.ops)
	gen := s.opsGen
	s.mu.Unlock()

	merged, err := s.write(ops)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if !s.failing {
			s.logger.Warn("state: writing the state file failed; changes are kept in memory and written again later",
				"path", s.path, "err", err)
		} else {
			s.logger.Debug("state: writing the state file failed again", "path", s.path, "err", err)
		}
		s.failing = true
		if s.failures != nil {
			s.failures.Inc()
		}
		return err
	}
	if s.failing {
		s.logger.Info("state: the state file is written again", "path", s.path)
		s.failing = false
	}
	rest := s.ops
	if gen == s.opsGen {
		rest = s.ops[len(ops):]
	}
	for _, o := range rest {
		o(merged)
	}
	s.doc = merged
	s.ops = slices.Clone(rest)
	if len(s.ops) > 0 {
		s.scheduleLocked()
	}
	return nil
}

func (s *Store) write(ops []op) (*document, error) {
	unlock, err := lockFile(s.path+lockFileSuffix, lockTimeout)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	defer unlock()
	doc, err := s.loadLocked(func() *document {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.doc.clone()
	})
	if err != nil {
		return nil, err
	}
	for _, o := range ops {
		o(doc)
	}
	doc.Version = SchemaVersion
	doc.UpdatedAt = s.now().UTC()
	doc.enforceCaps()
	if err := writeAtomic(s.path, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func writeAtomic(path string, doc *document) (err error) {
	buf, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+temporaryFileMarker+"*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	return s.Flush()
}

func (s *Store) Redacted() ([]byte, error) {
	s.mu.Lock()
	doc := s.doc.clone()
	s.mu.Unlock()
	for _, h := range doc.Hosts {
		for id, e := range h.ImageDefaults {
			e.Defaults = redactEnv(e.Defaults)
			h.ImageDefaults[id] = e
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}

const redactedValue = "***"

func redactEnv(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return json.RawMessage(`{}`)
	}
	envRaw, ok := fields["Env"]
	if !ok {
		return raw
	}
	var env []string
	if err := json.Unmarshal(envRaw, &env); err != nil {
		delete(fields, "Env")
	} else {
		for i, e := range env {
			if k, _, found := strings.Cut(e, "="); found {
				env[i] = k + "=" + redactedValue
			}
		}
		buf, err := json.Marshal(env)
		if err != nil {
			delete(fields, "Env")
		} else {
			fields["Env"] = buf
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func (s *Store) applyLocked(o op) {
	o(s.doc)
	if !s.durable {
		return
	}
	s.ops = append(s.ops, o)
	if len(s.ops) > maxPendingOps {
		snap := s.doc.clone()
		s.ops = []op{func(d *document) { *d = *snap.clone() }}
		s.opsGen++
	}
	s.scheduleLocked()
}

func (s *Store) scheduleLocked() {
	if !s.durable || s.closed || s.flushDelay < 0 || s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(s.flushDelay, func() { _ = s.Flush() })
}

type document struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at"`
	Hosts     map[string]*hostState `json:"hosts"`
}

type hostState struct {
	ImageDefaults      map[string]defaultsEntry                     `json:"image_defaults,omitempty"`
	FirstSeen          map[string]firstSeenEntry                    `json:"first_seen,omitempty"`
	Resolved           map[string]resolvedEntry                     `json:"resolved_images,omitempty"`
	Previous           map[string]PreviousImage                     `json:"previous_images,omitempty"`
	Containers         map[string]json.RawMessage                   `json:"containers,omitempty"`
	Notices            map[string]map[string]map[string]noticeEntry `json:"notices,omitempty"`
	SelfUpdateFailures map[string]SelfUpdateFailure                 `json:"self_update_failures,omitempty"`
}

type defaultsEntry struct {
	RecordedAt time.Time       `json:"recorded_at"`
	Defaults   json.RawMessage `json:"defaults"`
}

type firstSeenEntry struct {
	Repo      string    `json:"repo,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type resolvedEntry struct {
	Repo string `json:"repo,omitempty"`
	ResolvedImage
	LastUsed time.Time `json:"last_used"`
}

type noticeEntry struct {
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
}

func newDocument() *document {
	return &document{Version: SchemaVersion, Hosts: map[string]*hostState{}}
}

func (d *document) normalize() {
	if d.Hosts == nil {
		d.Hosts = map[string]*hostState{}
	}
	for name, h := range d.Hosts {
		if h == nil {
			delete(d.Hosts, name)
		}
	}
}

func (d *document) host(name string) *hostState {
	h := d.Hosts[name]
	if h == nil {
		h = &hostState{}
		d.Hosts[name] = h
	}
	return h
}

func (d *document) clone() *document {
	buf, err := json.Marshal(d)
	if err != nil {
		return newDocument()
	}
	var c document
	if err := json.Unmarshal(buf, &c); err != nil {
		return newDocument()
	}
	c.normalize()
	return &c
}

func (d *document) enforceCaps() {
	if len(d.Hosts) > maxHosts {
		names := slices.Sorted(maps.Keys(d.Hosts))
		for _, n := range names[maxHosts:] {
			delete(d.Hosts, n)
		}
	}
	for _, h := range d.Hosts {
		evictOldest(h.ImageDefaults, maxImageDefaults, func(e defaultsEntry) time.Time { return e.RecordedAt })
		evictOldest(h.FirstSeen, maxFirstSeen, func(e firstSeenEntry) time.Time { return e.LastSeen })
		evictOldest(h.Resolved, maxResolved, func(e resolvedEntry) time.Time { return e.LastUsed })
		evictOldest(h.Previous, maxPrevious, func(e PreviousImage) time.Time { return e.UpdatedAt })
		if len(h.Containers) > maxStatuses {
			names := slices.Sorted(maps.Keys(h.Containers))
			for _, n := range names[maxStatuses:] {
				delete(h.Containers, n)
			}
		}
		evictOldest(h.Notices, maxNoticeContainers, latestNotice)
		evictOldest(h.SelfUpdateFailures, maxSelfUpdateFailures, func(e SelfUpdateFailure) time.Time { return e.At })
	}
}

func evictOldest[V any](m map[string]V, limit int, at func(V) time.Time) {
	if len(m) <= limit {
		return
	}
	keys := slices.Collect(maps.Keys(m))
	sort.Slice(keys, func(i, j int) bool {
		ti, tj := at(m[keys[i]]), at(m[keys[j]])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys[:len(keys)-limit] {
		delete(m, k)
	}
}

func latestNotice(byNotifier map[string]map[string]noticeEntry) time.Time {
	var t time.Time
	for _, byEvent := range byNotifier {
		for _, e := range byEvent {
			if e.At.After(t) {
				t = e.At
			}
		}
	}
	return t
}

func canonical(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

func (s *Store) ImageDefaults(host, imageID string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return nil, false
	}
	e, ok := h.ImageDefaults[imageID]
	if !ok || len(e.Defaults) == 0 {
		return nil, false
	}
	return slices.Clone(e.Defaults), true
}

func (s *Store) PutImageDefaults(host, imageID string, defaults json.RawMessage) {
	if host == "" || imageID == "" || len(defaults) == 0 {
		return
	}
	value := canonical(defaults)
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.doc.Hosts[host]; h != nil {
		if e, ok := h.ImageDefaults[imageID]; ok && bytes.Equal(canonical(e.Defaults), value) {
			return
		}
	}
	entry := defaultsEntry{RecordedAt: s.now().UTC(), Defaults: value}
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.ImageDefaults == nil {
			h.ImageDefaults = map[string]defaultsEntry{}
		}
		h.ImageDefaults[imageID] = entry
		evictOldest(h.ImageDefaults, maxImageDefaults, func(e defaultsEntry) time.Time { return e.RecordedAt })
	})
}

func (s *Store) FirstSeen(host, digest, repo string, now time.Time) time.Time {
	if host == "" || digest == "" {
		return now
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.doc.Hosts[host]; h != nil {
		if e, ok := h.FirstSeen[digest]; ok && !e.FirstSeen.IsZero() {
			if now.Sub(e.LastSeen) < lastSeenResolution && (repo == "" || repo == e.Repo) {
				return e.FirstSeen
			}
		}
	}
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.FirstSeen == nil {
			h.FirstSeen = map[string]firstSeenEntry{}
		}
		e := h.FirstSeen[digest]
		if e.FirstSeen.IsZero() || now.Before(e.FirstSeen) {
			e.FirstSeen = now
		}
		if now.After(e.LastSeen) {
			e.LastSeen = now
		}
		if repo != "" {
			e.Repo = repo
		}
		h.FirstSeen[digest] = e
		evictOldest(h.FirstSeen, maxFirstSeen, func(e firstSeenEntry) time.Time { return e.LastSeen })
	})
	return s.doc.Hosts[host].FirstSeen[digest].FirstSeen
}

func (s *Store) LookupFirstSeen(host, digest string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return time.Time{}, false
	}
	e, ok := h.FirstSeen[digest]
	if !ok || e.FirstSeen.IsZero() {
		return time.Time{}, false
	}
	return e.FirstSeen, true
}

type ResolvedImage struct {
	ImageID  string            `json:"image_id"`
	Manifest string            `json:"manifest"`
	Created  time.Time         `json:"created,omitzero"`
	Labels   map[string]string `json:"labels,omitempty"`
}

func ResolvedKey(repo, digest, platform string) string {
	return repo + "@" + digest + " " + platform
}

func (s *Store) Resolved(host, key string) (ResolvedImage, bool) {
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return ResolvedImage{}, false
	}
	e, ok := h.Resolved[key]
	if !ok || e.ImageID == "" {
		return ResolvedImage{}, false
	}
	if now.Sub(e.LastUsed) >= lastSeenResolution {
		s.applyLocked(func(d *document) {
			if h := d.Hosts[host]; h != nil {
				if e, ok := h.Resolved[key]; ok && now.After(e.LastUsed) {
					e.LastUsed = now
					h.Resolved[key] = e
				}
			}
		})
	}
	r := e.ResolvedImage
	r.Labels = maps.Clone(r.Labels)
	return r, true
}

func (s *Store) PutResolved(host, key, repo string, r ResolvedImage) {
	if host == "" || key == "" || r.ImageID == "" {
		return
	}
	r.Created = r.Created.UTC()
	r.Labels = maps.Clone(r.Labels)
	entry := resolvedEntry{Repo: repo, ResolvedImage: r, LastUsed: s.now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.Resolved == nil {
			h.Resolved = map[string]resolvedEntry{}
		}
		h.Resolved[key] = entry
		evictOldest(h.Resolved, maxResolved, func(e resolvedEntry) time.Time { return e.LastUsed })
	})
}

type PreviousImage struct {
	Image       string    `json:"image"`
	ImageID     string    `json:"image_id"`
	RepoDigests []string  `json:"repo_digests,omitempty"`
	NewImage    string    `json:"new_image,omitempty"`
	NewImageID  string    `json:"new_image_id,omitempty"`
	ContainerID string    `json:"container_id,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	RunID       string    `json:"run_id,omitempty"`
}

func (s *Store) RecordPrevious(host, container string, p PreviousImage) {
	if host == "" || container == "" {
		return
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = s.now()
	}
	p.UpdatedAt = p.UpdatedAt.UTC()
	p.RepoDigests = slices.Clone(p.RepoDigests)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.Previous == nil {
			h.Previous = map[string]PreviousImage{}
		}
		h.Previous[container] = p
		evictOldest(h.Previous, maxPrevious, func(e PreviousImage) time.Time { return e.UpdatedAt })
	})
}

func (s *Store) Previous(host, container string) (PreviousImage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return PreviousImage{}, false
	}
	p, ok := h.Previous[container]
	p.RepoDigests = slices.Clone(p.RepoDigests)
	return p, ok
}

type SelfUpdateFailure struct {
	Image   string    `json:"image,omitempty"`
	ImageID string    `json:"image_id"`
	At      time.Time `json:"at"`
}

func (s *Store) RecordSelfUpdateFailure(host, container string, f SelfUpdateFailure) {
	if host == "" || container == "" || f.ImageID == "" {
		return
	}
	if f.At.IsZero() {
		f.At = s.now()
	}
	f.At = f.At.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.SelfUpdateFailures == nil {
			h.SelfUpdateFailures = map[string]SelfUpdateFailure{}
		}
		h.SelfUpdateFailures[container] = f
		evictOldest(h.SelfUpdateFailures, maxSelfUpdateFailures, func(e SelfUpdateFailure) time.Time { return e.At })
	})
}

func (s *Store) LastSelfUpdateFailure(host, container string) (SelfUpdateFailure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return SelfUpdateFailure{}, false
	}
	f, ok := h.SelfUpdateFailures[container]
	return f, ok
}

func (s *Store) ClearSelfUpdateFailure(host, container string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return
	}
	if _, ok := h.SelfUpdateFailures[container]; !ok {
		return
	}
	s.applyLocked(func(d *document) {
		if h := d.Hosts[host]; h != nil {
			delete(h.SelfUpdateFailures, container)
		}
	})
}

type StoredStatus struct {
	Host      string
	Container string
	Status    json.RawMessage
}

func (s *Store) PutContainerStatus(host, container string, status json.RawMessage) {
	if host == "" || container == "" || len(status) == 0 {
		return
	}
	value := canonical(status)
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.doc.Hosts[host]; h != nil {
		if old, ok := h.Containers[container]; ok && bytes.Equal(canonical(old), value) {
			return
		}
	}
	s.applyLocked(func(d *document) {
		h := d.host(host)
		if h.Containers == nil {
			h.Containers = map[string]json.RawMessage{}
		}
		h.Containers[container] = value
	})
}

func (s *Store) DeleteContainerStatus(host, container string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.doc.Hosts[host]
	if h == nil {
		return
	}
	if _, ok := h.Containers[container]; !ok {
		return
	}
	s.applyLocked(func(d *document) {
		if h := d.Hosts[host]; h != nil {
			delete(h.Containers, container)
		}
	})
}

func (s *Store) ContainerStatuses() []StoredStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StoredStatus
	for host, h := range s.doc.Hosts {
		for name, raw := range h.Containers {
			out = append(out, StoredStatus{Host: host, Container: name, Status: slices.Clone(raw)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Container < out[j].Container
	})
	return out
}

type NoticeKey struct {
	Notifier  string
	Event     string
	Host      string
	Container string
}

func (s *Store) MarkNotice(k NoticeKey, fingerprint string) (fresh bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claimed, ok := s.claims[k]; ok && claimed == fingerprint {
		return false
	}
	if e, ok := s.committedLocked(k); ok && e.Fingerprint == fingerprint {
		return false
	}
	s.claims[k] = fingerprint
	return true
}

func (s *Store) CommitNotice(k NoticeKey, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims[k] == fingerprint {
		delete(s.claims, k)
	}
	if e, ok := s.committedLocked(k); ok && e.Fingerprint == fingerprint {
		return
	}
	entry := noticeEntry{Fingerprint: fingerprint, At: s.now().UTC()}
	s.applyLocked(func(d *document) {
		h := d.host(k.Host)
		if h.Notices == nil {
			h.Notices = map[string]map[string]map[string]noticeEntry{}
		}
		byNotifier := h.Notices[k.Container]
		if byNotifier == nil {
			byNotifier = map[string]map[string]noticeEntry{}
			h.Notices[k.Container] = byNotifier
		}
		byEvent := byNotifier[k.Notifier]
		if byEvent == nil {
			byEvent = map[string]noticeEntry{}
			byNotifier[k.Notifier] = byEvent
		}
		byEvent[k.Event] = entry
		evictOldest(h.Notices, maxNoticeContainers, latestNotice)
	})
}

func (s *Store) UnmarkNotice(k NoticeKey, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims[k] == fingerprint {
		delete(s.claims, k)
	}
}

func (s *Store) ForgetNotices(host, container string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.claims {
		if k.Host == host && k.Container == container {
			delete(s.claims, k)
		}
	}
	if h := s.doc.Hosts[host]; h == nil || h.Notices[container] == nil {
		return
	}
	s.applyLocked(func(d *document) {
		if h := d.Hosts[host]; h != nil {
			delete(h.Notices, container)
		}
	})
}

func (s *Store) committedLocked(k NoticeKey) (noticeEntry, bool) {
	h := s.doc.Hosts[k.Host]
	if h == nil {
		return noticeEntry{}, false
	}
	e, ok := h.Notices[k.Container][k.Notifier][k.Event]
	return e, ok
}

type Live struct {
	Containers   []string
	Images       []string
	Repos        []string
	ReposUnknown bool
}

func (s *Store) PruneHost(host string, live Live) {
	containers := setOf(live.Containers)
	images := setOf(live.Images)
	repos := setOf(live.Repos)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.claims {
		if k.Host == host && !containers[k.Container] {
			delete(s.claims, k)
		}
	}
	h := s.doc.Hosts[host]
	if h == nil {
		return
	}
	cutoff := s.now().Add(-firstSeenRetention)
	var defaults, seen, resolved, previous, statuses, notices, failures []string
	for id := range h.ImageDefaults {
		if !images[id] {
			defaults = append(defaults, id)
		}
	}
	for digest, e := range h.FirstSeen {
		if (!live.ReposUnknown && e.Repo != "" && !repos[e.Repo]) || e.LastSeen.Before(cutoff) {
			seen = append(seen, digest)
		}
	}
	for key, e := range h.Resolved {
		if (!live.ReposUnknown && e.Repo != "" && !repos[e.Repo]) || e.LastUsed.Before(cutoff) {
			resolved = append(resolved, key)
		}
	}
	for name := range h.Previous {
		if !containers[name] {
			previous = append(previous, name)
		}
	}
	for name := range h.Containers {
		if !containers[name] {
			statuses = append(statuses, name)
		}
	}
	for name := range h.Notices {
		if !containers[name] {
			notices = append(notices, name)
		}
	}
	for name := range h.SelfUpdateFailures {
		if !containers[name] {
			failures = append(failures, name)
		}
	}
	if len(defaults)+len(seen)+len(resolved)+len(previous)+len(statuses)+len(notices)+len(failures) == 0 {
		return
	}
	s.applyLocked(func(d *document) {
		h := d.Hosts[host]
		if h == nil {
			return
		}
		for _, k := range defaults {
			delete(h.ImageDefaults, k)
		}
		for _, k := range seen {
			delete(h.FirstSeen, k)
		}
		for _, k := range resolved {
			delete(h.Resolved, k)
		}
		for _, k := range previous {
			delete(h.Previous, k)
		}
		for _, k := range statuses {
			delete(h.Containers, k)
		}
		for _, k := range notices {
			delete(h.Notices, k)
		}
		for _, k := range failures {
			delete(h.SelfUpdateFailures, k)
		}
	})
}

func (s *Store) RetainHosts(hosts []string) {
	keep := setOf(hosts)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.claims {
		if !keep[k.Host] {
			delete(s.claims, k)
		}
	}
	var drop []string
	for name := range s.doc.Hosts {
		if !keep[name] {
			drop = append(drop, name)
		}
	}
	if len(drop) == 0 {
		return
	}
	s.applyLocked(func(d *document) {
		for _, name := range drop {
			delete(d.Hosts, name)
		}
	})
}

func setOf(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			m[it] = true
		}
	}
	return m
}
