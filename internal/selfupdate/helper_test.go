package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/state"
)

func helperHost(t *testing.T, api string) (*dockertest.Daemon, *docker.Client) {
	t.Helper()
	d := dockertest.New(t, api)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cs, err := docker.OpenAll(context.Background(), []config.HostConfig{{Name: "h", Endpoint: d.Endpoint()}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Close)
	return d, cs.List()[0]
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fast(oldID, newImage, expectedID string) HelperOptions {
	o := DefaultHelperOptions(oldID, newImage, expectedID, 0)
	o.HealthTimeout = 300 * time.Millisecond
	o.HealthGrace = 20 * time.Millisecond
	o.HealthPoll = 2 * time.Millisecond
	return o
}

func callIndex(d *dockertest.Daemon, method, suffix string) int {
	for i, c := range d.Calls("", "") {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			return i
		}
	}
	return -1
}

func TestRunHelperKeepsStopTimeoutAndPlatform(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.SetArch("arm64")
	d.AddImage(dockertest.Image{ID: "old", Architecture: "amd64"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new", Architecture: "amd64"}, "example.com/dockpaum:2")
	grace := 400
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1", StopTimeout: &grace}, nil, nil)

	if err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", "sha256:new"), discard()); err != nil {
		t.Fatal(err)
	}
	if stops := d.Calls("POST", "/containers/"+oldID+"/stop"); len(stops) != 1 || stops[0].Query.Get("t") != "400" {
		t.Fatalf("stop calls = %+v, want t=400 (the container's own, longer than the helper minimum)", stops)
	}
	if q := d.Calls("POST", "/containers/create")[0].Query; q.Get("platform") != "linux/amd64" {
		t.Fatalf("create platform = %q", q.Get("platform"))
	}
	got, ok := d.Inspect("dockpaum")
	if !ok || got.Image != "sha256:new" || !got.State.Running {
		t.Fatalf("replacement = %+v", got)
	}
	if _, ok := d.Inspect(oldID); ok {
		t.Fatal("old daemon container must be removed")
	}
}

func TestRunHelperUsesRecordedDefaultsOfAVanishedImage(t *testing.T) {
	tests := []struct {
		name    string
		record  bool
		wantCmd []string
	}{
		{name: "recorded", record: true, wantCmd: []string{"--new"}},
		{name: "not recorded", wantCmd: []string{"--old"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, c := helperHost(t, "1.47")
			d.AddImage(dockertest.Image{ID: "old", Config: &container.Config{Cmd: []string{"--old"}}}, "example.com/dockpaum:latest")
			oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest"}, nil, nil)
			st := state.Open("", discard())
			if tc.record {
				raw, err := json.Marshal(docker.ImageDefaults{Cmd: []string{"--old"}})
				if err != nil {
					t.Fatal(err)
				}
				st.PutImageDefaults("h", "sha256:old", raw)
			}
			d.DropImage("old")
			d.AddImage(dockertest.Image{ID: "new", Config: &container.Config{Cmd: []string{"--new"}}}, "example.com/dockpaum:latest")

			o := fast(oldID, "example.com/dockpaum:latest", "sha256:new")
			o.RecordedDefaults = RecordedDefaults(st, "h")
			if err := RunHelperIfPresentWith(context.Background(), c, o, discard()); err != nil {
				t.Fatal(err)
			}
			got, _ := d.Inspect("dockpaum")
			if !slices.Equal(got.Config.Cmd, tc.wantCmd) {
				t.Fatalf("cmd = %q, want %q", got.Config.Cmd, tc.wantCmd)
			}
		})
	}
}

func TestRecordedDefaults(t *testing.T) {
	st := state.Open("", discard())
	st.PutImageDefaults("h", "sha256:a", []byte(`{"Cmd":["x"],"Env":["A=1"]}`))
	st.PutImageDefaults("h", "sha256:bad", []byte(`{"Cmd":"not-a-list"}`))
	lookup := RecordedDefaults(st, "h")
	if d, ok := lookup("sha256:a"); !ok || !slices.Equal(d.Cmd, []string{"x"}) || !slices.Equal(d.Env, []string{"A=1"}) {
		t.Fatalf("recorded = %+v %v", d, ok)
	}
	for _, id := range []string{"sha256:missing", "sha256:bad"} {
		if _, ok := lookup(id); ok {
			t.Fatalf("%s must not be found", id)
		}
	}
	if _, ok := RecordedDefaults(st, "other")("sha256:a"); ok {
		t.Fatal("defaults are per host")
	}
	if _, ok := RecordedDefaults(nil, "h")("sha256:a"); ok {
		t.Fatal("no state, no defaults")
	}
}

func TestRunHelperIfPresentWithAnotherHost(t *testing.T) {
	_, c := helperHost(t, "1.47")
	err := RunHelperIfPresentWith(context.Background(), c, fast("0123456789ab", "example.com/dockpaum:2", ""), discard())
	if !errors.Is(err, ErrNotFoundOnHost) {
		t.Fatalf("err = %v, want ErrNotFoundOnHost", err)
	}
}

func TestRunHelperStopTimeoutCoversTheDaemonDrain(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	if err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", ""), discard()); err != nil {
		t.Fatal(err)
	}
	if stops := d.Calls("POST", "/containers/"+oldID+"/stop"); len(stops) != 1 || stops[0].Query.Get("t") != "300" {
		t.Fatalf("stop calls = %+v, want t=300 (drain 60s + recovery grace 3m)", stops)
	}
}

func TestRunHelperRenamesBeforeStopAndPinsComposeImage(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:latest")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest", Labels: map[string]string{
		docker.ComposeImageLabel: "sha256:old", "com.docker.compose.service": "dockpaum",
	}}, nil, nil)
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:latest")

	if err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:latest", "sha256:new"), discard()); err != nil {
		t.Fatal(err)
	}
	rename, stop := callIndex(d, "POST", "/containers/"+oldID+"/rename"), callIndex(d, "POST", "/containers/"+oldID+"/stop")
	if rename < 0 || stop < 0 || rename > stop {
		t.Fatalf("the old daemon must be renamed before it is stopped (rename %d, stop %d)", rename, stop)
	}
	got, _ := d.Inspect("dockpaum")
	if got.Config.Labels[docker.ComposeImageLabel] != "sha256:new" {
		t.Fatalf("compose image label = %q, want the new image ID", got.Config.Labels[docker.ComposeImageLabel])
	}
}

func TestRunHelperRemovesIncompleteReplacement(t *testing.T) {
	d, c := helperHost(t, "1.43")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, &container.HostConfig{NetworkMode: "front"},
		map[string]*network.EndpointSettings{"front": {}, "back": {}})
	d.FailNetwork("back")

	err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", "sha256:new"), discard())
	if err == nil || !strings.Contains(err.Error(), "create replacement") || !strings.Contains(err.Error(), "old daemon was restored") {
		t.Fatalf("err = %v", err)
	}
	got, ok := d.Inspect("dockpaum")
	if !ok || got.ID != oldID || !got.State.Running {
		t.Fatalf("old daemon not restored: %+v", got)
	}
	if names := d.Names(); len(names) != 1 {
		t.Fatalf("incomplete replacement left behind: %v", names)
	}
}

func TestRunHelperRevertsAFailingDaemon(t *testing.T) {
	tests := []struct {
		name    string
		img     dockertest.Image
		hc      *container.HostConfig
		wantErr string
	}{
		{name: "unhealthy", img: dockertest.Image{ID: "new", Health: "unhealthy"}, wantErr: "unhealthy"},
		{name: "crash loop under a restart policy", img: dockertest.Image{ID: "new", CrashLoop: true},
			hc: &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}}, wantErr: "crash loop"},
		{name: "exits", img: dockertest.Image{ID: "new", CrashLoop: true}, wantErr: "exited"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, c := helperHost(t, "1.47")
			d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:latest")
			oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest"}, tc.hc, nil)
			d.AddImage(tc.img, "example.com/dockpaum:latest")

			err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:latest", "sha256:new"), discard())
			if err == nil || !strings.Contains(err.Error(), "health check") || !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), "the old daemon was restored") {
				t.Fatalf("err = %v", err)
			}
			got, ok := d.Inspect("dockpaum")
			if !ok || got.ID != oldID || !got.State.Running {
				t.Fatalf("old daemon not restored under its name: %+v", got)
			}
			if names := d.Names(); len(names) != 1 {
				t.Fatalf("failed replacement left behind: %v", names)
			}
			if id := d.ImageID("example.com/dockpaum:latest"); id != "sha256:old" {
				t.Fatalf("tag points at %s, want the old daemon's image again", id)
			}
		})
	}
}

func TestRunHelperStopFailureRestoresTheOldDaemon(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	d.FailNext("POST", "/containers/"+oldID+"/stop", "stop timed out")

	err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", ""), discard())
	if err == nil || !strings.Contains(err.Error(), "stop old") {
		t.Fatalf("err = %v", err)
	}
	if got, ok := d.Inspect("dockpaum"); !ok || got.ID != oldID || !got.State.Running {
		t.Fatal("old daemon must keep its name and run")
	}
	if n := len(d.Calls("POST", "/containers/create")); n != 0 {
		t.Fatalf("no replacement may be created after a failed stop, got %d", n)
	}
}

func TestRunHelperRenameFailureTouchesNothing(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	d.FailNext("POST", "/containers/"+oldID+"/rename", "rename refused")

	err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", ""), discard())
	if err == nil || !strings.Contains(err.Error(), "keeps running") {
		t.Fatalf("err = %v", err)
	}
	if n := len(d.Calls("POST", "/stop")); n != 0 {
		t.Fatalf("the old daemon must not be stopped when the rename failed, got %d stops", n)
	}
	if got, _ := d.Inspect(oldID); !got.State.Running || got.Name != "/dockpaum" {
		t.Fatalf("old daemon changed: %+v", got)
	}
}

func TestRunHelperRefusesAMovedTag(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "other"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	err := RunHelperWith(context.Background(), c, fast(oldID, "example.com/dockpaum:2", "sha256:new"), discard())
	if err == nil || !strings.Contains(err.Error(), "refusing self-update") {
		t.Fatalf("err = %v", err)
	}
	if n := len(d.Calls("POST", "/rename")) + len(d.Calls("POST", "/stop")); n != 0 {
		t.Fatal("nothing may be touched")
	}
}

func selfContainer(d *dockertest.Daemon) string {
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	return d.Run("dockpaum", &container.Config{
		Image: "example.com/dockpaum:1",
		Cmd:   []string{"--config", "/config/dockpaum.yml"},
		Env:   []string{"DOCKER_HOST=tcp://socket-proxy:2375", "DISCORD_URL=https://example.com/hook"},
		User:  "65532:65532",
		Labels: map[string]string{
			"com.docker.compose.project": "ops", "com.docker.compose.service": "dockpaum",
		},
	}, &container.HostConfig{
		Binds:       []string{"/srv/dockpaum:/config:ro", "/scratch"},
		Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: "dockpaum-audit", Target: "/var/lib/dockpaum/audit"}},
		GroupAdd:    []string{"999"},
		NetworkMode: "ops_proxy",
		ExtraHosts:  []string{"registry.local:10.0.0.5"},
		SecurityOpt: []string{"label=disable"},
	}, map[string]*network.EndpointSettings{
		"ops_proxy": {Aliases: []string{"dockpaum"}, IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("172.30.0.10")}},
	})
}

func TestSpawnReplicatesTheDaemonsDockerAccess(t *testing.T) {
	d, c := helperHost(t, "1.47")
	selfID := selfContainer(d)
	self, err := c.Inspect(context.Background(), selfID)
	if err != nil {
		t.Fatal(err)
	}
	name, err := Spawn(context.Background(), c, SpawnOptions{
		Self: self, ConfigPath: ConfigArg(self), NewImage: "example.com/dockpaum:2", ExpectedID: "sha256:new", Delay: 3 * time.Second,
	}, discard())
	if err != nil {
		t.Fatal(err)
	}
	h, ok := d.Inspect(name)
	if !ok || !h.State.Running {
		t.Fatalf("helper %s not running", name)
	}
	if h.HostConfig.AutoRemove {
		t.Fatal("the helper must be kept until its outcome is recorded")
	}
	wantCmd := []string{"--self-update-helper", selfID, "--new-image", "example.com/dockpaum:2", "--delay", "3",
		"--expect-id", "sha256:new", "--config", "/config/dockpaum.yml"}
	if !slices.Equal(h.Config.Cmd, wantCmd) {
		t.Fatalf("cmd = %q", h.Config.Cmd)
	}
	if !slices.Equal(h.HostConfig.Binds, []string{"/srv/dockpaum:/config:ro"}) {
		t.Errorf("binds = %q, want the named ones only", h.HostConfig.Binds)
	}
	if len(h.HostConfig.Mounts) != 1 || h.HostConfig.Mounts[0].Source != "dockpaum-audit" {
		t.Errorf("mounts = %+v", h.HostConfig.Mounts)
	}
	if !slices.Contains(h.Config.Env, "DOCKER_HOST=tcp://socket-proxy:2375") || h.Config.User != "65532:65532" ||
		!slices.Equal(h.HostConfig.GroupAdd, []string{"999"}) || !slices.Equal(h.HostConfig.SecurityOpt, []string{"label=disable"}) ||
		!slices.Equal(h.HostConfig.ExtraHosts, []string{"registry.local:10.0.0.5"}) {
		t.Errorf("access not replicated: env %q user %q groups %q secopt %q hosts %q",
			h.Config.Env, h.Config.User, h.HostConfig.GroupAdd, h.HostConfig.SecurityOpt, h.HostConfig.ExtraHosts)
	}
	ep, ok := h.NetworkSettings.Networks["ops_proxy"]
	if !ok || h.HostConfig.NetworkMode != "ops_proxy" {
		t.Fatalf("helper networks = %v mode %q", h.NetworkSettings.Networks, h.HostConfig.NetworkMode)
	}
	if slices.Contains(ep.Aliases, "dockpaum") || ep.IPAMConfig != nil {
		t.Errorf("the helper must not take the daemon's alias or address: %+v", ep)
	}
	if _, ok := h.Config.Labels["com.docker.compose.service"]; ok {
		t.Error("the helper must not look like a Compose service container")
	}
	if h.Config.Labels["dockpaum.enable"] != "false" || h.Config.Labels[HelperLabel] != selfID ||
		h.Config.Labels[HelperContainerLabel] != "dockpaum" || h.Config.Labels[HelperImageLabel] != "example.com/dockpaum:2" ||
		h.Config.Labels[HelperImageIDLabel] != "sha256:new" {
		t.Errorf("labels = %v", h.Config.Labels)
	}
	if h.Config.StopTimeout == nil || *h.Config.StopTimeout != helperStopTimeout+helperRevertGrace {
		t.Errorf("helper stop timeout = %v, want %d", h.Config.StopTimeout, helperStopTimeout+helperRevertGrace)
	}
}

func TestSpawnRefusesWhileAHelperRuns(t *testing.T) {
	d, c := helperHost(t, "1.47")
	selfID := selfContainer(d)
	d.Run("dockpaum-self-update-1", &container.Config{Image: "example.com/dockpaum:2", Labels: map[string]string{HelperLabel: selfID}}, nil, nil)
	self, _ := c.Inspect(context.Background(), selfID)
	_, err := Spawn(context.Background(), c, SpawnOptions{Self: self, NewImage: "example.com/dockpaum:2"}, discard())
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v", err)
	}
}

func TestSpawnRemovesAHelperThatCannotStart(t *testing.T) {
	d, c := helperHost(t, "1.47")
	selfID := selfContainer(d)
	self, _ := c.Inspect(context.Background(), selfID)
	d.FailNext("POST", "/start", "no such file")
	if _, err := Spawn(context.Background(), c, SpawnOptions{Self: self, NewImage: "example.com/dockpaum:2"}, discard()); err == nil ||
		!strings.Contains(err.Error(), "start helper") {
		t.Fatalf("err = %v", err)
	}
	if names := d.Names(); len(names) != 1 {
		t.Fatalf("containers = %v, want only the daemon", names)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		out     string
		wantErr string
		wantLog string
		secrets []string
	}{
		{name: "accepted", code: 0, out: "config OK: /config/dockpaum.yml\n"},
		{name: "rejected", code: 2, out: "config: parse /config/dockpaum.yml: line 3: field retention not found in type config.AuditConfig\n",
			wantErr: "rejects the configuration (exit 2)", wantLog: "field retention not found"},
		{name: "remote configuration with credentials", code: 1, // #nosec G101 -- a made-up credential the test expects to be redacted
			out:     "config: fetch https://ops:s3cr3t@config.example.com/dockpaum.yml?X-Amz-Signature=abc123: unexpected status 403\n",
			wantErr: "rejects the configuration (exit 1)", wantLog: "config: fetch https://config.example.com/***: unexpected status 403",
			secrets: []string{"s3cr3t", "abc123", "ops:"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, c := helperHost(t, "1.47")
			selfID := selfContainer(d)
			var got []string
			d.AddImage(dockertest.Image{ID: "checked", Exit: func(cmd []string) (int, string, bool) {
				got = cmd
				return tc.code, tc.out, true
			}})
			self, _ := c.Inspect(context.Background(), selfID)
			var logs syncBuffer
			err := ValidateConfig(context.Background(), c, self, "sha256:checked", ConfigArg(self), "", slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, ErrConfigRejected)) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), strings.TrimSpace(tc.out)) {
				t.Fatalf("the check's output must stay out of the error, which reaches audit records and notifications: %v", err)
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("log = %s, want it to contain %q", logs.String(), tc.wantLog)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(logs.String(), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Fatalf("%q leaked: err %v, log %s", secret, err, logs.String())
				}
			}
			if !slices.Equal(got, []string{"--validate", "--config", "/config/dockpaum.yml"}) {
				t.Fatalf("command = %q", got)
			}
			if names := d.Names(); len(names) != 1 {
				t.Fatalf("the check container must be removed: %v", names)
			}
		})
	}
}

func TestCollect(t *testing.T) {
	d, c := helperHost(t, "1.47")
	exits := func(code int, out string) func([]string) (int, string, bool) {
		return func([]string) (int, string, bool) { return code, out, true }
	}
	d.AddImage(dockertest.Image{ID: "ok", Exit: exits(0, `{"msg":"self-update complete"}`+"\n")})
	d.AddImage(dockertest.Image{ID: "bad", Exit: exits(1, `{"level":"ERROR","msg":"self-update failed"}`+"\n"+
		"self-update helper: new daemon failed its health check: container is unhealthy; the old daemon was restored\n")})
	d.AddImage(dockertest.Image{ID: "busy"})
	labels := func(old string) map[string]string {
		return map[string]string{HelperLabel: old, HelperContainerLabel: "dockpaum", HelperImageLabel: "example.com/dockpaum:2",
			HelperImageIDLabel: "sha256:two"}
	}
	d.Run("helper-ok", &container.Config{Image: "sha256:ok", Labels: labels("a")}, nil, nil)
	d.Run("helper-bad", &container.Config{Image: "sha256:bad", Labels: labels("b")}, nil, nil)
	d.Run("helper-busy", &container.Config{Image: "sha256:busy", Labels: labels("c")}, nil, nil)
	d.Create("helper-never", &container.Config{Image: "sha256:busy", Labels: labels("d")}, nil)
	d.Run("stale-check", &container.Config{Image: "sha256:ok", Labels: map[string]string{CheckLabel: "a"}}, nil, nil)
	d.Run("unrelated", &container.Config{Image: "sha256:busy"}, nil, nil)

	d.Run("helper-other-scope", &container.Config{Image: "sha256:ok", Labels: map[string]string{
		HelperLabel: "e", HelperContainerLabel: "dockpaum-blue", "dockpaum.scope": "blue",
	}}, nil, nil)

	outcomes, running, err := Collect(context.Background(), c, "", discard())
	if err != nil {
		t.Fatal(err)
	}
	if running != 1 {
		t.Fatalf("running = %d", running)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	for _, o := range outcomes {
		switch o.Helper {
		case "helper-ok":
			if o.Err != nil || o.Container != "dockpaum" || o.Image != "example.com/dockpaum:2" || o.ImageID != "sha256:two" || o.OldID != "a" {
				t.Errorf("ok outcome = %+v", o)
			}
		case "helper-bad":
			if o.Err == nil || o.ExitCode != 1 || !strings.Contains(o.Err.Error(), "exited 1: new daemon failed its health check") ||
				!strings.Contains(o.Err.Error(), "old daemon was restored") {
				t.Errorf("bad outcome = %+v (%v)", o, o.Err)
			}
		default:
			t.Errorf("unexpected outcome %+v", o)
		}
	}
	if got := d.Names(); !slices.Equal(got, []string{"helper-busy", "helper-other-scope", "unrelated"}) {
		t.Fatalf("containers left = %v (another instance's helper must be left to it)", got)
	}
	if n, err := Running(context.Background(), c, ""); err != nil || n != 1 {
		t.Fatalf("Running = %d, %v", n, err)
	}
	if outcomes, _, _ := Collect(context.Background(), c, "blue", discard()); len(outcomes) != 1 || outcomes[0].Container != "dockpaum-blue" {
		t.Fatalf("scoped outcomes = %+v", outcomes)
	}
}

func TestPeekChangesNothing(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "ok", Exit: func([]string) (int, string, bool) { return 0, "", true }})
	d.AddImage(dockertest.Image{ID: "bad", Exit: func([]string) (int, string, bool) {
		return 1, "self-update helper: start replacement: boom\n", true
	}})
	d.AddImage(dockertest.Image{ID: "busy"})
	labels := func(old string) map[string]string {
		return map[string]string{HelperLabel: old, HelperContainerLabel: "dockpaum"}
	}
	d.Run("helper-ok", &container.Config{Image: "sha256:ok", Labels: labels("a")}, nil, nil)
	d.Run("helper-bad", &container.Config{Image: "sha256:bad", Labels: labels("b")}, nil, nil)
	d.Run("helper-busy", &container.Config{Image: "sha256:busy", Labels: labels("c")}, nil, nil)
	d.Create("helper-never", &container.Config{Image: "sha256:busy", Labels: labels("d")}, nil)
	d.Run("stale-check", &container.Config{Image: "sha256:ok", Labels: map[string]string{CheckLabel: "a"}}, nil, nil)
	names := d.Names()
	before := len(d.Calls("", ""))

	outcomes, running, err := Peek(context.Background(), c, "", discard())
	if err != nil || running != 1 || len(outcomes) != 2 {
		t.Fatalf("outcomes %+v, running %d, err %v", outcomes, running, err)
	}
	for _, call := range d.Calls("", "")[before:] {
		if call.Method != "GET" {
			t.Fatalf("Peek changed Docker: %s %s", call.Method, call.Path)
		}
	}
	if got := d.Names(); !slices.Equal(got, names) {
		t.Fatalf("containers %v, want %v", got, names)
	}
	if outcomes, _, err := Collect(context.Background(), c, "", discard()); err != nil || len(outcomes) != 2 {
		t.Fatalf("outcomes after a peek = %+v, %v", outcomes, err)
	}
}

func TestCollectReportsAnOutcomeOnce(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "bad", Exit: func([]string) (int, string, bool) {
		return 1, "self-update helper: start replacement: boom\n", true
	}})
	id := d.Run("helper", &container.Config{Image: "sha256:bad", Labels: map[string]string{HelperLabel: "a", HelperContainerLabel: "dockpaum"}}, nil, nil)
	d.FailNext("DELETE", "/containers/"+id, "removal of container in progress")
	if outcomes, _, err := Collect(context.Background(), c, "", discard()); err != nil || len(outcomes) != 0 {
		t.Fatalf("a helper that could not be removed must not be reported yet: %+v %v", outcomes, err)
	}
	outcomes, _, err := Collect(context.Background(), c, "", discard())
	if err != nil || len(outcomes) != 1 || outcomes[0].Err == nil {
		t.Fatalf("outcomes = %+v, %v", outcomes, err)
	}
	if outcomes, _, _ := Collect(context.Background(), c, "", discard()); len(outcomes) != 0 {
		t.Fatalf("an outcome is reported once: %+v", outcomes)
	}
}

func TestRedact(t *testing.T) {
	tests := map[string]string{ // #nosec G101 -- made-up credentials the test expects to be redacted
		"config: fetch https://ops:pw@host.example/c.yml?token=abc: 403": "config: fetch https://host.example/***: 403",
		"config url \"http://host.example\" must use https":              "config url \"http://host.example\" must use https",
		"webhook https://hooks.slack.com/services/T0/B0/XYZ refused":     "webhook https://hooks.slack.com/*** refused",
		"DOCKPAUM_TOKEN=abc DOCKER_HOST=tcp://proxy:2375 level=ERROR":    "DOCKPAUM_TOKEN=*** DOCKER_HOST=*** level=ERROR",
		"line 4: cannot unmarshal !!str `hunter2` into time.Duration":    "line 4: cannot unmarshal !!str `***` into time.Duration",
		"field retention not found in type config.AuditConfig":           "field retention not found in type config.AuditConfig",
	}
	for in, want := range tests {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunHelperWaitsForAnInterruptedStop(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "new"}, "example.com/dockpaum:2")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	begun := d.SlowStop(oldID, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-begun
		cancel()
	}()
	err := RunHelperWith(ctx, c, fast(oldID, "example.com/dockpaum:2", ""), discard())
	if err == nil || !strings.Contains(err.Error(), "stop old") || !strings.Contains(err.Error(), "the old daemon was restored") {
		t.Fatalf("err = %v", err)
	}
	if n := len(d.Calls("POST", "/containers/"+oldID+"/start")); n != 1 {
		t.Fatalf("the old daemon was started %d time(s), want once after its stop ended", n)
	}
	got, ok := d.Inspect("dockpaum")
	if !ok || got.ID != oldID || !got.State.Running {
		t.Fatal("the old daemon must run again under its name once its interrupted stop ended")
	}
}

func TestRunHelperInterruptedDuringTheHealthGate(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "old"}, "example.com/dockpaum:latest")
	oldID := d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:latest"}, nil, nil)
	d.AddImage(dockertest.Image{ID: "new", Health: "starting"}, "example.com/dockpaum:latest")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for len(d.Calls("POST", "/start")) == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
	}()
	o := fast(oldID, "example.com/dockpaum:latest", "sha256:new")
	o.HealthTimeout = time.Minute
	err := RunHelperWith(ctx, c, o, discard())
	if err == nil || !strings.Contains(err.Error(), "the old daemon was restored") {
		t.Fatalf("err = %v", err)
	}
	var newStop *dockertest.Call
	for _, call := range d.Calls("POST", "/stop") {
		if !strings.Contains(call.Path, oldID) {
			newStop = &call
		}
	}
	if newStop == nil || newStop.Query.Get("t") != "60" {
		t.Fatalf("the new daemon must get its drain only, not the full stop timeout: %+v", newStop)
	}
	if got, ok := d.Inspect("dockpaum"); !ok || got.ID != oldID || !got.State.Running {
		t.Fatal("old daemon not restored")
	}
}

func TestConfigArg(t *testing.T) {
	tests := []struct {
		entrypoint, cmd []string
		want            string
	}{
		{cmd: []string{"--config", "/etc/dockpaum/dockpaum.yml"}, want: "/etc/dockpaum/dockpaum.yml"},
		{cmd: []string{"--once", "--config=/c.yml"}, want: "/c.yml"},
		{cmd: []string{"-config", "/c.yml"}, want: "/c.yml"},
		{entrypoint: []string{"/dockpaum", "-config=/e.yml"}, want: "/e.yml"},
		{cmd: []string{"--dry-run"}, want: ""},
		{cmd: []string{"--config"}, want: ""},
	}
	for _, tc := range tests {
		fc := &docker.FullContainer{Config: &container.Config{Entrypoint: tc.entrypoint, Cmd: tc.cmd}}
		if got := ConfigArg(fc); got != tc.want {
			t.Errorf("ConfigArg(%q %q) = %q, want %q", tc.entrypoint, tc.cmd, got, tc.want)
		}
	}
}

func TestCheckAccess(t *testing.T) {
	tests := []struct {
		name     string
		env      []string
		binds    []string
		mounts   []mount.Mount
		mode     container.NetworkMode
		endpoint string
		wantErr  string
	}{
		{name: "default socket bound", binds: []string{"/var/run/docker.sock:/var/run/docker.sock:ro"}},
		{name: "socket bound under /run", binds: []string{"/var/run/docker.sock:/run/docker.sock"}},
		{name: "directory holding the socket", mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/var/run", Target: "/var/run"}}},
		{name: "rootless socket through DOCKER_HOST", env: []string{"DOCKER_HOST=unix:///run/user/1000/docker.sock"},
			binds: []string{"/run/user/1000/docker.sock:/run/user/1000/docker.sock"}},
		{name: "configured endpoint wins over the environment", env: []string{"DOCKER_HOST=unix:///nowhere.sock"},
			endpoint: "unix:///var/run/docker.sock", binds: []string{"/var/run/docker.sock:/var/run/docker.sock"}},
		{name: "no socket mounted", binds: []string{"/srv/dockpaum:/config"}, wantErr: "/var/run/docker.sock is not mounted"},
		{name: "a different socket mounted", env: []string{"DOCKER_HOST=unix:///run/user/1000/docker.sock"},
			binds: []string{"/var/run/docker.sock:/var/run/docker.sock"}, wantErr: "/run/user/1000/docker.sock is not mounted"},
		{name: "socket proxy over tcp", env: []string{"DOCKER_HOST=tcp://socket-proxy:2375"}, mode: "ops_proxy"},
		{name: "tcp without a network", endpoint: "tcp://10.0.0.1:2376", mode: "none", wantErr: "has no network"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := &docker.FullContainer{
				Config:     &container.Config{Env: tc.env},
				HostConfig: &container.HostConfig{Binds: tc.binds, Mounts: tc.mounts, NetworkMode: tc.mode},
			}
			err := CheckAccess(fc, tc.endpoint)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestHelperHealthTimeout(t *testing.T) {
	tests := []struct {
		name string
		hc   *container.HealthConfig
		want time.Duration
	}{
		{name: "no healthcheck", want: minHelperHealthTimeout},
		{name: "disabled", hc: &container.HealthConfig{Test: []string{"NONE"}}, want: minHelperHealthTimeout},
		{name: "image default (30s interval, 10s start period)", hc: &container.HealthConfig{
			Test: []string{"CMD", "/dockpaum", "--healthcheck"}, Interval: 30 * time.Second, StartPeriod: 10 * time.Second,
		}, want: minHelperHealthTimeout},
		{name: "slow interval", hc: &container.HealthConfig{Test: []string{"CMD", "x"}, Interval: time.Minute, Retries: 4, StartPeriod: time.Minute},
			want: 6*time.Minute + 30*time.Second},
		{name: "capped", hc: &container.HealthConfig{Test: []string{"CMD", "x"}, Interval: time.Hour}, want: maxHelperHealthTimeout},
	}
	for _, tc := range tests {
		if got := helperHealthTimeout(tc.hc); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestHelperError(t *testing.T) {
	tests := map[string]string{
		"":              "no output",
		"just a line\n": "just a line",
		"{\"msg\":\"x\"}\nself-update helper: boom": "boom",
		"self-update helper: first\nlater noise\n":  "first",
		strings.Repeat("x", 2000):                   "…" + strings.Repeat("x", maxReportedOutput),
	}
	for in, want := range tests {
		if got := helperError(in); got != want {
			t.Errorf("helperError(%.30q) = %.40q, want %.40q", in, got, want)
		}
	}
}
