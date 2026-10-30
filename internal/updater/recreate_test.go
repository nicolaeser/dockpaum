package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/notify"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

type fakeHost struct {
	t        *testing.T
	d        *dockertest.Daemon
	u        *Updater
	reg      *fakeRegistry
	auditDir string
}

func newFakeHost(t *testing.T, apiVersion string, mutate func(*config.Config)) *fakeHost {
	t.Helper()
	d := dockertest.New(t, apiVersion)
	dir := t.TempDir()
	cfg := &config.Config{
		Hosts: []config.HostConfig{{Name: "fake", Endpoint: d.Endpoint()}},
		Update: config.UpdatePolicy{
			Strategy:    config.StrategyRolling,
			Concurrency: 1,
			StopTimeout: config.Duration(2 * time.Second),
			Policies: []config.Policy{
				{ID: "default", Mode: config.ModeTagFollow, HealthTimeout: config.Duration(2 * time.Second)},
			},
		},
		Hooks: config.HookConfig{Timeout: config.Duration(5 * time.Second)},
		Audit: config.AuditConfig{Dir: dir},
	}
	if mutate != nil {
		mutate(cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clients, err := docker.OpenAll(context.Background(), cfg.Hosts, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clients.Close)
	al, err := audit.New(cfg.Audit, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })
	u, err := New(Deps{
		Clients: clients, Notifier: notify.Build(nil, logger), Metrics: metrics.NewRegistry(),
		Logger: logger, Config: cfg, Audit: al,
	})
	if err != nil {
		t.Fatal(err)
	}
	u.selfID, u.selfChecked, u.selfUnknown, u.selfCands = "", true, false, nil
	u.healthGraceCap = 30 * time.Millisecond
	u.healthPoll = 5 * time.Millisecond
	reg := &fakeRegistry{}
	u.reg = reg
	return &fakeHost{t: t, d: d, u: u, reg: reg, auditDir: dir}
}

func (h *fakeHost) run() error {
	h.t.Helper()
	return h.u.RunWith(context.Background(), RunOptions{Trigger: "test"})
}

func (h *fakeHost) records(name string) []audit.Record {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.auditDir, "*.jsonl"))
	var out []audit.Record
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			h.t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var r audit.Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				h.t.Fatal(err)
			}
			if r.Container == name {
				out = append(out, r)
			}
		}
		_ = fh.Close()
	}
	return out
}

func (h *fakeHost) decisions(name string) []string {
	var out []string
	for _, r := range h.records(name) {
		out = append(out, string(r.Decision))
	}
	return out
}

func (h *fakeHost) inspect(name string) container.InspectResponse {
	h.t.Helper()
	ct, ok := h.d.Inspect(name)
	if !ok {
		h.t.Fatalf("container %s does not exist (have %v)", name, h.d.Names())
	}
	return ct
}

func (h *fakeHost) noLeftovers() {
	h.t.Helper()
	for _, n := range h.d.Names() {
		if strings.Contains(n, ".dockpaum-old-") {
			h.t.Fatalf("leftover temporary container %s", n)
		}
	}
}

func (h *fakeHost) release(ref string, oldImg, newImg dockertest.Image) {
	oldImg.RepoDigests = append(oldImg.RepoDigests, registry.DigestReference(ref, fakeDigest(ref, oldImg.ID)))
	h.d.AddImage(oldImg, ref)
	h.publish(ref, newImg)
}

func (h *fakeHost) publish(ref string, img dockertest.Image) string {
	h.t.Helper()
	d := fakeDigest(ref, img.ID)
	img.RepoDigests = append(img.RepoDigests, registry.DigestReference(ref, d))
	h.d.Publish(ref, img)
	cfg := registry.ImageConfig{Digest: imageID(img.ID), Manifest: d}
	created := img.Created
	if created == "" {
		created = "2024-01-01T00:00:00Z"
	}
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		cfg.Created = t
	}
	if img.Config != nil {
		cfg.Labels = img.Config.Labels
	}
	h.reg.setDigest(ref, d)
	h.reg.setConfig(d, cfg)
	return d
}

func imageID(id string) string {
	if strings.HasPrefix(id, "sha256:") {
		return id
	}
	return "sha256:" + id
}

func fakeDigest(ref, id string) string {
	sum := sha256.Sum256([]byte(ref + "\x00" + imageID(id)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestUpdateRollsBackCrashLoopingReplacement(t *testing.T) {
	for _, policy := range []container.RestartPolicyMode{container.RestartPolicyUnlessStopped, container.RestartPolicyAlways, ""} {
		t.Run("restart="+string(policy), func(t *testing.T) {
			h := newFakeHost(t, "1.47", nil)
			h.release("example.com/app:1", dockertest.Image{ID: "good"}, dockertest.Image{ID: "bad", CrashLoop: true})
			oldID := h.d.Run("web", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: policy},
			}, nil)

			if err := h.run(); err == nil {
				t.Fatal("a rolled-back update must fail the run")
			}
			if got := h.decisions("web"); !reflect.DeepEqual(got, []string{"rolled-back"}) {
				t.Fatalf("decisions = %v", got)
			}
			ct := h.inspect("web")
			if ct.ID != oldID || ct.Image != "sha256:good" || !ct.State.Running {
				t.Fatalf("old container not restored: id %s image %s running %v", ct.ID, ct.Image, ct.State.Running)
			}
			if !strings.Contains(h.records("web")[0].Error, "health check") {
				t.Fatalf("error = %q", h.records("web")[0].Error)
			}
			h.noLeftovers()
			if len(h.d.Names()) != 1 {
				t.Fatalf("the failed replacement must be removed: %v", h.d.Names())
			}
		})
	}
}

func TestUpdateWithHealthGateOffDoesNotObserve(t *testing.T) {
	h := newFakeHost(t, "1.47", func(c *config.Config) { c.Update.Policies[0].HealthTimeout = 0 })
	h.release("example.com/app:1", dockertest.Image{ID: "good"}, dockertest.Image{ID: "bad", CrashLoop: true})
	h.d.Run("web", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}, nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.decisions("web"); !reflect.DeepEqual(got, []string{"updated"}) {
		t.Fatalf("health_timeout 0 turns the gate off; decisions = %v", got)
	}
}

func TestUpdateGatesAutoRemoveAndOrchestratedContainers(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release("example.com/app:1", dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	h.d.Run("oneshot", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{AutoRemove: true}, nil)
	h.d.Run("task", &container.Config{Image: "example.com/app:1", Labels: map[string]string{
		"com.docker.swarm.task.id": "x1", "com.docker.swarm.service.id": "s1",
	}}, nil, nil)
	h.d.Run("pod", &container.Config{Image: "example.com/app:1", Labels: map[string]string{"io.kubernetes.pod.name": "web-0"}}, nil, nil)

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"oneshot": "gated-autoremove",
		"task":    "gated-orchestrated",
		"pod":     "gated-orchestrated",
	} {
		recs := h.records(name)
		if len(recs) != 1 || string(recs[0].Decision) != want || recs[0].Reason == "" {
			t.Errorf("%s: records %+v, want one %s with a reason", name, recs, want)
		}
		if h.inspect(name).Image != "sha256:v1" {
			t.Errorf("%s was changed", name)
		}
	}
	if n := len(h.d.Calls("POST", "/images/create")); n != 0 {
		t.Fatalf("gated containers must not cause pulls, got %d", n)
	}
	if !strings.Contains(h.records("task")[0].Reason, "Docker Swarm") {
		t.Errorf("reason = %q", h.records("task")[0].Reason)
	}
}

func TestUpdateKeepsStopTimeoutAndAnonymousVolumes(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release("example.com/db:1", dockertest.Image{ID: "v1", Config: &container.Config{Volumes: map[string]struct{}{"/image-vol": {}}}},
		dockertest.Image{ID: "v2", Config: &container.Config{Volumes: map[string]struct{}{"/image-vol": {}}}})
	grace := 45
	h.d.Run("db", &container.Config{Image: "example.com/db:1", StopTimeout: &grace, StopSignal: "SIGINT"}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
	}, nil)
	old := h.inspect("db")
	vols := map[string]string{}
	for _, m := range old.Mounts {
		vols[m.Destination] = m.Name
	}

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	stops := h.d.Calls("POST", "/stop")
	if len(stops) != 1 || stops[0].Query.Get("t") != "45" {
		t.Fatalf("stop calls = %+v, want t=45", stops)
	}
	ct := h.inspect("db")
	if ct.Image != "sha256:v2" {
		t.Fatalf("not updated: %s", ct.Image)
	}
	for _, m := range ct.Mounts {
		if vols[m.Destination] != m.Name {
			t.Errorf("%s: volume %s, want %s", m.Destination, m.Name, vols[m.Destination])
		}
	}
	if len(ct.Mounts) != 2 {
		t.Errorf("mounts = %+v", ct.Mounts)
	}
	if ct.Config.StopSignal != "SIGINT" || ct.Config.StopTimeout == nil || *ct.Config.StopTimeout != 45 {
		t.Errorf("stop settings lost: %q %v", ct.Config.StopSignal, ct.Config.StopTimeout)
	}
}

func TestUpdateOnAPIBefore144WithSeveralNetworks(t *testing.T) {
	h := newFakeHost(t, "1.43", nil)
	h.release("example.com/app:1", dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
	oldID := h.d.Run("web", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "front"},
		map[string]*network.EndpointSettings{"front": {Aliases: []string{"web"}}, "back": {}})

	if err := h.run(); err != nil {
		t.Fatalf("an Engine 24 host must update multi-network containers: %v", err)
	}
	ct := h.inspect("web")
	if ct.Image != "sha256:v2" || len(ct.NetworkSettings.Networks) != 2 {
		t.Fatalf("image %s networks %v", ct.Image, ct.NetworkSettings.Networks)
	}
	for _, a := range ct.NetworkSettings.Networks["front"].Aliases {
		if a == oldID[:12] {
			t.Fatal("old short ID carried over as an alias")
		}
	}
	if len(h.d.Calls("POST", "/networks/back/connect")) != 1 {
		t.Fatal("the second network must be connected before the start")
	}
}

func TestUpdateKeepsEmulatedPlatform(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.d.SetArch("arm64")
	h.d.AddImage(dockertest.Image{ID: "amd64-v1", Architecture: "amd64"}, "example.com/app:1")
	h.d.Publish("example.com/app:1", dockertest.Image{ID: "arm64-v2", Architecture: "arm64"})
	h.d.Publish("example.com/app:1", dockertest.Image{ID: "amd64-v2", Architecture: "amd64"})
	h.d.Run("legacy", &container.Config{Image: "example.com/app:1"}, nil, nil)

	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.inspect("legacy").Image; got != "sha256:amd64-v2" {
		t.Fatalf("replacement image %s, want the amd64 build", got)
	}
	pull := h.d.Calls("POST", "/images/create")[0]
	create := h.d.Calls("POST", "/containers/create")[0]
	if pull.Query.Get("platform") != "linux/amd64" || create.Query.Get("platform") != "linux/amd64" {
		t.Fatalf("pull platform %q, create platform %q", pull.Query.Get("platform"), create.Query.Get("platform"))
	}
}

func vpnSetup(h *fakeHost) (ownerID string) {
	h.release("example.com/vpn:1", dockertest.Image{ID: "vpn1"}, dockertest.Image{ID: "vpn2"})
	h.release("example.com/app:1", dockertest.Image{ID: "app1"}, dockertest.Image{ID: "app1"})
	ownerID = h.d.Run("vpn", &container.Config{Image: "example.com/vpn:1", Hostname: "vpn"}, &container.HostConfig{
		PortBindings: nil, DNS: []netip.Addr{netip.MustParseAddr("9.9.9.9")},
	}, nil)
	byID := container.NetworkMode("container:" + ownerID)
	h.d.Run("app-id", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: byID}, nil)
	h.d.Run("app-name", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	h.d.Run("app-mon", &container.Config{Image: "example.com/app:1", Labels: map[string]string{"dockpaum.monitor-only": "true"}},
		&container.HostConfig{NetworkMode: byID}, nil)
	return ownerID
}

func TestUpdateReattachesNetworkDependents(t *testing.T) {
	for _, strategy := range []string{config.StrategyRolling, config.StrategyParallel} {
		t.Run(strategy, func(t *testing.T) {
			h := newFakeHost(t, "1.47", func(c *config.Config) {
				c.Update.Strategy = strategy
				c.Update.Concurrency = 4
			})
			ownerID := vpnSetup(h)
			nameID := h.inspect("app-name").ID
			rec := h.recordEvents()

			err := h.run()
			if err == nil || !strings.Contains(err.Error(), "1 container update(s) failed") {
				t.Fatalf("run error = %v, want exactly the monitor-only dependent to fail", err)
			}
			owner := h.inspect("vpn")
			if owner.ID == ownerID || owner.Image != "sha256:vpn2" {
				t.Fatalf("owner not updated: %s %s", owner.ID, owner.Image)
			}

			app := h.inspect("app-id")
			if want := container.NetworkMode("container:" + owner.ID); app.HostConfig.NetworkMode != want || !app.State.Running {
				t.Fatalf("app-id network %q running %v, want %q", app.HostConfig.NetworkMode, app.State.Running, want)
			}
			if app.Image != "sha256:app1" {
				t.Fatalf("a dependent keeps its image, got %s", app.Image)
			}
			if got := h.decisions("app-id"); !reflect.DeepEqual(got, []string{"reattached", "skipped"}) {
				t.Fatalf("app-id decisions = %v", got)
			}
			if r := h.records("app-id")[0]; !strings.Contains(r.Reason, "recreated to join the network of vpn") {
				t.Fatalf("reason = %q", r.Reason)
			}

			byName := h.inspect("app-name")
			if byName.ID != nameID || !byName.State.Running {
				t.Fatal("a dependent naming its owner by name is restarted in place")
			}
			if got := h.decisions("app-name"); !reflect.DeepEqual(got, []string{"reattached", "skipped"}) {
				t.Fatalf("app-name decisions = %v", got)
			}
			if len(h.d.Calls("POST", "/containers/"+nameID+"/restart")) != 1 {
				t.Fatal("app-name was not restarted")
			}
			for _, name := range []string{"app-id", "app-name"} {
				r := h.records(name)[0]
				if evs := rec.forContainer(name); len(evs) != 1 || evs[0].Type != notify.EventRecovered || evs[0].Reason != r.Reason || evs[0].RunID != r.RunID {
					t.Fatalf("%s: events %+v, want one update.recovered with the reattach reason", name, evs)
				}
			}

			mon := h.records("app-mon")
			if len(mon) != 2 || mon[0].Decision != audit.DecisionSkipped || mon[1].Decision != audit.DecisionFailed ||
				!strings.Contains(mon[1].Error, "monitor-only") {
				t.Fatalf("app-mon records = %+v", mon)
			}
			if h.inspect("app-mon").HostConfig.NetworkMode != container.NetworkMode("container:"+ownerID) {
				t.Fatal("a monitor-only dependent must not be recreated")
			}
			h.noLeftovers()
		})
	}
}

func TestDependentWithMovedTagIsLeftToItsOwnUpdate(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	vpnSetup(h)
	h.d.AddImage(dockertest.Image{ID: "app2"}, "example.com/app:1")
	h.publish("example.com/app:1", dockertest.Image{ID: "app2"})

	_ = h.run()
	app := h.inspect("app-id")
	owner := h.inspect("vpn")
	if app.Image != "sha256:app2" || app.HostConfig.NetworkMode != container.NetworkMode("container:"+owner.ID) {
		t.Fatalf("app-id image %s network %s", app.Image, app.HostConfig.NetworkMode)
	}
	if got := h.decisions("app-id"); !reflect.DeepEqual(got, []string{"updated"}) {
		t.Fatalf("app-id decisions = %v, want only its own update", got)
	}
}

func TestDependentNotProcessedInTheRunFailsVisibly(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		label   string
		wantErr string
	}{
		{
			name:    "filtered out: this instance does not manage it",
			mutate:  func(c *config.Config) { c.Filters.ExcludeNames = []string{"app-id"} },
			wantErr: "does not manage it (in-exclude-list)",
		},
		{
			name: "own schedule, image tag moved: not updated behind its gates",
			mutate: func(c *config.Config) {
				c.Update.Policies = append(c.Update.Policies, config.Policy{ID: "nightly", Mode: config.ModeTagFollow, Schedule: "0 0 3 * * *"})
			},
			label:   "nightly",
			wantErr: "no longer resolves to the image it runs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, "1.47", tc.mutate)
			ownerID := vpnSetup(h)
			if tc.label != "" {
				h.d.AddImage(dockertest.Image{ID: "app1"}, "example.com/app:1")
				h.d.Run("app-id2", &container.Config{Image: "example.com/app:1", Labels: map[string]string{"dockpaum.policy": tc.label}},
					&container.HostConfig{NetworkMode: container.NetworkMode("container:" + ownerID)}, nil)
			}
			h.d.AddImage(dockertest.Image{ID: "app2"}, "example.com/app:1")
			_ = h.run()
			name := "app-id"
			if tc.label != "" {
				name = "app-id2"
			}
			recs := h.records(name)
			if len(recs) != 1 || recs[0].Decision != audit.DecisionFailed || !strings.Contains(recs[0].Error, tc.wantErr) {
				t.Fatalf("%s records = %+v", name, recs)
			}
			if got := h.inspect(name); got.Image != "sha256:app1" || got.HostConfig.NetworkMode != container.NetworkMode("container:"+ownerID) {
				t.Fatalf("%s must be left untouched: %s %s", name, got.Image, got.HostConfig.NetworkMode)
			}
		})
	}
}

func TestRollbackRestartsNetworkDependents(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release("example.com/vpn:1", dockertest.Image{ID: "vpn1"}, dockertest.Image{ID: "vpn2", CrashLoop: true})
	h.d.AddImage(dockertest.Image{ID: "app1"}, "example.com/app:1")
	h.d.Publish("example.com/app:1", dockertest.Image{ID: "app1"})
	ownerID := h.d.Run("vpn", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	appID := h.d.Run("app", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{
		NetworkMode: container.NetworkMode("container:" + ownerID),
	}, nil)

	if err := h.run(); err == nil {
		t.Fatal("expected the rolled-back update to fail the run")
	}
	if got := h.decisions("vpn"); !reflect.DeepEqual(got, []string{"rolled-back"}) {
		t.Fatalf("vpn decisions = %v", got)
	}
	recs := h.records("app")
	if len(recs) < 1 || recs[0].Decision != decisionReattached || !strings.Contains(recs[0].Reason, "rolled back") {
		t.Fatalf("app records = %+v", recs)
	}
	if len(h.d.Calls("POST", "/containers/"+appID+"/restart")) != 1 {
		t.Fatal("the dependent must be restarted to rejoin the restored owner")
	}
	if ct := h.inspect("app"); ct.ID != appID || !ct.State.Running {
		t.Fatal("dependent not running in place")
	}
}

func TestDependentOfStoppedOwnerFailsBeforeTouchingIt(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.d.AddImage(dockertest.Image{ID: "vpn1"}, "example.com/vpn:1")
	h.release("example.com/app:1", dockertest.Image{ID: "app1"}, dockertest.Image{ID: "app2"})
	h.d.Run("vpn", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	appID := h.d.Run("app", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	if err := h.u.deps.Clients.List()[0].StopContainer(context.Background(), "vpn", 1); err != nil {
		t.Fatal(err)
	}
	if err := h.run(); err == nil {
		t.Fatal("expected a failure")
	}
	recs := h.records("app")
	if len(recs) != 1 || recs[0].Decision != audit.DecisionFailed || !strings.Contains(recs[0].Error, "not running") {
		t.Fatalf("app records = %+v", recs)
	}
	if len(h.d.Calls("POST", "/containers/"+appID+"/stop")) != 0 {
		t.Fatal("the dependent must not be stopped when its replacement could not start")
	}
}

func TestNetworkUnits(t *testing.T) {
	ct := func(id, name, mode string) docker.Container {
		return docker.Container{ID: id, Name: name, NetworkMode: mode}
	}
	tests := []struct {
		name    string
		targets []docker.Container
		want    [][]string
	}{
		{
			name:    "independent containers stay separate",
			targets: []docker.Container{ct("a1", "a", "bridge"), ct("b1", "b", "")},
			want:    [][]string{{"a"}, {"b"}},
		},
		{
			name: "dependents follow their owner, whatever the listing order",
			targets: []docker.Container{
				ct("d1", "app", "container:v1"), ct("x1", "other", ""), ct("v1", "vpn", ""), ct("d2", "web", "container:vpn"),
			},
			want: [][]string{{"vpn", "app", "web"}, {"other"}},
		},
		{
			name: "chains are ordered by depth",
			targets: []docker.Container{
				ct("c1", "c", "container:b"), ct("b1", "b", "container:a"), ct("a1", "a", ""),
			},
			want: [][]string{{"a", "b", "c"}},
		},
		{
			name:    "an owner outside the targets does not group",
			targets: []docker.Container{ct("d1", "app", "container:gone"), ct("d2", "web", "container:gone")},
			want:    [][]string{{"app"}, {"web"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got [][]string
			for _, unit := range networkUnits(tc.targets) {
				got = append(got, names(unit))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("units = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOrchestratedBy(t *testing.T) {
	tests := []struct {
		labels map[string]string
		want   string
	}{
		{nil, ""},
		{map[string]string{"com.docker.compose.project": "p"}, ""},
		{map[string]string{"com.docker.swarm.task.id": "t"}, "Docker Swarm"},
		{map[string]string{"com.docker.swarm.service.id": "s"}, "Docker Swarm"},
		{map[string]string{"io.kubernetes.container.name": "c"}, "Kubernetes"},
		{map[string]string{"com.hashicorp.nomad.alloc_id": "a"}, "Nomad"},
		{map[string]string{"com.docker.swarm.node.id": "n"}, ""},
	}
	for _, tc := range tests {
		got := orchestratedBy(tc.labels)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("orchestratedBy(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestHostRunTracksReplacements(t *testing.T) {
	hr := newHostRun("r", []docker.Container{{ID: "a"}, {ID: "b"}})
	hr.replace("a", "a2")
	hr.replace("a2", "a3")
	if got := hr.current("a"); got != "a3" {
		t.Fatalf("current(a) = %q", got)
	}
	if got := hr.current("z"); got != "z" {
		t.Fatalf("current(z) = %q", got)
	}
	if !hr.claim("a3") || hr.claim("a3") {
		t.Fatal("claim must be exclusive")
	}
	hr.release("a3")
	if !hr.claim("a3") {
		t.Fatal("release must free the claim")
	}

	failure := dependentResult{ct: docker.Container{ID: "b"}, err: os.ErrInvalid}
	if !hr.deferFailure(failure) {
		t.Fatal("a pending target's failure is deferred")
	}
	hr.begin("b")
	if hr.deferFailure(dependentResult{ct: docker.Container{ID: "b"}}) {
		t.Fatal("a target already being processed is not deferred")
	}
	if d, ok := hr.settle("b"); !ok || d.err == nil {
		t.Fatal("an unresolved deferred failure is reported")
	}
	hr.deferred["a"] = failure
	if _, ok := hr.settle("a"); ok {
		t.Fatal("a replaced target's deferred failure is dropped")
	}
}

func TestAutoRemoveDependentsAreNotTouched(t *testing.T) {
	h := newFakeHost(t, "1.47", nil)
	h.release("example.com/vpn:1", dockertest.Image{ID: "vpn1"}, dockertest.Image{ID: "vpn2"})
	h.d.AddImage(dockertest.Image{ID: "app1"}, "example.com/app:1")
	ownerID := h.d.Run("vpn", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	byID := h.d.Run("rm-id", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{
		AutoRemove: true, NetworkMode: container.NetworkMode("container:" + ownerID),
	}, nil)
	byName := h.d.Run("rm-name", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{
		AutoRemove: true, NetworkMode: "container:vpn",
	}, nil)

	if err := h.run(); err == nil {
		t.Fatal("dependents left without a network must fail the run")
	}
	for _, name := range []string{"rm-id", "rm-name"} {
		recs := h.records(name)
		if len(recs) != 2 || recs[0].Decision != decisionGatedAutoRemove || recs[1].Decision != audit.DecisionFailed ||
			!strings.Contains(recs[1].Error, "--rm") {
			t.Errorf("%s records = %+v", name, recs)
		}
	}
	for _, id := range []string{byID, byName} {
		if ct, ok := h.d.Inspect(id); !ok || !ct.State.Running {
			t.Errorf("--rm dependent %s must be left running in place", id)
		}
		if len(h.d.Calls("POST", "/containers/"+id+"/stop"))+len(h.d.Calls("POST", "/containers/"+id+"/restart")) != 0 {
			t.Errorf("--rm dependent %s was stopped", id)
		}
	}
}

func TestContainerWideMACAddressGatesBeforeAPI144(t *testing.T) {
	const mac = "02:42:ac:11:00:99"
	for _, api := range []string{"1.43", "1.44"} {
		t.Run(api, func(t *testing.T) {
			h := newFakeHost(t, api, nil)
			sink := h.deliverTo(nil, notify.EventNoUpdate)
			rec := h.recordEvents()
			h.release(appRef, dockertest.Image{ID: "v1"}, dockertest.Image{ID: "v2"})
			oldID := h.d.Run("web", &container.Config{Image: appRef}, nil, nil)
			h.d.SetMACAddress("web", mac)
			for range 2 {
				if err := h.runAndDeliver(); err != nil {
					t.Fatal(err)
				}
				if api == "1.44" {
					break
				}
			}
			recs := h.records("web")
			if api == "1.44" {
				if len(recs) != 1 || recs[0].Decision != audit.DecisionUpdated || h.inspect("web").Image != "sha256:v2" {
					t.Fatalf("records %+v: API 1.44 keeps the MAC per endpoint, so the update goes through", recs)
				}
				if n := len(rec.of(notify.EventNoUpdate, "web")); n != 0 {
					t.Fatalf("%d update.skipped events for an update that went through", n)
				}
				return
			}
			if len(recs) != 2 {
				t.Fatalf("records %+v, want one per run", recs)
			}
			for _, r := range recs {
				if r.Decision != audit.DecisionGatedMACAddress || !strings.Contains(r.Reason, mac) ||
					!strings.Contains(r.Reason, "Docker Engine 25") || !strings.Contains(r.Reason, "remove --mac-address") {
					t.Fatalf("record %s %q, want gated-mac-address with the way out", r.Decision, r.Reason)
				}
			}
			if ct := h.inspect("web"); ct.ID != oldID || ct.Image != "sha256:v1" {
				t.Fatalf("container %s %s was replaced", ct.ID, ct.Image)
			}
			if n := h.pulls() + len(h.d.Calls("POST", "/containers/create")) + len(h.d.Calls("POST", "/stop")); n != 0 {
				t.Fatalf("%d pull, create or stop call(s) for a gated container", n)
			}
			evs := rec.of(notify.EventNoUpdate, "web")
			if len(evs) != 2 || evs[0].Reason != "gated-mac-address: "+recs[0].Reason || evs[0].RunID != recs[0].RunID {
				t.Fatalf("events %+v, want update.skipped with the record's reason in each run", evs)
			}
			if n := sink.count(notify.EventNoUpdate, "web"); n != 1 {
				t.Fatalf("update.skipped delivered %d times over two runs, want once", n)
			}
			want := `dockpaum_container_update_skipped_total{container="web",host="fake",policy="default",reason="gated-mac-address"} 2`
			if m := h.scrapeMetrics(); !strings.Contains(m, want) {
				t.Fatalf("metrics lack %s", want)
			}
		})
	}
}
