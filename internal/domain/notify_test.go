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
