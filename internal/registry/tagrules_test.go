package registry

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewTagRulesReportsInvalidRules(t *testing.T) {
	if _, err := NewTagRules("(", "", ""); err == nil || !strings.Contains(err.Error(), "tag include") {
		t.Fatalf("NewTagRules error = %v, want the include rule named", err)
	}
	var zero TagRules
	if !zero.IsZero() || !zero.Allows("x") || zero.Transform("1.2.3") != "1.2.3" {
		t.Fatal("the zero value must apply no rules")
	}
}

func TestTagRulesParseTagKeepsRawTag(t *testing.T) {
	r, err := NewTagRules("", "", `^release-(.+)$ => $1`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.ParseTag("release-1.2.3")
	if !ok || got.Raw != "release-1.2.3" || len(got.Components) != 3 || got.Components[2] != 3 {
		t.Fatalf("ParseTag = %+v, %v", got, ok)
	}
	if _, ok := r.ParseTag("nightly"); ok {
		t.Error("an untransformed non-version tag must not parse")
	}
}

func TestTagRulesPick(t *testing.T) {
	tests := []struct {
		name                        string
		include, exclude, transform string
		current                     string
		tags                        []string
		mode                        string
		wantOutcome                 PickOutcome
		wantTag                     string
		wantExcluded                int
	}{
		{
			name:        "include restricts candidates",
			include:     `^1\.2[0-9]\.`,
			current:     "1.25.0",
			tags:        []string{"1.25.1", "1.26.0", "1.30.0"},
			mode:        "minor",
			wantOutcome: PickFound, wantTag: "1.26.0", wantExcluded: 1,
		},
		{
			name:        "exclude drops a broken release",
			exclude:     `^1\.26\.0$`,
			current:     "1.25.0",
			tags:        []string{"1.25.1", "1.26.0"},
			mode:        "minor",
			wantOutcome: PickFound, wantTag: "1.25.1", wantExcluded: 1,
		},
		{
			name:        "transform compares the extracted version and returns the listed tag",
			transform:   `^release-(.+)$ => $1`,
			current:     "release-1.2.3",
			tags:        []string{"release-1.2.4", "release-1.3.0", "nightly"},
			mode:        "patch",
			wantOutcome: PickFound, wantTag: "release-1.2.4",
		},
		{
			name:        "transform into build metadata",
			transform:   `^(\d+\.\d+\.\d+)-(\d+)$ => $1+$2`,
			current:     "2.1.0-15",
			tags:        []string{"2.1.0-16", "2.1.0-9", "2.1.1-1"},
			mode:        "patch",
			wantOutcome: PickFound, wantTag: "2.1.1-1",
		},
		{
			name:        "everything excluded reports no candidates",
			exclude:     `-alpine$`,
			current:     "1.2.3-alpine",
			tags:        []string{"1.2.3-alpine", "1.2.4-alpine"},
			mode:        "patch",
			wantOutcome: PickNoCandidates, wantExcluded: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewTagRules(tt.include, tt.exclude, tt.transform)
			if err != nil {
				t.Fatal(err)
			}
			cur, ok := r.ParseTag(tt.current)
			if !ok {
				t.Fatalf("ParseTag(%q) failed", tt.current)
			}
			got := r.Pick(cur, tt.tags, tt.mode)
			if got.Outcome != tt.wantOutcome || got.Tag != tt.wantTag || got.Excluded != tt.wantExcluded {
				t.Fatalf("Pick = %+v, want outcome %d tag %q excluded %d", got, tt.wantOutcome, tt.wantTag, tt.wantExcluded)
			}
			if tt.wantExcluded > 0 && got.Outcome != PickFound && !strings.Contains(got.Reason(tt.mode), "tag rules excluded") {
				t.Errorf("Reason = %q, want it to mention the tag rules", got.Reason(tt.mode))
			}
		})
	}
}

func TestCompiledTagRulesMatchNewTagRules(t *testing.T) {
	compiled := CompiledTagRules(regexp.MustCompile(`^1\.`), regexp.MustCompile(`-rc`))
	parsed, err := NewTagRules(`^1\.`, `-rc`, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"1.2.3", "1.3.0-rc1", "2.0.0", "v1.2"} {
		if compiled.Allows(tag) != parsed.Allows(tag) {
			t.Errorf("Allows(%q): compiled %v, parsed %v", tag, compiled.Allows(tag), parsed.Allows(tag))
		}
	}
	if compiled.IsZero() || !CompiledTagRules(nil, nil).IsZero() {
		t.Fatal("IsZero must report whether a rule is set")
	}
	cur, _ := compiled.ParseTag("1.2.3")
	p := compiled.Pick(cur, []string{"1.2.3", "1.3.0-rc1", "1.4.0", "2.0.0"}, "major")
	if p.Outcome != PickFound || p.Tag != "1.4.0" || p.Excluded != 2 {
		t.Fatalf("Pick = %+v, want 1.4.0 with 2 tags excluded", p)
	}
}
