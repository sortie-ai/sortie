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

	// A successful run resets the sequence; only the later absence counts.
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "succeeded", "")
	appendAbsenceSequenceRun(t, store, "ISS-RESET", "failed", handoffAbsenceError())

	// Cancellation is not positive evidence and therefore does not reset.
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "cancelled", "shutdown")
	appendAbsenceSequenceRun(t, store, "ISS-CANCEL", "failed", handoffAbsenceError())

	// Repeated productive/empty alternation always leaves only the current
	// one-absence sequence, so it can never accumulate toward three.
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "succeeded", "")
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "succeeded", "")
	appendAbsenceSequenceRun(t, store, "ISS-ALTERNATING", "failed", handoffAbsenceError())

	// Similar text that does not begin with the reserved marker is ordinary
	// failure data and must not be counted.
	appendAbsenceSequenceRun(t, store, "ISS-OTHER", "failed", "worker error: handoff withheld: no output")

	ids := []string{"ISS-FAILURES", "ISS-RESET", "ISS-CANCEL", "ISS-ALTERNATING", "ISS-OTHER", "ISS-NONE"}
	got, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), ids)
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts: %v", err)
	}

	want := map[string]int{
		"ISS-FAILURES":    2,
		"ISS-RESET":       1,
		"ISS-CANCEL":      2,
		"ISS-ALTERNATING": 1,
	}
	for _, id := range ids {
		if got[id] != want[id] {
			t.Errorf("count[%s] = %d, want %d", id, got[id], want[id])
		}
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
