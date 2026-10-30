package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Problem struct {
	Path    string
	Line    int
	Message string

	err error
}

func (p Problem) String() string {
	if p.Line > 0 {
		return fmt.Sprintf("line %d: %s", p.Line, p.Message)
	}
	return p.Message
}

type Error struct {
	Source   string
	Problems []Problem
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		p := e.Problems[0]
		if p.Line > 0 {
			return fmt.Sprintf("%s:%d: %s", e.Source, p.Line, p.Message)
		}
		return fmt.Sprintf("%s: %s", e.Source, p.Message)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d problems", e.Source, len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p.String())
	}
	return b.String()
}

func (e *Error) Unwrap() []error {
	var out []error
	for _, p := range e.Problems {
		if p.err != nil {
			out = append(out, p.err)
		}
	}
	return out
}

type lineSpan struct{ start, end int }

type lineIndex map[string]lineSpan

func (x lineIndex) lookup(path string) (lineSpan, bool) {
	for path != "" {
		if s, ok := x[path]; ok {
			return s, true
		}
		path = parentPath(path)
	}
	return lineSpan{}, false
}

func parentPath(p string) string {
	if strings.HasSuffix(p, "]") {
		if i := strings.LastIndexByte(p, '['); i > 0 {
			return p[:i]
		}
	}
	if i := strings.LastIndexByte(p, '.'); i > 0 {
		return p[:i]
	}
	return ""
}

const decoration = ` \((?:"(?:[^"\\]|\\.)*"|[^()"])*\)`

var (
	messagePathRE = regexp.MustCompile(`^[a-z_]+(?:\[\d+\])?(?:` + decoration + `)?(?:\.[A-Za-z0-9_-]+(?:\[\d+\])?(?:` + decoration + `)?)*`)
	decorationRE  = regexp.MustCompile(decoration)
)

func messagePath(msg string) string {
	m := messagePathRE.FindString(msg)
	if m == "" || (len(m) < len(msg) && msg[len(m)] != ':' && msg[len(m)] != ' ') {
		return ""
	}
	path := decorationRE.ReplaceAllString(m, "")
	top, _, _ := strings.Cut(path, ".")
	top, _, _ = strings.Cut(top, "[")
	if _, ok := topLevelKeys()[top]; !ok {
		return ""
	}
	return path
}

type document struct {
	source    string
	text      string
	rewritten bool
	lines     lineIndex
	uses      []envUse
	warnings  []string
}

func parseDocument(source string, raw []byte, text string) (*document, error) {
	d := &document{source: source, lines: lineIndex{}, text: text}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, d.yamlError(err)
	}
	if len(root.Content) == 0 {
		return nil, d.failf(0, "no settings (the document is empty or only comments); write {} to use the built-in defaults")
	}
	d.uses = findEnvUses(raw)
	k := &checker{spans: d.lines, reported: map[*yaml.Node]bool{}}
	k.walk(&root.Content[0], configType, "", "")
	if len(k.problems) > 0 {
		return nil, d.fail(k.problems...)
	}
	if k.changed {
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode(&root); err != nil {
			return nil, d.failf(0, "rewrite shorthands: %v", err)
		}
		if err := enc.Close(); err != nil {
			return nil, d.failf(0, "rewrite shorthands: %v", err)
		}
		d.text, d.rewritten = b.String(), true
	}
	d.warnings = envWarnings(d.uses)
	return d, nil
}

func (d *document) decode(c *Config) error {
	dec := yaml.NewDecoder(strings.NewReader(d.text))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return d.yamlError(err)
	}
	if err := c.markExplicitGates(d.text); err != nil {
		return d.yamlError(err)
	}
	return nil
}

func (d *document) fail(problems ...Problem) error {
	for i := range problems {
		problems[i].Message += d.unsetNote(problems[i])
	}
	return &Error{Source: d.source, Problems: problems}
}

func (d *document) failf(line int, format string, args ...any) error {
	return d.fail(Problem{Line: line, Message: fmt.Sprintf(format, args...)})
}

func (d *document) failErr(err error) error {
	p := Problem{Message: err.Error(), err: err}
	if path := messagePath(p.Message); path != "" {
		p.Path = path
		if s, ok := d.lines.lookup(path); ok {
			p.Line = s.start
		}
	}
	return d.fail(p)
}

func (d *document) unsetNote(p Problem) string {
	if p.Line == 0 {
		return ""
	}
	span := lineSpan{p.Line, p.Line}
	if p.Path != "" {
		if s, ok := d.lines.lookup(p.Path); ok {
			span = s
		}
	}
	var names []string
	for _, u := range d.uses {
		if u.unset && u.line >= span.start && u.line <= span.end && !slices.Contains(names, u.name) {
			names = append(names, u.name)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" (${%s} is not set)", names[0])
	}
	return fmt.Sprintf(" (${%s} are not set)", strings.Join(names, "}, ${"))
}

var (
	yamlLineRE     = regexp.MustCompile(`^line (\d+): (.*)$`)
	yamlUnknownRE  = regexp.MustCompile(`^field (\S+) not found in type \S+$`)
	yamlUnmarshalR = regexp.MustCompile("^cannot unmarshal !!(\\w+) (?:`([^`]*)` )?into (\\S+)$")
)

func (d *document) yamlError(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		problems := make([]Problem, 0, len(te.Errors))
		for _, e := range te.Errors {
			p := Problem{Message: e}
			if m := yamlLineRE.FindStringSubmatch(e); m != nil {
				if !d.rewritten {
					p.Line, _ = strconv.Atoi(m[1])
				}
				p.Message = friendlyYAMLMessage(m[2])
			}
			problems = append(problems, p)
		}
		return d.fail(problems...)
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	if m := yamlLineRE.FindStringSubmatch(msg); m != nil && !d.rewritten {
		line, _ := strconv.Atoi(m[1])
		return d.failf(line, "invalid YAML: %s", m[2])
	}
	return d.failf(0, "invalid YAML: %s", msg)
}

func friendlyYAMLMessage(msg string) string {
	if m := yamlUnknownRE.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("unknown key %q", m[1])
	}
	if m := yamlUnmarshalR.FindStringSubmatch(msg); m != nil {
		got := yamlKinds[m[1]]
		if got == "" {
			got = m[1]
		}
		if m[2] != "" {
			got += " " + quoteValue(m[2])
		}
		return fmt.Sprintf("expected %s, got %s", goTypeWanted(m[3]), got)
	}
	return msg
}

var yamlKinds = map[string]string{
	"str": "text", "int": "a number", "float": "a number", "bool": "a boolean",
	"seq": "a list", "map": "a mapping", "null": "nothing", "timestamp": "a date",
}

func goTypeWanted(t string) string {
	switch {
	case strings.HasPrefix(t, "[]"):
		return "a list"
	case strings.HasPrefix(t, "map["):
		return "a mapping (name: value)"
	case t == "string":
		return "a text value"
	case t == "bool":
		return "true or false"
	case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint"):
		return "a whole number"
	case strings.HasSuffix(t, ".Duration"):
		return "a duration"
	}
	return "a mapping (key: value)"
}

var (
	configType   = reflect.TypeOf(Config{})
	durationType = reflect.TypeOf(Duration(0))
	scheduleType = reflect.TypeOf(ScheduleConfig{})
	policyType   = reflect.TypeOf(Policy{})
	hostType     = reflect.TypeOf(HostConfig{})
	jobType      = reflect.TypeOf(JobConfig{})
)

type modelField struct {
	key string
	typ reflect.Type
}

func modelFields(t reflect.Type) []modelField {
	var out []modelField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if strings.Contains(opts, "inline") {
			out = append(out, modelFields(f.Type)...)
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out = append(out, modelField{key: name, typ: f.Type})
	}
	return out
}

func fieldByKey(fields []modelField, key string) (modelField, bool) {
	for _, f := range fields {
		if f.key == key {
			return f, true
		}
	}
	return modelField{}, false
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isScalarType(t reflect.Type) bool {
	t = deref(t)
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

var (
	keyIndexOnce sync.Once
	keyIndex     map[string][]string
	topKeys      map[string]struct{}
)

func keyLocations(key string) []string {
	buildKeyIndex()
	return keyIndex[key]
}

func topLevelKeys() map[string]struct{} {
	buildKeyIndex()
	return topKeys
}

func buildKeyIndex() {
	keyIndexOnce.Do(func() {
		keyIndex = map[string][]string{}
		topKeys = map[string]struct{}{}
		for _, f := range modelFields(configType) {
			topKeys[f.key] = struct{}{}
		}
		var visit func(t reflect.Type, prefix string)
		visit = func(t reflect.Type, prefix string) {
			t = deref(t)
			switch t.Kind() {
			case reflect.Slice:
				visit(t.Elem(), prefix+"[]")
			case reflect.Struct:
				if t == durationType {
					return
				}
				for _, f := range modelFields(t) {
					p := joinPath(prefix, f.key)
					keyIndex[f.key] = append(keyIndex[f.key], p)
					visit(f.typ, p)
				}
			}
		}
		visit(configType, "")
	})
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

type checker struct {
	problems []Problem
	spans    lineIndex
	changed  bool
	reported map[*yaml.Node]bool
}

func (k *checker) add(n *yaml.Node, path, format string, args ...any) {
	if k.reported[n] {
		return
	}
	k.reported[n] = true
	k.problems = append(k.problems, Problem{Path: path, Line: n.Line, Message: fmt.Sprintf(format, args...)})
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

func (k *checker) walk(slot **yaml.Node, t reflect.Type, path, label string) int {
	end := (*slot).Line
	n := resolve(*slot)
	if isNull(n) {
		return end
	}
	t = deref(t)
	switch {
	case t == durationType:
		k.checkScalar(n, t, path, label)
	case t == scheduleType && n.Kind == yaml.ScalarNode:
		if k.scheduleShorthand(slot, n, path, label) {
			return k.walk(slot, t, path, label)
		}
	case t.Kind() == reflect.Struct:
		end = max(end, k.walkStruct(n, t, path, label))
	case t.Kind() == reflect.Slice:
		end = max(end, k.walkList(slot, t, path, label))
	case t.Kind() == reflect.Map:
		end = max(end, k.walkMap(n, t, path, label))
	default:
		k.checkScalar(n, t, path, label)
	}
	if (*slot).Kind == yaml.AliasNode {
		return (*slot).Line
	}
	return end
}

func entries(n *yaml.Node, fn func(key *yaml.Node, val **yaml.Node, merged bool)) {
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i]; key.Kind == yaml.ScalarNode && key.Value == "<<" && key.Tag == "!!merge" {
			merges = append(merges, n.Content[i+1])
			continue
		}
		fn(n.Content[i], &n.Content[i+1], false)
	}
	for _, m := range merges {
		m = resolve(m)
		maps := []*yaml.Node{m}
		if m.Kind == yaml.SequenceNode {
			maps = maps[:0]
			for _, x := range m.Content {
				maps = append(maps, resolve(x))
			}
		}
		for _, mm := range maps {
			if mm != nil && mm.Kind == yaml.MappingNode {
				entries(mm, func(key *yaml.Node, val **yaml.Node, _ bool) { fn(key, val, true) })
			}
		}
	}
}

func (k *checker) walkStruct(n *yaml.Node, t reflect.Type, path, label string) int {
	if n.Kind != yaml.MappingNode {
		k.wrongType(n, t, path, label)
		return n.Line
	}
	fields := modelFields(t)
	end := n.Line
	first := map[string]int{}
	entries(n, func(key *yaml.Node, val **yaml.Node, merged bool) {
		name := key.Value
		f, ok := fieldByKey(fields, name)
		if !ok {
			k.unknownKey(key, path, label, fields)
			return
		}
		if !merged {
			if line, dup := first[name]; dup {
				k.add(key, joinPath(path, name), "%s: set twice (first on line %d)", joinPath(label, name), line)
				return
			}
			first[name] = key.Line
		}
		p := joinPath(path, name)
		e := k.walk(val, f.typ, p, joinPath(label, name))
		if _, seen := k.spans[p]; !seen || !merged {
			k.spans[p] = lineSpan{key.Line, max(e, key.Line)}
		}
		if !merged {
			end = max(end, e)
		}
	})
	return end
}

func (k *checker) walkList(slot **yaml.Node, t reflect.Type, path, label string) int {
	n := resolve(*slot)
	elem := t.Elem()
	switch {
	case n.Kind == yaml.SequenceNode:
	case n.Kind == yaml.MappingNode && deref(elem) == policyType:
		if !k.policyMap(slot, n, path, label) {
			return n.Line
		}
		n = *slot
	case n.Kind == yaml.ScalarNode && isScalarType(elem):
		*slot = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Line: (*slot).Line, Column: (*slot).Column, Content: []*yaml.Node{*slot}}
		k.changed = true
		n = *slot
	default:
		k.wrongType(n, t, path, label)
		return n.Line
	}
	end := n.Line
	for i := range n.Content {
		item := resolve(n.Content[i])
		p := fmt.Sprintf("%s[%d]", path, i)
		l := fmt.Sprintf("%s[%d]%s", label, i, itemDecoration(item, deref(elem)))
		e := k.walk(&n.Content[i], elem, p, l)
		k.spans[p] = lineSpan{n.Content[i].Line, max(e, n.Content[i].Line)}
		end = max(end, e)
	}
	return end
}

func (k *checker) walkMap(n *yaml.Node, t reflect.Type, path, label string) int {
	if n.Kind != yaml.MappingNode {
		k.wrongType(n, t, path, label)
		return n.Line
	}
	end := n.Line
	entries(n, func(key *yaml.Node, val **yaml.Node, merged bool) {
		p := joinPath(path, key.Value)
		e := k.walk(val, t.Elem(), p, joinPath(label, key.Value))
		if _, seen := k.spans[p]; !seen || !merged {
			k.spans[p] = lineSpan{key.Line, max(e, key.Line)}
		}
		if !merged {
			end = max(end, e)
		}
	})
	return end
}

func itemDecoration(item *yaml.Node, elem reflect.Type) string {
	switch elem {
	case policyType:
		return fmt.Sprintf(" (id=%q)", mappingScalar(item, "id"))
	case hostType, jobType:
		if name := mappingScalar(item, "name"); name != "" {
			return " (" + name + ")"
		}
	}
	return ""
}

func mappingScalar(n *yaml.Node, key string) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			if v := resolve(n.Content[i+1]); v != nil && v.Kind == yaml.ScalarNode {
				return v.Value
			}
		}
	}
	return ""
}

func (k *checker) policyMap(slot **yaml.Node, n *yaml.Node, path, label string) bool {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Line: n.Line, Column: n.Column}
	ok := true
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Tag == "!!merge" {
			k.add(key, path, "%s: << merges are not supported in the map form of policies; write the policies as a list", label)
			ok = false
			continue
		}
		id := key.Value
		idKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "id", Line: key.Line, Column: key.Column}
		idVal := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id, Line: key.Line, Column: key.Column}
		v := resolve(val)
		switch {
		case val.Kind == yaml.AliasNode && v != nil && v.Kind == yaml.MappingNode:
			merge := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<", Line: key.Line, Column: key.Column}
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: key.Line, Column: key.Column, Content: []*yaml.Node{idKey, idVal, merge, val}})
		case isNull(v):
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: key.Line, Column: key.Column, Content: []*yaml.Node{idKey, idVal}})
		case v.Kind != yaml.MappingNode:
			k.add(val, joinPath(path, id), "%s: expected the settings of policy %q (a mapping such as {mode: minor}), got %s", joinPath(label, id), id, nodeKind(v))
			ok = false
		case mappingHasKey(v, "id"):
			if own := mappingScalar(v, "id"); !strings.EqualFold(strings.TrimSpace(own), strings.TrimSpace(id)) {
				k.add(val, joinPath(path, id), "%s.id: %q does not match its key %q; leave id out in the map form", joinPath(label, id), own, id)
				ok = false
				continue
			}
			seq.Content = append(seq.Content, val)
		default:
			v.Content = append([]*yaml.Node{idKey, idVal}, v.Content...)
			seq.Content = append(seq.Content, v)
		}
	}
	if !ok {
		return false
	}
	*slot = seq
	k.changed = true
	return true
}

func mappingHasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i].Tag != "!!merge" {
			return true
		}
	}
	return false
}

func (k *checker) scheduleShorthand(slot **yaml.Node, n *yaml.Node, path, label string) bool {
	key := "cron"
	if n.ShortTag() == "!!int" {
		key = "interval"
	} else {
		cron, _, err := parseScheduleValue(n.Value)
		if err != nil {
			k.add(n, path, "%s: %s: %v", label, quoteValue(n.Value), err)
			return false
		}
		if cron == "" {
			key = "interval"
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, Line: n.Line, Column: n.Column}
	*slot = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: (*slot).Line, Column: (*slot).Column, Content: []*yaml.Node{keyNode, *slot}}
	k.changed = true
	return true
}

func (k *checker) checkScalar(n *yaml.Node, t reflect.Type, path, label string) {
	if n.Kind != yaml.ScalarNode {
		k.wrongType(n, t, path, label)
		return
	}
	err := n.Decode(reflect.New(t).Interface())
	if err == nil {
		return
	}
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 && t.Kind() != reflect.String {
		plain := *n
		plain.Style, plain.Tag = 0, ""
		if plain.Decode(reflect.New(t).Interface()) == nil {
			k.add(n, path, "%s: %s is quoted, which makes it text; write it without quotes", label, quoteValue(n.Value))
			return
		}
	}
	switch {
	case t == durationType:
		reason := err.Error()
		if _, perr := ParseDuration(n.Value); perr != nil {
			reason = perr.Error()
		}
		k.add(n, path, "%s: %s is not a duration (%s) — write it like 30s, 5m, 1h30m, 1d or 2w", label, quoteValue(n.Value), reason)
	case t.Kind() == reflect.Bool:
		k.add(n, path, "%s: %s is not true or false", label, quoteValue(n.Value))
	case isScalarType(t) && t.Kind() != reflect.String:
		k.add(n, path, "%s: %s is not a whole number", label, quoteValue(n.Value))
	default:
		k.add(n, path, "%s: %v", label, err)
	}
}

func (k *checker) wrongType(n *yaml.Node, t reflect.Type, path, label string) {
	hint := ""
	if deref(t).Kind() == reflect.Slice && n.Kind == yaml.MappingNode {
		hint = ` — start each entry with "- "`
	}
	k.add(n, path, "%s: expected %s, got %s%s", label, typeWanted(t), nodeKind(n), hint)
}

func typeWanted(t reflect.Type) string {
	t = deref(t)
	switch {
	case t == durationType:
		return "a duration such as 30s, 5m, 1h or 1d"
	case t == scheduleType:
		return "a mapping (cron:, interval:, ...), an interval or a cron spec"
	case t.Kind() == reflect.Struct:
		return "a mapping (key: value)"
	case t.Kind() == reflect.Slice:
		return "a list"
	case t.Kind() == reflect.Map:
		return "a mapping (name: value)"
	case t.Kind() == reflect.Bool:
		return "true or false"
	case t.Kind() == reflect.String:
		return "a text value"
	}
	return "a whole number"
}

func nodeKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return quoteValue(n.Value)
	}
	return "an unsupported value"
}

func quoteValue(v string) string {
	switch {
	case strings.Contains(v, "://"):
		return "a URL (not shown)"
	case len(v) > 40:
		return "a long text value (not shown)"
	}
	return strconv.Quote(v)
}

func (k *checker) unknownKey(key *yaml.Node, path, label string, fields []modelField) {
	name := key.Value
	msg := joinPath(label, name) + ": unknown key"
	if s := suggestKey(name, fields); s != "" {
		msg += fmt.Sprintf(" — did you mean %q?", s)
	} else if where := elsewhere(name); len(where) > 0 {
		msg += " — did you mean " + strings.Join(where, " or ") + "?"
	} else {
		keys := make([]string, len(fields))
		for i, f := range fields {
			keys[i] = f.key
		}
		msg += " (known here: " + strings.Join(keys, ", ") + ")"
	}
	k.add(key, joinPath(path, name), "%s", msg)
}

func suggestKey(name string, fields []modelField) string {
	norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
	keys := make([]string, len(fields))
	for i, f := range fields {
		if f.key == norm || strings.ReplaceAll(f.key, "_", "") == norm {
			return f.key
		}
		keys[i] = f.key
	}
	return suggest(norm, keys)
}

func Suggest(name string, candidates []string) string { return suggest(name, candidates) }

func suggest(name string, candidates []string) string {
	limit := 2
	if len(name) <= 3 {
		limit = 1
	}
	best, bestDist := "", limit+1
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(name), strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func elsewhere(name string) []string {
	locs := keyLocations(name)
	return locs[:min(len(locs), 3)]
}
