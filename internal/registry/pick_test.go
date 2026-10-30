package registry

import "testing"

func TestPickOutcome(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		tags        []string
		mode        string
		wantOutcome PickOutcome
		wantTag     string
		wantForm    string
		wantReason  string
	}{
		{
			name:        "found",
			current:     "1.2.3",
			tags:        []string{"1.2.3", "1.2.4"},
			mode:        "patch",
			wantOutcome: PickFound,
			wantTag:     "1.2.4",
			wantForm:    "X.Y.Z",
		},
		{
			name:        "up to date with siblings",
			current:     "1.2.3",
			tags:        []string{"1.2.2", "1.2.3", "1.3.0"},
			mode:        "patch",
			wantOutcome: PickNone,
			wantForm:    "X.Y.Z",
			wantReason:  "no newer patch-eligible tag found",
		},
		{
			name:        "variant without candidates",
			current:     "1.2.3-alpine",
			tags:        []string{"1.2.3-alpine", "1.2.4", "1.2.4-bookworm"},
			mode:        "patch",
			wantOutcome: PickNoCandidates,
			wantForm:    "X.Y.Z-alpine",
			wantReason:  `no candidates for variant X.Y.Z-alpine: no other tag has the form of "1.2.3-alpine"`,
		},
		{
			name:        "current tag no longer listed",
			current:     "16.4.0-debian-12-r12",
			tags:        []string{"16.4.1", "latest"},
			mode:        "minor",
			wantOutcome: PickNoCandidates,
			wantForm:    "X.Y.Z-debian-12-rN",
			wantReason:  `no candidates for variant X.Y.Z-debian-12-rN: no other tag has the form of "16.4.0-debian-12-r12"`,
		},
		{
			name:        "build counter siblings are candidates",
			current:     "4.0.15.2941-ls290",
			tags:        []string{"4.0.15.2941-ls290", "4.0.14.2939-ls280"},
			mode:        "patch",
			wantOutcome: PickNone,
			wantForm:    "X.Y.Z.W-lsN",
			wantReason:  "no newer patch-eligible tag found",
		},
		{
			name:        "prerelease siblings count as candidates",
			current:     "1.2.3",
			tags:        []string{"1.2.3", "1.2.4-rc1"},
			mode:        "patch",
			wantOutcome: PickNone,
			wantForm:    "X.Y.Z",
		},
		{
			name:        "date snapshots count as candidates",
			current:     "3",
			tags:        []string{"3", "20260805"},
			mode:        "major",
			wantOutcome: PickNone,
			wantForm:    "X",
		},
		{
			name:        "v style is part of the form",
			current:     "v1.2.3",
			tags:        []string{"v1.2.3", "1.2.4"},
			mode:        "patch",
			wantOutcome: PickNoCandidates,
			wantForm:    "vX.Y.Z",
		},
		{
			name:        "five components",
			current:     "1.2.3.4.5",
			tags:        []string{"1.2.3.4.6"},
			mode:        "patch",
			wantOutcome: PickFound,
			wantTag:     "1.2.3.4.6",
			wantForm:    "X.Y.Z.W.N",
		},
		{
			name:        "mode not viable",
			current:     "1.2",
			tags:        []string{"1.3"},
			mode:        "patch",
			wantOutcome: PickNone,
			wantForm:    "X.Y",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TagRules{}.Pick(mustParseTag(t, tt.current), tt.tags, tt.mode)
			if got.Outcome != tt.wantOutcome || got.Tag != tt.wantTag {
				t.Fatalf("Pick = %+v, want outcome %d tag %q", got, tt.wantOutcome, tt.wantTag)
			}
			if got.Current != tt.current {
				t.Errorf("Current = %q, want %q", got.Current, tt.current)
			}
			if got.Form != tt.wantForm {
				t.Errorf("Form = %q, want %q", got.Form, tt.wantForm)
			}
			if tt.wantReason != "" && got.Reason(tt.mode) != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason(tt.mode), tt.wantReason)
			}
			if got.Outcome == PickFound && got.Reason(tt.mode) != "" {
				t.Errorf("Reason for a found tag = %q, want empty", got.Reason(tt.mode))
			}
		})
	}
}

func TestPickHighestMatchesPick(t *testing.T) {
	cur := mustParseTag(t, "1.2.3-alpine")
	tags := []string{"1.2.4-alpine", "1.2.5"}
	got := PickHighest(cur, tags, "patch")
	want := TagRules{}.Pick(cur, tags, "patch").Tag
	if got != want || got != "1.2.4-alpine" {
		t.Fatalf("PickHighest = %q, Pick = %q, want 1.2.4-alpine", got, want)
	}
}

func TestParseSuffix(t *testing.T) {
	tests := []struct {
		suffix string
		want   suffix
	}{
		{"", suffix{}},
		{"-alpine", suffix{variant: "-alpine"}},
		{"-rc1", suffix{pre: "-rc1"}},
		{"-ls290", suffix{buildKind: "-ls", build: 290}},
		{"-debian-12-r12", suffix{variant: "-debian-12", buildKind: "-r", build: 12}},
		{"-rc1-r2", suffix{pre: "-rc1", buildKind: "-r", build: 2}},
		{"+5", suffix{buildKind: "+", build: 5}},
		{"+build.7", suffix{buildKind: "+build.", build: 7}},
		{"-java17-ls12", suffix{variant: "-java17", buildKind: "-ls", build: 12}},
		{"-LS3", suffix{buildKind: "-ls", build: 3}},
		{"-jre17", suffix{variant: "-jre17"}},
		{"-r2d2", suffix{variant: "-r2d2"}},
		{"-ls", suffix{variant: "-ls"}},
		{"-12", suffix{variant: "-12"}},
	}
	for _, tt := range tests {
		t.Run(tt.suffix, func(t *testing.T) {
			if got := parseSuffix(tt.suffix); got != tt.want {
				t.Errorf("parseSuffix(%q) = %+v, want %+v", tt.suffix, got, tt.want)
			}
		})
	}
}
