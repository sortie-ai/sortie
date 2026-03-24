package orchestrator

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// newStateWithEntry returns a *State containing a single RunningEntry under
// issueID. Helpers call this to avoid repetitive setup in every test.
func newStateWithEntry(issueID string) (*State, *RunningEntry) {
	state := NewState(5000, 4, nil, AgentTotals{})
	entry := &RunningEntry{}
	state.Running[issueID] = entry
	return state, entry
}

// TestHandleAgentEvent_UnknownIssue verifies that delivering an event for an
// issueID that is not in state.Running is a silent no-op: no panic, no state
// change.
func TestHandleAgentEvent_UnknownIssue(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	HandleAgentEvent(state, "GHOST-999", domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: time.Now().UTC(),
		Message:   "should be dropped",
	}, slog.Default())

	if state.AgentTotals != (AgentTotals{}) {
		t.Errorf("AgentTotals modified for unknown issue: %+v", state.AgentTotals)
	}
	if len(state.Running) != 0 {
		t.Errorf("Running map modified for unknown issue: len=%d", len(state.Running))
	}
}

// TestHandleAgentEvent_BasicFields verifies that an EventNotification updates
// LastAgentEvent, LastAgentTimestamp, LastAgentMessage, and AgentPID, while
// TurnCount remains 0 and AgentTotals are unchanged.
func TestHandleAgentEvent_BasicFields(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-1")
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	HandleAgentEvent(state, "MT-1", domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: ts,
		AgentPID:  "4242",
		Message:   "hello",
	}, slog.Default())

	if entry.LastAgentEvent != "notification" {
		t.Errorf("LastAgentEvent = %q, want %q", entry.LastAgentEvent, "notification")
	}
	if !entry.LastAgentTimestamp.Equal(ts) {
		t.Errorf("LastAgentTimestamp = %v, want %v", entry.LastAgentTimestamp, ts)
	}
	if entry.LastAgentMessage != "hello" {
		t.Errorf("LastAgentMessage = %q, want %q", entry.LastAgentMessage, "hello")
	}
	if entry.AgentPID != "4242" {
		t.Errorf("AgentPID = %q, want %q", entry.AgentPID, "4242")
	}
	if entry.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0", entry.TurnCount)
	}
	if state.AgentTotals != (AgentTotals{}) {
		t.Errorf("AgentTotals modified unexpectedly: %+v", state.AgentTotals)
	}
}

// TestHandleAgentEvent_SessionStarted verifies that EventSessionStarted
// populates SessionID and AgentPID on the entry while leaving TurnCount
// unchanged (session_started is not a turn-finalization event).
func TestHandleAgentEvent_SessionStarted(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-2")

	HandleAgentEvent(state, "MT-2", domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: time.Now().UTC(),
		AgentPID:  "9999",
		SessionID: "sess-1",
		Message:   "session started",
	}, slog.Default())

	if entry.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", entry.SessionID, "sess-1")
	}
	if entry.AgentPID != "9999" {
		t.Errorf("AgentPID = %q, want %q", entry.AgentPID, "9999")
	}
	if entry.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0 (session_started is not a finalization event)", entry.TurnCount)
	}
}

// TestHandleAgentEvent_SessionStarted_EmptySessionID verifies that an
// EventSessionStarted with an empty SessionID does NOT overwrite an
// existing RunningEntry.SessionID.
func TestHandleAgentEvent_SessionStarted_EmptySessionID(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-3")
	entry.SessionID = "existing-id"

	HandleAgentEvent(state, "MT-3", domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: time.Now().UTC(),
		SessionID: "",
	}, slog.Default())

	if entry.SessionID != "existing-id" {
		t.Errorf("SessionID = %q, want %q (must not overwrite with empty)", entry.SessionID, "existing-id")
	}
}

// TestHandleAgentEvent_TurnCount verifies the turn-finalization set.
// Each finalization event type must increment TurnCount by exactly 1.
// Non-finalization event types must not increment TurnCount.
func TestHandleAgentEvent_TurnCount(t *testing.T) {
	t.Parallel()

	finalizationTypes := []struct {
		name      string
		eventType domain.AgentEventType
	}{
		{"turn_completed", domain.EventTurnCompleted},
		{"turn_failed", domain.EventTurnFailed},
		{"turn_cancelled", domain.EventTurnCancelled},
		{"turn_ended_with_error", domain.EventTurnEndedWithError},
		{"turn_input_required", domain.EventTurnInputRequired},
	}

	for _, ft := range finalizationTypes {
		t.Run("increments on "+ft.name, func(t *testing.T) {
			t.Parallel()
			state, entry := newStateWithEntry("MT-4")

			HandleAgentEvent(state, "MT-4", domain.AgentEvent{
				Type:      ft.eventType,
				Timestamp: time.Now().UTC(),
			}, slog.Default())

			if entry.TurnCount != 1 {
				t.Errorf("TurnCount = %d, want 1 for %q", entry.TurnCount, ft.eventType)
			}
		})
	}

	nonFinalizationTypes := []struct {
		name      string
		eventType domain.AgentEventType
	}{
		{"session_started", domain.EventSessionStarted},
		{"startup_failed", domain.EventStartupFailed},
		{"notification", domain.EventNotification},
		{"token_usage", domain.EventTokenUsage},
		{"other_message", domain.EventOtherMessage},
	}

	for _, nft := range nonFinalizationTypes {
		t.Run("no increment on "+nft.name, func(t *testing.T) {
			t.Parallel()
			state, entry := newStateWithEntry("MT-5")

			HandleAgentEvent(state, "MT-5", domain.AgentEvent{
				Type:      nft.eventType,
				Timestamp: time.Now().UTC(),
			}, slog.Default())

			if entry.TurnCount != 0 {
				t.Errorf("TurnCount = %d, want 0 for non-finalization event %q", entry.TurnCount, nft.eventType)
			}
		})
	}
}

// TestHandleAgentEvent_TokenUsage_DeltaAccumulation verifies the cumulative
// delta algorithm: the orchestrator computes positive-clamped deltas relative
// to LastReported* and accumulates them without double-counting on duplicate
// reports.
func TestHandleAgentEvent_TokenUsage_DeltaAccumulation(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-6")
	ts := time.Now().UTC()

	// First report: all counters start at zero; delta == reported value.
	HandleAgentEvent(state, "MT-6", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage: domain.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}, slog.Default())

	if entry.AgentInputTokens != 100 {
		t.Errorf("after 1st event: AgentInputTokens = %d, want 100", entry.AgentInputTokens)
	}
	if entry.AgentOutputTokens != 50 {
		t.Errorf("after 1st event: AgentOutputTokens = %d, want 50", entry.AgentOutputTokens)
	}
	if entry.AgentTotalTokens != 150 {
		t.Errorf("after 1st event: AgentTotalTokens = %d, want 150", entry.AgentTotalTokens)
	}
	if entry.LastReportedInputTokens != 100 {
		t.Errorf("after 1st event: LastReportedInputTokens = %d, want 100", entry.LastReportedInputTokens)
	}
	if state.AgentTotals.InputTokens != 100 {
		t.Errorf("after 1st event: AgentTotals.InputTokens = %d, want 100", state.AgentTotals.InputTokens)
	}

	// Second report: cumulative values increased; delta is +100/+50/+150.
	HandleAgentEvent(state, "MT-6", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage: domain.TokenUsage{
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
		},
	}, slog.Default())

	if entry.AgentInputTokens != 200 {
		t.Errorf("after 2nd event: AgentInputTokens = %d, want 200", entry.AgentInputTokens)
	}
	if state.AgentTotals.InputTokens != 200 {
		t.Errorf("after 2nd event: AgentTotals.InputTokens = %d, want 200", state.AgentTotals.InputTokens)
	}
	if state.AgentTotals.OutputTokens != 100 {
		t.Errorf("after 2nd event: AgentTotals.OutputTokens = %d, want 100", state.AgentTotals.OutputTokens)
	}

	// Third report: same values as second — zero delta, no double-counting.
	HandleAgentEvent(state, "MT-6", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage: domain.TokenUsage{
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
		},
	}, slog.Default())

	if entry.AgentInputTokens != 200 {
		t.Errorf("after duplicate event: AgentInputTokens = %d, want 200 (no double-count)", entry.AgentInputTokens)
	}
	if state.AgentTotals.InputTokens != 200 {
		t.Errorf("after duplicate event: AgentTotals.InputTokens = %d, want 200 (no double-count)", state.AgentTotals.InputTokens)
	}
}

// TestHandleAgentEvent_FullSequence sends the full five-event sequence and
// asserts the complete set of final field values.
func TestHandleAgentEvent_FullSequence(t *testing.T) {
	t.Parallel()

	const issueID = "MT-7"
	state, entry := newStateWithEntry(issueID)
	ts := time.Now().UTC()

	// 1. Session started.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: ts,
		AgentPID:  "1234",
		SessionID: "sess-1",
		Message:   "session started",
	}, slog.Default())

	// 2. First token usage: {100, 50, 150}.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, slog.Default())

	// 3. Second token usage: {200, 100, 300} — delta {+100, +50, +150}.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	}, slog.Default())

	// 4. Duplicate token usage — zero delta.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	}, slog.Default())

	// 5. Turn completed.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: ts,
	}, slog.Default())

	// Assert all final field values.
	if entry.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", entry.SessionID, "sess-1")
	}
	if entry.AgentPID != "1234" {
		t.Errorf("AgentPID = %q, want %q", entry.AgentPID, "1234")
	}
	if entry.LastAgentEvent != "turn_completed" {
		t.Errorf("LastAgentEvent = %q, want %q", entry.LastAgentEvent, "turn_completed")
	}
	if entry.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", entry.TurnCount)
	}
	if entry.AgentInputTokens != 200 {
		t.Errorf("AgentInputTokens = %d, want 200", entry.AgentInputTokens)
	}
	if entry.AgentOutputTokens != 100 {
		t.Errorf("AgentOutputTokens = %d, want 100", entry.AgentOutputTokens)
	}
	if entry.AgentTotalTokens != 300 {
		t.Errorf("AgentTotalTokens = %d, want 300", entry.AgentTotalTokens)
	}
	if state.AgentTotals.InputTokens != 200 {
		t.Errorf("AgentTotals.InputTokens = %d, want 200", state.AgentTotals.InputTokens)
	}
	if state.AgentTotals.OutputTokens != 100 {
		t.Errorf("AgentTotals.OutputTokens = %d, want 100", state.AgentTotals.OutputTokens)
	}
}

// TestHandleAgentEvent_TwoSessions_AgentTotals verifies that AgentTotals
// accumulates token deltas from independent sessions without leaking
// per-session counters between entries.
func TestHandleAgentEvent_TwoSessions_AgentTotals(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	entryA := &RunningEntry{}
	entryB := &RunningEntry{}
	state.Running["A-1"] = entryA
	state.Running["B-1"] = entryB
	ts := time.Now().UTC()

	HandleAgentEvent(state, "A-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 300, OutputTokens: 100, TotalTokens: 400},
	}, slog.Default())
	HandleAgentEvent(state, "B-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 50, TotalTokens: 250},
	}, slog.Default())

	// Global totals are the sum of both sessions.
	if state.AgentTotals.InputTokens != 500 {
		t.Errorf("AgentTotals.InputTokens = %d, want 500", state.AgentTotals.InputTokens)
	}
	if state.AgentTotals.OutputTokens != 150 {
		t.Errorf("AgentTotals.OutputTokens = %d, want 150", state.AgentTotals.OutputTokens)
	}
	if state.AgentTotals.TotalTokens != 650 {
		t.Errorf("AgentTotals.TotalTokens = %d, want 650", state.AgentTotals.TotalTokens)
	}

	// Per-session counters must not bleed between entries.
	if entryA.AgentInputTokens != 300 {
		t.Errorf("entryA.AgentInputTokens = %d, want 300", entryA.AgentInputTokens)
	}
	if entryB.AgentInputTokens != 200 {
		t.Errorf("entryB.AgentInputTokens = %d, want 200", entryB.AgentInputTokens)
	}
}

// TestHandleAgentEvent_RateLimits verifies that a non-nil RateLimits payload
// is stored as a shallow copy in state.AgentRateLimits so that mutating the
// original map after delivery does not corrupt the stored snapshot. Also
// verifies that a nil RateLimits event does not clear an existing snapshot.
func TestHandleAgentEvent_RateLimits(t *testing.T) {
	t.Parallel()

	state, _ := newStateWithEntry("MT-9")
	ts := time.Now().UTC()

	originalMap := map[string]any{"limit": 100}
	HandleAgentEvent(state, "MT-9", domain.AgentEvent{
		Type:       domain.EventNotification,
		Timestamp:  ts,
		RateLimits: originalMap,
	}, slog.Default())

	if state.AgentRateLimits == nil {
		t.Fatal("AgentRateLimits = nil, want non-nil after delivery")
	}
	if got := state.AgentRateLimits.Data["limit"]; got != 100 {
		t.Errorf("AgentRateLimits.Data[\"limit\"] = %v, want 100", got)
	}

	// Mutate the original map — the stored copy must be unaffected.
	originalMap["limit"] = 999
	if got := state.AgentRateLimits.Data["limit"]; got != 100 {
		t.Errorf("after mutation: AgentRateLimits.Data[\"limit\"] = %v, want 100 (shallow copy isolation breach)", got)
	}

	// A second event with nil RateLimits must NOT clear the existing snapshot.
	HandleAgentEvent(state, "MT-9", domain.AgentEvent{
		Type:       domain.EventNotification,
		Timestamp:  ts,
		RateLimits: nil,
	}, slog.Default())
	if state.AgentRateLimits == nil {
		t.Error("AgentRateLimits = nil after nil-RateLimits event, want previous snapshot preserved")
	}
}

// TestHandleAgentEvent_MonotonicTimestamp verifies the monotonic timestamp
// guard: out-of-order events must not regress LastAgentTimestamp, while
// LastAgentEvent is still updated unconditionally.
func TestHandleAgentEvent_MonotonicTimestamp(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-10")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tPlus2 := base.Add(2 * time.Second)
	tPlus1 := base.Add(1 * time.Second)
	tPlus3 := base.Add(3 * time.Second)

	// Event A at T+2s — advances timestamp from zero value.
	HandleAgentEvent(state, "MT-10", domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: tPlus2,
	}, slog.Default())
	if !entry.LastAgentTimestamp.Equal(tPlus2) {
		t.Errorf("after event A: LastAgentTimestamp = %v, want %v", entry.LastAgentTimestamp, tPlus2)
	}

	// Event B at T+1s (out-of-order) — must NOT regress the timestamp.
	HandleAgentEvent(state, "MT-10", domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: tPlus1,
	}, slog.Default())
	if !entry.LastAgentTimestamp.Equal(tPlus2) {
		t.Errorf("after out-of-order event B: LastAgentTimestamp = %v, want %v (must not regress)", entry.LastAgentTimestamp, tPlus2)
	}
	// LastAgentEvent must still be updated unconditionally.
	if entry.LastAgentEvent != "turn_completed" {
		t.Errorf("after event B: LastAgentEvent = %q, want %q", entry.LastAgentEvent, "turn_completed")
	}

	// Event C at T+3s — must advance the timestamp.
	HandleAgentEvent(state, "MT-10", domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: tPlus3,
	}, slog.Default())
	if !entry.LastAgentTimestamp.Equal(tPlus3) {
		t.Errorf("after event C: LastAgentTimestamp = %v, want %v", entry.LastAgentTimestamp, tPlus3)
	}
}

// TestHandleAgentEvent_TokenUsage_CounterRegression verifies that when an
// adapter reports a lower cumulative token count than previously seen
// (counter regression), the monotonic baseline prevents double-counting
// on subsequent legitimate increases.
func TestHandleAgentEvent_TokenUsage_CounterRegression(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MT-11")
	ts := time.Now().UTC()

	// First report: {200, 100, 300}.
	HandleAgentEvent(state, "MT-11", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	}, slog.Default())

	if entry.AgentInputTokens != 200 {
		t.Errorf("after 1st: AgentInputTokens = %d, want 200", entry.AgentInputTokens)
	}

	// Second report: regression {150, 80, 230} — delta clamped to zero,
	// baselines must NOT regress.
	HandleAgentEvent(state, "MT-11", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 150, OutputTokens: 80, TotalTokens: 230},
	}, slog.Default())

	if entry.AgentInputTokens != 200 {
		t.Errorf("after regression: AgentInputTokens = %d, want 200 (unchanged)", entry.AgentInputTokens)
	}
	if entry.LastReportedInputTokens != 200 {
		t.Errorf("after regression: LastReportedInputTokens = %d, want 200 (must not regress)", entry.LastReportedInputTokens)
	}
	if entry.LastReportedOutputTokens != 100 {
		t.Errorf("after regression: LastReportedOutputTokens = %d, want 100 (must not regress)", entry.LastReportedOutputTokens)
	}
	if entry.LastReportedTotalTokens != 300 {
		t.Errorf("after regression: LastReportedTotalTokens = %d, want 300 (must not regress)", entry.LastReportedTotalTokens)
	}

	// Third report: legitimate increase {250, 120, 370} — delta is
	// computed against the preserved baseline {200, 100, 300}, yielding
	// {+50, +20, +70}. Without monotonic baselines this would compute
	// against the regressed {150, 80, 230} and produce {+100, +40, +140},
	// double-counting 50/20/70 tokens.
	HandleAgentEvent(state, "MT-11", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 250, OutputTokens: 120, TotalTokens: 370},
	}, slog.Default())

	if entry.AgentInputTokens != 250 {
		t.Errorf("after recovery: AgentInputTokens = %d, want 250", entry.AgentInputTokens)
	}
	if entry.AgentOutputTokens != 120 {
		t.Errorf("after recovery: AgentOutputTokens = %d, want 120", entry.AgentOutputTokens)
	}
	if entry.AgentTotalTokens != 370 {
		t.Errorf("after recovery: AgentTotalTokens = %d, want 370", entry.AgentTotalTokens)
	}
	if state.AgentTotals.InputTokens != 250 {
		t.Errorf("after recovery: AgentTotals.InputTokens = %d, want 250", state.AgentTotals.InputTokens)
	}
	if state.AgentTotals.OutputTokens != 120 {
		t.Errorf("after recovery: AgentTotals.OutputTokens = %d, want 120", state.AgentTotals.OutputTokens)
	}
	if state.AgentTotals.TotalTokens != 370 {
		t.Errorf("after recovery: AgentTotals.TotalTokens = %d, want 370", state.AgentTotals.TotalTokens)
	}
	if entry.LastReportedInputTokens != 250 {
		t.Errorf("after recovery: LastReportedInputTokens = %d, want 250", entry.LastReportedInputTokens)
	}
}

// TestHandleAgentEvent_RateLimits_OutOfOrder verifies that an out-of-order
// event with a non-nil RateLimits payload does NOT overwrite a newer
// snapshot. The monotonic timestamp guard on rate-limit storage ensures the
// most recent payload survives reordering.
func TestHandleAgentEvent_RateLimits_OutOfOrder(t *testing.T) {
	t.Parallel()

	state, _ := newStateWithEntry("MT-12")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tPlus2 := base.Add(2 * time.Second)
	tPlus1 := base.Add(1 * time.Second)

	// Newer event arrives first at T+2s.
	HandleAgentEvent(state, "MT-12", domain.AgentEvent{
		Type:       domain.EventNotification,
		Timestamp:  tPlus2,
		RateLimits: map[string]any{"a": 1},
	}, slog.Default())

	if state.AgentRateLimits == nil {
		t.Fatal("AgentRateLimits = nil after first delivery")
	}
	if got := state.AgentRateLimits.Data["a"]; got != 1 {
		t.Errorf("after newer event: Data[\"a\"] = %v, want 1", got)
	}

	// Older event arrives second at T+1s — must NOT overwrite.
	HandleAgentEvent(state, "MT-12", domain.AgentEvent{
		Type:       domain.EventNotification,
		Timestamp:  tPlus1,
		RateLimits: map[string]any{"b": 2},
	}, slog.Default())

	if got := state.AgentRateLimits.Data["a"]; got != 1 {
		t.Errorf("after out-of-order event: Data[\"a\"] = %v, want 1 (must not be overwritten)", got)
	}
	if _, exists := state.AgentRateLimits.Data["b"]; exists {
		t.Error("after out-of-order event: Data[\"b\"] exists, want absent (older event must not replace newer snapshot)")
	}
	if !state.AgentRateLimits.ReceivedAt.Equal(tPlus2) {
		t.Errorf("after out-of-order event: ReceivedAt = %v, want %v", state.AgentRateLimits.ReceivedAt, tPlus2)
	}
}

// debugLogger returns a *slog.Logger that writes text records at Debug level
// to buf. Use buf.String() after the test action to inspect log output.
func debugLogger(t *testing.T, buf *bytes.Buffer) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestHandleAgentEvent_DebugLogging verifies that HandleAgentEvent emits
// Debug-level structured log lines containing the expected context fields
// for each event category: session_started, token_usage, turn finalization,
// and generic events. Also verifies that unknown-issue events log issue_id.
func TestHandleAgentEvent_DebugLogging(t *testing.T) {
	t.Parallel()

	const issueID = "LOG-1"
	const identifier = "LOG-1-ident"

	t.Run("session_started logs issue_id, issue_identifier, session_id", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := debugLogger(t, &buf)

		state, entry := newStateWithEntry(issueID)
		entry.Identifier = identifier

		HandleAgentEvent(state, issueID, domain.AgentEvent{
			Type:      domain.EventSessionStarted,
			Timestamp: time.Now().UTC(),
			SessionID: "sess-42",
		}, logger)

		out := buf.String()
		for _, want := range []string{"issue_id=" + issueID, "issue_identifier=" + identifier, "session_id=sess-42", "event_type=session_started"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output missing %q\ngot: %s", want, out)
			}
		}
	})

	t.Run("token_usage logs delta fields", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := debugLogger(t, &buf)

		state, entry := newStateWithEntry(issueID)
		entry.Identifier = identifier

		HandleAgentEvent(state, issueID, domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		}, logger)

		out := buf.String()
		for _, want := range []string{"issue_id=" + issueID, "issue_identifier=" + identifier, "delta_input=100", "delta_output=50", "delta_total=150"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output missing %q\ngot: %s", want, out)
			}
		}
	})

	t.Run("turn_completed logs turn_count", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := debugLogger(t, &buf)

		state, entry := newStateWithEntry(issueID)
		entry.Identifier = identifier

		HandleAgentEvent(state, issueID, domain.AgentEvent{
			Type:      domain.EventTurnCompleted,
			Timestamp: time.Now().UTC(),
		}, logger)

		out := buf.String()
		for _, want := range []string{"issue_id=" + issueID, "issue_identifier=" + identifier, "turn_count=1"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output missing %q\ngot: %s", want, out)
			}
		}
	})

	t.Run("unknown issue logs issue_id and event_type", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := debugLogger(t, &buf)

		state := NewState(5000, 4, nil, AgentTotals{})

		HandleAgentEvent(state, "GHOST-1", domain.AgentEvent{
			Type:      domain.EventNotification,
			Timestamp: time.Now().UTC(),
		}, logger)

		out := buf.String()
		for _, want := range []string{"issue_id=GHOST-1", "event_type=notification"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output missing %q\ngot: %s", want, out)
			}
		}
	})
}
