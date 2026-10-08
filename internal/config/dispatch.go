package config

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// DispatchConfig holds the parsed dispatch rule set and default
// selection. The zero value selects the workflow-wide fallback for
// every issue.
type DispatchConfig struct {
	// Rules holds the rule list in YAML order. Rules without a stage
	// label are evaluated first-match-wins; a rule with a stage label is
	// selected by that label. Empty when no rules are configured.
	Rules []DispatchRule

	// Default carries the workflow-author fallback selection used when
	// no rule matches. Zero value falls back further to the top-level
	// agent kind and the WORKFLOW.md body template.
	Default DispatchSelection

	// MaxConsecutiveHops is the per-issue ceiling on consecutive automatic
	// hops: dispatch.max_consecutive_hops as written, or the larger of 10
	// and the hop count of the longest chain when the key is absent or
	// null. Zero when the workflow has no dispatch section.
	MaxConsecutiveHops int
}

// DispatchRule pairs a match block with the selection it produces.
// IsCatchAll is computed at load time so evaluation does not re-derive
// it per issue.
type DispatchRule struct {
	// Name is the operator-supplied rule identifier used in logs and
	// metrics. Empty when the YAML omits the key.
	Name string

	// Stage is the rule's stage label as written in WORKFLOW.md, empty
	// when the rule has no stage key. A rule with a stage label is
	// selected by that label ahead of the ordered rules and never by Match.
	Stage string

	// Next is the name of the rule a successful run advances the issue to,
	// as written; empty when the rule has no next key.
	Next string

	// Match holds the per-key predicates evaluated with AND across
	// keys and OR within a key. A zero Match matches every issue.
	Match DispatchMatch

	// Selection holds the agent kind and template path overrides.
	// Either field may be empty; missing fields fall through to the
	// dispatch default and finally to the workflow-wide defaults.
	Selection DispatchSelection

	// IsCatchAll is true when Stage is empty and Match carries no keys.
	// Set by the builder and immutable thereafter.
	IsCatchAll bool

	// SettingsKind is the kind the rule's settings block is named for,
	// empty when the rule carries none.
	SettingsKind string

	// Settings is the rule's settings block with $VAR references
	// resolved. A nil value removes the inherited key. Read-only.
	Settings map[string]any
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

	// Title holds the title phrases as written, in YAML order. Nil when
	// the key is absent.
	Title []string
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
	"title":      true,
}

// ruleBlockForbiddenKeys maps each key a rule's settings block may not
// write to its message: the agent section owns these keys.
var ruleBlockForbiddenKeys = map[string]string{
	"kind":             "a rule chooses its agent kind with its agent key",
	"command":          "agent.command belongs to the default agent kind; a dispatch rule cannot set a command",
	"turn_timeout_ms":  "agent.turn_timeout_ms is workflow-wide; a dispatch rule cannot override it",
	"read_timeout_ms":  "agent.read_timeout_ms is workflow-wide; a dispatch rule cannot override it",
	"stall_timeout_ms": "agent.stall_timeout_ms is workflow-wide; a dispatch rule cannot override it",
	"stop_grace_ms":    "agent.stop_grace_ms is workflow-wide; a dispatch rule cannot override it",
}

// defaultMaxConsecutiveHops is the ceiling when dispatch.max_consecutive_hops
// is unset and the longest chain needs fewer hops.
const defaultMaxConsecutiveHops = 10

// chainSeparator joins rule names in chain paths shown to operators.
const chainSeparator = " -> "

// defaultRuleName is the name run history gives the dispatch.default
// selection.
const defaultRuleName = "default"

// ruleKeyAllowed enumerates the closed set of recognized per-rule keys.
var ruleKeyAllowed = map[string]bool{
	"name":     true,
	"stage":    true,
	"next":     true,
	"match":    true,
	"agent":    true,
	"template": true,
}

// StageLabelsEqual reports whether two stage labels name the same label.
// Selection, duplicate detection, and the collision check against tracker
// states and applied labels all compare through it, so they cannot
// disagree. The comparison folds case only: tracker labels arrive
// lowercased and untrimmed, and a stage label is taken literally.
func StageLabelsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// RuleByName returns the rule with exactly the given name. An empty name is
// never found, so an unnamed rule cannot be reached by name.
func (d DispatchConfig) RuleByName(name string) (DispatchRule, bool) {
	i := d.ruleIndex(name)
	if i < 0 {
		return DispatchRule{}, false
	}
	return d.Rules[i], true
}

// LongestChain returns the rule names of the longest path along next links,
// or nil when no rule carries next. Among paths of equal length it returns
// the one whose first rule has the lowest index. The hop count of the chain
// is its length minus one.
func (d DispatchConfig) LongestChain() []string {
	var longest []int
	for i := range d.Rules {
		if walk := d.walkNext(i); len(walk) > len(longest) {
			longest = walk
		}
	}
	if len(longest) < 2 {
		return nil
	}
	return d.ruleNames(longest)
}

// HopCeiling returns the per-issue ceiling on consecutive automatic hops in
// force: MaxConsecutiveHops when positive, otherwise the larger of the
// default and the hop count of the longest chain. A configuration without a
// dispatch section carries a zero MaxConsecutiveHops, so callers read the
// ceiling here rather than from the field.
func (d DispatchConfig) HopCeiling() int {
	if d.MaxConsecutiveHops > 0 {
		return d.MaxConsecutiveHops
	}
	return max(defaultMaxConsecutiveHops, len(d.LongestChain())-1)
}

// ChainPath returns the configured next path through the named rule: the
// rule itself, the rules that lead to it while exactly one rule names the
// path's first rule as its next, and the rules that follow it. No rule
// appears twice, so a path through a loop ends before it repeats.
func (d DispatchConfig) ChainPath(ruleName string) []string {
	path := []string{ruleName}
	for {
		predecessor := d.soleRuleNaming(path[0])
		if predecessor == "" || slices.Contains(path, predecessor) {
			break
		}
		path = slices.Insert(path, 0, predecessor)
	}
	for {
		source, ok := d.RuleByName(path[len(path)-1])
		if !ok || source.Next == "" || slices.Contains(path, source.Next) {
			break
		}
		if _, ok := d.RuleByName(source.Next); !ok {
			break
		}
		path = append(path, source.Next)
	}
	return path
}

// ruleIndex returns the index of the rule with the given name, or -1.
func (d DispatchConfig) ruleIndex(name string) int {
	if name == "" {
		return -1
	}
	return slices.IndexFunc(d.Rules, func(r DispatchRule) bool { return r.Name == name })
}

// walkNext returns the indices of the rules reached from start by following
// next, start included. The walk ends at a rule without next, at a next that
// names no rule, or before a rule it already holds.
func (d DispatchConfig) walkNext(start int) []int {
	walk := []int{start}
	for {
		following := d.ruleIndex(d.Rules[walk[len(walk)-1]].Next)
		if following < 0 || slices.Contains(walk, following) {
			return walk
		}
		walk = append(walk, following)
	}
}

// soleRuleNaming returns the name of the rule whose next is name, or empty
// when no rule or several rules name it.
func (d DispatchConfig) soleRuleNaming(name string) string {
	if name == "" {
		return ""
	}
	found := ""
	for _, rule := range d.Rules {
		if rule.Next != name {
			continue
		}
		if found != "" {
			return ""
		}
		found = rule.Name
	}
	return found
}

func (d DispatchConfig) ruleNames(indices []int) []string {
	names := make([]string, len(indices))
	for i, index := range indices {
		names[i] = d.Rules[index].Name
	}
	return names
}

// ValidateNextRequiresHandoff reports a *ConfigError for the first rule, in
// list order, that carries next when handoffState is empty: a chain's last
// stage ends on that state, and so does every hop that is not made.
func ValidateNextRequiresHandoff(dispatch DispatchConfig, handoffState string) error {
	if handoffState != "" {
		return nil
	}
	for i, rule := range dispatch.Rules {
		if rule.Next != "" {
			return &ConfigError{
				Field:   fmt.Sprintf("dispatch.rules[%d].next", i),
				Message: "next requires tracker.handoff_state, the state a stage ends on when a hop is not made",
			}
		}
	}
	return nil
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
// adapter that cannot be constructed. A rule key that names a kind per
// the probe, or equals the rule's own agent value, is its settings block.
// agentKind is the workflow-wide kind a rule falls back to.
func BuildDispatchConfig(raw map[string]any, workflowDir string, agentKindProbe func(kind string) bool, agentKind string) (DispatchConfig, error) {
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

	rules, blocks, err := parseDispatchRules(dispatchMap["rules"], agentKindProbe)
	if err != nil {
		return DispatchConfig{}, err
	}
	if err := validateNoDuplicateRuleNames(rules); err != nil {
		return DispatchConfig{}, err
	}
	if err := validateNoDuplicateStageLabels(rules); err != nil {
		return DispatchConfig{}, err
	}
	if err := validateCatchAllPosition(rules); err != nil {
		return DispatchConfig{}, err
	}
	if err := validateNextLinks(rules); err != nil {
		return DispatchConfig{}, err
	}
	maxHops, err := parseMaxConsecutiveHops(dispatchMap["max_consecutive_hops"], DispatchConfig{Rules: rules}.LongestChain())
	if err != nil {
		return DispatchConfig{}, err
	}

	defaultSel, err := parseDispatchDefault(dispatchMap["default"], agentKindProbe)
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
		if err := applyRuleSettingsBlock(&rules[i], blocks[i], i, defaultSel.AgentKind, agentKind); err != nil {
			return DispatchConfig{}, err
		}
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
		Rules:              rules,
		Default:            defaultSel,
		MaxConsecutiveHops: maxHops,
	}, nil
}

// parseDispatchRules converts the raw dispatch.rules YAML sequence
// into a typed slice and, index for index, each rule's settings block keys.
func parseDispatchRules(raw any, agentKindProbe func(kind string) bool) ([]DispatchRule, []map[string]any, error) {
	if raw == nil {
		return nil, nil, nil
	}
	seq, ok := raw.([]any)
	if !ok {
		return nil, nil, &ConfigError{
			Field:   "dispatch.rules",
			Message: fmt.Sprintf("expected sequence, got %T", raw),
		}
	}
	if len(seq) == 0 {
		return nil, nil, nil
	}
	rules := make([]DispatchRule, 0, len(seq))
	blocks := make([]map[string]any, 0, len(seq))
	for i, elem := range seq {
		rule, block, err := parseDispatchRule(i, elem, agentKindProbe)
		if err != nil {
			return nil, nil, err
		}
		rules = append(rules, rule)
		blocks = append(blocks, block)
	}
	return rules, blocks, nil
}

// parseDispatchRule converts a single rule element into a DispatchRule
// and its settings block keys, which [applyRuleSettingsBlock] checks once
// every kind is known.
func parseDispatchRule(index int, elem any, agentKindProbe func(kind string) bool) (DispatchRule, map[string]any, error) {
	ruleField := fmt.Sprintf("dispatch.rules[%d]", index)
	ruleMap, ok := elem.(map[string]any)
	if !ok {
		return DispatchRule{}, nil, &ConfigError{
			Field:   ruleField,
			Message: fmt.Sprintf("expected map, got %T", elem),
		}
	}

	ownAgent, fault := typeutil.StringField(ruleMap, "agent")
	if fault != nil {
		return DispatchRule{}, nil, &ConfigError{Field: ruleField + ".agent", Message: fault.Reason()}
	}
	blockKeys := make(map[string]any)
	for key, value := range ruleMap {
		switch {
		case ruleKeyAllowed[key]:
		case namesAgentKind(key, ownAgent, agentKindProbe):
			blockKeys[key] = value
		default:
			return DispatchRule{}, nil, &ConfigError{
				Field:   ruleField + "." + key,
				Message: "unknown key",
			}
		}
	}

	stage, hasStage, err := parseStageLabel(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, nil, err
	}

	next, err := parseNextRule(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, nil, err
	}

	hasMatch := ruleMap["match"] != nil
	if hasStage && hasMatch {
		return DispatchRule{}, nil, &ConfigError{
			Field:   ruleField,
			Message: "a rule with a stage label is selected by that label and cannot also carry match",
		}
	}
	hasAgent := false
	if v, ok := ruleMap["agent"]; ok && v != nil {
		hasAgent = true
	}
	hasTemplate := false
	if v, ok := ruleMap["template"]; ok && v != nil {
		hasTemplate = true
	}
	if !hasMatch && !hasStage && !hasAgent && !hasTemplate && len(blockKeys) == 0 {
		return DispatchRule{}, nil, &ConfigError{
			Field:   ruleField,
			Message: "rule must specify at least one of match, stage, agent, template, or a settings block",
		}
	}

	name, err := extractRuleName(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, nil, err
	}
	if hasStage && name == "" {
		return DispatchRule{}, nil, &ConfigError{
			Field:   ruleField,
			Message: "a rule that carries a stage label must have a name",
		}
	}
	if next != "" && name == "" {
		return DispatchRule{}, nil, &ConfigError{
			Field:   ruleField,
			Message: "a rule that carries next must have a name",
		}
	}

	match, err := parseDispatchMatch(ruleMap["match"], ruleField+".match")
	if err != nil {
		return DispatchRule{}, nil, err
	}

	selection, err := parseDispatchSelection(ruleMap, ruleField)
	if err != nil {
		return DispatchRule{}, nil, err
	}

	return DispatchRule{
		Name:       name,
		Stage:      stage,
		Next:       next,
		Match:      match,
		Selection:  selection,
		IsCatchAll: stage == "" && isEmptyMatch(match),
	}, blockKeys, nil
}

// parseStageLabel reads a rule's optional stage key. present reports
// whether the key exists, a null value included, because dropping a blank
// label would leave a rule that no issue can select. The label is returned
// as written.
func parseStageLabel(ruleMap map[string]any, ruleField string) (label string, present bool, err error) {
	raw, present := ruleMap["stage"]
	if !present {
		return "", false, nil
	}
	field := ruleField + ".stage"
	if raw == nil {
		return "", true, errStageNeedsLabel(field)
	}
	label, ok := raw.(string)
	if !ok {
		return "", true, &ConfigError{
			Field:   field,
			Message: "expected a label, got " + describeExtensionValue(raw),
		}
	}
	if strings.TrimFunc(label, unicode.IsSpace) == "" {
		return "", true, errStageNeedsLabel(field)
	}
	return label, true, nil
}

// parseNextRule reads a rule's optional next key and returns the rule name
// as written, empty when the key is absent. A null value is a fault, like a
// blank one, because dropping it would turn a chained rule into a last stage.
func parseNextRule(ruleMap map[string]any, ruleField string) (string, error) {
	raw, present := ruleMap["next"]
	if !present {
		return "", nil
	}
	field := ruleField + ".next"
	if raw == nil {
		return "", errNextNeedsName(field)
	}
	next, ok := raw.(string)
	if !ok {
		return "", &ConfigError{
			Field:   field,
			Message: "expected a rule name, got " + describeExtensionValue(raw),
		}
	}
	if strings.TrimFunc(next, unicode.IsSpace) == "" {
		return "", errNextNeedsName(field)
	}
	return next, nil
}

func errNextNeedsName(field string) error {
	return &ConfigError{Field: field, Message: "needs a rule name"}
}

func errStageNeedsLabel(field string) error {
	return &ConfigError{
		Field:   field,
		Message: "needs a label with a character other than white space",
	}
}

// namesAgentKind reports whether key is a registered kind or the rule's
// own agent value.
func namesAgentKind(key, ownAgent string, agentKindProbe func(kind string) bool) bool {
	if ownAgent != "" && key == ownAgent {
		return true
	}
	return agentKindProbe != nil && agentKindProbe(key)
}

// applyRuleSettingsBlock checks a rule's settings block against the kind
// the rule runs (its own agent, else dispatch.default.agent, else
// workflowKind) and stores it on the rule.
func applyRuleSettingsBlock(rule *DispatchRule, block map[string]any, index int, defaultKind, workflowKind string) error {
	if len(block) == 0 {
		return nil
	}
	ruleField := fmt.Sprintf("dispatch.rules[%d]", index)

	kind, origin := rule.Selection.AgentKind, ""
	switch {
	case kind != "":
	case defaultKind != "":
		kind, origin = defaultKind, ", taken from dispatch.default.agent"
	default:
		kind, origin = workflowKind, ", taken from agent.kind"
	}

	for _, key := range slices.Sorted(maps.Keys(block)) {
		if key != kind {
			return &ConfigError{
				Field:   ruleField + "." + key,
				Message: fmt.Sprintf("settings block for agent kind %q, but this rule runs agent kind %q%s", key, kind, origin),
			}
		}
	}

	blockMap, isMap := block[kind].(map[string]any)
	if !isMap {
		shape := "no value"
		if block[kind] != nil {
			shape = describeExtensionValue(block[kind])
		}
		return &ConfigError{
			Field:   ruleField + "." + kind,
			Message: fmt.Sprintf("a rule's settings block must hold the kind's settings as keys, got %s; write {} for an empty block", shape),
		}
	}

	for _, key := range slices.Sorted(maps.Keys(blockMap)) {
		if message, forbidden := ruleBlockForbiddenKeys[key]; forbidden {
			return &ConfigError{Field: ruleField + "." + kind + "." + key, Message: message}
		}
	}

	if rule.Name == "" {
		return &ConfigError{
			Field:   ruleField,
			Message: "a rule that carries a settings block must have a name",
		}
	}
	if rule.Name == defaultRuleName {
		return &ConfigError{
			Field:   ruleField + ".name",
			Message: fmt.Sprintf("%q is the name run history and statistics give the dispatch.default selection; a rule that carries a settings block must use another name", defaultRuleName),
		}
	}

	rule.SettingsKind = kind
	rule.Settings = blockMap
	return nil
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

	rawTitle, titlePresent := matchMap["title"]
	title, err := parseTitlePhrases(rawTitle, titlePresent, matchField+".title")
	if err != nil {
		return DispatchMatch{}, err
	}

	return DispatchMatch{
		Labels:     labels,
		IssueType:  issueTypes,
		Priority:   priority,
		Identifier: identifiers,
		Assignee:   assignees,
		Title:      title,
	}, nil
}

// parseTitlePhrases validates the raw value of one match block's title
// key and returns its phrases as written. present reports whether the
// key exists; field is the path of the key. Unlike the other match
// keys, a null or empty value is a fault, because dropping the key
// would turn a title-only rule into a catch-all. Every element is
// type-checked before any phrase is checked for emptiness, and the
// first fault is returned.
func parseTitlePhrases(raw any, present bool, field string) ([]string, error) {
	if !present {
		return nil, nil
	}

	var phrases []string
	switch v := raw.(type) {
	case nil:
		return nil, errTitleNeedsPhrase(field)
	case string:
		phrases = []string{v}
	case []any:
		if len(v) == 0 {
			return nil, errTitleNeedsPhrase(field)
		}
		phrases = make([]string, len(v))
		for j, elem := range v {
			phrase, ok := elem.(string)
			if !ok {
				shape := "no value"
				if elem != nil {
					shape = describeExtensionValue(elem)
				}
				return nil, &ConfigError{
					Field:   fmt.Sprintf("%s[%d]", field, j),
					Message: "expected a phrase, got " + shape + titleQuotingHint(shape),
				}
			}
			phrases[j] = phrase
		}
	default:
		shape := describeExtensionValue(raw)
		return nil, &ConfigError{
			Field:   field,
			Message: "expected a phrase or a list of phrases, got " + shape + titleQuotingHint(shape),
		}
	}

	for j, phrase := range phrases {
		if strings.TrimFunc(phrase, unicode.IsSpace) == "" {
			return nil, &ConfigError{
				Field:   fmt.Sprintf("%s[%d]", field, j),
				Message: "a phrase needs a character other than white space",
			}
		}
	}
	return phrases, nil
}

func errTitleNeedsPhrase(field string) error {
	return &ConfigError{
		Field:   field,
		Message: "needs at least one phrase; remove the key to leave the title out of the match",
	}
}

// titleQuotingHint returns the YAML quoting advice for a title value
// whose shape shows an unquoted phrase that YAML read as something
// else. A null element carries no advice.
func titleQuotingHint(shape string) string {
	switch shape {
	case "a list":
		return `; quote a phrase that starts with "[", as in "[infra]"`
	case "a map":
		return `; quote a phrase that contains ": ", as in "fix: typo"`
	case "no value":
		return ""
	default:
		return "; quote the phrase"
	}
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
					Message: integerFaultMessage(err, fmt.Sprintf("expected integer, got %T", elem)),
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
			Message: integerFaultMessage(err, fmt.Sprintf("expected integer, got %T", rawVal)),
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
func parseDispatchDefault(raw any, agentKindProbe func(kind string) bool) (DispatchSelection, error) {
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
		if allowed[key] {
			continue
		}
		message := "unknown key"
		if agentKindProbe != nil && agentKindProbe(key) {
			message = fmt.Sprintf("dispatch.default carries no settings block; the top-level %s block holds the default settings", key)
		}
		return DispatchSelection{}, &ConfigError{
			Field:   "dispatch.default." + key,
			Message: message,
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

// validateNoDuplicateStageLabels returns the first *ConfigError when two
// rules carry stage labels that [StageLabelsEqual] treats as one.
func validateNoDuplicateStageLabels(rules []DispatchRule) error {
	for j, rule := range rules {
		if rule.Stage == "" {
			continue
		}
		for i := range j {
			if rules[i].Stage != "" && StageLabelsEqual(rules[i].Stage, rule.Stage) {
				return &ConfigError{
					Field:   fmt.Sprintf("dispatch.rules[%d].stage", j),
					Message: fmt.Sprintf("duplicate stage label %q (first at index %d)", rule.Stage, i),
				}
			}
		}
	}
	return nil
}

// validateCatchAllPosition rejects a catch-all rule followed by a rule
// without a stage label. A rule with a stage label is reachable after a
// catch-all because the stage label selects it ahead of the ordered rules.
func validateCatchAllPosition(rules []DispatchRule) error {
	for i, r := range rules {
		if !r.IsCatchAll {
			continue
		}
		for j := i + 1; j < len(rules); j++ {
			if rules[j].Stage == "" {
				return &ConfigError{
					Field:   fmt.Sprintf("dispatch.rules[%d]", i),
					Message: fmt.Sprintf("unreachable_rules: catch-all rule at index %d precedes rule at index %d", i, j),
				}
			}
		}
	}
	return nil
}

// validateNextLinks returns the first *ConfigError among the next links:
// a target that does not exist, is the rule itself, or carries no stage
// label, then a loop. A loop is reported on its lowest-index rule with the
// path starting there, so the message does not depend on where a walk enters.
func validateNextLinks(rules []DispatchRule) error {
	dispatch := DispatchConfig{Rules: rules}
	for i, rule := range rules {
		if rule.Next == "" {
			continue
		}
		field := fmt.Sprintf("dispatch.rules[%d].next", i)
		target := dispatch.ruleIndex(rule.Next)
		switch {
		case target < 0:
			return &ConfigError{Field: field, Message: fmt.Sprintf("next %q names no dispatch rule", rule.Next)}
		case target == i:
			return &ConfigError{Field: field, Message: "next names the rule that carries it"}
		case rules[target].Stage == "":
			return &ConfigError{
				Field:   field,
				Message: fmt.Sprintf("next %q names a rule without a stage label; the rule a next names must carry stage", rule.Next),
			}
		}
	}

	for i := range rules {
		walk := dispatch.walkNext(i)
		reentry := dispatch.ruleIndex(rules[walk[len(walk)-1]].Next)
		if reentry < 0 {
			continue
		}
		loop := walk[slices.Index(walk, reentry):]
		lowest := slices.Min(loop)
		start := slices.Index(loop, lowest)
		cycle := append(slices.Clone(loop[start:]), loop[:start]...)
		cycle = append(cycle, lowest)
		return &ConfigError{
			Field:   fmt.Sprintf("dispatch.rules[%d].next", lowest),
			Message: "next links form a cycle: " + strings.Join(dispatch.ruleNames(cycle), chainSeparator),
		}
	}
	return nil
}

// parseMaxConsecutiveHops reads dispatch.max_consecutive_hops. An absent or
// null value yields the larger of the default and the hop count of the
// longest chain, so a long chain never needs the key.
func parseMaxConsecutiveHops(raw any, longestChain []string) (int, error) {
	longestHops := max(len(longestChain)-1, 0)
	if raw == nil {
		return max(defaultMaxConsecutiveHops, longestHops), nil
	}
	const field = "dispatch.max_consecutive_hops"
	n, err := coerceInt(raw)
	if err != nil {
		return 0, &ConfigError{Field: field, Message: integerFaultMessage(err, fmt.Sprintf("invalid integer value: %v", raw))}
	}
	if n <= 0 {
		return 0, &ConfigError{Field: field, Message: "must be greater than 0"}
	}
	if n < longestHops {
		return 0, &ConfigError{
			Field: field,
			Message: fmt.Sprintf("must be at least %d, the number of hops in the longest stage chain (%s)",
				longestHops, strings.Join(longestChain, chainSeparator)),
		}
	}
	return n, nil
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
		len(m.Title) == 0 &&
		m.Priority == nil
}
