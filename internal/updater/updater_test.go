package updater

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/policy"
)

func TestReplaceTag(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		newTag   string
		want     string
	}{
		{
			name:     "short form stays short form",
			imageRef: "nginx:1.25",
			newTag:   "1.26",
			want:     "nginx:1.26",
		},
		{
			name:     "fully qualified tag swapped",
			imageRef: "ghcr.io/a/b:v1",
			newTag:   "v2",
			want:     "ghcr.io/a/b:v2",
		},
		{
			name:     "registry port colon untouched, only tag replaced",
			imageRef: "registry:5000/a/b:1.0",
			newTag:   "1.1",
			want:     "registry:5000/a/b:1.1",
		},
		{
			name:     "registry port colon with no tag appends tag",
			imageRef: "registry:5000/a/b",
			newTag:   "1.1",
			want:     "registry:5000/a/b:1.1",
		},
		{
			name:     "no tag appends tag",
			imageRef: "nginx",
			newTag:   "1.26",
			want:     "nginx:1.26",
		},
		{
			name:     "digest reference stripped, tag appended",
			imageRef: "a/b@sha256:deadbeefdeadbeef",
			newTag:   "2.0",
			want:     "a/b:2.0",
		},
		{
			name:     "tag plus digest stripped, tag replaced",
			imageRef: "ghcr.io/a/b:v1@sha256:deadbeefdeadbeef",
			newTag:   "v2",
			want:     "ghcr.io/a/b:v2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaceTag(tc.imageRef, tc.newTag); got != tc.want {
				t.Fatalf("replaceTag(%q, %q) = %q, want %q", tc.imageRef, tc.newTag, got, tc.want)
			}
		})
	}
}

func TestDependencyList(t *testing.T) {
	tests := []struct {
		name  string
		label string
		want  []string
	}{
		{
			name:  "compose condition format",
			label: "db:service_healthy:false,cache:service_started:false",
			want:  []string{"db", "cache"},
		},
		{
			name:  "comma plus space separated",
			label: "db, cache",
			want:  []string{"db", "cache"},
		},
		{
			name:  "empty label is nil",
			label: "",
			want:  nil,
		},
		{
			name:  "single service",
			label: "db",
			want:  []string{"db"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dependencyList(tc.label)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dependencyList(%q) = %v, want %v", tc.label, got, tc.want)
			}
		})
	}
}

func composeCt(name, project, service, number, dependsOn string) docker.Container {
	labels := map[string]string{}
	if project != "" {
		labels[policy.ComposeProjectLabel] = project
	}
	if service != "" {
		labels[policy.ComposeServiceLabel] = service
	}
	if number != "" {
		labels[policy.ComposeNumberLabel] = number
	}
	if dependsOn != "" {
		labels[policy.ComposeDependsLabel] = dependsOn
	}
	return docker.Container{ID: "id-" + name, Name: name, Image: "img", Labels: labels}
}

func names(cts []docker.Container) []string {
	out := make([]string, 0, len(cts))
	for _, c := range cts {
		out = append(out, c.Name)
	}
	return out
}

func TestSortByDependenciesChain(t *testing.T) {
	zoo := composeCt("p-zookeeper-1", "p", "zookeeper", "1", "")
	broker := composeCt("p-broker-1", "p", "broker", "1", "zookeeper:service_started:false")
	app := composeCt("p-app-1", "p", "app", "1", "broker:service_started:false")

	want := []string{"p-zookeeper-1", "p-broker-1", "p-app-1"}

	permutations := [][]docker.Container{
		{app, broker, zoo},
		{zoo, broker, app},
		{broker, zoo, app},
		{app, zoo, broker},
		{broker, app, zoo},
		{zoo, app, broker},
	}
	for i, perm := range permutations {
		in := append([]docker.Container(nil), perm...)
		got := names(sortByDependencies(in))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("permutation %d (%v): got %v, want %v", i, names(perm), got, want)
		}
	}
}

func TestSortByDependenciesAlphabeticalConflict(t *testing.T) {
	aaa := composeCt("p-aaa-1", "p", "aaa", "1", "zzz:service_started:false")
	zzz := composeCt("p-zzz-1", "p", "zzz", "1", "")

	want := []string{"p-zzz-1", "p-aaa-1"}
	for i, in := range [][]docker.Container{{aaa, zzz}, {zzz, aaa}} {
		got := names(sortByDependencies(append([]docker.Container(nil), in...)))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("permutation %d: got %v, want %v", i, got, want)
		}
	}
}

func TestSortByDependenciesCycle(t *testing.T) {
	a := composeCt("p-a-1", "p", "a", "1", "b:service_started:false")
	b := composeCt("p-b-1", "p", "b", "1", "a:service_started:false")

	got := sortByDependencies([]docker.Container{a, b})
	if len(got) != 2 {
		t.Fatalf("cycle: got %d containers (%v), want 2", len(got), names(got))
	}
	seen := map[string]int{}
	for _, c := range got {
		seen[c.Name]++
	}
	for _, name := range []string{"p-a-1", "p-b-1"} {
		if seen[name] != 1 {
			t.Errorf("cycle: container %q appeared %d times, want exactly once", name, seen[name])
		}
	}
}

func TestSortByDependenciesNoServiceLabelLast(t *testing.T) {
	web := composeCt("m-web-1", "m", "web", "1", "")
	loner1 := docker.Container{ID: "id-zzz", Name: "zzz-standalone", Image: "img"}
	loner2 := docker.Container{ID: "id-aaa", Name: "aaa-standalone", Image: "img"}

	got := names(sortByDependencies([]docker.Container{loner1, web, loner2}))
	want := []string{"m-web-1", "aaa-standalone", "zzz-standalone"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSortByDependenciesReplicasStayTogether(t *testing.T) {
	db := composeCt("p-db-1", "p", "db", "1", "")
	web1 := composeCt("p-web-1", "p", "web", "1", "db:service_started:false")
	web2 := composeCt("p-web-2", "p", "web", "2", "db:service_started:false")

	got := names(sortByDependencies([]docker.Container{web2, db, web1}))
	want := []string{"p-db-1", "p-web-1", "p-web-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGroupByCompose(t *testing.T) {
	resolver, err := policy.NewResolver(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	alphaWeb := composeCt("alpha-web-1", "alpha", "web", "1", "")
	alphaDB := composeCt("alpha-db-1", "alpha", "db", "1", "")
	betaAPI := composeCt("beta-api-1", "beta", "api", "1", "")
	loner1 := docker.Container{ID: "id-s1", Name: "standalone-1", Image: "img"}
	loner2 := docker.Container{ID: "id-s2", Name: "standalone-2", Image: "img"}

	groups := groupByCompose(
		[]docker.Container{alphaWeb, betaAPI, loner1, alphaDB, loner2},
		resolver,
	)

	if len(groups) != 4 {
		gnames := make([]string, 0, len(groups))
		for _, g := range groups {
			gnames = append(gnames, g.name)
		}
		t.Fatalf("got %d groups (%v), want 4", len(groups), gnames)
	}

	byName := map[string][]string{}
	for _, g := range groups {
		byName[g.name] = names(g.containers)
	}

	wantAlpha := map[string]bool{"alpha-web-1": true, "alpha-db-1": true}
	gotAlpha := byName["alpha"]
	if len(gotAlpha) != 2 || !wantAlpha[gotAlpha[0]] || !wantAlpha[gotAlpha[1]] || gotAlpha[0] == gotAlpha[1] {
		t.Errorf("group alpha = %v, want both alpha containers", gotAlpha)
	}
	if got := byName["beta"]; !reflect.DeepEqual(got, []string{"beta-api-1"}) {
		t.Errorf("group beta = %v, want [beta-api-1]", got)
	}
	if got := byName["__ungrouped__:standalone-1"]; !reflect.DeepEqual(got, []string{"standalone-1"}) {
		t.Errorf("ungrouped standalone-1 group = %v", got)
	}
	if got := byName["__ungrouped__:standalone-2"]; !reflect.DeepEqual(got, []string{"standalone-2"}) {
		t.Errorf("ungrouped standalone-2 group = %v", got)
	}
}
