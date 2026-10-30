package registry

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrRateLimited = errors.New("registry: rate limit exceeded")

type RateLimit struct {
	Limit     int
	Remaining int
	Window    time.Duration
	Reset     time.Duration
}

func ParseRateLimit(h http.Header) (RateLimit, bool) {
	limit, limitWindow, ok := parseQuota(h.Get("RateLimit-Limit"))
	if !ok {
		return RateLimit{}, false
	}
	remaining, remainingWindow, ok := parseQuota(h.Get("RateLimit-Remaining"))
	if !ok {
		return RateLimit{}, false
	}
	rl := RateLimit{Limit: limit, Remaining: remaining, Window: limitWindow}
	if rl.Window == 0 {
		rl.Window = remainingWindow
	}
	if s := strings.TrimSpace(h.Get("RateLimit-Reset")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			rl.Reset = seconds(n)
		}
	}
	return rl, true
}

const maxQuotaDuration = 30 * 24 * time.Hour

func seconds(n int) time.Duration {
	if n >= int(maxQuotaDuration/time.Second) {
		return maxQuotaDuration
	}
	return time.Duration(n) * time.Second
}

func parseQuota(v string) (n int, window time.Duration, ok bool) {
	item, _, _ := strings.Cut(v, ",")
	num, params, _ := strings.Cut(strings.TrimSpace(item), ";")
	n, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || n < 0 {
		return 0, 0, false
	}
	for _, p := range strings.Split(params, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(p), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(k), "w") {
			continue
		}
		if secs, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && secs > 0 {
			window = seconds(secs)
		}
	}
	return n, window, true
}

type ObservedRateLimit struct {
	Registry string
	Account  string
	RateLimit
	At time.Time
}

type quotaKey struct{ registry, account string }

func (c *Client) LastRateLimit(host, account string) (RateLimit, bool) {
	if c == nil {
		return RateLimit{}, false
	}
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	o, ok := c.limits[quotaKey{host, account}]
	return o.RateLimit, ok
}

func (c *Client) RateLimits() []ObservedRateLimit {
	if c == nil {
		return nil
	}
	c.limitMu.Lock()
	out := make([]ObservedRateLimit, 0, len(c.limits))
	for _, o := range c.limits {
		out = append(out, o)
	}
	c.limitMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registry != out[j].Registry {
			return out[i].Registry < out[j].Registry
		}
		return out[i].Account < out[j].Account
	})
	return out
}

func (c *Client) observeRateLimit(host, account string, at time.Time, h http.Header) {
	rl, ok := ParseRateLimit(h)
	if !ok {
		return
	}
	key := quotaKey{host, account}
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	if prev, seen := c.limits[key]; seen && at.Before(prev.At) {
		return
	}
	if c.limits == nil {
		c.limits = map[quotaKey]ObservedRateLimit{}
	}
	c.limits[key] = ObservedRateLimit{Registry: host, Account: account, RateLimit: rl, At: at}
	if m, ok := c.metrics.(RateLimitMetrics); ok {
		m.RateLimit(host, account, rl.Limit, rl.Remaining)
	}
}

type RateLimitError struct {
	Registry   string
	Op         string
	RetryAfter time.Duration
	Quota      RateLimit
	HasQuota   bool
	Detail     string
}

func (e *RateLimitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s HTTP 429: %s rate limit exceeded", e.Op, e.Registry)
	if e.HasQuota {
		fmt.Fprintf(&b, " (%d of %d remaining", e.Quota.Remaining, e.Quota.Limit)
		if e.Quota.Window > 0 {
			fmt.Fprintf(&b, " per %s", e.Quota.Window)
		}
		b.WriteString(")")
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, ", retry after %s", e.RetryAfter)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

func newRateLimitError(host, op string, h http.Header, detail string, now time.Time) *RateLimitError {
	e := &RateLimitError{
		Registry:   host,
		Op:         op,
		RetryAfter: parseRetryAfter(h.Get("Retry-After"), now),
		Detail:     detail,
	}
	e.Quota, e.HasQuota = ParseRateLimit(h)
	if e.RetryAfter == 0 && e.HasQuota && e.Quota.Reset > 0 {
		e.RetryAfter = e.Quota.Reset
	}
	return e
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return seconds(secs)
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return min(d.Round(time.Second), maxQuotaDuration)
	}
	return 0
}
