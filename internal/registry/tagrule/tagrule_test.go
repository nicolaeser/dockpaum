package tagrule

import (
	"regexp"
	"strings"
	"testing"
)

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name                        string
		include, exclude, transform string
		wantErr                     string
	}{
		{name: "invalid include", include: "^(1\\.", wantErr: "tag include"},
		{name: "invalid exclude", exclude: "[", wantErr: "tag exclude"},
		{name: "transform without arrow", transform: "^v(.*)$", wantErr: "want"},
		{name: "transform without template", transform: "^v(.*)$ =>", wantErr: "want"},
		{name: "transform without regex", transform: " => $1", wantErr: "want"},
		{name: "invalid transform regex", transform: "^(v => $1", wantErr: "tag transform"},
		{name: "unknown group number", transform: "^v(.*)$ => $2", wantErr: "unknown group"},
		{name: "group number followed by underscore", transform: "^(\\d+)-(\\d+)$ => $1_$2", wantErr: "unknown group"},
		{name: "unknown group name", transform: "^(?P<ver>.*)$ => ${version}", wantErr: "unknown group"},
		{name: "unterminated brace", transform: "^(.*)$ => ${1", wantErr: "unterminated"},
		{name: "pattern too long", include: strings.Repeat("a", MaxLen+1), wantErr: "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(tt.include, tt.exclude, tt.transform)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Compile error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCompileAcceptsTemplates(t *testing.T) {
	for _, transform := range []string{
		"^v(.*)$ => $1",
		"^(\\d+)-(\\d+)$ => ${1}_${2}",
		"^(?P<ver>[0-9.]+)-.*$ => ${ver}",
		"^(?P<ver>[0-9.]+)-.*$ => $ver",
		"^(.*)$ => $$$1",
		"^(.*)$ => $0",
		"^(.*)$ => 1.$",
	} {
		if _, err := Compile("", "", transform); err != nil {
			t.Errorf("Compile(transform %q): %v", transform, err)
		}
	}
}

func TestZeroValue(t *testing.T) {
	var r Rules
	if !r.IsZero() || !r.Allows("anything") || r.Transform("1.2.3") != "1.2.3" {
		t.Fatal("the zero value must apply no rules")
	}
	r, err := Compile(" ", "", "")
	if err != nil || !r.IsZero() {
		t.Fatalf("blank rules = %+v, %v; want zero rules", r, err)
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name             string
		include, exclude string
		tag              string
		want             bool
	}{
		{name: "include matches", include: `^\d+\.\d+\.\d+$`, tag: "1.2.3", want: true},
		{name: "include rejects", include: `^\d+\.\d+\.\d+$`, tag: "1.2.3-alpine", want: false},
		{name: "include is unanchored", include: `alpine`, tag: "1.2.3-alpine", want: true},
		{name: "exclude rejects", exclude: `-(rc|beta)`, tag: "1.2.3-rc1", want: false},
		{name: "exclude passes", exclude: `-(rc|beta)`, tag: "1.2.3", want: true},
		{name: "exclude wins over include", include: `^1\.`, exclude: `^1\.2\.4$`, tag: "1.2.4", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Compile(tt.include, tt.exclude, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Allows(tt.tag); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestTransform(t *testing.T) {
	tests := []struct {
		transform string
		tag       string
		want      string
	}{
		{"^release-(.+)$ => $1", "release-1.2.3", "1.2.3"},
		{"^release-(.+)$ => $1", "1.2.3", "1.2.3"},
		{"^(\\d+\\.\\d+\\.\\d+)-(\\d+)$ => $1+$2", "1.2.3-45", "1.2.3+45"},
		{"^(?P<ver>[0-9.]+)_.*$ => ${ver}", "2.0.1_amd64", "2.0.1"},
		{"^(\\d+)-(\\d+)$ => ${1}.${2}", "7-4", "7.4"},
	}
	for _, tt := range tests {
		t.Run(tt.transform+"/"+tt.tag, func(t *testing.T) {
			r, err := Compile("", "", tt.transform)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Transform(tt.tag); got != tt.want {
				t.Errorf("Transform(%q) = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}
}

func TestFromRegexps(t *testing.T) {
	r := FromRegexps(regexp.MustCompile(`^2\.`), nil)
	if r.IsZero() || !r.Allows("2.1") || r.Allows("1.9") || r.Transform("2.1") != "2.1" {
		t.Fatalf("include-only rules from a compiled regexp misbehave: %+v", r)
	}
	if !FromRegexps(nil, nil).IsZero() {
		t.Fatal("no regexps must apply no rules")
	}
}
