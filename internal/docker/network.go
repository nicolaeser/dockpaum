package docker

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func createEndpoints(insp *container.InspectResponse, perEndpointMAC bool) *network.NetworkingConfig {
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if insp.HostConfig != nil && insp.HostConfig.NetworkMode.IsContainer() {
		return nc
	}
	if insp.NetworkSettings == nil {
		return nc
	}
	for name, ep := range insp.NetworkSettings.Networks {
		nc.EndpointsConfig[name] = endpointForCreate(ep, insp.ID, perEndpointMAC)
	}
	return nc
}

func endpointForCreate(ep *network.EndpointSettings, oldID string, perEndpointMAC bool) *network.EndpointSettings {
	out := &network.EndpointSettings{}
	if ep == nil {
		return out
	}
	if ep.IPAMConfig != nil {
		out.IPAMConfig = ep.IPAMConfig.Copy()
	}
	if len(ep.Links) > 0 {
		out.Links = append([]string(nil), ep.Links...)
	}
	if len(ep.DriverOpts) > 0 {
		out.DriverOpts = maps.Clone(ep.DriverOpts)
	}
	short := oldID
	if len(short) > 12 {
		short = short[:12]
	}
	for _, a := range ep.Aliases {
		if a == "" || (oldID != "" && (a == short || a == oldID)) {
			continue
		}
		out.Aliases = append(out.Aliases, a)
	}
	out.GwPriority = ep.GwPriority
	if perEndpointMAC {
		out.MacAddress = ep.MacAddress
	}
	return out
}

type namedEndpoint struct {
	name     string
	settings *network.EndpointSettings
}

func splitEndpoints(hc *container.HostConfig, nc *network.NetworkingConfig, multi bool) (*network.NetworkingConfig, []namedEndpoint) {
	if nc == nil || multi || len(nc.EndpointsConfig) <= 1 {
		return nc, nil
	}
	var mode container.NetworkMode
	if hc != nil {
		mode = hc.NetworkMode
	}
	primary := primaryNetwork(mode, nc.EndpointsConfig)
	create := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		primary: nc.EndpointsConfig[primary],
	}}
	names := make([]string, 0, len(nc.EndpointsConfig)-1)
	for name := range nc.EndpointsConfig {
		if name != primary {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	later := make([]namedEndpoint, 0, len(names))
	for _, name := range names {
		later = append(later, namedEndpoint{name: name, settings: nc.EndpointsConfig[name]})
	}
	return create, later
}

func primaryNetwork(mode container.NetworkMode, eps map[string]*network.EndpointSettings) string {
	want := string(mode)
	if mode == "" || mode.IsDefault() {
		want = network.NetworkBridge
	}
	if _, ok := eps[want]; ok {
		return want
	}
	names := make([]string, 0, len(eps))
	for name := range eps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
}

func clearForContainerNetwork(cfg *container.Config, hc *container.HostConfig) {
	if hc == nil || !hc.NetworkMode.IsContainer() {
		return
	}
	if cfg != nil {
		cfg.Hostname = ""
		cfg.Domainname = ""
		cfg.ExposedPorts = nil
	}
	hc.PortBindings = nil
	hc.PublishAllPorts = false
	hc.DNS = nil
	hc.DNSOptions = nil
	hc.DNSSearch = nil
	hc.ExtraHosts = nil
	hc.Links = nil
}

func (ct Container) NetworkOwnerRef() string {
	return container.NetworkMode(ct.NetworkMode).ConnectedContainer()
}

func FindContainer(list []Container, ref string) (Container, bool) {
	ref = strings.TrimPrefix(ref, "/")
	if ref == "" {
		return Container{}, false
	}
	for _, ct := range list {
		if ct.ID == ref {
			return ct, true
		}
	}
	for _, ct := range list {
		if ct.Name == ref {
			return ct, true
		}
	}
	var found Container
	n := 0
	for _, ct := range list {
		if strings.HasPrefix(ct.ID, ref) {
			found = ct
			n++
		}
	}
	return found, n == 1
}

func NetworkDependents(list []Container, ownerID string) []Container {
	var out []Container
	for _, ct := range list {
		ref := ct.NetworkOwnerRef()
		if ref == "" || ct.ID == ownerID {
			continue
		}
		if owner, ok := FindContainer(list, ref); ok && owner.ID == ownerID {
			out = append(out, ct)
		}
	}
	return out
}

func (fc *FullContainer) NetworkOwnerRef() string {
	if fc.HostConfig == nil {
		return ""
	}
	return fc.HostConfig.NetworkMode.ConnectedContainer()
}

func (fc *FullContainer) JoinNetworkOf(id string) {
	if fc.HostConfig == nil {
		fc.HostConfig = &container.HostConfig{}
	}
	fc.HostConfig.NetworkMode = container.NetworkMode("container:" + id)
}

func (c *Client) ResolveNetworkOwner(ctx context.Context, fc *FullContainer) error {
	if fc.HostConfig == nil || !fc.HostConfig.NetworkMode.IsContainer() {
		return nil
	}
	ref := fc.HostConfig.NetworkMode.ConnectedContainer()
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("network_mode container:%s: cannot inspect the container whose network it joins: %w", ref, err)
	}
	owner := res.Container
	if owner.ID == "" {
		return fmt.Errorf("network_mode container:%s: incomplete inspect response", ref)
	}
	name := strings.TrimPrefix(owner.Name, "/")
	if fc.Running && (owner.State == nil || !owner.State.Running) {
		return fmt.Errorf("network_mode container:%s: %s is not running, so a replacement could not start", ref, name)
	}
	if strings.TrimPrefix(ref, "/") != name {
		fc.HostConfig.NetworkMode = container.NetworkMode("container:" + owner.ID)
	}
	return nil
}
