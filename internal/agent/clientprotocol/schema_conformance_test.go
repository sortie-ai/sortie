package clientprotocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// This file validates the generated wire types, the messages the
// adapter itself writes (strict direction), and the messages the
// adapter reads from captured fixtures (weak direction) against
// the pinned schema artifact under testdata/schema-v1.21.0/. It runs
// without executing the generator: it reads wire_gen.go's own
// definition-to-Go-type mapping table rather than restating it.

// --- Pinned artifact loading and provenance ---

// schemaAssetsDir is the directory holding the pinned schema artifact
// and its provenance file, relative to this package's own directory.
const schemaAssetsDir = "testdata/schema-v1.21.0"

// assertProvenance asserts that every asset line PROVENANCE.txt records
// (the form "<path> <byte-count> sha256:<hex>") matches the file it
// names, both in byte count and in sha256, so a copy in the tree cannot
// silently drift from the release it claims to be. It fails unless
// exactly two asset lines were found and checked, matching the two
// pinned assets this directory vendors.
func assertProvenance(t *testing.T, dir string) {
	t.Helper()

	provPath := filepath.Join(dir, "PROVENANCE.txt")
	data, err := os.ReadFile(provPath) //nolint:gosec // G304: dir is a fixed test-owned path
	if err != nil {
		t.Fatalf("read %s: %v", provPath, err)
	}

	var checked int
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "sha256:") {
			continue
		}
		assetName, wantBytesStr, digestField := fields[0], fields[1], fields[2]
		wantBytes, err := strconv.Atoi(wantBytesStr)
		if err != nil {
			continue
		}
		wantDigest := strings.TrimPrefix(digestField, "sha256:")

		assetPath := filepath.Join(dir, assetName)
		assetData, err := os.ReadFile(assetPath) //nolint:gosec // G304: assetName is read from the test's own provenance file
		if err != nil {
			t.Errorf("read pinned asset %s: %v", assetPath, err)
			continue
		}
		checked++

		if len(assetData) != wantBytes {
			t.Errorf("len(%s) = %d, want %d (per %s)", assetPath, len(assetData), wantBytes, provPath)
		}
		sum := sha256.Sum256(assetData)
		gotDigest := hex.EncodeToString(sum[:])
		if gotDigest != wantDigest {
			t.Errorf("sha256(%s) = %s, want %s (per %s)", assetPath, gotDigest, wantDigest, provPath)
		}
	}

	if checked != 2 {
		t.Fatalf("checked %d asset line(s) in %s, want 2 (schema.json, meta.json)", checked, provPath)
	}
}

// loadSchemaDefs reads and decodes the pinned schema artifact and
// returns its top-level $defs object, the map every definition name in
// wire_gen.go's wireTypeByDefinition table resolves through.
func loadSchemaDefs(t *testing.T, dir string) map[string]any {
	t.Helper()

	path := filepath.Join(dir, "schema.json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: dir is a fixed test-owned path
	if err != nil {
		t.Fatalf("read pinned schema %s: %v", path, err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode pinned schema %s: %v", path, err)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("pinned schema %s has no top-level $defs object", path)
	}
	return defs
}

// definition looks up name in defs, reporting its presence.
func definition(defs map[string]any, name string) (map[string]any, bool) {
	d, ok := defs[name].(map[string]any)
	return d, ok
}

// --- Generated struct fields vs. the pinned definitions ---

// goTypeRegistry maps every Go type name wire_gen.go's
// wireTypeByDefinition table names for a plain-object-form definition
// to its reflect.Type, so the test can inspect that type's json tags.
// Go has no way to resolve a type by name string at runtime other than
// a literal reference, so this table is necessary scaffolding rather
// than a restatement of the generator's own mapping: the mapping this
// test consumes is wireTypeByDefinition itself, read directly from
// wire_gen.go below.
var goTypeRegistry = map[string]reflect.Type{
	"agentAuthCapabilities":                    reflect.TypeFor[agentAuthCapabilities](),
	"agentCapabilities":                        reflect.TypeFor[agentCapabilities](),
	"annotations":                              reflect.TypeFor[annotations](),
	"audioContent":                             reflect.TypeFor[audioContent](),
	"authCapabilities":                         reflect.TypeFor[authCapabilities](),
	"authMethodAgent":                          reflect.TypeFor[authMethodAgent](),
	"authMethodTerminal":                       reflect.TypeFor[authMethodTerminal](),
	"availableCommand":                         reflect.TypeFor[availableCommand](),
	"availableCommandsUpdate":                  reflect.TypeFor[availableCommandsUpdate](),
	"blobResourceContents":                     reflect.TypeFor[blobResourceContents](),
	"booleanConfigOptionCapabilities":          reflect.TypeFor[booleanConfigOptionCapabilities](),
	"cancelNotification":                       reflect.TypeFor[cancelNotification](),
	"clientCapabilities":                       reflect.TypeFor[clientCapabilities](),
	"clientSessionCapabilities":                reflect.TypeFor[clientSessionCapabilities](),
	"configOptionUpdate":                       reflect.TypeFor[configOptionUpdate](),
	"content":                                  reflect.TypeFor[content](),
	"contentChunk":                             reflect.TypeFor[contentChunk](),
	"cost":                                     reflect.TypeFor[cost](),
	"currentModeUpdate":                        reflect.TypeFor[currentModeUpdate](),
	"diff":                                     reflect.TypeFor[diff](),
	"elicitationCapabilities":                  reflect.TypeFor[elicitationCapabilities](),
	"elicitationFormCapabilities":              reflect.TypeFor[elicitationFormCapabilities](),
	"elicitationUrlCapabilities":               reflect.TypeFor[elicitationUrlCapabilities](),
	"embeddedResource":                         reflect.TypeFor[embeddedResource](),
	"envVariable":                              reflect.TypeFor[envVariable](),
	"fileSystemCapabilities":                   reflect.TypeFor[fileSystemCapabilities](),
	"httpHeader":                               reflect.TypeFor[httpHeader](),
	"imageContent":                             reflect.TypeFor[imageContent](),
	"implementation":                           reflect.TypeFor[implementation](),
	"initializeRequest":                        reflect.TypeFor[initializeRequest](),
	"initializeResponse":                       reflect.TypeFor[initializeResponse](),
	"loadSessionRequest":                       reflect.TypeFor[loadSessionRequest](),
	"loadSessionResponse":                      reflect.TypeFor[loadSessionResponse](),
	"logoutCapabilities":                       reflect.TypeFor[logoutCapabilities](),
	"mcpCapabilities":                          reflect.TypeFor[mcpCapabilities](),
	"mcpServerHttp":                            reflect.TypeFor[mcpServerHttp](),
	"mcpServerSse":                             reflect.TypeFor[mcpServerSse](),
	"mcpServerStdio":                           reflect.TypeFor[mcpServerStdio](),
	"newSessionRequest":                        reflect.TypeFor[newSessionRequest](),
	"newSessionResponse":                       reflect.TypeFor[newSessionResponse](),
	"permissionOption":                         reflect.TypeFor[permissionOption](),
	"plan":                                     reflect.TypeFor[plan](),
	"planEntry":                                reflect.TypeFor[planEntry](),
	"promptCapabilities":                       reflect.TypeFor[promptCapabilities](),
	"promptRequest":                            reflect.TypeFor[promptRequest](),
	"promptResponse":                           reflect.TypeFor[promptResponse](),
	"requestPermissionRequest":                 reflect.TypeFor[requestPermissionRequest](),
	"requestPermissionResponse":                reflect.TypeFor[requestPermissionResponse](),
	"resourceLink":                             reflect.TypeFor[resourceLink](),
	"resumeSessionRequest":                     reflect.TypeFor[resumeSessionRequest](),
	"resumeSessionResponse":                    reflect.TypeFor[resumeSessionResponse](),
	"selectedPermissionOutcome":                reflect.TypeFor[selectedPermissionOutcome](),
	"sessionAdditionalDirectoriesCapabilities": reflect.TypeFor[sessionAdditionalDirectoriesCapabilities](),
	"sessionCapabilities":                      reflect.TypeFor[sessionCapabilities](),
	"sessionCloseCapabilities":                 reflect.TypeFor[sessionCloseCapabilities](),
	"sessionConfigBoolean":                     reflect.TypeFor[sessionConfigBoolean](),
	"sessionConfigOptionsCapabilities":         reflect.TypeFor[sessionConfigOptionsCapabilities](),
	"sessionConfigSelect":                      reflect.TypeFor[sessionConfigSelect](),
	"sessionConfigSelectGroup":                 reflect.TypeFor[sessionConfigSelectGroup](),
	"sessionConfigSelectOption":                reflect.TypeFor[sessionConfigSelectOption](),
	"sessionDeleteCapabilities":                reflect.TypeFor[sessionDeleteCapabilities](),
	"sessionInfoUpdate":                        reflect.TypeFor[sessionInfoUpdate](),
	"sessionListCapabilities":                  reflect.TypeFor[sessionListCapabilities](),
	"sessionMode":                              reflect.TypeFor[sessionMode](),
	"sessionModeState":                         reflect.TypeFor[sessionModeState](),
	"sessionNotification":                      reflect.TypeFor[sessionNotification](),
	"sessionResumeCapabilities":                reflect.TypeFor[sessionResumeCapabilities](),
	"terminal":                                 reflect.TypeFor[terminal](),
	"textContent":                              reflect.TypeFor[textContent](),
	"textResourceContents":                     reflect.TypeFor[textResourceContents](),
	"toolCall":                                 reflect.TypeFor[toolCall](),
	"toolCallLocation":                         reflect.TypeFor[toolCallLocation](),
	"toolCallUpdate":                           reflect.TypeFor[toolCallUpdate](),
	"unstructuredCommandInput":                 reflect.TypeFor[unstructuredCommandInput](),
	"usageUpdate":                              reflect.TypeFor[usageUpdate](),
}

// isPlainObjectForm reports whether def is a definition the generator
// emits as a plain struct mirroring its own declared properties: type
// "object" with a properties map, and neither a oneOf nor a discriminator
// (which mark a union form instead). SessionConfigOption
// is the one definition at the pin that declares both a base properties
// map and a discriminator; it is excluded here because the generator
// emits it as a tagged-remainder union struct, not a properties mirror.
func isPlainObjectForm(def map[string]any) bool {
	if t, _ := def["type"].(string); t != "object" {
		return false
	}
	if _, ok := def["properties"]; !ok {
		return false
	}
	if _, ok := def["oneOf"]; ok {
		return false
	}
	if _, ok := def["discriminator"]; ok {
		return false
	}
	return true
}

// structFieldInfo is what assertStructMirrorsDefinition needs to know
// about one generated struct field's JSON wire shape.
type structFieldInfo struct {
	isPointer    bool
	hasOmitEmpty bool
	isRawMessage bool
}

// structJSONFields indexes typ's fields by their declared JSON name.
// It fails t if two fields declare the same name, which would make the
// wire shape ambiguous.
func structJSONFields(t *testing.T, typ reflect.Type) map[string]structFieldInfo {
	t.Helper()

	rawMessageType := reflect.TypeFor[json.RawMessage]()
	out := make(map[string]structFieldInfo, typ.NumField())
	for f := range typ.Fields() {
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			continue
		}
		if _, exists := out[name]; exists {
			t.Fatalf("type %s declares JSON name %q on more than one field", typ.Name(), name)
		}
		info := structFieldInfo{
			isPointer:    f.Type.Kind() == reflect.Pointer,
			isRawMessage: f.Type == rawMessageType,
		}
		for _, opt := range parts[1:] {
			if opt == "omitempty" {
				info.hasOmitEmpty = true
			}
		}
		out[name] = info
	}
	return out
}

// assertStructMirrorsDefinition compares a generated struct against the
// definition it mirrors: every JSON name typ declares must exist among
// def's own properties, every name in def's required list must be a
// non-pointer, non-omitempty field on typ, and every other field must be
// a pointer carrying omitempty, with _meta exempted into the
// json.RawMessage form the generator emits it as.
func assertStructMirrorsDefinition(t *testing.T, defName, goTypeName string, def map[string]any, typ reflect.Type) {
	t.Helper()

	properties, _ := def["properties"].(map[string]any)
	requiredAny, _ := def["required"].([]any)
	required := make(map[string]bool, len(requiredAny))
	for _, r := range requiredAny {
		if s, ok := r.(string); ok {
			required[s] = true
		}
	}

	fields := structJSONFields(t, typ)

	for name := range fields {
		if _, ok := properties[name]; !ok {
			t.Errorf("%s (%s): field with JSON name %q is not among the definition's declared properties", goTypeName, defName, name)
		}
	}

	for name := range required {
		info, ok := fields[name]
		if !ok {
			t.Errorf("%s (%s): required property %q has no matching struct field", goTypeName, defName, name)
			continue
		}
		if info.hasOmitEmpty {
			t.Errorf("%s (%s): required property %q carries omitempty", goTypeName, defName, name)
		}
		if info.isPointer {
			t.Errorf("%s (%s): required property %q is a pointer field, want non-pointer", goTypeName, defName, name)
		}
	}

	for name, info := range fields {
		if required[name] {
			continue
		}
		if name == "_meta" {
			if !info.isRawMessage {
				t.Errorf("%s (%s): _meta field is not json.RawMessage", goTypeName, defName)
			}
			if !info.hasOmitEmpty {
				t.Errorf("%s (%s): _meta field lacks omitempty", goTypeName, defName)
			}
			continue
		}
		if !info.isPointer {
			t.Errorf("%s (%s): optional field with JSON name %q is not a pointer", goTypeName, defName, name)
		}
		if !info.hasOmitEmpty {
			t.Errorf("%s (%s): optional field with JSON name %q lacks omitempty", goTypeName, defName, name)
		}
	}
}

// --- Closed value sets ---

// assertSetsEqual fails t on any member present in one set and absent
// from the other, naming label and the offending member.
func assertSetsEqual(t *testing.T, label string, schemaSet, goSet map[string]bool) {
	t.Helper()
	for v := range schemaSet {
		if !goSet[v] {
			t.Errorf("%s: pinned schema declares %q, no matching Go constant", label, v)
		}
	}
	for v := range goSet {
		if !schemaSet[v] {
			t.Errorf("%s: Go declares constant %q, pinned schema has no matching member", label, v)
		}
	}
}

// assertPlainEnumMatches checks a closed value set in the plain form:
// defName's oneOf members are each a string schema carrying its own
// const, and that set of consts must equal goSet exactly.
func assertPlainEnumMatches(t *testing.T, defs map[string]any, defName string, goSet map[string]bool) {
	t.Helper()

	def, ok := definition(defs, defName)
	if !ok {
		t.Fatalf("definition %q not found in pinned schema", defName)
	}
	oneOf, ok := def["oneOf"].([]any)
	if !ok {
		t.Fatalf("definition %q does not declare oneOf", defName)
	}

	schemaSet := make(map[string]bool, len(oneOf))
	for _, m := range oneOf {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("definition %q: oneOf member is not an object", defName)
			continue
		}
		c, ok := mm["const"].(string)
		if !ok {
			t.Fatalf("definition %q: oneOf member has no string const", defName)
			continue
		}
		schemaSet[c] = true
	}

	assertSetsEqual(t, defName, schemaSet, goSet)
}

// assertTaggedEnumMatches checks a closed value set in the tagged form:
// defName declares a discriminator named tagProperty,
// and each of its oneOf members' const sits one level down, at
// properties.<tagProperty>.const, not on the member itself.
func assertTaggedEnumMatches(t *testing.T, defs map[string]any, defName, tagProperty string, goSet map[string]bool) {
	t.Helper()

	def, ok := definition(defs, defName)
	if !ok {
		t.Fatalf("definition %q not found in pinned schema", defName)
	}
	disc, ok := def["discriminator"].(map[string]any)
	if !ok {
		t.Fatalf("definition %q does not declare a discriminator", defName)
	}
	if pn, _ := disc["propertyName"].(string); pn != tagProperty {
		t.Fatalf("definition %q discriminator propertyName = %q, want %q", defName, pn, tagProperty)
	}
	oneOf, ok := def["oneOf"].([]any)
	if !ok {
		t.Fatalf("definition %q does not declare oneOf", defName)
	}

	schemaSet := make(map[string]bool, len(oneOf))
	for _, m := range oneOf {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("definition %q: oneOf member is not an object", defName)
			continue
		}
		props, _ := mm["properties"].(map[string]any)
		tagSchema, _ := props[tagProperty].(map[string]any)
		c, ok := tagSchema["const"].(string)
		if !ok {
			t.Fatalf("definition %q: oneOf member has no nested const at properties.%s.const", defName, tagProperty)
			continue
		}
		schemaSet[c] = true
	}

	assertSetsEqual(t, defName, schemaSet, goSet)
}

// --- A generic conformance checker over the pinned artifact ---

// conformanceChecker validates a decoded JSON value against a schema
// definition. In strict mode (the client's own written request
// bodies) an undeclared property fails; in the weaker read-direction
// mode (messages the client reads) an undeclared property is recorded
// in undeclared and does not fail. A required property absent, a JSON
// type disagreement, or a value outside a const-constrained set always
// fails, in both modes.
type conformanceChecker struct {
	defs       map[string]any
	strict     bool
	violations []string
	undeclared []string
}

func (c *conformanceChecker) fail(format string, args ...any) {
	c.violations = append(c.violations, fmt.Sprintf(format, args...))
}

// check validates value against schema at path, resolving $ref and a
// one-element allOf to the definition they name. A $ref or an allOf
// this function cannot resolve fails rather than silently accepting
// the value.
func (c *conformanceChecker) check(schema map[string]any, value any, path string) {
	if ref, hasRef := schema["$ref"]; hasRef {
		refStr, ok := ref.(string)
		if !ok {
			c.fail("%s: $ref value is not a string", path)
			return
		}
		name := strings.TrimPrefix(refStr, "#/$defs/")
		resolved, ok := definition(c.defs, name)
		if !ok {
			c.fail("%s: $ref %q does not resolve to a definition in the pinned schema", path, refStr)
			return
		}
		c.check(resolved, value, path)
		return
	}
	if allOf, hasAllOf := schema["allOf"].([]any); hasAllOf {
		if len(allOf) != 1 {
			c.fail("%s: allOf has %d members, want exactly 1 (a form this checker does not resolve)", path, len(allOf))
			return
		}
		am, ok := allOf[0].(map[string]any)
		if !ok {
			c.fail("%s: allOf member is not an object", path)
			return
		}
		c.check(am, value, path)
		return
	}

	if anyOf, ok := schema["anyOf"].([]any); ok {
		if value == nil {
			return
		}
		for _, m := range anyOf {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := mm["type"].(string); t == "null" {
				continue
			}
			c.check(mm, value, path)
			return
		}
		c.fail("%s: anyOf schema has no non-null alternative to validate against", path)
		return
	}

	if value == nil {
		if !schemaTypeListContains(schema, "null") {
			c.fail("%s: value is null, the definition does not allow null here", path)
		}
		return
	}

	if constVal, ok := schema["const"]; ok {
		if !reflect.DeepEqual(constVal, value) {
			c.fail("%s = %v, want const %v", path, value, constVal)
		}
		return
	}

	if disc, ok := schema["discriminator"].(map[string]any); ok {
		c.checkDiscriminated(schema, disc, value, path)
		return
	}
	if oneOf, ok := schema["oneOf"].([]any); ok {
		c.checkEnum(oneOf, value, path)
		return
	}

	typ, hasType := schemaType(schema)
	switch typ {
	case "object":
		c.checkObject(schema, value, path)
	case "array":
		c.checkArray(schema, value, path)
	case "string":
		if _, ok := value.(string); !ok {
			c.fail("%s type = %T, want string", path, value)
		}
	case "integer", "number":
		if _, ok := value.(float64); !ok {
			c.fail("%s type = %T, want a number", path, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			c.fail("%s type = %T, want boolean", path, value)
		}
	default:
		if !hasType {
			// No type, const, oneOf, or discriminator: an
			// intentionally unconstrained schema (rawInput,
			// rawOutput at the pin), so any value is accepted.
			return
		}
		c.fail("%s: schema declares unhandled type %q", path, typ)
	}
}

// checkObject validates value as an object against schema's own
// properties and required list.
func (c *conformanceChecker) checkObject(schema map[string]any, value any, path string) {
	m, ok := value.(map[string]any)
	if !ok {
		c.fail("%s type = %T, want object", path, value)
		return
	}

	properties, _ := schema["properties"].(map[string]any)
	requiredAny, _ := schema["required"].([]any)
	for _, r := range requiredAny {
		name, _ := r.(string)
		if _, present := m[name]; !present {
			c.fail("%s: required property %q is absent", path, name)
		}
	}

	for key, v := range m {
		propSchema, declared := properties[key].(map[string]any)
		if !declared {
			if c.strict {
				c.fail("%s: property %q is not declared by the definition", path, key)
			} else {
				c.undeclared = append(c.undeclared, path+"."+key)
			}
			continue
		}
		c.check(propSchema, v, path+"."+key)
	}
}

// checkArray validates each element of value against schema's items
// schema.
func (c *conformanceChecker) checkArray(schema map[string]any, value any, path string) {
	arr, ok := value.([]any)
	if !ok {
		c.fail("%s type = %T, want array", path, value)
		return
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return
	}
	for i, el := range arr {
		c.check(items, el, fmt.Sprintf("%s[%d]", path, i))
	}
}

// checkEnum validates value as a string against oneOf's declared
// consts, the plain closed-value-set form.
func (c *conformanceChecker) checkEnum(oneOf []any, value any, path string) {
	s, ok := value.(string)
	if !ok {
		c.fail("%s type = %T, want string (enum)", path, value)
		return
	}
	for _, m := range oneOf {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if cv, ok := mm["const"].(string); ok && cv == s {
			return
		}
	}
	c.fail("%s = %q, not among the definition's declared const values", path, s)
}

// checkDiscriminated validates value as an object whose discriminator
// property selects one oneOf member, then validates value against that
// member merged with the definition it references through allOf. A
// discriminator value matching no member fails, since it is a value
// outside the definition's own const-constrained set.
func (c *conformanceChecker) checkDiscriminated(schema map[string]any, disc map[string]any, value any, path string) {
	propName, _ := disc["propertyName"].(string)
	m, ok := value.(map[string]any)
	if !ok {
		c.fail("%s type = %T, want object (discriminated union)", path, value)
		return
	}
	tagVal, _ := m[propName].(string)

	oneOf, _ := schema["oneOf"].([]any)
	for _, memberAny := range oneOf {
		member, ok := memberAny.(map[string]any)
		if !ok {
			continue
		}
		props, _ := member["properties"].(map[string]any)
		tagSchema, _ := props[propName].(map[string]any)
		cv, _ := tagSchema["const"].(string)
		if cv != tagVal {
			continue
		}
		c.checkObject(c.mergeMember(member), value, path)
		return
	}
	c.fail("%s: %s = %q is not among the definition's declared discriminator values", path, propName, tagVal)
}

// mergeMember merges a discriminated union member's own properties and
// required list with those of the definition its allOf names, so the
// merged schema is one checkObject can validate the full variant
// against in one pass.
func (c *conformanceChecker) mergeMember(member map[string]any) map[string]any {
	props := map[string]any{}
	var required []any

	if p, ok := member["properties"].(map[string]any); ok {
		maps.Copy(props, p)
	}
	if r, ok := member["required"].([]any); ok {
		required = append(required, r...)
	}
	if allOf, ok := member["allOf"].([]any); ok {
		for _, a := range allOf {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			refStr, ok := am["$ref"].(string)
			if !ok {
				continue
			}
			resolved, ok := definition(c.defs, strings.TrimPrefix(refStr, "#/$defs/"))
			if !ok {
				continue
			}
			if p, ok := resolved["properties"].(map[string]any); ok {
				maps.Copy(props, p)
			}
			if r, ok := resolved["required"].([]any); ok {
				required = append(required, r...)
			}
		}
	}

	return map[string]any{"properties": props, "required": required}
}

// schemaType reports schema's own "type" as a single string, taking
// the first non-null entry when "type" is a list (the nullable-field
// form). ok is false when no type is declared at all.
func schemaType(schema map[string]any) (typ string, ok bool) {
	switch t := schema["type"].(type) {
	case string:
		return t, true
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s, true
			}
		}
	}
	return "", false
}

// schemaTypeListContains reports whether schema's own "type" names
// want, whether "type" is a bare string or a list.
func schemaTypeListContains(schema map[string]any, want string) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == want
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// checkStrict validates value against defName in the strict direction:
// every required property present, no undeclared property, every
// const-constrained value inside its declared set.
func checkStrict(defs map[string]any, defName string, value any) []string {
	def, ok := definition(defs, defName)
	if !ok {
		return []string{fmt.Sprintf("definition %q not found in pinned schema", defName)}
	}
	c := &conformanceChecker{defs: defs, strict: true}
	c.check(def, value, defName)
	return c.violations
}

// checkWeak validates value against defName in the weaker read
// direction: a required property absent, a type disagreement, or a
// value outside a const-constrained set fails; an undeclared property
// is recorded in undeclared instead of failing.
func checkWeak(defs map[string]any, defName string, value any) (violations, undeclared []string) {
	def, ok := definition(defs, defName)
	if !ok {
		return []string{fmt.Sprintf("definition %q not found in pinned schema", defName)}, nil
	}
	c := &conformanceChecker{defs: defs, strict: false}
	c.check(def, value, defName)
	return c.violations, c.undeclared
}

// containsSuffix reports whether any entry of list ends with suffix.
func containsSuffix(list []string, suffix string) bool {
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// requestParams decodes raw, one full JSON-RPC request line, and
// returns the generic value its "params" member carries.
func requestParams(t *testing.T, raw []byte) any {
	t.Helper()

	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode request envelope %s: %v", raw, err)
	}
	var params any
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		t.Fatalf("decode request params %s: %v", envelope.Params, err)
	}
	return params
}

// responseResult decodes raw, one full JSON-RPC response line, and
// returns the generic value its "result" member carries.
func responseResult(t *testing.T, raw []byte) any {
	t.Helper()

	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode response envelope %s: %v", raw, err)
	}
	var result any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode response result %s: %v", envelope.Result, err)
	}
	return result
}

// nextRequestLine reads the next line out, asserting it names
// wantMethod, and returns its raw bytes and id.
func nextRequestLine(t *testing.T, out *outboundReader, wantMethod string) (raw []byte, id json.RawMessage) {
	t.Helper()

	raw = out.next(t)
	var hdr wireHeader
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatalf("decode outbound line %s: %v", raw, err)
	}
	if hdr.Method != wantMethod {
		t.Fatalf("outbound line method = %q, want %q (line: %s)", hdr.Method, wantMethod, raw)
	}
	return raw, hdr.ID
}

// --- TestSchemaConformance ---

// TestSchemaConformance validates the generated types against the
// pinned artifact without executing the generator, validates the
// adapter's own written request bodies against it strictly, and
// validates captured fixtures against it in the weaker read direction.
func TestSchemaConformance(t *testing.T) {
	dir := schemaAssetsDir
	assertProvenance(t, dir)
	defs := loadSchemaDefs(t, dir)

	t.Run("generated struct fields mirror the pinned definitions", func(t *testing.T) {
		t.Parallel()

		for defName, goTypeName := range wireTypeByDefinition {
			def, ok := definition(defs, defName)
			if !ok {
				t.Errorf("definition %q named in wire_gen.go's wireTypeByDefinition is absent from the pinned schema", defName)
				continue
			}
			if !isPlainObjectForm(def) {
				continue
			}
			typ, ok := goTypeRegistry[goTypeName]
			if !ok {
				t.Fatalf("no reflect.Type registered in this test's goTypeRegistry for generated type %q (definition %q); add it", goTypeName, defName)
			}
			assertStructMirrorsDefinition(t, defName, goTypeName, def, typ)
		}
	})

	t.Run("closed value sets match declared constants", func(t *testing.T) {
		t.Parallel()

		t.Run("StopReason", func(t *testing.T) {
			t.Parallel()
			assertPlainEnumMatches(t, defs, "StopReason", map[string]bool{
				string(stopReasonEndTurn):         true,
				string(stopReasonMaxTokens):       true,
				string(stopReasonMaxTurnRequests): true,
				string(stopReasonRefusal):         true,
				string(stopReasonCancelled):       true,
			})
		})

		t.Run("PermissionOptionKind", func(t *testing.T) {
			t.Parallel()
			assertPlainEnumMatches(t, defs, "PermissionOptionKind", map[string]bool{
				string(permissionOptionKindAllowOnce):    true,
				string(permissionOptionKindAllowAlways):  true,
				string(permissionOptionKindRejectOnce):   true,
				string(permissionOptionKindRejectAlways): true,
			})
		})

		t.Run("ToolKind", func(t *testing.T) {
			t.Parallel()
			assertPlainEnumMatches(t, defs, "ToolKind", map[string]bool{
				string(toolKindRead):       true,
				string(toolKindEdit):       true,
				string(toolKindDelete):     true,
				string(toolKindMove):       true,
				string(toolKindSearch):     true,
				string(toolKindExecute):    true,
				string(toolKindThink):      true,
				string(toolKindFetch):      true,
				string(toolKindSwitchMode): true,
				string(toolKindOther):      true,
			})
		})

		t.Run("SessionUpdate", func(t *testing.T) {
			t.Parallel()
			assertTaggedEnumMatches(t, defs, "SessionUpdate", "sessionUpdate", map[string]bool{
				sessionUpdateUserMessageChunk:        true,
				sessionUpdateAgentMessageChunk:       true,
				sessionUpdateAgentThoughtChunk:       true,
				sessionUpdateToolCall:                true,
				sessionUpdateToolCallUpdate:          true,
				sessionUpdatePlan:                    true,
				sessionUpdateAvailableCommandsUpdate: true,
				sessionUpdateCurrentModeUpdate:       true,
				sessionUpdateConfigOptionUpdate:      true,
				sessionUpdateSessionInfoUpdate:       true,
				sessionUpdateUsageUpdate:             true,
			})
		})
	})

	t.Run("written request bodies conform strictly to the pinned artifact", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)

		initDone := make(chan struct{})
		go func() {
			defer close(initDone)
			_, _ = doInitialize(context.Background(), state)
		}()
		initRaw, initID := nextRequestLine(t, out, methodInitialize)
		respondLine(t, inPw, initID, initializeResponse{ProtocolVersion: protocolVersion(pinnedProtocolVersion)})
		<-initDone

		parsed, agentErr := parseMCPServers("", false)
		if agentErr != nil {
			t.Fatalf("parseMCPServers: %v", agentErr)
		}
		servers, _ := parsed.wireServers(false)

		newSessDone := make(chan struct{})
		go func() {
			defer close(newSessDone)
			_, _ = doNewSession(context.Background(), state, t.TempDir(), servers)
		}()
		newSessRaw, newSessID := nextRequestLine(t, out, methodSessionNew)
		respondLine(t, inPw, newSessID, newSessionResponse{SessionID: sessionId("sess-conformance")})
		<-newSessDone

		markSessionKnown(state)
		var events []domain.AgentEvent
		outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "hello", OnEvent: collectEvents(&events)})
		promptRaw, promptID := nextRequestLine(t, out, methodSessionPrompt)

		sendLine(t, inPw, `{"jsonrpc":"2.0","id":"selected","method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"work"}}}`)
		selectedPermissionRaw := out.next(t)
		assertRawID(t, selectedPermissionRaw, `"selected"`)

		sendLine(t, inPw, `{"jsonrpc":"2.0","id":"cancelled","method":"session/request_permission","params":{"sessionId":"sess-test","options":[],"toolCall":{"toolCallId":"tc-2","title":"work"}}}`)
		cancelledPermissionRaw := out.next(t)
		assertRawID(t, cancelledPermissionRaw, `"cancelled"`)

		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
		awaitOutcome(t, outcomeCh)

		for _, tc := range []struct {
			label   string
			defName string
			raw     []byte
			body    func(*testing.T, []byte) any
		}{
			{"initialize", "InitializeRequest", initRaw, requestParams},
			{"session/new", "NewSessionRequest", newSessRaw, requestParams},
			{"session/prompt", "PromptRequest", promptRaw, requestParams},
			{"permission selected", "RequestPermissionResponse", selectedPermissionRaw, responseResult},
			{"permission cancelled", "RequestPermissionResponse", cancelledPermissionRaw, responseResult},
		} {
			t.Run(tc.label, func(t *testing.T) {
				body := tc.body(t, tc.raw)
				for _, violation := range checkStrict(defs, tc.defName, body) {
					t.Error(violation)
				}
			})
		}
	})

	t.Run("captured session/update fixtures conform in the weaker read direction", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name           string
			file           string
			wantViolation  bool
			wantUndeclared []string
		}{
			{"agent message chunk", "agent_message_chunk.json", false, nil},
			{"agent thought chunk", "agent_thought_chunk.json", false, nil},
			{"plan", "plan.json", false, nil},
			{"tool call", "tool_call.json", false, nil},
			{"tool call with a kind outside the const set fails", "tool_call_unknown_kind.json", true, nil},
			{"tool call update completed", "tool_call_update_completed.json", false, nil},
			{"tool call update failed", "tool_call_update_failed.json", false, nil},
			{"tool call update with a status outside the const set fails", "tool_call_update_unknown_status.json", true, nil},
			{"an undeclared property is recorded, not failed", "unknown_key_agent_message_chunk.json", false, []string{"futureField"}},
			{"a discriminator value outside the const set fails", "unknown_variant.json", true, nil},
			{"usage update", "usage_update.json", false, nil},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				raw, err := os.ReadFile(filepath.Join("testdata", "session_updates", tt.file)) //nolint:gosec // G304: tt.file is a fixed test-table literal
				if err != nil {
					t.Fatalf("read fixture %s: %v", tt.file, err)
				}
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatalf("decode fixture %s: %v", tt.file, err)
				}

				violations, undeclared := checkWeak(defs, "SessionUpdate", value)

				if tt.wantViolation {
					if len(violations) == 0 {
						t.Errorf("fixture %s: want at least one conformance violation, got none", tt.file)
					}
				} else {
					for _, v := range violations {
						t.Error(v)
					}
				}

				for _, want := range tt.wantUndeclared {
					if !containsSuffix(undeclared, want) {
						t.Errorf("fixture %s: undeclared properties = %v, want one ending in %q", tt.file, undeclared, want)
					}
				}
				if len(tt.wantUndeclared) == 0 && len(undeclared) != 0 {
					t.Errorf("fixture %s: undeclared properties = %v, want none", tt.file, undeclared)
				}
			})
		}
	})
}
