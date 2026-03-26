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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
			}, slog.Default(), nil)

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
			}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

	// 2. First token usage: {100, 50, 150}.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, slog.Default(), nil)

	// 3. Second token usage: {200, 100, 300} — delta {+100, +50, +150}.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	}, slog.Default(), nil)

	// 4. Duplicate token usage — zero delta.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
	}, slog.Default(), nil)

	// 5. Turn completed.
	HandleAgentEvent(state, issueID, domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: ts,
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)
	HandleAgentEvent(state, "B-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 50, TotalTokens: 250},
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)
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
	}, slog.Default(), nil)
	if !entry.LastAgentTimestamp.Equal(tPlus2) {
		t.Errorf("after event A: LastAgentTimestamp = %v, want %v", entry.LastAgentTimestamp, tPlus2)
	}

	// Event B at T+1s (out-of-order) — must NOT regress the timestamp.
	HandleAgentEvent(state, "MT-10", domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: tPlus1,
	}, slog.Default(), nil)
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
	}, slog.Default(), nil)
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
	}, slog.Default(), nil)

	if entry.AgentInputTokens != 200 {
		t.Errorf("after 1st: AgentInputTokens = %d, want 200", entry.AgentInputTokens)
	}

	// Second report: regression {150, 80, 230} — delta clamped to zero,
	// baselines must NOT regress.
	HandleAgentEvent(state, "MT-11", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 150, OutputTokens: 80, TotalTokens: 230},
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
	}, slog.Default(), nil)

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
		}, logger, nil)

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
		}, logger, nil)

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
		}, logger, nil)

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
		}, logger, nil)

		out := buf.String()
		for _, want := range []string{"issue_id=GHOST-1", "event_type=notification"} {
			if !strings.Contains(out, want) {
				t.Errorf("log output missing %q\ngot: %s", want, out)
			}
		}
	})
}

// --- Extended Token Metrics Tests ---

// TestHandleAgentEvent_CacheReadTokens_Delta verifies the delta algorithm
// for CacheReadTokens: cumulative deltas, zero on duplicate, clamped on
// regression, accumulated into AgentTotals.
func TestHandleAgentEvent_CacheReadTokens_Delta(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("CR-1")
	ts := time.Now().UTC()

	// First report: cache_read=500.
	HandleAgentEvent(state, "CR-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CacheReadTokens: 500},
	}, slog.Default(), nil)

	if entry.CacheReadTokens != 500 {
		t.Errorf("after 1st: CacheReadTokens = %d, want 500", entry.CacheReadTokens)
	}
	if entry.LastReportedCacheReadTokens != 500 {
		t.Errorf("after 1st: LastReportedCacheReadTokens = %d, want 500", entry.LastReportedCacheReadTokens)
	}
	if state.AgentTotals.CacheReadTokens != 500 {
		t.Errorf("after 1st: AgentTotals.CacheReadTokens = %d, want 500", state.AgentTotals.CacheReadTokens)
	}

	// Second report: cumulative 800 → delta +300.
	HandleAgentEvent(state, "CR-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300, CacheReadTokens: 800},
	}, slog.Default(), nil)

	if entry.CacheReadTokens != 800 {
		t.Errorf("after 2nd: CacheReadTokens = %d, want 800", entry.CacheReadTokens)
	}
	if state.AgentTotals.CacheReadTokens != 800 {
		t.Errorf("after 2nd: AgentTotals.CacheReadTokens = %d, want 800", state.AgentTotals.CacheReadTokens)
	}

	// Duplicate report: 800 again → zero delta.
	HandleAgentEvent(state, "CR-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300, CacheReadTokens: 800},
	}, slog.Default(), nil)

	if entry.CacheReadTokens != 800 {
		t.Errorf("after dup: CacheReadTokens = %d, want 800 (no double-count)", entry.CacheReadTokens)
	}

	// Regression: 600 → delta clamped to zero, baseline stays at 800.
	HandleAgentEvent(state, "CR-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300, CacheReadTokens: 600},
	}, slog.Default(), nil)

	if entry.CacheReadTokens != 800 {
		t.Errorf("after regression: CacheReadTokens = %d, want 800", entry.CacheReadTokens)
	}
	if entry.LastReportedCacheReadTokens != 800 {
		t.Errorf("after regression: LastReportedCacheReadTokens = %d, want 800", entry.LastReportedCacheReadTokens)
	}
}

// TestHandleAgentEvent_ModelTracking verifies model name tracking: the
// event's Model is stored; when empty, the last-known model persists;
// RequestsByModel is incremented per token_usage.
func TestHandleAgentEvent_ModelTracking(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("MOD-1")
	ts := time.Now().UTC()

	// First report with model.
	HandleAgentEvent(state, "MOD-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		Model:     "claude-sonnet-4-20250514",
	}, slog.Default(), nil)

	if entry.ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("ModelName = %q, want %q", entry.ModelName, "claude-sonnet-4-20250514")
	}
	if entry.RequestsByModel == nil {
		t.Fatal("RequestsByModel = nil, want non-nil")
	}
	if entry.RequestsByModel["claude-sonnet-4-20250514"] != 1 {
		t.Errorf("RequestsByModel[sonnet] = %d, want 1", entry.RequestsByModel["claude-sonnet-4-20250514"])
	}

	// Second report with empty model → falls back to last-known.
	HandleAgentEvent(state, "MOD-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
		Model:     "",
	}, slog.Default(), nil)

	if entry.ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("after empty model: ModelName = %q, want %q", entry.ModelName, "claude-sonnet-4-20250514")
	}
	if entry.RequestsByModel["claude-sonnet-4-20250514"] != 2 {
		t.Errorf("RequestsByModel[sonnet] = %d, want 2", entry.RequestsByModel["claude-sonnet-4-20250514"])
	}

	// Third report with a different model.
	HandleAgentEvent(state, "MOD-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 300, OutputTokens: 150, TotalTokens: 450},
		Model:     "claude-opus-4-20250514",
	}, slog.Default(), nil)

	if entry.ModelName != "claude-opus-4-20250514" {
		t.Errorf("ModelName = %q, want %q", entry.ModelName, "claude-opus-4-20250514")
	}
	if entry.RequestsByModel["claude-opus-4-20250514"] != 1 {
		t.Errorf("RequestsByModel[opus] = %d, want 1", entry.RequestsByModel["claude-opus-4-20250514"])
	}
	if entry.RequestsByModel["claude-sonnet-4-20250514"] != 2 {
		t.Errorf("RequestsByModel[sonnet] = %d, want 2 (unchanged)", entry.RequestsByModel["claude-sonnet-4-20250514"])
	}
}

// TestHandleAgentEvent_APIRequestCount verifies that APIRequestCount
// increments on every token_usage event and is unaffected by non-token events.
func TestHandleAgentEvent_APIRequestCount(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("ARC-1")
	ts := time.Now().UTC()

	// Non-token events should not increment.
	HandleAgentEvent(state, "ARC-1", domain.AgentEvent{
		Type: domain.EventNotification, Timestamp: ts,
	}, slog.Default(), nil)
	HandleAgentEvent(state, "ARC-1", domain.AgentEvent{
		Type: domain.EventSessionStarted, Timestamp: ts, SessionID: "s1",
	}, slog.Default(), nil)

	if entry.APIRequestCount != 0 {
		t.Errorf("after non-token events: APIRequestCount = %d, want 0", entry.APIRequestCount)
	}

	// Three token_usage events → count 3.
	for i := range 3 {
		HandleAgentEvent(state, "ARC-1", domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: ts,
			Usage:     domain.TokenUsage{InputTokens: int64((i + 1) * 100)},
		}, slog.Default(), nil)
	}

	if entry.APIRequestCount != 3 {
		t.Errorf("after 3 token_usage: APIRequestCount = %d, want 3", entry.APIRequestCount)
	}
}

// TestHandleAgentEvent_ModelTracking_NoModel verifies that when no model
// is ever reported, ModelName stays empty and RequestsByModel remains nil.
func TestHandleAgentEvent_ModelTracking_NoModel(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("NM-1")
	ts := time.Now().UTC()

	HandleAgentEvent(state, "NM-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: ts,
		Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, slog.Default(), nil)

	if entry.ModelName != "" {
		t.Errorf("ModelName = %q, want empty", entry.ModelName)
	}
	if entry.RequestsByModel != nil {
		t.Errorf("RequestsByModel = %v, want nil", entry.RequestsByModel)
	}
	if entry.APIRequestCount != 1 {
		t.Errorf("APIRequestCount = %d, want 1", entry.APIRequestCount)
	}
}

// TestHandleAgentEvent_CacheReadTokens_DebugLog verifies that the
// delta_cache_read field appears in the debug log for token_usage events.
func TestHandleAgentEvent_CacheReadTokens_DebugLog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := debugLogger(t, &buf)

	state, entry := newStateWithEntry("CRL-1")
	entry.Identifier = "CRL-1-ident"

	HandleAgentEvent(state, "CRL-1", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Usage:     domain.TokenUsage{InputTokens: 100, CacheReadTokens: 250},
	}, logger, nil)

	out := buf.String()
	if !strings.Contains(out, "delta_cache_read=250") {
		t.Errorf("log output missing delta_cache_read=250\ngot: %s", out)
	}
}

// TestHandleAgentEvent_TwoSessions_CacheReadTotals verifies cache-read
// delta accumulation across independent sessions.
func TestHandleAgentEvent_TwoSessions_CacheReadTotals(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running["A-1"] = &RunningEntry{}
	state.Running["B-1"] = &RunningEntry{}
	ts := time.Now().UTC()

	HandleAgentEvent(state, "A-1", domain.AgentEvent{
		Type: domain.EventTokenUsage, Timestamp: ts,
		Usage: domain.TokenUsage{CacheReadTokens: 1000},
	}, slog.Default(), nil)
	HandleAgentEvent(state, "B-1", domain.AgentEvent{
		Type: domain.EventTokenUsage, Timestamp: ts,
		Usage: domain.TokenUsage{CacheReadTokens: 2000},
	}, slog.Default(), nil)

	if state.AgentTotals.CacheReadTokens != 3000 {
		t.Errorf("AgentTotals.CacheReadTokens = %d, want 3000", state.AgentTotals.CacheReadTokens)
	}
}

// --- Per-session timing breakdown tests ---

// TestHandleAgentEvent_APIDurationMS_Accumulates verifies that
// APIDurationMS on any event type accumulates into entry.APITimeMs.
func TestHandleAgentEvent_APIDurationMS_Accumulates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		eventType  domain.AgentEventType
		durations  []int64
		wantAPIMs  int64
		wantTurnUp bool // whether TurnCount should increment
	}{
		{
			name:      "token_usage events accumulate API time",
			eventType: domain.EventTokenUsage,
			durations: []int64{500, 300},
			wantAPIMs: 800,
		},
		{
			name:       "turn_completed carries API time",
			eventType:  domain.EventTurnCompleted,
			durations:  []int64{1500},
			wantAPIMs:  1500,
			wantTurnUp: true,
		},
		{
			name:       "turn_failed carries API time",
			eventType:  domain.EventTurnFailed,
			durations:  []int64{200, 800},
			wantAPIMs:  1000,
			wantTurnUp: true,
		},
		{
			name:      "zero APIDurationMS ignored",
			eventType: domain.EventNotification,
			durations: []int64{0, 0},
			wantAPIMs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, entry := newStateWithEntry("API-1")
			ts := time.Now().UTC()

			for _, dur := range tt.durations {
				HandleAgentEvent(state, "API-1", domain.AgentEvent{
					Type:          tt.eventType,
					Timestamp:     ts,
					APIDurationMS: dur,
				}, slog.Default(), nil)
			}

			if entry.APITimeMs != tt.wantAPIMs {
				t.Errorf("APITimeMs = %d, want %d", entry.APITimeMs, tt.wantAPIMs)
			}
		})
	}
}

// TestHandleAgentEvent_ToolResult_Accumulates verifies that tool_result
// events with ToolDurationMS > 0 accumulate into entry.ToolTimeMs and
// do NOT increment TurnCount.
func TestHandleAgentEvent_ToolResult_Accumulates(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("TOOL-1")
	ts := time.Now().UTC()

	HandleAgentEvent(state, "TOOL-1", domain.AgentEvent{
		Type:           domain.EventToolResult,
		Timestamp:      ts,
		ToolName:       "Read",
		ToolDurationMS: 120,
	}, slog.Default(), nil)

	HandleAgentEvent(state, "TOOL-1", domain.AgentEvent{
		Type:           domain.EventToolResult,
		Timestamp:      ts,
		ToolName:       "Write",
		ToolDurationMS: 350,
	}, slog.Default(), nil)

	// Zero-duration tool_result should be ignored.
	HandleAgentEvent(state, "TOOL-1", domain.AgentEvent{
		Type:           domain.EventToolResult,
		Timestamp:      ts,
		ToolName:       "Bash",
		ToolDurationMS: 0,
	}, slog.Default(), nil)

	if entry.ToolTimeMs != 470 {
		t.Errorf("ToolTimeMs = %d, want 470", entry.ToolTimeMs)
	}
	if entry.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0 (tool_result must not increment)", entry.TurnCount)
	}
}

// TestHandleAgentEvent_ToolResult_OnlyToolResultAccumulatesToolTime
// verifies that ToolDurationMS is only accumulated for EventToolResult,
// not for other event types that happen to carry the field.
func TestHandleAgentEvent_ToolResult_OnlyToolResultAccumulatesToolTime(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("TOOLONLY-1")
	ts := time.Now().UTC()

	// A notification event with ToolDurationMS should NOT contribute.
	HandleAgentEvent(state, "TOOLONLY-1", domain.AgentEvent{
		Type:           domain.EventNotification,
		Timestamp:      ts,
		ToolDurationMS: 999,
	}, slog.Default(), nil)

	if entry.ToolTimeMs != 0 {
		t.Errorf("ToolTimeMs = %d, want 0 (only tool_result events accumulate)", entry.ToolTimeMs)
	}
}

// TestHandleAgentEvent_CombinedTimingAccumulation verifies that API time
// and tool time accumulate independently across mixed event sequences.
func TestHandleAgentEvent_CombinedTimingAccumulation(t *testing.T) {
	t.Parallel()

	state, entry := newStateWithEntry("COMBO-1")
	ts := time.Now().UTC()

	// Turn-completed with API time.
	HandleAgentEvent(state, "COMBO-1", domain.AgentEvent{
		Type:          domain.EventTurnCompleted,
		Timestamp:     ts,
		APIDurationMS: 1500,
	}, slog.Default(), nil)

	// Tool result with tool time.
	HandleAgentEvent(state, "COMBO-1", domain.AgentEvent{
		Type:           domain.EventToolResult,
		Timestamp:      ts,
		ToolName:       "Read",
		ToolDurationMS: 200,
	}, slog.Default(), nil)

	// Token usage with API time.
	HandleAgentEvent(state, "COMBO-1", domain.AgentEvent{
		Type:          domain.EventTokenUsage,
		Timestamp:     ts,
		APIDurationMS: 500,
	}, slog.Default(), nil)

	if entry.APITimeMs != 2000 {
		t.Errorf("APITimeMs = %d, want 2000", entry.APITimeMs)
	}
	if entry.ToolTimeMs != 200 {
		t.Errorf("ToolTimeMs = %d, want 200", entry.ToolTimeMs)
	}
	if entry.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", entry.TurnCount)
	}
}

// TestHandleAgentEvent_ToolCallMetric verifies that HandleAgentEvent
// increments the IncToolCalls metric for EventToolResult events with
// non-empty ToolName, using the correct result label based on ToolError.
func TestHandleAgentEvent_ToolCallMetric(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		toolName   string
		toolError  bool
		wantCalls  int
		wantTool   string
		wantResult string
	}{
		{
			name:       "success tool call",
			toolName:   "Bash",
			toolError:  false,
			wantCalls:  1,
			wantTool:   "Bash",
			wantResult: "success",
		},
		{
			name:       "error tool call",
			toolName:   "Bash",
			toolError:  true,
			wantCalls:  1,
			wantTool:   "Bash",
			wantResult: "error",
		},
		{
			name:      "empty ToolName skips metric",
			toolName:  "",
			toolError: false,
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spy := &spyMetrics{}
			state, _ := newStateWithEntry("TOOLM-1")

			HandleAgentEvent(state, "TOOLM-1", domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      time.Now().UTC(),
				ToolName:       tt.toolName,
				ToolDurationMS: 100,
				ToolError:      tt.toolError,
			}, slog.Default(), spy)

			if len(spy.toolCalls) != tt.wantCalls {
				t.Fatalf("IncToolCalls called %d times, want %d", len(spy.toolCalls), tt.wantCalls)
			}
			if tt.wantCalls > 0 {
				got := spy.toolCalls[0]
				if got.tool != tt.wantTool {
					t.Errorf("IncToolCalls tool = %q, want %q", got.tool, tt.wantTool)
				}
				if got.result != tt.wantResult {
					t.Errorf("IncToolCalls result = %q, want %q", got.result, tt.wantResult)
				}
			}
		})
	}
}
