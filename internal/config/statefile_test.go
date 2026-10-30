package config_test

import (
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func TestStatePathMatchesTheStatePackage(t *testing.T) {
	for _, s := range []config.StateConfig{{}, {Dir: config.DefaultStateDir}, {Dir: "/srv/dockpaum/state"}} {
		dir := s.Dir
		if dir == "" {
			dir = config.DefaultStateDir
		}
		if got, want := s.Path(), state.PathIn(dir); got != want {
			t.Errorf("%+v.Path() = %q, want %q (state.PathIn)", s, got, want)
		}
	}
}
