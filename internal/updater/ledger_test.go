package updater

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func TestNoticeLedgerKeepsDeliveredNoticesInTheState(t *testing.T) {
	path := state.PathIn(t.TempDir())
	st := durableState(t, path)
	l := NoticeLedger(st)
	k := notify.NoticeKey{Notifier: "ops", Event: notify.EventNoUpdate, Host: "nas", Container: "web"}
	if !l.Mark(k, "fp1") || l.Mark(k, "fp1") {
		t.Fatal("a claimed notice is not fresh again")
	}
	l.Commit(k, "fp1")
	other := notify.NoticeKey{Notifier: "ops", Event: notify.EventNoUpdate, Host: "nas", Container: "db"}
	if !l.Mark(other, "x") {
		t.Fatal("another container is fresh")
	}
	l.Unmark(other, "x")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	l = NoticeLedger(durableState(t, path))
	if l.Mark(k, "fp1") {
		t.Fatal("a delivered notice must stay suppressed after a restart")
	}
	if !l.Mark(other, "x") {
		t.Fatal("a released claim is not recorded")
	}
	if !l.Mark(k, "fp2") {
		t.Fatal("a changed notice is fresh")
	}
	l.Forget("nas", "web")
	if !l.Mark(k, "fp1") {
		t.Fatal("a forgotten container starts over")
	}
}

type hookSink struct {
	mu    sync.Mutex
	types []string
}

func (s *hookSink) count(typ string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.types {
		if t == typ {
			n++
		}
	}
	return n
}

func TestStandingNoticeIsNotRepeatedAfterARestart(t *testing.T) {
	sink := &hookSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		sink.mu.Lock()
		sink.types = append(sink.types, p.Type)
		sink.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	notifiers := []config.NotifierConfig{{Type: "webhook", URL: srv.URL, Events: []string{notify.EventAvailable}}}

	path := state.PathIn(t.TempDir())
	h := newFakeHost(t, "1.47", nil)
	h.u.deps.Notifier = notify.Build(notifiers, logger)
	h.restart(durableState(t, path), "")
	h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("mon", &container.Config{Image: appRef, Labels: map[string]string{"dockpaum.monitor-only": "true"}}, nil, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.u.deps.Notifier.Wait(5 * time.Second)
	if n := sink.count(notify.EventAvailable); n != 1 {
		t.Fatalf("%d update.available deliveries, want 1", n)
	}
	if err := h.u.state.Close(); err != nil {
		t.Fatal(err)
	}

	h.u.deps.Notifier = notify.Build(notifiers, logger)
	h.restart(durableState(t, path), "")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.u.deps.Notifier.Wait(5 * time.Second)
	if n := sink.count(notify.EventAvailable); n != 1 {
		t.Fatalf("%d update.available deliveries, want the notice delivered before the restart not to repeat", n)
	}
}
