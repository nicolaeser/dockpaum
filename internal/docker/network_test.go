package docker

import (
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

const oldID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestEndpointForCreate(t *testing.T) {
	inspected := &network.EndpointSettings{
		IPAMConfig:  &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.0.5")},
		Links:       []string{"db:database"},
		Aliases:     []string{"web", "0123456789ab", oldID, "api"},
		MacAddress:  network.HardwareAddr{0x02, 0x42, 0x0a, 0x00, 0x00, 0x05},
		DriverOpts:  map[string]string{"com.example.opt": "1"},
		GwPriority:  10,
		NetworkID:   "net-id",
		EndpointID:  "ep-id",
		Gateway:     netip.MustParseAddr("10.0.0.1"),
		IPAddress:   netip.MustParseAddr("10.0.0.5"),
		IPPrefixLen: 24,
		DNSNames:    []string{"web", "0123456789ab"},
	}
	tests := []struct {
		name string
		mac  bool
		want *network.EndpointSettings
	}{
		{
			name: "API 1.44+ keeps the MAC address",
			mac:  true,
			want: &network.EndpointSettings{
				IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.0.5")},
				Links:      []string{"db:database"},
				Aliases:    []string{"web", "api"},
				MacAddress: network.HardwareAddr{0x02, 0x42, 0x0a, 0x00, 0x00, 0x05},
				DriverOpts: map[string]string{"com.example.opt": "1"},
				GwPriority: 10,
			},
		},
		{
			name: "older APIs drop the MAC address",
			want: &network.EndpointSettings{
				IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.0.5")},
				Links:      []string{"db:database"},
				Aliases:    []string{"web", "api"},
				DriverOpts: map[string]string{"com.example.opt": "1"},
				GwPriority: 10,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := endpointForCreate(inspected, oldID, tc.mac)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("endpoint = %+v\nwant       %+v", got, tc.want)
			}
		})
	}
	if inspected.Aliases[1] != "0123456789ab" || inspected.DriverOpts == nil {
		t.Fatal("the inspected endpoint must not be modified")
	}
	if got := endpointForCreate(nil, oldID, true); !reflect.DeepEqual(got, &network.EndpointSettings{}) {
		t.Fatalf("nil endpoint = %+v", got)
	}
}

func TestCreateEndpointsContainerNetworkMode(t *testing.T) {
	insp := &container.InspectResponse{
		ID:              oldID,
		HostConfig:      &container.HostConfig{NetworkMode: "container:vpn"},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"bridge": {}}},
	}
	if got := createEndpoints(insp, true); len(got.EndpointsConfig) != 0 {
		t.Fatalf("container network mode must not send endpoints, got %v", got.EndpointsConfig)
	}
}

func TestSplitEndpoints(t *testing.T) {
	eps := map[string]*network.EndpointSettings{
		"proj_front": {Aliases: []string{"web"}},
		"proj_back":  {Aliases: []string{"web-internal"}},
		"monitoring": {},
	}
	tests := []struct {
		name        string
		mode        container.NetworkMode
		eps         map[string]*network.EndpointSettings
		multi       bool
		wantCreate  []string
		wantConnect []string
	}{
		{name: "API 1.44+ creates with every endpoint", mode: "proj_front", eps: eps, multi: true,
			wantCreate: []string{"monitoring", "proj_back", "proj_front"}},
		{name: "older API: network mode is the primary", mode: "proj_front", eps: eps,
			wantCreate: []string{"proj_front"}, wantConnect: []string{"monitoring", "proj_back"}},
		{name: "older API: default mode means bridge", mode: "default",
			eps:        map[string]*network.EndpointSettings{"bridge": {}, "extra": {}},
			wantCreate: []string{"bridge"}, wantConnect: []string{"extra"}},
		{name: "older API: unknown primary falls back to the first name", mode: "4f1c2d",
			eps:        map[string]*network.EndpointSettings{"b": {}, "a": {}},
			wantCreate: []string{"a"}, wantConnect: []string{"b"}},
		{name: "a single endpoint is never split", mode: "proj_front",
			eps:        map[string]*network.EndpointSettings{"proj_front": {}},
			wantCreate: []string{"proj_front"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			create, later := splitEndpoints(&container.HostConfig{NetworkMode: tc.mode}, &network.NetworkingConfig{EndpointsConfig: tc.eps}, tc.multi)
			var gotCreate []string
			for name := range create.EndpointsConfig {
				gotCreate = append(gotCreate, name)
			}
			sort.Strings(gotCreate)
			var gotConnect []string
			for _, ep := range later {
				gotConnect = append(gotConnect, ep.name)
				if ep.settings != tc.eps[ep.name] {
					t.Errorf("connect %s carries the wrong settings", ep.name)
				}
			}
			if !reflect.DeepEqual(gotCreate, tc.wantCreate) || !reflect.DeepEqual(gotConnect, tc.wantConnect) {
				t.Fatalf("create %v connect %v, want create %v connect %v", gotCreate, gotConnect, tc.wantCreate, tc.wantConnect)
			}
		})
	}
}

func TestClearForContainerNetwork(t *testing.T) {
	newCfg := func() (*container.Config, *container.HostConfig) {
		return &container.Config{
			Hostname:     "vpn-host",
			Domainname:   "example.org",
			ExposedPorts: network.PortSet{network.MustParsePort("8080/tcp"): {}},
			Env:          []string{"KEEP=1"},
		}, &container.HostConfig{
			PortBindings:    network.PortMap{network.MustParsePort("8080/tcp"): {{HostPort: "8080"}}},
			PublishAllPorts: true,
			DNS:             []netip.Addr{netip.MustParseAddr("1.1.1.1")},
			DNSOptions:      []string{"ndots:1"},
			DNSSearch:       []string{"example.org"},
			ExtraHosts:      []string{"db:10.0.0.2"},
			Links:           []string{"/db:/app/db"},
			Binds:           []string{"/srv:/srv"},
		}
	}

	cfg, hc := newCfg()
	hc.NetworkMode = "container:0123456789ab"
	clearForContainerNetwork(cfg, hc)
	if cfg.Hostname != "" || cfg.Domainname != "" || cfg.ExposedPorts != nil {
		t.Errorf("config not cleared: %+v", cfg)
	}
	if hc.PortBindings != nil || hc.PublishAllPorts || hc.DNS != nil || hc.DNSOptions != nil ||
		hc.DNSSearch != nil || hc.ExtraHosts != nil || hc.Links != nil {
		t.Errorf("host config not cleared: %+v", hc)
	}
	if len(cfg.Env) != 1 || len(hc.Binds) != 1 {
		t.Error("unrelated settings must be kept")
	}

	for _, mode := range []container.NetworkMode{"bridge", "host", "proj_default", ""} {
		cfg, hc := newCfg()
		hc.NetworkMode = mode
		clearForContainerNetwork(cfg, hc)
		if cfg.Hostname != "vpn-host" || len(hc.DNS) != 1 || len(hc.PortBindings) != 1 {
			t.Errorf("network mode %q: settings must be kept", mode)
		}
	}
}

func TestFindContainerAndNetworkDependents(t *testing.T) {
	owner := Container{ID: "aaaa1111bbbb2222", Name: "vpn"}
	other := Container{ID: "aaaa9999cccc0000", Name: "other"}
	byID := Container{ID: "d1", Name: "qbit", NetworkMode: "container:aaaa1111bbbb2222"}
	byName := Container{ID: "d2", Name: "sabnzbd", NetworkMode: "container:vpn"}
	byPrefix := Container{ID: "d3", Name: "prowlarr", NetworkMode: "container:aaaa1111"}
	ambiguous := Container{ID: "d4", Name: "amb", NetworkMode: "container:aaaa"}
	elsewhere := Container{ID: "d5", Name: "x", NetworkMode: "container:other"}
	plain := Container{ID: "d6", Name: "y", NetworkMode: "proj_default"}
	list := []Container{owner, other, byID, byName, byPrefix, ambiguous, elsewhere, plain}

	find := []struct {
		ref    string
		wantID string
	}{
		{"aaaa1111bbbb2222", owner.ID},
		{"vpn", owner.ID},
		{"/vpn", owner.ID},
		{"aaaa1111", owner.ID},
		{"aaaa", ""},
		{"missing", ""},
		{"", ""},
	}
	for _, tc := range find {
		got, ok := FindContainer(list, tc.ref)
		if (tc.wantID == "") == ok || (ok && got.ID != tc.wantID) {
			t.Errorf("FindContainer(%q) = %q, %v; want %q", tc.ref, got.ID, ok, tc.wantID)
		}
	}
	named := append(list, Container{ID: "ffff", Name: "aaaa1111"})
	if got, _ := FindContainer(named, "aaaa1111"); got.ID != "ffff" {
		t.Errorf("name must win over an ID prefix, got %q", got.ID)
	}

	var got []string
	for _, d := range NetworkDependents(list, owner.ID) {
		got = append(got, d.Name)
	}
	if want := []string{"qbit", "sabnzbd", "prowlarr"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependents = %v, want %v", got, want)
	}
	if byName.NetworkOwnerRef() != "vpn" || plain.NetworkOwnerRef() != "" {
		t.Error("NetworkOwnerRef")
	}
}

func TestEffectiveStopTimeout(t *testing.T) {
	secs := func(n int) *int { return &n }
	tests := []struct {
		name string
		own  *int
		want int
	}{
		{"no own timeout uses update.stop_timeout", nil, 30},
		{"a longer own timeout wins", secs(120), 120},
		{"a shorter own timeout keeps the global one", secs(10), 30},
		{"wait forever (-1) stays bounded", secs(-1), 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := &FullContainer{Config: &container.Config{StopTimeout: tc.own}}
			if got := fc.EffectiveStopTimeout(30); got != tc.want {
				t.Fatalf("EffectiveStopTimeout = %d, want %d", got, tc.want)
			}
		})
	}
	if got := (&FullContainer{}).EffectiveStopTimeout(5); got != 5 {
		t.Fatalf("without config: %d", got)
	}
}

func testClient(t *testing.T, d *dockertest.Daemon) *Client {
	t.Helper()
	c, err := open(context.Background(), config.HostConfig{Name: "test", Endpoint: d.Endpoint()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.api.Close() })
	return c
}

func createBody(t *testing.T, call dockertest.Call) container.CreateRequest {
	t.Helper()
	var req container.CreateRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func recreateOnto(t *testing.T, c *Client, name, newName string) (string, error) {
	t.Helper()
	fc, err := c.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.CreateContainer(context.Background(), newName, fc)
}

func multiNetworkDaemon(t *testing.T, api string) (*dockertest.Daemon, string) {
	d := dockertest.New(t, api)
	d.AddImage(dockertest.Image{}, "example.com/app:1")
	id := d.Run("web", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "proj_front"},
		map[string]*network.EndpointSettings{
			"proj_front": {Aliases: []string{"web"}, IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.1.0.9")}},
			"proj_back":  {Aliases: []string{"web-internal"}},
		})
	return d, id
}

func TestCreateContainerOldAPIConnectsFurtherNetworks(t *testing.T) {
	d, id := multiNetworkDaemon(t, "1.43")
	c := testClient(t, d)

	newID, err := recreateOnto(t, c, "web", "web-new")
	if err != nil {
		t.Fatalf("create on API 1.43: %v", err)
	}
	creates := d.Calls("POST", "/containers/create")
	if len(creates) != 1 {
		t.Fatalf("creates = %d", len(creates))
	}
	req := createBody(t, creates[0])
	if len(req.NetworkingConfig.EndpointsConfig) != 1 || req.NetworkingConfig.EndpointsConfig["proj_front"] == nil {
		t.Fatalf("create must carry only the primary network, got %v", req.NetworkingConfig.EndpointsConfig)
	}
	if mac := req.NetworkingConfig.EndpointsConfig["proj_front"].MacAddress; len(mac) != 0 {
		t.Fatalf("API 1.43 must not send a per-endpoint MAC, got %q", mac)
	}
	connects := d.Calls("POST", "/networks/proj_back/connect")
	if len(connects) != 1 {
		t.Fatalf("expected one connect to proj_back, got %d", len(connects))
	}
	var conn network.ConnectRequest
	if err := json.Unmarshal(connects[0].Body, &conn); err != nil {
		t.Fatal(err)
	}
	if conn.Container != newID || !reflect.DeepEqual(conn.EndpointConfig.Aliases, []string{"web-internal"}) {
		t.Fatalf("connect = %+v (aliases %v)", conn, conn.EndpointConfig.Aliases)
	}

	got, _ := d.Inspect(newID)
	if len(got.NetworkSettings.Networks) != 2 {
		t.Fatalf("replacement networks = %v", got.NetworkSettings.Networks)
	}
	front := got.NetworkSettings.Networks["proj_front"]
	if front.IPAMConfig == nil || front.IPAMConfig.IPv4Address != netip.MustParseAddr("10.1.0.9") {
		t.Errorf("static address lost: %+v", front.IPAMConfig)
	}
	for _, a := range append(front.Aliases, got.NetworkSettings.Networks["proj_back"].Aliases...) {
		if a == id[:12] {
			t.Errorf("the old short ID %s must not carry over as an alias", a)
		}
	}
}

func TestCreateContainerNewAPISendsAllEndpointsAndMAC(t *testing.T) {
	d, _ := multiNetworkDaemon(t, "1.47")
	c := testClient(t, d)
	old, _ := d.Inspect("web")

	if _, err := recreateOnto(t, c, "web", "web-new"); err != nil {
		t.Fatal(err)
	}
	if n := len(d.Calls("POST", "/connect")); n != 0 {
		t.Fatalf("API 1.47 must not need connects, got %d", n)
	}
	req := createBody(t, d.Calls("POST", "/containers/create")[0])
	if len(req.NetworkingConfig.EndpointsConfig) != 2 {
		t.Fatalf("endpoints = %v", req.NetworkingConfig.EndpointsConfig)
	}
	for name, ep := range req.NetworkingConfig.EndpointsConfig {
		if want := old.NetworkSettings.Networks[name].MacAddress; ep.MacAddress.String() != want.String() || len(want) == 0 {
			t.Errorf("%s: MAC %q, want %q", name, ep.MacAddress, want)
		}
		if ep.EndpointID != "" || ep.NetworkID != "" || ep.IPAddress.IsValid() {
			t.Errorf("%s: operational data sent back: %+v", name, ep)
		}
	}
}

func TestCreateContainerConnectFailureReturnsTheIncompleteContainer(t *testing.T) {
	d, _ := multiNetworkDaemon(t, "1.43")
	d.FailNetwork("proj_back")
	c := testClient(t, d)

	newID, err := recreateOnto(t, c, "web", "web-new")
	if err == nil || !strings.Contains(err.Error(), "connect network proj_back") {
		t.Fatalf("err = %v", err)
	}
	if newID == "" {
		t.Fatal("the incomplete container's ID must be returned for cleanup")
	}
	if _, ok := d.Inspect(newID); !ok {
		t.Fatal("CreateContainer must leave removal to the caller's rollback")
	}
}

func TestInspectKeepsAnonymousMountVolume(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{}, "example.com/db:1")
	d.Run("db", &container.Config{Image: "example.com/db:1"}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}},
	}, nil)
	old, _ := d.Inspect("db")
	c := testClient(t, d)

	newID, err := recreateOnto(t, c, "db", "db-new")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.Inspect(newID)
	if len(got.Mounts) != 1 || got.Mounts[0].Name != old.Mounts[0].Name || old.Mounts[0].Name == "" {
		t.Fatalf("replacement mounts %+v, want the volume %q", got.Mounts, old.Mounts[0].Name)
	}
}

func TestContainerNetworkModeRecreate(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{Config: &container.Config{ExposedPorts: network.PortSet{network.MustParsePort("8080/tcp"): {}}}}, "example.com/vpn:1", "example.com/app:1")
	ownerID := d.Run("vpn", &container.Config{Image: "example.com/vpn:1", Hostname: "vpn"}, &container.HostConfig{
		DNS: []netip.Addr{netip.MustParseAddr("9.9.9.9")},
	}, nil)
	d.Run("app", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	c := testClient(t, d)
	ctx := context.Background()

	fc, err := c.Inspect(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Config.Hostname != "" {
		t.Fatalf("hostname copied from the owner must be cleared, got %q", fc.Config.Hostname)
	}
	if err := c.ResolveNetworkOwner(ctx, fc); err != nil {
		t.Fatal(err)
	}
	if fc.HostConfig.NetworkMode != "container:vpn" {
		t.Fatalf("network mode %q: a reference by name must stay a name", fc.HostConfig.NetworkMode)
	}
	fc.Config.ExposedPorts = nil
	newID, err := c.CreateContainer(ctx, "app-new", fc)
	if err != nil {
		t.Fatalf("create in container network mode: %v", err)
	}
	if err := c.StartContainer(ctx, newID); err != nil {
		t.Fatal(err)
	}

	list, err := c.ListContainers(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var deps []string
	for _, ct := range NetworkDependents(list, ownerID) {
		deps = append(deps, ct.Name)
	}
	sort.Strings(deps)
	if !reflect.DeepEqual(deps, []string{"app", "app-new"}) {
		t.Fatalf("dependents = %v", deps)
	}
}

func TestResolveNetworkOwnerFailures(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{}, "example.com/app:1")
	d.Run("vpn", &container.Config{Image: "example.com/app:1"}, nil, nil)
	c := testClient(t, d)
	ctx := context.Background()

	gone := &FullContainer{Running: true, HostConfig: &container.HostConfig{NetworkMode: "container:deadbeef"}}
	if err := c.ResolveNetworkOwner(ctx, gone); err == nil || !strings.Contains(err.Error(), "container:deadbeef") {
		t.Fatalf("missing owner: %v", err)
	}
	if err := c.StopContainer(ctx, "vpn", 1); err != nil {
		t.Fatal(err)
	}
	stopped := &FullContainer{Running: true, HostConfig: &container.HostConfig{NetworkMode: "container:vpn"}}
	if err := c.ResolveNetworkOwner(ctx, stopped); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped owner: %v", err)
	}
	if stopped.HostConfig.NetworkMode != "container:vpn" {
		t.Fatal("a failed resolution must not change the network mode")
	}
	asleep := &FullContainer{HostConfig: &container.HostConfig{NetworkMode: "container:vpn"}}
	if err := c.ResolveNetworkOwner(ctx, asleep); err != nil {
		t.Fatal(err)
	}
	plain := &FullContainer{HostConfig: &container.HostConfig{NetworkMode: "bridge"}}
	if err := c.ResolveNetworkOwner(ctx, plain); err != nil || plain.HostConfig.NetworkMode != "bridge" {
		t.Fatalf("plain network mode: %v %q", err, plain.HostConfig.NetworkMode)
	}
}

func TestResolveNetworkOwnerPinsOnlyIDReferences(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{}, "example.com/app:1")
	ownerID := d.Run("vpn", &container.Config{Image: "example.com/app:1"}, nil, nil)
	c := testClient(t, d)
	tests := []struct{ ref, want string }{
		{ref: "vpn", want: "container:vpn"},
		{ref: "/vpn", want: "container:/vpn"},
		{ref: ownerID[:12], want: "container:" + ownerID},
		{ref: ownerID, want: "container:" + ownerID},
	}
	for _, tc := range tests {
		fc := &FullContainer{Running: true, HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode("container:" + tc.ref)}}
		if err := c.ResolveNetworkOwner(context.Background(), fc); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if got := string(fc.HostConfig.NetworkMode); got != tc.want {
			t.Errorf("container:%s resolved to %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestListContainersNetworkMode(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{}, "example.com/app:1")
	d.Run("vpn", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "proj_default"}, nil)
	d.Run("app", &container.Config{Image: "example.com/app:1"}, &container.HostConfig{NetworkMode: "container:vpn"}, nil)
	list, err := testClient(t, d).ListContainers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, ct := range list {
		modes[ct.Name] = ct.NetworkMode
	}
	if modes["vpn"] != "proj_default" || modes["app"] != "container:vpn" {
		t.Fatalf("network modes = %v", modes)
	}
}

func TestRestartContainer(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{}, "example.com/app:1")
	d.Run("app", &container.Config{Image: "example.com/app:1"}, nil, nil)
	if err := testClient(t, d).RestartContainer(context.Background(), "app", 7); err != nil {
		t.Fatal(err)
	}
	calls := d.Calls("POST", "/restart")
	if len(calls) != 1 || calls[0].Query.Get("t") != "7" {
		t.Fatalf("restart calls = %+v", calls)
	}
}

func TestInspectReportsTheContainerWideMACAddressBeforeAPI144(t *testing.T) {
	for _, tc := range []struct{ api, want string }{{"1.40", "02:42:ac:11:00:99"}, {"1.43", "02:42:ac:11:00:99"}, {"1.44", ""}, {"1.47", ""}} {
		t.Run(tc.api, func(t *testing.T) {
			d := dockertest.New(t, tc.api)
			d.AddImage(dockertest.Image{}, "example.com/app:1")
			d.Run("web", &container.Config{Image: "example.com/app:1"}, nil, nil)
			d.Run("plain", &container.Config{Image: "example.com/app:1"}, nil, nil)
			d.SetMACAddress("web", "02:42:ac:11:00:99")
			c := testClient(t, d)
			fc, err := c.Inspect(context.Background(), "web")
			if err != nil {
				t.Fatal(err)
			}
			if got := fc.MACAddress(); got != tc.want {
				t.Fatalf("MACAddress() = %q, want %q", got, tc.want)
			}
			plain, err := c.Inspect(context.Background(), "plain")
			if err != nil {
				t.Fatal(err)
			}
			if got := plain.MACAddress(); got != "" {
				t.Fatalf("a container without --mac-address reports %q", got)
			}
		})
	}
}
