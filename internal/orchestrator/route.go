package orchestrator

import (
	"path"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// ResolutionLayer identifies the layer that produced a dispatch
// resolution: a matched rule, the dispatch default block, or the
// workflow-wide fallback.
type ResolutionLayer int

const (
	// ResolvedFromRule indicates a dispatch rule matched the issue.
	ResolvedFromRule ResolutionLayer = 1

	// ResolvedFromDefault indicates no rule matched and the dispatch
	// default block supplied the selection.
	ResolvedFromDefault ResolutionLayer = 2

	// ResolvedFromFallback indicates neither a rule nor the dispatch
	// default supplied a selection; the workflow-wide agent kind and
	// the body template were used.
	ResolvedFromFallback ResolutionLayer = 3
)

// String returns the lower-case identifier for this layer used in
// log fields and metric label values.
func (l ResolutionLayer) String() string {
	switch l {
	case ResolvedFromRule:
		return "rule"
	case ResolvedFromDefault:
		return "default"
	case ResolvedFromFallback:
		return "fallback"
	default:
		return "unknown"
	}
}

// DispatchResolution carries the agent kind, template ID, rule name,
// and layer chosen for a single issue's initial dispatch. The
// orchestrator persists these on [RunningEntry] and propagates them
// through [RetryEntry] so retries and reaction continuations reuse
// the original selection.
type DispatchResolution struct {
	// AgentKind is the resolved adapter kind. Always non-empty for a
	// well-configured workflow; the resolver coalesces missing values
	// through the fallback chain.
	AgentKind string

	// TemplateID is the resolved template registry key. The empty
	// string selects the body template.
	TemplateID string

	// RuleName is the operator-defined name of the matched rule, or
	// "default" when the dispatch default fired, or "" when both
	// layers were absent.
	RuleName string

	// MatchedAt records which resolution layer produced this result.
	MatchedAt ResolutionLayer
}

// ResolveRule selects the dispatch agent kind, template ID, and rule
// name for an issue against a [config.DispatchConfig]. It is pure: no
// I/O, no time dependence, no goroutine. defaultAgentKind is the
// workflow-wide agent kind (typically cfg.Agent.Kind);
// defaultTemplateID is the body-template sentinel (typically the
// empty string). The same inputs always produce the same output.
func ResolveRule(issue domain.Issue, dispatch config.DispatchConfig, defaultAgentKind, defaultTemplateID string) DispatchResolution {
	for _, rule := range dispatch.Rules {
		if !rule.IsCatchAll && !matchRule(rule.Match, issue) {
			continue
		}
		return DispatchResolution{
			AgentKind:  coalesce(rule.Selection.AgentKind, dispatch.Default.AgentKind, defaultAgentKind),
			TemplateID: coalesce(rule.Selection.TemplateID, dispatch.Default.TemplateID, defaultTemplateID),
			RuleName:   rule.Name,
			MatchedAt:  ResolvedFromRule,
		}
	}

	if dispatch.Default.AgentKind != "" || dispatch.Default.TemplateID != "" {
		return DispatchResolution{
			AgentKind:  coalesce(dispatch.Default.AgentKind, defaultAgentKind),
			TemplateID: coalesce(dispatch.Default.TemplateID, defaultTemplateID),
			RuleName:   "default",
			MatchedAt:  ResolvedFromDefault,
		}
	}

	return DispatchResolution{
		AgentKind:  defaultAgentKind,
		TemplateID: defaultTemplateID,
		RuleName:   "",
		MatchedAt:  ResolvedFromFallback,
	}
}

// coalesce returns the first non-empty string from the arguments. An
// all-empty call returns the empty string.
func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeDispatchRuleName returns the rule name suitable for use as
// a Prometheus label value. Empty rule names map to the literal
// "<none>" sentinel so the dispatch rule match counter does not
// emit time series with empty label values.
func normalizeDispatchRuleName(name string) string {
	if name == "" {
		return "<none>"
	}
	return name
}

// matchRule applies AND across keys and OR within a key. An absent
// key does not participate in the match.
func matchRule(m config.DispatchMatch, issue domain.Issue) bool {
	if len(m.Labels) > 0 {
		if !anyGlobMatch(m.Labels, issue.Labels) {
			return false
		}
	}
	if len(m.IssueType) > 0 {
		if !anyCIEq(m.IssueType, issue.IssueType) {
			return false
		}
	}
	if len(m.Assignee) > 0 {
		if !anyCIEq(m.Assignee, issue.Assignee) {
			return false
		}
	}
	if len(m.Identifier) > 0 {
		if !anyGlobMatch(m.Identifier, []string{issue.Identifier}) {
			return false
		}
	}
	if m.Priority != nil {
		if issue.Priority == nil {
			return false
		}
		if !priorityPredicateMatch(m.Priority, *issue.Priority) {
			return false
		}
	}
	return true
}

// anyGlobMatch reports whether any candidate value matches any
// pattern under [path.Match] semantics. Builder pre-validation
// guarantees pattern syntax; a runtime error treats the pair as
// non-matching but does not block other pattern/value combinations.
func anyGlobMatch(patterns, values []string) bool {
	for _, p := range patterns {
		for _, v := range values {
			ok, err := path.Match(p, v)
			if err != nil {
				continue
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// anyCIEq reports whether value equals any entry in allowed under
// case-insensitive comparison. Empty value never matches a non-empty
// allowed entry, so issues without an assignee never satisfy an
// assignee rule.
func anyCIEq(allowed []string, value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	for _, a := range allowed {
		if strings.ToLower(a) == lower {
			return true
		}
	}
	return false
}

// priorityPredicateMatch dispatches by operator. Unknown operators
// return false defensively; the builder rejects unknown operators at
// load time.
func priorityPredicateMatch(p *config.PriorityPredicate, value int) bool {
	switch p.Op {
	case "eq":
		return value == p.Value
	case "in":
		return slices.Contains(p.Values, value)
	case "lt":
		return value < p.Value
	case "lte":
		return value <= p.Value
	case "gt":
		return value > p.Value
	case "gte":
		return value >= p.Value
	default:
		return false
	}
}
