package orchestrator

import (
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
)

// TestContinuationBuilderFieldsMatchSchema verifies that every field the
// three map-shaped continuation builders owned by this package emit is
// accepted by the template analyzer as a known sub-field of its top-level
// key. It reaches the analyzer only through the exported prompt.Parse and
// prompt.AnalyzeTemplate surface, so it fails when a builder gains or
// renames a field the analyzer's schema does not carry.
//
// review_comments and bot_review_comments are excluded: their builder
// returns a list, so element fields are addressed inside {{ range }},
// where the analyzer does not validate field names — a generated check on
// them would assert nothing. ci_failure is excluded because its builder
// lives in internal/domain, covered instead by
// TestTemplateFieldSchemaMatchesDomain in internal/prompt.
func TestContinuationBuilderFieldsMatchSchema(t *testing.T) {
	t.Parallel()

	requestedAt := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	tests := []struct {
		topKey string
		fields map[string]any
	}{
		{
			topKey: "merge_conflict",
			fields: buildMergeConflictTemplateMap(
				&MergeConflictReactionData{PRNumber: 42, Branch: "feature/x"},
				domain.PRMergeStatus{HeadSHA: "abc123", BaseBranch: "develop"},
			),
		},
		{
			topKey: "label_review",
			fields: buildLabelReviewMap(
				&LabelReviewReactionData{PRNumber: 99, Owner: "acme", Repo: "widgets"},
				"alice",
				requestedAt,
			),
		},
		{
			topKey: "label_fix",
			fields: buildLabelFixMap(
				&LabelFixReactionData{PRNumber: 99, Owner: "acme", Repo: "widgets", Branch: "feature/99"},
				"alice",
				requestedAt,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.topKey, func(t *testing.T) {
			t.Parallel()

			for field := range tt.fields {
				body := "{{ ." + tt.topKey + "." + field + " }}"
				tmpl, err := prompt.Parse(body, "test.md", 0)
				if err != nil {
					t.Fatalf("prompt.Parse(%q): %v", body, err)
				}
				if warnings := prompt.AnalyzeTemplate(tmpl); len(warnings) != 0 {
					t.Errorf("prompt.AnalyzeTemplate(%q) = %v, want no warnings", body, warnings)
				}
			}
		})
	}
}
