// Package typeutil provides type coercion utilities for loosely-typed values
// produced by JSON and YAML decoders. These helpers are shared across adapter
// packages that cannot import each other.
package typeutil

import "unicode/utf8"

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

// StringFrom returns the string value for key in config. Returns "" if
// the key is absent or the value is not a string.
func StringFrom(config map[string]any, key string) string {
	v, ok := config[key].(string)
	if !ok {
		return ""
	}
	return v
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
