package typeutil

import (
	"testing"
	"unicode/utf8"
)

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

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{name: "empty string", s: "", maxLen: 10, want: ""},
		{name: "below limit", s: "hello", maxLen: 10, want: "hello"},
		{name: "exact limit not truncated", s: "hello", maxLen: 5, want: "hello"},
		{name: "above limit gets ellipsis", s: "hello world", maxLen: 5, want: "hello…"},
		{name: "multi-byte CJK runes counted correctly", s: "日本語テスト", maxLen: 3, want: "日本語…"},
		{name: "emoji counted as single rune", s: "ab🎉cd", maxLen: 3, want: "ab🎉…"},
		{name: "maxLen zero returns ellipsis", s: "abc", maxLen: 0, want: "…"},
		{name: "unicode two-byte runes", s: "héllo", maxLen: 4, want: "héll…"},
		{name: "result rune count is maxLen plus one", s: strings("x", 20), maxLen: 5, want: strings("x", 5) + "…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateRunes(tt.s, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
			if utf8.RuneCountInString(got) > tt.maxLen+1 {
				t.Errorf("TruncateRunes(%q, %d): rune count %d exceeds maxLen+1", tt.s, tt.maxLen, utf8.RuneCountInString(got))
			}
		})
	}
}

// strings returns s repeated n times.
func strings(s string, n int) string {
	result := make([]byte, len(s)*n)
	for i := range n {
		copy(result[i*len(s):], s)
	}
	return string(result)
}

func TestStringFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		key    string
		want   string
	}{
		{name: "key present string value", config: map[string]any{"k": "hello"}, key: "k", want: "hello"},
		{name: "key present non-string value", config: map[string]any{"k": 42}, key: "k", want: ""},
		{name: "key absent", config: map[string]any{}, key: "missing", want: ""},
		{name: "nil value for key", config: map[string]any{"k": nil}, key: "k", want: ""},
		{name: "empty string value", config: map[string]any{"k": ""}, key: "k", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := StringFrom(tt.config, tt.key)
			if got != tt.want {
				t.Errorf("StringFrom(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestIntFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		config     map[string]any
		key        string
		defaultVal int
		want       int
	}{
		{name: "int value", config: map[string]any{"k": 7}, key: "k", defaultVal: 0, want: 7},
		{name: "float64 whole number", config: map[string]any{"k": float64(3)}, key: "k", defaultVal: 0, want: 3},
		{name: "float64 fractional returns default", config: map[string]any{"k": 3.5}, key: "k", defaultVal: -1, want: -1},
		{name: "key absent returns default", config: map[string]any{}, key: "missing", defaultVal: 99, want: 99},
		{name: "non-numeric type returns default", config: map[string]any{"k": "five"}, key: "k", defaultVal: 42, want: 42},
		{name: "bool type returns default", config: map[string]any{"k": true}, key: "k", defaultVal: 5, want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IntFrom(tt.config, tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("IntFrom(%q) = %d, want %d", tt.key, got, tt.want)
			}
		})
	}
}

func TestFloatFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		config     map[string]any
		key        string
		defaultVal float64
		want       float64
	}{
		{name: "float64 value", config: map[string]any{"k": 1.5}, key: "k", defaultVal: 0, want: 1.5},
		{name: "int value coerced", config: map[string]any{"k": 3}, key: "k", defaultVal: 0, want: 3.0},
		{name: "key absent returns default", config: map[string]any{}, key: "missing", defaultVal: 9.9, want: 9.9},
		{name: "non-numeric type returns default", config: map[string]any{"k": "fast"}, key: "k", defaultVal: -1.0, want: -1.0},
		{name: "zero float64", config: map[string]any{"k": float64(0)}, key: "k", defaultVal: 5.0, want: 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FloatFrom(tt.config, tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("FloatFrom(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestBoolFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		config     map[string]any
		key        string
		defaultVal bool
		want       bool
	}{
		{name: "true value", config: map[string]any{"k": true}, key: "k", defaultVal: false, want: true},
		{name: "false value", config: map[string]any{"k": false}, key: "k", defaultVal: true, want: false},
		{name: "key absent returns default true", config: map[string]any{}, key: "missing", defaultVal: true, want: true},
		{name: "key absent returns default false", config: map[string]any{}, key: "missing", defaultVal: false, want: false},
		{name: "non-bool type returns default", config: map[string]any{"k": "yes"}, key: "k", defaultVal: true, want: true},
		{name: "int type returns default", config: map[string]any{"k": 1}, key: "k", defaultVal: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BoolFrom(tt.config, tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("BoolFrom(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestMapFrom(t *testing.T) {
	t.Parallel()

	inner := map[string]any{"nested": "value"}

	tests := []struct {
		name    string
		config  map[string]any
		key     string
		wantNil bool
		want    map[string]any
	}{
		{name: "present map", config: map[string]any{"k": inner}, key: "k", wantNil: false, want: inner},
		{name: "key absent returns nil", config: map[string]any{}, key: "missing", wantNil: true},
		{name: "wrong type returns nil", config: map[string]any{"k": "not-a-map"}, key: "k", wantNil: true},
		{name: "nil value returns nil", config: map[string]any{"k": nil}, key: "k", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MapFrom(tt.config, tt.key)
			if tt.wantNil {
				if got != nil {
					t.Errorf("MapFrom(%q) = %v, want nil", tt.key, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("MapFrom(%q) = nil, want non-nil", tt.key)
			}
			if len(got) != len(tt.want) {
				t.Errorf("MapFrom(%q) len = %d, want %d", tt.key, len(got), len(tt.want))
			}
		})
	}
}
