package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/maputil"
)

// FieldType classifies the expected YAML type for a config field.
type FieldType int

const (
	FieldString      FieldType = iota + 1 // string scalar
	FieldInt                              // integer (or string-encoded integer)
	FieldBool                             // boolean
	FieldStringList                       // []string (YAML sequence)
	FieldMap                              // map[string]any (YAML mapping)
	FieldShellScript                      // multiline string (shell script)
	FieldSequence                         // []any (YAML sequence of maps or scalars)
)

// FieldDef describes a single known configuration field within a section.
type FieldDef struct {
	Name   string     // YAML key name (e.g. "kind", "interval_ms")
	Type   FieldType  // expected YAML value type
	Nested []FieldDef // known sub-fields for FieldMap types (e.g. tracker.comments)
}

// SectionSchema defines the known fields for one top-level config section.
type SectionSchema struct {
	Fields                  []FieldDef // recognized keys within this section
	AllowAdapterPassthrough bool       // exempt the adapter-kind sub-object from unknown-key warnings
	AllowDynamicKeys        bool       // skip all sub-key name and type validation (map-of-maps sections)
}

// knownFieldsRegistry enumerates the known sub-keys for each top-level
// config section.
var knownFieldsRegistry = map[string]SectionSchema{
	"tracker": {
		Fields: []FieldDef{
			{Name: "kind", Type: FieldString},
			{Name: "endpoint", Type: FieldString},
			{Name: "api_key", Type: FieldString},
			{Name: "project", Type: FieldString},
			{Name: "active_states", Type: FieldStringList},
			{Name: "terminal_states", Type: FieldStringList},
			{Name: "query_filter", Type: FieldString},
			{Name: "handoff_state", Type: FieldString},
			{Name: "handoff_evidence", Type: FieldString},
			{Name: "in_progress_state", Type: FieldString},
			{Name: "api_version", Type: FieldString},
			{Name: "comments", Type: FieldMap, Nested: []FieldDef{
				{Name: "on_dispatch", Type: FieldBool},
				{Name: "on_completion", Type: FieldBool},
				{Name: "on_failure", Type: FieldBool},
			}},
		},
		AllowAdapterPassthrough: true,
	},
	"polling": {
		Fields: []FieldDef{
			{Name: "interval_ms", Type: FieldInt},
		},
	},
	"workspace": {
		Fields: []FieldDef{
			{Name: "root", Type: FieldString},
			{Name: "retention_days", Type: FieldInt},
		},
	},
	"hooks": {
		Fields: []FieldDef{
			{Name: "after_create", Type: FieldShellScript},
			{Name: "before_run", Type: FieldShellScript},
			{Name: "after_run", Type: FieldShellScript},
			{Name: "before_remove", Type: FieldShellScript},
			{Name: "timeout_ms", Type: FieldInt},
		},
	},
	"agent": {
		Fields: []FieldDef{
			{Name: "kind", Type: FieldString},
			{Name: "command", Type: FieldString},
			{Name: "turn_timeout_ms", Type: FieldInt},
			{Name: "read_timeout_ms", Type: FieldInt},
			{Name: "stall_timeout_ms", Type: FieldInt},
			{Name: "max_concurrent_agents", Type: FieldInt},
			{Name: "max_turns", Type: FieldInt},
			{Name: "max_retry_backoff_ms", Type: FieldInt},
			{Name: "max_concurrent_agents_by_state", Type: FieldMap},
			{Name: "max_sessions", Type: FieldInt},
			{Name: "max_tokens", Type: FieldInt},
			{Name: "max_consecutive_absences", Type: FieldInt},
		},
		AllowAdapterPassthrough: true,
	},
	"ci_feedback": {
		Fields: []FieldDef{
			{Name: "kind", Type: FieldString},
			{Name: "max_retries", Type: FieldInt},
			{Name: "max_log_lines", Type: FieldInt},
			{Name: "escalation", Type: FieldString},
			{Name: "escalation_label", Type: FieldString},
		},
	},
	"self_review": {
		Fields: []FieldDef{
			{Name: "enabled", Type: FieldBool},
			{Name: "max_iterations", Type: FieldInt},
			{Name: "verification_commands", Type: FieldStringList},
			{Name: "verification_timeout_ms", Type: FieldInt},
			{Name: "max_diff_bytes", Type: FieldInt},
			{Name: "reviewer", Type: FieldString},
		},
	},
	"reactions": {
		Fields:           []FieldDef{},
		AllowDynamicKeys: true,
	},
	"dispatch": {
		Fields: []FieldDef{
			{Name: "rules", Type: FieldSequence},
			{Name: "default", Type: FieldMap, Nested: []FieldDef{
				{Name: "agent", Type: FieldString},
				{Name: "template", Type: FieldString},
			}},
		},
	},
}

// dispatchRuleAllowedKeys lists the recognized per-rule keys for
// front-matter static analysis. Mirrors [ruleKeyAllowed] in
// dispatch.go; kept here so schema descent does not import dispatch
// helpers directly.
var dispatchRuleAllowedKeys = map[string]bool{
	"name":     true,
	"match":    true,
	"agent":    true,
	"template": true,
}

// dispatchMatchAllowedKeys lists the recognized match keys for
// front-matter static analysis.
var dispatchMatchAllowedKeys = map[string]bool{
	"labels":     true,
	"issue_type": true,
	"priority":   true,
	"identifier": true,
	"assignee":   true,
}

// staticKnownExtensionKeys lists extension top-level keys defined by
// the architecture spec. These are not core schema keys but are
// recognized by Sortie's optional modules.
var staticKnownExtensionKeys = map[string]bool{
	"server":      true,
	"logging":     true,
	"worker":      true,
	"token_rates": true,
}

// FrontMatterWarning represents a single advisory diagnostic from
// front matter static analysis. These do not affect runtime behavior.
type FrontMatterWarning struct {
	Check   string // "unknown_key", "unknown_sub_key", "type_mismatch", or "unresolved_extension_var"
	Field   string // dotted path to the offending key
	Message string // operator-friendly description
}

// ValidateFrontMatter performs advisory static analysis on the raw
// front matter map. It requires the parsed [ServiceConfig] to extract
// dynamic extension keys (tracker.kind, agent.kind). Returns warnings
// only — these do not affect config validity or runtime behavior.
//
// The raw map must be the post-env-override map (same map that
// [NewServiceConfig] processed), so that env-override-created sections
// do not produce false positives.
func ValidateFrontMatter(raw map[string]any, cfg ServiceConfig) []FrontMatterWarning {
	if raw == nil {
		return nil
	}

	var warnings []FrontMatterWarning

	// Build recognized top-level key set.
	recognized := make(map[string]bool, len(knownTopLevelKeys)+len(staticKnownExtensionKeys)+2)
	for k := range knownTopLevelKeys {
		recognized[k] = true
	}
	for k := range staticKnownExtensionKeys {
		recognized[k] = true
	}
	if cfg.Tracker.Kind != "" {
		recognized[cfg.Tracker.Kind] = true
	}
	if cfg.Agent.Kind != "" {
		recognized[cfg.Agent.Kind] = true
	}

	// Augment the recognized set with every agent kind referenced by
	// dispatch.rules and dispatch.default. Deriving from the raw map
	// keeps this accurate when BuildDispatchConfig fails: the
	// recognized set must still exempt pass-through blocks named in
	// the failing rules so the operator sees the rule error, not a
	// stream of spurious unknown_key warnings.
	for _, kind := range derivePassThroughKindsFromRaw(raw) {
		recognized[kind] = true
	}

	// Flag top-level keys not in the recognized set.
	topKeys := maputil.SortedKeys(raw)
	for _, key := range topKeys {
		if !recognized[key] {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "unknown_key",
				Field:   key,
				Message: fmt.Sprintf("unknown top-level key %q", key),
			})
		}
	}

	// Validate sub-keys and value types within each known section.
	sectionNames := maputil.SortedKeys(knownFieldsRegistry)
	for _, sectionName := range sectionNames {
		schema := knownFieldsRegistry[sectionName]

		// If the section value is present but not a map, warn and skip
		// sub-key analysis.
		sectionVal, sectionExists := raw[sectionName]
		if !sectionExists || sectionVal == nil {
			continue
		}
		sectionMap, isMap := sectionVal.(map[string]any)
		if !isMap {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   sectionName,
				Message: fmt.Sprintf("expected map, got %T", sectionVal),
			})
			continue
		}

		if schema.AllowDynamicKeys {
			continue
		}

		// Build known-names set for this section.
		knownNames := make(map[string]bool, len(schema.Fields))
		for _, f := range schema.Fields {
			knownNames[f.Name] = true
		}

		// Determine adapter kind for pass-through exemption.
		var adapterKind string
		if schema.AllowAdapterPassthrough {
			adapterKind = extractString(sectionMap, "kind")
		}

		// Flag sub-keys not recognized by the section schema.
		subKeys := maputil.SortedKeys(sectionMap)
		for _, key := range subKeys {
			if knownNames[key] {
				continue
			}
			if schema.AllowAdapterPassthrough && adapterKind != "" && key == adapterKind {
				continue
			}
			warnings = append(warnings, FrontMatterWarning{
				Check:   "unknown_sub_key",
				Field:   sectionName + "." + key,
				Message: fmt.Sprintf("unknown key %q in section %q", key, sectionName),
			})
		}

		// Flag unknown keys within nested map fields (e.g. tracker.comments).
		for _, field := range schema.Fields {
			if field.Nested == nil {
				continue
			}
			nestedMap := extractSubMap(sectionMap, field.Name)
			if nestedMap == nil {
				continue
			}
			nestedNames := make(map[string]bool, len(field.Nested))
			for _, n := range field.Nested {
				nestedNames[n.Name] = true
			}
			nestedKeys := maputil.SortedKeys(nestedMap)
			for _, key := range nestedKeys {
				if !nestedNames[key] {
					warnings = append(warnings, FrontMatterWarning{
						Check:   "unknown_sub_key",
						Field:   sectionName + "." + field.Name + "." + key,
						Message: fmt.Sprintf("unknown key %q in section %q", key, sectionName+"."+field.Name),
					})
				}
			}
		}

		// Check value types for individual fields within this section.
		for _, field := range schema.Fields {
			v, exists := sectionMap[field.Name]
			if !exists || v == nil {
				continue
			}
			if !typeMatches(v, field.Type) {
				warnings = append(warnings, FrontMatterWarning{
					Check:   "type_mismatch",
					Field:   sectionName + "." + field.Name,
					Message: fmt.Sprintf("expected %s, got %T", typeName(field.Type), v),
				})
				continue
			}
			// Element-level checking for string lists.
			if field.Type == FieldStringList {
				warnings = checkStringListElements(warnings, sectionName+"."+field.Name, v)
			}
			// Sequence descent for sections that carry a YAML sequence
			// of maps (e.g. dispatch.rules).
			if field.Type == FieldSequence && sectionName == "dispatch" && field.Name == "rules" {
				warnings = descendDispatchRules(warnings, v)
			}
			// Nested map type checking (e.g. tracker.comments sub-fields).
			if field.Nested != nil {
				if nestedMap, ok := v.(map[string]any); ok {
					for _, nested := range field.Nested {
						nv, nExists := nestedMap[nested.Name]
						if !nExists || nv == nil {
							continue
						}
						if !typeMatches(nv, nested.Type) {
							warnings = append(warnings, FrontMatterWarning{
								Check:   "type_mismatch",
								Field:   sectionName + "." + field.Name + "." + nested.Name,
								Message: fmt.Sprintf("expected %s, got %T", typeName(nested.Type), nv),
							})
						}
					}
				}
			}
		}
	}

	// Validate db_path is a string when present.
	if v, exists := raw["db_path"]; exists && v != nil {
		if _, ok := v.(string); !ok {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   "db_path",
				Message: fmt.Sprintf("expected string, got %T", v),
			})
		}
	}

	// Emit semantic warnings for values that are type-correct but invalid.
	warnings = checkHooksTimeoutSemantic(warnings, raw)
	warnings = checkByStateSemantic(warnings, raw)

	// Surface unresolved $VAR references in extensions by replaying the
	// snapshot captured during NewServiceConfig. A non-empty resolved
	// leaf indicates the variable was set; a resolved empty leaf is the
	// warning trigger.
	warnings = checkUnresolvedExtensionVars(warnings, cfg)

	return warnings
}

// checkUnresolvedExtensionVars iterates the pre-resolution snapshot
// captured by [resolveExtensionEnvRefs] and emits one warning per
// extension leaf that contains a $VAR reference whose current value
// is unset or empty. The warning fires regardless of whether the
// resolved string is otherwise non-empty: a field like
// "https://$HOST:$PORT/path" still warns about PORT when only HOST is
// set. Entries whose extracted variable name is empty (e.g. the $$
// literal) are excluded.
func checkUnresolvedExtensionVars(warnings []FrontMatterWarning, cfg ServiceConfig) []FrontMatterWarning {
	if len(cfg.extensionsPreResolution) == 0 {
		return warnings
	}
	paths := maputil.SortedKeys(cfg.extensionsPreResolution)
	for _, path := range paths {
		before := cfg.extensionsPreResolution[path]
		_, ok := lookupExtensionValue(cfg.extensions, path)
		if !ok {
			continue
		}
		unsetNames := extractUnsetExtensionVars(before)
		if len(unsetNames) == 0 {
			continue
		}
		warnings = append(warnings, FrontMatterWarning{
			Check:   "unresolved_extension_var",
			Field:   path,
			Message: formatUnresolvedExtensionVarMessage(unsetNames),
		})
	}
	return warnings
}

// extractUnsetExtensionVars scans before for every $NAME and ${NAME}
// reference in declaration order and returns the distinct variable
// names (without the leading $ or wrapping braces) whose current
// [os.Getenv] value is empty. The first occurrence of a repeated name
// is preserved; later duplicates are dropped. Returns nil when every
// reference resolves to a non-empty value or when no extractable name
// is present.
func extractUnsetExtensionVars(before string) []string {
	var unset []string
	seen := make(map[string]bool)
	for i := 0; i < len(before); i++ {
		if before[i] != '$' {
			continue
		}
		name, consumed := readShellName(before[i+1:])
		i += consumed
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if os.Getenv(name) != "" {
			continue
		}
		unset = append(unset, name)
	}
	return unset
}

// readShellName parses a variable reference following a leading $ byte
// and returns the extracted name and the number of bytes consumed from
// s (excluding the leading $). The braced form ${NAME} consumes the
// closing brace; the bare form $NAME consumes only the identifier
// bytes. An empty name (no identifier or empty braces) returns the
// number of bytes that should be skipped past the malformed reference.
func readShellName(s string) (name string, consumed int) {
	if len(s) == 0 {
		return "", 0
	}
	if s[0] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return "", 0
		}
		return s[1:end], end + 1
	}
	if !isShellNameByte(s[0], true) {
		return "", 0
	}
	end := 1
	for end < len(s) && isShellNameByte(s[end], false) {
		end++
	}
	return s[:end], end
}

// isShellNameByte reports whether b is a valid byte for a shell-style
// identifier. When start is true, digits are not permitted.
func isShellNameByte(b byte, start bool) bool {
	if b == '_' {
		return true
	}
	if b >= 'A' && b <= 'Z' {
		return true
	}
	if b >= 'a' && b <= 'z' {
		return true
	}
	if !start && b >= '0' && b <= '9' {
		return true
	}
	return false
}

// formatUnresolvedExtensionVarMessage returns the operator-facing
// message for the unresolved_extension_var warning. The variable name
// is rendered via the %q verb so a name such as SORTIE_GITHUB_TOKEN
// prints surrounded by double quotes. The message is byte-stable
// across calls with the same input so operators can alert on it.
func formatUnresolvedExtensionVarMessage(unsetNames []string) string {
	switch len(unsetNames) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("unresolved $VAR reference: variable %q is unset or empty", unsetNames[0])
	case 2:
		return fmt.Sprintf("unresolved $VAR reference: variables %q and %q are unset or empty", unsetNames[0], unsetNames[1]) //nolint:gosec // G602: case 2 guarantees len(unsetNames) == 2
	}
	var b strings.Builder
	b.WriteString("unresolved $VAR reference: variables ")
	for i, name := range unsetNames {
		if i == len(unsetNames)-1 {
			b.WriteString("and ")
			fmt.Fprintf(&b, "%q", name)
			continue
		}
		fmt.Fprintf(&b, "%q, ", name)
	}
	b.WriteString(" are unset or empty")
	return b.String()
}

// lookupExtensionValue walks the dotted-and-indexed path produced by
// [resolveExtensionEnvRefs] under extensions and returns the resolved
// string at that path. Returns (value, true) when the path lands on a
// string leaf; returns ("", false) when any intermediate segment fails
// to type-assert or when the final leaf is not a string. Two-result
// type assertions guarantee the helper never panics on a malformed
// path.
func lookupExtensionValue(extensions map[string]any, path string) (string, bool) {
	if extensions == nil || path == "" {
		return "", false
	}
	var current any = extensions
	for path != "" {
		segment, rest, _ := strings.Cut(path, ".")
		path = rest
		name, indices := splitPathIndices(segment)
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = m[name]
		if !ok {
			return "", false
		}
		for _, idx := range indices {
			slice, ok := current.([]any)
			if !ok {
				return "", false
			}
			if idx < 0 || idx >= len(slice) {
				return "", false
			}
			current = slice[idx]
		}
	}
	s, ok := current.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// splitPathIndices separates a path segment into its name and any
// trailing [index] suffixes. For example "ssh_hosts[0][1]" returns
// ("ssh_hosts", [0, 1]). Malformed suffixes return the original
// segment with no indices.
func splitPathIndices(segment string) (string, []int) {
	openIdx := strings.IndexByte(segment, '[')
	if openIdx < 0 {
		return segment, nil
	}
	name := segment[:openIdx]
	rest := segment[openIdx:]
	var indices []int
	for rest != "" {
		if rest[0] != '[' {
			return segment, nil
		}
		closeIdx := strings.IndexByte(rest, ']')
		if closeIdx < 0 {
			return segment, nil
		}
		n, err := strconv.Atoi(rest[1:closeIdx])
		if err != nil {
			return segment, nil
		}
		indices = append(indices, n)
		rest = rest[closeIdx+1:]
	}
	return name, indices
}

// checkStringListElements appends type_mismatch warnings for non-string
// elements in a []any value.
func checkStringListElements(warnings []FrontMatterWarning, fieldPath string, v any) []FrontMatterWarning {
	slice, ok := v.([]any)
	if !ok {
		return warnings
	}
	for i, elem := range slice {
		if _, isStr := elem.(string); !isStr {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   fmt.Sprintf("%s[%d]", fieldPath, i),
				Message: fmt.Sprintf("expected string element, got %T", elem),
			})
		}
	}
	return warnings
}

// checkHooksTimeoutSemantic emits a warning when hooks.timeout_ms is
// coercible to an integer but non-positive, since the runtime silently
// falls back to the default 60000.
func checkHooksTimeoutSemantic(warnings []FrontMatterWarning, raw map[string]any) []FrontMatterWarning {
	hooksMap := extractSubMap(raw, "hooks")
	if hooksMap == nil {
		return warnings
	}
	v, exists := hooksMap["timeout_ms"]
	if !exists || v == nil {
		return warnings
	}
	n, err := coerceInt(v)
	if err != nil {
		return warnings // type mismatch already reported
	}
	if n <= 0 {
		warnings = append(warnings, FrontMatterWarning{
			Check:   "type_mismatch",
			Field:   "hooks.timeout_ms",
			Message: fmt.Sprintf("non-positive value %d will fall back to default 60000", n),
		})
	}
	return warnings
}

// checkByStateSemantic emits warnings for entries in
// agent.max_concurrent_agents_by_state that are non-numeric or
// non-positive, since these are silently dropped at runtime.
func checkByStateSemantic(warnings []FrontMatterWarning, raw map[string]any) []FrontMatterWarning {
	agentMap := extractSubMap(raw, "agent")
	if agentMap == nil {
		return warnings
	}
	byStateVal, exists := agentMap["max_concurrent_agents_by_state"]
	if !exists || byStateVal == nil {
		return warnings
	}
	byStateMap, ok := byStateVal.(map[string]any)
	if !ok {
		return warnings // type mismatch already reported
	}
	stateKeys := maputil.SortedKeys(byStateMap)
	for _, stateKey := range stateKeys {
		stateVal := byStateMap[stateKey]
		fieldPath := "agent.max_concurrent_agents_by_state." + stateKey
		n, err := coerceInt(stateVal)
		if err != nil {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   fieldPath,
				Message: "non-numeric value will be ignored",
			})
		} else if n <= 0 {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   fieldPath,
				Message: fmt.Sprintf("non-positive value %d will be ignored", n),
			})
		}
	}
	return warnings
}

// typeMatches reports whether v is an acceptable Go type for the given
// FieldType, matching the coercion semantics used at runtime.
func typeMatches(v any, ft FieldType) bool {
	switch ft {
	case FieldString, FieldShellScript:
		_, ok := v.(string)
		return ok
	case FieldBool:
		_, ok := v.(bool)
		return ok
	case FieldMap:
		_, ok := v.(map[string]any)
		return ok
	case FieldStringList:
		_, ok := v.([]any)
		return ok
	case FieldSequence:
		_, ok := v.([]any)
		return ok
	case FieldInt:
		switch val := v.(type) {
		case int, int64, int32:
			return true
		case float64:
			return val == math.Trunc(val)
		case string:
			_, err := strconv.Atoi(strings.TrimSpace(val))
			return err == nil
		default:
			return false
		}
	default:
		return false
	}
}

// typeName returns a human-readable name for a FieldType.
func typeName(ft FieldType) string {
	switch ft {
	case FieldString, FieldShellScript:
		return "string"
	case FieldInt:
		return "integer"
	case FieldBool:
		return "bool"
	case FieldStringList:
		return "list of strings"
	case FieldMap:
		return "map"
	case FieldSequence:
		return "sequence"
	default:
		return "unknown"
	}
}

// descendDispatchRules walks the dispatch.rules sequence and emits
// unknown_sub_key warnings for unrecognized per-rule keys and per-match
// keys. The sequence shape itself is already type-checked.
func descendDispatchRules(warnings []FrontMatterWarning, rulesVal any) []FrontMatterWarning {
	seq, ok := rulesVal.([]any)
	if !ok {
		return warnings
	}
	for i, elem := range seq {
		ruleMap, ok := elem.(map[string]any)
		if !ok {
			continue
		}
		rulePath := fmt.Sprintf("dispatch.rules[%d]", i)
		keys := maputil.SortedKeys(ruleMap)
		for _, key := range keys {
			if !dispatchRuleAllowedKeys[key] {
				warnings = append(warnings, FrontMatterWarning{
					Check:   "unknown_sub_key",
					Field:   rulePath + "." + key,
					Message: fmt.Sprintf("unknown key %q in dispatch rule", key),
				})
			}
		}
		matchRaw, exists := ruleMap["match"]
		if !exists || matchRaw == nil {
			continue
		}
		matchMap, ok := matchRaw.(map[string]any)
		if !ok {
			continue
		}
		matchKeys := maputil.SortedKeys(matchMap)
		for _, key := range matchKeys {
			if !dispatchMatchAllowedKeys[key] {
				warnings = append(warnings, FrontMatterWarning{
					Check:   "unknown_sub_key",
					Field:   rulePath + ".match." + key,
					Message: fmt.Sprintf("unknown match key %q", key),
				})
			}
		}
	}
	return warnings
}

// derivePassThroughKindsFromRaw scans the raw front matter for every
// agent kind referenced by dispatch.rules and dispatch.default. The
// returned slice may contain duplicates. Reading the raw map keeps
// this resilient to [BuildDispatchConfig] failure: the
// recognized-top-level-keys set must remain accurate even when the
// typed config is the zero value.
func derivePassThroughKindsFromRaw(raw map[string]any) []string {
	if raw == nil {
		return nil
	}
	dispatchRaw, ok := raw["dispatch"]
	if !ok || dispatchRaw == nil {
		return nil
	}
	dispatchMap, ok := dispatchRaw.(map[string]any)
	if !ok {
		return nil
	}

	var kinds []string

	if rulesRaw, ok := dispatchMap["rules"].([]any); ok {
		for _, elem := range rulesRaw {
			ruleMap, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := ruleMap["agent"].(string); ok && s != "" {
				kinds = append(kinds, s)
			}
		}
	}

	if defaultMap, ok := dispatchMap["default"].(map[string]any); ok {
		if s, ok := defaultMap["agent"].(string); ok && s != "" {
			kinds = append(kinds, s)
		}
	}

	return kinds
}
