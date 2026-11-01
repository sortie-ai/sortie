package orchestrator

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

func mustParseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", value, err)
	}
	return parsed
}

func TestBudgetHoldNoticeAllowed(t *testing.T) {
	t.Parallel()

	t.Run("a fresh state allows exactly maxBudgetHoldNoticesPerWindow calls and refuses the next", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)

		for i := range maxBudgetHoldNoticesPerWindow {
			if !budgetHoldNoticeAllowed(state, now) {
				t.Fatalf("budgetHoldNoticeAllowed() call %d = false, want true (within window budget)", i+1)
			}
		}
		if budgetHoldNoticeAllowed(state, now) {
			t.Error("budgetHoldNoticeAllowed() after the window budget is spent = true, want false")
		}
		if state.BudgetHoldNoticesInWindow != maxBudgetHoldNoticesPerWindow {
			t.Errorf("BudgetHoldNoticesInWindow = %d, want %d (refusal does not consume a slot)", state.BudgetHoldNoticesInWindow, maxBudgetHoldNoticesPerWindow)
		}
	})

	t.Run("a window start older than budgetHoldNoticeWindow opens a fresh window and allows again", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		state.BudgetHoldNoticeWindowStart = time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
		state.BudgetHoldNoticesInWindow = maxBudgetHoldNoticesPerWindow

		now := state.BudgetHoldNoticeWindowStart.Add(budgetHoldNoticeWindow)
		if !budgetHoldNoticeAllowed(state, now) {
			t.Fatal("budgetHoldNoticeAllowed() with an elapsed window = false, want true (fresh window opened)")
		}
		if !state.BudgetHoldNoticeWindowStart.Equal(now) {
			t.Errorf("BudgetHoldNoticeWindowStart = %v, want %v (reset to now)", state.BudgetHoldNoticeWindowStart, now)
		}
		if state.BudgetHoldNoticesInWindow != 1 {
			t.Errorf("BudgetHoldNoticesInWindow = %d, want 1 (the one call just granted)", state.BudgetHoldNoticesInWindow)
		}
	})

	t.Run("a recent window start that is already spent stays closed", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		windowStart := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
		state.BudgetHoldNoticeWindowStart = windowStart
		state.BudgetHoldNoticesInWindow = maxBudgetHoldNoticesPerWindow

		now := windowStart.Add(budgetHoldNoticeWindow - time.Second)
		if budgetHoldNoticeAllowed(state, now) {
			t.Error("budgetHoldNoticeAllowed() with an unelapsed, spent window = true, want false")
		}
		if !state.BudgetHoldNoticeWindowStart.Equal(windowStart) {
			t.Errorf("BudgetHoldNoticeWindowStart = %v, want unchanged %v", state.BudgetHoldNoticeWindowStart, windowStart)
		}
	})
}

// TestReleaseBudgetHoldNotice_RetainsLatchWhenDeleteFails fails if a
// failed durable delete drops the memory entry anyway. Dropping it strands
// the row: the early return would then skip every later delete, and a
// restart would reload the stale row and suppress a notice for a hold that
// genuinely re-formed.
func TestReleaseBudgetHoldNotice_RetainsLatchWhenDeleteFails(t *testing.T) {
	t.Parallel()

	const issueID = "iss-release-retry"
	store := &stubStore{deleteBudgetHoldNoticeErr: fmt.Errorf("db error")}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
	state.BudgetHoldNoticed[issueID] = budgetReasonSession

	releaseBudgetHoldNotice(context.Background(), state, store, issueID, discardLogger())

	if _, ok := state.BudgetHoldNoticed[issueID]; !ok {
		t.Fatalf("BudgetHoldNoticed lost %q after a failed delete, want the entry retained so the delete is retried", issueID)
	}

	store.deleteBudgetHoldNoticeErr = nil
	releaseBudgetHoldNotice(context.Background(), state, store, issueID, discardLogger())

	if _, ok := state.BudgetHoldNoticed[issueID]; ok {
		t.Errorf("BudgetHoldNoticed still holds %q after a successful delete", issueID)
	}
	if len(store.deletedBudgetHoldIDs) != 2 {
		t.Errorf("delete attempts = %d, want 2 (the failure is retried on the next release pass)", len(store.deletedBudgetHoldIDs))
	}
}

// TestReleaseAllBudgetHoldNotices_RetainsMemoryWhenDeleteFails fails if a
// failed bulk delete empties the memory anyway, which would strand every
// row for the same reason the single-issue path must not.
func TestReleaseAllBudgetHoldNotices_RetainsMemoryWhenDeleteFails(t *testing.T) {
	t.Parallel()

	store := &stubStore{deleteAllBudgetHoldErr: fmt.Errorf("db error")}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
	state.BudgetHoldNoticed["iss-a"] = budgetReasonSession
	state.BudgetHoldNoticed["iss-b"] = budgetReasonToken

	releaseAllBudgetHoldNotices(context.Background(), state, store, discardLogger())

	if len(state.BudgetHoldNoticed) != 2 {
		t.Fatalf("BudgetHoldNoticed = %+v after a failed bulk delete, want both entries retained", state.BudgetHoldNoticed)
	}

	store.deleteAllBudgetHoldErr = nil
	releaseAllBudgetHoldNotices(context.Background(), state, store, discardLogger())

	if len(state.BudgetHoldNoticed) != 0 {
		t.Errorf("BudgetHoldNoticed = %+v after a successful bulk delete, want empty", state.BudgetHoldNoticed)
	}
	if store.deleteAllBudgetHoldCalls != 2 {
		t.Errorf("bulk delete calls = %d, want 2 (the failure is retried)", store.deleteAllBudgetHoldCalls)
	}
}

// TestBuildBudgetHoldComment_CrossLaneParity drives the same IssueTokenUsage
// through both notice-posting lanes' real entry construction: the retry
// lane's blockBudget (via HandleRetryTimer) and the rebuild's token arm
// (via Orchestrator.handleTick). It asserts the two resulting entries
// render an identical comment body. The two lanes never see the same
// wall clock, so BudgetAnnounced is pre-seeded identically on both sides
// to freeze ExhaustedAt; buildBudgetHoldComment does not render
// Identifier or DisplayID, so those are free to differ between the
// lanes' fixtures.
func TestBuildBudgetHoldComment_CrossLaneParity(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-PARITY"
	fixedAt := mustParseRFC3339(t, "2026-08-25T09:14:03Z")
	announcement := BudgetAnnouncement{Reason: budgetReasonToken, At: fixedAt}

	retryStore := &mockRetryStore{tokenSum: 128400, tokenSessionCount: 5, tokenStoppedInFlight: 2}
	retryState := retryState(t, issueID, "PROJ-PARITY", 1)
	retryState.BudgetAnnounced[issueID] = announcement
	retryParams := defaultRetryParams(t, retryStore, &mockRetryTracker{})
	retryParams.MaxTokens = 100000

	HandleRetryTimer(retryState, issueID, retryParams)
	retryState.TrackerOpsWg.Wait()

	retryEntry, ok := retryState.BudgetExhausted[issueID]
	if !ok {
		t.Fatal("retry lane: BudgetExhausted entry missing after HandleRetryTimer")
	}

	issue := domain.Issue{ID: issueID, Identifier: "PROJ-PARITY", Title: "t", State: "To Do"}
	rebuildStore := &stubStore{
		tokenExhaustedIDs: []string{issueID},
		tokenExhaustedUsage: map[string]storage.IssueTokenUsage{
			issueID: {TotalTokens: 128400, UncachedTokens: 128400, Sessions: 5, StoppedInFlight: 2},
		},
	}
	rebuildState := NewState(60000, 10, 0, nil, AgentTotals{})
	rebuildState.BudgetAnnounced[issueID] = announcement
	wm := budgetTickConfigTokens(0, 100000)
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	budgetOrchestrator(rebuildState, wm, rebuildStore, tracker).handleTick(context.Background())
	rebuildState.TrackerOpsWg.Wait()

	rebuildEntry, ok := rebuildState.BudgetExhausted[issueID]
	if !ok {
		t.Fatal("rebuild lane: BudgetExhausted entry missing after handleTick")
	}

	retryFacts := budgetHeldNotification(issueID, retryEntry).Message.Facts
	rebuildFacts := budgetHeldNotification(issueID, rebuildEntry).Message.Facts
	if !reflect.DeepEqual(retryFacts, rebuildFacts) {
		t.Errorf("retry-lane facts = %+v, rebuild-lane facts = %+v, want identical facts for the same IssueTokenUsage", retryFacts, rebuildFacts)
	}
	if facts, ok := retryFacts.(domain.BudgetHeldFacts); !ok || facts.StoppedInFlight == nil || *facts.StoppedInFlight != 2 {
		t.Errorf("retry-lane facts = %+v, want StoppedInFlight 2", retryFacts)
	}
}

// Posting the notice twice for one hold, unaffected by this line, is
// covered by TestHandleTick_BudgetHoldNoticeOnce: the durable dedup in
// budget_hold_notices remains the only gate, and this change introduces
// no second one.
