package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type Client struct {
	auths      []Credential
	httpClient *http.Client

	metrics    Metrics
	tokenMu    sync.Mutex
	tokenCache map[string]cachedToken

	limitMu sync.Mutex
	limits  map[quotaKey]ObservedRateLimit
}

type Metrics interface {
	ListCall(registry, result string)
	AuthCall(registry, result string)
}

type ManifestMetrics interface {
	ManifestCall(registry, result string)
}

type RateLimitMetrics interface {
	RateLimit(registry, account string, limit, remaining int)
}

type cachedToken struct {
	token  string
	expiry time.Time
}

func New(auths []config.RegistryAuth) *Client {
	return &Client{
		auths:      CentralCredentials(auths),
		httpClient: &http.Client{Timeout: 20 * time.Second, CheckRedirect: checkRedirect},
		tokenCache: map[string]cachedToken{},
		limits:     map[quotaKey]ObservedRateLimit{},
	}
}

const maxRedirects = 5

type redirectError struct{ msg string }

func (e *redirectError) Error() string { return "registry: " + e.msg }

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return &redirectError{msg: fmt.Sprintf("stopped after %d redirects", maxRedirects)}
	}
	first := via[0].URL
	switch strings.ToLower(req.URL.Scheme) {
	case "https":
	case "http":
		if !strings.EqualFold(first.Scheme, "http") || registryScheme(req.URL.Host) != "http" {
			return &redirectError{msg: fmt.Sprintf("refusing redirect from %s to plain HTTP at %s",
				displayOrigin(first), displayOrigin(req.URL))}
		}
	default:
		return &redirectError{msg: fmt.Sprintf("refusing redirect from %s to unsupported scheme %q",
			displayOrigin(first), req.URL.Scheme)}
	}
	if originOf(req.URL) != originOf(first) {
		req.Header.Del("Authorization")
	}
	return nil
}

func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func displayOrigin(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

func (c *Client) AttachMetrics(m Metrics) {
	if c == nil {
		return
	}
	c.metrics = m
}

func (c *Client) recordList(host, result string) {
	if c == nil || c.metrics == nil {
		return
	}
	c.metrics.ListCall(host, result)
}

func (c *Client) recordAuth(host, result string) {
	if c == nil || c.metrics == nil {
		return
	}
	c.metrics.AuthCall(host, result)
}

type endpoint string

const (
	endpointTags     endpoint = "tags/list"
	endpointManifest endpoint = "manifest"
	endpointBlob     endpoint = "blob"
)

func (c *Client) recordCall(ep endpoint, host, result string) {
	if ep == endpointTags {
		c.recordList(host, result)
		return
	}
	if c == nil || c.metrics == nil {
		return
	}
	if m, ok := c.metrics.(ManifestMetrics); ok {
		m.ManifestCall(host, result)
	}
}

type Reference struct {
	Registry string
	Repo     string
	Tag      string
	Digest   string
}

const dockerHubRegistry = "registry-1.docker.io"

func isDockerHubAlias(host string) bool {
	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "registry.hub.docker.com", dockerHubRegistry:
		return true
	}
	return false
}

func Parse(ref string) Reference {
	r := Reference{}
	if i := strings.Index(ref, "@"); i >= 0 {
		r.Digest = ref[i+1:]
		ref = ref[:i]
	}
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.Registry = parts[0]
		ref = parts[1]
		if isDockerHubAlias(r.Registry) {
			r.Registry = dockerHubRegistry
			if !strings.Contains(ref, "/") {
				ref = "library/" + ref
			}
		}
	} else {
		r.Registry = dockerHubRegistry
		if !strings.Contains(ref, "/") {
			ref = "library/" + ref
		}
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		r.Repo = ref[:i]
		r.Tag = ref[i+1:]
	} else {
		r.Repo = ref
	}
	return r
}

func (r Reference) String() string {
	out := r.Registry + "/" + r.Repo
	if r.Tag != "" {
		out += ":" + r.Tag
	}
	return out
}

func (r Reference) WithTag(tag string) Reference {
	r2 := r
	r2.Tag = tag
	r2.Digest = ""
	return r2
}

func (r Reference) HasDigest() bool { return r.Digest != "" }

func AuthFor(auths []config.RegistryAuth, ref Reference) *config.RegistryAuth {
	host := canonicalHost(ref.Registry)
	repo := strings.ToLower(ref.Repo)
	var best *config.RegistryAuth
	bestLen := -1
	for i := range auths {
		a := &auths[i]
		h, path := a.Scope()
		h = canonicalHost(h)
		if h != host && !strings.HasSuffix(host, "."+h) {
			continue
		}
		if path != "" && repo != path && !strings.HasPrefix(repo, path+"/") {
			continue
		}
		if len(path) > bestLen {
			best, bestLen = a, len(path)
		}
	}
	return best
}

func canonicalHost(h string) string {
	h = strings.ToLower(h)
	switch h {
	case "registry-1.docker.io", "index.docker.io", "registry.hub.docker.com":
		return "docker.io"
	}
	return h
}

func (c *Client) credentialsFor(ctx context.Context, host, repo string) *Credentials {
	if cs, ok := credentialsFrom(ctx); ok {
		return cs
	}
	return &Credentials{list: Candidates(c.auths, Reference{Registry: host, Repo: repo})}
}

type tagsResp struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func registryScheme(host string) string {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.Trim(hostname, "[]")
	if strings.EqualFold(hostname, "localhost") {
		return "http"
	}
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		return "http"
	}
	return "https"
}

const maxTagPages = 50

const (
	maxTagsPageBytes  = 16 << 20
	maxTokenBytes     = 1 << 20
	maxErrorBodyBytes = 4096
)

func (c *Client) ListTags(ctx context.Context, ref Reference) ([]string, error) {
	host := ref.Registry
	base := registryScheme(host) + "://" + host
	u := fmt.Sprintf("%s/v2/%s/tags/list?n=1000", base, ref.Repo)

	var all []string
	for page := 0; page < maxTagPages && u != ""; page++ {
		tags, next, err := c.tagsPage(ctx, host, ref.Repo, u)
		if err != nil {
			return nil, err
		}
		all = append(all, tags...)
		if next != "" && !strings.Contains(next, "://") {
			if !strings.HasPrefix(next, "/") {
				next = "/" + next
			}
			next = base + next
		}
		if next != "" && !sameOrigin(next, base) {
			return nil, fmt.Errorf("registry: refusing pagination link to another origin: %s", next)
		}
		u = next
	}
	if u != "" {
		return nil, fmt.Errorf("registry: %s has more than %d pages of tags", ref.Repo, maxTagPages)
	}
	return all, nil
}

func sameOrigin(rawURL, base string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	return originOf(u) == originOf(b)
}

func (c *Client) tagsPage(ctx context.Context, host, repo, u string) (tags []string, next string, err error) {
	resp, err := c.doAuthed(ctx, host, repo, apiRequest{endpoint: endpointTags, method: http.MethodGet, url: u})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	tags, err = decodeTags(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return tags, nextLink(resp.Header.Get("Link")), nil
}

type apiRequest struct {
	endpoint endpoint
	method   string
	url      string
	accept   string
}

func (c *Client) doAuthed(ctx context.Context, host, repo string, r apiRequest) (*http.Response, error) {
	var resp *http.Response
	err := c.credentialsFor(ctx, host, repo).Try(func(auth *config.RegistryAuth) error {
		var err error
		resp, err = c.doAuthedAs(ctx, host, repo, r, auth)
		return err
	}, IsDenied)
	return resp, err
}

func (c *Client) doAuthedAs(ctx context.Context, host, repo string, r apiRequest, auth *config.RegistryAuth) (*http.Response, error) {
	var user, pass, id string
	if auth != nil {
		var err error
		if user, pass, err = auth.Credentials(); err != nil {
			c.recordCall(r.endpoint, host, "auth")
			return nil, err
		}
		id = credentialKey(Credential{Auth: *auth}.Scope(), user, pass)
	}

	send := func(bearer string) (*http.Response, error) {
		resp, err := c.doWithRetry(ctx, func() (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, r.method, r.url, nil)
			if err != nil {
				return nil, err
			}
			if r.accept != "" {
				req.Header.Set("Accept", r.accept)
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			} else if user != "" {
				req.SetBasicAuth(user, pass)
			}
			return c.httpClient.Do(req)
		})
		if err == nil {
			c.observeRateLimit(host, user, time.Now(), resp.Header)
		}
		return resp, err
	}

	if tok, ok := c.cachedTokenFor(host, repo, id); ok {
		resp, err := send(tok)
		if err != nil {
			c.recordCall(r.endpoint, host, "error")
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			c.recordCall(r.endpoint, host, "ok")
			return resp, nil
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return nil, c.callError(host, r.endpoint, resp)
		}
		_ = resp.Body.Close()
		c.invalidateToken(host, repo, id)
	}

	resp, err := send("")
	if err != nil {
		c.recordCall(r.endpoint, host, "error")
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		_ = resp.Body.Close()
		token, err := c.bearerToken(ctx, challenge, user, pass, host, repo, id)
		if err != nil {
			result := "auth"
			if errors.Is(err, ErrRateLimited) {
				result = "rate_limited"
			}
			c.recordCall(r.endpoint, host, result)
			return nil, err
		}
		resp, err = send(token)
		if err != nil {
			c.recordCall(r.endpoint, host, "error")
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.callError(host, r.endpoint, resp)
	}
	c.recordCall(r.endpoint, host, "ok")
	return resp, nil
}

var ErrNotFound = errors.New("registry: not found")

type HTTPError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s HTTP %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.Op, e.StatusCode, e.Body)
}

func (e *HTTPError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrDenied:
		return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
	}
	return false
}

func (c *Client) callError(host string, ep endpoint, resp *http.Response) error {
	body := readErrorBody(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.recordCall(ep, host, "rate_limited")
		return newRateLimitError(host, string(ep), resp.Header, body, time.Now())
	}
	c.recordCall(ep, host, "error")
	return &HTTPError{Op: string(ep), StatusCode: resp.StatusCode, Body: body}
}

func readErrorBody(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	return strings.TrimSpace(string(body))
}

func readLimited(r io.Reader, limit int64, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("registry: %s exceeds %d bytes", what, limit)
	}
	return b, nil
}

func (c *Client) doWithRetry(ctx context.Context, do func() (*http.Response, error)) (*http.Response, error) {
	backoffs := []time.Duration{0, time.Second, 3 * time.Second}
	var resp *http.Response
	var err error
	for _, wait := range backoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		resp, err = do()
		var refused *redirectError
		if errors.As(err, &refused) {
			return nil, refused
		}
		if err != nil {
			continue
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			err = serverError(resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("after retries: %w", err)
}

var ErrUnavailable = errors.New("registry: unavailable")

type serverError int

func (e serverError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func (e serverError) Is(target error) bool { return target == ErrUnavailable }

func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		target := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		if target == "" {
			continue
		}
		for _, p := range segs[1:] {
			p = strings.TrimSpace(p)
			if p == `rel="next"` || p == "rel=next" {
				return target
			}
		}
	}
	return ""
}

func tokenKey(host, repo, id string) string {
	return host + "|" + repo + "|" + id
}

func (c *Client) cachedTokenFor(host, repo, id string) (string, bool) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	key := tokenKey(host, repo, id)
	t, ok := c.tokenCache[key]
	if !ok {
		return "", false
	}
	if time.Now().After(t.expiry) {
		delete(c.tokenCache, key)
		return "", false
	}
	return t.token, true
}

func (c *Client) invalidateToken(host, repo, id string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	delete(c.tokenCache, tokenKey(host, repo, id))
}

func (c *Client) storeToken(host, repo, id, token string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.tokenCache[tokenKey(host, repo, id)] = cachedToken{token: token, expiry: time.Now().Add(ttl)}
}

func decodeTags(r io.Reader) ([]string, error) {
	b, err := readLimited(r, maxTagsPageBytes, "tags/list page")
	if err != nil {
		return nil, err
	}
	var t tagsResp
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return t.Tags, nil
}

func (c *Client) bearerToken(ctx context.Context, www, user, pass, host, repo, id string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(www), "bearer ") {
		return "", denied(errors.New("registry: not a bearer challenge"))
	}
	params := parseChallenge(www[len("Bearer "):])
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("registry: missing realm")
	}
	if ru, err := url.Parse(realm); user != "" && (err != nil || (ru.Scheme != "https" && registryScheme(ru.Host) != "http")) {
		c.recordAuth(host, "error")
		return "", fmt.Errorf("registry: refusing token realm %q (https required)", realm)
	}
	q := []string{}
	if s := params["service"]; s != "" {
		q = append(q, "service="+s)
	}
	if s := params["scope"]; s != "" {
		q = append(q, "scope="+s)
	}
	u := realm
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&")
	}
	resp, err := c.doWithRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		return c.httpClient.Do(req)
	})
	if err != nil {
		c.recordAuth(host, "error")
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.recordAuth(host, "error")
		return "", newRateLimitError(host, "token", resp.Header, readErrorBody(resp.Body), time.Now())
	}
	if resp.StatusCode != http.StatusOK {
		c.recordAuth(host, "error")
		err := fmt.Errorf("token HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", denied(err)
		}
		return "", err
	}
	body, err := readLimited(resp.Body, maxTokenBytes, "token response")
	if err != nil {
		c.recordAuth(host, "error")
		return "", err
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		c.recordAuth(host, "error")
		return "", err
	}
	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.storeToken(host, repo, id, token, ttl/2)
	c.recordAuth(host, "ok")
	return token, nil
}

func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitChallenge(s) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[k] = v
	}
	return out
}

func splitChallenge(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}
