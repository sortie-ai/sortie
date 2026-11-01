package issuekit

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestNormalizeLabels_nil(t *testing.T) {
	t.Parallel()

	got := NormalizeLabels(nil)
	if got == nil {
		t.Fatal("NormalizeLabels(nil) = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("NormalizeLabels(nil) len = %d, want 0", len(got))
	}
}

func TestNormalizeLabels_lowercase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"single uppercase", []string{"BUG"}, []string{"bug"}},
		{"mixed case", []string{"Feature", "AUTH"}, []string{"feature", "auth"}},
		{"already lowercase", []string{"done"}, []string{"done"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := NormalizeLabels(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("NormalizeLabels(%v) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("NormalizeLabels(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNormalizeLabels_duplicatesPreserved(t *testing.T) {
	t.Parallel()

	input := []string{"BUG", "bug", "Bug"}
	got := NormalizeLabels(input)
	if len(got) != 3 {
		t.Fatalf("NormalizeLabels(%v) len = %d, want 3", input, len(got))
	}
	for i, label := range got {
		if label != "bug" {
			t.Errorf("NormalizeLabels(%v)[%d] = %q, want %q", input, i, label, "bug")
		}
	}
}

func TestNormalizeLabels_empty(t *testing.T) {
	t.Parallel()

	got := NormalizeLabels([]string{})
	if got == nil {
		t.Fatal("NormalizeLabels([]) = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("NormalizeLabels([]) = %v, want empty", got)
	}
}

func TestParsePriorityIntStrict_valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input json.RawMessage
		want  int
	}{
		{"zero", json.RawMessage("0"), 0},
		{"positive", json.RawMessage("42"), 42},
		{"negative", json.RawMessage("-1"), -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParsePriorityIntStrict(tt.input)
			if got == nil {
				t.Fatalf("ParsePriorityIntStrict(%s) = nil, want %d", tt.input, tt.want)
			}
			if *got != tt.want {
				t.Errorf("ParsePriorityIntStrict(%s) = %d, want %d", tt.input, *got, tt.want)
			}
		})
	}
}

func TestParsePriorityIntStrict_rejectsFloat(t *testing.T) {
	t.Parallel()

	got := ParsePriorityIntStrict(json.RawMessage("3.14"))
	if got != nil {
		t.Errorf("ParsePriorityIntStrict(3.14) = %d, want nil", *got)
	}
}

func TestParsePriorityIntStrict_rejectsString(t *testing.T) {
	t.Parallel()

	got := ParsePriorityIntStrict(json.RawMessage(`"42"`))
	if got != nil {
		t.Errorf(`ParsePriorityIntStrict("42") = %d, want nil`, *got)
	}
}

func TestParsePriorityIntStrict_rejectsNull(t *testing.T) {
	t.Parallel()

	got := ParsePriorityIntStrict(json.RawMessage("null"))
	if got != nil {
		t.Errorf("ParsePriorityIntStrict(null) = %d, want nil", *got)
	}
}

func TestParsePriorityIntStrict_rejectsBoolean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input json.RawMessage
	}{
		{"true", json.RawMessage("true")},
		{"false", json.RawMessage("false")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParsePriorityIntStrict(tt.input)
			if got != nil {
				t.Errorf("ParsePriorityIntStrict(%s) = %d, want nil", tt.input, *got)
			}
		})
	}
}

func TestParsePriorityIntFromString_valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"positive", "42", 42},
		{"zero", "0", 0},
		{"negative", "-5", -5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParsePriorityIntFromString(tt.input)
			if got == nil {
				t.Fatalf("ParsePriorityIntFromString(%q) = nil, want %d", tt.input, tt.want)
			}
			if *got != tt.want {
				t.Errorf("ParsePriorityIntFromString(%q) = %d, want %d", tt.input, *got, tt.want)
			}
		})
	}
}

func TestParsePriorityIntFromString_emptyReturnsNil(t *testing.T) {
	t.Parallel()

	got := ParsePriorityIntFromString("")
	if got != nil {
		t.Errorf("ParsePriorityIntFromString(%q) = %d, want nil", "", *got)
	}
}

func TestParsePriorityIntFromString_nonIntReturnsNil(t *testing.T) {
	t.Parallel()

	inputs := []string{"3.14", "abc", "1e2"}
	for _, input := range inputs {
		got := ParsePriorityIntFromString(input)
		if got != nil {
			t.Errorf("ParsePriorityIntFromString(%q) = %d, want nil", input, *got)
		}
	}
}

func TestNormalizeComments_nil(t *testing.T) {
	t.Parallel()

	got := NormalizeComments(nil)
	if got == nil {
		t.Fatal("NormalizeComments(nil) = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("NormalizeComments(nil) len = %d, want 0", len(got))
	}
}

func TestNormalizeComments_order(t *testing.T) {
	t.Parallel()

	in := []SourceComment{
		{ID: "1", Author: "Alice", AuthorID: "alice", AuthorKind: domain.CommentAuthorBot, Body: "first", CreatedAt: "2025-01-01", UpdatedAt: "2025-01-03"},
		{ID: "2", Author: "Bob", Body: "second", CreatedAt: "2025-01-02"},
	}
	want := []domain.Comment{
		{ID: "1", Author: "Alice", AuthorID: "alice", AuthorKind: domain.CommentAuthorBot, Body: "first", CreatedAt: "2025-01-01", UpdatedAt: "2025-01-03"},
		{ID: "2", Author: "Bob", Body: "second", CreatedAt: "2025-01-02"},
	}

	got := NormalizeComments(in)

	if !slices.Equal(got, want) {
		t.Errorf("NormalizeComments(%+v) = %+v, want %+v", in, got, want)
	}
}
