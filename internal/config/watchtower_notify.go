package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

func applyWatchtowerNotify(w *watchtowerVars) error {
	o := w.o
	var added []int
	urls, err := w.notificationURLs()
	if err != nil {
		return err
	}
	for i, raw := range urls {
		idx, err := o.addNotifyURL(fmt.Sprintf("WATCHTOWER_NOTIFICATION_URL[%d]", i), raw)
		if err != nil {
			return err
		}
		added = append(added, idx...)
	}
	if len(urls) > 0 {
		o.record("WATCHTOWER_NOTIFICATION_URL", "notify[]")
	}
	if types, ok := w.get("WATCHTOWER_NOTIFICATIONS"); ok {
		for _, typ := range strings.FieldsFunc(strings.ToLower(types), func(r rune) bool { return r == ',' || r == ' ' }) {
			w.legacy[typ] = true
			n, err := w.legacyNotifier(typ)
			if err != nil {
				return err
			}
			if n == nil {
				continue
			}
			n.Name = o.c.uniqueNotifierName(n.Type)
			o.c.Notify = append(o.c.Notify, *n)
			o.c.notifyOrigin = append(o.c.notifyOrigin, "WATCHTOWER_NOTIFICATIONS ("+typ+")")
			added = append(added, len(o.c.Notify)-1)
		}
		o.record("WATCHTOWER_NOTIFICATIONS", "notify[]")
	}
	events, err := w.notifyEvents()
	if err != nil {
		return err
	}
	for _, i := range added {
		o.c.Notify[i].Events = append([]string(nil), events...)
		o.watchtowerNotify[i] = true
	}
	return nil
}

func (w *watchtowerVars) notificationURLs() ([]string, error) {
	const name = "WATCHTOWER_NOTIFICATION_URL"
	raw, ok := w.get(name)
	if !ok {
		return nil, nil
	}
	var urls []string
	for _, v := range splitURLs(raw) {
		if !strings.HasPrefix(v, "/") || strings.Contains(v, "://") {
			urls = append(urls, v)
			continue
		}
		s, err := readSecretFile(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		urls = append(urls, splitURLs(s)...)
	}
	return urls, nil
}

func (w *watchtowerVars) legacyNotifier(typ string) (*NotifierConfig, error) {
	const prefix = "WATCHTOWER_NOTIFICATION_"
	source := "WATCHTOWER_NOTIFICATIONS=" + typ
	required := func(names ...string) error {
		var missing []string
		for _, n := range names {
			if _, ok := w.o.env.get(prefix + n); !ok {
				missing = append(missing, prefix+n)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s needs %s", source, strings.Join(missing, " and "))
		}
		return nil
	}
	switch typ {
	case "shoutrrr":
		return nil, nil
	case "email":
		if err := required("EMAIL_SERVER", "EMAIL_TO"); err != nil {
			return nil, err
		}
		host, _ := w.get(prefix + "EMAIL_SERVER")
		port := 25
		if v, ok := w.get(prefix + "EMAIL_SERVER_PORT"); ok {
			p, err := parsePort(v)
			if err != nil {
				return nil, envError(prefix+"EMAIL_SERVER_PORT", v, err)
			}
			port = p
		}
		user, _ := w.get(prefix + "EMAIL_SERVER_USER")
		pass, _, err := w.value(prefix + "EMAIL_SERVER_PASSWORD")
		if err != nil {
			return nil, err
		}
		from, _ := w.get(prefix + "EMAIL_FROM")
		to, _ := w.get(prefix + "EMAIL_TO")
		return &NotifierConfig{
			Type: "email",
			From: from,
			To:   strings.FieldsFunc(to, func(r rune) bool { return r == ',' || r == ' ' }),
			SMTP: &SMTPConfig{Host: host, Port: port, Username: user, Password: pass},
		}, nil
	case "slack":
		if err := required("SLACK_HOOK_URL"); err != nil {
			return nil, err
		}
		hook, _, err := w.value(prefix + "SLACK_HOOK_URL")
		if err != nil {
			return nil, err
		}
		channel, _ := w.get(prefix + "SLACK_CHANNEL")
		return &NotifierConfig{Type: "slack", URL: hook, Channel: channel}, nil
	case "msteams":
		if err := required("MSTEAMS_HOOK_URL"); err != nil {
			return nil, err
		}
		hook, _, err := w.value(prefix + "MSTEAMS_HOOK_URL")
		if err != nil {
			return nil, err
		}
		if retiredTeamsConnector(hook) {
			return nil, fmt.Errorf("%sMSTEAMS_HOOK_URL: Office 365 connector webhooks were retired by Microsoft; create a Teams workflow with \"When a Teams webhook request is received\" and use its URL", prefix)
		}
		return &NotifierConfig{Type: "teams", URL: hook}, nil
	case "gotify":
		if err := required("GOTIFY_URL", "GOTIFY_TOKEN"); err != nil {
			return nil, err
		}
		base, _ := w.get(prefix + "GOTIFY_URL")
		token, _, err := w.value(prefix + "GOTIFY_TOKEN")
		if err != nil {
			return nil, err
		}
		return &NotifierConfig{Type: "gotify", URL: base, Token: token}, nil
	}
	return nil, fmt.Errorf("WATCHTOWER_NOTIFICATIONS: unknown notification type %q (Watchtower knows email, slack, msteams, gotify and shoutrrr)", typ)
}

func retiredTeamsConnector(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, ".webhook.office.com") || h == "outlook.office.com" || h == "outlook.office365.com"
}

func (w *watchtowerVars) notifyEvents() ([]string, error) {
	report, err := w.on("WATCHTOWER_NOTIFICATION_REPORT")
	if err != nil {
		return nil, err
	}
	quiet, err := w.on("WATCHTOWER_NO_STARTUP_MESSAGE")
	if err != nil {
		return nil, err
	}
	level, hasLevel := w.get("WATCHTOWER_NOTIFICATIONS_LEVEL")
	events := []string{"update.success", "update.failure"}
	if hasLevel {
		switch strings.ToLower(level) {
		case "panic", "fatal", "error", "warn", "warning":
			events = []string{"update.failure"}
		case "info", "debug", "trace":
		default:
			return nil, envError("WATCHTOWER_NOTIFICATIONS_LEVEL", level, errors.New("want panic, fatal, error, warn, info, debug or trace"))
		}
	}
	if report {
		events = []string{"run.completed"}
	}
	if !quiet {
		events = append(events, "dockpaum.started")
	}
	return events, nil
}
