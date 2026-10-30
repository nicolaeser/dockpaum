package config

import (
	"reflect"
	"strings"
	"testing"
)

const (
	slackT = "T00000000"
	slackB = "B00000000"
	slackX = "XXXXXXXXXXXXXXXXXXXXXXXX"
)

func TestConvertServiceURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want []NotifierConfig
	}{
		{
			name: "discord",
			url:  "discord://tok-en_1@123456789",
			want: []NotifierConfig{{Type: "discord", URL: "https://discord.com/api/webhooks/123456789/tok-en_1"}},
		},
		{
			name: "discord thread",
			url:  "discord://tok@123?thread_id=987",
			want: []NotifierConfig{{Type: "discord", URL: "https://discord.com/api/webhooks/123/tok?thread_id=987"}},
		},
		{
			name: "slack hook",
			url:  "slack://hook:" + slackT + "-" + slackB + "-" + slackX + "@webhook",
			want: []NotifierConfig{{Type: "slack", URL: "https://hooks.slack.com/services/" + slackT + "/" + slackB + "/" + slackX}},
		},
		{
			name: "slack hook with channel host",
			url:  "slack://hook:" + slackT + "-" + slackB + "-" + slackX + "@ops?color=good",
			want: []NotifierConfig{{Type: "slack", URL: "https://hooks.slack.com/services/" + slackT + "/" + slackB + "/" + slackX, Channel: "ops"}},
		},
		{
			name: "slack legacy form",
			url:  "slack://" + slackT + "/" + slackB + "/" + slackX + "?channel=alerts",
			want: []NotifierConfig{{Type: "slack", URL: "https://hooks.slack.com/services/" + slackT + "/" + slackB + "/" + slackX, Channel: "alerts"}},
		},
		{
			name: "telegram, one notifier per chat",
			url:  "telegram://110201543:AAHdqTcvCH1vGWJxfSe@telegram?chats=-1001234,@news",
			want: []NotifierConfig{
				{Type: "telegram", Token: "110201543:AAHdqTcvCH1vGWJxfSe", Channel: "-1001234"}, // #nosec G101 -- dummy credentials of a test fixture
				{Type: "telegram", Token: "110201543:AAHdqTcvCH1vGWJxfSe", Channel: "@news"},    // #nosec G101 -- dummy credentials of a test fixture
			},
		},
		{
			name: "ntfy public",
			url:  "ntfy://ntfy.sh/my-topic?priority=5",
			want: []NotifierConfig{{Type: "ntfy", URL: "https://ntfy.sh/my-topic"}},
		},
		{
			name: "ntfy self-hosted over http with token",
			url:  "ntfy://ntfy.lan:8080/updates?scheme=http&token=tk_abc",
			want: []NotifierConfig{{Type: "ntfy", URL: "http://ntfy.lan:8080/updates", Token: "tk_abc"}},
		},
		{
			name: "ntfy token as password",
			url:  "ntfy://:tk_pw@ntfy.example.com/t?disabletls=yes",
			want: []NotifierConfig{{Type: "ntfy", URL: "http://ntfy.example.com/t", Token: "tk_pw"}},
		},
		{
			name: "gotify",
			url:  "gotify://gotify.example.com/AbCdEf123",
			want: []NotifierConfig{{Type: "gotify", URL: "https://gotify.example.com", Token: "AbCdEf123"}},
		},
		{
			name: "gotify sub path without tls",
			url:  "gotify://gotify.lan:8080/push/AbCdEf123/?disabletls=yes",
			want: []NotifierConfig{{Type: "gotify", URL: "http://gotify.lan:8080/push", Token: "AbCdEf123"}},
		},
		{
			name: "teams workflow",
			url:  "teams://?host=https%3A%2F%2Fprod-00.westus.logic.azure.com%3A443%2Fworkflows%2Fabc%2Ftriggers%2Fmanual%2Fpaths%2Finvoke%3Fapi-version%3D2016-06-00%26sig%3DXX&title=Alert",
			want: []NotifierConfig{{Type: "teams", URL: "https://prod-00.westus.logic.azure.com:443/workflows/abc/triggers/manual/paths/invoke?api-version=2016-06-00&sig=XX"}},
		},
		{ // #nosec G101 -- dummy credentials of a test fixture
			name: "smtp with STARTTLS required",
			url:  "smtp://bot:p%40ss@mail.example.com:587/?fromaddress=dockpaum@example.com&toaddresses=ops+prod@example.com,oncall@example.com&requirestarttls=yes&subject=x",
			want: []NotifierConfig{{
				Type: "email", From: "dockpaum@example.com", To: []string{"ops+prod@example.com", "oncall@example.com"},
				SMTP: &SMTPConfig{Host: "mail.example.com", Port: 587, Username: "bot", Password: "p@ss", StartTLS: true},
			}},
		},
		{ // #nosec G101 -- dummy credentials of a test fixture
			name: "smtp default port and auth none",
			url:  "smtp://u:p@relay.lan/?from=a@b.c&to=d@e.f&auth=None",
			want: []NotifierConfig{{Type: "email", From: "a@b.c", To: []string{"d@e.f"}, SMTP: &SMTPConfig{Host: "relay.lan", Port: 25}}},
		},
		{ // #nosec G101 -- dummy credentials of a test fixture
			name: "smtp implicit tls picks 465",
			url:  "smtp://u:p@mail.example.com/?from=a@b.c&to=d@e.f&encryption=ImplicitTLS",
			want: []NotifierConfig{{Type: "email", From: "a@b.c", To: []string{"d@e.f"}, SMTP: &SMTPConfig{Host: "mail.example.com", Port: 465, Username: "u", Password: "p"}}},
		},
		{
			name: "pushover",
			url:  "pushover://shoutrrr:apptoken@userkey/?devices=phone,tablet",
			want: []NotifierConfig{{Type: "pushover", Token: "apptoken", User: "userkey", Channel: "phone,tablet"}},
		},
		{
			name: "matrix token login, one notifier per room",
			url:  "matrix://:syt_token@matrix.example.com:8448/?rooms=!abc:example.com,%23ops:example.com,general",
			want: []NotifierConfig{
				{Type: "matrix", URL: "https://matrix.example.com:8448", Token: "syt_token", Room: "!abc:example.com"},
				{Type: "matrix", URL: "https://matrix.example.com:8448", Token: "syt_token", Room: "#ops:example.com"},
				{Type: "matrix", URL: "https://matrix.example.com:8448", Token: "syt_token", Room: "#general"},
			},
		},
		{
			name: "mattermost as slack-compatible webhook",
			url:  "mattermost://chat.example.com/hooktoken/town-square",
			want: []NotifierConfig{{Type: "slack", URL: "https://chat.example.com/hooks/hooktoken", Channel: "town-square"}},
		},
		{
			name: "apprise stateful key over https",
			url:  "apprises://apprise.example.com/sub/mykey?tags=ops",
			want: []NotifierConfig{{Type: "apprise", URL: "https://apprise.example.com/sub/notify/mykey", Channel: "ops"}},
		},
		{
			name: "generic with headers and passthrough",
			url:  "generic://hooks.example.com/in?@Authorization=Bearer%20tok&@acceptLanguage=en&template=json&__title=t&key=v",
			want: []NotifierConfig{{
				Type: "webhook", URL: "https://hooks.example.com/in?key=v&title=t",
				Headers: map[string]string{"Authorization": "Bearer tok", "Accept-Language": "en"},
			}},
		},
		{
			name: "generic+http",
			url:  "generic+http://10.0.0.5:9000/hook?method=POST",
			want: []NotifierConfig{{Type: "webhook", URL: "http://10.0.0.5:9000/hook"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := convertServiceURL(tt.url)
			if err != nil {
				t.Fatalf("convertServiceURL: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestConvertServiceURLErrors(t *testing.T) {
	tests := []struct {
		name, url, want string
	}{
		{"unsupported scheme lists the supported ones", "bark://devicekey@host", "bark:// is not a supported notification service; supported are discord://"},
		{"no scheme", "hooks.example.com/x", "no scheme"},
		{"malformed", "discord://SECRET%zz@123", "malformed"},
		{"discord without id", "discord://SECRETTOKEN", "want discord://<token>@<webhook id>"},
		{"slack bot token", "slack://xoxb:" + slackT + "-" + slackB + "-SECRETSECRETSECRETSECRET@C0123", "bot and user API tokens (xoxb)"},
		{"slack garbage", "slack://SECRET@webhook", "want slack://hook:"},
		{"telegram without chats", "telegram://123:SECRET@telegram", "chats= is required"},
		{"telegram topic", "telegram://123:SECRET@telegram?chats=-100:5", "forum topics"},
		{"ntfy without topic", "ntfy://ntfy.sh", "want ntfy://<host>/<topic>"},
		{"ntfy basic auth", "ntfy://user:SECRET@ntfy.sh/t", "user/password login is not supported"},
		{"ntfy skip verify", "ntfy://ntfy.sh/t?disabletlsverification=yes", "always verified"},
		{"ntfy bad scheme", "ntfy://ntfy.sh/t?scheme=ftp", "parameter scheme"},
		{"gotify without token", "gotify://gotify.example.com", "want gotify://<host>/<application token>"},
		{"teams retired connector", "teams://group@tenant/alt/owner?host=acme.webhook.office.com", "retired by Microsoft"},
		{"teams unencoded workflow url", "teams://?host=https://prod.logic.azure.com/workflows/a/invoke?api-version=1&sp=/x&sig=SECRET", "must be percent-encoded"},
		{"smtp without recipients", "smtp://u:SECRET@mail.example.com/?from=a@b.c", "toaddresses= (or to=) is required"},
		{"smtp implicit tls on 587", "smtp://u:SECRET@mail.example.com:587/?to=a@b.c&encryption=ImplicitTLS", "on port 465 only"},
		{"smtp oauth2", "smtp://u:SECRET@mail.example.com/?to=a@b.c&auth=OAuth2", "auth=oauth2 is not supported"},
		{"smtp skip verify", "smtp://u:SECRET@mail.example.com/?to=a@b.c&skiptlsverify=yes", "always verified"},
		{"smtp bad bool", "smtp://u:SECRET@mail.example.com/?to=a@b.c&requirestarttls=maybe", "parameter requirestarttls"},
		{"pushover without token", "pushover://userkey", "want pushover://"},
		{"matrix password login", "matrix://bot:SECRET@matrix.example.com?rooms=!a:b", "user:password login is not supported"},
		{"matrix without rooms", "matrix://:SECRET@matrix.example.com", "rooms= is required"},
		{"generic GET", "generic://hooks.example.com/x?method=GET", "always POSTs"},
		{"apprise without key", "apprise://apprise.example.com", "want apprise://<host>/<config key>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := convertServiceURL(tt.url)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error echoes the secret: %v", err)
			}
		})
	}
}

func TestConvertServiceURLNotes(t *testing.T) {
	tests := []struct {
		url, want string
	}{
		{"ntfy://ntfy.sh/t?priority=5&Title=x", "ntfy:// parameters priority, title are not supported and are ignored"},
		{"discord://tok@123?username=bot", "discord:// parameters username"},
		{"generic://h.example.com/x?$source=dockpaum", "extra JSON fields $source are not supported"},
		{"generic+https://h.example.com/x?disabletls=yes&contenttype=text/plain", "parameters contenttype, disabletls"},
		{"smtp://u:p@m.example.com/?to=a@b.c&auth=Login", "dockpaum authenticates with PLAIN"},
		{"mattermost://bot@chat.example.com/tok", "user name is not supported"},
		{"slack://bot@" + slackT + "/" + slackB + "/" + slackX, "bot name is not supported"},
	}
	for _, tt := range tests {
		_, notes, err := convertServiceURL(tt.url)
		if err != nil {
			t.Errorf("%s: %v", tt.url, err)
			continue
		}
		if got := strings.Join(notes, "\n"); !strings.Contains(got, tt.want) {
			t.Errorf("%s: notes %q, want %q", tt.url, got, tt.want)
		}
	}
	if _, notes, _ := convertServiceURL("gotify://g.example.com/tok?useheader=yes"); len(notes) != 0 {
		t.Errorf("useheader is honored, not ignored: %v", notes)
	}
}

func TestSplitURLs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"discord://a@1 ntfy://ntfy.sh/t", []string{"discord://a@1", "ntfy://ntfy.sh/t"}},
		{"discord://a@1,ntfy://ntfy.sh/t,", []string{"discord://a@1", "ntfy://ntfy.sh/t"}},
		{"telegram://1:x@telegram?chats=1,2 , gotify://g/t", []string{"telegram://1:x@telegram?chats=1,2", "gotify://g/t"}},
		{"# primary\ndiscord://a@1\n\n  # backup\n generic+https://h/x?a=1,b\n", []string{"discord://a@1", "generic+https://h/x?a=1,b"}},
	}
	for _, tt := range tests {
		if got := splitURLs(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitURLs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestServiceURLsInConfigFile(t *testing.T) {
	raw := `
notify:
  - url: discord://tok@123
    events: [update.failure]
  - name: chats
    type: telegram
    url: telegram://1:SECRET@telegram?chats=10,20&preview=no
    events: [run.completed]
  - type: email
    url: smtp://u:p@mail.example.com:587/?from=a@b.c&to=d@e.f
  - type: webhook
    url: https://hooks.example.com/plain
` // #nosec G101 -- dummy credentials of a test fixture
	cfg, err := decodeEnv([]byte(raw), "urls", envMap{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify) != 5 {
		t.Fatalf("notify = %+v, want 5 entries (the telegram URL has two chats)", cfg.Notify)
	}
	want := []struct{ typ, name, events string }{
		{"discord", "", "update.failure"},
		{"telegram", "chats", "run.completed"},
		{"telegram", "chats-2", "run.completed"},
		{"email", "", ""},
		{"webhook", "", ""},
	}
	for i, w := range want {
		n := cfg.Notify[i]
		if n.Type != w.typ || n.Name != w.name || strings.Join(n.Events, ",") != w.events {
			t.Errorf("notify[%d] = %s/%q/%v, want %s/%q/%s", i, n.Type, n.Name, n.Events, w.typ, w.name, w.events)
		}
	}
	if cfg.Notify[2].Channel != "20" || cfg.notifyLabel(2) != "notify[1]" || cfg.notifyLabel(4) != "notify[3]" {
		t.Errorf("expanded entry: channel %q, labels %q %q", cfg.Notify[2].Channel, cfg.notifyLabel(2), cfg.notifyLabel(4))
	}
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "notify[1]: telegram:// parameters preview are not supported") {
		t.Errorf("ignored parameters must be a warning naming the entry:\n%s", w)
	}
	red := cfg.Redacted()
	if red.Notify[0].URL != "https://discord.com/***" || red.Notify[1].Token != "***" || red.Notify[3].SMTP.Password != "***" {
		t.Errorf("converted secrets not masked: %+v", red.Notify)
	}
}

func TestServiceURLEntryErrors(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"type mismatch", "notify:\n  - type: slack\n    url: discord://SECRET@1\n", "notify[0]: type slack does not match the discord:// URL, which is a discord notifier"},
		{"extra fields", "notify:\n  - url: telegram://1:SECRET@telegram?chats=1\n    channel: \"2\"\n    token: t\n", "notify[0]: with a service URL, set only type, name and events next to it (the URL carries the rest); found token, channel"},
		{"conversion error names the key", "notify:\n  - type: webhook\n    url: https://ok.example.com\n  - url: pigeon://SECRET@coop\n", "notify[1].url: pigeon:// is not a supported notification service"},
		{"http url without type", "notify:\n  - url: https://hooks.example.com/SECRET\n", "notify[0]: type is required with this url"},
		{"discord-like url on another path", "notify:\n  - url: https://discord.com/api/webhooks/1/SECRET/slack\n", "notify[0]: type is required with this url"},
		{"non-http url with a type", "notify:\n  - type: ntfy\n    url: ntfy.sh/SECRET\n", "notify[0] (ntfy).url: must be an http:// or https:// URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeEnv([]byte(tt.yaml), "entry", envMap{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error echoes the URL: %v", err)
			}
		})
	}
}

func TestNewNotifierTypesValidate(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"pushover ok", "notify:\n  - type: pushover\n    token: app\n    user: key\n", ""},
		{"pushover without user", "notify:\n  - type: pushover\n    token: app\n", "notify[0] (pushover): token (application API token) and user (user or group key) are required"},
		{"matrix ok", "notify:\n  - type: matrix\n    url: https://matrix.example.com\n    token: t\n    room: \"#ops:example.com\"\n", ""},
		{"matrix without room", "notify:\n  - type: matrix\n    url: https://matrix.example.com\n    token: t\n", "url (homeserver), token (access token) and room are required"},
		{"matrix bare room", "notify:\n  - type: matrix\n    url: https://matrix.example.com\n    token: t\n    room: ops\n", "room must be a room ID (!id:server) or an alias (#alias:server)"},
		{"apprise stateless ok", "notify:\n  - type: apprise\n    url: https://apprise.example.com/notify/\n    to: [\"tgram://bot/chat\", \"mailto://u:p@example.com\"]\n", ""},
		{"apprise without url", "notify:\n  - type: apprise\n", "(apprise): url (the Apprise API notify endpoint) is required"},
		{"apprise bad to", "notify:\n  - type: apprise\n    url: https://a.example.com/notify/\n    to: [nonsense]\n", "(apprise).to[0]: not an Apprise URL"},
		{"unknown type lists the known ones", "notify:\n  - type: pigeon\n", "want webhook, slack, discord, teams, telegram, ntfy, gotify, email, pushover, matrix or apprise"},
		{"ssh host rejected", "hosts:\n  - name: nas\n    endpoint: ssh://admin@nas\n", "hosts[0] (nas).endpoint: ssh:// is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeEnv([]byte(tt.yaml), "types", envMap{})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRedactedMasksAppriseURLs(t *testing.T) {
	cfg, err := decodeEnv([]byte("notify:\n  - type: apprise\n    url: https://apprise.example.com/notify/\n    to: [\"tgram://123456:BOTSECRET/chat\"]\n  - type: email\n    to: [ops@example.com]\n    smtp: { host: mail.example.com }\n"), "apprise", envMap{})
	if err != nil {
		t.Fatal(err)
	}
	red := cfg.Redacted()
	if got := red.Notify[0].To[0]; got != "tgram://***" {
		t.Errorf("apprise to = %q, want tgram://***", got)
	}
	if cfg.Notify[0].To[0] != "tgram://123456:BOTSECRET/chat" {
		t.Error("Redacted() mutated the original")
	}
	if red.Notify[1].To[0] != "ops@example.com" {
		t.Error("e-mail recipients are not secrets and stay readable")
	}
}

func TestHeaderName(t *testing.T) {
	for in, want := range map[string]string{
		"acceptLanguage": "Accept-Language",
		"Authorization":  "Authorization",
		"X-Custom":       "X-Custom",
		"x-api-key":      "X-Api-Key",
	} {
		if got := headerName(in); got != want {
			t.Errorf("headerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebhookType(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://discord.com/api/webhooks/123/tok", "discord"},
		{"https://discordapp.com/api/webhooks/123/tok/", "discord"},
		{"https://ptb.discord.com/api/webhooks/123/tok", "discord"},
		{"https://discord.com/api/webhooks/123/tok/slack", ""},
		{"https://hooks.slack.com/services/T0/B0/X0", "slack"},
		{"https://hooks.slack.com/workflows/T0/A0/1/X0", ""},
		{"https://prod-00.westus.logic.azure.com:443/workflows/abc/triggers/manual/paths/invoke?sig=x", "teams"},
		{"https://default1.2a.environment.api.powerplatform.com:443/powerautomate/automations/direct/workflows/abc/triggers/manual/paths/invoke", "teams"},
		{"http://discord.com/api/webhooks/123/tok", ""},
		{"https://hooks.example.com/discord.com/api/webhooks/1/2", ""},
		{"discord://tok@123", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := webhookType(tt.url); got != tt.want {
			t.Errorf("webhookType(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestWebhookURLsNeedNoType(t *testing.T) {
	raw := "notify:\n  - url: https://discord.com/api/webhooks/1/tok\n  - url: https://hooks.slack.com/services/T0/B0/X0\n    events: [update.failure]\n"
	cfg, err := decodeEnv([]byte(raw), "webhooks", envMap{
		"DOCKPAUM_NOTIFY_URLS": "https://prod-00.westus.logic.azure.com/workflows/abc/invoke ntfy://ntfy.sh/t",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range cfg.Notify {
		got = append(got, n.Type)
	}
	if strings.Join(got, ",") != "discord,slack,teams,ntfy" {
		t.Errorf("types = %v, want discord, slack, teams, ntfy", got)
	}
	if cfg.Notify[0].URL != "https://discord.com/api/webhooks/1/tok" || cfg.Notify[2].URL != "https://prod-00.westus.logic.azure.com/workflows/abc/invoke" {
		t.Errorf("the webhook URLs are used as given: %+v", cfg.Notify)
	}
	_, err = decodeEnv(DefaultConfig(), embeddedSource, envMap{"DOCKPAUM_NOTIFY_URLS": "https://hooks.example.com/SECRET"})
	if err == nil || !strings.Contains(err.Error(), "generic+https://… for any other webhook") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v, want the generic+https hint without the URL", err)
	}
}

func TestPushoverEncryptionKeyIsRefused(t *testing.T) {
	for _, u := range []string{
		"pushover://shoutrrr:apptoken@userkey/?encryptionkey=SECRETKEY",
		"pushover://shoutrrr:apptoken@userkey/?key=SECRETKEY",
	} {
		_, _, err := convertServiceURL(u)
		if err == nil || !strings.Contains(err.Error(), "end-to-end encryption (encryptionkey) is not supported") {
			t.Errorf("%s: err = %v, want the encryption refusal", u, err)
		}
		if err != nil && strings.Contains(err.Error(), "SECRETKEY") {
			t.Errorf("error echoes the key: %v", err)
		}
	}
	if _, notes, err := convertServiceURL("pushover://shoutrrr:apptoken@userkey/?priority=1"); err != nil || len(notes) != 1 {
		t.Errorf("priority is only ignored: notes %v, err %v", notes, err)
	}
}

func TestRetiredTeamsConnectorWarns(t *testing.T) {
	cfg, err := decodeEnv([]byte("notify:\n  - type: teams\n    url: https://acme.webhook.office.com/webhookb2/SECRET\n  - type: teams\n    url: https://prod-00.westus.logic.azure.com/workflows/abc/invoke\n"), "teams", envMap{})
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(w, "notify[0] (teams).url: Office 365 connector webhooks were retired") {
		t.Errorf("warnings miss the retired connector:\n%s", w)
	}
	if strings.Contains(w, "notify[1]") || strings.Contains(w, "SECRET") {
		t.Errorf("only the connector URL warns, without echoing it:\n%s", w)
	}
}
