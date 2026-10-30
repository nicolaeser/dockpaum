package config

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

func expandEnvPreservingRuntime(s string) string {
	out, _ := expandEnv(s, os.LookupEnv)
	return out
}

type requiredVarError struct {
	line int
	name string
	msg  string
}

func (e *requiredVarError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("${%s} is not set: %s", e.name, e.msg)
	}
	return fmt.Sprintf("${%s} is not set and is required here", e.name)
}

func expandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	var first error
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch != '$' {
			b.WriteByte(ch)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		if s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if s[i+1] == '{' {
			end := closingBrace(s, i+2)
			ref, ok := parseEnvRef(s[i+2 : max(end, i+2)])
			if end < 0 || !ok {
				b.WriteByte('$')
				continue
			}
			if strings.HasPrefix(ref.name, "DOCKPAUM_") {
				b.WriteString(s[i : end+1])
				i = end
				continue
			}
			val, err := ref.expand(lookup)
			if err != nil && first == nil && !inComment(s, i) {
				err.line = 1 + strings.Count(s[:i], "\n")
				first = err
			}
			b.WriteString(val)
			i = end
			continue
		}
		name := bareEnvName(s[i+1:])
		if name == "" {
			b.WriteByte('$')
			continue
		}
		if strings.HasPrefix(name, "DOCKPAUM_") {
			b.WriteString("${" + name + "}")
		} else {
			v, _ := lookup(name)
			b.WriteString(v)
		}
		i += len(name)
	}
	if first != nil {
		return b.String(), first
	}
	return b.String(), nil
}

type envRef struct {
	name string
	op   string
	arg  string
}

func parseEnvRef(body string) (envRef, bool) {
	name := bareEnvName(body)
	if name == "" {
		return envRef{}, false
	}
	rest := body[len(name):]
	if rest == "" {
		return envRef{name: name}, true
	}
	for _, op := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, op) {
			return envRef{name: name, op: op, arg: rest[len(op):]}, true
		}
	}
	return envRef{}, false
}

func (r envRef) expand(lookup func(string) (string, bool)) (string, *requiredVarError) {
	v, set := lookup(r.name)
	empty := !set || v == ""
	switch r.op {
	case ":-", "-":
		if (r.op == ":-" && empty) || (r.op == "-" && !set) {
			out, err := expandEnv(r.arg, lookup)
			if rerr, ok := err.(*requiredVarError); ok {
				return out, rerr
			}
			return out, nil
		}
	case ":?", "?":
		if (r.op == ":?" && empty) || (r.op == "?" && !set) {
			return "", &requiredVarError{name: r.name, msg: strings.TrimSpace(r.arg)}
		}
	case ":+", "+":
		if (r.op == ":+" && !empty) || (r.op == "+" && set) {
			out, err := expandEnv(r.arg, lookup)
			if rerr, ok := err.(*requiredVarError); ok {
				return out, rerr
			}
			return out, nil
		}
		return "", nil
	}
	return v, nil
}

func closingBrace(s string, start int) int {
	depth := 1
	for j := start; j < len(s); j++ {
		switch {
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case s[j] == '}':
			depth--
			if depth == 0 {
				return j
			}
		case s[j] == '\n':
			return -1
		}
	}
	return -1
}

func bareEnvName(s string) string {
	i := 0
	for i < len(s) && (s[i] == '_' ||
		(s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') ||
		(i > 0 && s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	return s[:i]
}

func inComment(s string, pos int) bool {
	start := strings.LastIndexByte(s[:pos], '\n') + 1
	var quote byte
	for j := start; j < pos; j++ {
		c := s[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (j == start || s[j-1] == ' ' || s[j-1] == '\t'):
			return true
		}
	}
	return false
}

type envUse struct {
	name    string
	path    string
	line    int
	unset   bool
	command bool
}

var commandPathRE = regexp.MustCompile(`(^|\.)hooks\.(pre_run|post_run|pre_update|post_update|on_failure)(\[\d+\])?$|^jobs\[\d+\]\.commands(\[\d+\])?$`)

func findEnvUses(raw []byte) []envUse {
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil || len(root.Content) == 0 {
		return nil
	}
	var out []envUse
	var visit func(n *yaml.Node, path string)
	visit = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				p := joinPath(path, key.Value)
				if key.Tag == "!!merge" {
					p = path
				}
				visit(n.Content[i+1], p)
			}
		case yaml.SequenceNode:
			for i, item := range n.Content {
				visit(item, fmt.Sprintf("%s[%d]", path, i))
			}
		case yaml.ScalarNode:
			command := commandPathRE.MatchString(path)
			for _, r := range scanEnvRefs(n.Value) {
				v, _ := os.LookupEnv(r.name)
				out = append(out, envUse{name: r.name, path: path, line: n.Line, unset: r.op == "" && v == "", command: command})
			}
		}
	}
	visit(root.Content[0], "")
	return out
}

func scanEnvRefs(s string) []envRef {
	var out []envRef
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '$' {
			continue
		}
		if s[i+1] == '$' {
			i++
			continue
		}
		if s[i+1] == '{' {
			end := closingBrace(s, i+2)
			if end < 0 {
				continue
			}
			if ref, ok := parseEnvRef(s[i+2 : end]); ok {
				out = append(out, ref)
			}
			i = end
			continue
		}
		if name := bareEnvName(s[i+1:]); name != "" {
			out = append(out, envRef{name: name})
			i += len(name)
		}
	}
	return out
}

func envWarnings(uses []envUse) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range uses {
		key := u.path + "\x00" + u.name
		if seen[key] {
			continue
		}
		seen[key] = true
		where := fmt.Sprintf("line %d: %s", u.line, u.path)
		dockpaum := strings.HasPrefix(u.name, "DOCKPAUM_")
		switch {
		case u.command && !dockpaum:
			msg := fmt.Sprintf("%s: $%s is replaced when the configuration is loaded, not when the command runs — write $$%s to leave it to the shell", where, u.name, u.name)
			if u.unset {
				msg += " (it is not set, so the command gets an empty value)"
			}
			out = append(out, msg)
		case dockpaum && !u.command:
			msg := fmt.Sprintf("%s: ${%s} stays literal here — ${DOCKPAUM_*} references are expanded only in hooks and jobs", where, u.name)
			if slices.Contains(knownDockpaumEnv, u.name) {
				msg += fmt.Sprintf("; %s is read from the environment by itself", u.name)
			}
			out = append(out, msg)
		case u.unset && !dockpaum:
			out = append(out, fmt.Sprintf("%s: ${%s} is not set, so the value is empty — set it, or write ${%s:-} if an empty value is intended", where, u.name, u.name))
		}
	}
	return out
}
