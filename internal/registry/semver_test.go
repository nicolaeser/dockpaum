package registry

import (
	"reflect"
	"testing"
)

func TestParseTag(t *testing.T) {
	tests := []struct {
		name       string
		tag        string
		wantOK     bool
		wantHasV   bool
		wantComps  []int
		wantSuffix string
	}{
		{name: "v-prefix", tag: "v1.2.3", wantOK: true, wantHasV: true, wantComps: []int{1, 2, 3}, wantSuffix: ""},
		{name: "plain", tag: "1.2.3", wantOK: true, wantHasV: false, wantComps: []int{1, 2, 3}, wantSuffix: ""},
		{name: "variant suffix", tag: "1.2.3-alpine", wantOK: true, wantHasV: false, wantComps: []int{1, 2, 3}, wantSuffix: "-alpine"},
		{name: "prerelease suffix", tag: "1.2.3-rc1", wantOK: true, wantHasV: false, wantComps: []int{1, 2, 3}, wantSuffix: "-rc1"},
		{name: "multi-digit components", tag: "1.10.2", wantOK: true, wantHasV: false, wantComps: []int{1, 10, 2}, wantSuffix: ""},
		{name: "partial version", tag: "1.2", wantOK: true, wantHasV: false, wantComps: []int{1, 2}, wantSuffix: ""},
		{name: "latest is not semver", tag: "latest", wantOK: false},
		{name: "stable is not semver", tag: "stable", wantOK: false},
		{name: "empty string", tag: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseTag(tt.tag)
			if ok != tt.wantOK {
				t.Fatalf("ParseTag(%q) ok = %v, want %v", tt.tag, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.HasV != tt.wantHasV {
				t.Errorf("ParseTag(%q).HasV = %v, want %v", tt.tag, got.HasV, tt.wantHasV)
			}
			if !reflect.DeepEqual(got.Components, tt.wantComps) {
				t.Errorf("ParseTag(%q).Components = %v, want %v", tt.tag, got.Components, tt.wantComps)
			}
			if got.Suffix != tt.wantSuffix {
				t.Errorf("ParseTag(%q).Suffix = %q, want %q", tt.tag, got.Suffix, tt.wantSuffix)
			}
			if got.Raw != tt.tag {
				t.Errorf("ParseTag(%q).Raw = %q, want %q", tt.tag, got.Raw, tt.tag)
			}
		})
	}
}

func mustParseTag(t *testing.T, s string) Tag {
	t.Helper()
	tag, ok := ParseTag(s)
	if !ok {
		t.Fatalf("ParseTag(%q) unexpectedly failed", s)
	}
	return tag
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{name: "patch numeric ordering", a: "1.2.3", b: "1.2.4", want: -1},
		{name: "minor numeric ordering", a: "1.2.3", b: "1.3.0", want: -1},
		{name: "major numeric ordering", a: "1.9.9", b: "2.0.0", want: -1},
		{name: "multi-digit segment compares numerically", a: "1.9.0", b: "1.10.0", want: -1},
		{name: "fewer components sorts lower", a: "1.2", b: "1.2.0", want: -1},
		{name: "prerelease sorts before final release", a: "1.2.3-rc1", b: "1.2.3", want: -1},
		{name: "rc1 before rc2", a: "1.2.3-rc1", b: "1.2.3-rc2", want: -1},
		{name: "rc2 before rc10 numerically", a: "1.2.3-rc2", b: "1.2.3-rc10", want: -1},
		{name: "alpha before beta", a: "1.2.3-alpha", b: "1.2.3-beta", want: -1},
		{name: "equal tags", a: "1.2.3", b: "1.2.3", want: 0},
		{name: "equal prerelease tags", a: "1.2.3-rc1", b: "1.2.3-rc1", want: 0},
		{name: "linuxserver build counter", a: "4.0.15.2941-ls290", b: "4.0.15.2941-ls291", want: -1},
		{name: "build counter compares numerically", a: "1.2.3-ls29", b: "1.2.3-ls290", want: -1},
		{name: "version wins over build counter", a: "4.0.15.2941-ls291", b: "4.0.16.2944-ls1", want: -1},
		{name: "bitnami revision", a: "16.4.0-debian-12-r12", b: "16.4.0-debian-12-r13", want: -1},
		{name: "bitnami revision restarts with version", a: "16.4.0-debian-12-r12", b: "16.4.1-debian-12-r0", want: -1},
		{name: "semver build metadata", a: "1.2.3+5", b: "1.2.3+12", want: -1},
		{name: "build marker is case-insensitive", a: "1.2.3-LS5", b: "1.2.3-ls6", want: -1},
		{name: "prerelease before build counter", a: "1.2.3-rc1-r9", b: "1.2.3-r0", want: -1},
		{name: "no counter before counter", a: "1.2.3", b: "1.2.3-r0", want: -1},
		{name: "equal build counters", a: "1.2.3-ls5", b: "1.2.3-ls5", want: 0},
	}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := mustParseTag(t, tt.a)
			b := mustParseTag(t, tt.b)
			if got := sign(Compare(a, b)); got != tt.want {
				t.Errorf("Compare(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
			}
			if got := sign(Compare(b, a)); got != -tt.want {
				t.Errorf("Compare(%q, %q) = %d, want sign %d", tt.b, tt.a, got, -tt.want)
			}
		})
	}
}

func TestPickHighest(t *testing.T) {
	tests := []struct {
		name    string
		current string
		tags    []string
		mode    string
		want    string
	}{
		{
			name:    "patch mode stays within minor",
			current: "1.2.3",
			tags:    []string{"1.2.4", "1.2.5", "1.3.0", "2.0.0"},
			mode:    "patch",
			want:    "1.2.5",
		},
		{
			name:    "minor mode stays within major",
			current: "1.2.3",
			tags:    []string{"1.2.4", "1.3.0", "1.10.0", "2.0.0"},
			mode:    "minor",
			want:    "1.10.0",
		},
		{
			name:    "major mode allows anything higher",
			current: "1.2.3",
			tags:    []string{"1.3.0", "2.0.0", "2.1.4"},
			mode:    "major",
			want:    "2.1.4",
		},
		{
			name:    "variant suffix only upgrades within variant",
			current: "1.2.3-alpine",
			tags:    []string{"1.2.4", "1.2.4-bookworm", "1.2.4-alpine", "1.2.5-bookworm"},
			mode:    "patch",
			want:    "1.2.4-alpine",
		},
		{
			name:    "variant current never picks bare tag",
			current: "1.2.3-alpine",
			tags:    []string{"1.2.4", "1.2.5"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "v-prefix current picks v-prefixed candidate",
			current: "v1.2.3",
			tags:    []string{"1.3.0", "v1.3.0"},
			mode:    "minor",
			want:    "v1.3.0",
		},
		{
			name:    "v-prefix current never picks bare candidate",
			current: "v1.2.3",
			tags:    []string{"1.3.0"},
			mode:    "minor",
			want:    "",
		},
		{
			name:    "bare current never picks v-prefixed candidate",
			current: "1.2.3",
			tags:    []string{"v1.3.0"},
			mode:    "minor",
			want:    "",
		},
		{
			name:    "stable current skips prerelease candidates",
			current: "1.2.3",
			tags:    []string{"1.2.4-rc1", "1.2.5-beta"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "stable current prefers final over prerelease",
			current: "1.2.3",
			tags:    []string{"1.2.4-rc1", "1.2.4"},
			mode:    "patch",
			want:    "1.2.4",
		},
		{
			name:    "prerelease current upgrades to its final release",
			current: "1.2.3-rc1",
			tags:    []string{"1.2.3"},
			mode:    "patch",
			want:    "1.2.3",
		},
		{
			name:    "prerelease current upgrades to newer prerelease",
			current: "1.2.3-rc1",
			tags:    []string{"1.2.3-rc2"},
			mode:    "patch",
			want:    "1.2.3-rc2",
		},
		{
			name:    "component count must match",
			current: "1.2",
			tags:    []string{"1.2.3", "1.2.4"},
			mode:    "minor",
			want:    "",
		},
		{
			name:    "two-component current picks two-component candidate",
			current: "1.2",
			tags:    []string{"1.2.3", "1.3"},
			mode:    "minor",
			want:    "1.3",
		},
		{
			name:    "patch mode not viable for two components",
			current: "1.2",
			tags:    []string{"1.3"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "nothing newer returns empty",
			current: "1.2.3",
			tags:    []string{"1.2.3", "1.2.2", "latest", "stable"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "empty tag list returns empty",
			current: "1.2.3",
			tags:    nil,
			mode:    "patch",
			want:    "",
		},
		{
			name:    "linuxserver patch moves version and build",
			current: "4.0.15.2941-ls290",
			tags:    []string{"4.0.15.2941-ls290", "4.0.16.2944-ls291", "4.0.16.2944", "4.1.0.3000-ls295", "version-4.0.16.2944"},
			mode:    "patch",
			want:    "4.0.16.2944-ls291",
		},
		{
			name:    "linuxserver minor moves version and build",
			current: "4.0.15.2941-ls290",
			tags:    []string{"4.0.16.2944-ls291", "4.1.0.3000-ls295", "5.0.0.1-ls300"},
			mode:    "minor",
			want:    "4.1.0.3000-ls295",
		},
		{
			name:    "linuxserver rebuild of the same version",
			current: "4.0.15.2941-ls290",
			tags:    []string{"4.0.15.2941-ls290", "4.0.15.2941-ls292", "4.0.15.2941-ls291"},
			mode:    "patch",
			want:    "4.0.15.2941-ls292",
		},
		{
			name:    "linuxserver never picks an older version with a higher build",
			current: "4.0.16.2944-ls291",
			tags:    []string{"4.0.15.2941-ls300"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "build counter current never picks bare tag",
			current: "4.0.15.2941-ls290",
			tags:    []string{"4.0.16.2944", "4.0.16.2944-r1"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "bare current never picks build counter tag",
			current: "1.2.3",
			tags:    []string{"1.2.4-ls5", "1.2.4-r0"},
			mode:    "patch",
			want:    "",
		},
		{
			name:    "bitnami patch",
			current: "16.4.0-debian-12-r12",
			tags:    []string{"16.4.0-debian-12-r13", "16.4.1-debian-12-r0", "16.4.1", "16.4.1-debian-11-r5", "17.0.0-debian-12-r0"},
			mode:    "patch",
			want:    "16.4.1-debian-12-r0",
		},
		{
			name:    "alpine package revision",
			current: "1.2.3-r0",
			tags:    []string{"1.2.3-r1", "1.2.4-r0"},
			mode:    "patch",
			want:    "1.2.4-r0",
		},
		{
			name:    "alpine:3 major skips date snapshots",
			current: "3",
			tags:    []string{"3", "4", "20260805", "20250108", "edge"},
			mode:    "major",
			want:    "4",
		},
		{
			name:    "alpine:3 major with only date snapshots",
			current: "3",
			tags:    []string{"3", "20260805"},
			mode:    "major",
			want:    "",
		},
		{
			name:    "date guard applies to the year-like first component",
			current: "24.04",
			tags:    []string{"2024.10", "24.10"},
			mode:    "major",
			want:    "24.10",
		},
		{
			name:    "date guard ignores later components",
			current: "4.0.15.2941",
			tags:    []string{"4.0.16.2944"},
			mode:    "patch",
			want:    "4.0.16.2944",
		},
		{
			name:    "calver current keeps updating",
			current: "2024.10.1",
			tags:    []string{"2024.10.2", "2025.1.0"},
			mode:    "major",
			want:    "2025.1.0",
		},
		{
			name:    "calver minor stays within the year",
			current: "2024.10.1",
			tags:    []string{"2024.10.2", "2024.11.0", "2025.1.0"},
			mode:    "minor",
			want:    "2024.11.0",
		},
		{
			name:    "date stamp current moves to newer date stamp",
			current: "20250108",
			tags:    []string{"20260805", "3"},
			mode:    "major",
			want:    "20260805",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := mustParseTag(t, tt.current)
			if got := PickHighest(cur, tt.tags, tt.mode); got != tt.want {
				t.Errorf("PickHighest(%q, %v, %q) = %q, want %q",
					tt.current, tt.tags, tt.mode, got, tt.want)
			}
		})
	}
}

func TestParseReference(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want Reference
	}{
		{
			name: "docker hub shortname",
			ref:  "nginx",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx"},
		},
		{
			name: "docker hub shortname with tag",
			ref:  "nginx:1.25-alpine",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "1.25-alpine"},
		},
		{
			name: "docker hub user repo",
			ref:  "grafana/grafana:10.2.0",
			want: Reference{Registry: "registry-1.docker.io", Repo: "grafana/grafana", Tag: "10.2.0"},
		},
		{
			name: "private registry with port",
			ref:  "myregistry.local:5000/team/app:v1.0.0",
			want: Reference{Registry: "myregistry.local:5000", Repo: "team/app", Tag: "v1.0.0"},
		},
		{
			name: "localhost registry",
			ref:  "localhost/app:1.0",
			want: Reference{Registry: "localhost", Repo: "app", Tag: "1.0"},
		},
		{
			name: "digest ref on docker hub",
			ref:  "nginx@sha256:0123456789abcdef",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx", Digest: "sha256:0123456789abcdef"},
		},
		{
			name: "tag plus digest on explicit registry",
			ref:  "ghcr.io/owner/app:1.0@sha256:deadbeef",
			want: Reference{Registry: "ghcr.io", Repo: "owner/app", Tag: "1.0", Digest: "sha256:deadbeef"},
		},
		{
			name: "explicit registry untagged",
			ref:  "ghcr.io/owner/app",
			want: Reference{Registry: "ghcr.io", Repo: "owner/app"},
		},
		{
			name: "docker.io official image",
			ref:  "docker.io/nginx:1.27",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "1.27"},
		},
		{
			name: "docker.io library path",
			ref:  "docker.io/library/nginx",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx"},
		},
		{
			name: "docker.io user repo",
			ref:  "docker.io/grafana/grafana:10.2.0",
			want: Reference{Registry: "registry-1.docker.io", Repo: "grafana/grafana", Tag: "10.2.0"},
		},
		{
			name: "index.docker.io official image",
			ref:  "index.docker.io/redis:7",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/redis", Tag: "7"},
		},
		{
			name: "registry.hub.docker.com user repo",
			ref:  "registry.hub.docker.com/linuxserver/radarr:5.14.0.9383-ls245",
			want: Reference{Registry: "registry-1.docker.io", Repo: "linuxserver/radarr", Tag: "5.14.0.9383-ls245"},
		},
		{
			name: "registry-1.docker.io official image",
			ref:  "registry-1.docker.io/nginx",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx"},
		},
		{
			name: "docker hub alias is case-insensitive",
			ref:  "Docker.IO/nginx:1",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx", Tag: "1"},
		},
		{
			name: "docker.io digest ref",
			ref:  "docker.io/nginx@sha256:0123456789abcdef",
			want: Reference{Registry: "registry-1.docker.io", Repo: "library/nginx", Digest: "sha256:0123456789abcdef"},
		},
		{
			name: "registry with docker.io suffix is not docker hub",
			ref:  "mirror.docker.io.example.com/nginx:1",
			want: Reference{Registry: "mirror.docker.io.example.com", Repo: "nginx", Tag: "1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Parse(tt.ref); got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.ref, got, tt.want)
			}
		})
	}
}
