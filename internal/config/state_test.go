package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStateDefaults(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		name     string
		doc      string
		enabled  bool
		dir      string
		path     string
		printed  bool
		warnings []string
	}{
		{name: "on by default", doc: "dry_run: true\n", enabled: true, dir: DefaultStateDir,
			path: filepath.Join(DefaultStateDir, "state.json"), printed: true},
		{name: "own directory", doc: "state:\n  dir: /data/dockpaum/\n", enabled: true, dir: "/data/dockpaum/",
			path: "/data/dockpaum/state.json", printed: true},
		{name: "blank directory is the default", doc: "state:\n  dir: '  '\n", enabled: true, dir: DefaultStateDir,
			path: filepath.Join(DefaultStateDir, "state.json"), printed: true},
		{name: "off", doc: "state:\n  enabled: false\n", enabled: false, dir: "", path: "", printed: false},
		{name: "off with a directory", doc: "state:\n  enabled: false\n  dir: /data/state\n", enabled: false, dir: "/data/state", path: "",
			warnings: []string{"line 3: state.dir has no effect: state.enabled is false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := decode([]byte(tt.doc), "dockpaum.yml")
			if err != nil {
				t.Fatal(err)
			}
			if c.State.IsEnabled() != tt.enabled || c.State.Dir != tt.dir || c.State.Path() != tt.path {
				t.Errorf("state = %+v (enabled %v, path %q), want enabled %v, dir %q, path %q",
					c.State, c.State.IsEnabled(), c.State.Path(), tt.enabled, tt.dir, tt.path)
			}
			if red := c.Redacted(); red.State.Enabled == nil || *red.State.Enabled != tt.enabled {
				t.Errorf("--print-config shows state.enabled %v, want %v", red.State.Enabled, tt.enabled)
			}
			got := strings.Join(c.Warnings(), "\n")
			for _, w := range tt.warnings {
				if !strings.Contains(got, w) {
					t.Errorf("warnings lack %q:\n%s", w, got)
				}
			}
			if len(tt.warnings) == 0 && strings.Contains(got, "state.") {
				t.Errorf("unexpected state warning:\n%s", got)
			}
		})
	}
}

func TestStateConfigOfAHandBuiltConfig(t *testing.T) {
	off := false
	for _, tt := range []struct {
		s    StateConfig
		want string
	}{
		{StateConfig{}, filepath.Join(DefaultStateDir, "state.json")},
		{StateConfig{Dir: "/srv/state"}, "/srv/state/state.json"},
		{StateConfig{Enabled: &off, Dir: "/srv/state"}, ""},
	} {
		if got := tt.s.Path(); got != tt.want {
			t.Errorf("%+v.Path() = %q, want %q", tt.s, got, tt.want)
		}
	}
	if !(StateConfig{}).IsEnabled() {
		t.Error("the state must be on unless state.enabled is false")
	}
}

func TestStateDirMustBeAbsolute(t *testing.T) {
	isolateEnv(t)
	for _, doc := range []string{"state:\n  dir: data/state\n", "state:\n  enabled: false\n  dir: ./state\n"} {
		err := decodeErr(t, doc)
		if !strings.Contains(err.Error(), "state.dir:") || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Errorf("%q: err = %v, want state.dir to require an absolute path", doc, err)
		}
	}
}

func TestInstance(t *testing.T) {
	isolateEnv(t)
	for doc, want := range map[string]string{
		"dry_run: true\n":                                         "",
		"instance: '  nas-1 '\n":                                  "nas-1",
		"instance: Büro NAS (rack 2)\n":                           "Büro NAS (rack 2)",
		"instance: " + strings.Repeat("ä", maxInstanceLen) + "\n": strings.Repeat("ä", maxInstanceLen),
		"instance: \"nas\u00a01\"\n":                              "nas\u00a01",
		"instance: \"\u6771\u4eac\u3000nas\"\n":                   "\u6771\u4eac\u3000nas",
		"instance: \"lab \U0001F9D1\u200d\U0001F4BB\"\n":          "lab \U0001F9D1\u200d\U0001F4BB",
	} {
		c, err := decode([]byte(doc), "dockpaum.yml")
		if err != nil {
			t.Errorf("%q: %v", doc, err)
			continue
		}
		if c.Instance != want {
			t.Errorf("%q: instance %q, want %q", doc, c.Instance, want)
		}
	}
	for doc, want := range map[string]string{
		"instance: \"nas\\n1\"\n":                                   `"nas\n1" contains a line break (U+000A); use a short name such as nas-1`,
		"instance: \"nas\\u20281\"\n":                               "contains a line break (U+2028)",
		"instance: \"a\\tb\"\n":                                     "contains a tab (U+0009)",
		"instance: \"a\\x07b\"\n":                                   "contains a control character (U+0007)",
		"instance: \"nas\\u202e1\"\n":                               "contains an invisible formatting character (U+202E)",
		"instance: \"nas\\ue0001\"\n":                               "contains a character that is not printable (U+E000)",
		"instance: " + strings.Repeat("x", maxInstanceLen+1) + "\n": "65 characters is too long for a name (at most 64)",
	} {
		err := decodeErr(t, doc)
		if !strings.Contains(err.Error(), "instance: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", doc, err, want)
		}
	}
}

func TestDeferredRefsInInstanceAndStateDir(t *testing.T) {
	isolateEnv(t)
	for doc, want := range map[string]string{
		"instance: ${DOCKPAUM_INSTANCE}\n":  "instance: ${DOCKPAUM_INSTANCE} stays literal here — ${DOCKPAUM_*} references are kept for hooks and jobs and expanded only there; DOCKPAUM_INSTANCE is read from the environment by itself",
		"state:\n  dir: ${DOCKPAUM_HOST}\n": "state.dir: ${DOCKPAUM_HOST} stays literal here",
	} {
		if err := decodeErr(t, doc); !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", doc, err, want)
		}
	}
}
