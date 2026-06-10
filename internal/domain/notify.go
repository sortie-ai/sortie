package domain

import "context"

// Notifier sends a normalized [Notification] to a single backend.
// One method keeps every backend interchangeable and lets any producer
// reuse the family.
type Notifier interface {
	// Send delivers the notification. It returns nil on a successful
	// send and a classified error on transport failure, a non-2xx
	// response, or an unparseable response. Implementations apply a
	// per-call timeout and must not log the endpoint URL, the request
	// body, or the response body.
	Send(ctx context.Context, n Notification) error
}

// Notification is the normalized payload every notifier backend
// consumes. The envelope is filled by the producer; the message is
// supplied by the agent. The value is self-contained: every field a
// backend needs rides in it, with no dependency on producer-only state.
type Notification struct {
	Envelope NotificationEnvelope
	Message  NotificationMessage
}

// NotificationEnvelope carries system-owned session context. The agent
// neither provides it nor can override it.
type NotificationEnvelope struct {
	// NotificationID is a generated unique id, such as a UUID.
	NotificationID string

	// Timestamp is the send time in ISO-8601 UTC, such as
	// 2026-06-10T14:03:05Z.
	Timestamp string

	// Source identifies the Sortie instance and defaults to the
	// hostname.
	Source string

	// IssueID is the tracker-internal issue id.
	IssueID string

	// Identifier is the human-readable issue key.
	Identifier string

	// SessionID is the agent session id. It may be empty early in a
	// lifecycle.
	SessionID string

	// Attempt is the retry or continuation attempt. It is nil on the
	// first run.
	Attempt *int

	// Agent is the dispatch-frozen agent kind, such as "claude-code".
	// It may be empty when no agent kind is resolved.
	Agent string
}

// NotificationMessage carries the agent-supplied content. Severity is
// constrained to info, warning, or critical, and Category to
// decision_needed, progress, blocked, completed, or other. The
// constraints are documented here but enforced by the producing tool,
// not by this type.
type NotificationMessage struct {
	// Severity is one of info, warning, or critical.
	Severity string

	// Title is a non-empty short summary.
	Title string

	// Body is the non-empty notification detail.
	Body string

	// Category is optional and, when set, is one of decision_needed,
	// progress, blocked, completed, or other.
	Category string
}
