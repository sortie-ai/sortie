package typeutil

import "testing"

func TestExtractStringSlice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   any
		want    []string
		wantNil bool
	}{
		{name: "nil", input: nil, want: nil, wantNil: true},
		{name: "[]any strings", input: []any{"A", "B"}, want: []string{"A", "B"}},
		{name: "[]string passthrough", input: []string{"X", "Y"}, want: []string{"X", "Y"}},
		{name: "[]any mixed types", input: []any{"ok", 42, "yes"}, want: []string{"ok", "yes"}},
		{name: "[]any empty", input: []any{}, want: []string{}},
		{name: "wrong type int", input: 42, want: nil, wantNil: true},
		{name: "wrong type string", input: "single", want: nil, wantNil: true},
		{name: "[]any with nil element", input: []any{nil, "a", nil}, want: []string{"a"}},
		{name: "[]any all non-string", input: []any{1, 2.5, true}, want: []string{}},
		{name: "map type", input: map[string]any{"k": "v"}, want: nil, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ExtractStringSlice(tt.input)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("ExtractStringSlice() = %v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Fatalf("ExtractStringSlice() = nil, want non-nil %v", tt.want)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("ExtractStringSlice() len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ExtractStringSlice()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
