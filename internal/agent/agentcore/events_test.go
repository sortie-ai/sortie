package agentcore

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
)

func captureOne(t *testing.T, fn func(emit func(domain.AgentEvent))) domain.AgentEvent {
	t.Helper()
	var got domain.AgentEvent
	count := 0
	fn(func(e domain.AgentEvent) {
		count++
		got = e
	})
	if count != 1 {
		t.Fatalf("emit called %d times, want 1", count)
	}
	return got
}

func assertTimestamp(t *testing.T, ts time.Time) {
	t.Helper()
	if ts.IsZero() {
		t.Error("Timestamp is zero, want non-zero")
	}
	if ts.Location() != time.UTC {
		t.Errorf("Timestamp.Location = %v, want UTC", ts.Location())
	}
}

func TestEmitSessionStarted(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitSessionStarted(emit, "42", "sess-1")
	})

	if got.Type != domain.EventSessionStarted {
		t.Errorf("Type = %q, want EventSessionStarted", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.AgentPID != "42" {
		t.Errorf("AgentPID = %q, want 42", got.AgentPID)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
	if got.Message != "session started" {
		t.Errorf("Message = %q, want 'session started'", got.Message)
	}
}

func TestEmitSessionStarted_EmptyPID(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitSessionStarted(emit, "", "sess-2")
	})
	if got.AgentPID != "" {
		t.Errorf("AgentPID = %q, want empty", got.AgentPID)
	}
}

func TestEmitTurnCompleted(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitTurnCompleted(emit, "done", 250)
	})

	if got.Type != domain.EventTurnCompleted {
		t.Errorf("Type = %q, want EventTurnCompleted", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.Message != "done" {
		t.Errorf("Message = %q, want 'done'", got.Message)
	}
	if got.APIDurationMS != 250 {
		t.Errorf("APIDurationMS = %d, want 250", got.APIDurationMS)
	}
	if got.AgentPID != "" || got.SessionID != "" {
		t.Errorf("AgentPID/SessionID should be zero, got %q/%q", got.AgentPID, got.SessionID)
	}
}

func TestEmitTurnFailed(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitTurnFailed(emit, "oops", 100)
	})

	if got.Type != domain.EventTurnFailed {
		t.Errorf("Type = %q, want EventTurnFailed", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.Message != "oops" {
		t.Errorf("Message = %q, want 'oops'", got.Message)
	}
	if got.APIDurationMS != 100 {
		t.Errorf("APIDurationMS = %d, want 100", got.APIDurationMS)
	}
}

func TestEmitTurnCancelled(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitTurnCancelled(emit, "cancelled by user")
	})

	if got.Type != domain.EventTurnCancelled {
		t.Errorf("Type = %q, want EventTurnCancelled", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.Message != "cancelled by user" {
		t.Errorf("Message = %q, want 'cancelled by user'", got.Message)
	}
}

func TestEmitMalformed_ShortLine(t *testing.T) {
	t.Parallel()

	line := []byte("bad json")
	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitMalformed(emit, line)
	})

	if got.Type != domain.EventMalformed {
		t.Errorf("Type = %q, want EventMalformed", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.Message != "bad json" {
		t.Errorf("Message = %q, want %q", got.Message, "bad json")
	}
}

func TestEmitMalformed_Truncation(t *testing.T) {
	t.Parallel()

	// Build a string with 600 Unicode code points (Greek letters to avoid
	// single-byte/multi-byte ambiguity).
	var sb strings.Builder
	for range 600 {
		sb.WriteRune('α')
	}
	line := []byte(sb.String())

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitMalformed(emit, line)
	})

	runeCount := utf8.RuneCountInString(got.Message)
	// TruncateRunes appends "…" (1 rune) after 500, so total = 501.
	if runeCount > 501 {
		t.Errorf("Message rune count = %d, want ≤ 501 after truncation", runeCount)
	}
	if !strings.HasSuffix(got.Message, "…") {
		t.Errorf("Message should end with '…' after truncation, got %q", got.Message[max(0, len(got.Message)-10):])
	}
}

func TestEmitNotification(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitNotification(emit, "plan updated")
	})

	if got.Type != domain.EventNotification {
		t.Errorf("Type = %q, want EventNotification", got.Type)
	}
	assertTimestamp(t, got.Timestamp)
	if got.Message != "plan updated" {
		t.Errorf("Message = %q, want 'plan updated'", got.Message)
	}
}

func TestEmitNotification_Empty(t *testing.T) {
	t.Parallel()

	got := captureOne(t, func(emit func(domain.AgentEvent)) {
		EmitNotification(emit, "")
	})
	if got.Message != "" {
		t.Errorf("Message = %q, want empty", got.Message)
	}
}
