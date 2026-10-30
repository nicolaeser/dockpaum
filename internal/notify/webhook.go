package notify

import (
	"context"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type webhook struct {
	url     string
	token   string
	headers map[string]string
}

func newWebhook(c config.NotifierConfig) *webhook {
	return &webhook{url: c.URL, token: c.Token, headers: c.Headers}
}

type webhookPayload struct {
	Type       string       `json:"type"`
	Instance   string       `json:"instance,omitempty"`
	RunID      string       `json:"run_id,omitempty"`
	Host       string       `json:"host,omitempty"`
	Container  string       `json:"container,omitempty"`
	Image      string       `json:"image,omitempty"`
	OldDigest  string       `json:"old_digest,omitempty"`
	NewDigest  string       `json:"new_digest,omitempty"`
	OldTag     string       `json:"old_tag,omitempty"`
	NewTag     string       `json:"new_tag,omitempty"`
	Policy     string       `json:"policy,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Error      string       `json:"error,omitempty"`
	ReleaseURL string       `json:"release_url,omitempty"`
	Stats      *RunStats    `json:"stats,omitempty"`
	Startup    *StartupInfo `json:"startup,omitempty"`
	At         string       `json:"at"`
}

func (w *webhook) Notify(ctx context.Context, ev Event) error {
	p := webhookPayload{
		Type:       ev.Type,
		Instance:   ev.Instance,
		RunID:      ev.RunID,
		Host:       ev.Host,
		Container:  ev.Container,
		Image:      ev.Image,
		OldDigest:  ev.OldDigest,
		NewDigest:  ev.NewDigest,
		OldTag:     ev.OldTag,
		NewTag:     ev.NewTag,
		Policy:     ev.Policy,
		Reason:     ev.Reason,
		ReleaseURL: ev.ReleaseURL,
		Stats:      ev.RunStats,
		Startup:    ev.Startup,
		At:         ev.At.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if ev.Err != nil {
		p.Error = ev.Err.Error()
	}
	headers := make(map[string]string, len(w.headers)+1)
	explicitAuth := false
	for k, v := range w.headers {
		headers[k] = v
		explicitAuth = explicitAuth || strings.EqualFold(k, "Authorization")
	}
	if w.token != "" && !explicitAuth {
		headers["Authorization"] = "Bearer " + w.token
	}
	return postJSON(ctx, w.url, p, headers, w.token)
}
