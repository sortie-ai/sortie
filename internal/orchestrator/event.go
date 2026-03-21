package orchestrator

import (
	"github.com/sortie-ai/sortie/internal/domain"
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
func HandleAgentEvent(state *State, issueID string, event domain.AgentEvent) {
	entry, ok := state.Running[issueID]
	if !ok {
		return
	}

	// Always record the most-recently-processed event type.
	entry.LastAgentEvent = string(event.Type)

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

	// Populate the session ID from the first EventSessionStarted event
	// that carries a non-empty identifier.
	if event.Type == domain.EventSessionStarted && event.SessionID != "" {
		entry.SessionID = event.SessionID
	}

	// Increment TurnCount on turn-finalization events only. This is
	// adapter-agnostic: every adapter emits exactly one finalization
	// event per completed turn regardless of how many session_started
	// events it emits (per architecture Section 7.3).
	switch event.Type {
	case domain.EventTurnCompleted,
		domain.EventTurnFailed,
		domain.EventTurnCancelled,
		domain.EventTurnEndedWithError,
		domain.EventTurnInputRequired:
		entry.TurnCount++
	}

	// Apply the token delta algorithm when the adapter reports usage.
	// Deltas are clamped to zero as a defensive guard against adapter
	// regressions that emit decreasing counts (architecture Section 13.5).
	if event.Type == domain.EventTokenUsage {
		deltaInput := max(event.Usage.InputTokens-entry.LastReportedInputTokens, 0)
		deltaOutput := max(event.Usage.OutputTokens-entry.LastReportedOutputTokens, 0)
		deltaTotal := max(event.Usage.TotalTokens-entry.LastReportedTotalTokens, 0)

		entry.AgentInputTokens += deltaInput
		entry.AgentOutputTokens += deltaOutput
		entry.AgentTotalTokens += deltaTotal

		entry.LastReportedInputTokens = event.Usage.InputTokens
		entry.LastReportedOutputTokens = event.Usage.OutputTokens
		entry.LastReportedTotalTokens = event.Usage.TotalTokens

		state.AgentTotals.InputTokens += deltaInput
		state.AgentTotals.OutputTokens += deltaOutput
		state.AgentTotals.TotalTokens += deltaTotal
	}

	// Snapshot the rate-limit payload when present. A shallow copy is
	// taken to prevent the adapter from mutating state.AgentRateLimits.Data
	// after delivery (concurrent map write from worker goroutine vs HTTP
	// server read would cause a data race).
	if event.RateLimits != nil {
		state.AgentRateLimits = &RateLimitSnapshot{
			Data:       shallowCopyMap(event.RateLimits),
			ReceivedAt: event.Timestamp,
		}
	}
}

// shallowCopyMap allocates a new map containing all top-level key–value
// pairs from src. The copy prevents the caller from mutating the stored
// snapshot by modifying the original map.
func shallowCopyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
