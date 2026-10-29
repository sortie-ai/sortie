package agentcore

import (
	"maps"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestTurnEndUsage_FinalizeOrdering(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	events = append(events, domain.AgentEvent{Type: domain.EventToolResult})

	u := NewTurnEndUsage()
	recovered := &RecoveredUsage{
		Run:   domain.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		Model: "  claude-x  ",
	}
	result, agentErr := u.Finalize(emit, nil, TurnEvidence{Terminal: TerminalSuccess}, "sess-1", 500, recovered, TurnSpend{})
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

func TestTurnEndUsage_FinalizeNilRecoveredEmitsNoReport(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	u := NewTurnEndUsage()
	result, agentErr := u.Finalize(emit, nil, TurnEvidence{Terminal: TerminalSuccess}, "sess-1", 0, nil, TurnSpend{})
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

func TestTurnEndUsage_UsageMeasuredLatches(t *testing.T) {
	t.Parallel()

	emit := func(domain.AgentEvent) {}
	u := NewTurnEndUsage()
	ev := TurnEvidence{Terminal: TerminalSuccess}

	if u.Measured() {
		t.Error("Measured() on a value from NewTurnEndUsage = true, want false")
	}

	unmeasured, _ := u.Finalize(emit, nil, ev, "s", 0, nil, TurnSpend{})
	if unmeasured.UsageMeasured {
		t.Error("turn finalized before any recovered figure: UsageMeasured = true, want false")
	}
	if u.Measured() != unmeasured.UsageMeasured {
		t.Errorf("Measured() = %v, want %v (equal to Finalize's UsageMeasured)", u.Measured(), unmeasured.UsageMeasured)
	}

	measured, _ := u.Finalize(emit, nil, ev, "s", 0, &RecoveredUsage{Run: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, TurnSpend{})
	if !measured.UsageMeasured {
		t.Error("turn finalized with the first non-nil recovered figure: UsageMeasured = false, want true")
	}
	if u.Measured() != measured.UsageMeasured {
		t.Errorf("Measured() = %v, want %v (equal to Finalize's UsageMeasured)", u.Measured(), measured.UsageMeasured)
	}

	laterNil, _ := u.Finalize(emit, nil, ev, "s", 0, nil, TurnSpend{})
	if !laterNil.UsageMeasured {
		t.Error("later turn finalized with nil after a measured turn: UsageMeasured = false, want true")
	}
	if u.Measured() != laterNil.UsageMeasured {
		t.Errorf("Measured() = %v, want %v (equal to Finalize's UsageMeasured)", u.Measured(), laterNil.UsageMeasured)
	}
}

func TestTurnEndUsage_SnapshotNonDecreasing(t *testing.T) {
	t.Parallel()

	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) { events = append(events, e) }

	u := NewTurnEndUsage()
	ev := TurnEvidence{Terminal: TerminalSuccess}

	sequence := []domain.TokenUsage{
		{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 1},
		{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 5},
		{InputTokens: 30, OutputTokens: 4, CacheReadTokens: 2},
	}

	var prevSnapshot domain.TokenUsage
	for i, run := range sequence {
		if _, agentErr := u.Finalize(emit, nil, ev, "s", 0, &RecoveredUsage{Run: run}, TurnSpend{}); agentErr != nil {
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

func TestTurnEndUsage_FinalizeNilEmitPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("Finalize(nil emit) did not panic, want a panic")
		}
	}()

	u := NewTurnEndUsage()
	_, _ = u.Finalize(nil, nil, TurnEvidence{Terminal: TerminalSuccess}, "s", 0, nil, TurnSpend{})
}

func TestTurnEndUsage_ReportLive(t *testing.T) {
	t.Parallel()

	tokens := func(in, out int64) domain.TokenUsage {
		return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
	}
	split := map[string]domain.TokenUsage{"m1": tokens(30, 4)}

	type step struct {
		settle *RecoveredUsage
		live   domain.TokenUsage
		want   *domain.TokenUsage
	}

	tests := []struct {
		name         string
		steps        []step
		wantMeasured bool
		wantByModel  map[string]domain.TokenUsage
	}{
		{
			name:  "counts with no input and no output emit nothing and leave the turn unmeasured",
			steps: []step{{live: domain.TokenUsage{CacheReadTokens: 7}}},
		},
		{
			name: "a later settled figure replaces the live counts instead of adding them",
			steps: []step{
				{live: tokens(10, 2), want: new(tokens(10, 2))},
				{settle: &RecoveredUsage{Run: tokens(30, 4), ByModel: split}, want: new(tokens(30, 4))},
			},
			wantMeasured: true,
			wantByModel:  split,
		},
		{
			name: "the snapshot never falls when the settled figure is below the live counts",
			steps: []step{
				{live: tokens(50, 10), want: new(tokens(50, 10))},
				{settle: &RecoveredUsage{Run: tokens(30, 4)}, want: new(tokens(50, 10))},
			},
			wantMeasured: true,
		},
		{
			name: "live counts after a settled figure add to it and keep its per-model split",
			steps: []step{
				{settle: &RecoveredUsage{Run: tokens(30, 4), ByModel: split}, want: new(tokens(30, 4))},
				{live: tokens(5, 1), want: new(tokens(35, 5))},
			},
			wantMeasured: true,
			wantByModel:  split,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var events []domain.AgentEvent
			emit := func(e domain.AgentEvent) { events = append(events, e) }
			u := NewTurnEndUsage()

			for i, st := range tt.steps {
				before := len(events)
				if st.settle != nil {
					u.Settle(emit, st.settle)
				} else {
					u.ReportLive(emit, st.live)
				}

				emitted := events[before:]
				if st.want == nil {
					if len(emitted) != 0 {
						t.Fatalf("step %d: emitted %d events, want none", i, len(emitted))
					}
					continue
				}
				if len(emitted) != 1 || emitted[0].Type != domain.EventTokenUsage {
					t.Fatalf("step %d: emitted %+v, want one %q event", i, emitted, domain.EventTokenUsage)
				}
				if st.settle == nil && emitted[0].Model != "" {
					t.Errorf("step %d: ReportLive event Model = %q, want empty", i, emitted[0].Model)
				}
				if emitted[0].Usage != *st.want {
					t.Errorf("step %d: event Usage = %+v, want %+v", i, emitted[0].Usage, *st.want)
				}
			}

			if got := u.Measured(); got != tt.wantMeasured {
				t.Errorf("Measured() = %v, want %v", got, tt.wantMeasured)
			}
			if len(events) > 0 && !maps.Equal(events[len(events)-1].UsageByModel, tt.wantByModel) {
				t.Errorf("last event UsageByModel = %+v, want %+v", events[len(events)-1].UsageByModel, tt.wantByModel)
			}
		})
	}
}
