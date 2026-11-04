package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

func scannedTurns(t *testing.T, s *Store) []storage.RunTurnRow {
	t.Helper()
	var rows []storage.RunTurnRow
	err := s.ScanRunTurnsRange(context.Background(), nil, nil, func(r storage.RunTurnRow) error {
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		t.Fatalf("ScanRunTurnsRange(nil, nil): %v", err)
	}
	return rows
}

func TestRunTurns_SurviveReopenAndEventPruning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sortie.db")
	cost := 0.5
	run := runWithDispatch("d1", "2026-03-19T10:00:30Z")
	run.Turns = []storage.RunTurn{
		{
			Seq: 1, EndedAt: time.Date(2026, 3, 19, 10, 0, 10, 123456789, time.UTC), TokensMeasured: true,
			ToolCalls: 3, Errors: 1,
			Models: map[string]storage.ModelTokens{
				"model-a": {InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 400, CacheWriteTokens: 100},
				"model-b": {InputTokens: 50, OutputTokens: 10},
			},
			EstimatedCostUSD: &cost,
			InputCostUSD:     0.125, OutputCostUSD: 0.25, CacheReadCostUSD: 0.0625, CacheWriteCostUSD: 0.0625,
		},
		{Seq: 2, EndedAt: time.Date(2026, 3, 19, 10, 0, 20, 0, time.UTC), ToolCalls: 1},
	}

	first := mustOpenFileStore(t, path)
	stored, err := first.AppendRunHistoryWithEvents(ctx, run, twoEvents(), unboundedRunEvents)
	if err != nil {
		t.Fatalf("AppendRunHistoryWithEvents(first run): %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := mustOpenFileStore(t, path)
	t.Cleanup(func() { closeStore(t, s) })

	assertScannedTurns := func(step string) {
		t.Helper()
		rows := scannedTurns(t, s)
		var got []storage.RunTurn
		for _, r := range rows {
			if r.RunID != stored.ID {
				t.Fatalf("%s: scanned turn of run %d, want only run %d", step, r.RunID, stored.ID)
			}
			got = append(got, r.Turn)
		}
		if !reflect.DeepEqual(got, run.Turns) {
			t.Errorf("%s: ScanRunTurnsRange turns = %+v, want %+v", step, got, run.Turns)
		}
	}
	assertScannedTurns("after reopen")

	limits := storage.RunEventLimits{MaxAge: 24 * time.Hour, MaxBytes: unboundedRunEvents.MaxBytes, MaxExpiredRuns: 4}
	if _, err := s.AppendRunHistoryWithEvents(ctx, runWithDispatch("d2", "2026-03-20T12:00:00Z"), twoEvents(), limits); err != nil {
		t.Fatalf("AppendRunHistoryWithEvents(second run): %v", err)
	}
	events, err := s.RunEvents(ctx, "d1", 0)
	if err != nil {
		t.Fatalf("RunEvents(d1): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("RunEvents(d1) returned %d events, want 0 after pruning", len(events))
	}

	assertScannedTurns("after pruning")
}
