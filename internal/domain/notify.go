package domain

import (
	"context"
	"slices"
)

// Notifier sends a normalized [Notification] to a single backend.
type Notifier interface {
	// Send delivers the notification. It returns nil on a successful
	// send and a classified error on transport failure, a non-2xx
	// response, or an unparseable response. Implementations apply a
	// per-call timeout and must not log the endpoint URL, the request
	// body, or the response body.
	Send(ctx context.Context, n Notification) error
}

// Notification is the normalized payload every notifier backend consumes.
type Notification struct {
	Envelope NotificationEnvelope
	Message  NotificationMessage
}

// EventType names one outbound event. The catalog is closed.
type EventType string

const (
	// EventTypeSessionStarted carries the Type prefix because
	// [EventSessionStarted] already names an agent event.
	EventTypeSessionStarted        EventType = "session.started"
	EventSessionCompleted          EventType = "session.completed"
	EventSessionStopped            EventType = "session.stopped"
	EventSessionFailed             EventType = "session.failed"
	EventEscalationCIFailure       EventType = "escalation.ci_failure"
	EventEscalationReviewComments  EventType = "escalation.review_comments"
	EventEscalationBotReview       EventType = "escalation.bot_review"
	EventEscalationMergeConflicts  EventType = "escalation.merge_conflicts"
	EventEscalationAutoMerge       EventType = "escalation.auto_merge"
	EventEscalationMergeCompletion EventType = "escalation.merge_completion"
	EventAutoMergeMerged           EventType = "auto_merge.merged"
	EventBudgetHeld                EventType = "budget.held"
	EventStageAdvanced             EventType = "stage.advanced"
	EventStageNotAdvanced          EventType = "stage.not_advanced"
	EventAgentMessage              EventType = "agent.message"
)

// TrackerCommentKind is the reserved notifications kind of the built-in
// tracker comment destination. No notifier registers under it.
const TrackerCommentKind = "tracker_comment"

var eventTypes = []EventType{
	EventTypeSessionStarted,
	EventSessionCompleted,
	EventSessionStopped,
	EventSessionFailed,
	EventEscalationCIFailure,
	EventEscalationReviewComments,
	EventEscalationBotReview,
	EventEscalationMergeConflicts,
	EventEscalationAutoMerge,
	EventEscalationMergeCompletion,
	EventAutoMergeMerged,
	EventBudgetHeld,
	EventStageAdvanced,
	EventStageNotAdvanced,
	EventAgentMessage,
}

// EventTypes returns the catalog in declaration order. The caller may
// modify the returned slice.
func EventTypes() []EventType {
	return slices.Clone(eventTypes)
}

// Valid reports whether t is a member of the catalog.
func (t EventType) Valid() bool {
	return slices.Contains(eventTypes, t)
}

// FromOrchestrator reports whether the orchestrator produces t: every
// valid type except [EventAgentMessage].
func (t EventType) FromOrchestrator() bool {
	return t.Valid() && t != EventAgentMessage
}

// Severity returns the notification severity the orchestrator stamps on
// t, or "" for [EventAgentMessage] and for a value outside the catalog.
func (t EventType) Severity() string {
	switch t {
	case EventTypeSessionStarted, EventSessionCompleted, EventAutoMergeMerged, EventStageAdvanced:
		return "info"
	case EventSessionStopped,
		EventSessionFailed,
		EventEscalationCIFailure,
		EventEscalationReviewComments,
		EventEscalationBotReview,
		EventEscalationMergeConflicts,
		EventEscalationAutoMerge,
		EventEscalationMergeCompletion,
		EventBudgetHeld,
		EventStageNotAdvanced:
		return "warning"
	default:
		return ""
	}
}

// StageTransition is the hop decision a stage event reports. The zero value
// means the notification is not a stage event.
type StageTransition struct {
	SourceRule string
	TargetRule string
	ChainID    string
	HopCount   int

	// Reason is "failed" or "ceiling" on [EventStageNotAdvanced] and empty
	// otherwise.
	Reason string
}

// NotificationEnvelope carries system-owned session context.
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

	// DispatchID fences session identity to a single worker attempt.
	DispatchID string

	// SessionID is accepted only from a record fenced by DispatchID.
	SessionID string

	// Attempt is the retry or continuation attempt. It is nil on the
	// first run.
	Attempt *int

	// Agent is the dispatch-frozen agent kind, such as "claude-code".
	// It may be empty when no agent kind is resolved.
	Agent string

	// EventType is the catalog entry the notification belongs to.
	EventType EventType

	// Stage is the hop decision of a stage event and the zero value on
	// every other event.
	Stage StageTransition
}

// NotificationMessage carries the content of a notification: agent-supplied
// for [EventAgentMessage], built by the orchestrator for every other event.
// Severity is constrained to info, warning, or critical, and Category to
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

	// AgentText is agent-authored text carried by an orchestrator event.
	// Every destination renders it apart from Body; it is empty on
	// agent.message.
	AgentText string
}
