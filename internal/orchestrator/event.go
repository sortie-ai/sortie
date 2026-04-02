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

	// Apply the token delta algorithm when the adapter reports usage.
	// Deltas are clamped to zero as a defensive guard against adapter
	// regressions that emit decreasing cumulative counts.
	if event.Type == domain.EventTokenUsage {
		deltaInput := max(event.Usage.InputTokens-entry.LastReportedInputTokens, 0)
		deltaOutput := max(event.Usage.OutputTokens-entry.LastReportedOutputTokens, 0)
		deltaTotal := max(event.Usage.TotalTokens-entry.LastReportedTotalTokens, 0)

		entry.AgentInputTokens += deltaInput
		entry.AgentOutputTokens += deltaOutput
		entry.AgentTotalTokens += deltaTotal

		entry.LastReportedInputTokens = max(entry.LastReportedInputTokens, event.Usage.InputTokens)
		entry.LastReportedOutputTokens = max(entry.LastReportedOutputTokens, event.Usage.OutputTokens)
		entry.LastReportedTotalTokens = max(entry.LastReportedTotalTokens, event.Usage.TotalTokens)

		deltaCacheRead := max(event.Usage.CacheReadTokens-entry.LastReportedCacheReadTokens, 0)
		entry.CacheReadTokens += deltaCacheRead
		entry.LastReportedCacheReadTokens = max(entry.LastReportedCacheReadTokens, event.Usage.CacheReadTokens)

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

		log.Debug("agent event processed",
			slog.Any("event_type", event.Type),
			slog.Int64("delta_input", deltaInput),
			slog.Int64("delta_output", deltaOutput),
			slog.Int64("delta_total", deltaTotal),
			slog.Int64("delta_cache_read", deltaCacheRead),
		)
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
		// Logged inside the delta computation block above.

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
