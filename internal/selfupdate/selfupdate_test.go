package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

func TestIsSelf(t *testing.T) {
	full := strings.Repeat("a", 64)
	cases := []struct {
		self, other string
		want        bool
	}{
		{full, full, true},
		{full, full[:12], true},
		{full[:12], full, true},
		{full, strings.Repeat("b", 64), false},
		{"", full, false},
		{full, "", false},
	}
	for i, c := range cases {
		if got := IsSelf(c.self, c.other); got != c.want {
			t.Errorf("case %d IsSelf(%q,%q)=%v want %v", i, c.self, c.other, got, c.want)
		}
	}
}

func TestReadHex(t *testing.T) {
	id := strings.Repeat("0", 64)
	cases := map[string]string{
		id + "/extra": id,
		"abc123":      "abc123",
		"abc_def":     "abc",
		"":            "",
		"ZZZ":         "",
		"deadBEEF":    "dead",
	}
	for in, want := range cases {
		if got := readHex(in); got != want {
			t.Errorf("readHex(%q)=%q want %q", in, got, want)
		}
	}
}

func TestExtractDockerID(t *testing.T) {
	id := strings.Repeat("a", 64)
	lines := []string{
		"0::/system.slice/docker-" + id + ".scope",
		"12:pids:/docker/" + id,
		"1:name=systemd:/docker/" + id + "/init",
		"0::/machine.slice/libpod-" + id + ".scope",
	}
	for _, l := range lines {
		if got := extractDockerID(l); got != id {
			t.Errorf("extractDockerID(%q)=%q want %q", l, got, id)
		}
	}
	if got := extractDockerID("0::/system.slice/sshd.service"); got != "" {
		t.Errorf("non-docker line should yield empty, got %q", got)
	}
}

func TestReadCgroupContainerID(t *testing.T) {
	id := strings.Repeat("b", 64)
	dir := t.TempDir()
	p := filepath.Join(dir, "cgroup")
	if err := os.WriteFile(p, []byte("0::/system.slice/docker-"+id+".scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readCgroupContainerID(p); got != id {
		t.Errorf("readCgroupContainerID=%q want %q", got, id)
	}
	if got := readCgroupContainerID(filepath.Join(dir, "nope")); got != "" {
		t.Errorf("missing file should yield empty, got %q", got)
	}
}

func mountinfoLine(root, point string) string {
	return "1234 1200 8:1 " + root + " " + point + " rw,relatime - ext4 /dev/sda1 rw\n"
}

func TestCandidates(t *testing.T) {
	id := strings.Repeat("c", 64)
	other := strings.Repeat("d", 64)
	overlayRoot := "1200 1 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/ABC,upperdir=/var/lib/docker/overlay2/" + other + "/diff\n"
	tests := []struct {
		name      string
		cgroup    string
		mountinfo string
		hostname  string
		want      []Candidate
	}{
		{name: "cgroup v1", cgroup: "12:pids:/docker/" + id + "\n", want: []Candidate{{id, SourceCgroup}}},
		{name: "cgroup v2 with the host's cgroup namespace", cgroup: "0::/system.slice/docker-" + id + ".scope\n", want: []Candidate{{id, SourceCgroup}}},
		{name: "podman", cgroup: "0::/machine.slice/libpod-" + id + ".scope/container\n", want: []Candidate{{id, SourceCgroup}}},
		{name: "default data root", cgroup: "0::/\n",
			mountinfo: overlayRoot + mountinfoLine("/var/lib/docker/containers/"+id+"/hostname", "/etc/hostname"),
			want:      []Candidate{{id, SourceMountinfo}}},
		{name: "data root on its own filesystem", cgroup: "0::/\n",
			mountinfo: mountinfoLine("/containers/"+id+"/resolv.conf", "/etc/resolv.conf"), want: []Candidate{{id, SourceMountinfo}}},
		{name: "synology", cgroup: "0::/\n",
			mountinfo: mountinfoLine("/@docker/containers/"+id+"/hosts", "/etc/hosts"), want: []Candidate{{id, SourceMountinfo}}},
		{name: "podman overlay containers", cgroup: "0::/\n",
			mountinfo: mountinfoLine("/var/lib/containers/storage/overlay-containers/"+id+"/userdata/hostname", "/etc/hostname"),
			want:      []Candidate{{id, SourceMountinfo}}},
		{name: "another container's file mounted elsewhere is ignored", cgroup: "0::/\n",
			mountinfo: mountinfoLine("/var/lib/docker/containers/"+other+"/config.v2.json", "/srv/other.json")},
		{name: "default hostname", cgroup: "0::/\n", hostname: id[:12], want: []Candidate{{id[:12], SourceHostname}}},
		{name: "hostname that is the ID prefix of the mountinfo candidate", cgroup: "0::/\n",
			mountinfo: mountinfoLine("/var/lib/docker/containers/"+id+"/hostname", "/etc/hostname"), hostname: id[:12],
			want: []Candidate{{id, SourceMountinfo}}},
		{name: "custom hostname", cgroup: "0::/\n", hostname: "dockpaum"},
		{name: "custom hostname that looks hex-ish", cgroup: "0::/\n", hostname: "dockpaum-updater"},
		{name: "hostname too short", cgroup: "0::/\n", hostname: "abc123"},
		{name: "uppercase hostname", cgroup: "0::/\n", hostname: strings.ToUpper(id[:12])},
		{name: "all sources, deduplicated and ordered", cgroup: "0::/system.slice/docker-" + id + ".scope\n",
			mountinfo: mountinfoLine("/var/lib/docker/containers/"+other+"/hostname", "/etc/hostname"), hostname: id[:12],
			want: []Candidate{{id, SourceCgroup}, {other, SourceMountinfo}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cg, mi := filepath.Join(dir, "cgroup"), filepath.Join(dir, "mountinfo")
			if err := os.WriteFile(cg, []byte(tc.cgroup), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mi, []byte(tc.mountinfo), 0o600); err != nil {
				t.Fatal(err)
			}
			got := candidates(cg, mi, tc.hostname)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDockpaum(t *testing.T) {
	tests := []struct {
		labels map[string]string
		image  string
		want   bool
	}{
		{labels: map[string]string{"org.opencontainers.image.title": "dockpaum"}, image: "registry.local/mirror/updater:1", want: true},
		{image: "ghcr.io/nicolaeser/dockpaum:latest", want: true},
		{image: "ghcr.io/nicolaeser/dockpaum@sha256:abc", want: true},
		{image: "registry:5000/dockpaum", want: true},
		{image: "dockpaum", want: true},
		{image: "nginx:1.27", want: false},
		{image: "ghcr.io/nicolaeser/dockpaum-website:1", want: false},
		{labels: map[string]string{"org.opencontainers.image.title": "nginx"}, image: "nginx", want: false},
	}
	for _, tc := range tests {
		if got := IsDockpaum(tc.labels, tc.image); got != tc.want {
			t.Errorf("IsDockpaum(%v, %q) = %v, want %v", tc.labels, tc.image, got, tc.want)
		}
	}
}

func TestIdentify(t *testing.T) {
	d, c := helperHost(t, "1.47")
	d.AddImage(dockertest.Image{ID: "dp", Config: &container.Config{
		Entrypoint: []string{"/dockpaum"}, Labels: map[string]string{"org.opencontainers.image.title": "dockpaum"},
	}}, "ghcr.io/nicolaeser/dockpaum:1")
	d.AddImage(dockertest.Image{ID: "vpn"}, "example.com/vpn:1")
	d.AddImage(dockertest.Image{ID: "app"}, "example.com/app:1")
	plain := d.Run("dockpaum", &container.Config{Image: "ghcr.io/nicolaeser/dockpaum:1"}, nil, nil)
	app := d.Run("app", &container.Config{Image: "example.com/app:1"}, nil, nil)
	vpn := d.Run("vpn", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	joiner := d.Run("dockpaum-vpn", &container.Config{Image: "ghcr.io/nicolaeser/dockpaum:1"}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	busy := d.Run("busy", &container.Config{Image: "example.com/vpn:1"}, nil, nil)
	d.Run("dp-a", &container.Config{Image: "ghcr.io/nicolaeser/dockpaum:1"}, &container.HostConfig{NetworkMode: container.NetworkMode("container:" + busy)}, nil)
	d.Run("dp-b", &container.Config{Image: "ghcr.io/nicolaeser/dockpaum:1"}, &container.HostConfig{NetworkMode: "container:busy"}, nil)

	tests := []struct {
		name  string
		cands []Candidate
		want  string
	}{
		{name: "cgroup names the own container, whatever it runs", cands: []Candidate{{app, SourceCgroup}}, want: app},
		{name: "mountinfo names a dockpaum container", cands: []Candidate{{plain, SourceMountinfo}}, want: plain},
		{name: "default hostname resolves to the full ID", cands: []Candidate{{plain[:12], SourceHostname}}, want: plain},
		{name: "network owner's files: the dockpaum container joining it", cands: []Candidate{{vpn, SourceMountinfo}}, want: joiner},
		{name: "owner hostname: the dockpaum container joining it", cands: []Candidate{{vpn[:12], SourceHostname}}, want: joiner},
		{name: "two dockpaum containers join the owner: unknown", cands: []Candidate{{busy, SourceMountinfo}}},
		{name: "not dockpaum and nothing joins it: unknown", cands: []Candidate{{app, SourceMountinfo}}},
		{name: "no such container", cands: []Candidate{{strings.Repeat("e", 64), SourceCgroup}}},
		{name: "first confirmed candidate wins", cands: []Candidate{{strings.Repeat("e", 64), SourceCgroup}, {plain, SourceMountinfo}}, want: plain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Identify(context.Background(), []*docker.Client{c}, tc.cands)
			if ok != (tc.want != "") || got.ID != tc.want {
				t.Fatalf("Identify = %+v, %v; want %q", got, ok, tc.want)
			}
			if ok && got.Host != "h" {
				t.Fatalf("host = %q", got.Host)
			}
		})
	}
}
