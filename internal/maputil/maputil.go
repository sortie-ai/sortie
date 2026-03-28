// Package maputil provides generic map utility functions shared across
// internal packages that cannot import each other.
package maputil

import "sort"

// SortedKeys returns the keys of m in sorted order for deterministic
// iteration.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
