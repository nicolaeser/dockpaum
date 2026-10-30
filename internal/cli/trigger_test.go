package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/server"
)

func TestSelectTargets(t *testing.T) {
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := []struct {
		name             string
		match, other     map[string]bool
		only             []string
		names, conflicts string
	}{
		{"all matches", set("web", "proxy"), set("db"), nil, "[proxy web]", "[]"},
		{"no match", set(), set("db"), nil, "[]", "[]"},
		{"name shared with another image", set("web", "proxy"), set("web"), nil, "[proxy]", "[web]"},
		{"restricted to requested containers", set("web", "proxy"), set(), []string{"proxy", "db"}, "[proxy]", "[]"},
	}
	for _, c := range cases {
		names, conflicts := selectTargets(c.match, c.other, c.only)
		if fmt.Sprint(names) != c.names {
			t.Errorf("%s: names = %v, want %s", c.name, names, c.names)
		}
		if fmt.Sprint(conflicts) != c.conflicts {
			t.Errorf("%s: conflicts = %v, want %s", c.name, conflicts, c.conflicts)
		}
	}
}

type fakeSource struct {
	list        []docker.Container
	listErr     error
	configImage map[string]string
	inspected   []string
}

func (f *fakeSource) ListContainers(context.Context, bool) ([]docker.Container, error) {
	return f.list, f.listErr
}

func (f *fakeSource) Inspect(_ context.Context, id string) (*docker.FullContainer, error) {
	f.inspected = append(f.inspected, id)
	img, ok := f.configImage[id]
	if !ok {
		return nil, errors.New("no such container")
	}
	return &docker.FullContainer{ID: id, Image: img}, nil
}

func TestClassifyContainers(t *testing.T) {
	const oldID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name         string
		images       []string
		list         []docker.Container
		configImage  map[string]string
		match, other string
		inspected    string
		err          string
	}{
		{
			name:   "listed reference",
			images: []string{"nginx"},
			list: []docker.Container{
				{ID: "1", Name: "web", Image: "nginx:1.27", ImageID: oldID},
				{ID: "2", Name: "db", Image: "postgres:16", ImageID: oldID},
			},
			match: "[web]", other: "[db]", inspected: "[]",
		},
		{
			name:        "tag moved to a new image",
			images:      []string{"myapp"},
			list:        []docker.Container{{ID: "1", Name: "app", Image: oldID, ImageID: oldID}},
			configImage: map[string]string{"1": "myapp:latest"},
			match:       "[app]", other: "[]", inspected: "[1]",
		},
		{
			name:        "image ID without the ImageID field",
			images:      []string{"ghcr.io/acme/api:2"},
			list:        []docker.Container{{ID: "1", Name: "api", Image: oldID}},
			configImage: map[string]string{"1": "ghcr.io/acme/api:2"},
			match:       "[api]", other: "[]", inspected: "[1]",
		},
		{
			name:        "moved tag of another image",
			images:      []string{"nginx"},
			list:        []docker.Container{{ID: "1", Name: "web", Image: oldID, ImageID: oldID}},
			configImage: map[string]string{"1": "httpd:2.4"},
			match:       "[]", other: "[web]", inspected: "[1]",
		},
		{
			name:        "created from an image ID",
			images:      []string{"nginx"},
			list:        []docker.Container{{ID: "1", Name: "adhoc", Image: oldID, ImageID: oldID}},
			configImage: map[string]string{"1": oldID},
			match:       "[]", other: "[adhoc]", inspected: "[1]",
		},
		{
			name:   "inspect fails",
			images: []string{"nginx"},
			list:   []docker.Container{{ID: "gone", Name: "web", Image: oldID, ImageID: oldID}},
			err:    "inspect container web: no such container",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeSource{list: c.list, configImage: c.configImage}
			match, other := map[string]bool{}, map[string]bool{}
			err := classifyContainers(context.Background(), src, false, server.TriggerRequest{Images: c.images}, match, other)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(slices.Sorted(maps.Keys(match))); got != c.match {
				t.Errorf("match = %s, want %s", got, c.match)
			}
			if got := fmt.Sprint(slices.Sorted(maps.Keys(other))); got != c.other {
				t.Errorf("other = %s, want %s", got, c.other)
			}
			if got := fmt.Sprint(src.inspected); got != c.inspected {
				t.Errorf("inspected = %s, want %s", got, c.inspected)
			}
		})
	}

	src := &fakeSource{listErr: errors.New("daemon gone")}
	if err := classifyContainers(context.Background(), src, false, server.TriggerRequest{Images: []string{"nginx"}}, map[string]bool{}, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "list containers: daemon gone") {
		t.Errorf("list failure: %v", err)
	}
}

func fakeEngine(t *testing.T, list []map[string]any, inspect map[string]map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = io.WriteString(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
			id := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/containers/")+len("/containers/"):], "/json")
			doc, ok := inspect[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"No such container"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(doc)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unexpected request"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + strings.TrimPrefix(srv.URL, "http://")
}

func TestImageTargets(t *testing.T) {
	const oldID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ct := func(id, name, image, imageID string) map[string]any {
		return map[string]any{"Id": id, "Names": []string{"/" + name}, "Image": image, "ImageID": imageID, "State": "running", "Labels": map[string]string{}}
	}
	insp := func(id, name, configImage string) map[string]any {
		return map[string]any{
			"Id": id, "Name": "/" + name, "Image": oldID,
			"Config":     map[string]any{"Image": configImage},
			"HostConfig": map[string]any{},
			"State":      map[string]any{"Running": true},
		}
	}
	hostA := fakeEngine(t,
		[]map[string]any{
			ct("a1", "app", oldID, oldID),
			ct("a2", "proxy", "nginx:1.27", "sha256:aa"),
			ct("a3", "web", "nginx:1.27", "sha256:aa"),
		},
		map[string]map[string]any{"a1": insp("a1", "app", "myapp:latest")})
	hostB := fakeEngine(t,
		[]map[string]any{
			ct("b1", "web", "httpd:2.4", "sha256:bb"),
			ct("b2", "db", "postgres:16", "sha256:cc"),
		}, nil)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	clients, err := docker.OpenAll(ctx, []config.HostConfig{{Name: "a", Endpoint: hostA}, {Name: "b", Endpoint: hostB}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clients.Close)

	cases := []struct {
		images, only     []string
		names, conflicts string
	}{
		{[]string{"myapp"}, nil, "[app]", "[]"},
		{[]string{"nginx"}, nil, "[proxy]", "[web]"},
		{[]string{"nginx", "myapp"}, []string{"app"}, "[app]", "[web]"},
		{[]string{"redis"}, nil, "[]", "[]"},
	}
	for _, c := range cases {
		names, conflicts, err := imageTargets(ctx, clients, false, server.TriggerRequest{Images: c.images, Containers: c.only})
		if err != nil {
			t.Fatalf("images %q: %v", c.images, err)
		}
		if fmt.Sprint(names) != c.names || fmt.Sprint(conflicts) != c.conflicts {
			t.Errorf("images %q only %q: names %v conflicts %v, want %s %s", c.images, c.only, names, conflicts, c.names, c.conflicts)
		}
	}
}

func TestImageTargetsUnreachableHost(t *testing.T) {
	up := fakeEngine(t, []map[string]any{{"Id": "1", "Names": []string{"/web"}, "Image": "nginx", "ImageID": "sha256:aa"}}, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "tcp://" + l.Addr().String()
	_ = l.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clients, err := docker.OpenAll(context.Background(), []config.HostConfig{{Name: "up", Endpoint: up}, {Name: "down", Endpoint: down}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clients.Close)
	_, _, err = imageTargets(context.Background(), clients, false, server.TriggerRequest{Images: []string{"nginx"}})
	if err == nil || !strings.Contains(err.Error(), "unreachable: down") {
		t.Fatalf("err = %v, want the unreachable host named", err)
	}
}
