package docker

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type RuntimeState struct {
	Status       string
	Running      bool
	Restarting   bool
	RestartCount int
	StartedAt    string
	ExitCode     int
	OOMKilled    bool
	Error        string
	Health       string
}

func (c *Client) ContainerState(ctx context.Context, id string) (RuntimeState, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	insp, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return RuntimeState{}, err
	}
	return runtimeState(insp.Container), nil
}

func runtimeState(insp container.InspectResponse) RuntimeState {
	var st RuntimeState
	st.RestartCount = insp.RestartCount
	s := insp.State
	if s == nil {
		return st
	}
	st.Status = string(s.Status)
	st.Running = s.Running
	st.Restarting = s.Restarting
	st.StartedAt = s.StartedAt
	st.ExitCode = s.ExitCode
	st.OOMKilled = s.OOMKilled
	st.Error = s.Error
	if s.Health != nil {
		st.Health = string(s.Health.Status)
	}
	return st
}
