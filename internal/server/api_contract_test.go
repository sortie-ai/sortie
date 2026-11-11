package server

import (
	"bytes"
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/runstats"
)

var updateContract = flag.Bool("update", false, "rewrite the generated API contract files")

var (
	timeType          = reflect.TypeFor[time.Time]()
	reactionCheckType = reflect.TypeFor[reactionCheckResponse]()
	runEventType      = reflect.TypeFor[runEventResponse]()
	agentEventType    = reflect.TypeFor[agentEventFrame]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

type contractNode struct {
	ts     string
	schema map[string]any
}

type contractMember struct {
	name     string
	node     contractNode
	optional bool
}

type contract struct {
	members map[string][]contractMember
	goTypes map[string]reflect.Type
}

type jsonField struct {
	name     string
	typ      reflect.Type
	optional bool
	depth    int
}

var contractEnums = map[reflect.Type]map[string][]string{
	reflect.TypeFor[holdResponse]():        {"reason": holdReasonValues()},
	reflect.TypeFor[usageWindowResponse](): {"status": usageWindowStatusValues()},
	reflect.TypeFor[runstats.Spend]():      {"period": spendPeriodValues()},
	reflect.TypeFor[runstats.SpanReport](): {"period": spendPeriodValues()},
}

var contractNeverNull = map[reflect.Type][]string{
	reflect.TypeFor[stateResponse](): {"rate_limits"},
}

var dateTime = contractNode{"string", map[string]any{"type": "string", "format": "date-time"}}

func primitive(ts, jsonType string) contractNode {
	return contractNode{ts, map[string]any{"type": jsonType}}
}

func enumNode(values []string) contractNode {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return contractNode{strings.Join(quoted, " | "), map[string]any{"type": "string", "enum": values}}
}

func holdReasonValues() []string {
	reasons := orchestrator.HoldReasons()
	values := make([]string, len(reasons))
	for i, r := range reasons {
		values[i] = string(r)
	}
	return values
}

func spendPeriodValues() []string {
	return []string{
		string(runstats.PeriodFiveMinutes),
		string(runstats.PeriodQuarterHour),
		string(runstats.PeriodHour),
		string(runstats.PeriodDay),
		string(runstats.PeriodMonth),
	}
}

func usageWindowStatusValues() []string {
	return []string{
		string(domain.UsageWindowAllowed),
		string(domain.UsageWindowWarning),
		string(domain.UsageWindowExhausted),
	}
}

func declaredHoldReasons(t *testing.T) []string {
	t.Helper()

	const dir = "../orchestrator"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var values []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "SkipReason" {
					continue
				}
				for i, name := range vs.Names {
					if name.Name == "SkipNone" || name.Name == "SkipIneligible" {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is not a string literal", name.Name)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("Unquote(%s): %v", lit.Value, err)
					}
					values = append(values, value)
				}
			}
		}
	}
	return values
}

func nullable(n contractNode) contractNode {
	return contractNode{n.ts + " | null", map[string]any{"anyOf": []any{n.schema, map[string]any{"type": "null"}}}}
}

func newContract(roots ...reflect.Type) (*contract, error) {
	c := &contract{members: map[string][]contractMember{}, goTypes: map[string]reflect.Type{}}
	for _, root := range roots {
		if _, err := c.convert(root); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *contract) convert(t reflect.Type) (contractNode, error) {
	if t.Kind() == reflect.Pointer {
		return c.convert(t.Elem())
	}
	if t == timeType {
		return dateTime, nil
	}
	if t != reactionCheckType && t != runEventType && t != agentEventType && (reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)) {
		return contractNode{}, fmt.Errorf("%v implements a marshaler interface", t)
	}

	switch k := t.Kind(); {
	case k == reflect.String:
		return primitive("string", "string"), nil
	case k == reflect.Bool:
		return primitive("boolean", "boolean"), nil
	case k >= reflect.Int && k <= reflect.Uint64:
		return primitive("number", "integer"), nil
	case k == reflect.Float32 || k == reflect.Float64:
		return primitive("number", "number"), nil
	case k == reflect.Interface:
		return contractNode{"unknown", map[string]any{}}, nil
	case k == reflect.Slice:
		e, err := c.convert(t.Elem())
		return contractNode{e.ts + "[]", map[string]any{"type": "array", "items": e.schema}}, err
	case k == reflect.Map:
		if t.Key() != reflect.TypeFor[string]() {
			return contractNode{}, fmt.Errorf("%v has a map key type other than string", t)
		}
		e, err := c.convert(t.Elem())
		return contractNode{"Record<string, " + e.ts + ">", map[string]any{"type": "object", "additionalProperties": e.schema}}, err
	case k == reflect.Struct:
		if t.Name() == "" {
			return contractNode{}, fmt.Errorf("%v is an unnamed struct type", t)
		}
		name, err := c.addStruct(t)
		return contractNode{name, map[string]any{"$ref": "#/components/schemas/" + name}}, err
	}
	return contractNode{}, fmt.Errorf("%v has unsupported kind %v", t, t.Kind())
}

func (c *contract) addStruct(t reflect.Type) (string, error) {
	name := strings.ToUpper(t.Name()[:1]) + t.Name()[1:]
	if prev, ok := c.goTypes[name]; ok {
		if prev != t {
			return "", fmt.Errorf("%v and %v both map to %s", prev, t, name)
		}
		return name, nil
	}
	c.goTypes[name] = t
	if t == reactionCheckType {
		c.members[name] = reactionCheckMembers()
		return name, nil
	}
	if t == runEventType || t == agentEventType {
		usage, err := c.convert(reflect.TypeFor[tokenInfo]())
		members := runEventMembers(usage)
		if t == agentEventType {
			members = slices.Insert(members, 0, contractMember{"dispatch_id", primitive("string", "string"), false})
		}
		c.members[name] = members
		return name, err
	}

	fields, err := jsonFields(t)
	if err != nil {
		return "", err
	}
	members := make([]contractMember, 0, len(fields))
	for _, f := range fields {
		n, err := c.convert(f.typ)
		if err != nil {
			return "", err
		}
		if values, ok := contractEnums[t][f.name]; ok {
			n = enumNode(values)
		}
		if k := f.typ.Kind(); !f.optional && !slices.Contains(contractNeverNull[t], f.name) && (k == reflect.Pointer || k == reflect.Slice || k == reflect.Map || k == reflect.Interface) {
			n = nullable(n)
		}
		members = append(members, contractMember{f.name, n, f.optional})
	}
	c.members[name] = members
	return name, nil
}

func reactionCheckMembers() []contractMember {
	str, num := primitive("string", "string"), primitive("number", "integer")
	return []contractMember{
		{"observed_at", dateTime, false},
		{"head_sha", str, true},
		{"status", str, true},
		{"failing_checks", contractNode{"string[]", map[string]any{"type": "array", "items": str.schema}}, true},
		{"failing", num, true},
		{"pending", num, true},
		{"passing", num, true},
		{"actionable_comments", num, true},
		{"new_comments", num, true},
		{"handled", primitive("boolean", "boolean"), true},
		{"awaiting_approval", primitive("boolean", "boolean"), true},
		{"mergeable", nullable(primitive("boolean", "boolean")), true},
	}
}

func runEventMembers(usage contractNode) []contractMember {
	str, num := primitive("string", "string"), primitive("number", "integer")
	return []contractMember{
		{"seq", num, false},
		{"at", dateTime, false},
		{"event", str, false},
		{"message", str, false},
		{"text", str, false},
		{"tool_line", str, true},
		{"session_id", str, true},
		{"tool_name", str, true},
		{"tool_kind", str, true},
		{"tool_argument", str, true},
		{"duration_ms", num, true},
		{"tool_error", primitive("boolean", "boolean"), true},
		{"usage", nullable(usage), true},
	}
}

func jsonFields(t reflect.Type) ([]jsonField, error) {
	var all []jsonField
	if err := collectFields(t, 0, &all); err != nil {
		return nil, err
	}
	shallowest := map[string]int{}
	for _, f := range all {
		if d, seen := shallowest[f.name]; !seen || f.depth < d {
			shallowest[f.name] = f.depth
		}
	}
	var kept []jsonField
	for _, f := range all {
		if f.depth != shallowest[f.name] {
			continue
		}
		if slices.ContainsFunc(kept, func(k jsonField) bool { return k.name == f.name }) {
			return nil, fmt.Errorf("%v has two fields named %q at depth %d", t, f.name, f.depth)
		}
		kept = append(kept, f)
	}
	return kept, nil
}

func collectFields(t reflect.Type, depth int, out *[]jsonField) error {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		options := strings.Split(opts, ",")
		if slices.Contains(options, "string") {
			return fmt.Errorf("%v field %s carries the string tag option", t, f.Name)
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			if err := collectFields(f.Type, depth+1, out); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		optional := slices.Contains(options, "omitempty") || slices.Contains(options, "omitzero")
		*out = append(*out, jsonField{name, f.Type, optional, depth})
	}
	return nil
}

func (c *contract) typescript() []byte {
	var b strings.Builder
	b.WriteString("// Code generated by TestAPIContract in internal/server. DO NOT EDIT.\n")
	for _, name := range slices.Sorted(maps.Keys(c.members)) {
		fmt.Fprintf(&b, "\nexport interface %s {\n", name)
		for _, m := range c.members[name] {
			mark := ""
			if m.optional {
				mark = "?"
			}
			fmt.Fprintf(&b, "  %s%s: %s;\n", m.name, mark, m.node.ts)
		}
		b.WriteString("}\n")
	}
	return []byte(b.String())
}

func contractRoots(routes []apiRoute) []reflect.Type {
	var roots []reflect.Type
	for _, route := range routes {
		for _, response := range route.responses {
			if response.body != nil {
				roots = append(roots, response.body)
			}
			for _, event := range response.events {
				if event.data != nil {
					roots = append(roots, event.data)
				}
			}
		}
	}
	return roots
}

func registeredRoutes(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var patterns []string
	var failure error
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || failure != nil {
			return failure == nil
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (fn.Sel.Name != "Handle" && fn.Sel.Name != "HandleFunc") || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			failure = fmt.Errorf("%s: %s pattern is not a string literal", fset.Position(call.Args[0].Pos()), fn.Sel.Name)
			return false
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			failure = fmt.Errorf("%s: unquote %s: %w", fset.Position(lit.Pos()), lit.Value, err)
			return false
		}
		_, route, found := strings.Cut(pattern, " ")
		if !found {
			route = pattern
		}
		if strings.HasPrefix(route, "/api/v1/") || route == "/livez" || route == "/readyz" {
			patterns = append(patterns, pattern)
		}
		return true
	})
	return patterns, failure
}

func absentFrom(names, others []string) []string {
	var absent []string
	for _, name := range names {
		if !slices.Contains(others, name) {
			absent = append(absent, name)
		}
	}
	return absent
}

var pathParamPattern = regexp.MustCompile(`\{([^}]+)\}`)

func (c *contract) schemas() map[string]any {
	schemas := map[string]any{}
	for name, members := range c.members {
		properties, required := map[string]any{}, []string{}
		for _, m := range members {
			properties[m.name] = m.node.schema
			if !m.optional {
				required = append(required, m.name)
			}
		}
		schemas[name] = map[string]any{"type": "object", "properties": properties, "required": required}
	}
	return schemas
}

func operationParameters(route apiRoute, path string) ([]any, error) {
	var parameters []any
	for _, match := range pathParamPattern.FindAllStringSubmatch(path, -1) {
		parameters = append(parameters, map[string]any{
			"name": match[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"},
		})
	}
	for _, location := range []struct {
		in    string
		names []string
	}{{"query", route.query}, {"header", route.headers}} {
		for _, name := range location.names {
			param, ok := route.params[name]
			if !ok {
				return nil, fmt.Errorf("%s has %s parameter %q without a params entry", route.pattern, location.in, name)
			}
			parameter := map[string]any{"name": name, "in": location.in, "schema": param.schema}
			if param.required {
				parameter["required"] = true
			}
			if param.description != "" {
				parameter["description"] = param.description
			}
			parameters = append(parameters, parameter)
		}
	}
	return parameters, nil
}

func (c *contract) content(response apiResponse) (map[string]any, error) {
	if response.events == nil {
		node, err := c.convert(response.body)
		return map[string]any{"application/json": map[string]any{"schema": node.schema}}, err
	}
	alternatives := make([]any, len(response.events))
	for i, event := range response.events {
		data := map[string]any{"type": "object"}
		if event.data != nil {
			node, err := c.convert(event.data)
			if err != nil {
				return nil, err
			}
			data = node.schema
		}
		alternatives[i] = map[string]any{"properties": map[string]any{
			"event": map[string]any{"const": event.name},
			"data":  map[string]any{"contentMediaType": "application/json", "contentSchema": data},
		}}
	}
	text := map[string]any{"type": "string"}
	return map[string]any{"text/event-stream": map[string]any{"itemSchema": map[string]any{
		"type":       "object",
		"required":   []string{"id", "event", "data"},
		"properties": map[string]any{"id": text, "event": text, "data": text},
		"oneOf":      alternatives,
	}}}, nil
}

func (c *contract) openAPI(routes []apiRoute) ([]byte, error) {
	paths := map[string]any{}
	for _, route := range routes {
		path := strings.TrimPrefix(route.pattern, route.method+" ")
		operation := map[string]any{}
		parameters, err := operationParameters(route, path)
		if err != nil {
			return nil, err
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		responses := map[string]any{}
		for _, response := range route.responses {
			content, err := c.content(response)
			if err != nil {
				return nil, fmt.Errorf("%s response %d: %w", route.pattern, response.status, err)
			}
			responses[strconv.Itoa(response.status)] = map[string]any{"description": http.StatusText(response.status), "content": content}
		}
		operation["responses"] = responses
		item, ok := paths[path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[path] = item
		}
		item[strings.ToLower(route.method)] = operation
	}
	out, err := json.MarshalIndent(map[string]any{
		"openapi":    "3.2.1",
		"info":       map[string]any{"title": "Sortie JSON API", "version": "v1"},
		"paths":      paths,
		"components": map[string]any{"schemas": c.schemas()},
	}, "", "  ")
	return append(out, '\n'), err
}

func TestAPIContract(t *testing.T) {
	declared := slices.Sorted(slices.Values(declaredHoldReasons(t)))
	if served := slices.Sorted(slices.Values(holdReasonValues())); !slices.Equal(served, declared) {
		t.Fatalf("sorted HoldReasons() = %v, want the SkipReason constants less SkipNone and SkipIneligible, each once: %v", served, declared)
	}

	registered, err := registeredRoutes("server.go")
	if err != nil {
		t.Fatalf("registeredRoutes: %v", err)
	}
	described := make([]string, len(apiRoutes))
	for i, route := range apiRoutes {
		described[i] = route.pattern
	}
	missing, extra := absentFrom(registered, described), absentFrom(described, registered)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("server.go and apiRoutes disagree: registered without an apiRoutes entry: %q; apiRoutes entries without a registration: %q", missing, extra)
	}

	c, err := newContract(contractRoots(apiRoutes)...)
	if err != nil {
		t.Fatalf("newContract: %v", err)
	}
	document, err := c.openAPI(apiRoutes)
	if err != nil {
		t.Fatalf("openAPI: %v", err)
	}

	files := map[string][]byte{"../../web/src/api/types.gen.ts": c.typescript(), "../../docs/openapi.json": document}
	for path, want := range files {
		if *updateContract {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatalf("WriteFile(%q): %v", path, err)
			}
			continue
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run go test ./internal/server -run TestAPIContract -update", path)
		}
	}
}

func TestAPIContractRejectsUnmappableTypes(t *testing.T) {
	t.Parallel()

	type left struct{ X int }
	type right struct{ X int }
	type collides struct {
		left
		right
	}
	type quoted struct {
		N int `json:"n,string"`
	}
	type foo struct{}
	type Foo struct{}

	tests := map[string][]reflect.Type{
		"json marshaler":             {reflect.TypeFor[json.RawMessage]()},
		"text marshaler":             {reflect.TypeFor[netip.Addr]()},
		"unnamed struct":             {reflect.TypeFor[struct{ A int }]()},
		"string tag option":          {reflect.TypeFor[quoted]()},
		"non-string map key":         {reflect.TypeFor[map[int]string]()},
		"same-depth field collision": {reflect.TypeFor[collides]()},
		"one TypeScript name":        {reflect.TypeFor[foo](), reflect.TypeFor[Foo]()},
	}

	for name, roots := range tests {
		if _, err := newContract(roots...); err == nil {
			t.Errorf("newContract(%s) = nil error, want error", name)
		}
	}
}
