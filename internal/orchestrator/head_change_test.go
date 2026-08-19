package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/persistence"
)

// headChangeStore is a minimal ReconcileStore double for classifyHeadChange
// tests. Only CountWorkerRunsCompletedSince carries configurable behavior;
// every other method is a no-op, since classifyHeadChange consults no
// other store method.
type headChangeStore struct {
	unsupportedReactionObservationStore

	count int
	err   error

	calls int
}

var _ ReconcileStore = (*headChangeStore)(nil)

func (s *headChangeStore) SaveRetryEntry(context.Context, persistence.RetryEntry) error { return nil }
func (s *headChangeStore) DeleteRetryEntry(context.Context, string) error               { return nil }
func (s *headChangeStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}
func (s *headChangeStore) UpsertReactionFingerprint(context.Context, string, string, string) error {
	return nil
}
func (s *headChangeStore) GetReactionFingerprint(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (s *headChangeStore) MarkReactionDispatched(context.Context, string, string) error { return nil }
func (s *headChangeStore) DeleteReactionFingerprint(context.Context, string, string) error {
	return nil
}

func (s *headChangeStore) CountWorkerRunsCompletedSince(context.Context, string, time.Time) (int, error) {
	s.calls++
	return s.count, s.err
}

// headChangeParams returns a ReconcileParams wired with store for
// classifyHeadChange tests.
func headChangeParams(store ReconcileStore) ReconcileParams {
	return ReconcileParams{
		Store: store,
		Ctx:   context.Background(),
	}
}

// headChangeBaseTime is a fixed reference time for classifyHeadChange
// tests.
var headChangeBaseTime = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

func TestClassifyHeadChange(t *testing.T) {
	t.Parallel()

	t.Run("HeadRecordedAt zero answers unknown without querying the store", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		store := &headChangeStore{count: 0, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-1"}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime, context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (zero HeadRecordedAt)", got, headChangeUnknown)
		}
		if store.calls != 0 {
			t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 0 (zero boundary short-circuits)", store.calls)
		}
	})

	t.Run("no live session and no qualifying run_history row answers notOurs", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		store := &headChangeStore{count: 0, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-2", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeNotOurs {
			t.Errorf("classifyHeadChange() = %q, want %q", got, headChangeNotOurs)
		}
		if store.calls != 1 {
			t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 1", store.calls)
		}
	})

	t.Run("a qualifying succeeded run_history row answers unknown", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		store := &headChangeStore{count: 1, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-3", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (positive worker-run count)", got, headChangeUnknown)
		}
	})

	t.Run("a live state.Running entry answers unknown without querying the store", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["ISS-HC-4"] = &RunningEntry{Identifier: "ISS-HC-4-ident"}
		store := &headChangeStore{count: 0, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-4", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (live Running entry)", got, headChangeUnknown)
		}
		if store.calls != 0 {
			t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 0 (Running entry short-circuits)", store.calls)
		}
	})

	t.Run("a live state.RetryAttempts entry answers unknown without querying the store", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["ISS-HC-5"] = &RetryEntry{IssueID: "ISS-HC-5"}
		store := &headChangeStore{count: 0, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-5", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (live RetryAttempts entry)", got, headChangeUnknown)
		}
		if store.calls != 0 {
			t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 0 (RetryAttempts entry short-circuits)", store.calls)
		}
	})

	t.Run("a state.Claimed entry answers unknown without querying the store", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.Claimed["ISS-HC-6"] = struct{}{}
		store := &headChangeStore{count: 0, err: nil}
		pending := &PendingReaction{IssueID: "ISS-HC-6", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (Claimed entry)", got, headChangeUnknown)
		}
		if store.calls != 0 {
			t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 0 (Claimed entry short-circuits)", store.calls)
		}
	})

	t.Run("a store error answers unknown", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		store := &headChangeStore{err: errors.New("db unavailable")}
		pending := &PendingReaction{IssueID: "ISS-HC-7", HeadRecordedAt: headChangeBaseTime}

		got := classifyHeadChange(state, headChangeParams(store), pending, headChangeBaseTime.Add(time.Hour), context.Background())

		if got != headChangeUnknown {
			t.Errorf("classifyHeadChange() = %q, want %q (store error)", got, headChangeUnknown)
		}
	})

	t.Run("the shared unsupportedReactionObservationStore default answers unknown, never notOurs", func(t *testing.T) {
		t.Parallel()

		var defaultStore unsupportedReactionObservationStore
		count, err := defaultStore.CountWorkerRunsCompletedSince(context.Background(), "any", headChangeBaseTime)
		if err == nil {
			t.Error("unsupportedReactionObservationStore.CountWorkerRunsCompletedSince returned nil error, want non-nil (must not default to notOurs in every unrelated test)")
		}
		if count != 0 {
			t.Errorf("unsupportedReactionObservationStore.CountWorkerRunsCompletedSince count = %d, want 0", count)
		}
	})
}
