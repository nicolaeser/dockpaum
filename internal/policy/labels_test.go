package policy

import "testing"

func TestReadLabels(t *testing.T) {
	wt := func(k string) string { return WatchtowerPrefix + k }
	tests := []struct {
		name       string
		labels     map[string]string
		watchtower bool
		check      func(Labels) bool
	}{
		{"nothing set", nil, true, func(l Labels) bool { return l.Enable == nil && !l.MonitorOnly && !l.NoPull && l.Scope == "" }},
		{"dockpaum enable=false", map[string]string{LabelEnable: "false"}, true, func(l Labels) bool { return l.Enable != nil && !*l.Enable }},
		{"watchtower enable=false honored", map[string]string{wt("enable"): "false"}, true, func(l Labels) bool { return l.Enable != nil && !*l.Enable }},
		{"watchtower ignored when off", map[string]string{wt("enable"): "false"}, false, func(l Labels) bool { return l.Enable == nil }},
		{"dockpaum label wins", map[string]string{LabelEnable: "true", wt("enable"): "false"}, true, func(l Labels) bool { return l.Enable != nil && *l.Enable }},
		{"monitor-only", map[string]string{LabelMonitorOnly: "true"}, true, func(l Labels) bool { return l.MonitorOnly }},
		{"watchtower monitor-only", map[string]string{wt("monitor-only"): "true"}, true, func(l Labels) bool { return l.MonitorOnly }},
		{"no-pull and scope", map[string]string{LabelNoPull: "yes", wt("scope"): "prod"}, true, func(l Labels) bool { return l.NoPull && l.Scope == "prod" }},
		{"hooks", map[string]string{LabelHookPre: "pg_dump", wt("lifecycle.post-update"): "warmup"}, true, func(l Labels) bool {
			return l.PreUpdate == "pg_dump" && l.PostUpdate == "warmup"
		}},
		{"link template", map[string]string{LabelLinkTemplate: " https://example.com/releases/${tag} "}, true, func(l Labels) bool {
			return l.LinkTemplate == "https://example.com/releases/${tag}"
		}},
		{"link template has no Watchtower label", map[string]string{wt("link.template"): "https://x"}, true, func(l Labels) bool { return l.LinkTemplate == "" }},
	}
	for _, tt := range tests {
		if got := ReadLabels(tt.labels, tt.watchtower); !tt.check(got) {
			t.Errorf("%s: ReadLabels = %+v", tt.name, got)
		}
	}
}

func TestResolveHonorsWatchtowerEnable(t *testing.T) {
	r := testResolver(t, nil)
	res := r.Resolve(testCt("x", map[string]string{WatchtowerPrefix + "enable": "false"}), nil)
	if !res.Skip {
		t.Fatal("com.centurylinklabs.watchtower.enable=false must skip the container by default")
	}
	res = r.Resolve(testCt("y", map[string]string{LabelMonitorOnly: "true", LabelPolicy: "default"}), nil)
	if res.Skip || !res.Labels.MonitorOnly {
		t.Fatalf("monitor-only container = %+v", res)
	}
}

func TestLabelValuesParseLikeBooleans(t *testing.T) {
	for v, want := range map[string]bool{"false": false, "0": false, "no": false, "FALSE": false, "true": true, "yes": true, "garbage": true} {
		l := ReadLabels(map[string]string{LabelEnable: v}, true)
		if l.Enable == nil || *l.Enable != want {
			t.Errorf("enable=%q → %v, want %v", v, l.Enable, want)
		}
	}
	if !ReadLabels(map[string]string{LabelMonitorOnly: "1"}, true).MonitorOnly || ReadLabels(map[string]string{LabelNoPull: "maybe"}, true).NoPull {
		t.Error("monitor-only=1 must be on, no-pull=maybe off")
	}
}
