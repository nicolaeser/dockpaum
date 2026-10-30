package docker

import (
	"context"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

func TestRuntimeState(t *testing.T) {
	tests := []struct {
		name string
		insp container.InspectResponse
		want RuntimeState
	}{
		{name: "empty response", want: RuntimeState{}},
		{
			name: "no state",
			insp: container.InspectResponse{RestartCount: 2},
			want: RuntimeState{RestartCount: 2},
		},
		{
			name: "restart backoff keeps Running true",
			insp: container.InspectResponse{
				RestartCount: 3,
				State: &container.State{
					Status: container.StateRestarting, Running: true, Restarting: true,
					ExitCode: 137, OOMKilled: true, StartedAt: "2026-01-01T00:00:00Z",
				},
			},
			want: RuntimeState{
				Status: "restarting", Running: true, Restarting: true, RestartCount: 3,
				ExitCode: 137, OOMKilled: true, StartedAt: "2026-01-01T00:00:00Z",
			},
		},
		{
			name: "healthcheck status",
			insp: container.InspectResponse{
				State: &container.State{Status: container.StateRunning, Running: true, Health: &container.Health{Status: "starting"}},
			},
			want: RuntimeState{Status: "running", Running: true, Health: "starting"},
		},
		{
			name: "start error",
			insp: container.InspectResponse{
				State: &container.State{Status: container.StateExited, ExitCode: 127, Error: "exec: not found"},
			},
			want: RuntimeState{Status: "exited", ExitCode: 127, Error: "exec: not found"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeState(tc.insp); got != tc.want {
				t.Fatalf("runtimeState = %+v\nwant           %+v", got, tc.want)
			}
		})
	}
}

func TestContainerStateCrashLoop(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{CrashLoop: true}, "example.com/bad:1")
	d.Run("looping", &container.Config{Image: "example.com/bad:1"}, &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}, nil)
	d.Run("exited", &container.Config{Image: "example.com/bad:1"}, nil, nil)
	c := testClient(t, d)
	ctx := context.Background()

	st, err := c.ContainerState(ctx, "looping")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || !st.Restarting || st.RestartCount != 1 || st.Status != "restarting" {
		t.Fatalf("crash loop state = %+v", st)
	}
	status, running, err := c.ContainerHealth(ctx, "looping")
	if err != nil || status != "" || !running {
		t.Fatalf("ContainerHealth = %q %v %v", status, running, err)
	}
	st, err = c.ContainerState(ctx, "exited")
	if err != nil || st.Running || st.ExitCode != 1 || st.Status != "exited" {
		t.Fatalf("exited state = %+v, %v", st, err)
	}
	if _, err := c.ContainerState(ctx, "missing"); err == nil {
		t.Fatal("expected an error for a missing container")
	}
}

func TestForeignPlatformDecision(t *testing.T) {
	tests := []struct {
		name   string
		native string
		img    *ocispec.Platform
		want   string
	}{
		{"native image", "linux/amd64", &ocispec.Platform{OS: "linux", Architecture: "amd64"}, ""},
		{"native arm variant is left to the daemon", "linux/arm", &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}, ""},
		{"emulated amd64 on arm64", "linux/arm64", &ocispec.Platform{OS: "linux", Architecture: "amd64"}, "linux/amd64"},
		{"emulated arm/v7 on amd64 keeps the variant", "linux/amd64", &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "linux/arm/v7"},
		{"unknown architecture", "linux/amd64", &ocispec.Platform{OS: "linux"}, ""},
		{"no platform", "linux/amd64", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := foreignPlatform(tc.native, tc.img); got != tc.want {
				t.Fatalf("foreignPlatform = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsePlatform(t *testing.T) {
	for in, want := range map[string]*ocispec.Platform{
		"linux/amd64":    {OS: "linux", Architecture: "amd64"},
		"linux/arm/v7":   {OS: "linux", Architecture: "arm", Variant: "v7"},
		"":               nil,
		"linux":          nil,
		"/amd64":         nil,
		"linux/arm/v7/x": nil,
	} {
		got := parsePlatform(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsePlatform(%q) = %+v, want %+v", in, got, want)
		}
		if got != nil && formatPlatform(got) != in {
			t.Errorf("formatPlatform(parsePlatform(%q)) = %q", in, formatPlatform(got))
		}
	}
}

func TestForeignPlatformKeepsEmulatedImages(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.SetArch("arm64")
	d.AddImage(dockertest.Image{Architecture: "amd64"}, "example.com/legacy:1")
	d.AddImage(dockertest.Image{}, "example.com/native:1")
	d.Run("legacy", &container.Config{Image: "example.com/legacy:1"}, nil, nil)
	d.Run("native", &container.Config{Image: "example.com/native:1"}, nil, nil)
	d.Publish("example.com/legacy:1", dockertest.Image{Architecture: "arm64"})
	d.Publish("example.com/legacy:1", dockertest.Image{Architecture: "amd64"})
	c := testClient(t, d)
	ctx := context.Background()

	legacy, err := c.Inspect(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.ForeignPlatform(ctx, legacy)
	if err != nil || p != "linux/amd64" {
		t.Fatalf("ForeignPlatform(legacy) = %q, %v", p, err)
	}
	native, _ := c.Inspect(ctx, "native")
	if p, err := c.ForeignPlatform(ctx, native); err != nil || p != "" {
		t.Fatalf("ForeignPlatform(native) = %q, %v", p, err)
	}
	if n := len(d.Calls("GET", "/version")); n != 1 {
		t.Fatalf("the daemon platform must be cached, got %d version calls", n)
	}

	newID, err := c.PullImagePlatform(ctx, "example.com/legacy:1", nil, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if pulls := d.Calls("POST", "/images/create"); len(pulls) != 1 || pulls[0].Query.Get("platform") != "linux/amd64" {
		t.Fatalf("pull calls = %+v", pulls)
	}
	legacy.Platform = "linux/amd64"
	legacy.Config.Image = "example.com/legacy:1"
	id, err := c.CreateContainer(ctx, "legacy-new", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if q := d.Calls("POST", "/containers/create")[0].Query; q.Get("platform") != "linux/amd64" {
		t.Fatalf("create platform = %q", q.Get("platform"))
	}
	got, _ := d.Inspect(id)
	if got.Image != newID {
		t.Fatalf("replacement image %s, want the pulled amd64 image %s", got.Image, newID)
	}
}

func TestCreateContainerPlatformNeedsAPI141(t *testing.T) {
	d := dockertest.New(t, "1.40")
	d.AddImage(dockertest.Image{Architecture: "arm64"}, "example.com/app:1")
	d.Run("app", &container.Config{Image: "example.com/app:1"}, nil, nil)
	c := testClient(t, d)
	fc, err := c.Inspect(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	fc.Platform = "linux/arm64"
	if _, err := c.CreateContainer(context.Background(), "app-new", fc); err != nil {
		t.Fatalf("API 1.40 create with a platform: %v", err)
	}
	if q := d.Calls("POST", "/containers/create")[0].Query; q.Has("platform") {
		t.Fatalf("API 1.40 has no create platform, got %q", q.Get("platform"))
	}
}
