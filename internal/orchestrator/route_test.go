package orchestrator

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

func prio(v int) *int { return &v }

func issueWithLabels(labels ...string) domain.Issue {
	return domain.Issue{
		ID:         "ISS-1",
		Identifier: "TEST-1",
		Title:      "test issue",
		State:      "To Do",
		Labels:     labels,
	}
}

// --- TestResolveRule ---

func TestResolveRule(t *testing.T) {
	t.Parallel()

	const defaultKind = "claude-code"
	const defaultTmpl = ""

	tests := []struct {
		name         string
		issue        domain.Issue
		dispatch     config.DispatchConfig
		defaultKind  string
		defaultTmpl  string
		wantAgent    string
		wantTemplate string
		wantRuleName string
		wantLayer    ResolutionLayer
	}{
		// --- Fallback chain ---
		{
			name:         "no dispatch section falls back to workflow defaults",
			issue:        issueWithLabels("bug"),
			dispatch:     config.DispatchConfig{},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantTemplate: defaultTmpl,
			wantRuleName: "",
			wantLayer:    ResolvedFromFallback,
		},
		{
			name:  "dispatch default fires when no rule matches",
			issue: issueWithLabels("other"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "docs",
						Match:      config.DispatchMatch{Labels: []string{"docs"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
				Default: config.DispatchSelection{AgentKind: "default-agent", TemplateID: "/tmpl/default.md"},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "default-agent",
			wantTemplate: "/tmpl/default.md",
			wantRuleName: "default",
			wantLayer:    ResolvedFromDefault,
		},
		{
			name:  "dispatch default with partial fields falls through to workflow defaults",
			issue: issueWithLabels("other"),
			dispatch: config.DispatchConfig{
				Default: config.DispatchSelection{AgentKind: "special-agent"},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  "/tmpl/body.md",
			wantAgent:    "special-agent",
			wantTemplate: "/tmpl/body.md",
			wantRuleName: "default",
			wantLayer:    ResolvedFromDefault,
		},

		// --- Rule matching ---
		{
			name:  "rule match returns ResolvedFromRule",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "bug-rule",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "codex", TemplateID: "/tmpl/bug.md"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantTemplate: "/tmpl/bug.md",
			wantRuleName: "bug-rule",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "partial override agent-only uses workflow default template",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "bug-agent-only",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  "/tmpl/body.md",
			wantAgent:    "codex",
			wantTemplate: "/tmpl/body.md",
			wantRuleName: "bug-agent-only",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "partial override template-only uses workflow default agent",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "bug-tmpl-only",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{TemplateID: "/tmpl/bug.md"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantTemplate: "/tmpl/bug.md",
			wantRuleName: "bug-tmpl-only",
			wantLayer:    ResolvedFromRule,
		},

		// --- Catch-all ---
		{
			name:  "catch-all rule short-circuits remaining rules",
			issue: issueWithLabels("anything"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "all",
						Match:      config.DispatchMatch{},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: true,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantTemplate: defaultTmpl,
			wantRuleName: "all",
			wantLayer:    ResolvedFromRule,
		},

		// --- AND/OR semantics ---
		{
			name: "AND across keys: label matches but issue_type does not",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Labels: []string{"bug"}, IssueType: "Feature",
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name: "both",
						Match: config.DispatchMatch{
							Labels:    []string{"bug"},
							IssueType: []string{"Bug"},
						},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantRuleName: "",
			wantLayer:    ResolvedFromFallback,
		},
		{
			name: "OR within labels: second label matches",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Labels: []string{"documentation"},
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "doc-rule",
						Match:      config.DispatchMatch{Labels: []string{"docs", "documentation"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "doc-rule",
			wantLayer:    ResolvedFromRule,
		},

		// --- Glob match ---
		{
			name:  "glob label match with wildcard",
			issue: issueWithLabels("p0-critical"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "priority-wild",
						Match:      config.DispatchMatch{Labels: []string{"p0-*"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "priority-wild",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "glob label no match",
			issue: issueWithLabels("p1-minor"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "priority-wild",
						Match:      config.DispatchMatch{Labels: []string{"p0-*"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantRuleName: "",
			wantLayer:    ResolvedFromFallback,
		},

		// --- Case-insensitive issue_type ---
		{
			name: "issue_type is case-insensitive",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				IssueType: "BUG",
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "bug-type",
						Match:      config.DispatchMatch{IssueType: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "bug-type",
			wantLayer:    ResolvedFromRule,
		},

		// --- Case-insensitive assignee ---
		{
			name: "assignee is case-insensitive",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Assignee: "Alice",
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "alice-rule",
						Match:      config.DispatchMatch{Assignee: []string{"alice"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "alice-rule",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "empty assignee does not match non-empty allowed list",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Assignee: "",
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "alice-rule",
						Match:      config.DispatchMatch{Assignee: []string{"alice"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantRuleName: "",
			wantLayer:    ResolvedFromFallback,
		},

		// --- Identifier glob ---
		{
			name: "identifier glob match",
			issue: domain.Issue{
				ID: "1", Identifier: "FE-123", Title: "t", State: "To Do",
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "fe-rule",
						Match:      config.DispatchMatch{Identifier: []string{"FE-*"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "fe-rule",
			wantLayer:    ResolvedFromRule,
		},

		// --- Priority predicates ---
		{
			name: "priority eq match",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(1),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-eq",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "eq", Value: 1}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-eq",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "priority in match",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(2),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-in",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "in", Values: []int{1, 2, 3}}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-in",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "priority lt match",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(1),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-lt",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "lt", Value: 3}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-lt",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "priority lte match at boundary",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(2),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-lte",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "lte", Value: 2}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-lte",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "priority gt match",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(5),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-gt",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "gt", Value: 3}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-gt",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "priority gte match at boundary",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: prio(3),
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-gte",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "gte", Value: 3}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantRuleName: "prio-gte",
			wantLayer:    ResolvedFromRule,
		},
		{
			name: "nil priority never matches numeric predicate",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: nil,
			},
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "prio-rule",
						Match:      config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "eq", Value: 1}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    defaultKind,
			wantRuleName: "",
			wantLayer:    ResolvedFromFallback,
		},

		// --- First-match-wins order ---
		{
			name:  "first matching rule wins",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "first",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "first-agent"},
						IsCatchAll: false,
					},
					{
						Name:       "second",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "second-agent"},
						IsCatchAll: false,
					},
				},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "first-agent",
			wantRuleName: "first",
			wantLayer:    ResolvedFromRule,
		},

		// --- Partial override falls through to dispatch default ---
		{
			name:  "rule agent-only falls through to dispatch default template",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:       "bug-agent-only",
						Match:      config.DispatchMatch{Labels: []string{"bug"}},
						Selection:  config.DispatchSelection{AgentKind: "codex"},
						IsCatchAll: false,
					},
				},
				Default: config.DispatchSelection{TemplateID: "/tmpl/default.md"},
			},
			defaultKind:  defaultKind,
			defaultTmpl:  defaultTmpl,
			wantAgent:    "codex",
			wantTemplate: "/tmpl/default.md",
			wantRuleName: "bug-agent-only",
			wantLayer:    ResolvedFromRule,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind := tt.defaultKind
			if kind == "" {
				kind = defaultKind
			}
			tmpl := tt.defaultTmpl

			got := ResolveRule(tt.issue, tt.dispatch, kind, tmpl)

			if got.AgentKind != tt.wantAgent {
				t.Errorf("ResolveRule(%q).AgentKind = %q, want %q", tt.issue.Identifier, got.AgentKind, tt.wantAgent)
			}
			if got.TemplateID != tt.wantTemplate {
				t.Errorf("ResolveRule(%q).TemplateID = %q, want %q", tt.issue.Identifier, got.TemplateID, tt.wantTemplate)
			}
			if got.RuleName != tt.wantRuleName {
				t.Errorf("ResolveRule(%q).RuleName = %q, want %q", tt.issue.Identifier, got.RuleName, tt.wantRuleName)
			}
			if got.MatchedAt != tt.wantLayer {
				t.Errorf("ResolveRule(%q).MatchedAt = %v, want %v", tt.issue.Identifier, got.MatchedAt, tt.wantLayer)
			}
		})
	}
}

// --- TestNormalizeDispatchRuleName ---

func TestNormalizeDispatchRuleName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty string becomes none sentinel", input: "", want: "<none>"},
		{name: "non-empty name is preserved", input: "bug-rule", want: "bug-rule"},
		{name: "single char name", input: "a", want: "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeDispatchRuleName(tt.input)
			if got != tt.want {
				t.Errorf("normalizeDispatchRuleName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- TestResolutionLayer_String ---

func TestResolutionLayer_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		layer ResolutionLayer
		want  string
	}{
		{ResolvedFromRule, "rule"},
		{ResolvedFromDefault, "default"},
		{ResolvedFromFallback, "fallback"},
		{ResolutionLayer(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			got := tt.layer.String()
			if got != tt.want {
				t.Errorf("ResolutionLayer(%d).String() = %q, want %q", tt.layer, got, tt.want)
			}
		})
	}
}
