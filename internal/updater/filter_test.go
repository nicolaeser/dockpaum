package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
)

func loadFilters(t *testing.T, yaml string) config.FilterConfig {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); config.IsConfigEnv(name) {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv(config.EnvConfigURL, "")
	path := filepath.Join(t.TempDir(), "dockpaum.yml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Validate(path)
	if err != nil {
		t.Fatalf("config.Validate: %v", err)
	}
	return cfg.Filters
}

func ct(name string, labels map[string]string) docker.Container {
	return docker.Container{ID: "id-" + name, Name: name, Image: "img", Labels: labels}
}

func TestKeepLabelEnableFalseMode(t *testing.T) {
	f := config.FilterConfig{LabelEnable: false}

	tests := []struct {
		name       string
		container  docker.Container
		wantKeep   bool
		wantReason string
	}{
		{
			name:      "no label kept",
			container: ct("plain", nil),
			wantKeep:  true,
		},
		{
			name:      "enable true kept",
			container: ct("opted-in", map[string]string{LabelEnable: "true"}),
			wantKeep:  true,
		},
		{
			name:       "enable false excluded",
			container:  ct("opted-out", map[string]string{LabelEnable: "false"}),
			wantKeep:   false,
			wantReason: "label-disabled",
		},
		{
			name:       "enable FALSE case-insensitive excluded",
			container:  ct("opted-out-caps", map[string]string{LabelEnable: "FALSE"}),
			wantKeep:   false,
			wantReason: "label-disabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := keep(tc.container, f)
			if got != tc.wantKeep {
				t.Fatalf("keep(%s) = %v, want %v (reason %q)", tc.container.Name, got, tc.wantKeep, reason)
			}
			if !tc.wantKeep && reason != tc.wantReason {
				t.Fatalf("keep(%s) reason = %q, want %q", tc.container.Name, reason, tc.wantReason)
			}
		})
	}
}

func TestKeepLabelEnableTrueMode(t *testing.T) {
	f := config.FilterConfig{LabelEnable: true}

	tests := []struct {
		name       string
		container  docker.Container
		wantKeep   bool
		wantReason string
	}{
		{
			name:       "no label excluded",
			container:  ct("plain", nil),
			wantKeep:   false,
			wantReason: "label-disabled",
		},
		{
			name:      "enable true kept",
			container: ct("opted-in", map[string]string{LabelEnable: "true"}),
			wantKeep:  true,
		},
		{
			name:       "enable false excluded",
			container:  ct("opted-out", map[string]string{LabelEnable: "false"}),
			wantKeep:   false,
			wantReason: "label-disabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := keep(tc.container, f)
			if got != tc.wantKeep {
				t.Fatalf("keep(%s) = %v, want %v (reason %q)", tc.container.Name, got, tc.wantKeep, reason)
			}
			if !tc.wantKeep && reason != tc.wantReason {
				t.Fatalf("keep(%s) reason = %q, want %q", tc.container.Name, reason, tc.wantReason)
			}
		})
	}
}

func TestKeepNameLists(t *testing.T) {
	tests := []struct {
		name       string
		filter     config.FilterConfig
		container  docker.Container
		wantKeep   bool
		wantReason string
	}{
		{
			name:      "include_names match kept",
			filter:    config.FilterConfig{IncludeNames: []string{"web", "db"}},
			container: ct("web", nil),
			wantKeep:  true,
		},
		{
			name:       "include_names miss excluded",
			filter:     config.FilterConfig{IncludeNames: []string{"web", "db"}},
			container:  ct("cache", nil),
			wantKeep:   false,
			wantReason: "not-in-include-list",
		},
		{
			name:       "exclude_names match excluded",
			filter:     config.FilterConfig{ExcludeNames: []string{"db"}},
			container:  ct("db", nil),
			wantKeep:   false,
			wantReason: "in-exclude-list",
		},
		{
			name:      "exclude_names miss kept",
			filter:    config.FilterConfig{ExcludeNames: []string{"db"}},
			container: ct("web", nil),
			wantKeep:  true,
		},
		{
			name:       "exclude wins over include",
			filter:     config.FilterConfig{IncludeNames: []string{"web"}, ExcludeNames: []string{"web"}},
			container:  ct("web", nil),
			wantKeep:   false,
			wantReason: "in-exclude-list",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := keep(tc.container, tc.filter)
			if got != tc.wantKeep {
				t.Fatalf("keep(%s) = %v, want %v (reason %q)", tc.container.Name, got, tc.wantKeep, reason)
			}
			if !tc.wantKeep && reason != tc.wantReason {
				t.Fatalf("keep(%s) reason = %q, want %q", tc.container.Name, reason, tc.wantReason)
			}
		})
	}
}

func TestKeepRegexFilters(t *testing.T) {
	f := loadFilters(t, `
filters:
  include_regex: "^app-"
  exclude_regex: "beta"
`)
	if f.IncludeRegexp() == nil || f.ExcludeRegexp() == nil {
		t.Fatal("expected compiled include/exclude regexps after config validate")
	}

	tests := []struct {
		name       string
		container  docker.Container
		wantKeep   bool
		wantReason string
	}{
		{
			name:      "include regex match kept",
			container: ct("app-frontend", nil),
			wantKeep:  true,
		},
		{
			name:       "include regex no match excluded",
			container:  ct("web-frontend", nil),
			wantKeep:   false,
			wantReason: "include-regex-no-match",
		},
		{
			name:       "exclude regex match excluded",
			container:  ct("app-beta", nil),
			wantKeep:   false,
			wantReason: "exclude-regex-match",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := keep(tc.container, f)
			if got != tc.wantKeep {
				t.Fatalf("keep(%s) = %v, want %v (reason %q)", tc.container.Name, got, tc.wantKeep, reason)
			}
			if !tc.wantKeep && reason != tc.wantReason {
				t.Fatalf("keep(%s) reason = %q, want %q", tc.container.Name, reason, tc.wantReason)
			}
		})
	}
}

func TestKeepTempContainersAlwaysExcluded(t *testing.T) {
	tests := []struct {
		name      string
		container docker.Container
		filter    config.FilterConfig
	}{
		{
			name:      "dockpaum-old leftover",
			container: ct("web.dockpaum-old-1718000000000", nil),
		},
		{
			name:      "self-update-old leftover",
			container: ct("dockpaum.self-update-old-1718000000000", nil),
		},
		{
			name: "dockpaum-old excluded even when explicitly included and labeled",
			container: ct("web.dockpaum-old-42",
				map[string]string{LabelEnable: "true"}),
			filter: config.FilterConfig{
				LabelEnable:  true,
				IncludeNames: []string{"web.dockpaum-old-42"},
			},
		},
		{
			name: "self-update-old excluded even when explicitly included",
			container: ct("d.self-update-old-42",
				map[string]string{LabelEnable: "true"}),
			filter: config.FilterConfig{IncludeNames: []string{"d.self-update-old-42"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := keep(tc.container, tc.filter)
			if got {
				t.Fatalf("keep(%s) = true, want false", tc.container.Name)
			}
			if reason != "dockpaum-temp-container" {
				t.Fatalf("keep(%s) reason = %q, want %q", tc.container.Name, reason, "dockpaum-temp-container")
			}
		})
	}
}

func TestKeepScopes(t *testing.T) {
	scoped := docker.Container{Name: "a", Labels: map[string]string{"dockpaum.scope": "prod"}}
	wtScoped := docker.Container{Name: "b", Labels: map[string]string{"com.centurylinklabs.watchtower.scope": "prod"}}
	plain := docker.Container{Name: "c"}
	off := false
	tests := []struct {
		name   string
		ct     docker.Container
		f      config.FilterConfig
		want   bool
		reason string
	}{
		{"unscoped instance skips scoped container", scoped, config.FilterConfig{}, false, "scoped-elsewhere"},
		{"unscoped instance keeps plain container", plain, config.FilterConfig{}, true, ""},
		{"scoped instance keeps its scope", scoped, config.FilterConfig{Scope: "prod"}, true, ""},
		{"scoped instance skips plain container", plain, config.FilterConfig{Scope: "prod"}, false, "scope-mismatch"},
		{"watchtower scope label honored", wtScoped, config.FilterConfig{Scope: "prod"}, true, ""},
		{"watchtower scope ignored when compat off", wtScoped, config.FilterConfig{WatchtowerLabels: &off}, true, ""},
	}
	for _, tt := range tests {
		ok, reason := keep(tt.ct, tt.f)
		if ok != tt.want || reason != tt.reason {
			t.Errorf("%s: keep = (%v, %q), want (%v, %q)", tt.name, ok, reason, tt.want, tt.reason)
		}
	}
}
