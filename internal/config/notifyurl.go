package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const notifySchemes = "discord://, slack://, telegram://, ntfy://, gotify://, teams://, smtp://, pushover://, matrix://, mattermost://, apprise:// (apprises://) and generic:// (generic+http://, generic+https://)"

var schemeRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://`)

func urlScheme(raw string) string {
	m := schemeRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

func isServiceURL(raw string) bool {
	s := urlScheme(raw)
	return s != "" && s != "http" && s != "https"
}

var (
	discordHookPath = regexp.MustCompile(`^/api/webhooks/[^/]+/[^/]+/?$`)
	slackHookPath   = regexp.MustCompile(`^/services/[^/]+/[^/]+/[^/]+/?$`)
)

func webhookType(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case (host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com")) && discordHookPath.MatchString(u.Path):
		return "discord"
	case host == "hooks.slack.com" && slackHookPath.MatchString(u.Path):
		return "slack"
	case (strings.HasSuffix(host, ".logic.azure.com") || strings.HasSuffix(host, ".powerplatform.com")) && strings.Contains(u.Path, "/workflows/"):
		return "teams"
	}
	return ""
}

type serviceParams struct {
	values url.Values
	used   map[string]bool
}

func newServiceParams(q url.Values) *serviceParams {
	p := &serviceParams{values: url.Values{}, used: map[string]bool{}}
	for k, vs := range q {
		lk := strings.ToLower(k)
		p.values[lk] = append(p.values[lk], vs...)
	}
	return p
}

func (p *serviceParams) get(keys ...string) string {
	for _, k := range keys {
		p.used[k] = true
		if vs, ok := p.values[k]; ok && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

func (p *serviceParams) has(key string) bool {
	_, ok := p.values[key]
	return ok
}

func (p *serviceParams) flag(key string) (bool, error) {
	v := p.get(key)
	if v == "" {
		return false, nil
	}
	b, err := parseBool(v)
	if err != nil {
		return false, fmt.Errorf("parameter %s: %w", key, err)
	}
	return b, nil
}

func (p *serviceParams) ignored() []string {
	var out []string
	for k := range p.values {
		if !p.used[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type serviceConverter func(u *url.URL, p *serviceParams) (out []NotifierConfig, notes []string, err error)

var serviceConverters = map[string]serviceConverter{
	"discord":       discordURL,
	"slack":         slackURL,
	"telegram":      telegramURL,
	"ntfy":          ntfyURL,
	"gotify":        gotifyURL,
	"teams":         teamsURL,
	"smtp":          smtpURL,
	"pushover":      pushoverURL,
	"matrix":        matrixURL,
	"mattermost":    mattermostURL,
	"apprise":       appriseURL,
	"apprises":      appriseURL,
	"generic":       genericURL,
	"generic+http":  genericURL,
	"generic+https": genericURL,
}

func convertServiceURL(raw string) ([]NotifierConfig, []string, error) {
	raw = strings.TrimSpace(raw)
	scheme := urlScheme(raw)
	if scheme == "" {
		return nil, nil, errors.New("not a URL (it has no scheme)")
	}
	convert, ok := serviceConverters[scheme]
	if !ok {
		return nil, nil, fmt.Errorf("%s:// is not a supported notification service; supported are %s (generic+https://… posts dockpaum's event JSON to any other webhook)", scheme, notifySchemes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s:// URL is malformed (special characters in a token or password must be percent-encoded)", scheme)
	}
	p := newServiceParams(u.Query())
	out, notes, err := convert(u, p)
	if err != nil {
		return nil, nil, fmt.Errorf("%s:// URL: %w", scheme, err)
	}
	return out, append(notes, ignoredNote(scheme, p)...), nil
}

func ignoredNote(scheme string, p *serviceParams) []string {
	ignored := p.ignored()
	if len(ignored) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s:// parameters %s are not supported and are ignored (dockpaum formats its own messages)", scheme, strings.Join(ignored, ", "))}
}

func password(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

func username(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	return u.User.Username()
}

func one(n NotifierConfig) ([]NotifierConfig, []string, error) {
	return []NotifierConfig{n}, nil, nil
}

func discordURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	token, id := username(u), u.Hostname()
	if token == "" || id == "" || password(u) != "" {
		return nil, nil, errors.New("want discord://<token>@<webhook id>")
	}
	hook := url.URL{Scheme: "https", Host: "discord.com", Path: "/api/webhooks/" + id + "/" + token}
	if thread := p.get("thread_id"); thread != "" {
		hook.RawQuery = url.Values{"thread_id": {thread}}.Encode()
	}
	return one(NotifierConfig{Type: "discord", URL: hook.String()})
}

var slackTokenRE = regexp.MustCompile(`^(?:(xox.|hook)[-:]|:?)([A-Z0-9]{9,})([-/,])([A-Z0-9]{9,})([-/,])([A-Za-z0-9]{24,})$`)

func slackURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	var token, channel string
	var notes []string
	if len(u.Path) > 1 {
		token = u.Hostname() + u.Path
		if username(u) != "" {
			notes = append(notes, "slack:// bot name is not supported and is ignored (the webhook's own name is used)")
		}
	} else {
		token = username(u)
		if pw := password(u); pw != "" {
			token += ":" + pw
		}
		if h := u.Hostname(); h != "" && !strings.EqualFold(h, "webhook") {
			channel = h
		}
	}
	m := slackTokenRE.FindStringSubmatch(strings.TrimSuffix(token, "/"))
	if m == nil || m[3] != m[5] {
		return nil, nil, errors.New("want slack://hook:<T…>-<B…>-<secret>@webhook (the three parts of the Incoming Webhook URL)")
	}
	if typ := m[1]; typ != "" && typ != "hook" {
		return nil, nil, fmt.Errorf("bot and user API tokens (%s) are not supported: dockpaum posts to an Incoming Webhook; use slack://hook:<T…>-<B…>-<secret>@webhook or type: slack with the https://hooks.slack.com/services/… URL", typ)
	}
	if c := p.get("channel"); c != "" {
		channel = c
	}
	hook := "https://hooks.slack.com/services/" + m[2] + "/" + m[4] + "/" + m[6]
	return []NotifierConfig{{Type: "slack", URL: hook, Channel: channel}}, notes, nil
}

func telegramURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	token := username(u)
	if pw := password(u); pw != "" {
		token += ":" + pw
	}
	if !strings.Contains(token, ":") {
		return nil, nil, errors.New("want telegram://<bot token>@telegram?chats=<chat id>")
	}
	chats := splitList(p.get("chats", "channels"))
	if len(chats) == 0 {
		return nil, nil, errors.New("chats= is required (a chat ID or @channel name)")
	}
	out := make([]NotifierConfig, 0, len(chats))
	for _, chat := range chats {
		if strings.Contains(chat, ":") {
			return nil, nil, errors.New("chats: forum topics (<chat id>:<thread id>) are not supported; give the chat ID")
		}
		out = append(out, NotifierConfig{Type: "telegram", Token: token, Channel: chat})
	}
	return out, nil, nil
}

func insecureScheme(p *serviceParams, key string) (string, error) {
	off, err := p.flag(key)
	if err != nil || !off {
		return "https", err
	}
	return "http", nil
}

func noSkipVerify(p *serviceParams, key string) error {
	skip, err := p.flag(key)
	if err != nil {
		return err
	}
	if skip {
		return fmt.Errorf("%s is not supported: the server certificate is always verified", key)
	}
	return nil
}

func ntfyURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	topic := strings.Trim(u.Path, "/")
	if u.Host == "" || topic == "" {
		return nil, nil, errors.New("want ntfy://<host>/<topic>, e.g. ntfy://ntfy.sh/my-topic")
	}
	scheme, err := insecureScheme(p, "disabletls")
	if err != nil {
		return nil, nil, err
	}
	switch s := strings.ToLower(p.get("scheme")); s {
	case "", "https":
	case "http":
		scheme = "http"
	default:
		return nil, nil, fmt.Errorf("parameter scheme: want http or https, got %q", s)
	}
	if err := noSkipVerify(p, "disabletlsverification"); err != nil {
		return nil, nil, err
	}
	token := p.get("token")
	if token == "" && u.User != nil {
		if username(u) != "" {
			return nil, nil, errors.New("user/password login is not supported; create an access token (ntfy token add) and use ntfy://<host>/<topic>?token=tk_…")
		}
		token = password(u)
	}
	target := url.URL{Scheme: scheme, Host: u.Host, Path: "/" + topic}
	return one(NotifierConfig{Type: "ntfy", URL: target.String(), Token: token})
}

func gotifyURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	path := strings.TrimSuffix(u.Path, "/")
	i := strings.LastIndexByte(path, '/')
	if u.Host == "" || i < 0 || path[i+1:] == "" {
		return nil, nil, errors.New("want gotify://<host>/<application token>")
	}
	scheme, err := insecureScheme(p, "disabletls")
	if err != nil {
		return nil, nil, err
	}
	if err := noSkipVerify(p, "insecureskipverify"); err != nil {
		return nil, nil, err
	}
	p.used["useheader"] = true
	base := url.URL{Scheme: scheme, Host: u.Host, Path: path[:i]}
	return one(NotifierConfig{Type: "gotify", URL: base.String(), Token: path[i+1:]})
}

func teamsURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	hook, err := url.Parse(p.get("host"))
	if u.User != nil || u.Host != "" || strings.Trim(u.Path, "/") != "" || err != nil ||
		(hook.Scheme != "https" && hook.Scheme != "http") || hook.Host == "" {
		return nil, nil, errors.New("want teams://?host=<percent-encoded workflow URL> (Office 365 connector webhooks were retired by Microsoft: create a Teams workflow with \"When a Teams webhook request is received\" and use its URL, or type: teams with the https:// workflow URL)")
	}
	for _, k := range []string{"sig", "sp", "sv", "api-version"} {
		if p.has(k) {
			return nil, nil, errors.New("the workflow URL in host= must be percent-encoded (its sig/sp/sv parameters ended up outside it)")
		}
	}
	return one(NotifierConfig{Type: "teams", URL: hook.String()})
}

const smtpsPort = 465

func smtpURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	host := u.Hostname()
	if host == "" {
		return nil, nil, errors.New("want smtp://[user:password@]<host>[:<port>]/?from=<address>&to=<address>")
	}
	port := 25
	explicitPort := u.Port() != ""
	if explicitPort {
		n, err := strconv.Atoi(u.Port())
		if err != nil || n < 1 || n > 65535 {
			return nil, nil, errors.New("invalid port")
		}
		port = n
	}
	addr := func(s string) string { return strings.ReplaceAll(s, " ", "+") }
	from := addr(p.get("fromaddress", "from"))
	var to []string
	for _, t := range splitList(p.get("toaddresses", "to")) {
		to = append(to, addr(t))
	}
	if len(to) == 0 {
		return nil, nil, errors.New("toaddresses= (or to=) is required")
	}
	switch enc := strings.ToLower(p.get("encryption")); enc {
	case "", "auto", "none", "explicittls":
	case "implicittls":
		if explicitPort && port != smtpsPort {
			return nil, nil, fmt.Errorf("encryption=ImplicitTLS is supported on port %d only", smtpsPort)
		}
		port = smtpsPort
	default:
		return nil, nil, fmt.Errorf("parameter encryption: want Auto, None, ExplicitTLS or ImplicitTLS, got %q", enc)
	}
	requireTLS, err := p.flag("requirestarttls")
	if err != nil {
		return nil, nil, err
	}
	p.used["usestarttls"], p.used["starttls"] = true, true
	if err := noSkipVerify(p, "skiptlsverify"); err != nil {
		return nil, nil, err
	}
	var notes []string
	user, pass := username(u), password(u)
	switch auth := strings.ToLower(p.get("auth")); auth {
	case "", "unknown", "plain":
	case "none":
		user, pass = "", ""
	case "login", "crammd5":
		notes = append(notes, fmt.Sprintf("smtp:// auth=%s: dockpaum authenticates with PLAIN, which a server offering only %s refuses", auth, strings.ToUpper(auth)))
	default:
		return nil, nil, fmt.Errorf("parameter auth=%s is not supported (dockpaum authenticates with PLAIN)", auth)
	}
	return []NotifierConfig{{
		Type: "email",
		From: from,
		To:   to,
		SMTP: &SMTPConfig{Host: host, Port: port, Username: user, Password: pass, StartTLS: requireTLS},
	}}, notes, nil
}

func pushoverURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	token, user := password(u), u.Hostname()
	if token == "" || user == "" {
		return nil, nil, errors.New("want pushover://shoutrrr:<application token>@<user key>")
	}
	if p.get("encryptionkey", "key") != "" {
		return nil, nil, errors.New("end-to-end encryption (encryptionkey) is not supported: dockpaum would send the messages unencrypted; remove the parameter to accept that")
	}
	devices := strings.Join(splitList(p.get("devices")), ",")
	return one(NotifierConfig{Type: "pushover", Token: token, User: user, Channel: devices})
}

func matrixURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	if u.Host == "" {
		return nil, nil, errors.New("want matrix://:<access token>@<homeserver>/?rooms=<room>")
	}
	if username(u) != "" {
		return nil, nil, errors.New("user:password login is not supported; create an access token for the account and use matrix://:<access token>@<homeserver>/?rooms=<room>")
	}
	token := password(u)
	if token == "" {
		return nil, nil, errors.New("an access token is required: matrix://:<access token>@<homeserver>/?rooms=<room>")
	}
	scheme, err := insecureScheme(p, "disabletls")
	if err != nil {
		return nil, nil, err
	}
	rooms := splitList(p.get("rooms", "room"))
	if len(rooms) == 0 {
		return nil, nil, errors.New("rooms= is required (a room ID !id:server or an alias #alias:server; dockpaum does not post to every joined room)")
	}
	base := url.URL{Scheme: scheme, Host: u.Host}
	out := make([]NotifierConfig, 0, len(rooms))
	for _, room := range rooms {
		if !strings.HasPrefix(room, "!") && !strings.HasPrefix(room, "#") {
			room = "#" + room
		}
		out = append(out, NotifierConfig{Type: "matrix", URL: base.String(), Token: token, Room: room})
	}
	return out, nil, nil
}

func mattermostURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host == "" || parts[0] == "" || len(parts) > 2 {
		return nil, nil, errors.New("want mattermost://<host>/<webhook token>[/<channel>]")
	}
	scheme, err := insecureScheme(p, "disabletls")
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	if username(u) != "" {
		notes = append(notes, "mattermost:// user name is not supported and is ignored (the webhook's own name is used)")
	}
	hook := url.URL{Scheme: scheme, Host: u.Host, Path: "/hooks/" + parts[0]}
	n := NotifierConfig{Type: "slack", URL: hook.String()}
	if len(parts) == 2 {
		n.Channel = parts[1]
	}
	return []NotifierConfig{n}, notes, nil
}

func appriseURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	path := strings.Trim(u.Path, "/")
	i := strings.LastIndexByte(path, '/')
	key := path[i+1:]
	if u.Host == "" || key == "" {
		return nil, nil, errors.New("want apprise://<host>/<config key> (apprises:// for https)")
	}
	scheme := "http"
	if strings.EqualFold(u.Scheme, "apprises") {
		scheme = "https"
	}
	prefix := ""
	if i > 0 {
		prefix = "/" + path[:i]
	}
	target := url.URL{Scheme: scheme, Host: u.Host, Path: prefix + "/notify/" + key}
	return one(NotifierConfig{Type: "apprise", URL: target.String(), Channel: p.get("tags", "tag")})
}

var genericConfigKeys = []string{"contenttype", "disabletls", "template", "title", "titlekey", "messagekey", "method"}

func genericURL(u *url.URL, p *serviceParams) ([]NotifierConfig, []string, error) {
	if u.Host == "" {
		return nil, nil, errors.New("want generic://<host>/<path> or generic+https://<host>/<path>")
	}
	target := *u
	switch strings.ToLower(u.Scheme) {
	case "generic+http":
		target.Scheme = "http"
	case "generic+https":
		target.Scheme = "https"
	default:
		s, err := insecureScheme(p, "disabletls")
		if err != nil {
			return nil, nil, err
		}
		target.Scheme = s
	}
	headers := map[string]string{}
	query := url.Values{}
	var extra []string
	for k, vs := range u.Query() {
		lk := strings.ToLower(k)
		switch {
		case strings.HasPrefix(k, "@") && len(k) > 1:
			headers[headerName(k[1:])] = vs[0]
		case strings.HasPrefix(k, "$"):
			extra = append(extra, k)
		case strings.HasPrefix(k, "__") && len(k) > 2:
			query[k[2:]] = vs
		case lk == "method":
			if !strings.EqualFold(vs[0], http.MethodPost) {
				return nil, nil, fmt.Errorf("method=%s is not supported: the webhook notifier always POSTs", vs[0])
			}
		case slices.Contains(genericConfigKeys, lk):
			continue
		default:
			query[k] = vs
		}
		p.used[lk] = true
	}
	var notes []string
	if len(extra) > 0 {
		sort.Strings(extra)
		notes = append(notes, fmt.Sprintf("%s:// extra JSON fields %s are not supported: the webhook sends dockpaum's event JSON", strings.ToLower(u.Scheme), strings.Join(extra, ", ")))
	}
	target.RawQuery = query.Encode()
	n := NotifierConfig{Type: "webhook", URL: target.String()}
	if len(headers) > 0 {
		n.Headers = headers
	}
	return []NotifierConfig{n}, notes, nil
}

func headerName(k string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range k {
		if prevLower && unicode.IsUpper(r) {
			b.WriteByte('-')
		}
		b.WriteRune(r)
		prevLower = unicode.IsLower(r)
	}
	return http.CanonicalHeaderKey(b.String())
}
