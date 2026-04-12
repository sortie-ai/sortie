// Package typeutil provides type coercion utilities for loosely-typed values
// produced by JSON and YAML decoders. These helpers are shared across adapter
// packages that cannot import each other.
package typeutil

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
