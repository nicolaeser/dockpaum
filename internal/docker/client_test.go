package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestStripImageDefaults_Healthcheck(t *testing.T) {
	hcOld := &container.HealthConfig{Test: []string{"CMD", "true"}, Retries: 2}

	t.Run("inherited healthcheck is cleared so new image applies", func(t *testing.T) {
		fc := &FullContainer{Config: &container.Config{
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}, Retries: 2},
		}}
		fc.StripImageDefaults(&ImageDefaults{Healthcheck: hcOld})
		if fc.Config.Healthcheck != nil {
			t.Fatalf("inherited healthcheck not cleared: %+v", fc.Config.Healthcheck)
		}
	})

	t.Run("user healthcheck differing from image is kept", func(t *testing.T) {
		user := &container.HealthConfig{Test: []string{"CMD", "curl", "-f", "localhost"}}
		fc := &FullContainer{Config: &container.Config{Healthcheck: user}}
		fc.StripImageDefaults(&ImageDefaults{Healthcheck: hcOld})
		if fc.Config.Healthcheck == nil {
			t.Fatal("user healthcheck was wrongly cleared")
		}
	})

	t.Run("explicitly disabled (NONE) is kept", func(t *testing.T) {
		fc := &FullContainer{Config: &container.Config{Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}}}
		fc.StripImageDefaults(&ImageDefaults{Healthcheck: hcOld})
		if fc.Config.Healthcheck == nil || fc.Config.Healthcheck.Test[0] != "NONE" {
			t.Fatal("disabled healthcheck must be preserved")
		}
	})
}

func TestStripImageDefaults_EnvCmd(t *testing.T) {
	old := &ImageDefaults{
		Env:        []string{"PATH=/bin", "APP_VERSION=1"},
		Cmd:        []string{"run"},
		Entrypoint: []string{"/entry"},
		WorkingDir: "/app",
	}
	fc := &FullContainer{Config: &container.Config{
		Env:        []string{"PATH=/bin", "APP_VERSION=1", "USER_SET=keep"},
		Cmd:        []string{"run"},
		Entrypoint: []string{"/entry"},
		WorkingDir: "/app",
	}}
	fc.StripImageDefaults(old)
	if fc.Config.Cmd != nil {
		t.Errorf("Cmd should be cleared, got %v", fc.Config.Cmd)
	}
	if fc.Config.Entrypoint != nil {
		t.Errorf("Entrypoint should be cleared, got %v", fc.Config.Entrypoint)
	}
	if fc.Config.WorkingDir != "" {
		t.Errorf("WorkingDir should be cleared, got %q", fc.Config.WorkingDir)
	}
	if len(fc.Config.Env) != 1 || fc.Config.Env[0] != "USER_SET=keep" {
		t.Errorf("only user env should remain, got %v", fc.Config.Env)
	}
}

func TestStripImageDefaults_MoreFields(t *testing.T) {
	old := &ImageDefaults{
		Labels:       map[string]string{"org.label": "old", "vendor": "acme"},
		User:         "1000:1000",
		StopSignal:   "SIGTERM",
		ExposedPorts: network.PortSet{network.MustParsePort("80/tcp"): struct{}{}},
		Volumes:      map[string]struct{}{"/data": {}},
	}
	fc := &FullContainer{Config: &container.Config{
		Labels: map[string]string{
			"org.label":                  "old",
			"vendor":                     "acme",
			"dockpaum.policy":            "fast",
			"com.docker.compose.project": "proj",
		},
		User:         "1000:1000",
		StopSignal:   "SIGTERM",
		ExposedPorts: network.PortSet{network.MustParsePort("80/tcp"): struct{}{}},
		Volumes:      map[string]struct{}{"/data": {}},
	}}
	fc.StripImageDefaults(old)

	if fc.Config.User != "" {
		t.Errorf("inherited User should be cleared, got %q", fc.Config.User)
	}
	if fc.Config.StopSignal != "" {
		t.Errorf("inherited StopSignal should be cleared, got %q", fc.Config.StopSignal)
	}
	if fc.Config.ExposedPorts != nil {
		t.Errorf("inherited ExposedPorts should be cleared, got %v", fc.Config.ExposedPorts)
	}
	if fc.Config.Volumes != nil {
		t.Errorf("inherited Volumes should be cleared, got %v", fc.Config.Volumes)
	}
	if _, ok := fc.Config.Labels["org.label"]; ok {
		t.Error("inherited image label org.label should be dropped")
	}
	if fc.Config.Labels["dockpaum.policy"] != "fast" {
		t.Error("user label dockpaum.policy must be preserved")
	}
	if fc.Config.Labels["com.docker.compose.project"] != "proj" {
		t.Error("compose label must be preserved")
	}
}

func TestStripImageDefaults_PortsKeepBindingsAndUserAdditions(t *testing.T) {
	old := &ImageDefaults{
		ExposedPorts: network.PortSet{
			network.MustParsePort("80/tcp"): struct{}{},
		},
		Volumes: map[string]struct{}{
			"/data": {},
		},
	}
	fc := &FullContainer{
		Config: &container.Config{
			ExposedPorts: network.PortSet{
				network.MustParsePort("80/tcp"):  struct{}{},
				network.MustParsePort("443/tcp"): struct{}{},
				network.MustParsePort("81/tcp"):  struct{}{},
			},
			Volumes: map[string]struct{}{
				"/data":  {},
				"/cache": {},
			},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"}},
			},
		},
	}
	old.ExposedPorts[network.MustParsePort("81/tcp")] = struct{}{}

	fc.StripImageDefaults(old)

	if _, ok := fc.Config.ExposedPorts[network.MustParsePort("80/tcp")]; !ok {
		t.Error("published inherited port should be preserved")
	}
	if _, ok := fc.Config.ExposedPorts[network.MustParsePort("443/tcp")]; !ok {
		t.Error("user-added exposed port should be preserved")
	}
	if _, ok := fc.Config.ExposedPorts[network.MustParsePort("81/tcp")]; ok {
		t.Error("unpublished inherited port should be dropped")
	}
	if _, ok := fc.Config.Volumes["/data"]; ok {
		t.Error("inherited volume declaration should be dropped")
	}
	if _, ok := fc.Config.Volumes["/cache"]; !ok {
		t.Error("user-added volume declaration should be preserved")
	}
}

func TestStripImageDefaults_UserOverrideKept(t *testing.T) {
	old := &ImageDefaults{User: "1000:1000", StopSignal: "SIGTERM"}
	fc := &FullContainer{Config: &container.Config{User: "root", StopSignal: "SIGKILL"}}
	fc.StripImageDefaults(old)
	if fc.Config.User != "root" {
		t.Errorf("explicit User should be kept, got %q", fc.Config.User)
	}
	if fc.Config.StopSignal != "SIGKILL" {
		t.Errorf("explicit StopSignal should be kept, got %q", fc.Config.StopSignal)
	}
}

func TestPreserveVolumeMounts(t *testing.T) {
	insp := &container.InspectResponse{
		HostConfig: &container.HostConfig{},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "data-vol", Destination: "/data", RW: true},
			{Type: mount.TypeBind, Name: "", Destination: "/host", RW: true},
		},
	}
	preserveVolumeMounts(insp)
	if len(insp.HostConfig.Mounts) != 1 {
		t.Fatalf("expected 1 preserved mount, got %d", len(insp.HostConfig.Mounts))
	}
	m := insp.HostConfig.Mounts[0]
	if m.Source != "data-vol" || m.Target != "/data" || m.Type != mount.TypeVolume {
		t.Fatalf("unexpected preserved mount: %+v", m)
	}
}

func TestPreserveVolumeMounts_AlreadyCovered(t *testing.T) {
	insp := &container.InspectResponse{
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "data-vol", Target: "/data"}},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "data-vol", Destination: "/data", RW: true},
		},
	}
	preserveVolumeMounts(insp)
	if len(insp.HostConfig.Mounts) != 1 {
		t.Fatalf("must not duplicate an already-declared volume, got %d", len(insp.HostConfig.Mounts))
	}
}

func TestPreserveVolumeMounts_SourcelessMounts(t *testing.T) {
	tests := []struct {
		name    string
		mounts  []mount.Mount
		runtime []container.MountPoint
		want    []mount.Mount
	}{
		{
			name:    "anonymous volume gets the generated name",
			mounts:  []mount.Mount{{Type: mount.TypeVolume, Target: "/data", VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
			runtime: []container.MountPoint{{Type: mount.TypeVolume, Name: "3f2a9c", Destination: "/data", RW: true}},
			want:    []mount.Mount{{Type: mount.TypeVolume, Source: "3f2a9c", Target: "/data", VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
		},
		{
			name:    "read-only stays read-only",
			mounts:  []mount.Mount{{Type: mount.TypeVolume, Target: "/conf", ReadOnly: true}},
			runtime: []container.MountPoint{{Type: mount.TypeVolume, Name: "c0ffee", Destination: "/conf"}},
			want:    []mount.Mount{{Type: mount.TypeVolume, Source: "c0ffee", Target: "/conf", ReadOnly: true}},
		},
		{
			name:   "no runtime entry leaves the mount as declared",
			mounts: []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
			want:   []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
		},
		{
			name:    "a bind at the same target is not a volume name",
			mounts:  []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
			runtime: []container.MountPoint{{Type: mount.TypeBind, Source: "/srv/data", Destination: "/data"}},
			want:    []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
		},
		{
			name: "named and anonymous volumes side by side, image volume appended once",
			mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: "pgdata", Target: "/var/lib/postgresql/data"},
				{Type: mount.TypeVolume, Target: "/backup"},
			},
			runtime: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "pgdata", Destination: "/var/lib/postgresql/data", RW: true},
				{Type: mount.TypeVolume, Name: "anon-backup", Destination: "/backup", RW: true},
				{Type: mount.TypeVolume, Name: "anon-image", Destination: "/cache", RW: true},
			},
			want: []mount.Mount{
				{Type: mount.TypeVolume, Source: "pgdata", Target: "/var/lib/postgresql/data"},
				{Type: mount.TypeVolume, Source: "anon-backup", Target: "/backup"},
				{Type: mount.TypeVolume, Source: "anon-image", Target: "/cache"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			insp := &container.InspectResponse{
				HostConfig: &container.HostConfig{Mounts: tc.mounts},
				Mounts:     tc.runtime,
			}
			preserveVolumeMounts(insp)
			if !reflect.DeepEqual(insp.HostConfig.Mounts, tc.want) {
				t.Fatalf("mounts = %+v\nwant     %+v", insp.HostConfig.Mounts, tc.want)
			}
		})
	}
}

func TestStringSlicesEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := stringSlicesEqual(c.a, c.b); got != c.want {
			t.Errorf("stringSlicesEqual(%v,%v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEncodeAuth(t *testing.T) {
	s, err := encodeAuth(&config.RegistryAuth{Username: "u", Password: "p", Server: "reg.io"})
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Fatal("expected non-empty base64 auth")
	}
}

func decodeAuth(t *testing.T, s string) registry.AuthConfig {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	var cfg registry.AuthConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEncodeAuthPasswordFileAndScopedServer(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), ".pull-token")
	if err := os.WriteFile(tokenFile, []byte("tok-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := encodeAuth(&config.RegistryAuth{
		Server: "https://ghcr.io/acme/project-a", Username: "bot", PasswordFile: tokenFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAuth(t, s)
	if got.Username != "bot" || got.Password != "tok-a" || got.ServerAddress != "ghcr.io" {
		t.Fatalf("auth = {%q %q %q}, want {bot tok-a ghcr.io}", got.Username, got.Password, got.ServerAddress)
	}

	if _, err := encodeAuth(&config.RegistryAuth{
		Server: "ghcr.io", Username: "bot", PasswordFile: filepath.Join(t.TempDir(), "missing"),
	}); err == nil {
		t.Fatal("expected an error for an unreadable password_file")
	}
}

func TestOpenRejectsSSH(t *testing.T) {
	_, err := open(context.Background(), config.HostConfig{Name: "x", Endpoint: "ssh://user@host:22"})
	if err == nil || !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("expected ssh rejection error, got %v", err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	n, err := b.Write([]byte("abcdef"))
	if err != nil || n != 6 || b.String() != "abcd" {
		t.Fatalf("Write = %d, %v, %q", n, err, b.String())
	}
	_, _ = b.Write([]byte("gh"))
	if b.String() != "abcd" {
		t.Fatalf("buffer grew past its bound: %q", b.String())
	}
}
