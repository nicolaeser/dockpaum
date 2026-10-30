package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type OneShot struct {
	Name        string
	Image       string
	Cmd         []string
	Labels      map[string]string
	Like        *FullContainer
	StopTimeout int
}

func (c *Client) CreateOneShotLike(ctx context.Context, spec OneShot) (string, error) {
	if spec.Like == nil || spec.Like.Config == nil {
		return "", errors.New("one-shot: no container to model it on")
	}
	like := spec.Like
	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Cmd,
		Env:    slices.Clone(like.Config.Env),
		User:   like.Config.User,
		Labels: maps.Clone(spec.Labels),
	}
	if spec.StopTimeout > 0 {
		t := spec.StopTimeout
		cfg.StopTimeout = &t
	}
	hc := &container.HostConfig{}
	if src := like.HostConfig; src != nil {
		hc.Binds = namedBinds(src.Binds)
		hc.Mounts = namedMounts(src.Mounts)
		hc.VolumesFrom = slices.Clone(src.VolumesFrom)
		hc.GroupAdd = slices.Clone(src.GroupAdd)
		hc.NetworkMode = src.NetworkMode
		hc.ExtraHosts = slices.Clone(src.ExtraHosts)
		hc.DNS = slices.Clone(src.DNS)
		hc.DNSOptions = slices.Clone(src.DNSOptions)
		hc.DNSSearch = slices.Clone(src.DNSSearch)
		hc.SecurityOpt = slices.Clone(src.SecurityOpt)
		hc.Privileged = src.Privileged
		hc.UsernsMode = src.UsernsMode
	}
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if like.NetworkConfig != nil && !hc.NetworkMode.IsContainer() {
		for name := range like.NetworkConfig.EndpointsConfig {
			nc.EndpointsConfig[name] = &network.EndpointSettings{}
		}
	}
	return c.CreateContainer(ctx, spec.Name, &FullContainer{Config: cfg, HostConfig: hc, NetworkConfig: nc})
}

func namedBinds(binds []string) []string {
	var out []string
	for _, b := range binds {
		if strings.Contains(b, ":") {
			out = append(out, b)
		}
	}
	return out
}

func namedMounts(mounts []mount.Mount) []mount.Mount {
	var out []mount.Mount
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Source == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (c *Client) RemoveOneShot(ctx context.Context, id string) error {
	ctx, cancel := opCtx(ctx, bulkTimeout)
	defer cancel()
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

var ErrWaitTimeout = errors.New("container did not exit in time")

func (c *Client) WaitExit(ctx context.Context, id string, timeout time.Duration) (int, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	wait := c.api.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.Error != nil && res.Error.Message != "" {
			return int(res.StatusCode), fmt.Errorf("wait: %s", res.Error.Message)
		}
		return int(res.StatusCode), nil
	case err := <-wait.Error:
		if wctx.Err() != nil && ctx.Err() == nil {
			return 0, ErrWaitTimeout
		}
		return 0, err
	}
}

const maxLogBytes = 16 * 1024

func (c *Client) LogsTail(ctx context.Context, id string, lines int) (string, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	rc, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines),
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	buf := &limitedBuffer{max: maxLogBytes}
	if _, err := stdcopy.StdCopy(buf, buf, io.LimitReader(rc, 4*maxLogBytes)); err != nil {
		return buf.String(), fmt.Errorf("read logs: %w", err)
	}
	return buf.String(), nil
}
