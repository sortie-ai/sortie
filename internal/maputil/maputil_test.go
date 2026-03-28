package maputil

import (
	"reflect"
	"testing"
)

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    map[string]any
		want []string
	}{
		{
			name: "nil map",
			m:    nil,
			want: []string{},
		},
		{
			name: "empty map",
			m:    map[string]any{},
			want: []string{},
		},
		{
			name: "single entry",
			m:    map[string]any{"only": 1},
			want: []string{"only"},
		},
		{
			name: "multiple entries sorted",
			m:    map[string]any{"zebra": 1, "apple": 2, "mango": 3},
			want: []string{"apple", "mango", "zebra"},
		},
		{
			name: "already sorted input",
			m:    map[string]any{"a": 1, "b": 2, "c": 3},
			want: []string{"a", "b", "c"},
		},
		{
			name: "reverse-sorted keys",
			m:    map[string]any{"c": 1, "b": 2, "a": 3},
			want: []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := SortedKeys(tt.m)

			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SortedKeys(%v) = %v, want %v", tt.m, got, tt.want)
			}
		})
	}
}

func TestSortedKeysTyped(t *testing.T) {
	t.Parallel()

	// Verify the generic constraint works with a concrete non-any type.
	m := map[string]int{"z": 26, "a": 1, "m": 13}
	got := SortedKeys(m)
	want := []string{"a", "m", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SortedKeys[int](%v) = %v, want %v", m, got, want)
	}
}
