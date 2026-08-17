package persistence

import (
	"context"
	"testing"
)

func appendAbsenceSequenceRun(t *testing.T, store *Store, issueID, status, errorText string) {
	t.Helper()
	var runErr *string
	if errorText != "" {
		runErr = &errorText
	}
	appendOrFatal(t, store, RunHistory{
		IssueID:      issueID,
		Identifier:   issueID,
		Attempt:      1,
		AgentAdapter: "mock",
		Workspace:    "/tmp/" + issueID,
		StartedAt:    "2026-08-17T00:00:00Z",
		CompletedAt:  "2026-08-17T00:01:00Z",
		Status:       status,
		Error:        runErr,
	})
}

func resetOrFatal(t *testing.T, store *Store, issueID string) {
	t.Helper()
	if err := store.ResetHandoffAbsenceSequence(context.Background(), issueID); err != nil {
		t.Fatalf("ResetHandoffAbsenceSequence(%q): %v", issueID, err)
	}
}

func absenceCountOrFatal(t *testing.T, store *Store, issueID string) int {
	t.Helper()
	counts, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts(%q): %v", issueID, err)
	}
	return counts[issueID]
}

func handoffAbsenceError() string {
	return HandoffAbsenceErrorPrefix + "absence of work observed under observed policy"
}

func TestQueryConsecutiveHandoffAbsenceCounts(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)

	// Non-absence failures do not pretend to reset a sequence.
	appendAbsenceSequenceRun(t, store, "ISS-FAILURES", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-FAILURES", "failed", "agent transport failed")
	appendAbsenceSequenceRun(t, store, "ISS-FAILURES", "failed", handoffAbsenceError())

	// A work-observed run resets the sequence; only the later absence counts.
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "succeeded", "")
	resetOrFatal(t, store, "ISS-RESET")
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "failed", handoffAbsenceError())

	// A successful run that carries no work-observed verdict - a blocked soft
	// stop, a reaction run, a run under the off policy - does not reset the
	// sequence, so absences either side of it accumulate toward the ceiling.
	appendAbsenceSequenceRun(t, store, "ISS-SUCCEEDED", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-SUCCEEDED", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-SUCCEEDED", "succeeded", "")
	appendAbsenceSequenceRun(t, store, "ISS-SUCCEEDED", "failed", handoffAbsenceError())

	// Cancellation is not positive evidence and therefore does not reset.
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "cancelled", "shutdown")
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "failed", handoffAbsenceError())

	// Repeated productive/empty alternation always leaves only the current
	// one-absence sequence, so it can never accumulate toward three.
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "succeeded", "")
	resetOrFatal(t, store, "ISS-ALTERNATING")
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "succeeded", "")
	resetOrFatal(t, store, "ISS-ALTERNATING")
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())

	// Similar text that does not begin with the reserved marker is ordinary
	// failure data and must not be counted.
	appendAbsenceSequenceRun(t, store, "ISS-OTHER", "failed", "worker error: handoff withheld: no output")

	ids := []string{"ISS-FAILURES", "ISS-RESET", "ISS-SUCCEEDED", "ISS-CANCEL", "ISS-ALTERNATING", "ISS-OTHER", "ISS-NONE"}
	got, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), ids)
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts: %v", err)
	}

	want := map[string]int{
		"ISS-FAILURES":    2,
		"ISS-RESET":       1,
		"ISS-SUCCEEDED":   3,
		"ISS-CANCEL":      2,
		"ISS-ALTERNATING": 1,
	}
	for _, id := range ids {
		if got[id] != want[id] {
			t.Errorf("count[%s] = %d, want %d", id, got[id], want[id])
		}
	}
}

func TestResetHandoffAbsenceSequence(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)

	appendAbsenceSequenceRun(t, store, "ISS-1", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-1", "failed", handoffAbsenceError())
	if got := absenceCountOrFatal(t, store, "ISS-1"); got != 2 {
		t.Fatalf("count before reset = %d, want 2", got)
	}

	resetOrFatal(t, store, "ISS-1")
	if got := absenceCountOrFatal(t, store, "ISS-1"); got != 0 {
		t.Errorf("count after reset = %d, want 0", got)
	}

	// A later absence starts a fresh sequence, and a second reset ends that
	// one too rather than being ignored as already recorded.
	appendAbsenceSequenceRun(t, store, "ISS-1", "failed", handoffAbsenceError())
	if got := absenceCountOrFatal(t, store, "ISS-1"); got != 1 {
		t.Errorf("count after later absence = %d, want 1", got)
	}
	resetOrFatal(t, store, "ISS-1")
	if got := absenceCountOrFatal(t, store, "ISS-1"); got != 0 {
		t.Errorf("count after second reset = %d, want 0", got)
	}
}

func TestResetHandoffAbsenceSequenceWithoutRunHistoryRow(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)

	// A work-observed run whose own history row could not be persisted still
	// clears the absences recorded before it, and does not swallow later ones.
	appendAbsenceSequenceRun(t, store, "ISS-NOROW", "failed", handoffAbsenceError())
	resetOrFatal(t, store, "ISS-NOROW")
	if got := absenceCountOrFatal(t, store, "ISS-NOROW"); got != 0 {
		t.Errorf("count after reset = %d, want 0", got)
	}

	appendAbsenceSequenceRun(t, store, "ISS-NOROW", "failed", handoffAbsenceError())
	if got := absenceCountOrFatal(t, store, "ISS-NOROW"); got != 1 {
		t.Errorf("count after later absence = %d, want 1", got)
	}
}

func TestResetHandoffAbsenceSequenceError(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.ResetHandoffAbsenceSequence(context.Background(), "ISS-1"); err == nil {
		t.Fatal("ResetHandoffAbsenceSequence on closed store returned nil error")
	}
}

func TestQueryConsecutiveHandoffAbsenceCountsEmptyInput(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)
	got, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), nil)
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("result = %#v, want empty non-nil map", got)
	}
}

func TestQueryConsecutiveHandoffAbsenceCountsQueryError(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	migrateOrFatal(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), []string{"ISS-1"}); err == nil {
		t.Fatal("QueryConsecutiveHandoffAbsenceCounts on closed store returned nil error")
	}
}
