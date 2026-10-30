package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHostHookRunsWithTarget(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		PreUpdate: []string{"printf %s \"$DOCKPAUM_TARGET\" > " + out},
		Timeout:   config.Duration(10 * time.Second),
	}, discard())
	if err := r.Run(context.Background(), PhasePre, nil, Target{Container: "mytarget"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "mytarget" {
		t.Fatalf("hook output = %q, want %q", string(b), "mytarget")
	}
}

func TestDisabledHooksSkip(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		Disabled:  true,
		PreUpdate: []string{"echo x > " + out},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	if err := r.Run(context.Background(), PhasePre, nil, Target{Container: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("disabled hook should not have run")
	}
}

func TestPreHookFailureReturnsError(t *testing.T) {
	r := NewRunner(config.HookConfig{
		PreUpdate: []string{"exit 3"},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	if err := r.Run(context.Background(), PhasePre, nil, Target{Container: "t"}); err == nil {
		t.Fatal("expected error from failing pre-update hook")
	}
}

func TestFailureHookErrorDoesNotAbort(t *testing.T) {
	r := NewRunner(config.HookConfig{
		OnFailure: []string{"exit 1"},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	if err := r.Run(context.Background(), PhaseFailure, nil, Target{Container: "t"}); err != nil {
		t.Fatalf("on_failure hook error should not propagate, got %v", err)
	}
}

func TestNoHooksIsNoOp(t *testing.T) {
	r := NewRunner(config.HookConfig{}, discard())
	if err := r.Run(context.Background(), PhasePost, nil, Target{Container: "t"}); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestPolicyHooksRunAfterGlobalHooks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		PreUpdate: []string{"echo global >> " + out},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	err := r.Run(context.Background(), PhasePre, nil, Target{
		Container:   "db",
		PolicyHooks: config.PolicyHooks{PreUpdate: []string{"echo policy-$DOCKPAUM_TARGET >> " + out}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, out), "global\npolicy-db\n"; got != want {
		t.Fatalf("hook order output = %q, want %q", got, want)
	}
}

func TestPolicyHooksSkippedAfterGlobalPreFailure(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		PreUpdate: []string{"exit 1"},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	err := r.Run(context.Background(), PhasePre, nil, Target{
		Container:   "db",
		PolicyHooks: config.PolicyHooks{PreUpdate: []string{"echo ran > " + out}},
	})
	if err == nil {
		t.Fatal("expected the failing global pre_update hook to abort")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("policy pre_update hook must not run after a failed global one")
	}
}

func TestPolicyFailureHooksRunAfterGlobalFailureHookFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		OnFailure: []string{"exit 1"},
		Timeout:   config.Duration(5 * time.Second),
	}, discard())
	err := r.Run(context.Background(), PhaseFailure, nil, Target{
		Container:   "db",
		PolicyHooks: config.PolicyHooks{OnFailure: []string{"echo ran > " + out}},
	})
	if err != nil {
		t.Fatalf("on_failure must not propagate, got %v", err)
	}
	if got := readFile(t, out); got != "ran\n" {
		t.Fatalf("policy on_failure output = %q", got)
	}
}

func TestHookEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		PostUpdate: []string{`printf '%s|%s|%s|%s|%s|%s|%s' "$DOCKPAUM_PHASE" "$DOCKPAUM_TARGET" "$DOCKPAUM_HOST" "$DOCKPAUM_RUN_ID" "$DOCKPAUM_POLICY" "$DOCKPAUM_IMAGE" "$DOCKPAUM_NEW_IMAGE" > ` + out},
		Timeout:    config.Duration(5 * time.Second),
	}, discard())
	err := r.Run(context.Background(), PhasePost, nil, Target{
		RunID: "r1", Host: "local", Container: "db", Policy: "database",
		Image: "postgres:16.1", NewImage: "postgres:16.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "post_update|db|local|r1|database|postgres:16.1|postgres:16.2"
	if got := readFile(t, out); got != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

func TestRunStage(t *testing.T) {
	dir := t.TempDir()
	pre := filepath.Join(dir, "pre")
	post := filepath.Join(dir, "post")
	r := NewRunner(config.HookConfig{
		PreRun:  []string{`printf '%s %s' "$DOCKPAUM_PHASE" "$DOCKPAUM_POLICY" > ` + pre},
		PostRun: []string{`printf '%s %s %s %s' "$DOCKPAUM_PHASE" "$DOCKPAUM_SCANNED" "$DOCKPAUM_UPDATED" "$DOCKPAUM_FAILED" > ` + post},
		Timeout: config.Duration(5 * time.Second),
	}, discard())
	if err := r.RunStage(context.Background(), PhasePreRun, RunInfo{RunID: "r", Policy: "database"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RunStage(context.Background(), PhasePostRun, RunInfo{RunID: "r", Scanned: 5, Updated: 2, Failed: 1}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, pre); got != "pre_run database" {
		t.Errorf("pre_run env = %q", got)
	}
	if got := readFile(t, post); got != "post_run 5 2 1" {
		t.Errorf("post_run env = %q", got)
	}
}

func TestRunStageFailureReturnsError(t *testing.T) {
	r := NewRunner(config.HookConfig{
		PreRun:  []string{"exit 2"},
		Timeout: config.Duration(5 * time.Second),
	}, discard())
	if err := r.RunStage(context.Background(), PhasePreRun, RunInfo{}); err == nil {
		t.Fatal("expected error from failing pre_run hook")
	}
}

func TestRunStageDisabled(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{
		Disabled: true,
		PreRun:   []string{"echo x > " + out},
		Timeout:  config.Duration(5 * time.Second),
	}, discard())
	if err := r.RunStage(context.Background(), PhasePreRun, RunInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("disabled pre_run hook should not have run")
	}
}

func TestRunJob(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{Disabled: true, Timeout: config.Duration(5 * time.Second)}, discard())
	err := r.RunJob(context.Background(), config.JobConfig{
		Name: "nightly",
		Commands: []string{
			`printf '%s %s\n' "$DOCKPAUM_PHASE" "$DOCKPAUM_JOB" >> ` + out,
			"host: echo second >> " + out,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, out), "job nightly\nsecond\n"; got != want {
		t.Fatalf("job output = %q, want %q", got, want)
	}
}

func TestRunJobStopsAtFirstFailure(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := NewRunner(config.HookConfig{Timeout: config.Duration(5 * time.Second)}, discard())
	err := r.RunJob(context.Background(), config.JobConfig{
		Name:     "j",
		Commands: []string{"exit 4", "echo ran > " + out},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "job j") {
		t.Fatalf("expected job error, got %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("commands after a failure must not run")
	}
}

func TestRunJobTimeout(t *testing.T) {
	r := NewRunner(config.HookConfig{Timeout: config.Duration(time.Minute)}, discard())
	start := time.Now()
	err := r.RunJob(context.Background(), config.JobConfig{
		Name:     "slow",
		Commands: []string{"sleep 30"},
		Timeout:  config.Duration(200 * time.Millisecond),
	}, nil)
	if err == nil {
		t.Fatal("expected the job timeout to kill the command")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("job timeout not enforced, took %s", took)
	}
}

func TestExecHookWithoutDockerHostFails(t *testing.T) {
	r := NewRunner(config.HookConfig{Timeout: config.Duration(5 * time.Second)}, discard())
	err := r.RunJob(context.Background(), config.JobConfig{
		Name:     "j",
		Commands: []string{"exec:db:pg_dumpall"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "no connected docker host") {
		t.Fatalf("expected a missing-host error instead of a panic, got %v", err)
	}
}

func TestLabelHooksNeedOptIn(t *testing.T) {
	target := Target{Container: "db", ContainerID: "abc123", LabelPreUpdate: "pg_dump", LabelPostUpdate: "warm", LabelOnFailure: "alert", Running: true}
	off := NewRunner(config.HookConfig{}, discard())
	if h := off.labelHook(PhasePre, target); h != "" {
		t.Fatalf("label hooks ran without hooks.label_hooks: %q", h)
	}
	on := NewRunner(config.HookConfig{LabelHooks: true}, discard())
	for phase, want := range map[Phase]string{
		PhasePre:     "exec:abc123:pg_dump",
		PhasePost:    "exec:db:warm",
		PhaseFailure: "exec:abc123:alert",
	} {
		if got := on.labelHook(phase, target); got != want {
			t.Errorf("labelHook(%s) = %q, want %q", phase, got, want)
		}
	}
	if err := on.Run(context.Background(), PhasePre, nil, target); err == nil || !strings.Contains(err.Error(), "no connected docker host") {
		t.Fatalf("pre-update label hook without docker = %v", err)
	}
}

func TestLabelHooksSkipStoppedContainers(t *testing.T) {
	r := NewRunner(config.HookConfig{LabelHooks: true}, discard())
	stopped := Target{Container: "db", ContainerID: "abc", LabelPreUpdate: "pg_dump"}
	if h := r.labelHook(PhasePre, stopped); h != "" {
		t.Fatalf("label hook for a stopped container = %q, want none (nothing to exec into)", h)
	}
}

func TestSkipExitCode(t *testing.T) {
	r := NewRunner(config.HookConfig{PreUpdate: []string{"exit 75"}, Timeout: config.Duration(5 * time.Second)}, discard())
	err := r.Run(context.Background(), PhasePre, nil, Target{Container: "t"})
	if !IsSkipRequest(err) {
		t.Fatalf("exit 75 = %v, want a skip request", err)
	}
	r2 := NewRunner(config.HookConfig{PreUpdate: []string{"exit 3"}, Timeout: config.Duration(5 * time.Second)}, discard())
	if err := r2.Run(context.Background(), PhasePre, nil, Target{Container: "t"}); err == nil || IsSkipRequest(err) {
		t.Fatalf("exit 3 = %v, want a plain failure", err)
	}
	if !IsSkipRequest(&docker.ExitError{Code: 75}) {
		t.Fatal("exec exit 75 must be a skip request")
	}
}
