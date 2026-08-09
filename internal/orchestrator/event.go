package orchestrator

import (
	"log/slog"
	"maps"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// HandleAgentEvent applies an incoming agent event from the worker's
// OnEvent relay to the running map entry for issueID and, for
// token_usage events, to the global agent totals.
//
// HandleAgentEvent is a no-op when issueID is not present in
// state.Running (the worker may exit before the orchestrator processes
// a queued event).
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

	log := logging.WithIssue(logger, issueID, entry.Identifier)
	if entry.SessionID != "" {
		log = logging.WithSession(log, entry.SessionID)
	}

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

	// Increment TurnCount on session_started — the signal that a new
	// turn has begun. Each adapter emits exactly one session_started
	// per turn.
	if event.Type == domain.EventSessionStarted {
		entry.TurnCount++

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

	// Increment the tool call completion counter for tool_result events
	// with a known tool name. Empty ToolName is a defensive guard —
	// well-behaved adapters always populate it.
	if event.Type == domain.EventToolResult && event.ToolName != "" {
		outcome := outcomeSuccess
		if event.ToolError {
			outcome = outcomeError
		}
		metrics.IncToolCalls(event.ToolName, outcome)

		if event.ToolError {
			errMsg := event.Message
			if errMsg == "" {
				errMsg = "tool returned error"
			}
			log.Info("tool call completed",
				slog.String("tool", event.ToolName),
				slog.Int64("duration_ms", event.ToolDurationMS),
				slog.String("outcome", outcome),
				slog.String("tool_error", errMsg),
			)
		} else {
			log.Info("tool call completed",
				slog.String("tool", event.ToolName),
				slog.Int64("duration_ms", event.ToolDurationMS),
				slog.String("outcome", outcome),
			)
		}
	}

	// Apply the token delta algorithm for any event that reports usage,
	// not only token_usage events, so an adapter can attach the
	// authoritative run-cumulative snapshot to a turn-finalization
	// event without losing it. api_request_count, ModelName, and
	// RequestsByModel stay bound to token_usage events, mirroring the
	// existing treatment of APIDurationMS.
	var usageDelta domain.TokenUsage
	if hasUsage(event.Usage) {
		usageDelta = applyUsageDelta(state, entry, event.Usage, metrics)

		if event.Type == domain.EventTokenUsage {
			// Increment API request count unconditionally — each
			// token_usage event represents one API round-trip.
			entry.APIRequestCount++

			// Track model: prefer the event's model, fall back to last known.
			model := event.Model
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

	// Snapshot the rate-limit payload when present. The worker's
	// OnEvent relay already defensive-copies the map before it crosses
	// the goroutine boundary. The second maps.Clone here is
	// defense-in-depth: it isolates the stored snapshot from any
	// top-level mutation of event.RateLimits within this function or
	// future callers.
	if event.RateLimits != nil {
		// Only replace the snapshot when this event is at least as recent
		// as the stored one, preserving monotonicity under out-of-order
		// delivery.
		if state.AgentRateLimits == nil || !event.Timestamp.Before(state.AgentRateLimits.ReceivedAt) {
			state.AgentRateLimits = &RateLimitSnapshot{
				Data:       maps.Clone(event.RateLimits),
				ReceivedAt: event.Timestamp,
			}
		}
	}

	// Emit a Debug-level summary for observability. Handlers skip
	// formatting at higher log levels; attribute construction here is
	// cheap enough that no additional log.Enabled guard is required.
	switch event.Type {
	case domain.EventSessionStarted:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.String("session_id", event.SessionID),
		)
	case domain.EventTokenUsage:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.Int64("delta_input", usageDelta.InputTokens),
			slog.Int64("delta_output", usageDelta.OutputTokens),
			slog.Int64("delta_total", usageDelta.TotalTokens),
			slog.Int64("delta_cache_read", usageDelta.CacheReadTokens),
		)

	case domain.EventTurnCompleted,
		domain.EventTurnFailed,
		domain.EventTurnCancelled,
		domain.EventTurnEndedWithError,
		domain.EventTurnInputRequired:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.Int("turn_count", entry.TurnCount),
		)
	default:
		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
		)
	}
}

// hasUsage reports whether usage carries a non-zero component. Shared
// by HandleAgentEvent, the worker's OnEvent relay, and
// maybeWriteIncrementalMetadata so all three sites apply the same
// usage-bearing-event condition.
func hasUsage(usage domain.TokenUsage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 || usage.CacheReadTokens != 0
}

// applyUsageDelta applies the monotone-delta token accounting rule to
// entry and state.AgentTotals: it computes each component's delta
// against entry's LastReported* watermarks, clamped to zero, adds the
// deltas to entry and state.AgentTotals, raises the watermarks to the
// componentwise maximum of their prior value and usage, emits the
// non-zero deltas through metrics, and returns the applied delta.
//
// applyUsageDelta is the only function that mutates
// entry.AgentInputTokens, entry.AgentOutputTokens,
// entry.AgentTotalTokens, entry.CacheReadTokens, the
// entry.LastReported* watermarks, and State.AgentTotals, and the only
// caller of metrics.AddTokens for token counters. It runs only on the
// orchestrator's single-writer event loop goroutine; the worker's own
// local mirror of these counters applies the identical rule
// independently on the worker goroutine and never calls this function.
func applyUsageDelta(state *State, entry *RunningEntry, usage domain.TokenUsage, metrics domain.Metrics) domain.TokenUsage {
	deltaInput := max(usage.InputTokens-entry.LastReportedInputTokens, 0)
	deltaOutput := max(usage.OutputTokens-entry.LastReportedOutputTokens, 0)
	deltaTotal := max(usage.TotalTokens-entry.LastReportedTotalTokens, 0)
	deltaCacheRead := max(usage.CacheReadTokens-entry.LastReportedCacheReadTokens, 0)

	entry.AgentInputTokens += deltaInput
	entry.AgentOutputTokens += deltaOutput
	entry.AgentTotalTokens += deltaTotal
	entry.CacheReadTokens += deltaCacheRead

	entry.LastReportedInputTokens = max(entry.LastReportedInputTokens, usage.InputTokens)
	entry.LastReportedOutputTokens = max(entry.LastReportedOutputTokens, usage.OutputTokens)
	entry.LastReportedTotalTokens = max(entry.LastReportedTotalTokens, usage.TotalTokens)
	entry.LastReportedCacheReadTokens = max(entry.LastReportedCacheReadTokens, usage.CacheReadTokens)

	state.AgentTotals.InputTokens += deltaInput
	state.AgentTotals.OutputTokens += deltaOutput
	state.AgentTotals.TotalTokens += deltaTotal
	state.AgentTotals.CacheReadTokens += deltaCacheRead

	if deltaInput > 0 {
		metrics.AddTokens("input", deltaInput)
	}
	if deltaOutput > 0 {
		metrics.AddTokens("output", deltaOutput)
	}
	if deltaCacheRead > 0 {
		metrics.AddTokens("cache_read", deltaCacheRead)
	}

	return domain.TokenUsage{
		InputTokens:     deltaInput,
		OutputTokens:    deltaOutput,
		TotalTokens:     deltaTotal,
		CacheReadTokens: deltaCacheRead,
	}
}
