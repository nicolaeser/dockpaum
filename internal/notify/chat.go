package notify

import (
	"context"
	"mime"
	"net/http"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type telegram struct {
	base, token, chat string
}

func newTelegram(c config.NotifierConfig) *telegram {
	base := strings.TrimSuffix(c.URL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &telegram{base: base, token: c.Token, chat: c.Channel}
}

func (t *telegram) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, t.base+"/bot"+t.token+"/sendMessage", map[string]any{
		"chat_id":                  t.chat,
		"text":                     truncate(formatTitle(ev)+"\n\n"+formatBody(ev), 4000),
		"disable_web_page_preview": true,
	}, nil, t.token)
}

type ntfy struct{ url, token string }

func newNtfy(c config.NotifierConfig) *ntfy { return &ntfy{url: c.URL, token: c.Token} }

func (n *ntfy) Notify(ctx context.Context, ev Event) error {
	req, err := newRequest(ctx, http.MethodPost, n.url, strings.NewReader(formatBody(ev)))
	if err != nil {
		return err
	}
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", formatTitle(ev)))
	req.Header.Set("Tags", "dockpaum")
	if isProblem(ev) {
		req.Header.Set("Priority", "high")
	}
	if l := httpLink(ev.ReleaseURL); l != "" {
		req.Header.Set("Click", l)
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	return send(req)
}

type gotify struct{ url, token string }

func newGotify(c config.NotifierConfig) *gotify {
	return &gotify{url: strings.TrimSuffix(c.URL, "/"), token: c.Token}
}

func (g *gotify) Notify(ctx context.Context, ev Event) error {
	priority := 4
	if isProblem(ev) {
		priority = 8
	}
	body := map[string]any{
		"title":    formatTitle(ev),
		"message":  formatBody(ev),
		"priority": priority,
	}
	if l := httpLink(ev.ReleaseURL); l != "" {
		body["extras"] = map[string]any{"client::notification": map[string]any{"click": map[string]any{"url": l}}}
	}
	return postJSON(ctx, g.url+"/message", body, map[string]string{"X-Gotify-Key": g.token})
}

type teams struct{ url string }

func newTeams(c config.NotifierConfig) *teams { return &teams{url: c.URL} }

func (m *teams) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, m.url, teamsPayload(ev), nil)
}

func teamsPayload(ev Event) map[string]any {
	facts := []map[string]string{}
	for _, r := range eventRows(ev) {
		v := r.value
		if r.item && r.link != "" {
			v += " · " + r.link
		}
		facts = append(facts, map[string]string{"title": r.label, "value": teamsText(v)})
	}
	color := "Default"
	switch {
	case isProblem(ev):
		color = "Attention"
	case ev.Type == EventUpdateSuccess || ev.Type == EventSelfUpdateSuccess || ev.Type == EventRunCompleted:
		color = "Good"
	}
	body := []any{
		map[string]any{"type": "TextBlock", "text": teamsText(formatTitle(ev)), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
	}
	if len(facts) > 0 {
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
		"msteams": map[string]any{"width": "Full"},
	}
	if l := httpLink(ev.ReleaseURL); l != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Release notes", "url": l}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}
