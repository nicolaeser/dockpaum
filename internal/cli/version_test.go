package cli

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	vcs := &debug.BuildInfo{
		GoVersion: "go1.25.11",
		Main:      debug.Module{Path: "github.com/nicolaeser/dockpaum", Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-10-01T08:00:00Z"},
		},
	}
	installed := &debug.BuildInfo{GoVersion: "go1.25.11", Main: debug.Module{Version: "v0.0.0-20261025080000-0123456789ab"}}
	cases := []struct {
		name                string
		v, c, d             string
		bi                  *debug.BuildInfo
		wantV, wantC, wantD string
		wantGo              string
	}{
		{"ldflags win", "v2026.09.01.abc1234", "abc1234", "2026-09-01", vcs, "v2026.09.01.abc1234", "abc1234", "2026-09-01", "go1.25.11"},
		{"go build in a checkout", "dev", "unknown", "unknown", vcs, "dev", "0123456", "2026-10-01T08:00:00Z", "go1.25.11"},
		{"go install module@version", "dev", "unknown", "unknown", installed, "v0.0.0-20261025080000-0123456789ab", "unknown", "unknown", "go1.25.11"},
		{"no build info", "dev", "unknown", "unknown", nil, "dev", "unknown", "unknown", runtime.Version()},
	}
	for _, c := range cases {
		got := resolveVersion(c.v, c.c, c.d, c.bi)
		if got.Version != c.wantV || got.Commit != c.wantC || got.Date != c.wantD || got.Go != c.wantGo {
			t.Errorf("%s: got %+v", c.name, got)
		}
		if got.Platform != runtime.GOOS+"/"+runtime.GOARCH {
			t.Errorf("%s: platform = %q", c.name, got.Platform)
		}
	}
}
