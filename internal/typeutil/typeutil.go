// Package typeutil provides type coercion utilities for loosely-typed values
// produced by JSON and YAML decoders. These helpers are shared across adapter
// packages that cannot import each other.
package typeutil

import (
	"strings"
	"time"
	"unicode/utf8"
)

// ExtractStringSlice converts a loosely-typed value to []string.
//
// It handles []any (as produced by JSON and YAML decoders) by extracting
// string elements and skipping non-string values, and []string by returning
// it directly without copying. For any other type, including nil, it returns
// nil.
func ExtractStringSlice(v any) []string {
	switch s := v.(type) {
	case []any:
		strs := make([]string, 0, len(s))
		for _, elem := range s {
			if str, ok := elem.(string); ok {
				strs = append(strs, str)
			}
		}
		return strs
	case []string:
		return s
	default:
		return nil
	}
}

// TruncateRunes returns s if it contains maxLen or fewer runes. When s
// exceeds maxLen runes, the first maxLen runes are returned with a "…"
// (U+2026) suffix. maxLen must be non-negative.
func TruncateRunes(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen]) + "…"
}

// TypeFault reports one configuration value whose YAML type is not the
// type its reader requires. Key is the config key as the operator wrote
// it, without any section prefix; the caller supplies the prefix when it
// has one.
type TypeFault struct {
	Key  string // e.g. "endpoint"
	Want string // required YAML type name, e.g. "string"
	Got  string // YAML type name found, e.g. "integer"
}

// Error renders the fault as "<Key>: expected <Want>, got <Got>".
func (f *TypeFault) Error() string {
	return f.Key + ": " + f.Reason()
}

// Reason renders the fault as "expected <Want>, got <Got>", for a caller
// that carries the field path in its own field and would otherwise print
// the key twice.
func (f *TypeFault) Reason() string {
	return "expected " + f.Want + ", got " + f.Got
}

// DescribeYAMLType returns the YAML type name of a decoded front-matter
// value. It never names a Go type: that detail is internal and an
// operator writing YAML cannot act on it.
func DescribeYAMLType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, uint64, int8, int16, int32, int64, uint, uint8, uint16, uint32:
		return "integer"
	case float64, float32:
		return "float"
	case []any:
		return "list"
	case map[string]any, map[any]any:
		return "mapping"
	case time.Time:
		return "timestamp"
	case nil:
		return "null"
	default:
		return "unrecognized value"
	}
}

// StringField returns the string value for key in config.
//
// An absent key, and a key whose value is nil, return ("", nil): the
// required-key decision belongs to the caller. A value of any other YAML
// type returns ("", fault) with the empty string, so a caller that
// ignores the fault degrades to the previous behavior rather than to an
// undefined value.
func StringField(config map[string]any, key string) (string, *TypeFault) {
	v, present := config[key]
	if !present || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &TypeFault{Key: key, Want: "string", Got: DescribeYAMLType(v)}
	}
	return s, nil
}

// IntFrom returns the integer value for key in config. Handles both int
// and float64 (JSON-decoded) values. A float64 that is not a whole number
// is rejected and defaultVal is returned. Returns defaultVal if the key is
// absent or the type is unrecognized.
func IntFrom(config map[string]any, key string, defaultVal int) int {
	raw, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch v := raw.(type) {
	case int:
		return v
	case float64:
		if v != float64(int(v)) {
			return defaultVal
		}
		return int(v)
	default:
		return defaultVal
	}
}

// FloatFrom returns the float64 value for key in config. Accepts both
// float64 and int values. Returns defaultVal otherwise.
func FloatFrom(config map[string]any, key string, defaultVal float64) float64 {
	raw, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch v := raw.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return defaultVal
	}
}

// BoolFrom returns the bool value for key in config. Returns defaultVal
// if the key is absent or the value is not a bool.
func BoolFrom(config map[string]any, key string, defaultVal bool) bool {
	v, ok := config[key].(bool)
	if !ok {
		return defaultVal
	}
	return v
}

// MapFrom returns the map[string]any value for key in config. Returns nil
// if the key is absent or the value is not a map.
func MapFrom(config map[string]any, key string) map[string]any {
	v, ok := config[key].(map[string]any)
	if !ok {
		return nil
	}
	return v
}

// LowerSet builds a membership set of the lowercased elements of ss, for
// case-insensitive lookups.
func LowerSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = struct{}{}
	}
	return m
}

// HasWhitespace reports whether s contains a space, tab, newline, or
// carriage return character.
func HasWhitespace(s string) bool {
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}
