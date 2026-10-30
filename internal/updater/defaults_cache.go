package updater

import (
	"context"
	"encoding/json"

	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
)

func (u *Updater) imageDefaults(ctx context.Context, c *docker.Client, imageID string) (d *docker.ImageDefaults, fromCache bool, err error) {
	d, err = c.ImageDefaults(ctx, imageID)
	if err == nil {
		if raw, merr := json.Marshal(d); merr == nil {
			u.state.PutImageDefaults(c.Name, imageID, raw)
		}
		return d, false, nil
	}
	if cached, ok := selfupdate.RecordedDefaults(u.state, c.Name)(imageID); ok {
		return cached, true, nil
	}
	return nil, false, err
}
