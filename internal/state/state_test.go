package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) count(level string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), "level="+level)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger() (*slog.Logger, *logBuffer) {
	lb := &logBuffer{}
	return slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})), lb
}

func open(t *testing.T, path string) (*Store, *logBuffer) {
	t.Helper()
	logger, lb := testLogger()
	s := OpenWith(path, Options{Logger: logger, FlushDelay: -1})
	t.Cleanup(func() { _ = s.Close() })
	return s, lb
}

func readFile(t *testing.T, path string) document {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d document
	if err := json.Unmarshal(buf, &d); err != nil {
		t.Fatalf("state file is not valid JSON: %v\n%s", err, buf)
	}
	return d
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

func readOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- a test directory, kept searchable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) }) // #nosec G302 -- restores the test directory
}

func writable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o750); err != nil { // #nosec G302 -- restores the test directory
		t.Fatal(err)
	}
}

type counter struct{ n atomic.Int32 }

func (c *counter) Inc() { c.n.Add(1) }

var t0 = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func TestInMemoryStore(t *testing.T) {
	s, lb := open(t, "")
	if s.Durable() || s.Path() != "" {
		t.Fatal("an empty path must give an in-memory store")
	}
	s.PutImageDefaults("local", "sha256:a", json.RawMessage(`{"Cmd":["a"]}`))
	if got, ok := s.ImageDefaults("local", "sha256:a"); !ok || string(got) != `{"Cmd":["a"]}` {
		t.Fatalf("defaults = %s %v", got, ok)
	}
	if got := s.FirstSeen("local", "sha256:b", "repo", t0); !got.Equal(t0) {
		t.Fatalf("first seen = %v", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.ops) != 0 {
		t.Fatal("an in-memory store queues no writes")
	}
	if lb.count("WARN") != 0 {
		t.Fatalf("an explicitly in-memory store must not warn:\n%s", lb)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := PathIn(dir)
	s, _ := open(t, path)
	s.now = func() time.Time { return t0 }
	if !s.Durable() {
		t.Fatal("store should be durable")
	}
	s.PutImageDefaults("local", "sha256:a", json.RawMessage(`{"Env":["A=1"],"Cmd":null}`))
	seen := s.FirstSeen("local", "sha256:new", "docker.io/library/nginx", t0)
	prev := PreviousImage{Image: "nginx:1.27", ImageID: "sha256:a", RepoDigests: []string{"nginx@sha256:aa"},
		NewImage: "nginx:1.28", NewImageID: "sha256:new", ContainerID: "c2", UpdatedAt: t0, RunID: "r1"}
	s.RecordPrevious("local", "web", prev)
	s.PutContainerStatus("local", "web", json.RawMessage(`{"decision": "updated"}`))
	k := NoticeKey{Notifier: "ops", Event: "update.skipped", Host: "local", Container: "web"}
	if !s.MarkNotice(k, "fp1") {
		t.Fatal("first notice must be fresh")
	}
	s.CommitNotice(k, "fp1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("state file mode = %o, want 600", mode)
	}
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm()&0o007 != 0 {
		t.Errorf("state directory must not be world-accessible: %v %v", di.Mode(), err)
	}
	if d := readFile(t, path); d.Version != SchemaVersion || !d.UpdatedAt.Equal(t0) {
		t.Errorf("file header = version %d updated %v", d.Version, d.UpdatedAt)
	}

	r, _ := open(t, path)
	if got, ok := r.ImageDefaults("local", "sha256:a"); !ok || string(got) != `{"Env":["A=1"],"Cmd":null}` {
		t.Errorf("defaults after reopen = %s %v", got, ok)
	}
	if got, ok := r.LookupFirstSeen("local", "sha256:new"); !ok || !got.Equal(seen) {
		t.Errorf("first seen after reopen = %v %v", got, ok)
	}
	if got, ok := r.Previous("local", "web"); !ok || !reflect.DeepEqual(got, prev) {
		t.Errorf("previous after reopen = %+v %v", got, ok)
	}
	if got := r.ContainerStatuses(); len(got) != 1 || string(got[0].Status) != `{"decision":"updated"}` {
		t.Errorf("statuses after reopen = %+v", got)
	}
	if r.MarkNotice(k, "fp1") {
		t.Error("a committed notice must stay delivered across a restart")
	}
}

func TestCorruptFileIsBackedUp(t *testing.T) {
	tests := []struct {
		name    string
		content func(path string) error
		reason  string
	}{
		{name: "garbage", content: func(p string) error { return os.WriteFile(p, []byte("{not json"), 0o600) }, reason: "unusable"},
		{name: "truncated", content: func(p string) error { return os.WriteFile(p, []byte(`{"version":1,"hosts":{"a":`), 0o600) }, reason: "unusable"},
		{name: "no version", content: func(p string) error { return os.WriteFile(p, []byte(`{"hosts":{}}`), 0o600) }, reason: "no schema version"},
		{name: "newer version", content: func(p string) error { return os.WriteFile(p, []byte(`{"version":99,"hosts":{}}`), 0o600) }, reason: "newer"},
		{name: "oversized", content: func(p string) error {
			if err := os.WriteFile(p, []byte(`{"version":1}`), 0o600); err != nil {
				return err
			}
			return os.Truncate(p, maxFileSize+10)
		}, reason: "larger than"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := PathIn(t.TempDir())
			if err := tc.content(path); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			s, lb := open(t, path)
			if !s.Durable() {
				t.Fatal("a corrupt file must not make the store read-only")
			}
			if lb.count("WARN") != 1 || !strings.Contains(lb.String(), tc.reason) {
				t.Fatalf("want one warning naming %q:\n%s", tc.reason, lb)
			}
			backups, _ := filepath.Glob(path + corruptBackupMarker + "*")
			if len(backups) != 1 {
				t.Fatalf("backups = %v", backups)
			}
			if bi, _ := os.Stat(backups[0]); bi.Size() != before.Size() {
				t.Fatal("the backup must keep the original content")
			}
			if len(s.ContainerStatuses()) != 0 {
				t.Fatal("the store must start empty")
			}
			s.PutContainerStatus("local", "web", json.RawMessage(`{}`))
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			if d := readFile(t, path); d.Version != SchemaVersion || d.Hosts["local"] == nil {
				t.Fatalf("a fresh file must be written: %+v", d)
			}
		})
	}
}

func TestOnlyTheNewestBackupsAreKept(t *testing.T) {
	path := PathIn(t.TempDir())
	clock := t0
	for i := 0; i < keepBackups+2; i++ {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("broken %d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		logger, _ := testLogger()
		s := &Store{path: path, logger: logger, now: func() time.Time { return clock }}
		if _, err := s.openFile(); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Second)
	}
	backups, _ := filepath.Glob(path + corruptBackupMarker + "*")
	if len(backups) != keepBackups {
		t.Fatalf("backups = %v, want %d", backups, keepBackups)
	}
	newest, err := os.ReadFile(backups[len(backups)-1])
	if err != nil || string(newest) != fmt.Sprintf("broken %d", keepBackups+1) {
		t.Fatalf("newest backup = %q %v", newest, err)
	}
}

func TestUnwritableLocationFallsBackToMemory(t *testing.T) {
	t.Run("directory cannot be created", func(t *testing.T) {
		parent := t.TempDir()
		blocker := filepath.Join(parent, "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s, lb := open(t, PathIn(filepath.Join(blocker, "state")))
		if s.Durable() {
			t.Fatal("store must fall back to memory")
		}
		if lb.count("WARN") != 1 {
			t.Fatalf("want exactly one warning:\n%s", lb)
		}
		s.PutContainerStatus("local", "web", json.RawMessage(`{}`))
		if err := s.Flush(); err != nil {
			t.Fatal("an in-memory store's flush must not fail")
		}
		if lb.count("WARN") != 1 {
			t.Fatalf("later writes must not warn again:\n%s", lb)
		}
	})
	t.Run("read-only directory keeps the recorded data", func(t *testing.T) {
		skipAsRoot(t)
		dir := t.TempDir()
		path := PathIn(dir)
		w, _ := open(t, path)
		w.RecordPrevious("local", "web", PreviousImage{Image: "app:1", ImageID: "sha256:a"})
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		readOnly(t, dir)
		s, lb := open(t, path)
		if s.Durable() {
			t.Fatal("store must fall back to memory")
		}
		if lb.count("WARN") != 1 {
			t.Fatalf("want exactly one warning:\n%s", lb)
		}
		if p, ok := s.Previous("local", "web"); !ok || p.Image != "app:1" {
			t.Fatalf("data of the read-only file must be loaded: %+v %v", p, ok)
		}
	})
}

func TestFailedWriteKeepsTheOldFile(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	path := PathIn(dir)
	s, lb := open(t, path)
	failures := &counter{}
	s.AttachFailureSink(failures)
	s.PutContainerStatus("local", "web", json.RawMessage(`{"v":1}`))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	readOnly(t, dir)
	s.PutContainerStatus("local", "web", json.RawMessage(`{"v":2}`))
	for i := 0; i < 2; i++ {
		if err := s.Flush(); err == nil {
			t.Fatal("writing into a read-only directory must fail")
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("a failed write changed the file:\n%s\n%s", before, after)
	}
	if lb.count("WARN") != 1 || failures.n.Load() != 2 {
		t.Fatalf("want one warning and two counted failures, got %d / %d:\n%s", lb.count("WARN"), failures.n.Load(), lb)
	}
	if leftovers, _ := filepath.Glob(path + temporaryFileMarker + "*"); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}

	writable(t, dir)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path).Hosts["local"].Containers["web"]; string(got) != `{"v":2}` {
		t.Fatalf("pending change not written after recovery: %s", got)
	}
	if !strings.Contains(lb.String(), "written again") {
		t.Fatalf("recovery must be logged:\n%s", lb)
	}
}

func TestLeftoverTemporaryFilesAreRemoved(t *testing.T) {
	path := PathIn(t.TempDir())
	if err := os.WriteFile(path, []byte(`{"version":1,"hosts":{"local":{"containers":{"web":{"v":1}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := path + temporaryFileMarker + "123"
	if err := os.WriteFile(tmp, []byte(`{"version":1,"hosts":{"local":{"containers":{"web":{"v":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := open(t, path)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("leftover temporary file must be removed")
	}
	if got := s.ContainerStatuses(); len(got) != 1 || string(got[0].Status) != `{"v":1}` {
		t.Fatalf("the complete file must be used: %+v", got)
	}
}

func TestConcurrentWritersMerge(t *testing.T) {
	path := PathIn(t.TempDir())
	a, _ := open(t, path)
	b, _ := open(t, path)
	a.PutContainerStatus("local", "web", json.RawMessage(`{"by":"a"}`))
	a.FirstSeen("local", "sha256:x", "repo", t0)
	b.PutContainerStatus("local", "db", json.RawMessage(`{"by":"b"}`))
	b.FirstSeen("local", "sha256:x", "repo", t0.Add(time.Hour))
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	d := readFile(t, path)
	if len(d.Hosts["local"].Containers) != 2 {
		t.Fatalf("a write lost the other process's record: %v", d.Hosts["local"].Containers)
	}
	if got := d.Hosts["local"].FirstSeen["sha256:x"].FirstSeen; !got.Equal(t0) {
		t.Fatalf("first seen = %v, want the earliest sighting %v", got, t0)
	}
	if got, _ := b.LookupFirstSeen("local", "sha256:x"); !got.Equal(t0) {
		t.Fatalf("b's view after its write = %v", got)
	}
	if len(b.ContainerStatuses()) != 2 {
		t.Fatal("b's view must include a's records after its write")
	}
}

func TestFileIsLockedWhileWritten(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	unlock, err := lockFile(path+lockFileSuffix, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.PutContainerStatus("local", "web", json.RawMessage(`{}`))
	done := make(chan error, 1)
	go func() { done <- s.Flush() }()
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "locked") {
			t.Skip("advisory locks are not available on this platform")
		}
		t.Fatalf("write did not wait for the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the file must be written once the lock is free")
	}
}

func TestFirstSeen(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	if got := s.FirstSeen("local", "sha256:x", "repo", t0); !got.Equal(t0) {
		t.Fatalf("first sighting = %v", got)
	}
	if got := s.FirstSeen("local", "sha256:x", "repo", t0.Add(2*time.Hour)); !got.Equal(t0) {
		t.Fatalf("a later sighting must keep the first time, got %v", got)
	}
	if got := s.doc.Hosts["local"].FirstSeen["sha256:x"].LastSeen; !got.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("last seen = %v", got)
	}
	n := len(s.ops)
	s.FirstSeen("local", "sha256:x", "repo", t0.Add(2*time.Hour+time.Minute))
	if len(s.ops) != n {
		t.Fatal("a sighting within the resolution must not queue a write")
	}
	if got := s.FirstSeen("other", "sha256:x", "repo", t0.Add(3*time.Hour)); !got.Equal(t0.Add(3 * time.Hour)) {
		t.Fatalf("first sightings are per host, got %v", got)
	}
	if _, ok := s.LookupFirstSeen("local", "sha256:unknown"); ok {
		t.Fatal("lookup must not invent entries")
	}
	if _, ok := s.LookupFirstSeen("local", "sha256:unknown"); ok {
		t.Fatal("lookup must not record anything")
	}
}

func TestUnchangedValuesAreNotRewritten(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	s.PutImageDefaults("local", "sha256:a", json.RawMessage(`{"Cmd": ["a"]}`))
	s.PutContainerStatus("local", "web", json.RawMessage(`{"a": 1}`))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.PutImageDefaults("local", "sha256:a", json.RawMessage(`{"Cmd":["a"]}`))
	s.PutContainerStatus("local", "web", json.RawMessage("{\n \"a\": 1\n}"))
	if len(s.ops) != 0 {
		t.Fatalf("unchanged values queued %d writes", len(s.ops))
	}
}

func TestNoticeLedger(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	k := NoticeKey{Notifier: "ops", Event: "update.skipped", Host: "local", Container: "web"}
	other := NoticeKey{Notifier: "team", Event: "update.skipped", Host: "local", Container: "web"}

	steps := []struct {
		name string
		do   func() bool
		want bool
	}{
		{"new notice is fresh", func() bool { return s.MarkNotice(k, "digest-1|monitor-only") }, true},
		{"claimed notice is not sent twice", func() bool { return s.MarkNotice(k, "digest-1|monitor-only") }, false},
		{"another notifier is independent", func() bool { return s.MarkNotice(other, "digest-1|monitor-only") }, true},
		{"failed delivery is retried", func() bool {
			s.UnmarkNotice(other, "digest-1|monitor-only")
			return s.MarkNotice(other, "digest-1|monitor-only")
		}, true},
		{"delivered notice is not repeated", func() bool {
			s.CommitNotice(k, "digest-1|monitor-only")
			return s.MarkNotice(k, "digest-1|monitor-only")
		}, false},
		{"a failed delivery of a newer notice keeps the delivered one", func() bool {
			s.MarkNotice(k, "digest-2|monitor-only")
			s.UnmarkNotice(k, "digest-2|monitor-only")
			return s.MarkNotice(k, "digest-1|monitor-only")
		}, false},
		{"a new candidate is fresh", func() bool { return s.MarkNotice(k, "digest-2|monitor-only") }, true},
		{"after an update everything is fresh", func() bool {
			s.CommitNotice(k, "digest-2|monitor-only")
			s.ForgetNotices("local", "web")
			return s.MarkNotice(k, "digest-2|monitor-only")
		}, true},
	}
	for _, st := range steps {
		if got := st.do(); got != st.want {
			t.Fatalf("%s: fresh = %v, want %v", st.name, got, st.want)
		}
	}

	s.CommitNotice(k, "digest-3|dry-run")
	s.MarkNotice(other, "digest-3|dry-run")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := open(t, path)
	if r.MarkNotice(k, "digest-3|dry-run") {
		t.Fatal("a committed notice must stay delivered after a restart")
	}
	if !r.MarkNotice(other, "digest-3|dry-run") {
		t.Fatal("a notice that was only claimed must be sent again after a restart")
	}
}

func TestPruneHost(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	s.now = func() time.Time { return t0 }
	for _, id := range []string{"sha256:live", "sha256:gone"} {
		s.PutImageDefaults("local", id, json.RawMessage(`{}`))
	}
	s.FirstSeen("local", "sha256:cand", "example.com/app", t0)
	s.FirstSeen("local", "sha256:orphan", "example.com/removed", t0)
	s.FirstSeen("local", "sha256:stale", "example.com/app", t0.Add(-firstSeenRetention-time.Hour))
	for _, name := range []string{"web", "old"} {
		s.RecordPrevious("local", name, PreviousImage{Image: "app:1"})
		s.PutContainerStatus("local", name, json.RawMessage(`{}`))
		s.CommitNotice(NoticeKey{Notifier: "n", Event: "e", Host: "local", Container: name}, "fp")
	}
	s.MarkNotice(NoticeKey{Notifier: "n", Event: "e2", Host: "local", Container: "old"}, "claimed")
	s.PutContainerStatus("remote", "old", json.RawMessage(`{}`))
	for _, name := range []string{"web", "old"} {
		s.RecordSelfUpdateFailure("local", name, SelfUpdateFailure{ImageID: "sha256:bad"})
	}

	s.PruneHost("local", Live{
		Containers: []string{"web"},
		Images:     []string{"sha256:live"},
		Repos:      []string{"example.com/app"},
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, view := range []map[string]*hostState{s.doc.Hosts, func() map[string]*hostState { d := readFile(t, path); return d.Hosts }()} {
		h := view["local"]
		if got := keys(h.ImageDefaults); !reflect.DeepEqual(got, []string{"sha256:live"}) {
			t.Errorf("image defaults = %v", got)
		}
		if got := keys(h.FirstSeen); !reflect.DeepEqual(got, []string{"sha256:cand"}) {
			t.Errorf("first seen = %v", got)
		}
		if got := keys(h.Previous); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("previous = %v", got)
		}
		if got := keys(h.Containers); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("statuses = %v", got)
		}
		if got := keys(h.Notices); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("notices = %v", got)
		}
		if got := keys(h.SelfUpdateFailures); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("failed self-updates = %v", got)
		}
		if view["remote"] == nil || len(view["remote"].Containers) != 1 {
			t.Error("pruning a host must not touch another")
		}
	}
	if len(s.claims) != 0 {
		t.Errorf("claims of removed containers must go: %v", s.claims)
	}
}

func TestPruneHostKeepsSightingsWhenReposAreUnknown(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	s.now = func() time.Time { return t0 }
	s.FirstSeen("local", "sha256:cand", "example.com/app", t0)
	s.PruneHost("local", Live{Containers: []string{"web"}, Images: []string{"sha256:old"}, ReposUnknown: true})
	if _, ok := s.LookupFirstSeen("local", "sha256:cand"); !ok {
		t.Fatal("a sighting must not be pruned by repository while a repository is unknown")
	}
	s.PruneHost("local", Live{Containers: []string{"web"}, Images: []string{"sha256:old"}})
	if _, ok := s.LookupFirstSeen("local", "sha256:cand"); ok {
		t.Fatal("a sighting in a repository no container follows is pruned")
	}
}

func TestSelfUpdateFailures(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	s.now = func() time.Time { return t0 }
	if _, ok := s.LastSelfUpdateFailure("local", "dockpaum"); ok {
		t.Fatal("nothing recorded yet")
	}
	s.RecordSelfUpdateFailure("local", "dockpaum", SelfUpdateFailure{Image: "example.com/dockpaum:latest", ImageID: "sha256:bad"})
	s.RecordSelfUpdateFailure("local", "dockpaum", SelfUpdateFailure{ImageID: ""})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := open(t, path)
	f, ok := r.LastSelfUpdateFailure("local", "dockpaum")
	if !ok || f.ImageID != "sha256:bad" || f.Image != "example.com/dockpaum:latest" || !f.At.Equal(t0) {
		t.Fatalf("after a restart: %+v %v", f, ok)
	}
	if _, ok := r.LastSelfUpdateFailure("other", "dockpaum"); ok {
		t.Fatal("failures are per host")
	}
	r.ClearSelfUpdateFailure("local", "dockpaum")
	if _, ok := r.LastSelfUpdateFailure("local", "dockpaum"); ok {
		t.Fatal("cleared after a successful self-update")
	}
}

func TestRetainHosts(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	for _, h := range []string{"a", "b", "c"} {
		s.PutContainerStatus(h, "web", json.RawMessage(`{}`))
	}
	s.MarkNotice(NoticeKey{Host: "c", Container: "web"}, "fp")
	s.RetainHosts([]string{"a", "b"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := keys(readFile(t, s.Path()).Hosts); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("hosts = %v", got)
	}
	if len(s.claims) != 0 {
		t.Fatal("claims of removed hosts must go")
	}
}

func TestTablesAreBounded(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	clock := t0
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < maxImageDefaults+5; i++ {
		s.PutImageDefaults("local", fmt.Sprintf("sha256:%05d", i), json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
	}
	h := s.doc.Hosts["local"]
	if len(h.ImageDefaults) != maxImageDefaults {
		t.Fatalf("image defaults = %d, want %d", len(h.ImageDefaults), maxImageDefaults)
	}
	if _, ok := h.ImageDefaults["sha256:00000"]; ok {
		t.Fatal("the oldest entry must be evicted first")
	}
	if _, ok := h.ImageDefaults[fmt.Sprintf("sha256:%05d", maxImageDefaults+4)]; !ok {
		t.Fatal("the newest entry must be kept")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := len(readFile(t, s.Path()).Hosts["local"].ImageDefaults); got != maxImageDefaults {
		t.Fatalf("file holds %d image defaults", got)
	}
}

func TestPendingChangesAreBounded(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	s, _ := open(t, PathIn(dir))
	readOnly(t, dir)
	for i := 0; i < maxPendingOps+10; i++ {
		s.PutContainerStatus("local", "web", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
	}
	if len(s.ops) > maxPendingOps {
		t.Fatalf("%d changes queued while the file is not writable", len(s.ops))
	}
	writable(t, dir)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"n":%d}`, maxPendingOps+9)
	if got := string(readFile(t, s.Path()).Hosts["local"].Containers["web"]); got != want {
		t.Fatalf("status = %s, want %s", got, want)
	}
}

func TestDelayedFlush(t *testing.T) {
	path := PathIn(t.TempDir())
	logger, _ := testLogger()
	s := OpenWith(path, Options{Logger: logger, FlushDelay: 10 * time.Millisecond})
	t.Cleanup(func() { _ = s.Close() })
	s.PutContainerStatus("local", "web", json.RawMessage(`{}`))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if buf, err := os.ReadFile(path); err == nil && bytes.Contains(buf, []byte(`"web"`)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the change was not written on its own")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrentUse(t *testing.T) {
	path := PathIn(t.TempDir())
	logger, _ := testLogger()
	a := OpenWith(path, Options{Logger: logger, FlushDelay: time.Millisecond})
	b := OpenWith(path, Options{Logger: logger, FlushDelay: time.Millisecond})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		s := a
		if w%2 == 1 {
			s = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := fmt.Sprintf("c%d-%d", w, i)
				s.PutContainerStatus("local", name, json.RawMessage(`{}`))
				s.PutImageDefaults("local", "sha256:"+name, json.RawMessage(`{}`))
				s.FirstSeen("local", "sha256:cand"+name, "repo", time.Now())
				k := NoticeKey{Notifier: "n", Event: "e", Host: "local", Container: name}
				if s.MarkNotice(k, "fp") {
					s.CommitNotice(k, "fp")
				}
				s.RecordPrevious("local", name, PreviousImage{Image: name})
				_, _ = s.ImageDefaults("local", "sha256:"+name)
				_ = s.ContainerStatuses()
				if i%10 == 0 {
					_ = s.Flush()
				}
			}
		}()
	}
	wg.Wait()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	h := readFile(t, path).Hosts["local"]
	if len(h.Containers) != 400 || len(h.Previous) != 400 || len(h.Notices) != 400 || len(h.FirstSeen) != 400 {
		t.Fatalf("records lost: %d statuses, %d previous, %d notices, %d first seen",
			len(h.Containers), len(h.Previous), len(h.Notices), len(h.FirstSeen))
	}
}

func keys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

func TestRedacted(t *testing.T) {
	s, _ := open(t, "")
	s.PutImageDefaults("local", "sha256:a", json.RawMessage(`{"Env":["PATH=/bin","TOKEN=s3cret","FLAG"],"Cmd":["serve"]}`))
	s.PutImageDefaults("local", "sha256:b", json.RawMessage(`{"Env":"not-a-list","Cmd":["x"]}`))
	s.PutImageDefaults("local", "sha256:c", json.RawMessage(`{"Cmd":["y"]}`))
	s.PutContainerStatus("local", "web", json.RawMessage(`{"decision":"updated"}`))
	out, err := s.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "s3cret") || strings.Contains(text, "/bin") || strings.Contains(text, "not-a-list") {
		t.Fatalf("environment values leaked:\n%s", text)
	}
	for _, want := range []string{"TOKEN=***", "PATH=***", `"FLAG"`, `"serve"`, `"y"`, `"decision": "updated"`} {
		if !strings.Contains(text, want) {
			t.Errorf("redacted view lacks %s:\n%s", want, text)
		}
	}
	if raw, _ := s.ImageDefaults("local", "sha256:a"); !strings.Contains(string(raw), "s3cret") {
		t.Fatal("redaction must not change the stored state")
	}
}
