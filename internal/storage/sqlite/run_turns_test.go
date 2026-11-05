package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
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
	run.EstimatedCostUSD, run.CostStatus = new(1.0), "estimated"
	run.ModelUsage = map[string]storage.ModelTokens{"model-a": {InputTokens: 1000, OutputTokens: 200}}
	run.ModelCosts = map[string]storage.ModelCost{"model-a": {EstimatedCostUSD: new(0.25), CostStatus: "estimated", PriceSource: "agent"}}
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
			if r.RunCostUSD == nil || *r.RunCostUSD != 1.0 || r.RunAgentPricedUSD == nil || *r.RunAgentPricedUSD != 0.25 {
				t.Errorf("%s: ScanRunTurnsRange run cost, agent-priced = %v, %v, want 1, 0.25", step, r.RunCostUSD, r.RunAgentPricedUSD)
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

func scannedUntracked(ctx context.Context, s *Store, caps storage.RunHistoryCapabilities, since, until *time.Time) ([]storage.UntrackedRunRow, error) {
	var rows []storage.UntrackedRunRow
	err := s.ScanUntrackedRunsRange(ctx, caps, since, until, func(r storage.UntrackedRunRow) error {
		rows = append(rows, r)
		return nil
	})
	return rows, err
}

func identifiersOf(rows []storage.UntrackedRunRow) []string {
	var identifiers []string
	for _, r := range rows {
		identifiers = append(identifiers, r.Identifier)
	}
	return identifiers
}

func fixtureRun(identifier, status, completedAt string) storage.RunHistory {
	run := runWithDispatch("d-"+identifier, completedAt)
	run.Identifier, run.Status = identifier, status
	return run
}

func TestScanUntrackedRunsRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := openTestStore(t)
	migrateOrFatal(t, s)

	rich := fixtureRun("old-a", "succeeded", "2020-01-10T10:00:00Z")
	rich.IssueTitle, rich.RuleName, rich.TokensMeasured, rich.UnaccountedTurns = "Fix it", "implement", true, 1
	rich.InputTokens, rich.OutputTokens, rich.TotalTokens, rich.CacheReadTokens, rich.CacheWriteTokens = 1000, 100, 1100, 200, 300
	rich.EstimatedCostUSD, rich.CostStatus = new(2.0), "estimated"
	rich.ModelUsage = map[string]storage.ModelTokens{"model-a": {InputTokens: 1000, OutputTokens: 100}}
	rich.ModelCosts = map[string]storage.ModelCost{"model-a": {EstimatedCostUSD: new(0.5), CostStatus: "estimated", PriceSource: "agent"}}
	tracked := fixtureRun("old-turns", "succeeded", "2020-01-25T10:00:00Z")
	tracked.Turns = []storage.RunTurn{{Seq: 1, EndedAt: time.Date(2020, 1, 25, 9, 59, 0, 0, time.UTC)}}
	fixture := []storage.RunHistory{
		rich,
		fixtureRun("old-ci", "ci_failed", "2020-01-20T10:00:00Z"),
		tracked,
		fixtureRun("old-b", "failed", "2020-02-10T10:00:00Z"),
		fixtureRun("recorded-later", "succeeded", "2099-01-01T00:00:00Z"),
	}
	for _, run := range fixture {
		if _, err := s.AppendRunHistoryWithEvents(ctx, run, nil, unboundedRunEvents); err != nil {
			t.Fatalf("AppendRunHistoryWithEvents(%s): %v", run.Identifier, err)
		}
	}
	caps, err := s.RunHistoryCapabilities(ctx)
	if err != nil {
		t.Fatalf("RunHistoryCapabilities: %v", err)
	}
	withoutTurnTables := caps
	withoutTurnTables.HasRunTurns = false
	january11, february10 := time.Date(2020, 1, 11, 0, 0, 0, 0, time.UTC), time.Date(2020, 2, 10, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		caps         storage.RunHistoryCapabilities
		since, until *time.Time
		want         []string
	}{
		{name: "runs recorded before turn records", caps: caps, want: []string{"old-a", "old-b"}},
		{name: "since is inclusive", caps: caps, since: new(time.Date(2020, 1, 10, 10, 0, 0, 0, time.UTC)), want: []string{"old-a", "old-b"}},
		{name: "until is exclusive", caps: caps, until: &february10, want: []string{"old-a"}},
		{name: "since after the first run", caps: caps, since: &january11, want: []string{"old-b"}},
		{name: "database without turn tables", caps: withoutTurnTables, want: []string{"old-a", "old-turns", "old-b", "recorded-later"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows, err := scannedUntracked(ctx, s, tt.caps, tt.since, tt.until)

			if err != nil {
				t.Fatalf("ScanUntrackedRunsRange(%v, %v) = %v, want nil", tt.since, tt.until, err)
			}
			if got := identifiersOf(rows); !slices.Equal(got, tt.want) {
				t.Errorf("ScanUntrackedRunsRange(%v, %v) visited %v, want %v", tt.since, tt.until, got, tt.want)
			}
		})
	}

	t.Run("projects the recorded figures", func(t *testing.T) {
		t.Parallel()

		rows, err := scannedUntracked(ctx, s, caps, nil, nil)
		if err != nil || len(rows) == 0 {
			t.Fatalf("ScanUntrackedRunsRange(nil, nil) = %d rows, %v, want rows", len(rows), err)
		}

		got := rows[0]
		want := storage.UntrackedRunRow{
			RunID: got.RunID, IssueID: rich.IssueID, Identifier: "old-a", IssueTitle: "Fix it", DispatchID: "d-old-a",
			RuleName: "implement", AgentAdapter: rich.AgentAdapter, Status: "succeeded",
			CompletedAt: time.Date(2020, 1, 10, 10, 0, 0, 0, time.UTC), TokensMeasured: true,
			InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, CacheReadTokens: 200, CacheWriteTokens: 300,
			EstimatedCostUSD: new(2.0), CostStatus: "estimated", UnaccountedTurns: 1, AgentPricedUSD: new(0.5),
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ScanUntrackedRunsRange(nil, nil)[0] = %+v, want %+v", got, want)
		}
	})

	t.Run("counts a run finished in the second the migration ran", func(t *testing.T) {
		t.Parallel()

		upgraded := openTestStore(t)
		migrateOrFatal(t, upgraded)
		appliedAt, err := upgraded.runTurnsTrackedSince(ctx)
		if err != nil {
			t.Fatalf("runTurnsTrackedSince: %v", err)
		}
		run := fixtureRun("same-second", "succeeded", appliedAt.Add(500*time.Millisecond).Format(time.RFC3339Nano))
		if _, err := upgraded.AppendRunHistoryWithEvents(ctx, run, nil, unboundedRunEvents); err != nil {
			t.Fatalf("AppendRunHistoryWithEvents: %v", err)
		}

		rows, err := scannedUntracked(ctx, upgraded, caps, nil, nil)

		if got := identifiersOf(rows); err != nil || !slices.Equal(got, []string{"same-second"}) {
			t.Errorf("ScanUntrackedRunsRange(nil, nil) = %v, %v, want [same-second], nil", got, err)
		}
	})

	t.Run("fails on a creation time that does not parse", func(t *testing.T) {
		t.Parallel()

		broken := openTestStore(t)
		migrateOrFatal(t, broken)
		if _, err := broken.db.ExecContext(ctx, `UPDATE schema_migrations SET applied_at = 'yesterday' WHERE version = ?`, runTurnsMigration); err != nil {
			t.Fatalf("corrupt applied_at: %v", err)
		}

		_, err := scannedUntracked(ctx, broken, caps, nil, nil)

		if err == nil {
			t.Errorf("ScanUntrackedRunsRange(nil, nil) = nil, want an error")
		}
	})
}
