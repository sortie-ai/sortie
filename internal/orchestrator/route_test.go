package orchestrator

import (
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

	tests := []struct {
		name   string
		cfg    config.ServiceConfig
		frozen DispatchResolution
		issue  domain.Issue
		want   DispatchResolution
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
			name:   "kind the configuration does not name routes the issue afresh",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen: DispatchResolution{AgentKind: "kind-gone", RuleName: "old-work"},
			issue:  issueWithLabels("docs"),
			want:   DispatchResolution{AgentKind: "kind-b", RuleName: "docs"},
		},
		{
			name:   "kind the configuration does not name falls back to the default kind when no rule matches",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen: DispatchResolution{AgentKind: "kind-gone", TemplateID: held, RuleName: "old-work"},
			issue:  issueWithLabels("unrelated"),
			want:   DispatchResolution{AgentKind: "kind-a"},
		},
		{
			name:   "template that is not held routes the issue afresh",
			cfg:    config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a"}, Dispatch: plainRules},
			frozen: DispatchResolution{AgentKind: "kind-b", TemplateID: "/prompts/renamed.md", RuleName: "held"},
			issue:  issueWithLabels("docs"),
			want:   DispatchResolution{AgentKind: "kind-b", RuleName: "docs"},
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
			frozen: DispatchResolution{AgentKind: "legacy", TemplateID: "/prompts/renamed.md", RuleName: "backend"},
			issue:  issueWithLabels("backend"),
			want:   DispatchResolution{AgentKind: "modern", RuleName: "backend"},
		},
		{
			name: "retired kind hand-migrated away routes the issue afresh",
			cfg: config.ServiceConfig{
				Agent: config.AgentConfig{Kind: "kind-a"},
			},
			frozen: DispatchResolution{AgentKind: "legacy", RuleName: "backend"},
			issue:  issueWithLabels("backend"),
			want:   DispatchResolution{AgentKind: "kind-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := retrySelection(tt.cfg, templateHeld, tt.frozen, tt.issue)

			if got.AgentKind != tt.want.AgentKind || got.TemplateID != tt.want.TemplateID || got.RuleName != tt.want.RuleName {
				t.Errorf("retrySelection(frozen %+v) = {%q, %q, %q}, want {%q, %q, %q}", tt.frozen,
					got.AgentKind, got.TemplateID, got.RuleName, tt.want.AgentKind, tt.want.TemplateID, tt.want.RuleName)
			}
		})
	}
}
