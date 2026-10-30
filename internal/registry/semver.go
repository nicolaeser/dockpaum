package registry

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Tag struct {
	HasV       bool
	Components []int
	Suffix     string
	Raw        string
}

func ParseTag(tag string) (Tag, bool) {
	out := Tag{Raw: tag}
	s := tag
	if len(s) == 0 {
		return out, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		if len(s) >= 2 && isDigitByte(s[1]) {
			out.HasV = true
			s = s[1:]
		}
	}
	i := 0
	for i < len(s) && (isDigitByte(s[i]) || s[i] == '.') {
		i++
	}
	if i == 0 {
		return out, false
	}
	numPart := s[:i]
	out.Suffix = s[i:]

	parts := strings.Split(numPart, ".")
	out.Components = make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.Components = append(out.Components, n)
	}
	if len(out.Components) == 0 {
		return out, false
	}
	return out, true
}

func (a Tag) Less(b Tag) bool { return Compare(a, b) < 0 }

func Compare(a, b Tag) int {
	n := len(a.Components)
	if len(b.Components) < n {
		n = len(b.Components)
	}
	for i := 0; i < n; i++ {
		if a.Components[i] != b.Components[i] {
			if a.Components[i] < b.Components[i] {
				return -1
			}
			return 1
		}
	}
	if len(a.Components) != len(b.Components) {
		if len(a.Components) < len(b.Components) {
			return -1
		}
		return 1
	}
	as, bs := parseSuffix(a.Suffix), parseSuffix(b.Suffix)
	if c := comparePreRelease(as.pre, bs.pre); c != 0 {
		return c
	}
	return compareBuild(as, bs)
}

func comparePreRelease(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	return comparePre(a, b)
}

func compareBuild(a, b suffix) int {
	switch {
	case a.buildKind == "" && b.buildKind == "":
		return 0
	case a.buildKind == "":
		return -1
	case b.buildKind == "":
		return 1
	case a.build < b.build:
		return -1
	case a.build > b.build:
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	for a != "" || b != "" {
		ac, an, arest := nextChunk(a)
		bc, bn, brest := nextChunk(b)
		if ac == "" || bc == "" {
			switch ac {
			case bc:
				return 0
			case "":
				return -1
			default:
				return 1
			}
		}
		switch {
		case an && bn:
			ai, _ := strconv.Atoi(ac)
			bi, _ := strconv.Atoi(bc)
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case an != bn:
			if an {
				return -1
			}
			return 1
		default:
			if ac != bc {
				if ac < bc {
					return -1
				}
				return 1
			}
		}
		a, b = arest, brest
	}
	return 0
}

func nextChunk(s string) (chunk string, numeric bool, rest string) {
	if s == "" {
		return "", false, ""
	}
	if isDigitByte(s[0]) {
		i := 0
		for i < len(s) && isDigitByte(s[i]) {
			i++
		}
		return s[:i], true, s[i:]
	}
	i := 0
	for i < len(s) && !isDigitByte(s[i]) {
		i++
	}
	return s[:i], false, s[i:]
}

var preReleaseTailRE = regexp.MustCompile(`(?i)[-_.](rc|alpha|beta|pre|preview|nightly|snapshot|dev|m[0-9])[a-z0-9._-]*$`)

func splitSuffix(s string) (variant, pre string) {
	if s == "" {
		return "", ""
	}
	loc := preReleaseTailRE.FindStringIndex(s)
	if loc == nil {
		return s, ""
	}
	return s[:loc[0]], s[loc[0]:]
}

var buildCounterRE = regexp.MustCompile(`(?i)(-ls|-r|\+(?:build[.-]?)?)([0-9]+)$`)

type suffix struct {
	variant   string
	pre       string
	buildKind string
	build     int
}

func parseSuffix(s string) suffix {
	var out suffix
	if m := buildCounterRE.FindStringSubmatchIndex(s); m != nil {
		if n, err := strconv.Atoi(s[m[4]:m[5]]); err == nil {
			out.buildKind = strings.ToLower(s[m[2]:m[3]])
			out.build = n
			s = s[:m[0]]
		}
	}
	out.variant, out.pre = splitSuffix(s)
	return out
}

func sameForm(cur Tag, cs suffix, cand Tag, ks suffix) bool {
	return cand.HasV == cur.HasV &&
		len(cand.Components) == len(cur.Components) &&
		ks.variant == cs.variant &&
		ks.buildKind == cs.buildKind
}

func form(t Tag, s suffix) string {
	const names = "XYZW"
	var b strings.Builder
	if t.HasV {
		b.WriteByte('v')
	}
	for i := range t.Components {
		if i > 0 {
			b.WriteByte('.')
		}
		if i < len(names) {
			b.WriteByte(names[i])
		} else {
			b.WriteByte('N')
		}
	}
	b.WriteString(s.variant)
	if s.buildKind != "" {
		b.WriteString(s.buildKind)
		b.WriteByte('N')
	}
	return b.String()
}

func dateLike(t Tag) bool { return t.Components[0] >= 1900 }

type PickOutcome int

const (
	PickNone PickOutcome = iota
	PickFound
	PickNoCandidates
)

type Pick struct {
	Outcome  PickOutcome
	Tag      string
	Current  string
	Form     string
	Excluded int
}

func (p Pick) Reason(mode string) string {
	var msg string
	switch p.Outcome {
	case PickFound:
		return ""
	case PickNoCandidates:
		msg = fmt.Sprintf("no candidates for variant %s: no other tag has the form of %q", p.Form, p.Current)
	default:
		msg = fmt.Sprintf("no newer %s-eligible tag found", mode)
	}
	if p.Excluded > 0 {
		msg += fmt.Sprintf(" (tag rules excluded %d tag(s))", p.Excluded)
	}
	return msg
}

func PickHighest(current Tag, tags []string, mode string) string {
	return TagRules{}.Pick(current, tags, mode).Tag
}

func (r TagRules) Pick(current Tag, tags []string, mode string) Pick {
	cs := parseSuffix(current.Suffix)
	out := Pick{Current: current.Raw, Form: form(current, cs)}
	if !modeViable(mode, len(current.Components)) {
		return out
	}

	var best Tag
	have := false
	siblings := 0
	for _, raw := range tags {
		if !r.Allows(raw) {
			out.Excluded++
			continue
		}
		cand, ok := r.ParseTag(raw)
		if !ok {
			continue
		}
		ks := parseSuffix(cand.Suffix)
		if !sameForm(current, cs, cand, ks) {
			continue
		}
		if raw != current.Raw {
			siblings++
		}
		if cs.pre == "" && ks.pre != "" {
			continue
		}
		if dateLike(cand) && !dateLike(current) {
			continue
		}
		if Compare(cand, current) <= 0 {
			continue
		}
		if !modeAllows(mode, current, cand) {
			continue
		}
		if !have || Compare(best, cand) < 0 {
			best = cand
			have = true
		}
	}
	switch {
	case have:
		out.Outcome = PickFound
		out.Tag = best.Raw
	case siblings == 0:
		out.Outcome = PickNoCandidates
	}
	return out
}

func modeViable(mode string, n int) bool {
	switch mode {
	case "patch":
		return n >= 3
	case "minor":
		return n >= 2
	case "major":
		return n >= 1
	}
	return false
}

func MinComponents(mode string) int {
	switch mode {
	case "patch":
		return 3
	case "minor":
		return 2
	}
	return 1
}

func modeAllows(mode string, cur, cand Tag) bool {
	switch mode {
	case "patch":
		return cand.Components[0] == cur.Components[0] &&
			cand.Components[1] == cur.Components[1]
	case "minor":
		return cand.Components[0] == cur.Components[0]
	case "major":
		return true
	}
	return false
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }
