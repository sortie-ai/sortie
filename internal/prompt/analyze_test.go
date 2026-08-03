package prompt

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// mustParseAnalyze compiles a template body or fatals the test.
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
		name            string
		body            string
		wantCount       int
		wantKind        WarnKind // checked only when wantCount > 0
		wantNode        string   // checked only when non-empty
		wantMsgContains string   // checked only when non-empty
	}{
		// Dot-context misuse inside range/with.
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

		// Unknown top-level variable.
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
		// A continuation-era unrecognized name must still warn, and its
		// message must enumerate the full recognized set.
		{
			name:            "UnrecognizedContinuationLikeName",
			body:            `{{ .not_a_reaction }}`,
			wantCount:       1,
			wantKind:        WarnUnknownVar,
			wantNode:        ".not_a_reaction",
			wantMsgContains: topLevelList,
		},
		{
			name:      "UnrecognizedContinuationLikeNameDollar",
			body:      `{{ $.not_a_reaction }}`,
			wantCount: 1,
			wantKind:  WarnUnknownVar,
			wantNode:  "$.not_a_reaction",
		},
		// All three top-level keys are valid — no warning.
		{
			name:      "KnownTopLevelNoWarn",
			body:      `{{ .issue.title }}{{ .attempt }}{{ .run.turn_number }}`,
			wantCount: 0,
		},

		// Unknown sub-field of a known top-level key.
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
		// Depth-4+ chains: level-3 fields are scalars, further chaining is invalid.
		{
			name:      "Depth4ChainKnownLevel3",
			body:      `{{ .issue.parent.identifier.extra }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.parent.identifier.extra",
		},
		{
			name:      "Depth4ChainDollarKnownLevel3",
			body:      `{{ $.issue.parent.id.surplus }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  "$.issue.parent.id.surplus",
		},
		// Depth-5 chain still produces exactly one warning.
		{
			name:      "Depth5ChainKnownLevel3",
			body:      `{{ .issue.parent.identifier.a.b }}`,
			wantCount: 1,
			wantKind:  WarnUnknownField,
			wantNode:  ".issue.parent.identifier.a.b",
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
		// Range body triggers both dot-context warnings (two
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
				if tt.wantMsgContains != "" && !strings.Contains(warnings[0].Message, tt.wantMsgContains) {
					t.Errorf("AnalyzeTemplate(%q)[0].Message = %q, want to contain %q",
						tt.body, warnings[0].Message, tt.wantMsgContains)
				}
			}
		})
	}
}

// TestAnalyzeTemplateDepth4ChainMessage verifies the full warning content
// for depth-4+ field chains: kind, node text, and message substring.
func TestAnalyzeTemplateDepth4ChainMessage(t *testing.T) {
	t.Parallel()

	tmpl := mustParseAnalyze(t, `{{ .issue.parent.identifier.extra }}`)
	warnings := AnalyzeTemplate(tmpl)

	if len(warnings) != 1 {
		t.Fatalf("AnalyzeTemplate returned %d warnings, want 1: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.Kind != WarnUnknownField {
		t.Errorf("Kind = %v, want WarnUnknownField", w.Kind)
	}
	if w.Node != ".issue.parent.identifier.extra" {
		t.Errorf("Node = %q, want %q", w.Node, ".issue.parent.identifier.extra")
	}
	const wantSubstr = "scalar with no sub-fields"
	if !strings.Contains(w.Message, wantSubstr) {
		t.Errorf("Message = %q, want to contain %q", w.Message, wantSubstr)
	}
	// The message must name the parent scalar field (issue.parent.identifier).
	const wantBase = "issue.parent.identifier"
	if !strings.Contains(w.Message, wantBase) {
		t.Errorf("Message = %q, want to contain %q", w.Message, wantBase)
	}
}

// TestAnalyzeTemplateNestedRangeAllWarnings verifies the DotContextRangeNested
// case fully: both warnings must be WarnDotContext and reference the correct
// node expressions.
func TestAnalyzeTemplateNestedRangeAllWarnings(t *testing.T) {
	t.Parallel()

	// Outer range body contains an inner range whose pipe (.issue.labels)
	// fires at scopeDepth=1; the inner body's .run.turn_number fires at
	// scopeDepth=2.
	body := `{{ range .issue.labels }}{{ range .issue.labels }}{{ .run.turn_number }}{{ end }}{{ end }}`
	tmpl := mustParseAnalyze(t, body)
	warnings := AnalyzeTemplate(tmpl)

	if len(warnings) != 2 {
		t.Fatalf("AnalyzeTemplate returned %d warnings, want 2: %v", len(warnings), warnings)
	}
	want := []struct {
		kind WarnKind
		node string
	}{
		{WarnDotContext, ".issue.labels"},
		{WarnDotContext, ".run.turn_number"},
	}
	for i, w := range warnings {
		if w.Kind != want[i].kind {
			t.Errorf("warnings[%d].Kind = %v, want %v", i, w.Kind, want[i].kind)
		}
		if w.Node != want[i].node {
			t.Errorf("warnings[%d].Node = %q, want %q", i, w.Node, want[i].node)
		}
	}
}

// TestTemplateFieldSchemaMatchesDomain cross-checks the static schema
// registry against the actual domain model to detect schema drift.
func TestTemplateFieldSchemaMatchesDomain(t *testing.T) {
	t.Parallel()

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

	// Cross-check ci_failure fields: every key returned by
	// CIResult.ToTemplateMap must be present in
	// templateFieldSchema["ci_failure"].
	ciFailureSchema := templateFieldSchema["ci_failure"]
	ciFailureMap := (&domain.CIResult{}).ToTemplateMap()
	for k := range ciFailureMap {
		if _, ok := ciFailureSchema[k]; !ok {
			t.Errorf("CIResult.ToTemplateMap() key %q not present in templateFieldSchema[\"ci_failure\"]", k)
		}
	}
}

// TestTemplateFieldSchemaCoversRecognizedKeys verifies that
// templateFieldSchema carries exactly one entry per recognized top-level
// key, computed as coreKeys plus continuationKeys, with no missing entry,
// no extra entry, and no shadowing between the two source lists.
func TestTemplateFieldSchemaCoversRecognizedKeys(t *testing.T) {
	t.Parallel()

	want := make(map[string]struct{}, len(coreKeys)+len(continuationKeys))
	for _, k := range coreKeys {
		want[k] = struct{}{}
	}
	for _, k := range continuationKeys {
		want[k] = struct{}{}
	}

	for k := range want {
		if _, ok := templateFieldSchema[k]; !ok {
			t.Errorf("templateFieldSchema is missing an entry for recognized key %q; add a field schema for it", k)
		}
	}
	for k := range templateFieldSchema {
		if _, ok := want[k]; !ok {
			t.Errorf("templateFieldSchema has entry %q, which is not in coreKeys or continuationKeys", k)
		}
	}
	if got, wantLen := len(want), len(coreKeys)+len(continuationKeys); got != wantLen {
		t.Errorf("len(coreKeys)+len(continuationKeys) = %d, but the computed recognized set has %d entries; a continuation key duplicates another entry or shadows a core key",
			wantLen, got)
	}
}

// TestContinuationKeysRecognizedBare verifies that every entry of
// continuationKeys is recognized by AnalyzeTemplate as a bare top-level
// reference, both in its plain and its $-qualified form.
func TestContinuationKeysRecognizedBare(t *testing.T) {
	t.Parallel()

	for _, key := range continuationKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			for _, body := range []string{
				"{{ if ." + key + " }}x{{ end }}",
				"{{ if $." + key + " }}x{{ end }}",
			} {
				tmpl := mustParseAnalyze(t, body)
				if warnings := AnalyzeTemplate(tmpl); len(warnings) != 0 {
					t.Errorf("AnalyzeTemplate(%q) = %v, want no warnings", body, warnings)
				}
			}
		})
	}
}

// TestContinuationKeySubFieldBehavior verifies per-key sub-field behavior
// for every continuation key: a map-shaped key accepts each of its
// documented fields and flags an invented field name with the real field
// list; a list-shaped key accepts an element field inside {{ range }} and
// flags direct sub-field access as unknown.
func TestContinuationKeySubFieldBehavior(t *testing.T) {
	t.Parallel()

	mapShaped := []struct {
		key    string
		fields []string
	}{
		{"ci_failure", []string{"status", "check_runs", "log_excerpt", "failing_count", "ref"}},
		{"merge_conflict", []string{"pr_number", "branch", "head_sha", "base"}},
		{"label_review", []string{"pr_number", "owner", "repo", "actor", "requested_at"}},
		{"label_fix", []string{"pr_number", "owner", "repo", "branch", "actor", "requested_at"}},
	}

	for _, tt := range mapShaped {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			for _, field := range tt.fields {
				body := "{{ ." + tt.key + "." + field + " }}"
				tmpl := mustParseAnalyze(t, body)
				if warnings := AnalyzeTemplate(tmpl); len(warnings) != 0 {
					t.Errorf("AnalyzeTemplate(%q) = %v, want no warnings", body, warnings)
				}
			}

			body := "{{ ." + tt.key + ".not_a_field }}"
			tmpl := mustParseAnalyze(t, body)
			warnings := AnalyzeTemplate(tmpl)
			if len(warnings) != 1 {
				t.Fatalf("AnalyzeTemplate(%q) returned %d warnings, want 1: %v", body, len(warnings), warnings)
			}
			if warnings[0].Kind != WarnUnknownField {
				t.Errorf("AnalyzeTemplate(%q)[0].Kind = %v, want WarnUnknownField", body, warnings[0].Kind)
			}
			for _, field := range tt.fields {
				if !strings.Contains(warnings[0].Message, field) {
					t.Errorf("AnalyzeTemplate(%q)[0].Message = %q, want to contain field %q", body, warnings[0].Message, field)
				}
			}
		})
	}

	listShaped := []string{"review_comments", "bot_review_comments"}
	for _, key := range listShaped {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			rangeBody := "{{ range ." + key + " }}{{ .body }}{{ end }}"
			tmpl := mustParseAnalyze(t, rangeBody)
			if warnings := AnalyzeTemplate(tmpl); len(warnings) != 0 {
				t.Errorf("AnalyzeTemplate(%q) = %v, want no warnings", rangeBody, warnings)
			}

			subFieldBody := "{{ ." + key + ".body }}"
			tmpl = mustParseAnalyze(t, subFieldBody)
			warnings := AnalyzeTemplate(tmpl)
			if len(warnings) != 1 {
				t.Fatalf("AnalyzeTemplate(%q) returned %d warnings, want 1: %v", subFieldBody, len(warnings), warnings)
			}
			if warnings[0].Kind != WarnUnknownField {
				t.Errorf("AnalyzeTemplate(%q)[0].Kind = %v, want WarnUnknownField", subFieldBody, warnings[0].Kind)
			}
		})
	}
}

// TestTopLevelKeysMatchRendererSeededKeys anchors the recognized set on
// what Template.Render actually seeds, rather than on the same two
// literals the analyzer derives topLevelKeys from. It renders a template
// that enumerates the data map with no RenderOption applied and compares
// the resulting key set against topLevelKeys in both directions.
func TestTopLevelKeysMatchRendererSeededKeys(t *testing.T) {
	t.Parallel()

	const sep = "\x00"
	tmpl := mustParseAnalyze(t, "{{ range $k, $v := . }}{{ $k }}"+sep+"{{ end }}")

	rendered, err := tmpl.Render(map[string]any{}, nil, RunContext{})
	if err != nil {
		t.Fatalf("Template.Render: %v", err)
	}

	seeded := make(map[string]struct{})
	for k := range strings.SplitSeq(rendered, sep) {
		if k == "" {
			continue
		}
		seeded[k] = struct{}{}
	}

	for k := range seeded {
		if _, ok := topLevelKeys[k]; !ok {
			t.Errorf("Template.Render seeds top-level key %q, which topLevelKeys does not recognize", k)
		}
	}
	for k := range topLevelKeys {
		if _, ok := seeded[k]; !ok {
			t.Errorf("topLevelKeys recognizes %q, but Template.Render does not seed it", k)
		}
	}
}
