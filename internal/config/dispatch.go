package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// DispatchConfig holds the parsed dispatch rule set and default
// selection. The zero value selects the workflow-wide fallback for
// every issue.
type DispatchConfig struct {
	// Rules holds the ordered first-match-wins rule list. Empty when
	// no rules are configured. Order is preserved from the YAML source.
	Rules []DispatchRule

	// Default carries the workflow-author fallback selection used when
	// no rule matches. Zero value falls back further to the top-level
	// agent kind and the WORKFLOW.md body template.
	Default DispatchSelection
}

// DispatchRule pairs a match block with the selection it produces.
// IsCatchAll is computed at load time so evaluation does not re-derive
// it per issue.
type DispatchRule struct {
	// Name is the operator-supplied rule identifier used in logs and
	// metrics. Empty when the YAML omits the key.
	Name string

	// Match holds the per-key predicates evaluated with AND across
	// keys and OR within a key. A zero Match matches every issue.
	Match DispatchMatch

	// Selection holds the agent kind and template path overrides.
	// Either field may be empty; missing fields fall through to the
	// dispatch default and finally to the workflow-wide defaults.
	Selection DispatchSelection

	// IsCatchAll is true when Match carries no keys. Set by the
	// builder and immutable thereafter.
	IsCatchAll bool
}

// DispatchMatch enumerates the per-key predicates a rule applies to
// the candidate issue. A key with a nil or empty slice is not
// evaluated.
type DispatchMatch struct {
	Labels     []string
	IssueType  []string
	Priority   *PriorityPredicate
	Identifier []string
	Assignee   []string
}

// PriorityPredicate carries a single numeric operator and its
// operand. Exactly one of Value or Values is meaningful depending on
// Op: "in" uses Values; all other operators use Value.
type PriorityPredicate struct {
	Op     string
	Value  int
	Values []int
}

// DispatchSelection carries the rule-level overrides for agent kind
// and template ID. Empty fields fall through to the dispatch default
// and the workflow-wide defaults.
type DispatchSelection struct {
	// AgentKind is the adapter kind to dispatch with. Empty selects
	// the fallback chain.
	AgentKind string

	// TemplateID is the resolved absolute template path used as the
	// registry key. Empty selects the WORKFLOW.md body template.
	TemplateID string
}

// ruleNamePattern enforces the rule-name lexical form.
var ruleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// matchKeyAllowed enumerates the closed set of recognized match keys.
var matchKeyAllowed = map[string]bool{
	"labels":     true,
	"issue_type": true,
	"priority":   true,
	"identifier": true,
	"assignee":   true,
}

// ruleKeyAllowed enumerates the closed set of recognized per-rule keys.
var ruleKeyAllowed = map[string]bool{
	"name":     true,
	"match":    true,
	"agent":    true,
	"template": true,
}

// priorityOpAllowed enumerates the closed set of recognized priority
// predicate operators.
var priorityOpAllowed = map[string]bool{
	"eq":  true,
	"in":  true,
	"lt":  true,
	"lte": true,
	"gt":  true,
	"gte": true,
}

// BuildDispatchConfig parses the dispatch sub-map from the raw front
// matter, validates structural and cross-reference rules, and resolves
// every referenced template path against workflowDir. Returns the zero
// value and nil error when the dispatch key is absent or its value is
// nil. Returns the first *ConfigError encountered; the builder does
// not accumulate errors.
//
// agentKindProbe is the orchestrator-supplied closure that reports
// whether the kind is currently registered. The builder rejects
// unknown kinds at load time so dispatch never spawns workers for an
// adapter that cannot be constructed.
func BuildDispatchConfig(raw map[string]any, workflowDir string, agentKindProbe func(kind string) bool) (DispatchConfig, error) {
	if raw == nil {
		return DispatchConfig{}, nil
	}
	dispatchRaw, exists := raw["dispatch"]
	if !exists || dispatchRaw == nil {
		return DispatchConfig{}, nil
	}
	dispatchMap, ok := dispatchRaw.(map[string]any)
	if !ok {
		return DispatchConfig{}, &ConfigError{
			Field:   "dispatch",
			Message: fmt.Sprintf("expected map, got %T", dispatchRaw),
		}
	}

	resolvedWorkflowDir, err := resolveWorkflowDir(workflowDir)
	if err != nil {
		return DispatchConfig{}, &ConfigError{
			Field:   "dispatch",
			Message: fmt.Sprintf("cannot resolve workflow directory: %v", err),
		}
	}

	rules, err := parseDispatchRules(dispatchMap["rules"])
	if err != nil {
		return DispatchConfig{}, err
	}
	if err := validateNoDuplicateRuleNames(rules); err != nil {
		return DispatchConfig{}, err
	}
	if err := validateCatchAllPosition(rules); err != nil {
		return DispatchConfig{}, err
	}

	defaultSel, err := parseDispatchDefault(dispatchMap["default"])
	if err != nil {
		return DispatchConfig{}, err
	}

	for i := range rules {
		if err := probeAgentKind(rules[i].Selection.AgentKind, agentKindProbe, fmt.Sprintf("dispatch.rules[%d].agent", i)); err != nil {
			return DispatchConfig{}, err
		}
	}
	if err := probeAgentKind(defaultSel.AgentKind, agentKindProbe, "dispatch.default.agent"); err != nil {
		return DispatchConfig{}, err
	}

	for i := range rules {
		resolved, err := resolveTemplatePath(rules[i].Selection.TemplateID, resolvedWorkflowDir, fmt.Sprintf("dispatch.rules[%d].template", i))
		if err != nil {
			return DispatchConfig{}, err
		}
		rules[i].Selection.TemplateID = resolved
	}
	resolvedDefaultTemplate, err := resolveTemplatePath(defaultSel.TemplateID, resolvedWorkflowDir, "dispatch.default.template")
	if err != nil {
		return DispatchConfig{}, err
	}
	defaultSel.TemplateID = resolvedDefaultTemplate

	return DispatchConfig{
		Rules:   rules,
		Default: defaultSel,
	}, nil
}

// parseDispatchRules converts the raw dispatch.rules YAML sequence
// into a typed slice. Returns an empty slice when the value is absent
// or nil.
func parseDispatchRules(raw any) ([]DispatchRule, error) {
	if raw == nil {
		return nil, nil
	}
	seq, ok := raw.([]any)
	if !ok {
		return nil, &ConfigError{
			Field:   "dispatch.rules",
			Message: fmt.Sprintf("expected sequence, got %T", raw),
		}
	}
	if len(seq) == 0 {
		return nil, nil
	}
	rules := make([]DispatchRule, 0, len(seq))
	for i, elem := range seq {
		rule, err := parseDispatchRule(i, elem)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// parseDispatchRule converts a single rule element into a DispatchRule.
func parseDispatchRule(index int, elem any) (DispatchRule, error) {
	ruleField := fmt.Sprintf("dispatch.rules[%d]", index)
	ruleMap, ok := elem.(map[string]any)
	if !ok {
		return DispatchRule{}, &ConfigError{
			Field:   ruleField,
			Message: fmt.Sprintf("expected map, got %T", elem),
		}
	}

	for key := range ruleMap {
		if !ruleKeyAllowed[key] {
			return DispatchRule{}, &ConfigError{
				Field:   ruleField + "." + key,
				Message: "unknown key",
			}
		}
	}

	hasMatch := ruleMap["match"] != nil
	hasAgent := false
	if v, ok := ruleMap["agent"]; ok && v != nil {
		hasAgent = true
	}
	hasTemplate := false
	if v, ok := ruleMap["template"]; ok && v != nil {
		hasTemplate = true
	}
	if !hasMatch && !hasAgent && !hasTemplate {
		return DispatchRule{}, &ConfigError{
			Field:   ruleField,
			Message: "rule must specify at least one of match, agent, or template",
		}
	}

	name, err := extractRuleName(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, err
	}

	match, err := parseDispatchMatch(ruleMap["match"], ruleField+".match")
	if err != nil {
		return DispatchRule{}, err
	}

	selection, err := parseDispatchSelection(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, err
	}

	return DispatchRule{
		Name:       name,
		Match:      match,
		Selection:  selection,
		IsCatchAll: isEmptyMatch(match),
	}, nil
}

// extractRuleName reads and validates the optional rule name.
func extractRuleName(ruleMap map[string]any, ruleField string) (string, error) {
	raw, ok := ruleMap["name"]
	if !ok || raw == nil {
		return "", nil
	}
	name, ok := raw.(string)
	if !ok {
		return "", &ConfigError{
			Field:   ruleField + ".name",
			Message: fmt.Sprintf("expected string, got %T", raw),
		}
	}
	if name == "" {
		return "", nil
	}
	if !ruleNamePattern.MatchString(name) {
		return "", &ConfigError{
			Field:   ruleField + ".name",
			Message: fmt.Sprintf("must match ^[a-z][a-z0-9_-]*$, got %q", name),
		}
	}
	return name, nil
}

// parseDispatchMatch decodes the per-key predicates in a rule's match
// block. Returns the zero DispatchMatch when raw is nil.
func parseDispatchMatch(raw any, matchField string) (DispatchMatch, error) {
	if raw == nil {
		return DispatchMatch{}, nil
	}
	matchMap, ok := raw.(map[string]any)
	if !ok {
		return DispatchMatch{}, &ConfigError{
			Field:   matchField,
			Message: fmt.Sprintf("expected map, got %T", raw),
		}
	}

	for key := range matchMap {
		if !matchKeyAllowed[key] {
			return DispatchMatch{}, &ConfigError{
				Field:   matchField + "." + key,
				Message: "unknown match key",
			}
		}
	}

	labels, err := parseStringList(matchMap["labels"], matchField+".labels")
	if err != nil {
		return DispatchMatch{}, err
	}
	if err := validateGlobPatterns(labels, matchField+".labels"); err != nil {
		return DispatchMatch{}, err
	}

	issueTypes, err := parseStringList(matchMap["issue_type"], matchField+".issue_type")
	if err != nil {
		return DispatchMatch{}, err
	}

	identifiers, err := parseStringList(matchMap["identifier"], matchField+".identifier")
	if err != nil {
		return DispatchMatch{}, err
	}
	if err := validateGlobPatterns(identifiers, matchField+".identifier"); err != nil {
		return DispatchMatch{}, err
	}

	assignees, err := parseStringList(matchMap["assignee"], matchField+".assignee")
	if err != nil {
		return DispatchMatch{}, err
	}

	var priority *PriorityPredicate
	if rawPrio, ok := matchMap["priority"]; ok && rawPrio != nil {
		p, err := parsePriorityPredicate(rawPrio, matchField+".priority")
		if err != nil {
			return DispatchMatch{}, err
		}
		priority = p
	}

	return DispatchMatch{
		Labels:     labels,
		IssueType:  issueTypes,
		Priority:   priority,
		Identifier: identifiers,
		Assignee:   assignees,
	}, nil
}

// parseStringList decodes a YAML scalar or sequence into []string.
// Returns nil when raw is nil. Scalar strings are wrapped in a
// one-element slice for ergonomics.
func parseStringList(raw any, field string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	if s, ok := raw.(string); ok {
		return []string{s}, nil
	}
	seq, ok := raw.([]any)
	if !ok {
		return nil, &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("expected string or sequence of strings, got %T", raw),
		}
	}
	out := make([]string, 0, len(seq))
	for i, elem := range seq {
		s, ok := elem.(string)
		if !ok {
			return nil, &ConfigError{
				Field:   fmt.Sprintf("%s[%d]", field, i),
				Message: fmt.Sprintf("expected string, got %T", elem),
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// validateGlobPatterns rejects malformed glob patterns at load time.
func validateGlobPatterns(patterns []string, field string) error {
	for i, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return &ConfigError{
				Field:   fmt.Sprintf("%s[%d]", field, i),
				Message: fmt.Sprintf("invalid glob pattern: %v", err),
			}
		}
	}
	return nil
}

// parsePriorityPredicate decodes a single-operator numeric predicate.
func parsePriorityPredicate(raw any, field string) (*PriorityPredicate, error) {
	predMap, ok := raw.(map[string]any)
	if !ok {
		return nil, &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("expected map, got %T", raw),
		}
	}

	var opFound string
	for k := range predMap {
		if !priorityOpAllowed[k] {
			return nil, &ConfigError{
				Field:   field,
				Message: fmt.Sprintf("unknown operator %q; expected one of eq, in, lt, lte, gt, gte", k),
			}
		}
		if opFound != "" {
			return nil, &ConfigError{
				Field:   field,
				Message: "expected exactly one of eq, in, lt, lte, gt, gte",
			}
		}
		opFound = k
	}
	if opFound == "" {
		return nil, &ConfigError{
			Field:   field,
			Message: "expected exactly one of eq, in, lt, lte, gt, gte",
		}
	}

	pred := &PriorityPredicate{Op: opFound}
	rawVal := predMap[opFound]
	if opFound == "in" {
		seq, ok := rawVal.([]any)
		if !ok {
			return nil, &ConfigError{
				Field:   field + ".in",
				Message: fmt.Sprintf("expected sequence of integers, got %T", rawVal),
			}
		}
		pred.Values = make([]int, 0, len(seq))
		for i, elem := range seq {
			n, err := coerceInt(elem)
			if err != nil {
				return nil, &ConfigError{
					Field:   fmt.Sprintf("%s.in[%d]", field, i),
					Message: fmt.Sprintf("expected integer, got %T", elem),
				}
			}
			pred.Values = append(pred.Values, n)
		}
		return pred, nil
	}
	n, err := coerceInt(rawVal)
	if err != nil {
		return nil, &ConfigError{
			Field:   field + "." + opFound,
			Message: fmt.Sprintf("expected integer, got %T", rawVal),
		}
	}
	pred.Value = n
	return pred, nil
}

// parseDispatchSelection extracts the agent and template fields from
// the rule map. The template value is carried verbatim through this
// step; cross-reference resolution happens later.
func parseDispatchSelection(ruleMap map[string]any, ruleField string) (DispatchSelection, error) {
	var sel DispatchSelection

	if raw, ok := ruleMap["agent"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return DispatchSelection{}, &ConfigError{
				Field:   ruleField + ".agent",
				Message: fmt.Sprintf("expected string, got %T", raw),
			}
		}
		sel.AgentKind = s
	}

	if raw, ok := ruleMap["template"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return DispatchSelection{}, &ConfigError{
				Field:   ruleField + ".template",
				Message: fmt.Sprintf("expected string, got %T", raw),
			}
		}
		sel.TemplateID = s
	}

	return sel, nil
}

// parseDispatchDefault decodes the optional dispatch.default block.
func parseDispatchDefault(raw any) (DispatchSelection, error) {
	if raw == nil {
		return DispatchSelection{}, nil
	}
	defaultMap, ok := raw.(map[string]any)
	if !ok {
		return DispatchSelection{}, &ConfigError{
			Field:   "dispatch.default",
			Message: fmt.Sprintf("expected map, got %T", raw),
		}
	}

	allowed := map[string]bool{"agent": true, "template": true}
	for key := range defaultMap {
		if !allowed[key] {
			return DispatchSelection{}, &ConfigError{
				Field:   "dispatch.default." + key,
				Message: "unknown key",
			}
		}
	}

	var sel DispatchSelection
	if raw, ok := defaultMap["agent"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return DispatchSelection{}, &ConfigError{
				Field:   "dispatch.default.agent",
				Message: fmt.Sprintf("expected string, got %T", raw),
			}
		}
		sel.AgentKind = s
	}
	if raw, ok := defaultMap["template"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return DispatchSelection{}, &ConfigError{
				Field:   "dispatch.default.template",
				Message: fmt.Sprintf("expected string, got %T", raw),
			}
		}
		sel.TemplateID = s
	}
	return sel, nil
}

// validateNoDuplicateRuleNames returns the first *ConfigError when two
// rules share the same non-empty name.
func validateNoDuplicateRuleNames(rules []DispatchRule) error {
	seen := make(map[string]int, len(rules))
	for i, r := range rules {
		if r.Name == "" {
			continue
		}
		if first, exists := seen[r.Name]; exists {
			return &ConfigError{
				Field:   fmt.Sprintf("dispatch.rules[%d]", i),
				Message: fmt.Sprintf("duplicate rule name %q (first at index %d)", r.Name, first),
			}
		}
		seen[r.Name] = i
	}
	return nil
}

// validateCatchAllPosition rejects any rule that lacks a match block
// (catch-all) and is followed by another rule.
func validateCatchAllPosition(rules []DispatchRule) error {
	for i, r := range rules {
		if !r.IsCatchAll {
			continue
		}
		if i != len(rules)-1 {
			return &ConfigError{
				Field:   fmt.Sprintf("dispatch.rules[%d]", i),
				Message: fmt.Sprintf("unreachable_rules: catch-all rule at index %d precedes rule at index %d", i, i+1),
			}
		}
	}
	return nil
}

// probeAgentKind validates that the kind, when non-empty, is currently
// registered. A nil probe defaults to permissive behavior so callers
// that have no orchestrator dependency (dryrun, tests) keep working.
func probeAgentKind(kind string, probe func(string) bool, field string) error {
	if kind == "" {
		return nil
	}
	if probe == nil {
		return nil
	}
	if probe(kind) {
		return nil
	}
	return &ConfigError{
		Field:   field,
		Message: fmt.Sprintf("unknown agent kind %q", kind),
	}
}

// resolveTemplatePath validates a per-rule template path. The literal
// value gate rejects absolute paths and tilde expansion before any
// filesystem call. The symlink/prefix gate rejects targets that
// resolve outside the workflow directory tree. Returns the resolved
// absolute path used as the template registry key. Empty input
// returns empty output (the body template sentinel).
func resolveTemplatePath(raw, workflowDir, field string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if filepath.IsAbs(raw) {
		return "", &ConfigError{
			Field:   field,
			Message: "template path must be relative to WORKFLOW.md",
		}
	}
	if strings.HasPrefix(raw, "~") {
		return "", &ConfigError{
			Field:   field,
			Message: "template path must be relative to WORKFLOW.md",
		}
	}

	joined := filepath.Join(workflowDir, raw)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("cannot read template: %v", err),
		}
	}

	if !isUnderDirectory(resolved, workflowDir) {
		return "", &ConfigError{
			Field:   field,
			Message: "template path escapes workflow directory tree",
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("cannot read template: %v", err),
		}
	}
	if !info.Mode().IsRegular() {
		return "", &ConfigError{
			Field:   field,
			Message: "template path must be a regular file",
		}
	}

	return resolved, nil
}

// resolveWorkflowDir evaluates symlinks for the workflow directory so
// the prefix containment check operates on canonical paths.
func resolveWorkflowDir(workflowDir string) (string, error) {
	if workflowDir == "" {
		return "", fmt.Errorf("workflow directory is empty")
	}
	abs := workflowDir
	if !filepath.IsAbs(abs) {
		converted, err := filepath.Abs(abs)
		if err != nil {
			return "", err
		}
		abs = converted
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// isUnderDirectory reports whether candidate is the same path as root
// or a path nested under root, using clean filesystem semantics. Both
// arguments must be absolute paths produced by [filepath.EvalSymlinks]
// to avoid relative-path false negatives.
//
// The relative path is evaluated with [filepath.IsLocal] so component
// names that happen to contain ".." as a substring (for example,
// "prompts/v1..md") are accepted, while genuine upward-escape
// segments are rejected.
func isUnderDirectory(candidate, root string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return filepath.IsLocal(rel)
}

// isEmptyMatch reports whether the match block carries no keys and
// therefore matches every issue.
func isEmptyMatch(m DispatchMatch) bool {
	return len(m.Labels) == 0 &&
		len(m.IssueType) == 0 &&
		len(m.Identifier) == 0 &&
		len(m.Assignee) == 0 &&
		m.Priority == nil
}
