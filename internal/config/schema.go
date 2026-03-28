package config

import (
	"fmt"
	"math"
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
	AllowAdapterPassthrough bool       // exempt adapter-kind sub-objects from unknown-key warnings
}

// knownFieldsRegistry enumerates the known sub-keys for each top-level
// config section. Derived from architecture Section 5.3.1–5.3.6 and the
// TrackerCommentsConfig struct in config.go.
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
			{Name: "in_progress_state", Type: FieldString},
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
		},
		AllowAdapterPassthrough: true,
	},
}

// staticKnownExtensionKeys lists extension top-level keys defined by
// the architecture spec. These are not core schema keys but are
// recognized by Sortie's optional modules.
var staticKnownExtensionKeys = map[string]bool{
	"server":  true,
	"logging": true,
	"worker":  true,
}

// FrontMatterWarning represents a single advisory diagnostic from
// front matter static analysis. These do not affect runtime behavior.
type FrontMatterWarning struct {
	Check   string // "unknown_key", "unknown_sub_key", or "type_mismatch"
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

	// Phase 1: Unknown top-level keys.
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

	// Phase 2 & 3: Iterate sections in deterministic order.
	sectionNames := maputil.SortedKeys(knownFieldsRegistry)
	for _, sectionName := range sectionNames {
		schema := knownFieldsRegistry[sectionName]

		// Phase 3 (section-level type check): if section value is present
		// but not a map, warn and skip sub-key analysis.
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

		// Phase 2: Unknown sub-keys.
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

		// Phase 2b: Nested sub-keys (e.g. tracker.comments).
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

		// Phase 3: Type mismatch for individual fields.
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

	// Phase 3: Top-level db_path type check.
	if v, exists := raw["db_path"]; exists && v != nil {
		if _, ok := v.(string); !ok {
			warnings = append(warnings, FrontMatterWarning{
				Check:   "type_mismatch",
				Field:   "db_path",
				Message: fmt.Sprintf("expected string, got %T", v),
			})
		}
	}

	// Phase 3b: Semantic value warnings.
	warnings = checkHooksTimeoutSemantic(warnings, raw)
	warnings = checkByStateSemantic(warnings, raw)

	return warnings
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
		return warnings // already caught by Phase 3 type mismatch
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
		return warnings // already caught by Phase 3 type mismatch
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
	default:
		return "unknown"
	}
}
