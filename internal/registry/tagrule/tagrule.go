package tagrule

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Rules struct {
	include   *regexp.Regexp
	exclude   *regexp.Regexp
	transform *regexp.Regexp
	template  string
}

const MaxLen = 1024

func Compile(include, exclude, transform string) (Rules, error) {
	var r Rules
	var err error
	if r.include, err = compile("include", include); err != nil {
		return Rules{}, err
	}
	if r.exclude, err = compile("exclude", exclude); err != nil {
		return Rules{}, err
	}
	if strings.TrimSpace(transform) == "" {
		return r, nil
	}
	pattern, template, ok := strings.Cut(transform, "=>")
	pattern, template = strings.TrimSpace(pattern), strings.TrimSpace(template)
	if !ok || pattern == "" || template == "" {
		return Rules{}, fmt.Errorf("tag transform %q: want \"<regex> => <template>\"", transform)
	}
	if r.transform, err = compile("transform", pattern); err != nil {
		return Rules{}, err
	}
	if err := checkTemplate(r.transform, template); err != nil {
		return Rules{}, fmt.Errorf("tag transform %q: %w", transform, err)
	}
	r.template = template
	return r, nil
}

func FromRegexps(include, exclude *regexp.Regexp) Rules {
	return Rules{include: include, exclude: exclude}
}

func compile(name, pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > MaxLen {
		return nil, fmt.Errorf("tag %s: pattern longer than %d characters", name, MaxLen)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("tag %s %q: %w", name, pattern, err)
	}
	return re, nil
}

func checkTemplate(re *regexp.Regexp, template string) error {
	known := map[string]bool{"0": true}
	for i, name := range re.SubexpNames() {
		if i == 0 {
			continue
		}
		known[strconv.Itoa(i)] = true
		if name != "" {
			known[name] = true
		}
	}
	for i := 0; i < len(template); i++ {
		if template[i] != '$' || i+1 == len(template) {
			continue
		}
		var name string
		switch template[i+1] {
		case '$':
			i++
			continue
		case '{':
			end := strings.IndexByte(template[i+2:], '}')
			if end < 0 {
				return errors.New("template has an unterminated ${")
			}
			name = template[i+2 : i+2+end]
			i += 2 + end
		default:
			j := i + 1
			for j < len(template) && isNameByte(template[j]) {
				j++
			}
			name = template[i+1 : j]
			i = j - 1
		}
		if name != "" && !known[name] {
			return fmt.Errorf("template refers to unknown group %q (write ${1}x when a group number is followed by a letter, digit or _)", name)
		}
	}
	return nil
}

func isNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func (r Rules) IsZero() bool {
	return r.include == nil && r.exclude == nil && r.transform == nil
}

func (r Rules) Allows(tag string) bool {
	if r.include != nil && !r.include.MatchString(tag) {
		return false
	}
	return r.exclude == nil || !r.exclude.MatchString(tag)
}

func (r Rules) Transform(tag string) string {
	if r.transform == nil {
		return tag
	}
	m := r.transform.FindStringSubmatchIndex(tag)
	if m == nil {
		return tag
	}
	return string(r.transform.ExpandString(nil, r.template, tag, m))
}
