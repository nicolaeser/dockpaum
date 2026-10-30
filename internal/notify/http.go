package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var client = &http.Client{CheckRedirect: safeRedirect}

func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		return errors.New("refusing redirect to another origin or scheme")
	}
	return nil
}

var errInvalidURL = errors.New("invalid notification url")

func newRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil || req.URL.Host == "" {
		return nil, errInvalidURL
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%w: the scheme must be http or https", errInvalidURL)
	}
	return req, nil
}

func send(req *http.Request, secrets ...string) error {
	resp, err := client.Do(req) // #nosec G704 -- admin-configured notification endpoint
	return httpResult(resp, err, secrets)
}

func sendJSON(ctx context.Context, method, rawURL string, body any, headers map[string]string, secrets ...string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := newRequest(ctx, method, rawURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return send(req, secrets...)
}

func postJSON(ctx context.Context, rawURL string, body any, headers map[string]string, secrets ...string) error {
	return sendJSON(ctx, http.MethodPost, rawURL, body, headers, secrets...)
}

const maxResponseBody = 64 << 10

func getJSON(ctx context.Context, rawURL string, headers map[string]string, out any) error {
	req, err := newRequest(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) // #nosec G704 -- admin-configured notification endpoint
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpResult(resp, err, nil)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func checkHTTP(resp *http.Response, err error) error { return httpResult(resp, err, nil) }

func httpResult(resp *http.Response, err error, secrets []string) error {
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	if d := responseDetail(resp, secrets); d != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, d)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

const maxErrorBody = 4 << 10

var codeBody = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func responseDetail(resp *http.Response, secrets []string) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	text := strings.TrimSpace(string(b))
	if text == "" {
		return ""
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var d string
	switch {
	case ct == "application/json" || strings.HasSuffix(ct, "+json") || strings.HasPrefix(text, "{"):
		d = jsonErrorDetail(b)
	case codeBody.MatchString(text):
		d = text
	case ct == "text/plain" && len(text) <= 160 && !strings.ContainsAny(text, "\r\n"):
		d = text
	}
	if d == "" {
		return ""
	}
	return truncate(oneLine(scrub(d, resp.Request, secrets)), 300)
}

func jsonErrorDetail(b []byte) string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	var parts []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, p := range parts {
			if p == s {
				return
			}
		}
		parts = append(parts, s)
	}
	for _, k := range []string{"errcode", "error", "errorDescription", "description", "message", "errors", "detail"} {
		switch v := m[k].(type) {
		case string:
			add(v)
		case []any:
			var list []string
			for _, e := range v {
				if s, ok := e.(string); ok {
					list = append(list, s)
				}
			}
			add(strings.Join(list, "; "))
		case map[string]any:
			if s, ok := v["code"].(string); ok {
				add(s)
			}
			if s, ok := v["message"].(string); ok {
				add(s)
			}
		}
	}
	return strings.Join(parts, ": ")
}

const (
	minURLScrub        = 8
	minCredentialScrub = 4
)

var contentHeaders = map[string]bool{
	"Accept": true, "Content-Type": true,
	"Title": true, "Tags": true, "Priority": true, "Click": true,
}

func scrub(d string, req *http.Request, secrets []string) string {
	type candidate struct {
		s      string
		minLen int
	}
	var cands []candidate
	addAll := func(minLen int, vs ...string) {
		for _, v := range vs {
			cands = append(cands, candidate{v, minLen})
		}
	}
	if req != nil && req.URL != nil {
		u := req.URL
		addAll(minURLScrub, u.String(), u.Path, u.EscapedPath(), u.RawQuery)
		addAll(minURLScrub, strings.Split(u.Path, "/")...)
		addAll(minURLScrub, strings.Split(u.EscapedPath(), "/")...)
		for _, pair := range strings.Split(u.RawQuery, "&") {
			addAll(minURLScrub, pair)
			if _, v, ok := strings.Cut(pair, "="); ok {
				addAll(minURLScrub, v)
			}
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				addAll(minURLScrub, v, url.QueryEscape(v))
			}
		}
		if u.User != nil {
			addAll(minCredentialScrub, u.User.Username())
			if p, ok := u.User.Password(); ok {
				addAll(minCredentialScrub, p)
			}
		}
	}
	if req != nil {
		for name, vs := range req.Header {
			if contentHeaders[name] {
				continue
			}
			for _, v := range vs {
				addAll(minCredentialScrub, headerSecrets(v)...)
			}
		}
	}
	addAll(minCredentialScrub, secrets...)
	sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].s) > len(cands[j].s) })
	for _, c := range cands {
		if len(c.s) >= c.minLen {
			d = strings.ReplaceAll(d, c.s, "[redacted]")
		}
	}
	return d
}

func headerSecrets(v string) []string {
	out := []string{v}
	scheme, cred, ok := strings.Cut(v, " ")
	if !ok {
		return out
	}
	out = append(out, cred)
	if strings.EqualFold(scheme, "Basic") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred)); err == nil {
			userPass := string(b)
			out = append(out, userPass)
			if user, pass, ok := strings.Cut(userPass, ":"); ok {
				out = append(out, user, pass)
			}
		}
	}
	return out
}

func urlSecrets(raw string) []string {
	out := []string{raw}
	add := func(s string) {
		out = append(out, s)
		if d, err := url.PathUnescape(s); err == nil && d != s {
			out = append(out, d)
		}
	}
	rest := raw
	if _, after, ok := strings.Cut(raw, "://"); ok {
		rest = after
	}
	authority, _, _ := strings.Cut(rest, "/")
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		userinfo := authority[:i]
		add(userinfo)
		if user, pass, ok := strings.Cut(userinfo, ":"); ok {
			add(user)
			add(pass)
		}
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune("/@?&=#,;", r) }) {
		if len(part) >= minURLScrub {
			add(part)
		}
		for _, sub := range strings.Split(part, ":") {
			if sub != part && len(sub) >= minURLScrub {
				add(sub)
			}
		}
	}
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsControl), " ")
}
