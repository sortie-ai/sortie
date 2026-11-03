package issuekit

import (
	"math"
	"reflect"
	"testing"
)

func TestPriorityFromLabels(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		labels     []string
		priorities map[string]int
		want       *int
	}{
		{name: "no labels", priorities: map[string]int{"p1": 1}},
		{name: "one mapped", labels: []string{"p1"}, priorities: map[string]int{"p1": 1}, want: new(1)},
		{name: "several mapped", labels: []string{"p4", "p1", "p2"}, priorities: map[string]int{"p1": 1, "p2": 2, "p4": 4}, want: new(1)},
		{name: "case insensitive", labels: NormalizeLabels([]string{"PRIORITY/High"}), priorities: map[string]int{"priority/high": 2}, want: new(2)},
		{name: "unmapped", labels: []string{"bug"}, priorities: map[string]int{"p1": 1}},
		{name: "no map", labels: []string{"p1"}},
		{name: "no globs", labels: []string{"priority/high"}, priorities: map[string]int{"priority/*": 1}},
		{name: "literal glob characters", labels: []string{"priority/*"}, priorities: map[string]int{"priority/*": 1}, want: new(1)},
		{name: "zero", labels: []string{"p0"}, priorities: map[string]int{"p0": 0}, want: new(0)},
		{name: "negative", labels: []string{"urgent", "p0"}, priorities: map[string]int{"urgent": -1, "p0": 0}, want: new(-1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PriorityFromLabels(tt.labels, tt.priorities)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PriorityFromLabels(%v, %v) = %v, want %v", tt.labels, tt.priorities, got, tt.want)
			}
		})
	}
}

func TestParsePriorityLabels(t *testing.T) {
	t.Parallel()
	for _, raw := range []any{map[string]any{"P1": 1, "low": int64(math.MaxInt)}, map[string]int{"P1": 1, "low": math.MaxInt}} {
		t.Run("map", func(t *testing.T) {
			t.Parallel()
			got, err := ParsePriorityLabels(raw)
			want := map[string]int{"p1": 1, "low": math.MaxInt}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("ParsePriorityLabels(%v) = %v, %v, want %v, nil", raw, got, err, want)
			}
		})
	}
}
