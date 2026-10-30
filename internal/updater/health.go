package updater

import (
	"context"
	"time"

	"github.com/nicolaeser/dockpaum/internal/docker"
)

const (
	healthGraceCap     = 15 * time.Second
	healthPollInterval = 500 * time.Millisecond
)

type healthGate struct {
	timeout  time.Duration
	graceCap time.Duration
	poll     time.Duration
}

func (g healthGate) wait(ctx context.Context, c docker.StateReader, id string, started docker.RuntimeState) error {
	return docker.HealthGate{Timeout: g.timeout, GraceCap: g.graceCap, Poll: g.poll}.Wait(ctx, c, id, started)
}
