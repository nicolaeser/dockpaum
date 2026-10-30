package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func writeHistory(t *testing.T, dir string, recs ...Record) {
	t.Helper()
	l, err := New(config.AuditConfig{Enabled: boolp(true), Dir: dir, Retention: config.Duration(100 * 365 * 24 * time.Hour)}, discard())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		l.Append(r)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func names(recs []Record) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Container+"@"+r.At.UTC().Format("2006-01-02"))
	}
	return strings.Join(out, " ")
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 10, 0, 0, 0, time.UTC) }
	writeHistory(t, dir,
		Record{At: day(8, 30), Container: "web", Decision: DecisionUpdated},
		Record{At: day(9, 2), Container: "api", Decision: DecisionSkipped},
		Record{At: day(9, 20), Container: "web", Decision: DecisionFailed},
		Record{At: day(10, 1), Container: "db", Decision: DecisionDryRun},
		Record{At: day(10, 5), Container: "web", Decision: DecisionUpdated},
	)
	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"everything, oldest first", Query{}, "web@2026-08-30 api@2026-09-02 web@2026-09-20 db@2026-10-01 web@2026-10-05"},
		{"one container", Query{Containers: []string{"web"}}, "web@2026-08-30 web@2026-09-20 web@2026-10-05"},
		{"several containers", Query{Containers: []string{"api", "db"}}, "api@2026-09-02 db@2026-10-01"},
		{"since inside a month", Query{Since: day(9, 10)}, "web@2026-09-20 db@2026-10-01 web@2026-10-05"},
		{"since a month boundary", Query{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, "db@2026-10-01 web@2026-10-05"},
		{"limit keeps the newest", Query{Limit: 2}, "db@2026-10-01 web@2026-10-05"},
		{"limit across months", Query{Limit: 4}, "api@2026-09-02 web@2026-09-20 db@2026-10-01 web@2026-10-05"},
		{"limit with container", Query{Containers: []string{"web"}, Limit: 2}, "web@2026-09-20 web@2026-10-05"},
		{"limit above total", Query{Limit: 50}, "web@2026-08-30 api@2026-09-02 web@2026-09-20 db@2026-10-01 web@2026-10-05"},
		{"no match", Query{Containers: []string{"nope"}}, ""},
		{"since after everything", Query{Since: day(12, 1)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Read(dir, c.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(res.Records); got != c.want {
				t.Errorf("records = %q, want %q", got, c.want)
			}
			if res.Malformed != 0 {
				t.Errorf("malformed = %d, want 0", res.Malformed)
			}
		})
	}
}

func TestReadSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	writeHistory(t, dir, Record{At: at, Container: "web", Decision: DecisionUpdated})
	path := filepath.Join(dir, "dockpaum-2026-10.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	long := `{"container":"` + strings.Repeat("x", maxRecordBytes+10) + `"}`
	good := fmt.Sprintf(`{"at":%q,"container":"api","decision":"skipped"}`, at.Add(time.Hour).Format(time.RFC3339))
	if _, err := f.WriteString("{\"at\":\"2026-10-0\n" + long + "\n{}\n\n   \n" + good); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	res, err := Read(dir, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Records); got != "web@2026-10-01 api@2026-10-01" {
		t.Errorf("records = %q", got)
	}
	if res.Malformed != 3 {
		t.Errorf("malformed = %d, want 3", res.Malformed)
	}
}

func TestReadIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "dockpaum-latest.jsonl", "dockpaum-2026-13.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("garbage\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "dockpaum-2026-01.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Read(dir, Query{})
	if err != nil || len(res.Records) != 0 || res.Malformed != 0 {
		t.Errorf("Read = %+v, %v; want nothing", res, err)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "missing"), Query{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dir: %v, want fs.ErrNotExist", err)
	}
	if _, err := Read(t.TempDir(), Query{Limit: -1}); err == nil {
		t.Error("negative limit should fail")
	}
	if os.Geteuid() != 0 {
		dir := t.TempDir()
		p := filepath.Join(dir, "dockpaum-2026-10.jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir, Query{}); err == nil {
			t.Error("an unreadable audit file must fail the read, not shorten the history")
		}
	}
}
