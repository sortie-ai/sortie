package orchestrator

import (
	"log/slog"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
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
// and layer chosen for a single issue's dispatch. The orchestrator
// records these on [RunningEntry] and propagates them through
// [RetryEntry].
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
// name for an issue against a [config.DispatchConfig]. A rule whose
// stage label the issue carries is selected first, the one lowest in
// the list when the issue carries several. Otherwise the rules without
// a stage label are tried in order. It is pure: no I/O, no logging, no
// time dependence, no goroutine. defaultAgentKind is the workflow-wide
// agent kind (typically cfg.Agent.Kind); defaultTemplateID is the
// body-template sentinel (typically the empty string). The same inputs
// always produce the same output.
func ResolveRule(issue domain.Issue, dispatch config.DispatchConfig, defaultAgentKind, defaultTemplateID string) DispatchResolution {
	if staged := stageCandidates(issue, dispatch.Rules); len(staged) > 0 {
		return ruleResolution(dispatch.Rules[staged[0]], dispatch, defaultAgentKind, defaultTemplateID)
	}

	for _, rule := range dispatch.Rules {
		if rule.Stage != "" {
			continue
		}
		if !rule.IsCatchAll && !matchRule(rule.Match, issue) {
			continue
		}
		return ruleResolution(rule, dispatch, defaultAgentKind, defaultTemplateID)
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

func ruleResolution(rule config.DispatchRule, dispatch config.DispatchConfig, defaultAgentKind, defaultTemplateID string) DispatchResolution {
	return DispatchResolution{
		AgentKind:  coalesce(rule.Selection.AgentKind, dispatch.Default.AgentKind, defaultAgentKind),
		TemplateID: coalesce(rule.Selection.TemplateID, dispatch.Default.TemplateID, defaultTemplateID),
		RuleName:   rule.Name,
		MatchedAt:  ResolvedFromRule,
	}
}

// stageCandidates returns the indices, in list order, of the rules whose
// stage label the issue carries.
func stageCandidates(issue domain.Issue, rules []config.DispatchRule) []int {
	var indices []int
	for i, rule := range rules {
		if rule.Stage == "" {
			continue
		}
		if slices.ContainsFunc(issue.Labels, func(label string) bool { return config.StageLabelsEqual(rule.Stage, label) }) {
			indices = append(indices, i)
		}
	}
	return indices
}

// warnSeveralStageLabels logs one warning naming every stage label the
// issue carries when it carries two or more. ruleName is the rule the
// evaluation selected. [ResolveRule] stays silent so each lane that
// evaluates the rules logs once per evaluation.
func warnSeveralStageLabels(log *slog.Logger, issue domain.Issue, dispatch config.DispatchConfig, ruleName string) {
	staged := stageCandidates(issue, dispatch.Rules)
	if len(staged) < 2 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	labels := make([]string, len(staged))
	for i, index := range staged {
		labels[i] = dispatch.Rules[index].Stage
	}
	logging.WithIssue(log, issue.ID, issue.Identifier).Warn("several stage labels found",
		slog.Any("stage_labels", labels),
		slog.String("rule_name", ruleName))
}

// retrySelection selects the agent kind, template ID, and rule name a
// waiting retry dispatches on under cfg, the configuration in force.
// frozen is the selection the retry entry carries. The frozen selection
// stands, with a retired kind replaced by its replacement, while its
// kind is still reachable and its template is still held; otherwise the
// issue is routed afresh. The second result is true only when the
// issue was routed afresh. templateHeld reports whether the workflow
// holds a template for an ID.
func retrySelection(cfg config.ServiceConfig, templateHeld func(id string) bool, frozen DispatchResolution, issue domain.Issue) (DispatchResolution, bool) {
	target := frozen.AgentKind
	for _, conversion := range cfg.AgentKindConversions() {
		if conversion.Kind == target {
			target = conversion.Replacement
			break
		}
	}

	reachable := slices.ContainsFunc(orderedUniqueAgentKinds(cfg), func(ref agentKindRef) bool { return ref.Kind == target })
	if reachable && templateHeld(frozen.TemplateID) {
		return DispatchResolution{AgentKind: target, TemplateID: frozen.TemplateID, RuleName: frozen.RuleName, MatchedAt: frozen.MatchedAt}, false
	}
	return ResolveRule(issue, cfg.Dispatch, cfg.Agent.Kind, ""), true
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
	if len(m.Title) > 0 {
		if !anyTitlePhraseMatch(m.Title, issue.Title) {
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

// anyTitlePhraseMatch reports whether any phrase occurs in title as
// whole words, ignoring letter case and white-space differences. The
// search is literal: no character of a phrase is a wildcard. An empty
// title never matches.
func anyTitlePhraseMatch(phrases []string, title string) bool {
	t := normalizeTitleText(title)
	if t == "" {
		return false
	}
	for _, phrase := range phrases {
		p := normalizeTitleText(phrase)
		if p == "" {
			continue
		}
		// Every occurrence is tried, overlapping ones included: the
		// first can sit inside a longer word while a later one is whole.
		for from := 0; from < len(t); {
			idx := strings.Index(t[from:], p)
			if idx < 0 {
				break
			}
			i := from + idx
			if isWholeOccurrence(t, p, i) {
				return true
			}
			_, size := utf8.DecodeRuneInString(t[i:])
			from = i + size
		}
	}
	return false
}

// normalizeTitleText folds every case form of a letter to one rune, drops
// the emoji variation selectors, replaces each run of white space with one
// space, and trims the ends. The fold is simple, not full: ß stays distinct
// from ss, and accents and Unicode forms stay as written.
func normalizeTitleText(s string) string {
	var b strings.Builder
	pendingSpace := false
	for _, original := range s {
		r := foldCase(original)
		if isEmojiPresentationSelector(r) {
			continue
		}
		if unicode.IsSpace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// foldCase maps r to the smallest letter in its simple case-folding orbit,
// so σ, ς, and Σ, or s, S, and ſ, compare equal. Lowercasing runs first
// because İ reaches i only through it. Only letters join an orbit, which
// keeps U+0345 a combining mark rather than a spelling of ι.
func foldCase(r rune) rune {
	r = unicode.ToLower(r)
	if !unicode.IsLetter(r) {
		return r
	}
	canon := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < canon && unicode.IsLetter(f) {
			canon = f
		}
	}
	return canon
}

// isEmojiPresentationSelector reports whether r is U+FE0E or U+FE0F.
// They only pick the glyph style of the rune before them and are
// invisible, so a phrase typed without one must match a title that
// carries one.
func isEmojiPresentationSelector(r rune) bool {
	return r == '\uFE0E' || r == '\uFE0F'
}

// isWholeOccurrence reports whether the occurrence of p in t at byte
// offset i neither starts nor ends inside a word.
func isWholeOccurrence(t, p string, i int) bool {
	if i > 0 {
		before, _ := utf8.DecodeLastRuneInString(t[:i])
		first, _ := utf8.DecodeRuneInString(p)
		if runesJoin(before, first) {
			return false
		}
	}
	if end := i + len(p); end < len(t) {
		last, _ := utf8.DecodeLastRuneInString(p)
		after, _ := utf8.DecodeRuneInString(t[end:])
		if runesJoin(last, after) {
			return false
		}
	}
	return true
}

// runesJoin reports whether adjacent runes a and b belong to one word.
// A combining mark always stays with the rune before it, in every
// script, so a match never separates a base character from its vowel,
// tone, or variation sign.
func runesJoin(a, b rune) bool {
	if unicode.IsMark(b) {
		return true
	}
	return isWordRune(a) && isWordRune(b) && !isUnspacedScriptRune(a) && !isUnspacedScriptRune(b)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r)
}

// isUnspacedScriptRune reports whether r is written without spaces
// between words, so every character boundary in these scripts is a
// word boundary. Hangul is excluded because Korean uses spaces.
func isUnspacedScriptRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
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
