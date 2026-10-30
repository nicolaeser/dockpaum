package docker

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

func TestWaitExitAndLogsTail(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "job", Exit: func([]string) (int, string, bool) {
		return 3, "one\ntwo\nthree\n", true
	}})
	d.AddImage(dockertest.Image{ID: "svc"})
	done := d.Run("job", &container.Config{Image: "sha256:job"}, nil, nil)
	running := d.Run("svc", &container.Config{Image: "sha256:svc"}, nil, nil)
	c := testClient(t, d)
	ctx := context.Background()

	code, err := c.WaitExit(ctx, done, time.Second)
	if err != nil || code != 3 {
		t.Fatalf("WaitExit = %d, %v", code, err)
	}
	out, err := c.LogsTail(ctx, done, 2)
	if err != nil || out != "two\nthree\n" {
		t.Fatalf("LogsTail = %q, %v", out, err)
	}
	if _, err := c.WaitExit(ctx, running, 20*time.Millisecond); !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("a running container must time out, got %v", err)
	}
}

func TestOneShotKeepsOnlyNamedVolumes(t *testing.T) {
	binds := namedBinds([]string{"/var/run/docker.sock:/var/run/docker.sock:ro", "/anon", "vol:/data"})
	if !reflect.DeepEqual(binds, []string{"/var/run/docker.sock:/var/run/docker.sock:ro", "vol:/data"}) {
		t.Fatalf("binds = %q", binds)
	}
	mounts := namedMounts([]mount.Mount{
		{Type: mount.TypeVolume, Target: "/anon"},
		{Type: mount.TypeVolume, Source: "audit", Target: "/audit"},
		{Type: mount.TypeBind, Source: "/srv/cfg", Target: "/cfg"},
		{Type: mount.TypeTmpfs, Target: "/tmp"},
	})
	if len(mounts) != 3 || mounts[0].Source != "audit" {
		t.Fatalf("mounts = %+v", mounts)
	}
}

func TestCreateOneShotLikeNeedsAModel(t *testing.T) {
	d := dockertest.New(t, "1.47")
	c := testClient(t, d)
	if _, err := c.CreateOneShotLike(context.Background(), OneShot{Name: "x", Image: "busybox"}); err == nil {
		t.Fatal("expected an error without a container to model on")
	}
}

func TestOneShotStopTimeout(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "img"}, "example.com/app:1")
	model := d.Run("model", &container.Config{Image: "example.com/app:1"}, nil, nil)
	c := testClient(t, d)
	like, err := c.Inspect(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	for _, secs := range []int{0, 420} {
		name := "one-shot-" + strconv.Itoa(secs)
		if _, err := c.CreateOneShotLike(context.Background(), OneShot{Name: name, Image: "sha256:img", Like: like, StopTimeout: secs}); err != nil {
			t.Fatal(err)
		}
		got, _ := d.Inspect(name)
		switch {
		case secs == 0 && got.Config.StopTimeout != nil:
			t.Errorf("no stop timeout requested, got %d", *got.Config.StopTimeout)
		case secs > 0 && (got.Config.StopTimeout == nil || *got.Config.StopTimeout != secs):
			t.Errorf("stop timeout = %v, want %d", got.Config.StopTimeout, secs)
		}
	}
}

func TestListContainersNamesTheReferenceOfAStaleTag(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "v1"}, "example.com/app:1")
	d.Run("old", &container.Config{Image: "example.com/app:1"}, nil, nil)
	d.AddImage(dockertest.Image{ID: "v2"}, "example.com/app:1")
	d.Run("new", &container.Config{Image: "example.com/app:1"}, nil, nil)
	d.Run("by-id", &container.Config{Image: "sha256:v1"}, nil, nil)
	list, err := testClient(t, d).ListContainers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ct := range list {
		got[ct.Name] = ct.Image + " " + ct.ImageID
	}
	want := map[string]string{
		"old":   "example.com/app:1 sha256:v1",
		"new":   "example.com/app:1 sha256:v2",
		"by-id": "sha256:v1 sha256:v1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed = %v, want %v", got, want)
	}
}

func TestImageTag(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/app:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/app:1")
	c := testClient(t, d)
	if err := c.ImageTag(context.Background(), "sha256:old", "example.com/app:1"); err != nil {
		t.Fatal(err)
	}
	if got := d.ImageID("example.com/app:1"); got != "sha256:old" {
		t.Fatalf("tag points at %s", got)
	}
	if err := c.RemoveImageIfDangling(context.Background(), "sha256:new"); err != nil || d.HasImage("new") {
		t.Fatalf("dangling image not removed: %v", err)
	}
	if err := c.RemoveImageIfDangling(context.Background(), "sha256:old"); err != nil || !d.HasImage("old") {
		t.Fatalf("a tagged image must be kept: %v", err)
	}
}

func TestImageRepoDigests(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "pulled", RepoDigests: []string{"example.com/app@sha256:aa"}}, "example.com/app:1")
	d.AddImage(dockertest.Image{ID: "built"}, "local/app:dev")
	c := testClient(t, d)
	ctx := context.Background()
	if got, err := c.ImageRepoDigests(ctx, "example.com/app:1"); err != nil || !reflect.DeepEqual(got, []string{"example.com/app@sha256:aa"}) {
		t.Fatalf("pulled image: %v, %v", got, err)
	}
	if got, err := c.ImageRepoDigests(ctx, "sha256:built"); err != nil || len(got) != 0 {
		t.Fatalf("locally built image: %v, %v", got, err)
	}
	if _, err := c.ImageRepoDigests(ctx, "sha256:missing"); err == nil {
		t.Fatal("a missing image must be an error")
	}
}

func TestPinComposeImage(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   map[string]string
	}{
		{name: "compose container", labels: map[string]string{ComposeImageLabel: "sha256:old", "com.docker.compose.service": "web"},
			want: map[string]string{ComposeImageLabel: "sha256:new", "com.docker.compose.service": "web"}},
		{name: "not created by compose", labels: map[string]string{"team": "a"}, want: map[string]string{"team": "a"}},
		{name: "no labels", labels: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := &FullContainer{Config: &container.Config{Labels: tc.labels}}
			fc.PinComposeImage("sha256:new")
			if !reflect.DeepEqual(fc.Config.Labels, tc.want) {
				t.Fatalf("labels = %v, want %v", fc.Config.Labels, tc.want)
			}
		})
	}
	(&FullContainer{}).PinComposeImage("sha256:new")
}

func TestHealthGateCrashedReasons(t *testing.T) {
	started := RuntimeState{Status: "running", Running: true, StartedAt: "t0"}
	if err := Crashed(started, started); err != nil {
		t.Fatalf("a running container did not crash: %v", err)
	}
	st := started
	st.Running, st.Status, st.ExitCode, st.OOMKilled = false, "exited", 137, true
	if err := Crashed(started, st); err == nil || !strings.Contains(err.Error(), "exit code 137, killed for running out of memory") {
		t.Fatalf("err = %v", err)
	}
}
