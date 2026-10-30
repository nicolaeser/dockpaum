package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestSMTPTarget(t *testing.T) {
	cases := []struct {
		host     string
		port     int
		addr     string
		implicit bool
	}{
		{"smtp.example.org", 0, "smtp.example.org:587", false},
		{"smtp.example.org", 587, "smtp.example.org:587", false},
		{"smtp.example.org", 465, "smtp.example.org:465", true},
		{"relay", 25, "relay:25", false},
		{"::1", 0, "[::1]:587", false},
	}
	for _, tc := range cases {
		addr, implicit := smtpTarget(&config.SMTPConfig{Host: tc.host, Port: tc.port})
		if addr != tc.addr || implicit != tc.implicit {
			t.Errorf("%s:%d → %s implicit=%v, want %s implicit=%v", tc.host, tc.port, addr, implicit, tc.addr, tc.implicit)
		}
	}
}

func TestEmailWithoutSMTPIsAnError(t *testing.T) {
	if err := newEmail(config.NotifierConfig{To: []string{"x@y.z"}}).Notify(context.Background(), Event{Type: EventTest}); err == nil {
		t.Fatal("an email notifier without smtp must not report success")
	}
}

func mimeParts(t *testing.T, raw []byte) (mail.Header, map[string]string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q: %v", msg.Header.Get("Content-Type"), err)
	}
	parts := map[string]string{}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[ct] = string(b)
	}
	return msg.Header, parts
}

func TestEmailTextAndHTMLParity(t *testing.T) {
	for typ, ev := range sampleEvents() {
		t.Run(typ, func(t *testing.T) {
			raw := buildEmail("dockpaum@example.org", []string{"ops@example.org"}, ev)
			hdr, parts := mimeParts(t, raw)
			text, htm := parts["text/plain"], parts["text/html"]
			if text == "" || htm == "" {
				t.Fatalf("parts = %v", parts)
			}
			if _, err := hdr.Date(); err != nil {
				t.Errorf("Date header: %v", err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(hdr.Get("Subject"))
			if err != nil || !strings.Contains(strings.ToLower(subject), strings.Fields(stateWord[typ])[0]) {
				t.Errorf("subject %q does not state the outcome", subject)
			}
			for _, r := range eventRows(ev) {
				if !strings.Contains(text, r.label+": "+r.value) {
					t.Errorf("text part lacks %s: %s", r.label, r.value)
				}
				if !strings.Contains(htm, html.EscapeString(r.value)) {
					t.Errorf("html part lacks %s: %s", r.label, r.value)
				}
				if r.link != "" && (!strings.Contains(text, r.link) || !strings.Contains(htm, `href="`+html.EscapeString(r.link)+`"`)) {
					t.Errorf("link %s missing from a part", r.link)
				}
			}
		})
	}
}

func TestEmailEscapesAndFoldsLongLines(t *testing.T) {
	ev := hostile
	ev.Err = errors.New(strings.Repeat("very-long-error-", 200) + "<img src=x onerror=alert(1)>")
	raw := buildEmail("a@b.c", []string{"x@y.z"}, ev)
	for i, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d is %d bytes, over the SMTP limit", i, len(line))
		}
	}
	_, parts := mimeParts(t, raw)
	htm := parts["text/html"]
	for _, bad := range []string{"<script>", "<img src=x", "<!channel>"} {
		if strings.Contains(htm, bad) {
			t.Errorf("html part keeps %q", bad)
		}
	}
	if !strings.Contains(parts["text/plain"], "<img src=x onerror=alert(1)>") {
		t.Error("the plain part must keep the value verbatim")
	}
}

func TestEmailRejectsUnsafeLinks(t *testing.T) {
	ev := sampleEvents()[EventUpdateSuccess]
	ev.ReleaseURL = `javascript:alert("x")`
	if strings.Contains(emailHTML(ev), "javascript:") || strings.Contains(emailText(ev), "javascript:") {
		t.Fatal("a non-http release link reached the email")
	}
}

func TestEmailSubjectCounts(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{Type: EventRunCompleted, RunStats: &RunStats{Updated: 2, Skipped: 9}}, "🏁 dockpaum: run completed — 2 updated"},
		{Event{Type: EventRunCompleted, Err: errors.New("x"), RunStats: &RunStats{Updated: 1, Failed: 1}}, "🏁 dockpaum: run failed — 1 updated, 1 failed"},
		{Event{Type: EventRunCompleted}, "🏁 dockpaum: run completed"},
		{Event{Type: EventRunCompleted, RunStats: &RunStats{Items: []RunItem{
			{Container: "web", Result: "monitor-only"}, {Container: "db", Result: "dry-run"}, {Container: "api", Result: "rolled-back"},
		}}}, "🏁 dockpaum: run completed — 0 updated, 2 available"},
		{Event{Type: EventUpdateSuccess, Container: "web"}, "✅ dockpaum: web updated"},
		{Event{Type: EventStarted}, "🚀 dockpaum: started"},
		{Event{Type: EventTest}, "🔔 dockpaum: test notification"},
	}
	for _, tc := range cases {
		if got := emailSubject(tc.ev); got != tc.want {
			t.Errorf("subject = %q, want %q", got, tc.want)
		}
	}
}

type fakeSMTP struct {
	ln        net.Listener
	serverTLS *tls.Config
	starttls  bool
	got       chan smtpSession
}

type smtpSession struct {
	tls  bool
	auth bool
	data string
}

func testCert(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &tls.Config{Certificates: ts.TLS.Certificates, MinVersion: tls.VersionTLS12}, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func startFakeSMTP(t *testing.T, implicitTLS, offerSTARTTLS bool, serverTLS *tls.Config) *fakeSMTP {
	t.Helper()
	var ln net.Listener
	var err error
	if implicitTLS {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, serverTLS: serverTLS, starttls: offerSTARTTLS, got: make(chan smtpSession, 1)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		f.serve(conn, implicitTLS)
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn, isTLS bool) {
	s := smtpSession{tls: isTLS}
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch verb {
		case "EHLO":
			ext := []string{"250-fake"}
			if f.starttls && !s.tls {
				ext = append(ext, "250-STARTTLS")
			}
			ext = append(ext, "250 AUTH PLAIN")
			_ = tp.PrintfLine("%s", strings.Join(ext, "\r\n"))
		case "STARTTLS":
			if !f.starttls || s.tls {
				_ = tp.PrintfLine("502 not offered")
				continue
			}
			_ = tp.PrintfLine("220 go ahead")
			tc := tls.Server(conn, f.serverTLS)
			if tc.Handshake() != nil {
				return
			}
			conn, s.tls = tc, true
			tp = textproto.NewConn(tc)
		case "AUTH":
			s.auth = true
			_ = tp.PrintfLine("235 ok")
		case "MAIL", "RCPT":
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go")
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			s.data = string(b)
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			f.got <- s
			return
		default:
			_ = tp.PrintfLine("502 unknown")
		}
	}
}

func TestSMTPTransportModes(t *testing.T) {
	serverTLS, clientTLS := testCert(t)
	msg := buildEmail("a@b.c", []string{"x@y.z"}, sampleEvents()[EventTest])
	cases := []struct {
		name                       string
		implicit, offer, require   bool
		wantErr, wantTLS, wantAuth bool
	}{
		{name: "implicit TLS (465)", implicit: true, wantTLS: true, wantAuth: true},
		{name: "STARTTLS when offered", offer: true, wantTLS: true, wantAuth: true},
		{name: "required STARTTLS not offered", require: true, wantErr: true},
		{name: "plaintext to loopback without STARTTLS", wantAuth: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startFakeSMTP(t, tc.implicit, tc.offer, serverTLS)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := sendMail(ctx, smtpDelivery{
				addr: f.ln.Addr().String(), host: "127.0.0.1",
				auth: smtp.PlainAuth("", "user", "pass", "127.0.0.1"),
				from: "a@b.c", to: []string{"x@y.z"}, msg: msg,
				implicitTLS: tc.implicit, requireTLS: tc.require, tls: clientTLS,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			s := <-f.got
			if s.tls != tc.wantTLS || s.auth != tc.wantAuth {
				t.Fatalf("session tls=%v auth=%v, want tls=%v auth=%v", s.tls, s.auth, tc.wantTLS, tc.wantAuth)
			}
			if _, err := mail.ReadMessage(bufio.NewReader(strings.NewReader(s.data))); err != nil {
				t.Fatalf("relay received an unreadable message: %v", err)
			}
		})
	}
}

func TestImplicitTLSVerifiesTheServer(t *testing.T) {
	serverTLS, _ := testCert(t)
	f := startFakeSMTP(t, true, false, serverTLS)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sendMail(ctx, smtpDelivery{
		addr: f.ln.Addr().String(), host: "127.0.0.1", from: "a@b.c", to: []string{"x@y.z"},
		msg: []byte("x"), implicitTLS: true,
	})
	if err == nil {
		t.Fatal("an unverified certificate was accepted")
	}
}
