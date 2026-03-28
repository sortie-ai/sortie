package prompt

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// mustParse compiles a template body or fatals the test.
func mustParseAnalyze(t *testing.T, body string) *Template {
	t.Helper()
	tmpl, err := Parse(body, "test.md", 0)
	if err != nil {
		t.Fatalf("Parse(%q): %v", body, err)
	}
	return tmpl
}

// TestAnalyzeTemplate verifies all three warning classes, scope edge
// cases, and boundary conditions defined in the spec.
func TestAnalyzeTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantCount int
		wantKind  WarnKind // checked only when wantCount > 0
		wantNode  string   // checked only when non-empty
	}{
		// Check 1: dot-context misuse inside range/with.
		{
			name:      "DotContextRange",
			body:      `{{ range .issue.labels }}{{ .issue.title }}{{ end }}`,
			wantCount: 1,
			wantKind:  WarnDotContext,
			wantNode:  ".issue.title",
		},
		{
			// The inner {{ range .issue.labels }} pipe is itself inside the
			// outer range body (scopeDepth=1), so .issue.labels fires
			// WarnDotContext; then .run.turn_number at scopeDepth=2 fires again.
			name:      "DotContextRangeNested",
			body:      `{{ range .issue.labels }}{{ range .issue.labels }}{{ .run.turn_number }}{{ end }}{{ end }}`,
			wantCount: 2,
			wantKind:  WarnDotContext,
			wantNode:  ".issue.labels",
		},
		{
			name:      "DotContextWith",
			body:      `{{ with .issue.parent }}{{ .issue.title }}{{ end }}`,
			wantCount: 1,
			wantKind:  WarnDotContext,
			wantNode:  ".issue.title",
		},
		// if does NOT redefine dot — no warning expected.
		{
			name:      "DotContextIfNoWarn",
			body:      `{{ if .issue.parent }}{{ .issue.title }}{{ end }}`,
			wantCount: 0,
		},
		// else body of range is outside the redefined-dot scope.
		{
			name:      "DotContextElseNoWarn",
			body:      `{{ range .issue.labels }}ok{{ else }}{{ .issue.title }}{{ end }}`,
			wantCount: 0,
		},
		// Iterating element itself (dot node) is fine.
		{
			name:      "DotContextRangePipeNoWarn",
			body:      `{{ range .issue.labels }}{{ . }}{{ end }}`,
			wantCount: 0,
		},
		// $.issue.title uses root-qualified $, not dot — no warning.
		{
			name:      "DollarEscapeNoWarn",
			body:      `{{ range .issue.labels }}{{ $.issue.title }}{{ end }}`,
			wantCount: 0,
		},

		// Check 2: unknown top-level variable.
		{
			name:      "UnknownTopLevel",
			body:      `{{ .config }}`,
			wantCount: 1,
			wantKind:  WarnUnknownVar,
			wantNode:  ".config",
		},
		// $.config is always invalid regardless of scope.
		{
			name:      "UnknownTopLevelDollar",
			body:      `{{ range .issue.labels }}{{ $.config }}{{ end }}`,
			wantCount: 1,
			wantKind:  WarnUnknownVar,
			wantNode:  "$.config",
		},
		// All three top-level keys are valid — no warning.
		{
			name:      "KnownTopLevelNoWarn",
			body:      `{{ .issue.title }}{{ .attempt }}{{ .run.turn_number }}`,
			wantCount: 0,
		},

		// Check 3: unknown sub-field of a known top-level key.
		{
			name:      "UnknownSubFieldIssue",
			body:      `{{ .issue.nonexistent }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.nonexistent",
		},
		{
			name:      "UnknownSubFieldRun",
			body:      `{{ .run.foo }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".run.foo",
		},
		// attempt is a scalar — any sub-field is invalid.
		{
			name:      "AttemptSubField",
			body:      `{{ .attempt.something }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".attempt.something",
		},
		// .issue.parent.identifier is a valid level-3 chain.
		{
			name:      "ValidNestedField",
			body:      `{{ .issue.parent.identifier }}`,
			wantCount: 0,
		},
		// .issue.parent exists but .nonexistent is not in its nested schema.
		{
			name:      "UnknownNestedField",
			body:      `{{ .issue.parent.nonexistent }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.parent.nonexistent",
		},
		// .issue.title is a scalar — chaining further is invalid.
		{
			name:      "ScalarNestedAccess",
			body:      `{{ .issue.title.something }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.title.something",
		},
		// Slice fields are opaque scalars in the schema — sub-access is flagged.
		{
			name:      "SliceSubFieldBlocked",
			body:      `{{ .issue.comments.author }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.comments.author",
		},
		{
			name:      "SliceBlockedBySubFieldBlocked",
			body:      `{{ .issue.blocked_by.state }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.blocked_by.state",
		},
		// $.issue.comments is a valid top-level reference (no sub-access).
		{
			name:      "KnownSliceRefNoWarn",
			body:      `{{ range .issue.comments }}{{ $.issue.comments }}{{ end }}`,
			wantCount: 0,
		},
		// $.run.nonexistent — dollar-prefixed unknown sub-field.
		{
			name:      "DollarUnknownSubField",
			body:      `{{ $.run.nonexistent }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  "$.run.nonexistent",
		},
		// FuncMap calls (join, lower, toJSON) must not produce warnings.
		{
			name:      "FuncMapNoWarn",
			body:      `{{ .issue.labels | join "," }}{{ .issue.title | lower }}{{ .issue | toJSON }}`,
			wantCount: 0,
		},
		// Clean template with if/else and known fields — no warnings.
		{
			name:      "CleanTemplate",
			body:      `{{ if .attempt }}retry{{ else }}{{ .issue.title }}{{ end }}`,
			wantCount: 0,
		},
		// Section 8.2: range body triggers both dot-context warnings (two
		// separate FieldNode references — both are top-level keys inside range).
		{
			name:      "MultipleWarnings",
			body:      `{{ range .issue.labels }}{{ .issue.nonexistent }}{{ .run.turn_number }}{{ end }}`,
			wantCount: 2,
			wantKind:  WarnDotContext,
		},
		// Boundary: nil template must return nil without panic.
		{
			name:      "NilTemplate",
			body:      "", // will be overridden in loop
			wantCount: 0,
		},
		// Boundary: empty body — produces a valid template with empty tree.
		{
			name:      "EmptyTemplate",
			body:      "",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var warnings []TemplateWarning

			if tt.name == "NilTemplate" {
				warnings = AnalyzeTemplate(nil)
			} else {
				tmpl := mustParseAnalyze(t, tt.body)
				warnings = AnalyzeTemplate(tmpl)
			}

			if len(warnings) != tt.wantCount {
				t.Fatalf("AnalyzeTemplate(%q) returned %d warnings, want %d: %v",
					tt.body, len(warnings), tt.wantCount, warnings)
			}
			if tt.wantCount > 0 {
				if warnings[0].Kind != tt.wantKind {
					t.Errorf("AnalyzeTemplate(%q)[0].Kind = %v, want %v",
						tt.body, warnings[0].Kind, tt.wantKind)
				}
				if tt.wantNode != "" && warnings[0].Node != tt.wantNode {
					t.Errorf("AnalyzeTemplate(%q)[0].Node = %q, want %q",
						tt.body, warnings[0].Node, tt.wantNode)
				}
			}
		})
	}
}

// TestTemplateFieldSchemaMatchesDomain cross-checks the static schema
// registry against the actual domain model to detect schema drift.
func TestTemplateFieldSchemaMatchesDomain(t *testing.T) {
	t.Parallel()

	// Verify templateFieldSchema has exactly the three expected top-level keys.
	wantTopLevel := []string{"issue", "attempt", "run"}
	if len(templateFieldSchema) != len(wantTopLevel) {
		t.Errorf("templateFieldSchema has %d top-level keys, want %d (%v)",
			len(templateFieldSchema), len(wantTopLevel), wantTopLevel)
	}
	for _, key := range wantTopLevel {
		if _, ok := templateFieldSchema[key]; !ok {
			t.Errorf("templateFieldSchema missing top-level key %q", key)
		}
	}

	// Cross-check issue fields: every key returned by ToTemplateMap must be
	// present in templateFieldSchema["issue"].
	issueSchema := templateFieldSchema["issue"]
	issueMap := (&domain.Issue{}).ToTemplateMap()
	for k := range issueMap {
		if _, ok := issueSchema[k]; !ok {
			t.Errorf("Issue.ToTemplateMap() key %q not present in templateFieldSchema[\"issue\"]", k)
		}
	}

	// Cross-check run fields: every key returned by runContextToMap must be
	// present in templateFieldSchema["run"].
	runSchema := templateFieldSchema["run"]
	runMap := runContextToMap(RunContext{})
	for k := range runMap {
		if _, ok := runSchema[k]; !ok {
			t.Errorf("runContextToMap() key %q not present in templateFieldSchema[\"run\"]", k)
		}
	}
}
