package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/server"
)

func imageTargets(ctx context.Context, clients *docker.Clients, includeStopped bool, req server.TriggerRequest) (names, conflicts []string, err error) {
	clients.Reconnect(ctx)
	if down := clients.Unreachable(); len(down) > 0 {
		return nil, nil, fmt.Errorf("docker host(s) unreachable: %s", strings.Join(down, ", "))
	}
	match, other := map[string]bool{}, map[string]bool{}
	for _, c := range clients.List() {
		if err := classifyContainers(ctx, c, includeStopped, req, match, other); err != nil {
			return nil, nil, fmt.Errorf("host %s: %w", c.Name, err)
		}
	}
	names, conflicts = selectTargets(match, other, req.Containers)
	return names, conflicts, nil
}

type containerSource interface {
	ListContainers(ctx context.Context, includeStopped bool) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (*docker.FullContainer, error)
}

func classifyContainers(ctx context.Context, src containerSource, includeStopped bool, req server.TriggerRequest, match, other map[string]bool) error {
	list, err := src.ListContainers(ctx, includeStopped)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, ct := range list {
		image, err := createdFrom(ctx, src, ct)
		if err != nil {
			return err
		}
		if req.MatchesImage(image) {
			match[ct.Name] = true
		} else {
			other[ct.Name] = true
		}
	}
	return nil
}

func createdFrom(ctx context.Context, src containerSource, ct docker.Container) (string, error) {
	if !strings.HasPrefix(ct.Image, "sha256:") && (ct.ImageID == "" || ct.Image != ct.ImageID) {
		return ct.Image, nil
	}
	full, err := src.Inspect(ctx, ct.ID)
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", ct.Name, err)
	}
	return full.Image, nil
}

func selectTargets(match, other map[string]bool, only []string) (names, conflicts []string) {
	for n := range match {
		switch {
		case other[n]:
			conflicts = append(conflicts, n)
		case len(only) == 0 || slices.Contains(only, n):
			names = append(names, n)
		}
	}
	slices.Sort(names)
	slices.Sort(conflicts)
	return names, conflicts
}
