package notify

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const pushoverAPI = "https://api.pushover.net/1/messages.json"

const (
	pushoverTitleMax    = 250
	pushoverMessageMax  = 1024
	pushoverURLMax      = 512
	pushoverURLTitleMax = 100
)

type pushover struct {
	endpoint, token, user, device string
}

func newPushover(c config.NotifierConfig) *pushover {
	endpoint := c.URL
	if endpoint == "" {
		endpoint = pushoverAPI
	}
	return &pushover{endpoint: endpoint, token: c.Token, user: c.User, device: c.Channel}
}

func (p *pushover) Notify(ctx context.Context, ev Event) error {
	form := url.Values{}
	form.Set("token", p.token)
	form.Set("user", p.user)
	form.Set("title", truncate(formatTitle(ev), pushoverTitleMax))
	msg := strings.TrimSpace(formatBody(ev))
	if msg == "" {
		msg = formatTitle(ev)
	}
	form.Set("message", truncate(msg, pushoverMessageMax))
	if !ev.At.IsZero() {
		form.Set("timestamp", strconv.FormatInt(ev.At.Unix(), 10))
	}
	form.Set("priority", "0")
	if l := httpLink(ev.ReleaseURL); l != "" && len(l) <= pushoverURLMax {
		form.Set("url", l)
		form.Set("url_title", truncate("Release notes", pushoverURLTitleMax))
	}
	if p.device != "" {
		form.Set("device", p.device)
	}
	req, err := newRequest(ctx, http.MethodPost, p.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(req, p.token, p.user)
}
