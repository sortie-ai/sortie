package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Provenance the generator is pinned to. Changing the pinned release means
// re-vendoring the assets under testdata/ and updating these two values.
const (
	upstreamTag       = "schema-v1.21.0"
	upstreamCommit    = "272bf799f35a258c6a4107a0410ed361e83683d3"
	pinnedWireVersion = 1

	generatorPackagePath = "github.com/sortie-ai/sortie/internal/agent/clientprotocol/schemagen"
)

// rootMethods is the fixed seed for the $ref closure: the agent methods the
// client is permitted to call plus the two client methods it implements.
var rootMethods = map[string]bool{
	"initialize":                 true,
	"session/load":               true,
	"session/new":                true,
	"session/prompt":             true,
	"session/resume":             true,
	"session/cancel":             true,
	"session/update":             true,
	"session/request_permission": true,
}

// Generate reads the pinned schema assets under assetsDir, verifies them
// against the provenance file's recorded digests, computes the $ref closure
// from the fixed root set, and returns the emitted Go source. It writes
// nothing itself; the caller decides the destination.
func Generate(assetsDir string) ([]byte, error) {
	provenance, err := parseProvenance(filepath.Join(assetsDir, "PROVENANCE.txt"))
	if err != nil {
		return nil, err
	}

	metaBytes, err := verifyAsset(assetsDir, "meta.json", provenance)
	if err != nil {
		return nil, err
	}
	schemaBytes, err := verifyAsset(assetsDir, "schema.json", provenance)
	if err != nil {
		return nil, err
	}

	var meta metaDoc
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}
	if meta.Version != pinnedWireVersion {
		return nil, fmt.Errorf("meta.json declares wire version %d, the generator is pinned to version %d", meta.Version, pinnedWireVersion)
	}

	var doc schemaDoc
	if err := json.Unmarshal(schemaBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse schema.json: %w", err)
	}

	g := &generator{defs: doc.Defs, meta: &meta}
	if err := g.computeClosure(); err != nil {
		return nil, err
	}

	src, err := g.emit()
	if err != nil {
		return nil, err
	}

	formatted, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w", err)
	}
	return formatted, nil
}

// provenanceEntry is one asset's recorded size and digest, read from
// PROVENANCE.txt.
type provenanceEntry struct {
	bytes  int64
	sha256 string
}

// provenanceLineRE matches a data line of PROVENANCE.txt:
// "<relative-file-path> <byte-count> sha256:<hex-digest>".
var provenanceLineRE = regexp.MustCompile(`^(\S+)\s+(\d+)\s+sha256:([0-9a-f]{64})$`)

func parseProvenance(path string) (map[string]provenanceEntry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the generator's own pinned assets directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	entries := map[string]provenanceEntry{}
	for line := range strings.SplitSeq(string(data), "\n") {
		m := provenanceLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid byte count %q", path, m[2])
		}
		entries[m[1]] = provenanceEntry{bytes: n, sha256: m[3]}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: no asset lines found", path)
	}
	return entries, nil
}

// verifyAsset reads filename from assetsDir and fails when its sha256
// disagrees with the digest the provenance file records for it.
func verifyAsset(assetsDir, filename string, provenance map[string]provenanceEntry) ([]byte, error) {
	entry, ok := provenance[filename]
	if !ok {
		return nil, fmt.Errorf("provenance file does not record %s", filename)
	}
	path := filepath.Join(assetsDir, filename)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the generator's own pinned assets directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	computed := hex.EncodeToString(sum[:])
	if computed != entry.sha256 {
		return nil, fmt.Errorf("%s: sha256 mismatch: provenance records %s, computed %s", filename, entry.sha256, computed)
	}
	return data, nil
}

// metaDoc mirrors the shape of the pinned meta.json.
type metaDoc struct {
	Version         int               `json:"version"`
	AgentMethods    map[string]string `json:"agentMethods"`
	ClientMethods   map[string]string `json:"clientMethods"`
	ProtocolMethods map[string]string `json:"protocolMethods"`
}

// schemaDoc is the pinned schema.json's top level: only $defs matters here.
type schemaDoc struct {
	Defs map[string]*schemaDef `json:"$defs"`
}

// discriminator mirrors a oneOf schema's "discriminator" keyword.
type discriminator struct {
	PropertyName string `json:"propertyName"`
}

// schemaDef models the subset of JSON Schema this pinned artifact uses. It
// is used both for the $defs entries and for every nested schema fragment
// (a property's schema, an array's items, a oneOf/anyOf/allOf member).
type schemaDef struct {
	Ref                  string                `json:"$ref,omitempty"`
	Title                string                `json:"title,omitempty"`
	Type                 json.RawMessage       `json:"type,omitempty"`
	Properties           map[string]*schemaDef `json:"properties,omitempty"`
	Required             []string              `json:"required,omitempty"`
	Items                *schemaDef            `json:"items,omitempty"`
	OneOf                []*schemaDef          `json:"oneOf,omitempty"`
	AnyOf                []*schemaDef          `json:"anyOf,omitempty"`
	AllOf                []*schemaDef          `json:"allOf,omitempty"`
	Const                json.RawMessage       `json:"const,omitempty"`
	Discriminator        *discriminator        `json:"discriminator,omitempty"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties,omitempty"`
	XMethod              string                `json:"x-method,omitempty"`
}

// typeList normalizes the "type" keyword, which the artifact writes as
// either a bare string or an array of strings.
func (d *schemaDef) typeList() []string {
	if d == nil || len(d.Type) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(d.Type, &single); err == nil {
		return []string{single}
	}
	var multi []string
	if err := json.Unmarshal(d.Type, &multi); err == nil {
		return multi
	}
	return nil
}

// constString reads a string "const" value, reporting whether one is
// present.
func (d *schemaDef) constString() (string, bool) {
	if d == nil || len(d.Const) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(d.Const, &s); err != nil {
		return "", false
	}
	return s, true
}

// resolveSingleRef reports the definition name a schema resolves to when it
// is exactly a $ref, or an allOf of exactly one $ref. It reports false for
// every other shape.
func resolveSingleRef(d *schemaDef) (string, bool) {
	if d == nil {
		return "", false
	}
	if d.Ref != "" {
		return refName(d.Ref), true
	}
	if len(d.AllOf) == 1 && d.AllOf[0] != nil && d.AllOf[0].Ref != "" {
		return refName(d.AllOf[0].Ref), true
	}
	return "", false
}

func refName(ref string) string {
	i := strings.LastIndex(ref, "/")
	if i < 0 {
		return ref
	}
	return ref[i+1:]
}

func isNullSchema(d *schemaDef) bool {
	types := d.typeList()
	return len(types) == 1 && types[0] == "null"
}

// parseAdditionalProperties reads the "additionalProperties" keyword, which
// the artifact writes either as the boolean true (an arbitrary-value
// container, as _meta uses it) or as a schema constraining every additional
// value (a map type).
func parseAdditionalProperties(raw json.RawMessage) (arbitrary bool, sub *schemaDef, err error) {
	var b bool
	if uerr := json.Unmarshal(raw, &b); uerr == nil {
		return b, nil, nil
	}
	var s schemaDef
	if uerr := json.Unmarshal(raw, &s); uerr != nil {
		return false, nil, fmt.Errorf("parse additionalProperties: %w", uerr)
	}
	return false, &s, nil
}

// generator holds the parsed schema and the definitions the closure reached.
type generator struct {
	defs    map[string]*schemaDef
	meta    *metaDoc
	closure map[string]bool
}

// computeClosure seeds the closure from every definition whose x-method
// names one of rootMethods, then follows every $ref reachable from there,
// so no definition outside the reachable closure is emitted.
func (g *generator) computeClosure() error {
	seen := map[string]bool{}
	var frontier []string

	var names []string
	for name := range g.defs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if def := g.defs[name]; def.XMethod != "" && rootMethods[def.XMethod] {
			seen[name] = true
			frontier = append(frontier, name)
		}
	}
	if len(frontier) == 0 {
		return errors.New("no definition in schema.json declared an x-method in the root set")
	}

	for len(frontier) > 0 {
		var next []string
		for _, name := range frontier {
			refs := map[string]bool{}
			collectRefs(g.defs[name], refs)
			var refNames []string
			for r := range refs {
				refNames = append(refNames, r)
			}
			sort.Strings(refNames)
			for _, r := range refNames {
				if seen[r] {
					continue
				}
				if _, ok := g.defs[r]; !ok {
					return fmt.Errorf("definition %q references undefined %q", name, r)
				}
				seen[r] = true
				next = append(next, r)
			}
		}
		frontier = next
	}

	g.closure = seen
	return nil
}

// collectRefs walks every nested schema fragment reachable from def and
// records the definition name of each $ref found, directly or through a
// one-element allOf.
func collectRefs(def *schemaDef, out map[string]bool) {
	if def == nil {
		return
	}
	if def.Ref != "" {
		out[refName(def.Ref)] = true
	}
	for _, p := range def.Properties {
		collectRefs(p, out)
	}
	collectRefs(def.Items, out)
	for _, m := range def.OneOf {
		collectRefs(m, out)
	}
	for _, m := range def.AnyOf {
		collectRefs(m, out)
	}
	for _, m := range def.AllOf {
		collectRefs(m, out)
	}
}

// formKind classifies a definition's shape into the forms the emitter
// handles.
type formKind int

const (
	formUnknown formKind = iota
	formObject
	formStringConstEnum
	formOpenStringEnum
	formStringAlias
	formIntegerAlias
	formDiscUnion
	formNonDiscUnion
)

func classify(d *schemaDef) formKind {
	if len(d.OneOf) > 0 {
		if d.Discriminator != nil {
			return formDiscUnion
		}
		return formStringConstEnum
	}
	if len(d.AnyOf) > 0 {
		allString := true
		for _, m := range d.AnyOf {
			types := m.typeList()
			if len(types) != 1 || types[0] != "string" {
				allString = false
				break
			}
		}
		if allString {
			return formOpenStringEnum
		}
		return formNonDiscUnion
	}
	types := d.typeList()
	if len(d.Properties) > 0 || slices.Contains(types, "object") {
		return formObject
	}
	if slices.Contains(types, "string") {
		return formStringAlias
	}
	if slices.Contains(types, "integer") {
		return formIntegerAlias
	}
	return formUnknown
}

// goTypeName maps a definition name to the Go type name emitted for it: the
// definition name with its first rune lowercased, which the mapping table
// records for every closure member.
func goTypeName(defName string) string {
	if defName == "" {
		return defName
	}
	name := strings.ToLower(defName[:1]) + defName[1:]
	if goKeywords[name] {
		name += "_"
	}
	return name
}

var goKeywords = map[string]bool{
	"break": true, "default": true, "func": true, "interface": true, "select": true,
	"case": true, "defer": true, "go": true, "map": true, "struct": true,
	"chan": true, "else": true, "goto": true, "package": true, "switch": true,
	"const": true, "fallthrough": true, "if": true, "range": true, "type": true,
	"continue": true, "for": true, "import": true, "return": true, "var": true,
}

// initialisms holds the acronyms this schema's property names spell out, so
// goFieldName capitalizes them the way Go style expects (SessionID, not
// SessionId).
var initialisms = map[string]bool{
	"ID": true, "URL": true, "URI": true, "HTTP": true, "HTTPS": true,
	"SSE": true, "MCP": true,
}

var wordRE = regexp.MustCompile(`[A-Z]+[a-z0-9]*|[a-z0-9]+`)

// goFieldName maps a JSON property name (or a union member's title or const
// value) to an exported Go field name.
func goFieldName(name string) string {
	if name == "_meta" {
		return "Meta"
	}
	words := wordRE.FindAllString(name, -1)
	var sb strings.Builder
	for _, w := range words {
		upper := strings.ToUpper(w)
		if initialisms[upper] {
			sb.WriteString(upper)
			continue
		}
		sb.WriteString(strings.ToUpper(w[:1]))
		sb.WriteString(strings.ToLower(w[1:]))
	}
	if sb.Len() == 0 {
		return "Field"
	}
	return sb.String()
}

// pascalFromSnake maps a snake_case wire value (an enum const or a
// meta.json method key) to a PascalCase identifier fragment.
func pascalFromSnake(s string) string {
	var sb strings.Builder
	for p := range strings.SplitSeq(s, "_") {
		if p == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(p[:1]))
		sb.WriteString(strings.ToLower(p[1:]))
	}
	return sb.String()
}

// emit renders every closure member plus the mapping table and the method
// constants into one Go source file.
func (g *generator) emit() ([]byte, error) {
	var names []string
	for name := range g.closure {
		names = append(names, name)
	}
	sort.Strings(names)

	var body strings.Builder
	typeByDef := make(map[string]string, len(names))

	for _, name := range names {
		def := g.defs[name]
		typeByDef[name] = goTypeName(name)

		var err error
		switch classify(def) {
		case formStringConstEnum:
			err = g.emitStringConstEnum(&body, name, def)
		case formOpenStringEnum:
			g.emitOpenStringEnum(&body, name, def)
		case formStringAlias:
			typeName := goTypeName(name)
			fmt.Fprintf(&body, "// %s is generated from the %s string definition of the pinned schema.\ntype %s string\n\n", typeName, name, typeName)
		case formIntegerAlias:
			typeName := goTypeName(name)
			fmt.Fprintf(&body, "// %s is generated from the %s integer definition of the pinned schema.\ntype %s int64\n\n", typeName, name, typeName)
		case formObject:
			err = g.emitObject(&body, name, def)
		case formDiscUnion:
			err = g.emitDiscUnion(&body, name, def)
		case formNonDiscUnion:
			err = g.emitNonDiscUnion(&body, name, def)
		default:
			err = fmt.Errorf("definition %q has a schema form the generator does not recognize", name)
		}
		if err != nil {
			return nil, err
		}
	}

	body.WriteString("// marshalWithDiscriminant marshals v and injects the key/value pair that\n")
	body.WriteString("// a non-discriminated union member's wrapper schema adds beside it.\n")
	body.WriteString("func marshalWithDiscriminant(v interface{}, key, value string) ([]byte, error) {\n")
	body.WriteString("\tbody, err := json.Marshal(v)\n")
	body.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	body.WriteString("\tvar fields map[string]json.RawMessage\n")
	body.WriteString("\tif err := json.Unmarshal(body, &fields); err != nil {\n\t\treturn nil, err\n\t}\n")
	body.WriteString("\tencodedValue, err := json.Marshal(value)\n")
	body.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	body.WriteString("\tfields[key] = encodedValue\n")
	body.WriteString("\treturn json.Marshal(fields)\n")
	body.WriteString("}\n\n")

	g.emitMappingTable(&body, names, typeByDef)
	if err := g.emitMethodConstants(&body); err != nil {
		return nil, err
	}

	var out strings.Builder
	fmt.Fprintf(&out, "// Code generated by %s from the Agent Client Protocol schema tag %s (commit %s). DO NOT EDIT.\n\n", generatorPackagePath, upstreamTag, upstreamCommit)
	out.WriteString("package clientprotocol\n\n")
	out.WriteString("import (\n\t\"encoding/json\"\n\t\"fmt\"\n)\n\n")
	out.WriteString(body.String())
	return []byte(out.String()), nil
}

func (g *generator) emitStringConstEnum(sb *strings.Builder, name string, def *schemaDef) error {
	typeName := goTypeName(name)
	fmt.Fprintf(sb, "// %s is generated from the %s enumeration of the pinned schema.\ntype %s string\n\n", typeName, name, typeName)
	sb.WriteString("const (\n")
	for _, m := range def.OneOf {
		v, ok := m.constString()
		if !ok {
			return fmt.Errorf("%s: enumeration member has no const value", name)
		}
		fmt.Fprintf(sb, "\t%s%s %s = %q\n", typeName, pascalFromSnake(v), typeName, v)
	}
	sb.WriteString(")\n\n")
	return nil
}

// emitOpenStringEnum handles an anyOf whose members are all string schemas
// but where one member carries no const, accepting any value the closed
// members do not name.
func (g *generator) emitOpenStringEnum(sb *strings.Builder, name string, def *schemaDef) {
	typeName := goTypeName(name)
	fmt.Fprintf(sb, "// %s is generated from the %s open-ended string enumeration of\n// the pinned schema; the artifact reserves an uncategorized value that\n// this underlying-string type accepts without a matching constant.\ntype %s string\n\n", typeName, name, typeName)
	var consts []string
	for _, m := range def.AnyOf {
		if v, ok := m.constString(); ok {
			consts = append(consts, v)
		}
	}
	if len(consts) == 0 {
		return
	}
	sb.WriteString("const (\n")
	for _, v := range consts {
		fmt.Fprintf(sb, "\t%s%s %s = %q\n", typeName, pascalFromSnake(v), typeName, v)
	}
	sb.WriteString(")\n\n")
}

func (g *generator) emitObject(sb *strings.Builder, name string, def *schemaDef) error {
	typeName := goTypeName(name)
	required := map[string]bool{}
	for _, r := range def.Required {
		required[r] = true
	}
	var propNames []string
	for p := range def.Properties {
		propNames = append(propNames, p)
	}
	sort.Strings(propNames)

	fmt.Fprintf(sb, "// %s is generated from the %s definition of the pinned schema.\ntype %s struct {\n", typeName, name, typeName)
	for _, p := range propNames {
		if p == "_meta" {
			sb.WriteString("\tMeta json.RawMessage `json:\"_meta,omitempty\"`\n")
			continue
		}
		goType, err := g.goType(def.Properties[p])
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, p, err)
		}
		fieldName := goFieldName(p)
		if required[p] {
			fmt.Fprintf(sb, "\t%s %s `json:%q`\n", fieldName, goType, p)
		} else {
			fmt.Fprintf(sb, "\t%s *%s `json:%q`\n", fieldName, goType, p+",omitempty")
		}
	}
	sb.WriteString("}\n\n")
	return nil
}

// goType resolves a property's schema to a Go type expression: a single
// $ref (directly or through a one-element allOf), a nullable anyOf of one
// such ref and null, an array, a map (an additionalProperties schema), an
// arbitrary value (additionalProperties true, or a schema naming no type at
// all), or a primitive.
func (g *generator) goType(prop *schemaDef) (string, error) {
	if name, ok := resolveSingleRef(prop); ok {
		if !g.closure[name] {
			return "", fmt.Errorf("references %q outside the generator's closure", name)
		}
		return goTypeName(name), nil
	}
	if len(prop.AnyOf) == 2 {
		for i, m := range prop.AnyOf {
			if isNullSchema(prop.AnyOf[1-i]) {
				if name, ok := resolveSingleRef(m); ok {
					if !g.closure[name] {
						return "", fmt.Errorf("references %q outside the generator's closure", name)
					}
					return goTypeName(name), nil
				}
			}
		}
		return "", errors.New("unsupported anyOf property shape")
	}
	if len(prop.AdditionalProperties) > 0 {
		arbitrary, sub, err := parseAdditionalProperties(prop.AdditionalProperties)
		if err != nil {
			return "", err
		}
		if arbitrary {
			return "json.RawMessage", nil
		}
		if sub == nil {
			return "", errors.New("additionalProperties: false is not a supported property shape")
		}
		valType, err := g.goType(sub)
		if err != nil {
			return "", err
		}
		return "map[string]" + valType, nil
	}
	types := prop.typeList()
	switch {
	case slices.Contains(types, "array"):
		if prop.Items == nil {
			return "", errors.New("array property declares no items schema")
		}
		itemType, err := g.goType(prop.Items)
		if err != nil {
			return "", err
		}
		return "[]" + itemType, nil
	case slices.Contains(types, "string"):
		return "string", nil
	case slices.Contains(types, "boolean"):
		return "bool", nil
	case slices.Contains(types, "integer"):
		return "int64", nil
	case slices.Contains(types, "number"):
		return "float64", nil
	case len(types) == 0:
		return "json.RawMessage", nil
	}
	return "", fmt.Errorf("unsupported property shape (types=%v)", types)
}

func (g *generator) emitMappingTable(sb *strings.Builder, names []string, typeByDef map[string]string) {
	sb.WriteString("// wireTypeByDefinition maps each pinned schema definition this generator\n")
	sb.WriteString("// reached to the Go type emitted for it.\n")
	sb.WriteString("var wireTypeByDefinition = map[string]string{\n")
	for _, name := range names {
		fmt.Fprintf(sb, "\t%q: %q,\n", name, typeByDef[name])
	}
	sb.WriteString("}\n\n")
}

func (g *generator) emitMethodConstants(sb *strings.Builder) error {
	type entry struct{ key, value string }
	var entries []entry
	for k, v := range g.meta.AgentMethods {
		entries = append(entries, entry{k, v})
	}
	for k, v := range g.meta.ClientMethods {
		entries = append(entries, entry{k, v})
	}
	for k, v := range g.meta.ProtocolMethods {
		entries = append(entries, entry{k, v})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	sb.WriteString("// Method-name constants generated from the pinned method index (meta.json).\n")
	sb.WriteString("const (\n")
	seen := map[string]bool{}
	for _, e := range entries {
		ident := "method" + pascalFromSnake(e.key)
		if seen[ident] {
			return fmt.Errorf("method constant %q collides across meta.json's method groups", ident)
		}
		seen[ident] = true
		fmt.Fprintf(sb, "\t%s = %q\n", ident, e.value)
	}
	sb.WriteString(")\n\n")
	return nil
}

// analyzedMember is what emitNonDiscUnion learns about one anyOf member
// before choosing a detection strategy and rendering a template.
type analyzedMember struct {
	FieldName string
	ElemType  string
	IsSlice   bool
	DiscProp  string
	DiscValue string
	Required  []string
}

// FieldType is the Go type of the wrapper struct's field for this member.
func (m analyzedMember) FieldType() string {
	if m.IsSlice {
		return "[]" + m.ElemType
	}
	return "*" + m.ElemType
}

func (g *generator) analyzeMember(m *schemaDef) (analyzedMember, error) {
	var am analyzedMember

	var propNames []string
	for p := range m.Properties {
		propNames = append(propNames, p)
	}
	sort.Strings(propNames)
	for _, p := range propNames {
		if v, ok := m.Properties[p].constString(); ok {
			am.DiscProp = p
			am.DiscValue = v
			break
		}
	}

	if slices.Contains(m.typeList(), "array") {
		if m.Items == nil {
			return am, errors.New("array union member declares no items schema")
		}
		name, ok := resolveSingleRef(m.Items)
		if !ok {
			return am, errors.New("array union member's items is not a single reference")
		}
		if !g.closure[name] {
			return am, fmt.Errorf("references %q outside the generator's closure", name)
		}
		am.IsSlice = true
		am.ElemType = goTypeName(name)
		am.Required = g.defs[name].Required
	} else {
		name, ok := resolveSingleRef(m)
		if !ok {
			return am, errors.New("union member is not a single reference")
		}
		if !g.closure[name] {
			return am, fmt.Errorf("references %q outside the generator's closure", name)
		}
		am.ElemType = goTypeName(name)
		am.Required = g.defs[name].Required
	}

	switch {
	case m.Title != "":
		am.FieldName = goFieldName(m.Title)
	case am.DiscValue != "":
		am.FieldName = goFieldName(am.DiscValue)
	default:
		am.FieldName = goFieldName(am.ElemType)
	}
	return am, nil
}

func allArray(members []analyzedMember) bool {
	for _, m := range members {
		if !m.IsSlice {
			return false
		}
	}
	return true
}

// assignUniqueKeys finds, for each member, a required property name absent
// from every sibling member's own required set, used to tell members apart
// when the artifact declares no discriminator at all.
func assignUniqueKeys(members []analyzedMember) []string {
	keys := make([]string, len(members))
	for i := range members {
		for _, k := range members[i].Required {
			unique := true
			for j := range members {
				if j != i && slices.Contains(members[j].Required, k) {
					unique = false
					break
				}
			}
			if unique {
				keys[i] = k
				break
			}
		}
	}
	return keys
}

func (g *generator) emitNonDiscUnion(sb *strings.Builder, name string, def *schemaDef) error {
	typeName := goTypeName(name)
	members := make([]analyzedMember, 0, len(def.AnyOf))
	for _, m := range def.AnyOf {
		am, err := g.analyzeMember(m)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		members = append(members, am)
	}

	discProp := ""
	numDisc := 0
	for _, m := range members {
		if m.DiscValue != "" {
			numDisc++
			if discProp == "" {
				discProp = m.DiscProp
			} else if discProp != m.DiscProp {
				return fmt.Errorf("%s: union members disagree on their discriminant property name", name)
			}
		}
	}

	switch {
	case numDisc > 0:
		return g.emitConstBasedUnion(sb, name, typeName, discProp, members)
	case len(members) == 1:
		return trivialUnionTmpl.Execute(sb, struct {
			TypeName string
			DefName  string
			Member   analyzedMember
		}{typeName, name, members[0]})
	case allArray(members):
		return g.emitKeyedUnion(sb, name, typeName, members, arrayElementUnionTmpl)
	default:
		return g.emitKeyedUnion(sb, name, typeName, members, requiredPropUnionTmpl)
	}
}

func (g *generator) emitConstBasedUnion(sb *strings.Builder, name, typeName, discProp string, members []analyzedMember) error {
	data := struct {
		TypeName      string
		DefName       string
		DiscProp      string
		Members       []analyzedMember
		ConstMembers  []analyzedMember
		DefaultMember *analyzedMember
	}{TypeName: typeName, DefName: name, DiscProp: discProp, Members: members}

	for i := range members {
		if members[i].DiscValue == "" {
			if data.DefaultMember != nil {
				return fmt.Errorf("%s: more than one union member lacks a discriminant value", name)
			}
			data.DefaultMember = &members[i]
		} else {
			data.ConstMembers = append(data.ConstMembers, members[i])
		}
	}
	return constBasedUnionTmpl.Execute(sb, data)
}

type keyedMember struct {
	analyzedMember
	Key string
}

func (g *generator) emitKeyedUnion(sb *strings.Builder, name, typeName string, members []analyzedMember, tmpl *template.Template) error {
	keys := assignUniqueKeys(members)
	last := len(members) - 1

	data := struct {
		TypeName      string
		DefName       string
		Members       []analyzedMember
		KeyedMembers  []keyedMember
		DefaultMember analyzedMember
	}{TypeName: typeName, DefName: name, Members: members, DefaultMember: members[last]}

	for i := range last {
		if keys[i] == "" {
			return fmt.Errorf("%s: member %d has no property distinguishing it from its siblings", name, i)
		}
		data.KeyedMembers = append(data.KeyedMembers, keyedMember{members[i], keys[i]})
	}
	return tmpl.Execute(sb, data)
}

func (g *generator) emitDiscUnion(sb *strings.Builder, name string, def *schemaDef) error {
	typeName := goTypeName(name)
	propName := def.Discriminator.PropertyName
	fieldName := goFieldName(propName)

	type constEntry struct{ Ident, Value string }
	var consts []constEntry
	for _, m := range def.OneOf {
		propSchema, ok := m.Properties[propName]
		if !ok {
			continue
		}
		if v, ok := propSchema.constString(); ok {
			consts = append(consts, constEntry{typeName + pascalFromSnake(v), v})
		}
	}

	return discUnionTmpl.Execute(sb, struct {
		TypeName  string
		DefName   string
		FieldName string
		PropName  string
		Consts    []constEntry
	}{typeName, name, fieldName, propName, consts})
}

var discUnionTmpl = template.Must(template.New("discUnion").Parse(`// {{.TypeName}} is generated from the {{.DefName}} discriminated union of
// the pinned schema; a variant published after the pin is carried in
// Remainder rather than refused.
type {{.TypeName}} struct {
	{{.FieldName}} string ` + "`" + `json:"{{.PropName}}"` + "`" + `
	Remainder json.RawMessage ` + "`" + `json:"-"` + "`" + `
}

func (v *{{.TypeName}}) UnmarshalJSON(data []byte) error {
	var probe struct {
		Value string ` + "`" + `json:"{{.PropName}}"` + "`" + `
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("{{.TypeName}}: %w", err)
	}
	v.{{.FieldName}} = probe.Value
	v.Remainder = append(json.RawMessage(nil), data...)
	return nil
}

func (v {{.TypeName}}) MarshalJSON() ([]byte, error) {
	if v.Remainder != nil {
		return v.Remainder, nil
	}
	return nil, fmt.Errorf("{{.TypeName}}: no decoded payload to re-encode")
}
{{if .Consts}}
const (
{{range .Consts}}	{{.Ident}} = "{{.Value}}"
{{end}})
{{end}}
`))

var constBasedUnionTmpl = template.Must(template.New("constBasedUnion").Parse(`// {{.TypeName}} is generated from the {{.DefName}} union of the pinned
// schema, which declares no discriminator; the marshaler writes exactly the
// member the caller set.
type {{.TypeName}} struct {
{{range .Members}}	{{.FieldName}} {{.FieldType}} ` + "`" + `json:"-"` + "`" + `
{{end}}}

func (v *{{.TypeName}}) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type *string ` + "`" + `json:"{{.DiscProp}}"` + "`" + `
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("{{.TypeName}}: %w", err)
	}
	switch {
{{range .ConstMembers}}	case probe.Type != nil && *probe.Type == "{{.DiscValue}}":
		v.{{.FieldName}} = new({{.ElemType}})
		return json.Unmarshal(data, v.{{.FieldName}})
{{end}}{{if .DefaultMember}}	default:
		v.{{.DefaultMember.FieldName}} = new({{.DefaultMember.ElemType}})
		return json.Unmarshal(data, v.{{.DefaultMember.FieldName}})
{{else}}	default:
		return fmt.Errorf("{{.TypeName}}: unrecognized member")
{{end}}	}
}

func (v {{.TypeName}}) MarshalJSON() ([]byte, error) {
	switch {
{{range .ConstMembers}}	case v.{{.FieldName}} != nil:
		return marshalWithDiscriminant(v.{{.FieldName}}, "{{$.DiscProp}}", "{{.DiscValue}}")
{{end}}{{if .DefaultMember}}	case v.{{.DefaultMember.FieldName}} != nil:
		return json.Marshal(v.{{.DefaultMember.FieldName}})
{{end}}	default:
		return nil, fmt.Errorf("{{.TypeName}}: no member set")
	}
}
`))

var requiredPropUnionTmpl = template.Must(template.New("requiredPropUnion").Parse(`// {{.TypeName}} is generated from the {{.DefName}} union of the pinned
// schema, which declares no discriminator; the marshaler writes exactly the
// member the caller set.
type {{.TypeName}} struct {
{{range .Members}}	{{.FieldName}} {{.FieldType}} ` + "`" + `json:"-"` + "`" + `
{{end}}}

func (v *{{.TypeName}}) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("{{.TypeName}}: %w", err)
	}
	switch {
{{range .KeyedMembers}}	case probe["{{.Key}}"] != nil:
		v.{{.FieldName}} = new({{.ElemType}})
		return json.Unmarshal(data, v.{{.FieldName}})
{{end}}	default:
		v.{{.DefaultMember.FieldName}} = new({{.DefaultMember.ElemType}})
		return json.Unmarshal(data, v.{{.DefaultMember.FieldName}})
	}
}

func (v {{.TypeName}}) MarshalJSON() ([]byte, error) {
	switch {
{{range .Members}}	case v.{{.FieldName}} != nil:
		return json.Marshal(v.{{.FieldName}})
{{end}}	default:
		return nil, fmt.Errorf("{{.TypeName}}: no member set")
	}
}
`))

var arrayElementUnionTmpl = template.Must(template.New("arrayElementUnion").Parse(`// {{.TypeName}} is generated from the {{.DefName}} union of the pinned
// schema, whose members are all arrays and which declares no discriminator;
// the marshaler writes exactly the member the caller set.
type {{.TypeName}} struct {
{{range .Members}}	{{.FieldName}} {{.FieldType}} ` + "`" + `json:"-"` + "`" + `
{{end}}}

func (v *{{.TypeName}}) UnmarshalJSON(data []byte) error {
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return fmt.Errorf("{{.TypeName}}: %w", err)
	}
	var probe map[string]json.RawMessage
	if len(elems) > 0 {
		_ = json.Unmarshal(elems[0], &probe)
	}
	switch {
{{range .KeyedMembers}}	case probe["{{.Key}}"] != nil:
		return json.Unmarshal(data, &v.{{.FieldName}})
{{end}}	default:
		return json.Unmarshal(data, &v.{{.DefaultMember.FieldName}})
	}
}

func (v {{.TypeName}}) MarshalJSON() ([]byte, error) {
	switch {
{{range .Members}}	case v.{{.FieldName}} != nil:
		return json.Marshal(v.{{.FieldName}})
{{end}}	default:
		return nil, fmt.Errorf("{{.TypeName}}: no member is set")
	}
}
`))

var trivialUnionTmpl = template.Must(template.New("trivialUnion").Parse(`// {{.TypeName}} is generated from the {{.DefName}} union of the pinned
// schema, which declares a single member and therefore needs no
// discriminant to decode.
type {{.TypeName}} struct {
	{{.Member.FieldName}} {{.Member.FieldType}} ` + "`" + `json:"-"` + "`" + `
}

func (v *{{.TypeName}}) UnmarshalJSON(data []byte) error {
	v.{{.Member.FieldName}} = new({{.Member.ElemType}})
	return json.Unmarshal(data, v.{{.Member.FieldName}})
}

func (v {{.TypeName}}) MarshalJSON() ([]byte, error) {
	if v.{{.Member.FieldName}} != nil {
		return json.Marshal(v.{{.Member.FieldName}})
	}
	return nil, fmt.Errorf("{{.TypeName}}: no member set")
}
`))
