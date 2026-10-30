package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func hostOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestListTagsFollowsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/repo/tags/list" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("last") == "b" {
			_, _ = fmt.Fprint(w, `{"name":"repo","tags":["c","d"]}`)
			return
		}
		w.Header().Set("Link", `</v2/repo/tags/list?last=b>; rel="next"`)
		_, _ = fmt.Fprint(w, `{"name":"repo","tags":["a","b"]}`)
	}))
	defer srv.Close()

	c := New(nil)
	tags, err := c.ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("ListTags = %v, want %v", tags, want)
	}
}

func TestListTagsBearerAuth(t *testing.T) {
	var srv *httptest.Server
	var tokenCalls atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			if got := r.URL.Query().Get("service"); got != "x" {
				t.Errorf("token request service = %q, want %q", got, "x")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"token":"t"}`)
		case "/v2/repo/tags/list":
			if r.Header.Get("Authorization") != "Bearer t" {
				w.Header().Set("Www-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="x"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"name":"repo","tags":["1.0.0","1.0.1"]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(nil)
	tags, err := c.ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	want := []string{"1.0.0", "1.0.1"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("ListTags = %v, want %v", tags, want)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1", got)
	}
}

func TestRegistryScheme(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{host: "localhost", want: "http"},
		{host: "localhost:5000", want: "http"},
		{host: "127.0.0.1:5000", want: "http"},
		{host: "[::1]:5000", want: "http"},
		{host: "localhost.evil.com", want: "https"},
		{host: "ghcr.io", want: "https"},
		{host: "myregistry.local:5000", want: "https"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := registryScheme(tt.host); got != tt.want {
				t.Errorf("registryScheme(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "relative next link",
			header: `</v2/repo/tags/list?last=b>; rel="next"`,
			want:   "/v2/repo/tags/list?last=b",
		},
		{
			name:   "absolute next link",
			header: `<https://example.com/v2/repo/tags/list?last=z>; rel="next"`,
			want:   "https://example.com/v2/repo/tags/list?last=z",
		},
		{
			name:   "unquoted rel",
			header: `</v2/repo/tags/list?last=b>; rel=next`,
			want:   "/v2/repo/tags/list?last=b",
		},
		{
			name:   "picks rel next among multiple links",
			header: `</v2/r?last=a>; rel="prev", </v2/r?last=m>; rel="next"`,
			want:   "/v2/r?last=m",
		},
		{
			name:   "missing header",
			header: "",
			want:   "",
		},
		{
			name:   "no next relation",
			header: `</v2/r?last=a>; rel="prev"`,
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextLink(tt.header); got != tt.want {
				t.Errorf("nextLink(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestListTagsRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"repo","tags":["1.0.0"]}`)
	}))
	defer srv.Close()

	c := New(nil)
	tags, err := c.ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err != nil {
		t.Fatalf("ListTags after two 500s: %v", err)
	}
	want := []string{"1.0.0"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("ListTags = %v, want %v", tags, want)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3 (two failures plus success)", got)
	}
}

func TestServerErrorsThroughTheRetriesAreUnavailable(t *testing.T) {
	err := fmt.Errorf("after retries: %w", serverError(http.StatusServiceUnavailable))
	if !errors.Is(err, ErrUnavailable) || err.Error() != "after retries: HTTP 503" {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(&HTTPError{Op: "manifest", StatusCode: http.StatusNotFound}, ErrUnavailable) {
		t.Fatal("a 404 is an answer, not an unavailable registry")
	}
}

func TestListTagsRefusesSilentTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</v2/repo/tags/list?last=x>; rel="next"`)
		_, _ = fmt.Fprint(w, `{"tags":["1.0.0"]}`)
	}))
	defer srv.Close()
	_, err := New(nil).ListTags(context.Background(), Reference{Registry: hostOf(srv), Repo: "repo"})
	if err == nil || !strings.Contains(err.Error(), "pages of tags") {
		t.Fatalf("endless pagination must be an error, got %v", err)
	}
}
