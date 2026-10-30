package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/docker"
)

type scriptedState struct {
	states []docker.RuntimeState
	err    error
	calls  int
}

func (s *scriptedState) ContainerState(context.Context, string) (docker.RuntimeState, error) {
	s.calls++
	if s.err != nil {
		return docker.RuntimeState{}, s.err
	}
	i := s.calls - 1
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	return s.states[i], nil
}

const t0 = "2026-01-01T00:00:00.000000001Z"

func running(health string) docker.RuntimeState {
	return docker.RuntimeState{Status: "running", Running: true, StartedAt: t0, Health: health}
}

func TestHealthGate(t *testing.T) {
	restarting := docker.RuntimeState{Status: "restarting", Running: true, Restarting: true, RestartCount: 1, ExitCode: 1, StartedAt: t0}
	restartedOnce := running("")
	restartedOnce.RestartCount = 1
	restartedOnce.StartedAt = "2026-01-01T00:00:01Z"
	newStart := running("")
	newStart.StartedAt = "2026-01-01T00:00:02Z"
	exited := docker.RuntimeState{Status: "exited", ExitCode: 3, StartedAt: t0}
	oom := docker.RuntimeState{Status: "exited", ExitCode: 137, OOMKilled: true, StartedAt: t0}
	paused := running("")
	paused.Status = "paused"
	crashedBeforeInspect := running("")
	crashedBeforeInspect.RestartCount = 1

	tests := []struct {
		name    string
		started docker.RuntimeState
		polls   []docker.RuntimeState
		timeout time.Duration
		wantErr string
	}{
		{name: "no healthcheck, keeps running through the grace period", started: running(""), polls: []docker.RuntimeState{running("")}},
		{name: "crash loop under a restart policy at the first look", started: restarting, wantErr: "crash loop: exit code 1"},
		{name: "restart policy brings it back between polls", started: running(""), polls: []docker.RuntimeState{running(""), restartedOnce}, wantErr: "restarted 1 time(s)"},
		{name: "restarted between the start and the first inspect", started: crashedBeforeInspect, wantErr: "restarted 1 time(s)"},
		{name: "started again without a counted restart", started: running(""), polls: []docker.RuntimeState{newStart}, wantErr: "was restarted"},
		{name: "exits", started: running(""), polls: []docker.RuntimeState{exited}, wantErr: "exited before becoming healthy (exit code 3)"},
		{name: "out of memory", started: running(""), polls: []docker.RuntimeState{oom}, wantErr: "running out of memory"},
		{name: "paused", started: running(""), polls: []docker.RuntimeState{paused}, wantErr: "container is paused"},
		{name: "healthy before the grace period ends", started: running("starting"), polls: []docker.RuntimeState{running("healthy")}},
		{name: "unhealthy", started: running("starting"), polls: []docker.RuntimeState{running("unhealthy")}, wantErr: "unhealthy"},
		{name: "never healthy", started: running("starting"), polls: []docker.RuntimeState{running("starting")}, wantErr: "timed out"},
		{name: "healthcheck starting, then a crash loop", started: running("starting"),
			polls: []docker.RuntimeState{running("starting"), restarting}, wantErr: "crash loop"},
		{name: "healthy but restarted meanwhile", started: running("starting"),
			polls: []docker.RuntimeState{{Status: "running", Running: true, RestartCount: 2, StartedAt: "x", Health: "healthy"}}, wantErr: "restarted 2 time(s)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 300 * time.Millisecond
			}
			g := healthGate{timeout: timeout, graceCap: 40 * time.Millisecond, poll: 2 * time.Millisecond}
			src := &scriptedState{states: tc.polls}
			if len(src.states) == 0 {
				src.states = []docker.RuntimeState{tc.started}
			}
			err := g.wait(context.Background(), src, "id", tc.started)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestHealthGateGraceIsBoundedByTimeout(t *testing.T) {
	g := healthGate{timeout: 20 * time.Millisecond, graceCap: time.Hour, poll: 2 * time.Millisecond}
	start := time.Now()
	if err := g.wait(context.Background(), &scriptedState{states: []docker.RuntimeState{running("")}}, "id", running("")); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("grace not capped by the timeout: took %s", took)
	}
}

func TestHealthGateDisabledObservesNothing(t *testing.T) {
	src := &scriptedState{err: errors.New("must not be called")}
	if err := (healthGate{}).wait(context.Background(), src, "id", docker.RuntimeState{}); err != nil || src.calls != 0 {
		t.Fatalf("err = %v, calls = %d", err, src.calls)
	}
}

func TestHealthGateInspectErrorAndCancel(t *testing.T) {
	g := healthGate{timeout: time.Second, graceCap: 500 * time.Millisecond, poll: time.Millisecond}
	boom := errors.New("daemon gone")
	if err := g.wait(context.Background(), &scriptedState{err: boom}, "id", running("")); !errors.Is(err, boom) {
		t.Fatalf("inspect error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.poll = time.Hour
	if err := g.wait(ctx, &scriptedState{states: []docker.RuntimeState{running("")}}, "id", running("")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
