package notify

import (
	"context"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type slack struct {
	url     string
	channel string
}

func newSlack(c config.NotifierConfig) *slack {
	return &slack{url: c.URL, channel: c.Channel}
}

func slackText(ev Event) string {
	var b strings.Builder
	b.WriteString(slackEscape(formatTitle(ev)))
	b.WriteString("\n```\n")
	b.WriteString(fenceSafe(slackEscape(plainBody(ev, false))))
	b.WriteString("```")
	for _, l := range releaseLinks(ev) {
		b.WriteString("\n<" + slackEscape(l.url) + "|" + slackEscape(l.label) + ">")
	}
	return b.String()
}

func (s *slack) Notify(ctx context.Context, ev Event) error {
	body := map[string]any{"text": slackText(ev)}
	if s.channel != "" {
		body["channel"] = s.channel
	}
	return postJSON(ctx, s.url, body, nil)
}
