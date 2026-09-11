package agentcore

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TestTurnEndUsage_FinalizeOrdering pins P1 and P2: a non-nil recovered
// figure emits exactly one token_usage event, positioned after every
// event the caller emitted before calling Finalize and immediately
// before the terminal event, with nothing following the terminal
// event; the token_usage event's Usage equals the terminal event's
// Usage and the returned TurnResult.Usage, its Model is recovered.Model
// trimmed, its APIDurationMS is 0, and the terminal event's
// APIDurationMS equals the apiDurationMS argument.
func TestTurnEndUsage_FinalizeOrdering(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	// A prior event the adapter emitted before calling Finalize.
	events = append(events, domain.AgentEvent{Type: domain.EventToolResult})

	u := NewTurnEndUsage()
	recovered := &RecoveredUsage{
		Run:   domain.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		Model: "  claude-x  ",
	}
	result, agentErr := u.Finalize(emit, nil, TurnEvidence{Terminal: TerminalSuccess}, "sess-1", 500, recovered)
	if agentErr != nil {
		t.Fatalf("Finalize() error = %+v, want nil", agentErr)
	}

	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3 (the prior event, one token_usage event, the terminal event)", len(events))
	}
	if events[0].Type != domain.EventToolResult {
		t.Errorf("events[0].Type = %q, want %q (the pre-existing event)", events[0].Type, domain.EventToolResult)
	}
	if events[1].Type != domain.EventTokenUsage {
		t.Fatalf("events[1].Type = %q, want %q", events[1].Type, domain.EventTokenUsage)
	}
	if events[2].Type != domain.EventTurnCompleted {
		t.Fatalf("events[2].Type = %q, want %q", events[2].Type, domain.EventTurnCompleted)
	}

	wantUsage := domain.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	if events[1].Usage != wantUsage {
		t.Errorf("token_usage event Usage = %+v, want %+v", events[1].Usage, wantUsage)
	}
	if events[2].Usage != wantUsage {
		t.Errorf("terminal event Usage = %+v, want %+v", events[2].Usage, wantUsage)
	}
	if result.Usage != wantUsage {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if events[1].Model != "claude-x" {
		t.Errorf("token_usage event Model = %q, want %q (surrounding whitespace trimmed)", events[1].Model, "claude-x")
	}
	if events[1].APIDurationMS != 0 {
		t.Errorf("token_usage event APIDurationMS = %d, want 0", events[1].APIDurationMS)
	}
	if events[2].APIDurationMS != 500 {
		t.Errorf("terminal event APIDurationMS = %d, want 500", events[2].APIDurationMS)
	}
}

// TestTurnEndUsage_FinalizeNilRecoveredEmitsNoReport pins P3: a turn
// finalized with a nil recovered figure emits no token_usage event, and
// its terminal event and TurnResult carry Snapshot() (the zero value,
// since no turn has settled a figure yet).
func TestTurnEndUsage_FinalizeNilRecoveredEmitsNoReport(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	u := NewTurnEndUsage()
	result, agentErr := u.Finalize(emit, nil, TurnEvidence{Terminal: TerminalSuccess}, "sess-1", 0, nil)
	if agentErr != nil {
		t.Fatalf("Finalize() error = %+v, want nil", agentErr)
	}

	for i, e := range events {
		if e.Type == domain.EventTokenUsage {
			t.Errorf("events[%d] = %+v, want no token_usage event", i, e)
		}
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 (the terminal event alone)", len(events))
	}

	zero := domain.TokenUsage{}
	if events[0].Usage != zero {
		t.Errorf("terminal event Usage = %+v, want %+v", events[0].Usage, zero)
	}
	if result.Usage != zero {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, zero)
	}
	if u.Snapshot() != zero {
		t.Errorf("Snapshot() = %+v, want %+v", u.Snapshot(), zero)
	}
}

// TestTurnEndUsage_UsageMeasuredLatches pins P4: TurnResult.UsageMeasured
// is false on every turn before the first non-nil recovered figure, and
// true on that turn and every later turn of the session, including a
// later turn finalized with nil.
func TestTurnEndUsage_UsageMeasuredLatches(t *testing.T) {
	t.Parallel()

	emit := func(domain.AgentEvent) {}
	u := NewTurnEndUsage()
	ev := TurnEvidence{Terminal: TerminalSuccess}

	unmeasured, _ := u.Finalize(emit, nil, ev, "s", 0, nil)
	if unmeasured.UsageMeasured {
		t.Error("turn finalized before any recovered figure: UsageMeasured = true, want false")
	}

	measured, _ := u.Finalize(emit, nil, ev, "s", 0, &RecoveredUsage{Run: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	if !measured.UsageMeasured {
		t.Error("turn finalized with the first non-nil recovered figure: UsageMeasured = false, want true")
	}

	laterNil, _ := u.Finalize(emit, nil, ev, "s", 0, nil)
	if !laterNil.UsageMeasured {
		t.Error("later turn finalized with nil after a measured turn: UsageMeasured = false, want true")
	}
}

// TestTurnEndUsage_SnapshotNonDecreasing pins P5: over a rising-then-
// falling sequence of recovered.Run values, Snapshot() and every
// emitted Usage are componentwise non-decreasing, and TotalTokens
// always equals InputTokens plus OutputTokens.
func TestTurnEndUsage_SnapshotNonDecreasing(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	u := NewTurnEndUsage()
	ev := TurnEvidence{Terminal: TerminalSuccess}

	sequence := []domain.TokenUsage{
		{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 1},
		{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 5},
		{InputTokens: 30, OutputTokens: 4, CacheReadTokens: 2}, // falls relative to the previous turn
	}

	var prevSnapshot domain.TokenUsage
	for i, run := range sequence {
		if _, agentErr := u.Finalize(emit, nil, ev, "s", 0, &RecoveredUsage{Run: run}); agentErr != nil {
			t.Fatalf("turn %d: Finalize() error = %+v, want nil", i, agentErr)
		}

		snap := u.Snapshot()
		if snap.InputTokens < prevSnapshot.InputTokens ||
			snap.OutputTokens < prevSnapshot.OutputTokens ||
			snap.CacheReadTokens < prevSnapshot.CacheReadTokens ||
			snap.TotalTokens < prevSnapshot.TotalTokens {
			t.Errorf("turn %d: Snapshot() = %+v, want componentwise >= previous %+v", i, snap, prevSnapshot)
		}
		if snap.TotalTokens != snap.InputTokens+snap.OutputTokens {
			t.Errorf("turn %d: Snapshot().TotalTokens = %d, want InputTokens+OutputTokens = %d", i, snap.TotalTokens, snap.InputTokens+snap.OutputTokens)
		}
		prevSnapshot = snap
	}

	var prevEventUsage domain.TokenUsage
	for i, e := range events {
		if e.Type != domain.EventTokenUsage {
			continue
		}
		if e.Usage.InputTokens < prevEventUsage.InputTokens ||
			e.Usage.OutputTokens < prevEventUsage.OutputTokens ||
			e.Usage.CacheReadTokens < prevEventUsage.CacheReadTokens ||
			e.Usage.TotalTokens < prevEventUsage.TotalTokens {
			t.Errorf("events[%d].Usage = %+v, want componentwise >= previous emitted %+v", i, e.Usage, prevEventUsage)
		}
		if e.Usage.TotalTokens != e.Usage.InputTokens+e.Usage.OutputTokens {
			t.Errorf("events[%d].Usage.TotalTokens = %d, want InputTokens+OutputTokens = %d", i, e.Usage.TotalTokens, e.Usage.InputTokens+e.Usage.OutputTokens)
		}
		prevEventUsage = e.Usage
	}
}

// TestTurnEndUsage_FinalizeNilEmitPanics pins Finalize's documented
// contract that a nil emit panics, mirroring FinalizeTurn.
func TestTurnEndUsage_FinalizeNilEmitPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("Finalize(nil emit) did not panic, want a panic")
		}
	}()

	u := NewTurnEndUsage()
	_, _ = u.Finalize(nil, nil, TurnEvidence{Terminal: TerminalSuccess}, "s", 0, nil)
}
