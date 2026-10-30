package docker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type StateReader interface {
	ContainerState(ctx context.Context, id string) (RuntimeState, error)
}

type HealthGate struct {
	Timeout  time.Duration
	GraceCap time.Duration
	Poll     time.Duration
}

func (g HealthGate) Wait(ctx context.Context, c StateReader, id string, started RuntimeState) error {
	if g.Timeout <= 0 {
		return nil
	}
	grace := g.GraceCap
	if g.Timeout < grace {
		grace = g.Timeout
	}
	poll := g.Poll
	if poll <= 0 {
		poll = time.Second
	}
	start := time.Now()
	deadline := start.Add(g.Timeout)
	graceUntil := start.Add(grace)

	st := started
	for {
		if err := Crashed(started, st); err != nil {
			return err
		}
		now := time.Now()
		switch st.Health {
		case "healthy":
			return nil
		case "unhealthy":
			return errors.New("container is unhealthy")
		case "":
			if now.After(graceUntil) {
				return nil
			}
		}
		if now.After(deadline) {
			return errors.New("timed out waiting for container to become healthy")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		var err error
		if st, err = c.ContainerState(ctx, id); err != nil {
			return err
		}
	}
}

func Crashed(started, st RuntimeState) error {
	switch {
	case st.Restarting:
		return fmt.Errorf("container is restarting (crash loop: %s, restarted %d time(s))", exitText(st), st.RestartCount)
	case st.RestartCount > 0:
		return fmt.Errorf("container restarted %d time(s) since its start (%s)", st.RestartCount, exitText(st))
	case started.StartedAt != "" && st.StartedAt != started.StartedAt:
		return fmt.Errorf("container was restarted (started at %s, then again at %s)", started.StartedAt, st.StartedAt)
	case !st.Running:
		return fmt.Errorf("container exited before becoming healthy (%s)", exitText(st))
	case st.Status != "" && st.Status != "running":
		return fmt.Errorf("container is %s, not running", st.Status)
	}
	return nil
}

func exitText(st RuntimeState) string {
	s := fmt.Sprintf("exit code %d", st.ExitCode)
	if st.OOMKilled {
		s += ", killed for running out of memory"
	}
	if st.Error != "" {
		s += ": " + st.Error
	}
	return s
}
