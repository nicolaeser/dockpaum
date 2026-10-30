package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFormatTitleBody(t *testing.T) {
	if got := formatTitle(Event{Type: EventUpdateSuccess}); got == "" {
		t.Fatal("empty title")
	}
	body := formatBody(Event{
		Type: EventUpdateSuccess, Host: "h1", Container: "c1", Image: "img:1",
		OldTag: "1.0.0", NewTag: "1.0.1", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb",
		Policy: "default",
	})
	for _, want := range []string{"h1", "c1", "img:1", "1.0.0 → 1.0.1", "default"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestShortAndItoa(t *testing.T) {
	if short("sha256:0123456789abcdef") != "0123456789ab" {
		t.Errorf("short digest = %q", short("sha256:0123456789abcdef"))
	}
	if itoa(0) != "0" || itoa(42) != "42" || itoa(-7) != "-7" {
		t.Errorf("itoa wrong: %q %q %q", itoa(0), itoa(42), itoa(-7))
	}
}

func TestWebhookDelivery(t *testing.T) {
	var got webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	wh := newWebhook(config.NotifierConfig{URL: srv.URL})
	err := wh.Notify(context.Background(), Event{
		Type: EventUpdateSuccess, Container: "c1", Image: "img:1", NewDigest: "sha256:bbbb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != EventUpdateSuccess || got.Container != "c1" || got.NewDigest != "sha256:bbbb" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestSlackAndDiscordShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string) Notifier
		key  string
	}{
		{"slack", func(u string) Notifier { return newSlack(config.NotifierConfig{URL: u, Channel: "#ops"}) }, "text"},
		{"discord", func(u string) Notifier { return newDiscord(config.NotifierConfig{URL: u}) }, "embeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(200)
			}))
			defer srv.Close()
			if err := tc.make(srv.URL).Notify(context.Background(), Event{Type: EventUpdateSuccess, Container: "c1"}); err != nil {
				t.Fatal(err)
			}
			if _, ok := body[tc.key]; !ok {
				t.Fatalf("%s payload missing %q: %+v", tc.name, tc.key, body)
			}
		})
	}
}

func TestCheckHTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if e := checkHTTP(resp, err); e == nil {
		t.Fatal("expected error on 500")
	}
}

func TestMultiEventFiltering(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := Build([]config.NotifierConfig{
		{Type: "webhook", Name: "only-success", URL: srv.URL, Events: []string{"update.success"}},
	}, discardLogger())

	m.Notify(context.Background(), Event{Type: EventRunStarted})
	m.Notify(context.Background(), Event{Type: EventUpdateSuccess})
	m.Wait(3 * time.Second)

	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("expected exactly 1 delivery (filtered), got %d", n)
	}
}

type fakeSink struct{ n int32 }

func (f *fakeSink) WithLabelValues(...string) interface{ Inc() } { return f }
func (f *fakeSink) Inc()                                         { atomic.AddInt32(&f.n, 1) }

func TestMultiFailureSink(t *testing.T) {
	m := Build([]config.NotifierConfig{
		{Type: "webhook", Name: "dead", URL: "http://127.0.0.1:1/nope", Events: []string{"*"}},
	}, discardLogger())
	sink := &fakeSink{}
	m.AttachFailureSink(sink)
	m.Notify(context.Background(), Event{Type: EventUpdateSuccess})
	m.Wait(3 * time.Second)
	if atomic.LoadInt32(&sink.n) == 0 {
		t.Fatal("failure sink not incremented on delivery failure")
	}
}

func TestRunCompletedWithErrorIsStatedAsFailed(t *testing.T) {
	failed := Event{
		Type: EventRunCompleted, Err: errors.New("pre-run hook: exit status 3"),
		RunStats: &RunStats{},
	}
	ok := Event{Type: EventRunCompleted, RunStats: &RunStats{}}

	for name, got := range map[string]string{
		"plain title":    formatTitle(failed),
		"discord title":  discordTitle(failed),
		"email subject":  emailSubject(failed),
		"email headline": emailHeadline(failed),
		"email title":    emailTitleLine(failed),
	} {
		if !strings.Contains(strings.ToLower(got), "run failed") {
			t.Errorf("%s = %q, want the failure stated in text", name, got)
		}
	}
	if !strings.Contains(formatBody(failed), "pre-run hook: exit status 3") {
		t.Error("plain body must carry the run error")
	}
	if strings.Contains(strings.ToLower(formatTitle(ok)), "failed") || discordColor(ok) == discordAmber {
		t.Error("a clean run must not read as failed")
	}
	if discordColor(failed) != discordAmber || emailColor(failed) == emailColor(ok) {
		t.Error("a failed run should use the degraded accent")
	}
}

func TestNewChannelsPayloads(t *testing.T) {
	type got struct {
		path    string
		headers http.Header
		body    string
	}
	var last got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = got{r.URL.Path, r.Header.Clone(), string(b)}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	ev := Event{
		Type: EventRunCompleted, Host: "local", Err: errors.New("1 container update(s) failed"),
		RunStats: &RunStats{Updated: 1, Failed: 1, Items: []RunItem{
			{Host: "local", Container: "web", OldTag: "1.2", NewTag: "1.3", Result: "updated"},
			{Host: "local", Container: "db", Result: "check-failed", Error: "registry down"},
		}},
	}
	cases := []struct {
		name  string
		n     Notifier
		check func(got) bool
	}{
		{"telegram", newTelegram(config.NotifierConfig{URL: srv.URL, Token: "123:abc", Channel: "-100"}), func(g got) bool {
			return g.path == "/bot123:abc/sendMessage" && strings.Contains(g.body, `"chat_id":"-100"`) && strings.Contains(g.body, "local/web: 1.2 → 1.3 (updated)")
		}},
		{"ntfy", newNtfy(config.NotifierConfig{URL: srv.URL + "/dockpaum", Token: "tk"}), func(g got) bool {
			return g.path == "/dockpaum" && g.headers.Get("Priority") == "high" && g.headers.Get("Authorization") == "Bearer tk" &&
				strings.HasPrefix(g.headers.Get("Title"), "=?utf-8?q?") && strings.Contains(g.body, "local/db (check-failed)")
		}},
		{"gotify", newGotify(config.NotifierConfig{URL: srv.URL + "/", Token: "app"}), func(g got) bool {
			return g.path == "/message" && g.headers.Get("X-Gotify-Key") == "app" && strings.Contains(g.body, `"priority":8`)
		}},
		{"teams", newTeams(config.NotifierConfig{URL: srv.URL + "/workflow"}), func(g got) bool {
			return strings.Contains(g.body, `"application/vnd.microsoft.card.adaptive"`) && strings.Contains(g.body, `"Attention"`) && strings.Contains(g.body, "local/web")
		}},
	}
	for _, c := range cases {
		if err := c.n.Notify(context.Background(), ev); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.check(last) {
			t.Errorf("%s payload: path=%s headers=%v body=%s", c.name, last.path, last.headers, last.body)
		}
	}
}

func TestWebhookDigestItems(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer srv.Close()
	ev := Event{Type: EventRunCompleted, RunStats: &RunStats{Items: []RunItem{{Host: "h", Container: "web", Result: "monitor-only"}}}}
	if err := newWebhook(config.NotifierConfig{URL: srv.URL}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	items := body["stats"].(map[string]any)["items"].([]any)
	if items[0].(map[string]any)["result"] != "monitor-only" {
		t.Fatalf("stats.items = %v", items)
	}
}

func TestEmailRespectsContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	e := newEmail(config.NotifierConfig{From: "a@b.c", To: []string{"x@y.z"}, SMTP: &config.SMTPConfig{Host: "127.0.0.1", Port: port}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := e.Notify(ctx, Event{Type: EventUpdateSuccess, Container: "c"}); err == nil {
		t.Fatal("expected an error from a silent relay")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("email delivery ignored the context deadline (%s)", took)
	}
}

func TestTransportErrorsDoNotLeakSecretURLs(t *testing.T) {
	n := newTelegram(config.NotifierConfig{URL: "http://127.0.0.1:1", Token: "123456:SECRET-TOKEN", Channel: "1"})
	err := n.Notify(context.Background(), Event{Type: EventRunStarted})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), "127.0.0.1:1/bot") {
		t.Fatalf("error leaks the secret URL: %v", err)
	}
}

func TestNotifiersRefuseCrossOriginRedirects(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gotify-Key") != "" || r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer evil.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	for _, n := range []Notifier{
		newGotify(config.NotifierConfig{URL: origin.URL, Token: "app-secret"}),
		newNtfy(config.NotifierConfig{URL: origin.URL + "/topic", Token: "ntfy-secret"}),
		newWebhook(config.NotifierConfig{URL: origin.URL, Headers: map[string]string{"Authorization": "Bearer hook-secret"}}),
	} {
		if err := n.Notify(context.Background(), Event{Type: EventRunStarted}); err == nil {
			t.Errorf("%T followed a cross-origin redirect", n)
		}
	}
	if leaked.Load() {
		t.Fatal("credentials reached another origin")
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	for n := 0; n <= 13; n++ {
		got := truncate("ääääää", n)
		if !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("truncate(_, %d) = %q (%d bytes)", n, got, len(got))
		}
	}
	if truncate("short", 10) != "short" {
		t.Fatal("a fitting string must stay unchanged")
	}
}
