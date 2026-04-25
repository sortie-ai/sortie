package agentcore

import (
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// EmitSessionStarted emits an EventSessionStarted event with the given agent
// process ID and session identifier. agentPID is set directly on AgentPID;
// pass "" when the process ID is unavailable. Subprocess adapters (Claude,
// Copilot) pass strconv.Itoa(cmd.Process.Pid) after cmd.Start; persistent-
// process adapters (Codex) pass session.AgentPID directly.
func EmitSessionStarted(emit func(domain.AgentEvent), agentPID string, sessionID string) {
	emit(domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: time.Now().UTC(),
		AgentPID:  agentPID,
		SessionID: sessionID,
		Message:   "session started",
	})
}

// EmitTurnCompleted emits an EventTurnCompleted event. message is a
// human-readable summary; use "" when unavailable. apiDurationMS is the LLM
// API response wait time in milliseconds for this turn; use 0 when
// unavailable.
func EmitTurnCompleted(emit func(domain.AgentEvent), message string, apiDurationMS int64) {
	emit(domain.AgentEvent{
		Type:          domain.EventTurnCompleted,
		Timestamp:     time.Now().UTC(),
		Message:       message,
		APIDurationMS: apiDurationMS,
	})
}

// EmitTurnFailed emits an EventTurnFailed event. message describes the
// failure. apiDurationMS is attached to the event; use 0 when unavailable.
func EmitTurnFailed(emit func(domain.AgentEvent), message string, apiDurationMS int64) {
	emit(domain.AgentEvent{
		Type:          domain.EventTurnFailed,
		Timestamp:     time.Now().UTC(),
		Message:       message,
		APIDurationMS: apiDurationMS,
	})
}

// EmitTurnCancelled emits an EventTurnCancelled event with the given
// human-readable message.
func EmitTurnCancelled(emit func(domain.AgentEvent), message string) {
	emit(domain.AgentEvent{
		Type:      domain.EventTurnCancelled,
		Timestamp: time.Now().UTC(),
		Message:   message,
	})
}

// EmitMalformed emits an EventMalformed event. line is the raw unparseable
// bytes from the agent output stream; it is truncated to 500 Unicode code
// points before inclusion in the event message.
func EmitMalformed(emit func(domain.AgentEvent), line []byte) {
	emit(domain.AgentEvent{
		Type:      domain.EventMalformed,
		Timestamp: time.Now().UTC(),
		Message:   typeutil.TruncateRunes(string(line), 500),
	})
}

// EmitNotification emits an EventNotification event with the given
// informational message. message may be empty for stall-timer-reset
// notifications.
func EmitNotification(emit func(domain.AgentEvent), message string) {
	emit(domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: time.Now().UTC(),
		Message:   message,
	})
}
