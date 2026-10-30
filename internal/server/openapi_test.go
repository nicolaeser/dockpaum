package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/metrics"
	"github.com/nicolaeser/dockpaum/internal/updater"
)

type openAPIOp struct {
	method, path string
	op           map[string]any
}

func (o openAPIOp) String() string { return o.method + " " + o.path }

func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("docs/openapi.json is not JSON: %v", err)
	}
	return doc
}

func openAPIOperations(t *testing.T, doc map[string]any) []openAPIOp {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("docs/openapi.json documents no paths")
	}
	var out []openAPIOp
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item, _ := paths[path].(map[string]any)
		for _, method := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
			if op, ok := item[method].(map[string]any); ok {
				out = append(out, openAPIOp{strings.ToUpper(method), path, op})
			}
		}
	}
	return out
}

func openAPIPointer(doc map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = doc
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func openAPIResolve(doc map[string]any, v any) map[string]any {
	m, _ := v.(map[string]any)
	for range 16 {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		target, _ := openAPIPointer(doc, ref)
		m, _ = target.(map[string]any)
	}
	return nil
}

func openAPIStrings(v any) []string {
	var out []string
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, s := range v {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func openAPISchema(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	s, ok := openAPIPointer(doc, "#/components/schemas/"+name)
	m, _ := s.(map[string]any)
	if !ok || m == nil {
		t.Fatalf("components.schemas.%s is missing", name)
	}
	return m
}

func openAPIRefs(v any, visit func(string)) {
	switch v := v.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			visit(ref)
		}
		for _, e := range v {
			openAPIRefs(e, visit)
		}
	case []any:
		for _, e := range v {
			openAPIRefs(e, visit)
		}
	}
}

func TestOpenAPIDocument(t *testing.T) {
	doc := loadOpenAPI(t)
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x", v)
	}
	info, _ := doc["info"].(map[string]any)
	if info["title"] != "dockpaum HTTP API" || info["version"] != "1" {
		t.Errorf("info = %v %v", info["title"], info["version"])
	}
	schemes, _ := openAPIPointer(doc, "#/components/securitySchemes")
	schemeSet, _ := schemes.(map[string]any)
	if b, _ := schemeSet["bearer"].(map[string]any); b["type"] != "http" || b["scheme"] != "bearer" {
		t.Errorf("securitySchemes.bearer = %v", schemeSet["bearer"])
	}
	tags := map[string]bool{}
	docTags, _ := doc["tags"].([]any)
	for _, tag := range docTags {
		if m, ok := tag.(map[string]any); ok {
			tags[fmt.Sprint(m["name"])] = true
		}
	}
	openAPIRefs(doc, func(ref string) {
		if _, ok := openAPIPointer(doc, ref); !ok {
			t.Errorf("$ref %q does not resolve", ref)
		}
	})

	ids := map[string]string{}
	for _, o := range openAPIOperations(t, doc) {
		id, _ := o.op["operationId"].(string)
		if id == "" {
			t.Errorf("%s: no operationId", o)
		} else if prev, dup := ids[id]; dup {
			t.Errorf("%s: operationId %q already used by %s", o, id, prev)
		}
		ids[id] = o.String()
		for _, key := range []string{"summary", "description"} {
			if s, _ := o.op[key].(string); strings.TrimSpace(s) == "" {
				t.Errorf("%s: no %s", o, key)
			}
		}
		opTags := openAPIStrings(o.op["tags"])
		if len(opTags) == 0 {
			t.Errorf("%s: no tags", o)
		}
		for _, tag := range opTags {
			if !tags[tag] {
				t.Errorf("%s: tag %q is not declared", o, tag)
			}
		}
		sec, ok := o.op["security"].([]any)
		if !ok {
			t.Errorf("%s: no security (use [] for an open route)", o)
		}
		for _, req := range sec {
			for name := range req.(map[string]any) {
				if _, ok := schemeSet[name]; !ok {
					t.Errorf("%s: security scheme %q is not declared", o, name)
				}
			}
		}
		if access := o.op["x-dockpaum-access"]; len(sec) > 0 && access != "read" && access != "write" {
			t.Errorf("%s: x-dockpaum-access = %v, want read or write", o, access)
		}
		if resp, _ := o.op["responses"].(map[string]any); len(resp) == 0 {
			t.Errorf("%s: no responses", o)
		}
	}
}

func openAPIRegistered(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, m := range regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		method, rest, ok := strings.Cut(m[1], " ")
		if !ok {
			method, rest = "", m[1]
		}
		path := rest[strings.Index(rest, "/"):]
		out[path] = append(out[path], strings.TrimSpace(method))
	}
	if len(out) == 0 {
		t.Fatal("found no mux.Handle or mux.HandleFunc route in server.go")
	}
	return out
}

func TestOpenAPICoversRegisteredRoutes(t *testing.T) {
	doc := loadOpenAPI(t)
	paths := doc["paths"].(map[string]any)
	registered := openAPIRegistered(t)
	for path, methods := range registered {
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("server.go registers %s, docs/openapi.json does not document it", path)
			continue
		}
		for _, method := range methods {
			if method != "" && item[strings.ToLower(method)] == nil {
				t.Errorf("server.go registers %s %s, docs/openapi.json does not document it", method, path)
			}
		}
	}
	for _, o := range openAPIOperations(t, doc) {
		if _, ok := registered[o.path]; !ok {
			t.Errorf("docs/openapi.json documents %s, server.go registers no such route", o)
		}
	}
}

var openAPITime = time.Date(2026, 10, 30, 4, 0, 41, 0, time.UTC)

func openAPIFill(v reflect.Value, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		openAPIFill(v.Elem(), depth+1)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(openAPITime))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				openAPIFill(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		openAPIFill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

func openAPIFilled[T any]() T {
	var v T
	openAPIFill(reflect.ValueOf(&v).Elem(), 0)
	return v
}

func openAPIServer(cfg config.HTTPConfig, trigger string) http.Handler {
	reg := metrics.NewRegistry()
	api := API{
		Trigger:    func(TriggerRequest) string { return trigger },
		Status:     func() any { return openAPIFilled[updater.Status]() },
		Containers: func(ContainersQuery) any { return []updater.ContainerStatus{openAPIFilled[updater.ContainerStatus]()} },
		Summary:    openAPIFilled[Summary],
		Busy:       openAPIFilled[Busy],
		WatchtowerMetrics: reg.WatchtowerHandler(func() metrics.LastRun {
			return metrics.LastRun{Scanned: 3, Updated: 1}
		}),
		WatchtowerCompat: true,
	}
	return New(cfg, reg.Handler(), slog.New(slog.DiscardHandler), openAPIFilled[Version](), nil, api).Handler()
}

func openAPIServe(h http.Handler, method, target, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func openAPITypeOK(schema map[string]any, v any) bool {
	types := openAPIStrings(schema["type"])
	if len(types) == 0 {
		return true
	}
	var got string
	switch x := v.(type) {
	case nil:
		got = "null"
	case bool:
		got = "boolean"
	case string:
		got = "string"
	case float64:
		if x == math.Trunc(x) && slices.Contains(types, "integer") {
			return true
		}
		got = "number"
	case map[string]any:
		got = "object"
	case []any:
		got = "array"
	}
	return slices.Contains(types, got)
}

func openAPIMatch(t *testing.T, doc map[string]any, schema any, v any, at string) {
	t.Helper()
	s := openAPIResolve(doc, schema)
	if s == nil {
		t.Errorf("%s: no schema", at)
		return
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		alts, ok := s[key].([]any)
		if !ok {
			continue
		}
		for _, alt := range alts {
			if a := openAPIResolve(doc, alt); a != nil && openAPITypeOK(a, v) && (a["type"] != nil || v != nil) {
				openAPIMatch(t, doc, a, v, at)
				return
			}
		}
		t.Errorf("%s: %v fits no %s branch", at, v, key)
		return
	}
	if !openAPITypeOK(s, v) {
		t.Errorf("%s: JSON %T does not fit type %v", at, v, s["type"])
		return
	}
	switch v := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if got, want := slices.Sorted(maps.Keys(v)), slices.Sorted(maps.Keys(props)); !slices.Equal(got, want) {
			t.Errorf("%s: response fields %v, spec documents %v", at, got, want)
		}
		for _, r := range openAPIStrings(s["required"]) {
			if _, ok := v[r]; !ok {
				t.Errorf("%s: required field %q missing from the response", at, r)
			}
		}
		for k, val := range v {
			if p, ok := props[k]; ok {
				openAPIMatch(t, doc, p, val, at+"."+k)
			}
		}
	case []any:
		for i, e := range v {
			openAPIMatch(t, doc, s["items"], e, fmt.Sprintf("%s[%d]", at, i))
		}
	}
}

func openAPICheckResponse(t *testing.T, doc map[string]any, o openAPIOp, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	responses, _ := o.op["responses"].(map[string]any)
	resp := openAPIResolve(doc, responses[strconv.Itoa(rec.Code)])
	if resp == nil {
		t.Errorf("%s %s: status %d is not documented (body %q)", label, o, rec.Code, rec.Body.String())
		return
	}
	headers, _ := resp["headers"].(map[string]any)
	for name, h := range headers {
		got := rec.Header().Get(name)
		if got == "" {
			t.Errorf("%s %s: documented header %s missing on %d", label, o, name, rec.Code)
			continue
		}
		schema, _ := openAPIResolve(doc, h)["schema"].(map[string]any)
		if want, ok := schema["const"].(string); ok && got != want {
			t.Errorf("%s %s: header %s = %q, spec says %q", label, o, name, got, want)
		}
	}
	content, _ := resp["content"].(map[string]any)
	if len(content) == 0 || rec.Body.Len() == 0 {
		return
	}
	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(rec.Body.Bytes())
	}
	gotType, gotParams, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Errorf("%s %s: Content-Type %q: %v", label, o, ct, err)
		return
	}
	for key, media := range content {
		wantType, wantParams, err := mime.ParseMediaType(key)
		if err != nil {
			t.Errorf("%s %s: content key %q: %v", label, o, key, err)
			continue
		}
		if wantType != gotType {
			continue
		}
		for p, v := range wantParams {
			if gotParams[p] != v {
				t.Errorf("%s %s: Content-Type %q lacks %s=%s documented in %q", label, o, ct, p, v, key)
			}
		}
		if gotType == "application/json" {
			var body any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("%s %s: body is not JSON: %v", label, o, err)
				return
			}
			openAPIMatch(t, doc, media.(map[string]any)["schema"], body, label+" "+o.String()+" "+strconv.Itoa(rec.Code))
		}
		return
	}
	t.Errorf("%s %s: Content-Type %q on %d is not documented (have %v)", label, o, ct, rec.Code, slices.Sorted(maps.Keys(content)))
}

func openAPIBadQuery(o openAPIOp) string {
	params, _ := o.op["parameters"].([]any)
	for _, p := range params {
		param, _ := p.(map[string]any)
		ref, _ := param["$ref"].(string)
		name, _ := param["name"].(string)
		switch {
		case name == "image" || strings.HasSuffix(ref, "/TriggerImage"):
			return "?image=bad%20name"
		case name == "pending" || strings.HasSuffix(ref, "/ListPending"):
			return "?pending=maybe"
		}
	}
	return ""
}

func TestOpenAPIOperationsAreServed(t *testing.T) {
	doc := loadOpenAPI(t)
	secured := openAPIServer(config.HTTPConfig{Token: "full", ReadToken: "reader", UpdateAPI: true}, "queued")
	open := openAPIServer(config.HTTPConfig{}, "queued")
	for _, o := range openAPIOperations(t, doc) {
		rec := openAPIServe(secured, o.method, o.path, "Bearer full")
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s with the full token = %d: the documented operation is not served", o, rec.Code)
			continue
		}
		if rec.Code >= 300 {
			t.Errorf("%s with the full token = %d %q, want success", o, rec.Code, rec.Body.String())
		}
		openAPICheckResponse(t, doc, o, rec, "full token:")

		rec = openAPIServe(secured, o.method, o.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials = %d, want 401", o, rec.Code)
		}
		openAPICheckResponse(t, doc, o, rec, "no credentials:")

		rec = openAPIServe(secured, o.method, o.path, "Bearer reader")
		switch o.op["x-dockpaum-access"] {
		case "read":
			if rec.Code >= 300 {
				t.Errorf("%s is documented as read access, the read token got %d", o, rec.Code)
			}
		case "write":
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s is documented as write access, the read token got %d, want 403", o, rec.Code)
			}
		default:
			t.Errorf("%s: x-dockpaum-access = %v", o, o.op["x-dockpaum-access"])
		}
		openAPICheckResponse(t, doc, o, rec, "read token:")

		rec = openAPIServe(open, o.method, o.path, "")
		if o.op["x-dockpaum-access"] == "write" {
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s without http.token configured = %d, want 404", o, rec.Code)
			}
		} else if rec.Code >= 300 {
			t.Errorf("%s without any token configured = %d, want success", o, rec.Code)
		}
		openAPICheckResponse(t, doc, o, rec, "no token configured:")

		responses, _ := o.op["responses"].(map[string]any)
		if _, ok := responses["405"]; ok {
			rec = openAPIServe(secured, http.MethodDelete, o.path, "Bearer full")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("DELETE %s = %d, want the documented 405", o.path, rec.Code)
			}
			openAPICheckResponse(t, doc, o, rec, "wrong method:")
		}
		if _, ok := responses["400"]; ok {
			q := openAPIBadQuery(o)
			if q == "" {
				t.Errorf("%s documents 400 but has no parameter this test knows how to break", o)
				continue
			}
			rec = openAPIServe(secured, o.method, o.path+q, "Bearer full")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s%s = %d, want the documented 400", o, q, rec.Code)
			}
			openAPICheckResponse(t, doc, o, rec, "bad query:")
		}
	}
}

func openAPIJSONFields(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			maps.Copy(out, openAPIJSONFields(ft))
			continue
		}
		if name == "" {
			name = f.Name
		}
		required := true
		for opt := range strings.SplitSeq(opts, ",") {
			if opt == "omitempty" || opt == "omitzero" {
				required = false
			}
		}
		out[name] = required
	}
	return out
}

func openAPIExampleValues(doc map[string]any, resp map[string]any) []any {
	var out []any
	content, _ := resp["content"].(map[string]any)
	for _, media := range content {
		m, _ := media.(map[string]any)
		if ex, ok := m["example"]; ok {
			out = append(out, ex)
		}
		examples, _ := m["examples"].(map[string]any)
		for _, ex := range examples {
			out = append(out, openAPIResolve(doc, ex)["value"])
		}
	}
	return out
}

func TestOpenAPIResponseFieldsMatchGoTypes(t *testing.T) {
	doc := loadOpenAPI(t)
	for name, typ := range map[string]reflect.Type{
		"Version":         reflect.TypeFor[Version](),
		"Summary":         reflect.TypeFor[Summary](),
		"Busy":            reflect.TypeFor[Busy](),
		"Status":          reflect.TypeFor[updater.Status](),
		"HostStatus":      reflect.TypeFor[updater.HostStatus](),
		"RunSummary":      reflect.TypeFor[updater.RunSummary](),
		"ContainerStatus": reflect.TypeFor[updater.ContainerStatus](),
		"ImageVersion":    reflect.TypeFor[updater.ImageVersion](),
		"Candidate":       reflect.TypeFor[updater.Candidate](),
		"ApprovalStatus":  reflect.TypeFor[updater.ApprovalStatus](),
	} {
		schema := openAPISchema(t, doc, name)
		props, _ := schema["properties"].(map[string]any)
		fields := openAPIJSONFields(typ)
		if got, want := slices.Sorted(maps.Keys(props)), slices.Sorted(maps.Keys(fields)); !slices.Equal(got, want) {
			t.Errorf("components.schemas.%s properties %v, %s JSON fields %v", name, got, typ, want)
		}
		var always []string
		for f, req := range fields {
			if req {
				always = append(always, f)
			}
		}
		required := openAPIStrings(schema["required"])
		slices.Sort(required)
		slices.Sort(always)
		if !slices.Equal(required, always) {
			t.Errorf("components.schemas.%s required %v, %s always encodes %v", name, required, typ, always)
		}
		for f, p := range props {
			if d, _ := openAPIResolve(doc, p)["description"].(string); d == "" {
				if d, _ = p.(map[string]any)["description"].(string); d == "" {
					t.Errorf("components.schemas.%s.%s has no description", name, f)
				}
			}
		}
	}

	status, _ := openAPIPointer(doc, "#/components/schemas/TriggerResponse/properties/status")
	statusSchema, _ := status.(map[string]any)
	values := openAPIStrings(statusSchema["enum"])
	if len(values) == 0 {
		t.Fatal("components.schemas.TriggerResponse.status has no enum")
	}
	cfg := config.HTTPConfig{Token: "full", UpdateAPI: true}
	for _, value := range values {
		h := openAPIServer(cfg, value)
		for _, o := range openAPIOperations(t, doc) {
			if o.op["x-dockpaum-access"] != "write" {
				continue
			}
			rec := openAPIServe(h, o.method, o.path, "Bearer full")
			responses, _ := o.op["responses"].(map[string]any)
			resp := openAPIResolve(doc, responses[strconv.Itoa(rec.Code)])
			if resp == nil {
				t.Errorf("%s answering %q: status %d is not documented", o, value, rec.Code)
				continue
			}
			openAPICheckResponse(t, doc, o, rec, "trigger "+value+":")
			found := false
			for _, ex := range openAPIExampleValues(doc, resp) {
				if m, ok := ex.(map[string]any); ok && m["status"] == value {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: the documented %d response has no example with status %q", o, rec.Code, value)
			}
		}
	}
}

func TestOpenAPIListsTheMetrics(t *testing.T) {
	doc := loadOpenAPI(t)
	describe := func(path string) string {
		op, _ := openAPIPointer(doc, "#/paths/"+strings.ReplaceAll(path, "/", "~1")+"/get/description")
		s, _ := op.(string)
		return s
	}
	names := func(re, s string) []string {
		set := map[string]bool{}
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(s, -1) {
			set[m[len(m)-1]] = true
		}
		return slices.Sorted(maps.Keys(set))
	}

	src, err := os.ReadFile(filepath.Join("..", "metrics", "metrics.go"))
	if err != nil {
		t.Fatal(err)
	}
	inCode := names(`"(dockpaum_[a-z0-9_]+)"`, string(src))
	if documented := names("`(dockpaum_[a-z0-9_]+)`", describe("/internal-api/metrics")); !slices.Equal(documented, inCode) {
		t.Errorf("GET /internal-api/metrics documents %v, internal/metrics defines %v", documented, inCode)
	}

	reg := metrics.NewRegistry()
	rec := openAPIServe(reg.WatchtowerHandler(func() metrics.LastRun { return metrics.LastRun{} }), http.MethodGet, "/v1/metrics", "")
	served := names(`(?m)^# TYPE (watchtower_[a-z0-9_]+) `, rec.Body.String())
	if len(served) == 0 {
		t.Fatalf("the Watchtower handler serves no watchtower_ metric: %d %q", rec.Code, rec.Body.String())
	}
	if documented := names("`(watchtower_[a-z0-9_]+)`", describe("/v1/metrics")); !slices.Equal(documented, served) {
		t.Errorf("GET /v1/metrics documents %v, the handler serves %v", documented, served)
	}
}
