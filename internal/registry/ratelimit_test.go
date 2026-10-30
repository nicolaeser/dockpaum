package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestParseRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     string
		remaining string
		reset     string
		want      RateLimit
		wantOK    bool
	}{
		{
			name: "docker hub", limit: "100;w=21600", remaining: "76;w=21600",
			want: RateLimit{Limit: 100, Remaining: 76, Window: 6 * time.Hour}, wantOK: true,
		},
		{
			name: "plain numbers with reset", limit: "5000", remaining: "4999", reset: "60",
			want: RateLimit{Limit: 5000, Remaining: 4999, Reset: time.Minute}, wantOK: true,
		},
		{
			name: "policy list uses the first item", limit: "10, 10;w=1, 50;w=60", remaining: "3",
			want: RateLimit{Limit: 10, Remaining: 3}, wantOK: true,
		},
		{
			name: "window only on remaining", limit: "200", remaining: "0;w=3600",
			want: RateLimit{Limit: 200, Remaining: 0, Window: time.Hour}, wantOK: true,
		},
		{
			name: "spacing and case", limit: " 100 ; W=60 ", remaining: "1",
			want: RateLimit{Limit: 100, Remaining: 1, Window: time.Minute}, wantOK: true,
		},
		{
			name: "huge window is capped", limit: "1;w=999999999999", remaining: "1",
			want: RateLimit{Limit: 1, Remaining: 1, Window: maxQuotaDuration}, wantOK: true,
		},
		{name: "absent", wantOK: false},
		{name: "remaining missing", limit: "100", wantOK: false},
		{name: "not a number", limit: "lots", remaining: "1", wantOK: false},
		{name: "negative", limit: "100", remaining: "-1", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.limit != "" {
				h.Set("RateLimit-Limit", tt.limit)
			}
			if tt.remaining != "" {
				h.Set("RateLimit-Remaining", tt.remaining)
			}
			if tt.reset != "" {
				h.Set("RateLimit-Reset", tt.reset)
			}
			got, ok := ParseRateLimit(h)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ParseRateLimit = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"99999999999999", maxQuotaDuration},
		{now.Add(400 * 24 * time.Hour).Format(http.TimeFormat), maxQuotaDuration},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestRateLimitError(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		header  http.Header
		detail  string
		want    string
		wantRA  time.Duration
		wantHas bool
	}{
		{
			name:   "bare",
			header: http.Header{},
			want:   "manifest HTTP 429: registry-1.docker.io rate limit exceeded",
		},
		{
			name: "quota retry and detail",
			header: http.Header{
				"Ratelimit-Limit":     {"100;w=21600"},
				"Ratelimit-Remaining": {"0;w=21600"},
				"Retry-After":         {"3600"},
			},
			detail:  `{"errors":[{"code":"TOOMANYREQUESTS"}]}`,
			want:    `manifest HTTP 429: registry-1.docker.io rate limit exceeded (0 of 100 remaining per 6h0m0s), retry after 1h0m0s: {"errors":[{"code":"TOOMANYREQUESTS"}]}`,
			wantRA:  time.Hour,
			wantHas: true,
		},
		{
			name: "reset stands in for retry-after",
			header: http.Header{
				"Ratelimit-Limit":     {"10"},
				"Ratelimit-Remaining": {"0"},
				"Ratelimit-Reset":     {"42"},
			},
			want:    "manifest HTTP 429: registry-1.docker.io rate limit exceeded (0 of 10 remaining), retry after 42s",
			wantRA:  42 * time.Second,
			wantHas: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newRateLimitError("registry-1.docker.io", "manifest", tt.header, tt.detail, now)
			if e.Error() != tt.want {
				t.Errorf("Error() = %q, want %q", e.Error(), tt.want)
			}
			if e.RetryAfter != tt.wantRA || e.HasQuota != tt.wantHas {
				t.Errorf("RetryAfter = %s HasQuota = %v", e.RetryAfter, e.HasQuota)
			}
			wrapped := fmt.Errorf("registry tag list: %w", e)
			if !errors.Is(wrapped, ErrRateLimited) {
				t.Error("a wrapped RateLimitError must match ErrRateLimited")
			}
		})
	}
}

func TestLastRateLimitUnknownHost(t *testing.T) {
	if _, ok := New(nil).LastRateLimit("ghcr.io", ""); ok {
		t.Error("no quota was observed")
	}
	if got := New(nil).RateLimits(); len(got) != 0 {
		t.Errorf("RateLimits = %+v, want none", got)
	}
	var c *Client
	if _, ok := c.LastRateLimit("ghcr.io", ""); ok {
		t.Error("nil client has no quota")
	}
	if got := c.RateLimits(); got != nil {
		t.Errorf("nil client RateLimits = %+v", got)
	}
}

func TestRateLimitKeptPerAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); ok && user == "alice" {
			w.Header().Set("RateLimit-Limit", "200;w=21600")
			w.Header().Set("RateLimit-Remaining", "150;w=21600")
		} else {
			w.Header().Set("RateLimit-Limit", "100;w=21600")
			w.Header().Set("RateLimit-Remaining", "40;w=21600")
		}
		_, _ = fmt.Fprint(w, `{"tags":["1"]}`)
	}))
	defer srv.Close()
	host := hostOf(srv)
	m := &recordingMetrics{}
	c := New([]config.RegistryAuth{{Server: host + "/acme", Username: "alice", Password: "pw"}})
	c.AttachMetrics(m)
	for _, repo := range []string{"acme/app", "library/nginx", "acme/app"} {
		if _, err := c.ListTags(context.Background(), Reference{Registry: host, Repo: repo}); err != nil {
			t.Fatalf("ListTags %s: %v", repo, err)
		}
	}

	if rl, ok := c.LastRateLimit(host, "alice"); !ok || rl.Limit != 200 || rl.Remaining != 150 {
		t.Errorf("alice quota = %+v, %v; want 150 of 200", rl, ok)
	}
	if rl, ok := c.LastRateLimit(host, ""); !ok || rl.Limit != 100 || rl.Remaining != 40 {
		t.Errorf("anonymous quota = %+v, %v; want 40 of 100", rl, ok)
	}
	all := c.RateLimits()
	if len(all) != 2 || all[0].Account != "" || all[1].Account != "alice" ||
		all[0].Registry != host || all[1].Remaining != 150 || all[0].At.IsZero() {
		t.Errorf("RateLimits = %+v, want anonymous then alice", all)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := strings.Join(m.rateLimit, ","); got != "alice:150/200,40/100,alice:150/200" {
		t.Errorf("rate limit metrics = %s", got)
	}
}

func TestObserveRateLimitKeepsNewestArrival(t *testing.T) {
	quota := func(remaining int) http.Header {
		h := http.Header{}
		h.Set("RateLimit-Limit", "100")
		h.Set("RateLimit-Remaining", strconv.Itoa(remaining))
		return h
	}
	m := &recordingMetrics{}
	c := New(nil)
	c.AttachMetrics(m)
	t0 := time.Now()
	c.observeRateLimit("hub", "", t0.Add(time.Second), quota(10))
	c.observeRateLimit("hub", "", t0, quota(50))
	if rl, _ := c.LastRateLimit("hub", ""); rl.Remaining != 10 {
		t.Errorf("Remaining = %d, want 10 (the later arrival)", rl.Remaining)
	}
	c.observeRateLimit("hub", "", t0.Add(2*time.Second), quota(9))
	if rl, _ := c.LastRateLimit("hub", ""); rl.Remaining != 9 {
		t.Errorf("Remaining = %d, want 9", rl.Remaining)
	}
	c.observeRateLimit("hub", "", t0.Add(3*time.Second), http.Header{})
	if rl, _ := c.LastRateLimit("hub", ""); rl.Remaining != 9 {
		t.Errorf("Remaining = %d after a response without quota, want 9", rl.Remaining)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := strings.Join(m.rateLimit, ","); got != "10/100,9/100" {
		t.Errorf("rate limit metrics = %s, want the stale quota skipped", got)
	}
}
