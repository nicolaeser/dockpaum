package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolvedImages(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	clock := t0
	s.now = func() time.Time { return clock }
	key := ResolvedKey("example.com/app", "sha256:idx", "linux/amd64")
	if _, ok := s.Resolved("local", key); ok {
		t.Fatal("nothing recorded yet")
	}
	want := ResolvedImage{
		ImageID: "sha256:cfg", Manifest: "sha256:man",
		Created: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.FixedZone("x", 3600)),
		Labels:  map[string]string{"org.opencontainers.image.version": "1.2.3"},
	}
	s.PutResolved("local", key, "example.com/app", want)
	s.PutResolved("local", "other", "example.com/app", ResolvedImage{})
	got, ok := s.Resolved("local", key)
	if !ok || got.ImageID != want.ImageID || got.Manifest != want.Manifest || !got.Created.Equal(want.Created) ||
		!reflect.DeepEqual(got.Labels, want.Labels) {
		t.Fatalf("Resolved = %+v, %v", got, ok)
	}
	got.Labels["x"] = "y"
	if again, _ := s.Resolved("local", key); again.Labels["x"] != "" {
		t.Fatal("the caller must get a copy of the labels")
	}
	if _, ok := s.Resolved("remote", key); ok {
		t.Fatal("resolved images are per host")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r, _ := open(t, path)
	r.now = func() time.Time { return clock }
	if got, ok := r.Resolved("local", key); !ok || got.ImageID != "sha256:cfg" {
		t.Fatalf("after a restart: %+v, %v", got, ok)
	}
	if len(r.ops) != 0 {
		t.Fatal("a use within the resolution must not queue a write")
	}
	clock = clock.Add(2 * time.Hour)
	r.Resolved("local", key)
	if len(r.ops) != 1 || !r.doc.Hosts["local"].Resolved[key].LastUsed.Equal(clock) {
		t.Fatal("a later use keeps the entry from expiring")
	}
}

func TestPruneResolvedImages(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	s.now = func() time.Time { return t0 }
	followed := ResolvedKey("example.com/app", "sha256:a", "linux/amd64")
	orphan := ResolvedKey("example.com/gone", "sha256:b", "linux/amd64")
	s.PutResolved("local", followed, "example.com/app", ResolvedImage{ImageID: "sha256:1"})
	s.PutResolved("local", orphan, "example.com/gone", ResolvedImage{ImageID: "sha256:2"})
	s.PruneHost("local", Live{Containers: []string{"web"}, Repos: []string{"example.com/app"}, ReposUnknown: true})
	if len(s.doc.Hosts["local"].Resolved) != 2 {
		t.Fatal("nothing is pruned by repository while a repository is unknown")
	}
	s.PruneHost("local", Live{Containers: []string{"web"}, Repos: []string{"example.com/app"}})
	if got := keys(s.doc.Hosts["local"].Resolved); !reflect.DeepEqual(got, []string{followed}) {
		t.Fatalf("resolved = %v", got)
	}
	s.now = func() time.Time { return t0.Add(firstSeenRetention + time.Hour) }
	s.PruneHost("local", Live{Containers: []string{"web"}, Repos: []string{"example.com/app"}})
	if len(s.doc.Hosts["local"].Resolved) != 0 {
		t.Fatal("an entry not used for a month is pruned")
	}
}

func TestResolvedImagesAreBounded(t *testing.T) {
	s, _ := open(t, PathIn(t.TempDir()))
	clock := t0
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < maxResolved+3; i++ {
		s.PutResolved("local", fmt.Sprintf("k%05d", i), "example.com/app", ResolvedImage{ImageID: "sha256:x"})
	}
	h := s.doc.Hosts["local"]
	if len(h.Resolved) != maxResolved {
		t.Fatalf("resolved = %d, want %d", len(h.Resolved), maxResolved)
	}
	if _, ok := h.Resolved["k00000"]; ok {
		t.Fatal("the least recently used entry is evicted first")
	}
}

func TestSnapshotWritesNothing(t *testing.T) {
	path := PathIn(t.TempDir())
	s, _ := open(t, path)
	s.now = func() time.Time { return t0 }
	s.FirstSeen("local", "sha256:cand", "example.com/app", t0)
	s.PutResolved("local", "k", "example.com/app", ResolvedImage{ImageID: "sha256:cfg"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	snap := OpenSnapshot(path, nil)
	if !snap.Durable() || !snap.Snapshot() || snap.Path() != path {
		t.Fatal("a snapshot reports the durability of the file it holds")
	}
	if got, ok := snap.LookupFirstSeen("local", "sha256:cand"); !ok || !got.Equal(t0) {
		t.Fatalf("first sighting %v, %v", got, ok)
	}
	if _, ok := snap.Resolved("local", "k"); !ok {
		t.Fatal("resolved images are read")
	}
	snap.FirstSeen("local", "sha256:new", "example.com/app", t0.Add(48*time.Hour))
	snap.PutContainerStatus("local", "web", json.RawMessage(`{}`))
	if _, ok := snap.LookupFirstSeen("local", "sha256:new"); !ok {
		t.Fatal("what a snapshot records is kept in memory")
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the snapshot wrote the file (%v)", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), temporaryFileMarker) {
			t.Fatalf("temporary file %s left", e.Name())
		}
	}

	if s := OpenSnapshot(PathIn(t.TempDir()), nil); !s.Durable() || len(s.doc.Hosts) != 0 {
		t.Fatal("a missing file gives an empty snapshot")
	}
	if s := OpenSnapshot("", nil); s.Durable() || s.Snapshot() {
		t.Fatal("without a path the store is in memory")
	}
	corrupt := PathIn(t.TempDir())
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := OpenSnapshot(corrupt, nil); len(s.doc.Hosts) != 0 {
		t.Fatal("an unreadable file gives an empty snapshot")
	}
	if b, _ := os.ReadFile(corrupt); string(b) != "{" {
		t.Fatal("a snapshot never moves or rewrites the file")
	}
}
