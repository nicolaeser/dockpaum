package cli

import (
	"path/filepath"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

func TestStatePath(t *testing.T) {
	off, on := false, true
	cases := []struct {
		cfg  config.StateConfig
		want string
	}{
		{config.StateConfig{}, filepath.Join(state.DefaultDir, state.FileName)},
		{config.StateConfig{Enabled: &on}, filepath.Join(state.DefaultDir, state.FileName)},
		{config.StateConfig{Dir: "/data/state"}, "/data/state/state.json"},
		{config.StateConfig{Dir: "  "}, filepath.Join(state.DefaultDir, state.FileName)},
		{config.StateConfig{Enabled: &off, Dir: "/data/state"}, ""},
	}
	for _, c := range cases {
		if got := statePath(c.cfg); got != c.want {
			t.Errorf("statePath(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestStateLedgerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), state.FileName)
	key := notify.NoticeKey{Notifier: "ops", Event: notify.EventNoUpdate, Host: "local", Container: "web"}

	st := state.Open(path, discardLogger())
	l := stateLedger{st}
	if !l.Mark(key, "sha256:new") {
		t.Fatal("a new notice must be fresh")
	}
	if l.Mark(key, "sha256:new") {
		t.Error("a claimed notice must not be sent twice")
	}
	l.Commit(key, "sha256:new")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = state.Open(path, discardLogger())
	defer func() { _ = st.Close() }()
	l = stateLedger{st}
	if l.Mark(key, "sha256:new") {
		t.Error("a delivered notice was repeated after a restart")
	}
	if !l.Mark(key, "sha256:newer") {
		t.Error("a changed notice must be fresh")
	}
	l.Unmark(key, "sha256:newer")
	if !l.Mark(key, "sha256:newer") {
		t.Error("a released claim must be fresh again")
	}
	l.Forget("local", "web")
	if !l.Mark(key, "sha256:new") {
		t.Error("notices of an updated container must be fresh again")
	}
}

func TestUpdateAvailability(t *testing.T) {
	got := updateAvailability([]updater.ContainerStatus{
		{Host: "local", Container: "web", Policy: "default", Pending: true},
		{Host: "edge", Container: "db", Policy: "pinned"},
	})
	if len(got) != 2 || !got[0].Pending || got[0].Host != "local" || got[0].Container != "web" || got[0].Policy != "default" ||
		got[1].Pending || got[1].Container != "db" {
		t.Errorf("updateAvailability = %+v", got)
	}
	if got := updateAvailability(nil); got == nil || len(got) != 0 {
		t.Errorf("updateAvailability(nil) = %#v, want empty", got)
	}
}
