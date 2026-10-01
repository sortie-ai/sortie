package domain

import (
	"context"
	"testing"
)

// stubNotifier is a minimal Notifier implementation for compile-time
// interface assertion.
type stubNotifier struct{}

var _ Notifier = (*stubNotifier)(nil)

func (s *stubNotifier) Send(_ context.Context, _ Notification) error { return nil }

func TestNotifierInterface(t *testing.T) {
	t.Parallel()

	// The compile-time assertion (var _ Notifier = (*stubNotifier)(nil))
	// verifies interface satisfaction. This test exercises the Send method
	// to confirm the concrete type is callable via the interface.
	var n Notifier = &stubNotifier{}
	if err := n.Send(context.Background(), Notification{}); err != nil {
		t.Errorf("Send() = %v, want nil", err)
	}
}

func TestNotification_FieldRoundTrip(t *testing.T) {
	t.Parallel()

	n := Notification{
		Envelope: NotificationEnvelope{
			NotificationID: "uuid-1234",
			Timestamp:      "2026-06-10T14:03:05Z",
			Source:         "builder-host",
			IssueID:        "issue-99",
			Identifier:     "PROJ-99",
			SessionID:      "sess-abc",
			Attempt:        new(3),
			Agent:          "claude-code",
		},
		Message: NotificationMessage{
			Severity: "warning",
			Title:    "Review needed",
			Body:     "Agent blocked on ambiguous requirements.",
			Category: "decision_needed",
		},
	}

	if got := n.Envelope.NotificationID; got != "uuid-1234" {
		t.Errorf("Envelope.NotificationID = %q, want %q", got, "uuid-1234")
	}
	if got := n.Envelope.Timestamp; got != "2026-06-10T14:03:05Z" {
		t.Errorf("Envelope.Timestamp = %q, want %q", got, "2026-06-10T14:03:05Z")
	}
	if got := n.Envelope.Source; got != "builder-host" {
		t.Errorf("Envelope.Source = %q, want %q", got, "builder-host")
	}
	if got := n.Envelope.IssueID; got != "issue-99" {
		t.Errorf("Envelope.IssueID = %q, want %q", got, "issue-99")
	}
	if got := n.Envelope.Identifier; got != "PROJ-99" {
		t.Errorf("Envelope.Identifier = %q, want %q", got, "PROJ-99")
	}
	if got := n.Envelope.SessionID; got != "sess-abc" {
		t.Errorf("Envelope.SessionID = %q, want %q", got, "sess-abc")
	}
	if n.Envelope.Attempt == nil {
		t.Fatal("Envelope.Attempt = nil, want non-nil")
	}
	if got := *n.Envelope.Attempt; got != 3 {
		t.Errorf("Envelope.Attempt = %d, want 3", got)
	}
	if got := n.Envelope.Agent; got != "claude-code" {
		t.Errorf("Envelope.Agent = %q, want %q", got, "claude-code")
	}
	if got := n.Message.Severity; got != "warning" {
		t.Errorf("Message.Severity = %q, want %q", got, "warning")
	}
	if got := n.Message.Title; got != "Review needed" {
		t.Errorf("Message.Title = %q, want %q", got, "Review needed")
	}
	if got := n.Message.Body; got != "Agent blocked on ambiguous requirements." {
		t.Errorf("Message.Body = %q, want %q", got, "Agent blocked on ambiguous requirements.")
	}
	if got := n.Message.Category; got != "decision_needed" {
		t.Errorf("Message.Category = %q, want %q", got, "decision_needed")
	}
}

func TestNotificationEnvelope_AttemptNilOnFirstRun(t *testing.T) {
	t.Parallel()

	n := Notification{
		Envelope: NotificationEnvelope{
			NotificationID: "id-1",
			Attempt:        nil,
		},
	}

	if n.Envelope.Attempt != nil {
		t.Errorf("Envelope.Attempt = %v, want nil on first run", *n.Envelope.Attempt)
	}
}

func TestNotification_SelfContained(t *testing.T) {
	t.Parallel()

	// A self-contained Notification holds all fields without referring to
	// external state; creating a valid zero-field value compiles and
	// round-trips its fields.
	n := Notification{}
	if n.Envelope.Agent != "" {
		t.Errorf("zero Envelope.Agent = %q, want empty string", n.Envelope.Agent)
	}
	if n.Message.Category != "" {
		t.Errorf("zero Message.Category = %q, want empty string", n.Message.Category)
	}
}

func TestEventTypes_Catalog(t *testing.T) {
	t.Parallel()

	want := []EventType{
		"session.started", "session.completed", "session.stopped", "session.failed",
		"escalation.ci_failure", "escalation.review_comments", "escalation.bot_review",
		"escalation.merge_conflicts", "escalation.auto_merge", "escalation.merge_completion",
		"auto_merge.merged", "budget.held", "agent.message",
	}

	got := EventTypes()
	if len(got) != len(want) {
		t.Fatalf("EventTypes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EventTypes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	got[0] = "tampered"
	if EventTypes()[0] != want[0] {
		t.Errorf("EventTypes()[0] after modifying a returned slice = %q, want %q", EventTypes()[0], want[0])
	}
}

func TestEventType_Classification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		eventType        EventType
		wantValid        bool
		wantOrchestrator bool
		wantSeverity     string
	}{
		{"session.started", true, true, "info"},
		{"session.completed", true, true, "info"},
		{"session.stopped", true, true, "warning"},
		{"session.failed", true, true, "warning"},
		{"escalation.ci_failure", true, true, "warning"},
		{"escalation.review_comments", true, true, "warning"},
		{"escalation.bot_review", true, true, "warning"},
		{"escalation.merge_conflicts", true, true, "warning"},
		{"escalation.auto_merge", true, true, "warning"},
		{"escalation.merge_completion", true, true, "warning"},
		{"auto_merge.merged", true, true, "info"},
		{"budget.held", true, true, "warning"},
		{"agent.message", true, false, ""},
		{"session.begun", false, false, ""},
		{"", false, false, ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.eventType), func(t *testing.T) {
			t.Parallel()

			if got := tt.eventType.Valid(); got != tt.wantValid {
				t.Errorf("EventType(%q).Valid() = %v, want %v", tt.eventType, got, tt.wantValid)
			}
			if got := tt.eventType.FromOrchestrator(); got != tt.wantOrchestrator {
				t.Errorf("EventType(%q).FromOrchestrator() = %v, want %v", tt.eventType, got, tt.wantOrchestrator)
			}
			if got := tt.eventType.Severity(); got != tt.wantSeverity {
				t.Errorf("EventType(%q).Severity() = %q, want %q", tt.eventType, got, tt.wantSeverity)
			}
		})
	}
}
