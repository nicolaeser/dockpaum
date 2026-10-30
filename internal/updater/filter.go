package updater

import (
	"regexp"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/policy"
)

const LabelEnable = policy.LabelEnable

var tempNameRE = regexp.MustCompile(`\.(dockpaum-old|self-update-old)-\d+$`)

const tempContainerReason = "dockpaum-temp-container"

func keep(c docker.Container, f config.FilterConfig) (bool, string) {
	if tempNameRE.MatchString(c.Name) {
		return false, tempContainerReason
	}
	cl := policy.ReadLabels(c.Labels, f.HonorWatchtowerLabels())
	if f.LabelEnable {
		if cl.Enable == nil || !*cl.Enable {
			return false, "label-disabled"
		}
	} else if cl.Enable != nil && !*cl.Enable {
		return false, "label-disabled"
	}
	if f.Scope != "" && cl.Scope != f.Scope {
		return false, "scope-mismatch"
	}
	if f.Scope == "" && cl.Scope != "" {
		return false, "scoped-elsewhere"
	}
	if len(f.IncludeNames) > 0 {
		if !contains(f.IncludeNames, c.Name) {
			return false, "not-in-include-list"
		}
	}
	if contains(f.ExcludeNames, c.Name) {
		return false, "in-exclude-list"
	}
	if re := f.IncludeRegexp(); re != nil && !re.MatchString(c.Name) {
		return false, "include-regex-no-match"
	}
	if re := f.ExcludeRegexp(); re != nil && re.MatchString(c.Name) {
		return false, "exclude-regex-match"
	}
	return true, ""
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
