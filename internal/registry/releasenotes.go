package registry

import (
	"net/url"
	"regexp"
	"strings"
)

const (
	labelOCISource  = "org.opencontainers.image.source"
	labelOCIURL     = "org.opencontainers.image.url"
	labelOCIVersion = "org.opencontainers.image.version"
)

func ReleaseNotesURL(labels map[string]string, tag, linkTemplate string) string {
	tag = strings.TrimSpace(tag)
	version, parsed, haveVersion := releaseVersion(tag, strings.TrimSpace(labels[labelOCIVersion]))
	if tmpl := strings.TrimSpace(linkTemplate); tmpl != "" {
		return expandLinkTemplate(tmpl, tag, version, parsed, haveVersion)
	}
	for _, key := range []string{labelOCISource, labelOCIURL} {
		f, ok := parseForge(labels[key])
		if !ok {
			continue
		}
		if !haveVersion {
			return f.releasesURL()
		}
		return f.releaseURL(gitTag(version, parsed))
	}
	return ""
}

func releaseVersion(tag, label string) (string, Tag, bool) {
	tt, tagOK := ParseTag(tag)
	lt, labelOK := ParseTag(label)
	switch {
	case tagOK && labelOK && refines(lt, tt):
		return label, lt, true
	case tagOK:
		return tag, tt, true
	case labelOK:
		return label, lt, true
	}
	return "", Tag{}, false
}

func refines(v, t Tag) bool {
	if len(v.Components) <= len(t.Components) {
		return false
	}
	for i, c := range t.Components {
		if v.Components[i] != c {
			return false
		}
	}
	return true
}

func numericPart(raw string, t Tag) string {
	return raw[:len(raw)-len(t.Suffix)]
}

func gitTag(raw string, t Tag) string {
	s := parseSuffix(t.Suffix)
	if s.buildKind == "-ls" {
		return raw
	}
	core := numericPart(raw, t) + s.pre
	if t.HasV || dateLike(t) {
		return core
	}
	return "v" + core
}

var linkPlaceholderRE = regexp.MustCompile(`\$\{([A-Za-z]+)\}`)

func expandLinkTemplate(tmpl, tag, version string, parsed Tag, haveVersion bool) string {
	vars := map[string]string{"tag": tag}
	if haveVersion {
		vars["version"] = version
		digits := strings.TrimLeft(numericPart(version, parsed), "vV")
		for i, part := range strings.Split(digits, ".") {
			switch i {
			case 0:
				vars["major"] = part
			case 1:
				vars["minor"] = part
			case 2:
				vars["patch"] = part
			}
		}
	}
	complete := true
	out := linkPlaceholderRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		v := vars[strings.ToLower(m[2:len(m)-1])]
		if v == "" {
			complete = false
			return ""
		}
		return url.PathEscape(v)
	})
	if !complete {
		return ""
	}
	return safeLink(out)
}

func safeLink(s string) string {
	if strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return s
}

type forge struct {
	base   string
	gitlab bool
}

func (f forge) releaseURL(tag string) string {
	if f.gitlab {
		return f.base + "/-/releases/" + url.PathEscape(tag)
	}
	return f.base + "/releases/tag/" + url.PathEscape(tag)
}

func (f forge) releasesURL() string {
	if f.gitlab {
		return f.base + "/-/releases"
	}
	return f.base + "/releases"
}

var forgeSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

var gitlabPageSegments = map[string]bool{
	"activity": true, "blame": true, "blob": true, "branches": true,
	"commit": true, "commits": true, "compare": true, "container_registry": true,
	"edit": true, "environments": true, "files": true, "issues": true,
	"jobs": true, "merge_requests": true, "packages": true, "pipelines": true,
	"raw": true, "refs": true, "releases": true, "tags": true,
	"tree": true, "wikis": true,
}

func parseForge(raw string) (forge, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return forge{}, false
	}
	raw = strings.TrimPrefix(raw, "git+")
	host, path := "", ""
	if !strings.Contains(raw, "://") {
		colon, slash := strings.IndexByte(raw, ':'), strings.IndexByte(raw, '/')
		if colon < 0 || (slash >= 0 && slash < colon) {
			raw = "https://" + raw
		}
	}
	if !strings.Contains(raw, "://") {
		userHost, p, _ := strings.Cut(raw, ":")
		if i := strings.LastIndexByte(userHost, '@'); i >= 0 {
			userHost = userHost[i+1:]
		}
		host, path = userHost, p
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return forge{}, false
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return forge{}, false
		}
		host, path = u.Hostname(), u.Path
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if !forgeSegmentRE.MatchString(s) {
			return forge{}, false
		}
	}

	f := forge{}
	switch host {
	case "github.com":
		if len(segs) != 2 {
			return forge{}, false
		}
	case "gitlab.com":
		if len(segs) < 2 {
			return forge{}, false
		}
		for _, s := range segs[2:] {
			if gitlabPageSegments[strings.ToLower(s)] {
				return forge{}, false
			}
		}
		f.gitlab = true
	default:
		return forge{}, false
	}
	f.base = "https://" + host + "/" + strings.Join(segs, "/")
	return f, true
}
