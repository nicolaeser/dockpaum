package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const (
	defaultSMTPPort = 587
	smtpsPort       = 465
)

type email struct {
	from string
	to   []string
	smtp *config.SMTPConfig
	tls  *tls.Config
}

func newEmail(c config.NotifierConfig) *email {
	return &email{from: c.From, to: c.To, smtp: c.SMTP}
}

func (e *email) Notify(ctx context.Context, ev Event) error {
	if e.smtp == nil || e.smtp.Host == "" {
		return errors.New("email: smtp.host is not configured")
	}
	var auth smtp.Auth
	if e.smtp.Username != "" {
		auth = smtp.PlainAuth("", e.smtp.Username, e.smtp.Password, e.smtp.Host)
	}
	addr, implicitTLS := smtpTarget(e.smtp)
	return sendMail(ctx, smtpDelivery{
		addr:        addr,
		host:        e.smtp.Host,
		auth:        auth,
		from:        e.from,
		to:          e.to,
		msg:         buildEmail(e.from, e.to, ev),
		implicitTLS: implicitTLS,
		requireTLS:  e.smtp.StartTLS,
		tls:         e.tls,
	})
}

func smtpTarget(c *config.SMTPConfig) (addr string, implicitTLS bool) {
	port := c.Port
	if port == 0 {
		port = defaultSMTPPort
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port)), port == smtpsPort
}

const smtpTimeout = 30 * time.Second

type smtpDelivery struct {
	addr, host  string
	auth        smtp.Auth
	from        string
	to          []string
	msg         []byte
	implicitTLS bool
	requireTLS  bool
	tls         *tls.Config
}

func (d smtpDelivery) tlsConfig() *tls.Config {
	if d.tls != nil {
		c := d.tls.Clone()
		if c.ServerName == "" {
			c.ServerName = d.host
		}
		return c
	}
	return &tls.Config{ServerName: d.host, MinVersion: tls.VersionTLS12}
}

func sendMail(ctx context.Context, d smtpDelivery) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(smtpTimeout)
	}
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	var err error
	if d.implicitTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: d.tlsConfig()}).DialContext(ctx, "tcp", d.addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", d.addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(deadline)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	c, err := smtp.NewClient(conn, d.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if !d.implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok || d.requireTLS {
			if err := c.StartTLS(d.tlsConfig()); err != nil {
				return err
			}
		}
	}
	if d.auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := c.Auth(d.auth); err != nil {
			return err
		}
	}
	if err := c.Mail(d.from); err != nil {
		return err
	}
	for _, r := range d.to {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(d.msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildEmail(from string, to []string, ev Event) []byte {
	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)
	writePart := func(contentType, content string) {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", contentType)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, _ := mw.CreatePart(h)
		qp := quotedprintable.NewWriter(pw)
		_, _ = qp.Write([]byte(content))
		_ = qp.Close()
	}
	writePart("text/plain; charset=utf-8", emailText(ev))
	writePart("text/html; charset=utf-8", emailHTML(ev))
	_ = mw.Close()

	var b bytes.Buffer
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.BEncoding.Encode("utf-8", emailSubject(ev)) + "\r\n")
	b.WriteString("Date: " + ev.At.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary())
	b.Write(parts.Bytes())
	return b.Bytes()
}

func emailSubject(ev Event) string {
	if ev.Type != EventRunCompleted {
		return formatTitle(ev)
	}
	state := "completed"
	if ev.Err != nil {
		state = "failed"
	}
	if s := ev.RunStats; s != nil {
		subject := fmt.Sprintf("🏁 dockpaum: run %s — %d updated", state, s.Updated)
		if s.Failed > 0 {
			subject += fmt.Sprintf(", %d failed", s.Failed)
		}
		available := 0
		for _, it := range s.Items {
			if standing(it.Result) {
				available++
			}
		}
		if available > 0 {
			subject += fmt.Sprintf(", %d available", available)
		}
		return subject
	}
	return "🏁 dockpaum: run " + state
}

func emailHeadline(ev Event) string {
	switch ev.Type {
	case EventUpdateSuccess:
		return "Container updated"
	case EventUpdateFailure:
		return "Update failed"
	case EventAvailable:
		return "Update available"
	case EventNoUpdate:
		return "Update skipped"
	case EventDenied:
		return "Update denied"
	case EventRolledBack:
		return "Update rolled back"
	case EventRecovered:
		return "Container recovered"
	case EventCheckFailed:
		return "Update check failed"
	case EventSelfUpdateStarted:
		return "Self-update started"
	case EventSelfUpdateSuccess:
		return "Self-update completed"
	case EventSelfUpdateFailure:
		return "Self-update failed"
	case EventStopping:
		return "dockpaum stopping"
	case EventRunStarted:
		return "Run started"
	case EventRunCompleted:
		if ev.Err != nil {
			return "Run failed"
		}
		return "Run completed"
	case EventStarted:
		return "dockpaum started"
	case EventTest:
		return "Test notification"
	}
	return "dockpaum: " + ev.Type
}

func emailText(ev Event) string {
	var b strings.Builder
	b.WriteString(emailHeadline(ev) + "\n\n")
	for _, r := range eventRows(ev) {
		b.WriteString(r.label + ": " + r.value)
		if r.item && r.link != "" {
			b.WriteString(" · " + r.link)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n-- \ndockpaum · automated container updates · ")
	b.WriteString(ev.At.UTC().Format("2006-01-02 15:04 UTC") + "\n")
	return b.String()
}

func emailColor(ev Event) string {
	switch {
	case failureEvent(ev.Type):
		return "#d6453d"
	}
	switch ev.Type {
	case EventNoUpdate, EventAvailable:
		return "#8a92a6"
	case EventRunStarted, EventStarted, EventTest, EventRecovered, EventSelfUpdateStarted, EventStopping:
		return "#3a6ea5"
	case EventRunCompleted:
		if ev.Err != nil || (ev.RunStats != nil && ev.RunStats.Failed > 0) {
			return "#d98a1f"
		}
		return "#3a6ea5"
	}
	return "#1f9d57"
}

func emailTitleLine(ev Event) string { return cardTitle(ev) }

const emailLinkStyle = `color:#1d5fa8;text-decoration:underline;`

func emailHTML(ev Event) string {
	color := emailColor(ev)
	lines := []string{
		`<!doctype html><html><head><meta charset="utf-8"></head>`,
		`<body style="margin:0;background:#f4f5f7;padding:24px 12px;font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,Helvetica,Arial,sans-serif;">`,
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">`,
		`<table role="presentation" width="500" cellpadding="0" cellspacing="0" style="max-width:500px;width:100%;background:#ffffff;border-radius:14px;overflow:hidden;border:1px solid #e6e8ec;">`,
		`<tr><td style="background:` + color + `;padding:18px 22px;">`,
		`<div style="color:#ffffff;font-size:17px;font-weight:600;line-height:1.3;">` + html.EscapeString(emailTitleLine(ev)) + `</div>`,
	}
	if ev.Host != "" {
		lines = append(lines, `<div style="color:rgba(255,255,255,0.88);font-size:12.5px;margin-top:5px;font-family:ui-monospace,Menlo,monospace;">🖥 `+html.EscapeString(ev.Host)+`</div>`)
	}
	lines = append(lines,
		`</td></tr>`,
		`<tr><td style="padding:10px 6px 6px;"><table role="presentation" width="100%" cellpadding="0" cellspacing="0">`,
	)
	for _, r := range eventRows(ev) {
		if r.label == "Host" || r.label == "Container" {
			continue
		}
		vcolor := "#1c2230"
		switch r.label {
		case "Updated":
			vcolor = "#1f9d57"
		case "Failed", "Error":
			vcolor = "#d6453d"
		}
		value := html.EscapeString(r.value)
		switch {
		case r.link != "" && r.item:
			value += ` · <a href="` + html.EscapeString(r.link) + `" style="` + emailLinkStyle + `">release notes</a>`
		case r.link != "":
			value = `<a href="` + html.EscapeString(r.link) + `" style="` + emailLinkStyle + `">` + value + `</a>`
		}
		lines = append(lines,
			`<tr><td style="padding:7px 18px;color:#8a92a6;font-size:13px;white-space:nowrap;vertical-align:top;width:84px;">`+html.EscapeString(r.label)+
				`</td><td style="padding:7px 18px;color:`+vcolor+`;font-size:13px;font-family:ui-monospace,Menlo,Consolas,monospace;word-break:break-all;">`+value+`</td></tr>`)
	}
	lines = append(lines,
		`</table></td></tr>`,
		`<tr><td style="padding:14px 22px;border-top:1px solid #eef0f3;color:#9aa1b1;font-size:12px;">dockpaum · `+ev.At.UTC().Format("2006-01-02 15:04 UTC")+`</td></tr>`,
		`</table></td></tr></table></body></html>`,
	)
	return strings.Join(lines, "\r\n")
}
