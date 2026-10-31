package orchestrator

import (
	"cmp"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

// isTurnTerminalEvent reports whether t is one of the five event
// types that end a turn. Shared by HandleAgentEvent's Debug-log
// routing and its per-turn API-request baseline reset, so the set is
// asserted in exactly one place.
func isTurnTerminalEvent(t domain.AgentEventType) bool {
	switch t {
	case domain.EventTurnCompleted,
		domain.EventTurnFailed,
		domain.EventTurnCancelled,
		domain.EventTurnEndedWithError,
		domain.EventTurnInputRequired:
		return true
	default:
		return false
	}
}

// HandleAgentEvent applies an incoming agent event from the worker's
// OnEvent relay to the running map entry for issueID and, for
// token_usage events, to the global agent totals. It also retains the
// event in the entry's bounded buffer, except those that carry nothing
// to show.
//
// HandleAgentEvent is a no-op when issueID is not present in
// state.Running.
//
// Must be called from the orchestrator's single-writer event loop
// goroutine. Not safe for concurrent use.
func HandleAgentEvent(state *State, issueID string, event domain.AgentEvent, logger *slog.Logger, metrics domain.Metrics) {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	entry, ok := state.Running[issueID]
	if !ok {
		logger.Debug("agent event for unknown issue",
			slog.String("issue_id", issueID),
			slog.Any("event_type", event.Type),
		)
		return
	}

	if event.Type == domain.EventToolResult {
		switch event.ToolKind {
		case domain.ToolKindShell:
			event.ToolArgument = unwrapShell(event.ToolArgument)
		case domain.ToolKindRead, domain.ToolKindEdit:
			event.ToolArgument = workspaceRelative(event.ToolArgument, entry.WorkspacePath)
		case domain.ToolKindSkill:
		default:
			event.ToolKind = ""
			event.ToolArgument = ""
		}
		event.ToolName = toolText(event.ToolName)
		event.ToolArgument = toolText(event.ToolArgument)
	}

	log := logging.WithIssue(logger, issueID, entry.Identifier)
	if entry.SessionID != "" {
		log = logging.WithSession(log, entry.SessionID)
	}

	event.Message = redact.Mask(event.Message)

	// Always record the most-recently-processed event type.
	entry.LastAgentEvent = event.Type

	// Advance the timestamp only when the incoming event is strictly
	// later, preserving monotonicity under out-of-order delivery.
	// The zero value of time.Time is before all real UTC timestamps,
	// so the first event always sets the field.
	if event.Timestamp.After(entry.LastAgentTimestamp) {
		entry.LastAgentTimestamp = event.Timestamp
	}

	if event.Message != "" {
		entry.LastAgentMessage = event.Message
	}
	if event.AgentPID != "" {
		entry.AgentPID = event.AgentPID
	}

	if event.Type == domain.EventSessionStarted {
		// Overwrite the session ID when the event carries a non-empty
		// session ID. Claude Code spawns a fresh subprocess per turn,
		// so the active session ID changes.
		if event.SessionID != "" {
			entry.SessionID = event.SessionID
		}
	}

	// Accumulate API timing from any event that carries it. This is
	// event-type-independent so adapters can set APIDurationMS on
	// turn-finalization events without polluting APIRequestCount.
	if event.APIDurationMS > 0 {
		entry.APITimeMs += event.APIDurationMS
	}

	// Accumulate tool execution time from tool_result events.
	if event.Type == domain.EventToolResult && event.ToolDurationMS > 0 {
		entry.ToolTimeMs += event.ToolDurationMS
	}

	// Skill calls are kept from every event on arrival because the retained
	// buffer evicts early events of a long run.
	if event.Type == domain.EventToolResult && event.ToolKind == domain.ToolKindSkill && event.ToolArgument != "" {
		entry.Skills = append(entry.Skills, domain.SkillCall{Skill: event.ToolArgument, ToolError: event.ToolError})
	}

	if event.Type == domain.EventToolResult {
		tool := toolLabel(event.ToolName, event.ToolKind)
		outcome := outcomeSuccess
		if event.ToolError {
			outcome = outcomeError
		}
		metrics.IncToolCalls(tool, outcome)

		// Agents call tools constantly and a failed call is routine, so these
		// records stay at Debug; the error keeps only its first line because
		// a tool's output can run to thousands of lines of workspace content.
		if event.ToolError {
			errMsg, _, _ := strings.Cut(event.Message, "\n")
			if strings.TrimSpace(errMsg) == "" {
				errMsg = "tool returned error"
			}
			log.Debug("tool call completed",
				slog.String("tool", tool),
				slog.Int64("duration_ms", event.ToolDurationMS),
				slog.String("outcome", outcome),
				slog.String("tool_error", redact.Truncate(errMsg, 200)),
			)
		} else {
			log.Debug("tool call completed",
				slog.String("tool", tool),
				slog.Int64("duration_ms", event.ToolDurationMS),
				slog.String("outcome", outcome),
			)
		}
	}

	// Amounts are not token figures, so the usage arrival does not gate
	// them. The in-flight ceilings read the entry after this call returns.
	if event.ReportedAmounts != nil {
		entry.ReportedAmounts = usage.MergeAmounts(entry.ReportedAmounts, event.ReportedAmounts)
	}

	// Apply the token delta algorithm for any event that reports usage,
	// not only token_usage events, so an adapter can attach the
	// authoritative run-cumulative snapshot to a turn-finalization
	// event without losing it. api_request_count, ModelName, and
	// RequestsByModel stay bound to token_usage events, mirroring the
	// existing treatment of APIDurationMS.
	var usageDelta domain.TokenUsage

	// Prepared outside the usage gate: a kind whose figures the gate
	// refuses still records runtime and exit samples under this model.
	carrier := tokenUsageModel(event)
	if carrier != "" && carrier != entry.ModelName {
		metrics.PrepareModel(carrier)
	}
	if admitsUsageFigures(entry.UsageArrival) {
		if hasUsage(event.Usage) {
			usageDelta = applyUsageDelta(state, entry, event.Usage, event.UsageByModel, carrier, metrics)
		}

		// A token_usage event, even one carrying an all-zero payload, or
		// any event carrying a non-zero usage component asserts that the
		// agent's runtime reported a measurement. Monotone from false:
		// set once and never cleared.
		if event.Type == domain.EventTokenUsage || hasUsage(event.Usage) {
			entry.UsageMeasured = true
		}

		if event.Type == domain.EventTokenUsage {
			// The counter counts token_usage events, including one whose
			// reported usage is entirely zero. That is a round-trip count
			// only for a session the measurement verdict admits, and the
			// increment stays unconditional so the verdict, not the
			// counter, carries the distinction.
			entry.APIRequestCount++

			// A turn_end kind settles at most one figure per turn; more
			// than one within the current turn contradicts that
			// declaration.
			if entry.UsageArrival == registry.UsageArrivalTurnEnd &&
				entry.APIRequestCount-entry.APIRequestCountAtLastTurnEnd > 1 {
				log.Debug("turn_end arrival reported more than one usage figure within a turn",
					slog.Int("excess_count", entry.APIRequestCount-entry.APIRequestCountAtLastTurnEnd-1),
				)
			}

			// Track model: prefer the event's model, fall back to last known.
			model := tokenUsageModel(event)
			if model != "" {
				entry.ModelName = model
			} else {
				model = entry.ModelName
			}
			if model != "" {
				if entry.RequestsByModel == nil {
					entry.RequestsByModel = make(map[string]int)
				}
				entry.RequestsByModel[model]++
			}
		}
	}

	if len(event.UsageWindows) > 0 {
		if state.UsageWindows == nil {
			state.UsageWindows = make(map[string][]domain.UsageWindow)
		}
		state.UsageWindows[entry.AgentKind] = usage.MergeWindows(state.UsageWindows[entry.AgentKind], event.UsageWindows)
	}

	// Emit a Debug-level summary for observability. Handlers skip
	// formatting at higher log levels; attribute construction here is
	// cheap enough that no additional log.Enabled guard is required.
	switch {
	case event.Type == domain.EventSessionStarted:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.String("session_id", event.SessionID),
		)
	case event.Type == domain.EventTokenUsage:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.Int64("delta_input", usageDelta.InputTokens),
			slog.Int64("delta_output", usageDelta.OutputTokens),
			slog.Int64("delta_total", usageDelta.TotalTokens),
			slog.Int64("delta_cache_read", usageDelta.CacheReadTokens),
			slog.Int64("delta_cache_write", usageDelta.CacheWriteTokens),
		)

	case isTurnTerminalEvent(event.Type):
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.Int("turn_count", entry.TurnCount),
		)
	default:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
		)
	}

	// Reset the per-turn API-request baseline on every turn-terminal
	// event, so the turn_end drift check above compares against the
	// current turn only, never against a session-wide total.
	if isTurnTerminalEvent(event.Type) {
		entry.APIRequestCountAtLastTurnEnd = entry.APIRequestCount
	}

	retainEvent(entry, event)
}

// toolText returns s as text safe to store and show: control characters
// become spaces, format characters (bidirectional overrides, zero-width
// marks) are dropped, and the result is masked and bounded as a message is.
func toolText(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cc, r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return redact.Truncate(strings.TrimSpace(cleaned), retainedMessageRunes)
}

// unwrapShell returns the script of a "bash -lc", "sh -c", or "zsh -c"
// invocation without its wrapper and outer quotes, and cmd unchanged for any
// other command line.
func unwrapShell(cmd string) string {
	shell, rest := cutWord(cmd)
	switch shell[strings.LastIndexByte(shell, '/')+1:] {
	case "bash", "sh", "zsh":
	default:
		return cmd
	}
	flag, script := cutWord(rest)
	script = strings.TrimSpace(script)
	if (flag != "-c" && flag != "-lc") || script == "" {
		return cmd
	}
	if len(script) >= 2 && (script[0] == '\'' || script[0] == '"') && script[len(script)-1] == script[0] {
		return script[1 : len(script)-1]
	}
	return script
}

// cutWord splits s at its first white-space-separated word.
func cutWord(s string) (word, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// workspaceRelative removes the workspace root, with the separator that
// follows it, from the start of each ", "-joined path in s, so a path inside
// the run's workspace reads relative to it and names no directory of the host.
func workspaceRelative(s, workspace string) string {
	root := strings.TrimRight(workspace, `/\`)
	if root == "" {
		return s
	}
	paths := strings.Split(s, ", ")
	for i, p := range paths {
		if rest, ok := strings.CutPrefix(p, root+"/"); ok {
			paths[i] = rest
		} else if rest, ok := strings.CutPrefix(p, root+`\`); ok {
			paths[i] = rest
		}
	}
	return strings.Join(paths, ", ")
}

// toolLabel returns the metric and log label of a tool call: the tool's name
// when it is plain enough to keep the label set bounded, else its kind, else
// "other".
func toolLabel(name string, kind domain.ToolKind) string {
	if name == "" {
		return cmp.Or(string(kind), "other")
	}
	for _, r := range name {
		if !isLabelRune(r) {
			return "other"
		}
	}
	return name
}

func isLabelRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '_' || r == '-' || r == '.' || r == ':'
}

// hasUsage reports whether usage carries a non-zero component. Every
// site that asks whether an event bears usage calls it, so they all
// apply the same condition.
func hasUsage(usage domain.TokenUsage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 ||
		usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0
}

// admitsUsageFigures reports whether a session with the given arrival
// may record usage. A figure reported under arrival none contradicts
// the declaration and is discarded rather than trusted.
func admitsUsageFigures(arrival registry.UsageArrival) bool {
	return arrival != registry.UsageArrivalNone
}

// tokenUsageModel returns the model name a token_usage event carries,
// verbatim, or "" for an event of any other type.
func tokenUsageModel(event domain.AgentEvent) string {
	if event.Type != domain.EventTokenUsage {
		return ""
	}
	return event.Model
}

// metricModel returns the model value for a metric sample: the first
// non-empty of the carrier's model, the run's reported model, and its
// configured model, or "" when none is set. It writes nothing, so an
// empty carrier model never displaces a known reported model.
func metricModel(carrierModel string, entry *RunningEntry) string {
	return coalesce(carrierModel, entry.ModelName, entry.ConfiguredModel)
}

// usageDeltaAndWatermark computes usage's componentwise delta against
// watermark, clamped to zero, and the componentwise-maximum watermark
// raised by usage, over every domain.TokenUsage field. foldUsage derives
// its delta and raised watermark from this one rule.
func usageDeltaAndWatermark(usage, watermark domain.TokenUsage) (delta, raisedWatermark domain.TokenUsage) {
	delta = domain.TokenUsage{
		InputTokens:      max(usage.InputTokens-watermark.InputTokens, 0),
		OutputTokens:     max(usage.OutputTokens-watermark.OutputTokens, 0),
		TotalTokens:      max(usage.TotalTokens-watermark.TotalTokens, 0),
		CacheReadTokens:  max(usage.CacheReadTokens-watermark.CacheReadTokens, 0),
		CacheWriteTokens: max(usage.CacheWriteTokens-watermark.CacheWriteTokens, 0),
	}
	raisedWatermark = domain.TokenUsage{
		InputTokens:      max(watermark.InputTokens, usage.InputTokens),
		OutputTokens:     max(watermark.OutputTokens, usage.OutputTokens),
		TotalTokens:      max(watermark.TotalTokens, usage.TotalTokens),
		CacheReadTokens:  max(watermark.CacheReadTokens, usage.CacheReadTokens),
		CacheWriteTokens: max(watermark.CacheWriteTokens, usage.CacheWriteTokens),
	}
	return delta, raisedWatermark
}

// foldUsage applies the clamped-delta rule to usage and splits the delta
// into per-model credits that sum to it: each model of byModel gets its
// claim against totals, capped by what the delta has left, and current
// gets the rest.
func foldUsage(usage domain.TokenUsage, byModel map[string]domain.TokenUsage, current string, watermark domain.TokenUsage, totals map[string]domain.TokenUsage) (delta, raisedWatermark domain.TokenUsage, credits map[string]domain.TokenUsage) {
	delta, raisedWatermark = usageDeltaAndWatermark(usage, watermark)
	budget := delta
	credits = make(map[string]domain.TokenUsage)

	normalized := agentcore.NormalizeUsageByModel(byModel)
	for _, name := range slices.Sorted(maps.Keys(normalized)) {
		claim := agentcore.SubtractUsage(normalized[name], totals[name])
		take := minUsage(claim, budget)
		if !hasTokenClasses(take) {
			continue
		}
		credits[name] = take
		budget = agentcore.SubtractUsage(budget, take)
	}
	if hasTokenClasses(budget) {
		credits[current] = agentcore.AddUsage(credits[current], budget)
	}
	return delta, raisedWatermark, credits
}

// hasTokenClasses reports whether usage is non-zero in an input, output,
// cache read, or cache write count. TotalTokens is not a class.
func hasTokenClasses(usage domain.TokenUsage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 ||
		usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0
}

// minUsage returns the componentwise minimum of a and b over the token
// classes.
func minUsage(a, b domain.TokenUsage) domain.TokenUsage {
	least := domain.TokenUsage{
		InputTokens:      min(a.InputTokens, b.InputTokens),
		OutputTokens:     min(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:  min(a.CacheReadTokens, b.CacheReadTokens),
		CacheWriteTokens: min(a.CacheWriteTokens, b.CacheWriteTokens),
	}
	least.TotalTokens = least.InputTokens + least.OutputTokens
	return least
}

// applyUsageDelta applies the monotone-delta token accounting rule to
// entry and state.AgentTotals: it computes each component's delta
// against entry's LastReported* watermarks, clamped to zero, adds the
// deltas to entry and state.AgentTotals, raises the watermarks to the
// componentwise maximum of their prior value and usage, and returns the
// applied delta. byModel is usage's per-model split, nil when the record
// names none; carrierModel is the model named by the record that brought
// usage, or "" when it names none. Each model's share of the delta
// reaches metrics.AddTokens exactly once under that model, and the
// unattributed rest under metricModel(carrierModel, entry).
//
// applyUsageDelta is the only function that mutates
// entry.AgentInputTokens, entry.AgentOutputTokens,
// entry.AgentTotalTokens, entry.CacheReadTokens, entry.UsageByModel,
// entry.CacheWriteTokens, the entry.LastReported* watermarks, and
// State.AgentTotals, and the only caller of metrics.AddTokens for
// token counters. It runs only on the orchestrator's single-writer
// event loop goroutine; the worker's own local mirror of these
// counters applies the identical rule independently on the worker
// goroutine and never calls this function.
func applyUsageDelta(state *State, entry *RunningEntry, usage domain.TokenUsage, byModel map[string]domain.TokenUsage, carrierModel string, metrics domain.Metrics) domain.TokenUsage {
	watermark := domain.TokenUsage{
		InputTokens:      entry.LastReportedInputTokens,
		OutputTokens:     entry.LastReportedOutputTokens,
		TotalTokens:      entry.LastReportedTotalTokens,
		CacheReadTokens:  entry.LastReportedCacheReadTokens,
		CacheWriteTokens: entry.LastReportedCacheWriteTokens,
	}
	delta, raisedWatermark, credits := foldUsage(usage, byModel, metricModel(carrierModel, entry), watermark, entry.UsageByModel)

	entry.AgentInputTokens += delta.InputTokens
	entry.AgentOutputTokens += delta.OutputTokens
	entry.AgentTotalTokens += delta.TotalTokens
	entry.CacheReadTokens += delta.CacheReadTokens
	entry.CacheWriteTokens += delta.CacheWriteTokens

	entry.LastReportedInputTokens = raisedWatermark.InputTokens
	entry.LastReportedOutputTokens = raisedWatermark.OutputTokens
	entry.LastReportedTotalTokens = raisedWatermark.TotalTokens
	entry.LastReportedCacheReadTokens = raisedWatermark.CacheReadTokens
	entry.LastReportedCacheWriteTokens = raisedWatermark.CacheWriteTokens

	state.AgentTotals.InputTokens += delta.InputTokens
	state.AgentTotals.OutputTokens += delta.OutputTokens
	state.AgentTotals.TotalTokens += delta.TotalTokens
	state.AgentTotals.CacheReadTokens += delta.CacheReadTokens
	state.AgentTotals.CacheWriteTokens += delta.CacheWriteTokens

	for _, model := range slices.Sorted(maps.Keys(credits)) {
		credit := credits[model]
		if _, seen := entry.UsageByModel[model]; !seen {
			metrics.PrepareModel(model)
		}
		if entry.UsageByModel == nil {
			entry.UsageByModel = make(map[string]domain.TokenUsage)
		}
		entry.UsageByModel[model] = agentcore.AddUsage(entry.UsageByModel[model], credit)
		if credit.InputTokens > 0 {
			metrics.AddTokens("input", model, credit.InputTokens)
		}
		if credit.OutputTokens > 0 {
			metrics.AddTokens("output", model, credit.OutputTokens)
		}
		if credit.CacheReadTokens > 0 {
			metrics.AddTokens("cache_read", model, credit.CacheReadTokens)
		}
		if credit.CacheWriteTokens > 0 {
			metrics.AddTokens("cache_write", model, credit.CacheWriteTokens)
		}
	}

	return delta
}
