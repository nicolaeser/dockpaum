package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var updateSchema = flag.Bool("update", false, "rewrite dockpaum.schema.json from the configuration model")

var schemaFile = filepath.Join("..", "..", "dockpaum.schema.json")

func TestSchema(t *testing.T) {
	got, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if *updateSchema {
		if err := os.MkdirAll(filepath.Dir(schemaFile), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(schemaFile, got, 0o644); err != nil { // #nosec G306 -- a published, non-secret file
			t.Fatal(err)
		}
		t.Logf("wrote %s", schemaFile)
	}
	want, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read the committed schema: %v (create it with: go test ./internal/config -run TestSchema -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("dockpaum.schema.json does not match the configuration model; regenerate it with: go test ./internal/config -run TestSchema -update")
	}
}

func TestSchemaDescribesEveryKey(t *testing.T) {
	known := map[string]bool{}
	var visit func(t reflect.Type)
	visit = func(rt reflect.Type) {
		rt = deref(rt)
		switch {
		case rt == durationType:
		case rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map:
			visit(rt.Elem())
		case rt.Kind() == reflect.Struct:
			if known[rt.Name()] {
				return
			}
			if rt != configType {
				known[rt.Name()] = true
			}
			for _, f := range modelFields(rt) {
				known[rt.Name()+"."+f.key] = true
				visit(f.typ)
			}
		}
	}
	visit(configType)
	for key := range known {
		if schemaDescriptions[key] == "" {
			t.Errorf("schemaDescriptions has no entry for %s", key)
		}
	}
	for key := range schemaDescriptions {
		if !known[key] {
			t.Errorf("schemaDescriptions describes %s, which the model does not have", key)
		}
	}
	for key := range schemaEnums {
		if !known[key] {
			t.Errorf("schemaEnums lists %s, which the model does not have", key)
		}
	}
	for key := range schemaRules {
		if !known[key] {
			t.Errorf("schemaRules lists %s, which the model does not have", key)
		}
	}
}

func TestSchemaAcceptsShippedConfigs(t *testing.T) {
	s := loadSchema(t)
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "examples", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if errs := s.check(t, raw); len(errs) > 0 {
			t.Errorf("%s does not conform to the schema:\n  %s", file, strings.Join(errs, "\n  "))
		}
	}
	if errs := s.check(t, DefaultConfig()); len(errs) > 0 {
		t.Errorf("the embedded default does not conform:\n  %s", strings.Join(errs, "\n  "))
	}
}

func TestSchemaFollowsTheLoader(t *testing.T) {
	isolateEnv(t)
	s := loadSchema(t)
	tests := []struct {
		name string
		doc  string
		ok   bool
	}{
		{"schedule mapping", "schedule:\n  cron: \"0 4 * * *\"\n  run_on_start: true\n", true},
		{"schedule interval shorthand", "schedule: 6h\n", true},
		{"schedule cron shorthand", "schedule: \"0 4 * * *\"\n", true},
		{"schedule seconds shorthand", "schedule: 3600\n", true},
		{"policies as a list", "update:\n  policies:\n    - id: db\n      mode: minor\n      monitor_only: true\n      tags: {include: '^\\d+\\.\\d+$'}\n", true},
		{"policies as a map", "update:\n  policies:\n    default: {min_image_age: 2h}\n    db: {mode: minor}\n    web:\n", true},
		{"one value where a list is expected", "notify:\n  - url: https://example.com/hook\n    type: webhook\n    events: update.failure\nschedule:\n  windows:\n    - days: Mon-Fri\n      times: \"03:00-05:00\"\n", true},
		{"durations", "audit:\n  retention: 31d\nupdate:\n  stop_timeout: 90\n  policies: [{min_image_age: 1h30m, health_timeout: \"0s\"}]\n", true},
		{"environment references in typed values", "dry_run: ${DRY_RUN}\nhttp:\n  port: ${PORT:-8080}\nschedule:\n  interval: ${INTERVAL}\n", true},
		{"unknown key", "dryrun: true\n", false},
		{"unknown nested key", "update:\n  policies:\n    - windos: []\n", false},
		{"unknown mode", "update:\n  policies:\n    - mode: minr\n", false},
		{"unknown event", "notify:\n  - type: slack\n    url: x\n    events: [update.sucess]\n", false},
		{"unknown notifier type", "notify:\n  - type: pigeon\n", false},
		{"bad duration", "schedule:\n  interval: 1 day\n", false},
		{"list where a mapping is expected", "log: [debug]\n", false},
		{"bool as text", "dry_run: maybe\n", false},
		{"YAML 1.1 booleans", "dry_run: yes\nfilters:\n  label_enable: Off\nupdate:\n  self_update:\n    enabled: n\n", true},
		{"a quoted YAML 1.1 boolean", "dry_run: \"on\"\n", true},
		{"an unclosed reference stays text", "dry_run: ${DRY_RUN\n", false},
		{"an escaped dollar is no reference", "dry_run: $$DRY_RUN\n", false},
		{"a reference with text around it", "schedule:\n  interval: ${HOURS}h\n", true},
		{"a long instance name", "instance: " + strings.Repeat("x", maxInstanceLen) + "\n", true},
		{"a too long instance name", "instance: " + strings.Repeat("x", maxInstanceLen+1) + "\n", false},
		{"an instance name from a reference", "instance: ${HOSTNAME_FOR_DOCKPAUM_" + strings.Repeat("X", maxInstanceLen) + "}\n", true},
		{"an absolute state directory", "state:\n  dir: /srv/state\n", true},
		{"a blank state directory", "state:\n  dir: ''\n", true},
		{"a relative state directory", "state:\n  dir: data/state\n", false},
		{"a state directory from a reference", "state:\n  dir: ${STATE_DIR}\n", true},
		{"an SMTP port", "notify:\n  - type: email\n    to: [ops@example.com]\n    smtp: {host: mail.example.com, port: 465}\n", true},
		{"an SMTP port out of range", "notify:\n  - type: email\n    to: [ops@example.com]\n    smtp: {host: mail.example.com, port: 65536}\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := s.check(t, []byte(tt.doc))
			if tt.ok && len(errs) > 0 {
				t.Errorf("the schema rejects a valid document:\n  %s", strings.Join(errs, "\n  "))
			}
			if !tt.ok && len(errs) == 0 {
				t.Error("the schema accepts a document dockpaum rejects")
			}
			if tt.ok && !strings.Contains(tt.doc, "${") {
				if _, err := decode([]byte(tt.doc), "schema"); err != nil {
					t.Errorf("the loader rejects what the schema accepts: %v", err)
				}
			}
			if !tt.ok {
				if _, err := decode([]byte(tt.doc), "schema"); err == nil {
					t.Error("the loader accepts what the schema rejects")
				}
			}
		})
	}
}

type jsonSchema struct {
	root map[string]any
	defs map[string]any
}

func loadSchema(t *testing.T) jsonSchema {
	t.Helper()
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("the schema is not valid JSON: %v", err)
	}
	if root["$schema"] != "http://json-schema.org/draft-07/schema#" || root["$id"] != SchemaURL {
		t.Errorf("$schema/$id = %v/%v", root["$schema"], root["$id"])
	}
	defs, _ := root["definitions"].(map[string]any)
	return jsonSchema{root: root, defs: defs}
}

func (s jsonSchema) check(t *testing.T, doc []byte) []string {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(doc, &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v == nil {
		v = map[string]any{}
	}
	return s.validate(s.root, v, "")
}

func (s jsonSchema) validate(sch map[string]any, v any, path string) []string {
	if r, ok := sch["$ref"].(string); ok {
		def, _ := s.defs[strings.TrimPrefix(r, "#/definitions/")].(map[string]any)
		if def == nil {
			return []string{path + ": unresolved " + r}
		}
		return s.validate(def, v, path)
	}
	var errs []string
	if all, ok := sch["allOf"].([]any); ok {
		for _, sub := range all {
			errs = append(errs, s.validate(sub.(map[string]any), v, path)...)
		}
	}
	if alts, ok := sch["anyOf"].([]any); ok {
		matched := slices.ContainsFunc(alts, func(sub any) bool { return len(s.validate(sub.(map[string]any), v, path)) == 0 })
		if !matched {
			errs = append(errs, fmt.Sprintf("%s: %v matches no alternative", path, v))
		}
	}
	if typ, ok := sch["type"].(string); ok && !jsonTypeIs(typ, v) {
		return append(errs, fmt.Sprintf("%s: %v is not %s", path, v, typ))
	}
	if enum, ok := sch["enum"].([]any); ok && !slices.Contains(enum, v) {
		errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", path, v, enum))
	}
	if pat, ok := sch["pattern"].(string); ok {
		if str, isStr := v.(string); isStr && !regexp.MustCompile(pat).MatchString(str) {
			errs = append(errs, fmt.Sprintf("%s: %q does not match %s", path, str, pat))
		}
	}
	if limit, ok := sch["maxLength"].(float64); ok {
		if str, isStr := v.(string); isStr && float64(utf8.RuneCountInString(str)) > limit {
			errs = append(errs, fmt.Sprintf("%s: %q is longer than %v characters", path, str, limit))
		}
	}
	if n, isNum := jsonNumber(v); isNum {
		if limit, ok := sch["minimum"].(float64); ok && n < limit {
			errs = append(errs, fmt.Sprintf("%s: %v is less than %v", path, v, limit))
		}
		if limit, ok := sch["maximum"].(float64); ok && n > limit {
			errs = append(errs, fmt.Sprintf("%s: %v is more than %v", path, v, limit))
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := sch["properties"].(map[string]any)
		for k, val := range obj {
			if p, ok := props[k].(map[string]any); ok {
				errs = append(errs, s.validate(p, val, joinPath(path, k))...)
				continue
			}
			switch extra := sch["additionalProperties"].(type) {
			case bool:
				if !extra && props != nil {
					errs = append(errs, fmt.Sprintf("%s: unknown key %q", path, k))
				}
			case map[string]any:
				errs = append(errs, s.validate(extra, val, joinPath(path, k))...)
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := sch["items"].(map[string]any); ok {
			for i, item := range arr {
				errs = append(errs, s.validate(items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return errs
}

func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func jsonTypeIs(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch v.(type) {
		case int, int64, uint64:
			return true
		}
		return false
	case "null":
		return v == nil
	}
	return false
}
