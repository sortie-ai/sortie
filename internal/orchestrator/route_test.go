package orchestrator

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func issueWithLabels(labels ...string) domain.Issue {
	return domain.Issue{
		ID:         "ISS-1",
		Identifier: "TEST-1",
		Title:      "test issue",
		State:      "To Do",
		Labels:     labels,
	}
}

func issueWithTitle(title string) domain.Issue {
	return domain.Issue{ID: "ISS-1", Identifier: "TEST-1", Title: title, State: "To Do"}
}

func titleOnlyConfig(phrases ...string) config.DispatchConfig {
	return config.DispatchConfig{Rules: []config.DispatchRule{
		{Name: "titled", Match: config.DispatchMatch{Title: phrases}, Selection: config.DispatchSelection{AgentKind: "title-agent"}},
	}}
}

func stageWorkflow(leading ...config.DispatchRule) config.DispatchConfig {
	rules := append(slices.Clone(leading),
		config.DispatchRule{Name: "everything", IsCatchAll: true, Selection: config.DispatchSelection{TemplateID: "/prompts/default.md"}},
		config.DispatchRule{Name: "plan", Stage: "Stage-Plan", Selection: config.DispatchSelection{TemplateID: "/prompts/plan.md"}},
		config.DispatchRule{Name: "implement", Stage: "stage-implement", Selection: config.DispatchSelection{TemplateID: "/prompts/implement.md"}},
	)
	return config.DispatchConfig{Rules: rules}
}

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

		{
			name: "priority eq match",
			issue: domain.Issue{
				ID: "1", Identifier: "T-1", Title: "t", State: "To Do",
				Priority: new(1),
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
				Priority: new(2),
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
				Priority: new(1),
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
				Priority: new(2),
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
				Priority: new(5),
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
				Priority: new(3),
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

		{
			name:         "a bug issue takes the catch-all that precedes the staged rules",
			issue:        issueWithLabels("bug"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/default.md",
			wantRuleName: "everything",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:         "a stage label runs its rule after a catch-all, case-insensitively",
			issue:        issueWithLabels("stage-plan"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/plan.md",
			wantRuleName: "plan",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:         "a stage label beside other labels runs its rule",
			issue:        issueWithLabels("stage-implement", "bug"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/implement.md",
			wantRuleName: "implement",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:         "several stage labels run the rule listed first",
			issue:        issueWithLabels("stage-plan", "stage-implement"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/plan.md",
			wantRuleName: "plan",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:         "several stage labels run the rule listed first whatever the label order",
			issue:        issueWithLabels("stage-implement", "stage-plan"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/plan.md",
			wantRuleName: "plan",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:         "a label no rule owns is routed by the ordered rules",
			issue:        issueWithLabels("stage-review"),
			dispatch:     stageWorkflow(),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/default.md",
			wantRuleName: "everything",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a stage label beats an ordered rule that globs it, whatever its position",
			issue: issueWithLabels("stage-implement"),
			dispatch: stageWorkflow(config.DispatchRule{
				Name:      "by-label",
				Match:     config.DispatchMatch{Labels: []string{"stage-*"}},
				Selection: config.DispatchSelection{TemplateID: "/prompts/by-label.md"},
			}),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/implement.md",
			wantRuleName: "implement",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a label the ordered rule globs but no stage rule owns still takes the ordered rule",
			issue: issueWithLabels("stage-x"),
			dispatch: stageWorkflow(config.DispatchRule{
				Name:      "by-label",
				Match:     config.DispatchMatch{Labels: []string{"stage-*"}},
				Selection: config.DispatchSelection{TemplateID: "/prompts/by-label.md"},
			}),
			wantAgent:    defaultKind,
			wantTemplate: "/prompts/by-label.md",
			wantRuleName: "by-label",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a staged rule with an empty match never matches an issue without its label",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
				{Name: "plan", Stage: "stage-plan", Selection: config.DispatchSelection{AgentKind: "planner"}},
			}},
			wantAgent:    defaultKind,
			wantLayer:    ResolvedFromFallback,
			wantRuleName: "",
		},
		{
			name:  "a staged rule listed first never captures an issue the catch-all after it should take",
			issue: issueWithLabels("bug"),
			dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
				{Name: "plan", Stage: "stage-plan", Selection: config.DispatchSelection{AgentKind: "planner"}},
				{Name: "rest", IsCatchAll: true, Selection: config.DispatchSelection{AgentKind: "generalist"}},
			}},
			wantAgent:    "generalist",
			wantRuleName: "rest",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a staged rule listed first never captures an issue without labels",
			issue: issueWithLabels(),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{
					{Name: "plan", Stage: "stage-plan", Selection: config.DispatchSelection{AgentKind: "planner"}},
				},
				Default: config.DispatchSelection{AgentKind: "default-agent"},
			},
			wantAgent:    "default-agent",
			wantRuleName: "default",
			wantLayer:    ResolvedFromDefault,
		},
		{
			name:  "a glob character in a stage label is literal",
			issue: issueWithLabels("stage-plan"),
			dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
				{Name: "any-stage", Stage: "stage-*", Selection: config.DispatchSelection{AgentKind: "planner"}},
			}},
			wantAgent: defaultKind,
			wantLayer: ResolvedFromFallback,
		},
		{
			name:  "a stage label holding a glob character selects the issue that carries it as written",
			issue: issueWithLabels("stage-*"),
			dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
				{Name: "any-stage", Stage: "stage-*", Selection: config.DispatchSelection{AgentKind: "planner"}},
			}},
			wantAgent:    "planner",
			wantRuleName: "any-stage",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "edge white space is part of the stage label",
			issue: issueWithLabels("stage-plan"),
			dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
				{Name: "plan", Stage: " stage-plan", Selection: config.DispatchSelection{AgentKind: "planner"}},
			}},
			wantAgent: defaultKind,
			wantLayer: ResolvedFromFallback,
		},
		{
			name:  "a staged rule without agent or template takes the dispatch default",
			issue: issueWithLabels("stage-plan"),
			dispatch: config.DispatchConfig{
				Rules:   []config.DispatchRule{{Name: "plan", Stage: "stage-plan"}},
				Default: config.DispatchSelection{AgentKind: "default-agent", TemplateID: "/tmpl/default.md"},
			},
			wantAgent:    "default-agent",
			wantTemplate: "/tmpl/default.md",
			wantRuleName: "plan",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a staged rule without agent or template takes the workflow defaults",
			issue: issueWithLabels("stage-plan"),
			dispatch: config.DispatchConfig{
				Rules: []config.DispatchRule{{Name: "plan", Stage: "stage-plan"}},
			},
			defaultKind:  "workflow-kind",
			defaultTmpl:  "/tmpl/body.md",
			wantAgent:    "workflow-kind",
			wantTemplate: "/tmpl/body.md",
			wantRuleName: "plan",
			wantLayer:    ResolvedFromRule,
		},
		{
			name:  "a staged rule agent beats the dispatch default",
			issue: issueWithLabels("stage-plan"),
			dispatch: config.DispatchConfig{
				Rules:   []config.DispatchRule{{Name: "plan", Stage: "stage-plan", Selection: config.DispatchSelection{AgentKind: "planner"}}},
				Default: config.DispatchSelection{AgentKind: "default-agent"},
			},
			wantAgent:    "planner",
			wantRuleName: "plan",
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

			got := ResolveRule(tt.issue, tt.dispatch, HopRoute{}, kind, tmpl)

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

func retiredFixtureLookup(kind string) (registry.RetiredAgent, bool) {
	if kind != "legacy" {
		return registry.RetiredAgent{}, false
	}
	return registry.RetiredAgent{
		Replacement:   "modern",
		CredentialEnv: registry.DeclareCredentialEnv("LEGACY_KEY"),
		Convert: func(in registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			suffix := []string{"serve"}
			if in.Remote {
				suffix = append(suffix, "--remote")
			}
			base := in.Command
			if !base.NamesExecutable() {
				base = domain.AgentCommand{Line: "legacy-cli"}
			}
			if len(base.Argv) > 0 {
				return registry.AgentConversion{Command: domain.AgentCommand{Argv: append(append([]string{}, base.Argv...), suffix...)}, Args: suffix}, nil
			}
			return registry.AgentConversion{Command: domain.AgentCommand{Line: base.Line + " " + strings.Join(suffix, " ")}, Args: suffix}, nil
		},
	}, true
}

func convertedConfig(t *testing.T, raw map[string]any) config.ServiceConfig {
	t.Helper()
	cfg, err := config.NewServiceConfig(raw, config.WithRetiredAgents(retiredFixtureLookup))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	dispatch, err := config.BuildDispatchConfig(raw, t.TempDir(), func(string) bool { return true }, cfg.Agent.Kind)
	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v", err)
	}
	cfg.SetDispatch(dispatch)
	return cfg
}

func backendRuleRaw(agent string) map[string]any {
	return map[string]any{"name": "backend", "match": map[string]any{"labels": []any{"backend"}}, "agent": agent}
}

func TestRetrySelection(t *testing.T) {
	t.Parallel()

	const held = "/prompts/held.md"
	templateHeld := func(id string) bool { return id == "" || id == held }
	plainRules := config.DispatchConfig{Rules: []config.DispatchRule{
		{Name: "docs", Match: config.DispatchMatch{Labels: []string{"docs"}}, Selection: config.DispatchSelection{AgentKind: "kind-b"}},
		{Name: "held", Match: config.DispatchMatch{Labels: []string{"held"}}, Selection: config.DispatchSelection{AgentKind: "kind-b", TemplateID: held}},
	}}
	titleRules := config.DispatchConfig{Rules: []config.DispatchRule{
		{Name: "infra", Match: config.DispatchMatch{Title: []string{"[infra]"}}, Selection: config.DispatchSelection{AgentKind: "kind-b"}},
	}}
	stagedRetryRules := config.DispatchConfig{Rules: []config.DispatchRule{
		{Name: "plan", Stage: "stage-plan", Selection: config.DispatchSelection{AgentKind: "kind-b"}},
		{Name: "implement", Stage: "stage-implement", Selection: config.DispatchSelection{AgentKind: "kind-c"}},
	}}

	tests := []struct {
		name   string
		cfg    config.ServiceConfig
		frozen DispatchResolution
		issue  domain.Issue
		want   DispatchResolution

		wantFresh bool
	}{
		{
			name:   "reachable rule kind with a held template keeps the frozen selection",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen: DispatchResolution{AgentKind: "kind-b", TemplateID: held, RuleName: "held"},
			issue:  issueWithLabels("unrelated"),
			want:   DispatchResolution{AgentKind: "kind-b", TemplateID: held, RuleName: "held"},
		},
		{
			name:   "default kind with a changed command keeps the frozen selection",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a", Command: "changed-command"}, Dispatch: plainRules},
			frozen: DispatchResolution{AgentKind: "kind-a"},
			issue:  issueWithLabels("docs"),
			want:   DispatchResolution{AgentKind: "kind-a"},
		},
		{
			name: "kind only a rule reaches after agent.kind moved keeps the frozen selection",
			cfg: config.ServiceConfig{
				Agent: config.AgentConfig{Kind: "kind-b"},
				Dispatch: config.DispatchConfig{Rules: []config.DispatchRule{
					{Name: "old-work", Match: config.DispatchMatch{Labels: []string{"old"}}, Selection: config.DispatchSelection{AgentKind: "kind-a"}},
				}},
			},
			frozen: DispatchResolution{AgentKind: "kind-a", RuleName: "old-work"},
			issue:  issueWithLabels("unrelated"),
			want:   DispatchResolution{AgentKind: "kind-a", RuleName: "old-work"},
		},
		{
			name:      "kind the configuration does not name routes the issue afresh",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen:    DispatchResolution{AgentKind: "kind-gone", RuleName: "old-work"},
			issue:     issueWithLabels("docs"),
			want:      DispatchResolution{AgentKind: "kind-b", RuleName: "docs"},
			wantFresh: true,
		},
		{
			name:      "kind the configuration does not name falls back to the default kind when no rule matches",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen:    DispatchResolution{AgentKind: "kind-gone", TemplateID: held, RuleName: "old-work"},
			issue:     issueWithLabels("unrelated"),
			want:      DispatchResolution{AgentKind: "kind-a"},
			wantFresh: true,
		},
		{
			name:      "template that is not held routes the issue afresh",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen:    DispatchResolution{AgentKind: "kind-b", TemplateID: "/prompts/renamed.md", RuleName: "held"},
			issue:     issueWithLabels("docs"),
			want:      DispatchResolution{AgentKind: "kind-b", RuleName: "docs"},
			wantFresh: true,
		},
		{
			name: "retired kind with a conversion record moves to its replacement with the frozen template and rule",
			cfg: convertedConfig(t, map[string]any{
				"agent":    map[string]any{"kind": "kind-a"},
				"dispatch": map[string]any{"rules": []any{backendRuleRaw("legacy")}},
			}),
			frozen: DispatchResolution{AgentKind: "legacy", TemplateID: held, RuleName: "backend"},
			issue:  issueWithLabels("unrelated"),
			want:   DispatchResolution{AgentKind: "modern", TemplateID: held, RuleName: "backend"},
		},
		{
			name: "retired kind whose replacement is the default kind moves to it",
			cfg: convertedConfig(t, map[string]any{
				"agent": map[string]any{"kind": "legacy"},
			}),
			frozen: DispatchResolution{AgentKind: "legacy"},
			issue:  issueWithLabels("unrelated"),
			want:   DispatchResolution{AgentKind: "modern"},
		},
		{
			name: "retired kind with an unheld template routes the issue afresh",
			cfg: convertedConfig(t, map[string]any{
				"agent":    map[string]any{"kind": "kind-a"},
				"dispatch": map[string]any{"rules": []any{backendRuleRaw("legacy")}},
			}),
			frozen:    DispatchResolution{AgentKind: "legacy", TemplateID: "/prompts/renamed.md", RuleName: "backend"},
			issue:     issueWithLabels("backend"),
			want:      DispatchResolution{AgentKind: "modern", RuleName: "backend"},
			wantFresh: true,
		},
		{
			name: "retired kind hand-migrated away routes the issue afresh",
			cfg: config.ServiceConfig{
				Agent: config.AgentConfig{Kind: "kind-a"},
			},
			frozen:    DispatchResolution{AgentKind: "legacy", RuleName: "backend"},
			issue:     issueWithLabels("backend"),
			want:      DispatchResolution{AgentKind: "kind-a"},
			wantFresh: true,
		},
		{
			name:   "reachable kind keeps the frozen selection after the title stops matching the frozen rule",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: titleRules},
			frozen: DispatchResolution{AgentKind: "kind-b", RuleName: "infra"},
			issue:  issueWithTitle("Renamed to something else"),
			want:   DispatchResolution{AgentKind: "kind-b", RuleName: "infra"},
		},
		{
			name:      "kind the configuration does not name evaluates the title of the issue it was handed",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: titleRules},
			frozen:    DispatchResolution{AgentKind: "kind-gone", RuleName: "old-work"},
			issue:     issueWithTitle("[INFRA] rotate keys"),
			want:      DispatchResolution{AgentKind: "kind-b", RuleName: "infra"},
			wantFresh: true,
		},
		{
			name:      "kind the configuration does not name falls back when the new title matches no rule",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: titleRules},
			frozen:    DispatchResolution{AgentKind: "kind-gone", RuleName: "old-work"},
			issue:     issueWithTitle("Improve infra docs"),
			want:      DispatchResolution{AgentKind: "kind-a"},
			wantFresh: true,
		},
		{
			name:   "frozen staged selection stands after the stage label on the issue moved",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: stagedRetryRules},
			frozen: DispatchResolution{AgentKind: "kind-b", RuleName: "plan", MatchedAt: ResolvedFromRule},
			issue:  issueWithLabels("stage-implement"),
			want:   DispatchResolution{AgentKind: "kind-b", RuleName: "plan"},
		},
		{
			name:   "kind only a staged rule reaches counts as reachable",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: stagedRetryRules},
			frozen: DispatchResolution{AgentKind: "kind-c", RuleName: "implement", MatchedAt: ResolvedFromRule},
			issue:  issueWithLabels("stage-plan"),
			want:   DispatchResolution{AgentKind: "kind-c", RuleName: "implement"},
		},
		{
			name:      "kind the configuration does not name routes afresh through the stage step",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: stagedRetryRules},
			frozen:    DispatchResolution{AgentKind: "kind-gone", RuleName: "old-work"},
			issue:     issueWithLabels("docs", "stage-implement"),
			want:      DispatchResolution{AgentKind: "kind-c", RuleName: "implement"},
			wantFresh: true,
		},
		{
			name:      "template that is not held routes afresh through the stage step",
			cfg:       config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: stagedRetryRules},
			frozen:    DispatchResolution{AgentKind: "kind-b", TemplateID: "/prompts/renamed.md", RuleName: "plan"},
			issue:     issueWithLabels("stage-implement"),
			want:      DispatchResolution{AgentKind: "kind-c", RuleName: "implement"},
			wantFresh: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fresh := retrySelection(tt.cfg, templateHeld, tt.frozen, tt.issue, HopRoute{})

			if got.AgentKind != tt.want.AgentKind || got.TemplateID != tt.want.TemplateID || got.RuleName != tt.want.RuleName {
				t.Errorf("retrySelection(frozen %+v) = {%q, %q, %q}, want {%q, %q, %q}", tt.frozen,
					got.AgentKind, got.TemplateID, got.RuleName, tt.want.AgentKind, tt.want.TemplateID, tt.want.RuleName)
			}
			if fresh != tt.wantFresh {
				t.Errorf("retrySelection(frozen %+v) routed afresh = %v, want %v", tt.frozen, fresh, tt.wantFresh)
			}
		})
	}
}

func TestResolveRule_TitleMatching(t *testing.T) {
	t.Parallel()

	const (
		thaiKo       = "\u0e40\u0e01"
		thaiKoTone   = "\u0e40\u0e01\u0e48\u0e07"
		thaiThi      = "\u0e17\u0e35\u0e48"
		thaiThiNee   = "\u0e17\u0e35\u0e48\u0e19\u0e35\u0e48"
		warning      = "\u26a0"
		warningVS16  = "\u26a0\ufe0f"
		warningVS15  = "\u26a0\ufe0e"
		bug          = "\U0001f41b"
		bugVS16      = "\U0001f41b\ufe0f"
		bugVS15      = "\U0001f41b\ufe0e"
		acuteMark    = "\u0301"
		eAcute       = "\u00e9"
		noBreakSpace = "\u00a0"
		ideographic  = "\u3000"
	)
	tests := []struct {
		name    string
		phrases []string
		title   string
		want    bool
	}{
		{name: "case is ignored and the word is whole", phrases: []string{"fix"}, title: "Fix login redirect", want: true},
		{name: "starts inside prefix", phrases: []string{"fix"}, title: "Add prefix to logs", want: false},
		{name: "ends inside Fixes", phrases: []string{"fix"}, title: "Fixes typo in docs", want: false},
		{name: "the second occurrence is whole", phrases: []string{"fix"}, title: "prefix fix", want: true},
		{name: "starts inside slow", phrases: []string{"low-cost"}, title: "Speed up slow-costly import", want: false},
		{name: "brackets and hyphen are not word runes", phrases: []string{"low-cost"}, title: "[low-cost] bump deps", want: true},
		{name: "punctuation must appear and case is ignored", phrases: []string{"[infra]"}, title: "[INFRA] rotate keys", want: true},
		{name: "absent brackets do not match", phrases: []string{"[infra]"}, title: "Improve infra docs", want: false},
		{name: "brackets separate words", phrases: []string{"infra"}, title: "[infra] rotate keys", want: true},
		{name: "the colon matches itself and spacing collapses", phrases: []string{"wip:"}, title: "WIP:  refactor store", want: true},
		{name: "a white-space run compares as one space", phrases: []string{"rotate keys"}, title: "Rotate  keys", want: true},
		{name: "ends inside toolsy", phrases: []string{"x/tools"}, title: "x/toolsy: flaky test", want: false},
		{name: "underscore separates words", phrases: []string{"snake"}, title: "snake_case config keys", want: true},
		{name: "question mark is literal", phrases: []string{"?"}, title: "Why does sync stall?", want: true},
		{name: "asterisks match only asterisks", phrases: []string{"*infra*"}, title: "*INFRA* rotate keys", want: true},
		{name: "asterisks are not wildcards", phrases: []string{"*infra*"}, title: "Improve infra docs", want: false},
		{name: "a symbol carries no boundary constraint", phrases: []string{bug}, title: bug + " Crash on start", want: true},
		{name: "a variation selector after the symbol is ignored", phrases: []string{warning}, title: warningVS16 + " deprecate v1 API", want: true},
		{name: "a text-style variation selector after the symbol is ignored", phrases: []string{warning}, title: warningVS15 + " deprecate v1 API", want: true},
		{name: "a variation selector in the phrase is ignored", phrases: []string{warningVS16}, title: warning + " deprecate v1 API", want: true},
		{name: "a text-style variation selector in the phrase is ignored", phrases: []string{warningVS15}, title: warning + " deprecate v1 API", want: true},
		{name: "a variation selector on both sides is ignored", phrases: []string{warningVS16}, title: warningVS16 + " deprecate v1 API", want: true},
		{name: "the two variation selectors are interchangeable", phrases: []string{warningVS15}, title: warningVS16 + " deprecate v1 API", want: true},
		{name: "a variation selector inside a longer phrase is ignored", phrases: []string{warningVS16 + " deprecate"}, title: warning + " deprecate v1 API", want: true},
		{name: "a variation selector after a word is ignored", phrases: []string{"fix"}, title: "fix\ufe0f login redirect", want: true},
		{name: "a variation selector does not hide an inner word boundary", phrases: []string{"fix"}, title: "prefix\ufe0f login", want: false},
		{name: "a combining mark after a variation selector still blocks the match", phrases: []string{warning}, title: warningVS16 + acuteMark + " deprecate v1 API", want: false},
		{name: "a combining mark after the symbol still blocks the match", phrases: []string{warning}, title: warning + acuteMark + " deprecate v1 API", want: false},
		{name: "Han boundaries are word boundaries", phrases: []string{"\u8bbe\u8ba1"}, title: "\u7f13\u5b58\u8bbe\u8ba1\u6587\u6863", want: true},
		{name: "a Thai tone mark joins the consonant before it", phrases: []string{thaiKo}, title: thaiKoTone, want: false},
		{name: "the boundary after a Thai tone mark is ordinary", phrases: []string{thaiThi}, title: thaiThiNee, want: true},
		{name: "Hangul particles attach to the noun", phrases: []string{"\ubc84\uadf8"}, title: "\ubc84\uadf8\ub97c \uc218\uc815", want: false},
		{name: "an accent is not folded", phrases: []string{"cafe"}, title: "Caf" + eAcute + " opening hours", want: false},
		{name: "an empty title never matches", phrases: []string{"fix"}, title: "", want: false},

		{name: "overlapping occurrences are tried and the later one is whole", phrases: []string{"ab ab"}, title: "xab ab ab", want: true},
		{name: "overlapping occurrences all inside words do not match", phrases: []string{"ab ab"}, title: "xab ab", want: false},
		{name: "a later whole occurrence is found after an occurrence inside a word", phrases: []string{"fix"}, title: "prefix suffix fix", want: true},
		{name: "a white-space-only title never matches", phrases: []string{"fix"}, title: " \t\n", want: false},

		{name: "any phrase of a list may match", phrases: []string{"nothing", "docs", "wip:"}, title: "Update DOCS", want: true},
		{name: "no phrase of a list matching is no match", phrases: []string{"nothing", "else"}, title: "Update docs", want: false},
		{name: "a blank phrase never matches", phrases: []string{" "}, title: "Update docs", want: false},
		{name: "a blank phrase does not hide a later phrase", phrases: []string{" ", "docs"}, title: "Update docs", want: true},
		{name: "a phrase with outer spacing matches the trimmed text", phrases: []string{"  Fix  "}, title: "\tfix\n", want: true},

		{name: "a tab in the title collapses like a space", phrases: []string{"rotate keys"}, title: "rotate\tkeys", want: true},
		{name: "a newline in the title collapses like a space", phrases: []string{"rotate keys"}, title: "rotate\nkeys", want: true},
		{name: "a no-break space in the title collapses like a space", phrases: []string{"rotate keys"}, title: "rotate" + noBreakSpace + "keys", want: true},
		{name: "an ideographic space in the title collapses like a space", phrases: []string{"rotate keys"}, title: "rotate" + ideographic + "keys", want: true},
		{name: "a no-break space in the phrase collapses like a space", phrases: []string{"rotate" + noBreakSpace + noBreakSpace + "keys"}, title: "rotate keys", want: true},

		{name: "a combining mark joins the letter before it in a spaced script", phrases: []string{"cafe"}, title: "cafe" + acuteMark + " opening hours", want: false},
		{name: "a precomposed accent does not match its bare base", phrases: []string{"caf"}, title: "caf" + eAcute + " opening hours", want: false},
		{name: "a phrase ending in a non-word rune is constrained by a following mark", phrases: []string{"[infra]"}, title: "[infra]" + acuteMark + " rotate keys", want: false},
		{name: "a phrase ending in a non-word rune is free before an ordinary letter", phrases: []string{"[infra]"}, title: "[infra]rotate keys", want: true},
		{name: "a variation selector after an emoji is ignored", phrases: []string{bug}, title: bugVS16 + " Crash on start", want: true},
		{name: "a text-style variation selector after an emoji is ignored", phrases: []string{bug}, title: bugVS15 + " Crash on start", want: true},
		{name: "a variation selector in an emoji phrase is ignored", phrases: []string{bugVS16}, title: bug + " Crash on start", want: true},
		{name: "a combining mark after a letter and a variation selector still blocks the match", phrases: []string{"cafe"}, title: "cafe\ufe0f" + acuteMark + " opening hours", want: false},
		{name: "a base letter with a combining accent does not match the bare base as a prefix", phrases: []string{"e"}, title: "e" + acuteMark + "cole", want: false},
		{name: "sharp s is not folded to ss", phrases: []string{"stra\u00dfe"}, title: "STRASSE closed", want: false},

		{name: "a final sigma in the phrase matches a capital sigma in the title", phrases: []string{"\u03bf\u03c2"}, title: "\u039f\u03a3 \u03bb\u03ac\u03b8\u03bf\u03c2", want: true},
		{name: "a capital sigma in the phrase matches a final sigma in the title", phrases: []string{"\u039f\u03a3"}, title: "\u03bf\u03c2", want: true},
		{name: "a long s in the title matches a plain s in the phrase", phrases: []string{"s"}, title: "add \u017f flag", want: true},
		{name: "a long s in the phrase matches a capital S in the title", phrases: []string{"\u017f"}, title: "S", want: true},
		{name: "the micro sign matches Greek mu", phrases: []string{"\u00b5"}, title: "\u03bc", want: true},
		{name: "a Greek beta symbol matches beta", phrases: []string{"\u03d0"}, title: "\u03b2", want: true},

		{name: "a dotted capital I in the title matches a plain i", phrases: []string{"i"}, title: "Fix \u0130 login", want: true},
		{name: "a Greek word with a final sigma matches itself", phrases: []string{"\u03b9\u03c3\u03c4\u03cc\u03c2"}, title: "Fix \u03b9\u03c3\u03c4\u03cc\u03c2", want: true},
		{name: "sharp s does not match ss", phrases: []string{"ss"}, title: "\u00df", want: false},
		{name: "ss in the title does not match a phrase with sharp s", phrases: []string{"stra\u00dfe"}, title: "strasse", want: false},
		{name: "ss in the phrase does not match sharp s in the title", phrases: []string{"strasse"}, title: "STRA\u00dfE", want: false},
		{name: "a lone combining iota mark does not match the letter iota", phrases: []string{"\u03b9"}, title: "\u0345", want: false},
		{name: "a bare phrase does not match the accented title", phrases: []string{"cafe"}, title: "Caf" + eAcute, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ResolveRule(issueWithTitle(tt.title), titleOnlyConfig(tt.phrases...), HopRoute{}, "fallback", "")

			if matched := got.RuleName == "titled"; matched != tt.want {
				t.Errorf("ResolveRule(title %q, phrases %q) matched = %v, want %v", tt.title, tt.phrases, matched, tt.want)
			}
		})
	}

	t.Run("resolution is pure", func(t *testing.T) {
		t.Parallel()

		phrases := []string{"  Fix ", "WIP:"}
		wantPhrases := slices.Clone(phrases)
		title := "prefix  FIX it"
		issue := issueWithTitle(title)
		dispatch := titleOnlyConfig(phrases...)

		first := ResolveRule(issue, dispatch, HopRoute{}, "fallback", "")
		second := ResolveRule(issue, dispatch, HopRoute{}, "fallback", "")

		if first != second {
			t.Errorf("ResolveRule() twice = %+v, then %+v, want equal", first, second)
		}
		if first.RuleName != "titled" {
			t.Errorf("ResolveRule().RuleName = %q, want %q", first.RuleName, "titled")
		}
		if !slices.Equal(phrases, wantPhrases) {
			t.Errorf("phrases after ResolveRule = %q, want %q", phrases, wantPhrases)
		}
		if issue.Title != title {
			t.Errorf("issue.Title after ResolveRule = %q, want %q", issue.Title, title)
		}
	})
}

func TestResolveRule_TitleWithOtherKeys(t *testing.T) {
	t.Parallel()

	byTitle := config.DispatchRule{Name: "by-title", Match: config.DispatchMatch{Title: []string{"[docs]"}}, Selection: config.DispatchSelection{AgentKind: "title-agent"}}
	byLabel := config.DispatchRule{Name: "by-label", Match: config.DispatchMatch{Labels: []string{"bug"}}, Selection: config.DispatchSelection{AgentKind: "label-agent"}}
	both := config.DispatchRule{Name: "both", Match: config.DispatchMatch{Title: []string{"[docs]"}, Labels: []string{"bug"}}, Selection: config.DispatchSelection{AgentKind: "both-agent"}}
	catchAll := config.DispatchRule{Name: "rest", IsCatchAll: true, Selection: config.DispatchSelection{AgentKind: "rest-agent"}}
	docsBug := domain.Issue{ID: "ISS-1", Identifier: "TEST-1", Title: "[docs] Fix broken link", Labels: []string{"bug"}}

	tests := []struct {
		name         string
		issue        domain.Issue
		dispatch     config.DispatchConfig
		wantRuleName string
		wantLayer    ResolutionLayer
	}{
		{name: "a rule with title and labels matches when both hold", issue: docsBug, dispatch: config.DispatchConfig{Rules: []config.DispatchRule{both}}, wantRuleName: "both", wantLayer: ResolvedFromRule},
		{name: "title holds but labels do not", issue: issueWithTitle("[docs] Fix broken link"), dispatch: config.DispatchConfig{Rules: []config.DispatchRule{both}}, wantRuleName: "", wantLayer: ResolvedFromFallback},
		{name: "labels hold but title does not", issue: domain.Issue{ID: "ISS-1", Identifier: "TEST-1", Title: "Fix broken link", Labels: []string{"bug"}}, dispatch: config.DispatchConfig{Rules: []config.DispatchRule{both}}, wantRuleName: "", wantLayer: ResolvedFromFallback},
		{name: "the title rule first wins over a label rule", issue: docsBug, dispatch: config.DispatchConfig{Rules: []config.DispatchRule{byTitle, byLabel}}, wantRuleName: "by-title", wantLayer: ResolvedFromRule},
		{name: "the label rule first wins over a title rule", issue: docsBug, dispatch: config.DispatchConfig{Rules: []config.DispatchRule{byLabel, byTitle}}, wantRuleName: "by-label", wantLayer: ResolvedFromRule},
		{name: "a title rule that misses falls through to a later label rule", issue: domain.Issue{ID: "ISS-1", Identifier: "TEST-1", Title: "Fix broken link", Labels: []string{"bug"}}, dispatch: config.DispatchConfig{Rules: []config.DispatchRule{byTitle, byLabel}}, wantRuleName: "by-label", wantLayer: ResolvedFromRule},
		{name: "a title rule that misses falls through to a later catch-all", issue: issueWithTitle("Fix broken link"), dispatch: config.DispatchConfig{Rules: []config.DispatchRule{byTitle, catchAll}}, wantRuleName: "rest", wantLayer: ResolvedFromRule},
		{
			name:         "a title rule that misses falls through to the dispatch default",
			issue:        issueWithTitle("Fix broken link"),
			dispatch:     config.DispatchConfig{Rules: []config.DispatchRule{byTitle}, Default: config.DispatchSelection{AgentKind: "default-agent"}},
			wantRuleName: "default",
			wantLayer:    ResolvedFromDefault,
		},
		{name: "a title rule that misses falls through to the workflow defaults", issue: issueWithTitle("Fix broken link"), dispatch: config.DispatchConfig{Rules: []config.DispatchRule{byTitle}}, wantRuleName: "", wantLayer: ResolvedFromFallback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ResolveRule(tt.issue, tt.dispatch, HopRoute{}, "fallback", "")

			if got.RuleName != tt.wantRuleName || got.MatchedAt != tt.wantLayer {
				t.Errorf("ResolveRule(title %q).{RuleName, MatchedAt} = {%q, %v}, want {%q, %v}", tt.issue.Title, got.RuleName, got.MatchedAt, tt.wantRuleName, tt.wantLayer)
			}
		})
	}
}

func TestStageCandidates(t *testing.T) {
	t.Parallel()

	rules := stageWorkflow().Rules

	tests := []struct {
		name  string
		issue domain.Issue
		rules []config.DispatchRule
		want  []int
	}{
		{name: "no carried stage label", issue: issueWithLabels("bug"), rules: rules, want: nil},
		{name: "no labels", issue: issueWithLabels(), rules: rules, want: nil},
		{name: "one carried stage label", issue: issueWithLabels("stage-implement"), rules: rules, want: []int{2}},
		{name: "two carried labels come back in rule order", issue: issueWithLabels("stage-implement", "stage-plan"), rules: rules, want: []int{1, 2}},
		{name: "label case does not matter", issue: issueWithLabels("STAGE-PLAN"), rules: rules, want: []int{1}},
		{name: "no rules", issue: issueWithLabels("stage-plan"), rules: nil, want: nil},
		{name: "a rule without a stage never counts", issue: issueWithLabels(""), rules: []config.DispatchRule{{Name: "rest", IsCatchAll: true}}, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stageCandidates(tt.issue, tt.rules)

			if !slices.Equal(got, tt.want) {
				t.Errorf("stageCandidates(labels %v) = %v, want %v", tt.issue.Labels, got, tt.want)
			}
		})
	}
}

func decodeLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestWarnSeveralStageLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issue      domain.Issue
		wantLabels []any
	}{
		{name: "two stage labels draw one record in list order as written", issue: issueWithLabels("stage-implement", "stage-plan"), wantLabels: []any{"Stage-Plan", "stage-implement"}},
		{name: "two stage labels beside other labels draw one record", issue: issueWithLabels("bug", "stage-plan", "stage-implement"), wantLabels: []any{"Stage-Plan", "stage-implement"}},
		{name: "one stage label draws none", issue: issueWithLabels("stage-plan", "bug")},
		{name: "no stage label draws none", issue: issueWithLabels("bug")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))

			warnSeveralStageLabels(log, tt.issue, stageWorkflow(), "plan")

			records := decodeLogRecords(t, &buf)
			if tt.wantLabels == nil {
				if len(records) != 0 {
					t.Fatalf("warnSeveralStageLabels(labels %v) logged %d records, want 0: %v", tt.issue.Labels, len(records), records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("warnSeveralStageLabels(labels %v) logged %d records, want 1: %v", tt.issue.Labels, len(records), records)
			}
			record := records[0]
			if record["level"] != "WARN" || record["msg"] != "several stage labels found" {
				t.Errorf("record level, msg = %v, %v, want WARN, %q", record["level"], record["msg"], "several stage labels found")
			}
			if got, _ := record["stage_labels"].([]any); !slices.Equal(got, tt.wantLabels) {
				t.Errorf("record stage_labels = %v, want %v", record["stage_labels"], tt.wantLabels)
			}
			if record["rule_name"] != "plan" {
				t.Errorf("record rule_name = %v, want %q", record["rule_name"], "plan")
			}
			if record["issue_id"] != tt.issue.ID || record["issue_identifier"] != tt.issue.Identifier {
				t.Errorf("record issue_id, issue_identifier = %v, %v, want %q, %q", record["issue_id"], record["issue_identifier"], tt.issue.ID, tt.issue.Identifier)
			}
		})
	}
}
