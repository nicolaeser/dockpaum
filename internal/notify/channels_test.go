package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type captured struct {
	method, path, rawPath, query string
	header                       http.Header
	body                         string
}

type endpoint struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []captured
	status int
	ctype  string
	reply  string
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{status: http.StatusOK}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.reqs = append(e.reqs, captured{r.Method, r.URL.Path, r.URL.RawPath, r.URL.RawQuery, r.Header.Clone(), string(b)})
		status, ctype, reply := e.status, e.ctype, e.reply
		e.mu.Unlock()
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) last() captured {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reqs) == 0 {
		return captured{}
	}
	return e.reqs[len(e.reqs)-1]
}

func (e *endpoint) answer(status int, ctype, reply string) {
	e.mu.Lock()
	e.status, e.ctype, e.reply = status, ctype, reply
	e.mu.Unlock()
}

const releaseURL = "https://github.com/acme/web/releases/tag/v1.3.0"

var hostile = Event{
	Type: EventNoUpdate, Host: "local", Container: "web", Image: "ghcr.io/acme/web:1.2",
	OldTag: "1.2", NewTag: "1.3",
	Reason: "<!channel> @everyone ```oops``` [click](https://evil.example) *bold* <script>x</script>",
	Err:    errors.New("boom ``` <@U123> & <b>more</b>"),
	At:     time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC),
}

func sampleEvents() map[string]Event {
	at := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	return map[string]Event{
		EventUpdateSuccess: {Type: EventUpdateSuccess, Host: "local", Container: "web", Image: "ghcr.io/acme/web:1.3.0",
			OldTag: "1.2.0", NewTag: "1.3.0", OldDigest: "sha256:0123456789abcdef", NewDigest: "sha256:fedcba9876543210",
			Policy: "default", RunID: "r-42", Instance: "nas", ReleaseURL: releaseURL, At: at},
		EventUpdateFailure: {Type: EventUpdateFailure, Host: "local", Container: "db", Image: "postgres:16.4",
			OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb", Err: errors.New("rolled back: replacement exited 1"), RunID: "r-42", At: at},
		EventAvailable: {Type: EventAvailable, Host: "local", Container: "cache", Image: "redis:7",
			NewDigest: "sha256:cccc", Reason: "update available; the container is labelled monitor-only", At: at},
		EventNoUpdate: {Type: EventNoUpdate, Host: "local", Container: "queue", Image: "rabbitmq:4",
			OldDigest: "sha256:dddd", NewDigest: "sha256:eeee", Reason: `outside policy "default" window`, RunID: "r-42", At: at},
		EventDenied: {Type: EventDenied, Host: "local", Container: "api", Image: "ghcr.io/acme/api:2",
			OldDigest: "sha256:1111", NewDigest: "sha256:2222", Reason: "denied by https://approver.example (HTTP 403): change freeze", At: at},
		EventRolledBack: {Type: EventRolledBack, Host: "local", Container: "db", Image: "postgres:16.4",
			OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb", Err: errors.New("rolled back: replacement exited 1"), RunID: "r-42", At: at},
		EventRecovered: {Type: EventRecovered, Host: "local", Container: "worker", Image: "ghcr.io/acme/worker:1",
			Reason: "restored from leftover worker.dockpaum-old-1760587200000000000", At: at},
		EventCheckFailed: {Type: EventCheckFailed, Host: "local", Container: "proxy", Image: "traefik:3.1",
			Err: errors.New("registry check failed: 503 Service Unavailable"), RunID: "r-42", At: at},
		EventSelfUpdateStarted: {Type: EventSelfUpdateStarted, Host: "local", Container: "dockpaum", Image: "ghcr.io/nicolaeser/dockpaum:latest",
			Reason: "helper dockpaum-self-update-1 started", RunID: "r-42", At: at},
		EventSelfUpdateSuccess: {Type: EventSelfUpdateSuccess, Host: "local", Container: "dockpaum", Image: "ghcr.io/nicolaeser/dockpaum:latest",
			NewDigest: "sha256:3333", Reason: "helper dockpaum-self-update-1 replaced the daemon", At: at},
		EventSelfUpdateFailure: {Type: EventSelfUpdateFailure, Host: "local", Container: "dockpaum", Image: "ghcr.io/nicolaeser/dockpaum:latest",
			NewDigest: "sha256:3333", Err: errors.New("the new daemon failed its health check"), At: at},
		EventStopping:   {Type: EventStopping, Instance: "nas", At: at},
		EventRunStarted: {Type: EventRunStarted, Host: "local", RunID: "r-42", At: at},
		EventRunCompleted: {Type: EventRunCompleted, Host: "local", RunID: "r-42", Instance: "nas", At: at,
			Err: errors.New("1 container update(s) failed"),
			RunStats: &RunStats{Scanned: 3, Updated: 1, Failed: 1, Took: 41 * time.Second, Items: []RunItem{
				{Host: "local", Container: "web", OldTag: "1.2.0", NewTag: "1.3.0", Result: "updated", ReleaseURL: releaseURL},
				{Host: "local", Container: "db", Image: "postgres:16.4", Result: "rolled-back", Error: "exited 1"},
			}}},
		EventStarted: {Type: EventStarted, Instance: "nas", At: at, Startup: &StartupInfo{
			Version: "1.0.0", Hosts: []string{"local", "edge"}, Schedule: "@every 5m",
			NextRun: time.Date(2026, 10, 16, 4, 5, 0, 0, time.UTC), DryRun: true}},
		EventTest: {Type: EventTest, Instance: "nas", At: at},
	}
}

var stateWord = map[string]string{
	EventUpdateSuccess:     "updated",
	EventUpdateFailure:     "failed",
	EventAvailable:         "update available",
	EventNoUpdate:          "skipped",
	EventDenied:            "denied",
	EventRolledBack:        "rolled back",
	EventRecovered:         "recovered",
	EventCheckFailed:       "check failed",
	EventSelfUpdateStarted: "self-update started",
	EventSelfUpdateSuccess: "self-update completed",
	EventSelfUpdateFailure: "self-update failed",
	EventRunStarted:        "started",
	EventRunCompleted:      "run failed",
	EventStarted:           "started",
	EventStopping:          "stopping",
	EventTest:              "test",
}

type channelCase struct {
	name string
	make func(base string) Notifier
}

func humanChannels() []channelCase {
	return []channelCase{
		{"slack", func(u string) Notifier {
			return newSlack(config.NotifierConfig{URL: u + "/services/T0/B0/SECRETSLACKTOKEN"})
		}},
		{"discord", func(u string) Notifier {
			return newDiscord(config.NotifierConfig{URL: u + "/api/webhooks/1/SECRETDISCORDTOKEN"})
		}},
		{"teams", func(u string) Notifier {
			return newTeams(config.NotifierConfig{URL: u + "/workflows/run?sig=SECRETTEAMSSIGNATURE"})
		}},
		{"telegram", func(u string) Notifier {
			return newTelegram(config.NotifierConfig{URL: u, Token: "123456:SECRETTELEGRAMTOKEN", Channel: "-100"})
		}},
		{"ntfy", func(u string) Notifier {
			return newNtfy(config.NotifierConfig{URL: u + "/SECRETNTFYTOPIC", Token: "SECRETNTFYTOKEN"})
		}},
		{"gotify", func(u string) Notifier { return newGotify(config.NotifierConfig{URL: u, Token: "SECRETGOTIFYTOKEN"}) }},
		{"pushover", func(u string) Notifier {
			return newPushover(config.NotifierConfig{URL: u + "/1/messages.json", Token: "SECRETPUSHOVERAPP", User: "SECRETPUSHOVERUSER"})
		}},
		{"matrix", func(u string) Notifier {
			return newMatrix(config.NotifierConfig{URL: u, Token: "SECRETMATRIXTOKEN", Room: "!room:example.org"})
		}},
		{"apprise", func(u string) Notifier {
			return newApprise(config.NotifierConfig{URL: u, To: []string{"mailto://user:SECRETAPPRISEPASS@example.org", "tgram://123456:SECRETAPPRISEBOT/42"},
				Headers: map[string]string{"X-Proxy-Auth": "SECRETAPPRISEPROXY"}})
		}},
		{"webhook", func(u string) Notifier {
			return newWebhook(config.NotifierConfig{URL: u + "/hook/SECRETWEBHOOKPATH", Token: "SECRETWEBHOOKTOKEN",
				Headers: map[string]string{"X-Api-Key": "sk-live-SECRETWEBHOOKKEY"}})
		}},
	}
}

func delivered(c captured) string {
	text := c.body
	if strings.HasPrefix(strings.TrimSpace(c.body), "{") {
		var v any
		if json.Unmarshal([]byte(c.body), &v) == nil {
			text = flatten(v)
		}
	} else if q, err := url.ParseQuery(c.body); err == nil && len(q) > 1 {
		text = flatten(q)
	}
	for k, vs := range c.header {
		text += "\n" + k + ": " + strings.Join(vs, ",")
	}
	return text
}

func flatten(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		var parts []string
		for k, e := range x {
			parts = append(parts, k+"="+flatten(e))
		}
		return strings.Join(parts, "\n")
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, flatten(e))
		}
		return strings.Join(parts, "\n")
	case url.Values:
		var parts []string
		for k, vs := range x {
			parts = append(parts, k+"="+strings.Join(vs, ","))
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEveryChannelRendersEveryEvent(t *testing.T) {
	samples := sampleEvents()
	for _, typ := range knownEvents {
		if _, ok := samples[typ]; !ok {
			t.Errorf("no sample of %s: every known event must be rendered", typ)
		}
		if stateWord[typ] == "" {
			t.Errorf("no state word for %s", typ)
		}
		if ev := samples[typ]; formatTitle(ev) == "dockpaum: "+typ || cardTitle(ev) == "dockpaum: "+typ || emailHeadline(ev) == "dockpaum: "+typ {
			t.Errorf("%s has no title of its own", typ)
		}
	}
	for _, ch := range humanChannels() {
		for typ, ev := range sampleEvents() {
			t.Run(ch.name+"/"+typ, func(t *testing.T) {
				e := newEndpoint(t)
				if err := ch.make(e.URL).Notify(context.Background(), ev); err != nil {
					t.Fatal(err)
				}
				text := strings.ToLower(delivered(e.last()))
				if ch.name == "ntfy" {
					text += strings.ToLower(formatTitle(ev))
				}
				if ch.name == "webhook" {
					if !strings.Contains(text, typ) {
						t.Fatalf("webhook payload lacks the event type: %s", text)
					}
					return
				}
				if !strings.Contains(text, stateWord[typ]) {
					t.Errorf("message does not state %q:\n%s", stateWord[typ], text)
				}
				for _, want := range []string{ev.Container, ev.Instance, ev.RunID} {
					if want != "" && !strings.Contains(text, strings.ToLower(want)) {
						t.Errorf("message lacks %q:\n%s", want, text)
					}
				}
				if ev.ReleaseURL != "" && !strings.Contains(text, strings.ToLower(releaseURL)) {
					t.Errorf("message lacks the release notes link:\n%s", text)
				}
				if typ == EventRunCompleted && !strings.Contains(text, strings.ToLower(releaseURL)) {
					t.Errorf("digest lacks the item's release notes link:\n%s", text)
				}
				if typ == EventStarted && !strings.Contains(text, "1.0.0") {
					t.Errorf("startup message lacks the version:\n%s", text)
				}
			})
		}
	}
}

func TestSecretsNeverLeakIntoErrors(t *testing.T) {
	secretRe := "SECRET"
	t.Run("invalid url", func(t *testing.T) {
		for _, ch := range humanChannels() {
			err := ch.make("https://hooks.example.com/x\r").Notify(context.Background(), Event{Type: EventRunStarted})
			if err == nil {
				t.Errorf("%s: expected an error for an invalid url", ch.name)
				continue
			}
			if strings.Contains(err.Error(), secretRe) || strings.Contains(err.Error(), "hooks.example.com") {
				t.Errorf("%s: error leaks the url: %v", ch.name, err)
			}
		}
	})
	t.Run("transport error", func(t *testing.T) {
		for _, ch := range humanChannels() {
			err := ch.make("http://127.0.0.1:1").Notify(context.Background(), Event{Type: EventRunStarted})
			if err == nil {
				t.Errorf("%s: expected a transport error", ch.name)
				continue
			}
			if strings.Contains(err.Error(), secretRe) {
				t.Errorf("%s: error leaks a secret: %v", ch.name, err)
			}
		}
	})
	t.Run("server echoes the request", func(t *testing.T) {
		basicAuth := channelCase{"webhook basic auth", func(u string) Notifier {
			return newWebhook(config.NotifierConfig{URL: u + "/hook",
				Headers: map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("robot:SECRETBASICPASS"))}})
		}}
		for _, ch := range append(humanChannels(), basicAuth) {
			echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				msg := r.URL.String() + " " + string(b)
				for _, vs := range r.Header {
					msg += " " + strings.Join(vs, " ")
				}
				if user, pass, ok := r.BasicAuth(); ok {
					msg += " " + user + ":" + pass + " " + pass
				}
				if strings.Contains(string(b), "tgram://") {
					msg += " 123456:SECRETAPPRISEBOT SECRETAPPRISEBOT SECRETAPPRISEPASS"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
			}))
			err := ch.make(echo.URL).Notify(context.Background(), Event{Type: EventRunStarted})
			echo.Close()
			if err == nil {
				t.Errorf("%s: a 400 must be an error", ch.name)
				continue
			}
			if !strings.HasPrefix(err.Error(), "HTTP 400") {
				t.Errorf("%s: error = %v", ch.name, err)
			}
			if strings.Contains(err.Error(), secretRe) {
				t.Errorf("%s: error leaks a secret: %v", ch.name, err)
			}
		}
	})
}

func TestEscapingPerDestination(t *testing.T) {
	t.Run("slack", func(t *testing.T) {
		text := slackText(hostile)
		for _, bad := range []string{"<!channel>", "<@U123>", "<script>", "<b>"} {
			if strings.Contains(text, bad) {
				t.Errorf("slack text keeps %q:\n%s", bad, text)
			}
		}
		if !strings.Contains(text, "&lt;!channel&gt;") || !strings.Contains(text, "&amp;") {
			t.Errorf("slack text not entity-encoded:\n%s", text)
		}
		if n := strings.Count(text, "```"); n != 2 {
			t.Errorf("a value closed the code block: %d fences in\n%s", n, text)
		}
	})
	t.Run("slack links", func(t *testing.T) {
		ev := sampleEvents()[EventRunCompleted]
		ev.ReleaseURL = releaseURL + "?a=1&b=2"
		text := slackText(ev)
		if !strings.Contains(text, "\n<"+releaseURL+"?a=1&amp;b=2|Release notes>") {
			t.Errorf("event link missing:\n%s", text)
		}
		if !strings.Contains(text, "|Release notes: web 1.3.0>") {
			t.Errorf("digest link missing:\n%s", text)
		}
		if strings.Contains(strings.SplitN(text, "```", 3)[1], "https://") {
			t.Errorf("links belong below the code block:\n%s", text)
		}
	})
	t.Run("discord", func(t *testing.T) {
		p := discordPayload(hostile)
		if am, _ := json.Marshal(p["allowed_mentions"]); string(am) != `{"parse":[]}` {
			t.Errorf("allowed_mentions = %s", am)
		}
		embed := p["embeds"].([]any)[0].(map[string]any)
		desc := embed["description"].(string)
		if n := strings.Count(desc, "```"); n != 2 {
			t.Errorf("the error closed its code block: %q", desc)
		}
		var reason string
		for _, f := range embed["fields"].([]discordField) {
			if f.Name == "Reason" {
				reason = f.Value
			}
		}
		for _, bad := range []string{"[click](", "*bold*", "```", "<script>"} {
			if strings.Contains(reason, bad) {
				t.Errorf("reason keeps markup %q: %s", bad, reason)
			}
		}
		if !strings.Contains(reason, `\[click\]`) || !strings.Contains(reason, `\@everyone`) {
			t.Errorf("reason not escaped: %s", reason)
		}
	})
	t.Run("discord inline code", func(t *testing.T) {
		ev := Event{Type: EventUpdateSuccess, Container: "web", Image: "img`with`ticks", Policy: "tag-semver"}
		for _, f := range discordFields(ev) {
			if f.Name == "Image" && f.Value != "img\\`with\\`ticks" {
				t.Errorf("image = %q", f.Value)
			}
			if f.Name == "Policy" && f.Value != "`tag-semver`" {
				t.Errorf("policy = %q", f.Value)
			}
		}
	})
	t.Run("discord long values keep their markup", func(t *testing.T) {
		long := strings.Repeat("a", 1100)
		ev := Event{
			Type: EventNoUpdate, Container: "web",
			Image:  long,
			Policy: "x`" + long,
			Reason: strings.Repeat("_", 2000),
			OldTag: strings.Repeat("1", 700), NewTag: strings.Repeat("2", 700),
		}
		for _, f := range discordFields(ev) {
			if len(f.Value) > discordFieldMax {
				t.Errorf("%s is %d bytes", f.Name, len(f.Value))
			}
			if strings.Count(f.Value, "`")%2 != 0 && !strings.Contains(f.Value, "\\`") {
				t.Errorf("%s lost a backtick of its code span: ...%s", f.Name, f.Value[len(f.Value)-20:])
			}
			body := strings.TrimSuffix(f.Value, "…")
			if trailing := len(body) - len(strings.TrimRight(body, `\`)); trailing%2 != 0 {
				t.Errorf("%s ends in a split escape: ...%s", f.Name, f.Value[len(f.Value)-20:])
			}
			switch f.Name {
			case "Image":
				if !strings.HasPrefix(f.Value, "`a") || !strings.HasSuffix(f.Value, "…`") {
					t.Errorf("image = ...%s", f.Value[len(f.Value)-20:])
				}
			case "Tag":
				if strings.Count(f.Value, "`") != 4 || !strings.Contains(f.Value, "…` → `2") {
					t.Errorf("tag = %s", f.Value)
				}
			case "Reason":
				if !strings.HasSuffix(f.Value, `\_…`) {
					t.Errorf("reason = ...%s", f.Value[len(f.Value)-20:])
				}
			}
		}
		item := RunItem{Container: strings.Repeat("c", 900), Result: "updated", ReleaseURL: releaseURL}
		line := discordItemLine(item)
		if len(line) > discordLineMax || strings.Count(line, "`") != 2 || strings.Contains(line, "](") {
			t.Errorf("digest line = %s", line)
		}
		if l := discordItemLine(RunItem{Container: "web", Result: "updated", ReleaseURL: releaseURL}); !strings.HasSuffix(l, "("+releaseURL+")") {
			t.Errorf("short digest line lost its link: %s", l)
		}
	})
	t.Run("teams", func(t *testing.T) {
		b, _ := json.Marshal(teamsPayload(hostile))
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		text := flatten(p)
		if strings.Contains(text, "](") {
			t.Errorf("a value forms a masked link:\n%s", text)
		}
		if !strings.Contains(text, "[click]\u200b(https://evil.example)") {
			t.Errorf("link text not kept visible:\n%s", text)
		}
	})
	t.Run("matrix", func(t *testing.T) {
		h := matrixHTML(hostile)
		for _, bad := range []string{"<!channel>", "<script>", "<b>more"} {
			if strings.Contains(h, bad) {
				t.Errorf("formatted body keeps %q: %s", bad, h)
			}
		}
		if !strings.Contains(h, "&lt;script&gt;") {
			t.Errorf("formatted body not escaped: %s", h)
		}
	})
	t.Run("telegram stays plain text", func(t *testing.T) {
		e := newEndpoint(t)
		_ = newTelegram(config.NotifierConfig{URL: e.URL, Token: "1:x", Channel: "1"}).Notify(context.Background(), hostile)
		if strings.Contains(e.last().body, "parse_mode") {
			t.Fatal("telegram must not enable a parse mode")
		}
	})
}

func TestDiscordDigestTruncatesWholeLines(t *testing.T) {
	var items []RunItem
	for i := 0; i < 60; i++ {
		items = append(items, RunItem{Host: "local", Container: "service-with-a-long-name-" + itoa(i), OldTag: "1.0.0", NewTag: "1.0.1", Result: "updated"})
	}
	for _, f := range discordFields(Event{Type: EventRunCompleted, RunStats: &RunStats{Items: items}}) {
		if f.Name != "Changes" {
			continue
		}
		if len(f.Value) > discordFieldMax {
			t.Fatalf("field is %d bytes", len(f.Value))
		}
		if strings.Count(f.Value, "`")%2 != 0 {
			t.Fatalf("a code span was cut:\n%s", f.Value)
		}
		if !strings.Contains(f.Value, "more") {
			t.Fatalf("the omission is not stated:\n%s", f.Value)
		}
		return
	}
	t.Fatal("no Changes field")
}

func TestDiscordEmbed(t *testing.T) {
	ev := sampleEvents()[EventUpdateSuccess]
	embed := discordPayload(ev)["embeds"].([]any)[0].(map[string]any)
	if embed["url"] != releaseURL {
		t.Errorf("title link = %v", embed["url"])
	}
	if f := embed["footer"].(map[string]any)["text"]; f != "dockpaum · nas · run r-42" {
		t.Errorf("footer = %v", f)
	}
	var names []string
	for _, f := range embed["fields"].([]discordField) {
		names = append(names, f.Name)
		if f.Name == "Digest" && f.Value != "`0123456789ab → fedcba987654`" {
			t.Errorf("digest = %q", f.Value)
		}
		if f.Name == "Release notes" && f.Value != "[Release notes]("+releaseURL+")" {
			t.Errorf("release notes = %q", f.Value)
		}
	}
	if !strings.Contains(strings.Join(names, ","), "Tag,Digest,Policy") {
		t.Errorf("fields = %v", names)
	}
	if discordLink("x", "https://e.example/a_(b)") != "[x](https://e.example/a_%28b%29)" {
		t.Error("parentheses must not end a masked link")
	}
}

func TestPushover(t *testing.T) {
	e := newEndpoint(t)
	p := newPushover(config.NotifierConfig{URL: e.URL + "/1/messages.json", Token: "app-token", User: "user-key", Channel: "phone"})
	ev := sampleEvents()[EventUpdateFailure]
	ev.ReleaseURL = releaseURL
	if err := p.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	c := e.last()
	if c.method != http.MethodPost || c.path != "/1/messages.json" || c.header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("request = %s %s %v", c.method, c.path, c.header)
	}
	form, _ := url.ParseQuery(c.body)
	want := map[string]string{
		"token": "app-token", "user": "user-key", "device": "phone", "priority": "0",
		"title": "🔥 dockpaum: db update failed", "url": releaseURL, "url_title": "Release notes",
		"timestamp": strconv.FormatInt(ev.At.Unix(), 10),
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	if !strings.Contains(form.Get("message"), "error: rolled back") {
		t.Errorf("message = %q", form.Get("message"))
	}

	long := Event{Type: EventUpdateSuccess, Container: "web", Err: nil, Reason: strings.Repeat("x", 3000)}
	if err := p.Notify(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	form, _ = url.ParseQuery(e.last().body)
	if len(form.Get("message")) > pushoverMessageMax || form.Get("priority") != "0" || form.Has("url") {
		t.Errorf("limits not applied: %d bytes, priority %q, url %q", len(form.Get("message")), form.Get("priority"), form.Get("url"))
	}

	e.answer(http.StatusBadRequest, "application/json", `{"user":"invalid","errors":["user identifier is not a valid user, group, or subscribed user key, see https://pushover.net/api#identifiers"],"status":0}`)
	err := p.Notify(context.Background(), ev)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400: user identifier is not a valid user") {
		t.Fatalf("error = %v", err)
	}
	if d := newPushover(config.NotifierConfig{}); d.endpoint != pushoverAPI {
		t.Errorf("default endpoint = %q", d.endpoint)
	}
}

func TestMatrix(t *testing.T) {
	var mu sync.Mutex
	var lookups int
	var sends []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token passed."}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/_matrix/client/v3/directory/room/%23ops:example.org":
			lookups++
			_, _ = io.WriteString(w, `{"room_id":"!abc:example.org","servers":["example.org"]}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Room alias not found"}`)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!abc:example.org/send/m.room.message/"):
			sends = append(sends, captured{method: r.Method, path: r.URL.Path, body: string(b)})
			_, _ = io.WriteString(w, `{"event_id":"$e"}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errcode":"M_FORBIDDEN","error":"User @bot:example.org not in room"}`)
		}
	}))
	defer srv.Close()

	m := newMatrix(config.NotifierConfig{URL: srv.URL + "/", Token: "tok", Room: "#ops:example.org"})
	ev := sampleEvents()[EventUpdateSuccess]
	for i := 0; i < 2; i++ {
		if err := m.Notify(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if lookups != 1 || len(sends) != 2 {
		t.Fatalf("alias lookups = %d (want 1, cached), sends = %d", lookups, len(sends))
	}
	if sends[0].path == sends[1].path {
		t.Fatal("transaction IDs must differ per message")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(sends[0].body), &body)
	mu.Unlock()
	if body["msgtype"] != "m.text" || body["format"] != "org.matrix.custom.html" {
		t.Errorf("content = %v", body)
	}
	if mm, ok := body["m.mentions"].(map[string]any); !ok || len(mm) != 0 {
		t.Errorf("m.mentions = %v, want an empty object", body["m.mentions"])
	}
	if fb := body["formatted_body"].(string); !strings.Contains(fb, `<a href="`+releaseURL+`">`) || !strings.Contains(fb, "<strong>") {
		t.Errorf("formatted_body = %s", fb)
	}
	if b := body["body"].(string); !strings.Contains(b, "tag: 1.2.0 → 1.3.0") || !strings.Contains(b, "release notes: "+releaseURL) {
		t.Errorf("body = %s", b)
	}

	cases := []struct {
		name, room, token, want string
	}{
		{"room id needs no lookup but membership", "!other:example.org", "tok", "HTTP 403: M_FORBIDDEN: User @bot:example.org not in room"},
		{"unknown alias", "#nope:example.org", "tok", "matrix: resolve room alias: HTTP 404: M_NOT_FOUND: Room alias not found"},
		{"bad token", "!abc:example.org", "wrong", "HTTP 401: M_UNKNOWN_TOKEN: Invalid access token passed."},
		{"no room", "", "tok", "matrix: room is not configured"},
	}
	for _, tc := range cases {
		err := newMatrix(config.NotifierConfig{URL: srv.URL, Token: tc.token, Room: tc.room}).Notify(context.Background(), ev)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestApprise(t *testing.T) {
	cases := []struct {
		name     string
		cfg      func(base string) config.NotifierConfig
		ev       Event
		wantPath string
		want     map[string]any
		absent   []string
	}{
		{
			"stateless", func(u string) config.NotifierConfig {
				return config.NotifierConfig{URL: u, To: []string{"tgram://bot/chat", "ntfy://ntfy.example/topic"}}
			},
			sampleEvents()[EventUpdateFailure], "/notify/",
			map[string]any{"urls": "tgram://bot/chat, ntfy://ntfy.example/topic", "type": "failure", "format": "text"},
			[]string{"tag"},
		},
		{
			"stateless /notify without slash", func(u string) config.NotifierConfig {
				return config.NotifierConfig{URL: u + "/notify", To: []string{"json://hook"}}
			},
			sampleEvents()[EventUpdateSuccess], "/notify/",
			map[string]any{"type": "success", "title": "✅ dockpaum: web updated"},
			nil,
		},
		{
			"stateful with tags", func(u string) config.NotifierConfig {
				return config.NotifierConfig{URL: u + "/notify/homelab", Channel: "ops", Headers: map[string]string{"X-Proxy-Token": "p"}}
			},
			sampleEvents()[EventNoUpdate], "/notify/homelab",
			map[string]any{"tag": "ops", "type": "info"},
			[]string{"urls"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEndpoint(t)
			if err := newApprise(tc.cfg(e.URL)).Notify(context.Background(), tc.ev); err != nil {
				t.Fatal(err)
			}
			c := e.last()
			if c.path != tc.wantPath {
				t.Errorf("path = %q, want %q", c.path, tc.wantPath)
			}
			var body map[string]any
			_ = json.Unmarshal([]byte(c.body), &body)
			for k, v := range tc.want {
				if body[k] != v {
					t.Errorf("%s = %v, want %v", k, body[k], v)
				}
			}
			for _, k := range tc.absent {
				if _, ok := body[k]; ok {
					t.Errorf("%s must be absent: %v", k, body)
				}
			}
			if !strings.Contains(body["body"].(string), tc.ev.Container) {
				t.Errorf("body = %v", body["body"])
			}
			if tc.name == "stateful with tags" && c.header.Get("X-Proxy-Token") != "p" {
				t.Error("configured headers are not sent")
			}
		})
	}
}

func TestTeamsWorkflowsCard(t *testing.T) {
	e := newEndpoint(t)
	e.answer(http.StatusAccepted, "", "")
	if err := newTeams(config.NotifierConfig{URL: e.URL + "/workflows/x/triggers/manual/paths/invoke?sig=s"}).Notify(context.Background(), sampleEvents()[EventUpdateSuccess]); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string          `json:"contentType"`
			ContentURL  *string         `json:"contentUrl"`
			Content     json.RawMessage `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(e.last().body), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "message" || len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" || msg.Attachments[0].ContentURL != nil {
		t.Fatalf("envelope = %+v", msg)
	}
	var card struct {
		Type    string `json:"type"`
		Version string `json:"version"`
		Body    []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Color string `json:"color"`
			Facts []struct{ Title, Value string }
		} `json:"body"`
		Actions []struct{ Type, Title, URL string } `json:"actions"`
	}
	if err := json.Unmarshal(msg.Attachments[0].Content, &card); err != nil {
		t.Fatal(err)
	}
	if card.Type != "AdaptiveCard" || card.Version != "1.4" || card.Body[0].Text != "✅ dockpaum: web updated" || card.Body[0].Color != "Good" {
		t.Fatalf("card = %+v", card)
	}
	if len(card.Actions) != 1 || card.Actions[0].Type != "Action.OpenUrl" || card.Actions[0].URL != releaseURL {
		t.Fatalf("actions = %+v", card.Actions)
	}
	facts := map[string]string{}
	for _, f := range card.Body[1].Facts {
		facts[f.Title] = f.Value
	}
	if facts["Instance"] != "nas" || facts["Run"] != "r-42" || facts["Tag"] != "1.2.0 → 1.3.0" {
		t.Fatalf("facts = %v", facts)
	}
}

func TestWebhookTokenAndFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.NotifierConfig
		wantAut string
	}{
		{"token becomes a bearer header", config.NotifierConfig{Token: "s3cret"}, "Bearer s3cret"},
		{"an explicit header wins", config.NotifierConfig{Token: "s3cret", Headers: map[string]string{"authorization": "Basic abc"}}, "Basic abc"},
		{"no token, no header", config.NotifierConfig{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEndpoint(t)
			tc.cfg.URL = e.URL
			if err := newWebhook(tc.cfg).Notify(context.Background(), sampleEvents()[EventUpdateSuccess]); err != nil {
				t.Fatal(err)
			}
			if got := e.last().header.Get("Authorization"); got != tc.wantAut {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantAut)
			}
		})
	}

	e := newEndpoint(t)
	wh := newWebhook(config.NotifierConfig{URL: e.URL})
	if err := wh.Notify(context.Background(), sampleEvents()[EventUpdateSuccess]); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(e.last().body), &p)
	for k, v := range map[string]string{"run_id": "r-42", "instance": "nas", "release_url": releaseURL, "type": EventUpdateSuccess} {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}

	if err := wh.Notify(context.Background(), sampleEvents()[EventStarted]); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(e.last().body), &p)
	st, _ := p["startup"].(map[string]any)
	if st["version"] != "1.0.0" || st["dry_run"] != true || st["next_run"] != "2026-10-16T04:05:00Z" || st["schedule"] != "@every 5m" {
		t.Errorf("startup = %v", p["startup"])
	}

	b, _ := json.Marshal(StartupInfo{})
	if string(b) != `{"dry_run":false}` {
		t.Errorf("empty startup = %s", b)
	}

	if err := wh.Notify(context.Background(), Event{Type: EventRunCompleted, RunStats: &RunStats{Items: []RunItem{
		{Host: "h", Container: "web", NewDigest: "sha256:b", Result: "monitor-only", ReleaseURL: releaseURL},
	}}}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(e.last().body), &p)
	item := p["stats"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if item["new_digest"] != "sha256:b" || item["release_url"] != releaseURL {
		t.Errorf("item = %v", item)
	}
}

func TestPushLinksAndPriority(t *testing.T) {
	e := newEndpoint(t)
	ev := sampleEvents()[EventUpdateSuccess]
	if err := newNtfy(config.NotifierConfig{URL: e.URL + "/topic"}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if h := e.last().header; h.Get("Click") != releaseURL || h.Get("Priority") != "" {
		t.Errorf("ntfy headers = %v", h)
	}
	if err := newGotify(config.NotifierConfig{URL: e.URL, Token: "t"}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	var g map[string]any
	_ = json.Unmarshal([]byte(e.last().body), &g)
	click, _ := json.Marshal(g["extras"])
	if string(click) != `{"client::notification":{"click":{"url":"`+releaseURL+`"}}}` {
		t.Errorf("gotify extras = %s", click)
	}
	ev.ReleaseURL = "javascript:alert(1)"
	if err := newNtfy(config.NotifierConfig{URL: e.URL + "/topic"}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if e.last().header.Get("Click") != "" {
		t.Error("a non-http link must not become a click action")
	}
}

func TestResponseDetail(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.example.org/bot123456:SECRETTOKEN/send?sig=SECRETSIGNATURE&v=ABC%2BSECRETSIG%2Fxyz", nil)
	req.Header.Set("Authorization", "Bearer SECRETBEARER")
	req.Header.Set("X-Api-Key", "sk-live-SECRETKEY123")
	req.Header.Set("Content-Type", "application/json")
	cases := []struct {
		name, ctype, body, want string
	}{
		{"matrix", "application/json", `{"errcode":"M_FORBIDDEN","error":"not in room"}`, "M_FORBIDDEN: not in room"},
		{"pushover", "application/json; charset=utf-8", `{"errors":["a","b"],"status":0}`, "a; b"},
		{"teams nested", "application/json", `{"error":{"code":"TriggerInputSchemaMismatch","message":"bad body"}}`, "TriggerInputSchemaMismatch: bad body"},
		{"telegram", "application/json", `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, "Bad Request: chat not found"},
		{"discord numeric code ignored", "application/json", `{"message":"Unknown Webhook","code":10015}`, "Unknown Webhook"},
		{"slack code in html", "text/html", "invalid_token", "invalid_token"},
		{"short plain text", "text/plain; charset=utf-8", "topic is reserved", "topic is reserved"},
		{"html page is ignored", "text/html", "<html>Cannot POST /bot123456:SECRETTOKEN/send</html>", ""},
		{"long plain text is ignored", "text/plain", strings.Repeat("x ", 200), ""},
		{"json sent as text", "text/plain", `{"errcode":"M_LIMIT_EXCEEDED","error":"Too many requests","retry_after_ms":2000}`, "M_LIMIT_EXCEEDED: Too many requests"},
		{"echoed secrets are scrubbed", "application/json", `{"error":"bad token SECRETBEARER for /bot123456:SECRETTOKEN/send?sig=SECRETSIGNATURE and SECRETEXTRA"}`, "bad token [redacted] for [redacted]?[redacted] and [redacted]"},
		{"encoded query values are scrubbed", "application/json", `{"error":"bad signature v=ABC%2BSECRETSIG%2Fxyz and ABC%2BSECRETSIG%2Fxyz"}`, "bad signature [redacted] and [redacted]"},
		{"configured headers are scrubbed", "application/json", `{"detail":"invalid api key sk-live-SECRETKEY123"}`, "invalid api key [redacted]"},
		{"content headers are kept", "application/json", `{"error":"unsupported content type application/json"}`, "unsupported content type application/json"},
		{"control characters", "application/json", "{\"error\":\"line1\\nlevel=error msg=forged\"}", "line1 level=error msg=forged"},
		{"empty", "application/json", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: 400, Header: http.Header{"Content-Type": {tc.ctype}},
				Body: io.NopCloser(strings.NewReader(tc.body)), Request: req,
			}
			if got := responseDetail(resp, []string{"SECRETEXTRA"}); got != tc.want {
				t.Fatalf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScrubThresholds(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://gotify.example/message", nil)
	req.Header.Set("X-Gotify-Key", "k3y9")
	got := scrub("message rejected: key k3y9 and pin 0042 are invalid", req, []string{"0042"})
	if got != "message rejected: key [redacted] and pin [redacted] are invalid" {
		t.Fatalf("scrub = %q", got)
	}
}

func TestURLSecrets(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"tgram://123456:SECRETBOTTOKEN/42", []string{"123456:SECRETBOTTOKEN", "SECRETBOTTOKEN"}},
		{"mailto://alice:pa%2Fss@smtp.example.org", []string{"alice:pa%2Fss", "alice", "pa%2Fss", "pa/ss"}},
		{"discord://123456789/SECRETWEBHOOKTOKEN?avatar=no", []string{"123456789", "SECRETWEBHOOKTOKEN"}},
	}
	for _, tc := range cases {
		got := strings.Join(urlSecrets(tc.raw), "\n")
		for _, w := range append(tc.want, tc.raw) {
			if !strings.Contains("\n"+got+"\n", "\n"+w+"\n") {
				t.Errorf("%s: %q missing from %q", tc.raw, w, got)
			}
		}
		if strings.Contains("\n"+got+"\n", "\nno\n") || strings.Contains("\n"+got+"\n", "\n42\n") {
			t.Errorf("%s: short parts are not secrets: %q", tc.raw, got)
		}
	}
	if got := headerSecrets("Basic " + base64.StdEncoding.EncodeToString([]byte("robot:pw1234"))); !strings.Contains(strings.Join(got, ","), "robot:pw1234,robot,pw1234") {
		t.Errorf("basic credentials = %q", got)
	}
}

func TestNewRequestRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"discord://token@id", "ftp://example.org/x", "/relative", "https://"} {
		if _, err := newRequest(context.Background(), http.MethodPost, raw, nil); !errors.Is(err, errInvalidURL) {
			t.Errorf("%q: err = %v", raw, err)
		} else if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "example.org") {
			t.Errorf("%q: error echoes the url: %v", raw, err)
		}
	}
}
