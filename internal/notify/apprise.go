package notify

import (
	"context"
	"net/url"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type apprise struct {
	endpoint string
	urls     []string
	tag      string
	headers  map[string]string
}

func newApprise(c config.NotifierConfig) *apprise {
	return &apprise{endpoint: appriseEndpoint(c.URL), urls: c.To, tag: c.Channel, headers: c.Headers}
}

func appriseEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	switch strings.TrimSuffix(u.Path, "/") {
	case "", "/notify":
		u.Path = "/notify/"
		u.RawPath = ""
	}
	return u.String()
}

func appriseType(ev Event) string {
	switch {
	case isProblem(ev):
		return "failure"
	case ev.Type == EventUpdateSuccess || ev.Type == EventSelfUpdateSuccess:
		return "success"
	}
	return "info"
}

func (a *apprise) Notify(ctx context.Context, ev Event) error {
	body := map[string]any{
		"title":  formatTitle(ev),
		"body":   formatBody(ev),
		"type":   appriseType(ev),
		"format": "text",
	}
	if len(a.urls) > 0 {
		body["urls"] = strings.Join(a.urls, ", ")
	}
	if a.tag != "" {
		body["tag"] = a.tag
	}
	headers := map[string]string{"Accept": "application/json"}
	for k, v := range a.headers {
		headers[k] = v
	}
	var secrets []string
	for _, u := range a.urls {
		secrets = append(secrets, urlSecrets(u)...)
	}
	return postJSON(ctx, a.endpoint, body, headers, secrets...)
}
