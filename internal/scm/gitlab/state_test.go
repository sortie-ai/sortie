package gitlab

import (
	"slices"
	"testing"
)

func TestLabelVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		labels  []string
		lowered string
		want    []string
	}{
		{"no matches", []string{"bug", "documentation"}, "review", nil},
		{"single match", []string{"backlog", "review"}, "review", []string{"review"}},
		{"multiple case-variant matches preserve input order", []string{"REVIEW", "bug", "review", "Review"}, "review", []string{"REVIEW", "review", "Review"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := labelVariants(tt.labels, tt.lowered)
			if !slices.Equal(got, tt.want) {
				t.Errorf("labelVariants(%v, %q) = %v, want %v", tt.labels, tt.lowered, got, tt.want)
			}
		})
	}
}
