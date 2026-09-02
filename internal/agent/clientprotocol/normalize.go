package clientprotocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// reasoningBlockMessage and planUpdateMessage are the compile-time
// constant messages the normalization table assigns to a thought chunk
// and a plan update; neither carries agent-produced text.
const (
	reasoningBlockMessage = "reasoning block"
	planUpdateMessage     = "plan update"
)

// messageTruncateLimit bounds every agent-produced message this adapter
// emits, matching the shared per-adapter convention.
const messageTruncateLimit = 500

// normalizedUpdate is what applySessionUpdate returns for one recognized
// session/update variant: the event to publish, if any, and whether the
// variant is evidence of the model producing something during the turn.
type normalizedUpdate struct {
	event       domain.AgentEvent
	hasEvent    bool
	workPresent bool
}

// applySessionUpdate normalizes one decoded session/update variant,
// mutating tracker for the two variants that begin or end a tool call.
// tracker is the session's own tool tracker; the pump is its only
// caller, so no synchronization is needed here.
func applySessionUpdate(tracker *agentcore.ToolTracker, ev sessionUpdateEvent) normalizedUpdate {
	now := time.Now().UTC()

	switch ev.kind {
	case updateAgentMessageChunk:
		text := chunkText(ev.chunk)
		return normalizedUpdate{
			event: domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   typeutil.TruncateRunes(text, messageTruncateLimit),
			},
			hasEvent:    true,
			workPresent: true,
		}

	case updateAgentThoughtChunk:
		return normalizedUpdate{
			event:       domain.AgentEvent{Type: domain.EventOtherMessage, Timestamp: now, Message: reasoningBlockMessage},
			hasEvent:    true,
			workPresent: true,
		}

	case updateUserMessageChunk:
		// No event on the pinned line: this variant carries replay
		// evidence for session continuation only, which this piece does
		// not implement.
		return normalizedUpdate{}

	case updateToolCall:
		tracker.Begin(string(ev.toolCallBegin.ToolCallID), normalizeToolKind(ev.toolCallBegin.Kind))
		return normalizedUpdate{workPresent: true}

	case updateToolCallUpdate:
		if ev.toolCallUpdate.Status == nil {
			return normalizedUpdate{}
		}
		switch *ev.toolCallUpdate.Status {
		case toolCallStatusCompleted, toolCallStatusFailed:
			name, durationMS, ok := tracker.End(string(ev.toolCallUpdate.ToolCallID))
			if !ok {
				return normalizedUpdate{}
			}
			var title string
			if ev.toolCallUpdate.Title != nil {
				title = *ev.toolCallUpdate.Title
			}
			return normalizedUpdate{
				event: domain.AgentEvent{
					Type:           domain.EventToolResult,
					Timestamp:      now,
					ToolName:       name,
					ToolDurationMS: durationMS,
					ToolError:      *ev.toolCallUpdate.Status == toolCallStatusFailed,
					Message:        typeutil.TruncateRunes(title, messageTruncateLimit),
				},
				hasEvent: true,
			}
		default:
			// Any other status: recorded for the tracker only, per the
			// normalization table; there is nothing further to record
			// here because the tracker's own state already reflects it
			// by construction (begin/end are the only transitions this
			// adapter reads).
			return normalizedUpdate{}
		}

	case updatePlan:
		return normalizedUpdate{
			event:    domain.AgentEvent{Type: domain.EventOtherMessage, Timestamp: now, Message: planUpdateMessage},
			hasEvent: true,
		}

	case updateAvailableCommands, updateCurrentMode, updateConfigOption, updateSessionInfo, updateUsage:
		// Debug log only, per the normalization table; the caller does
		// not emit an event for these.
		return normalizedUpdate{}

	default:
		return normalizedUpdate{}
	}
}

// chunkText extracts the text of a content chunk's content block. It
// returns "" for anything other than the text content block this
// transport reads and writes; the adapter never sends or expects image,
// audio, or resource content.
func chunkText(chunk contentChunk) string {
	if chunk.Content.Type != contentBlockText {
		return ""
	}
	var text textContent
	if err := json.Unmarshal(chunk.Content.Remainder, &text); err != nil {
		return ""
	}
	return text.Text
}

// normalizeToolKind maps a tool call's declared kind to the ten-value
// closed set the orchestrator's tool-call metric uses as a label,
// substituting "other" for an absent or unrecognized value.
func normalizeToolKind(kind *toolKind) string {
	if kind == nil {
		return string(toolKindOther)
	}
	switch *kind {
	case toolKindRead, toolKindEdit, toolKindDelete, toolKindMove, toolKindSearch,
		toolKindExecute, toolKindThink, toolKindFetch, toolKindSwitchMode, toolKindOther:
		return string(*kind)
	default:
		return string(toolKindOther)
	}
}

// stopReasonEvidence maps promptResponse.StopReason to the shared turn
// evidence, per the pinned line's five stop reasons plus the two
// conditions that precede this call (a JSON-RPC error response and a
// lost connection, both handled by their own callers). The two
// cancelled-stop-reason rows that depend on who cancelled are resolved
// by the caller through activeTurn.pendingEnd, which overrides whatever
// this function returns; the case handled here is the third row: nobody
// cancelled, which is a nonconformant answer reported as failed.
func stopReasonEvidence(reason stopReason, work agentcore.WorkReport) agentcore.TurnEvidence {
	switch reason {
	case stopReasonEndTurn:
		return agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess, Work: work}
	case stopReasonRefusal:
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnRefused,
			Work:              work,
		}
	case stopReasonMaxTokens, stopReasonMaxTurnRequests:
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnFailed,
			Work:              work,
		}
	case stopReasonCancelled:
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnFailed,
			TerminalMessage:   cancelledWithNoCancellationMessage,
			Work:              work,
		}
	default:
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
			TerminalMessage: typeutil.TruncateRunes(
				fmt.Sprintf("agent reported an unrecognized stop reason: %q", string(reason)),
				messageTruncateLimit,
			),
			Work: work,
		}
	}
}

// workReportFrom converts the turn-scoped observation flag into the
// shared package's closed WorkReport set.
func workReportFrom(observed bool) agentcore.WorkReport {
	if observed {
		return agentcore.WorkPresent
	}
	return agentcore.WorkAbsent
}

// cancelledWithNoCancellationMessage is the compile-time constant
// message for a "cancelled" stop reason that neither the orchestrator
// nor this adapter's own end-attempt sequence induced.
const cancelledWithNoCancellationMessage = "agent reported a cancelled stop reason without a cancellation on either side"
