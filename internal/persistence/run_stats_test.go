package persistence

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// createLegacyRunHistoryTable creates a run_history table carrying only
// the migration-001 columns, with no schema_migrations tracking and none
// of the optional column groups.
func createLegacyRunHistoryTable(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `CREATE TABLE run_history (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		issue_id      TEXT    NOT NULL,
		identifier    TEXT    NOT NULL,
		attempt       INTEGER NOT NULL,
		agent_adapter TEXT    NOT NULL,
		workspace     TEXT    NOT NULL,
		started_at    TEXT    NOT NULL,
		completed_at  TEXT    NOT NULL,
		status        TEXT    NOT NULL,
		error         TEXT
	)`); err != nil {
		t.Fatalf("create migration-001 run_history table: %v", err)
	}
}

// createPartialRunHistoryTable creates a run_history table carrying
// turns_completed and review_metadata but neither rule_name/template_id
// nor the four token columns, matching this repository's own .sortie.db
// shape (schema version 9).
func createPartialRunHistoryTable(t *testing.T, s *Store) {
	t.Helper()
	createLegacyRunHistoryTable(t, s)
	ctx := context.Background()
	for _, stmt := range []string{
		"ALTER TABLE run_history ADD COLUMN turns_completed INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE run_history ADD COLUMN review_metadata TEXT",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("alter run_history (%s): %v", stmt, err)
		}
	}
}

func TestRunHistoryCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("fully migrated store reports all four flags true", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)

		caps, err := s.RunHistoryCapabilities(context.Background())
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.HasTurnsCompleted || !caps.HasReviewMetadata || !caps.HasRuleRouting || !caps.HasTokens || !caps.HasTokenMeasurement {
			t.Errorf("RunHistoryCapabilities() = %+v, want all five flags true", caps)
		}
		if !caps.Full() {
			t.Errorf("Full() = false, want true for a fully migrated store")
		}
	})

	t.Run("migration-001-only table reports all five flags false", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		createLegacyRunHistoryTable(t, s)

		caps, err := s.RunHistoryCapabilities(context.Background())
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if caps.HasTurnsCompleted || caps.HasReviewMetadata || caps.HasRuleRouting || caps.HasTokens || caps.HasTokenMeasurement {
			t.Errorf("RunHistoryCapabilities() = %+v, want all five flags false", caps)
		}
		if caps.Full() {
			t.Errorf("Full() = true, want false for a migration-001-only table")
		}
	})

	t.Run("tokens present but tokens_measured absent reports the fifth flag false", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateToVersion(t, s, 11)

		caps, err := s.RunHistoryCapabilities(context.Background())
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.HasTokens {
			t.Errorf("HasTokens = false, want true (migration 011 applied)")
		}
		if caps.HasTokenMeasurement {
			t.Errorf("HasTokenMeasurement = true, want false (migration 012 not applied)")
		}
		if caps.Full() {
			t.Errorf("Full() = true, want false when tokens_measured is absent")
		}
	})

	t.Run("turns and review metadata present, rule routing and tokens absent", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		createPartialRunHistoryTable(t, s)

		caps, err := s.RunHistoryCapabilities(context.Background())
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.HasTurnsCompleted {
			t.Errorf("HasTurnsCompleted = false, want true")
		}
		if !caps.HasReviewMetadata {
			t.Errorf("HasReviewMetadata = false, want true")
		}
		if caps.HasRuleRouting {
			t.Errorf("HasRuleRouting = true, want false")
		}
		if caps.HasTokens {
			t.Errorf("HasTokens = true, want false")
		}
		if caps.Full() {
			t.Errorf("Full() = true, want false for the mixed-migration shape")
		}
	})

	t.Run("no run_history table names run_history in the error", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)

		_, err := s.RunHistoryCapabilities(context.Background())
		if err == nil {
			t.Fatal("RunHistoryCapabilities() error = nil, want an error naming run_history")
		}
		if !strings.Contains(err.Error(), "run_history") {
			t.Errorf("RunHistoryCapabilities() error = %q, want to contain %q", err.Error(), "run_history")
		}
	})
}

// runAt returns a minimal RunHistory whose StartedAt and CompletedAt are
// both completedAt, for tests that only care about the completed_at
// range filter.
func runAt(id, completedAt string) RunHistory {
	return RunHistory{
		IssueID:      id,
		Identifier:   id,
		Attempt:      1,
		AgentAdapter: "mock",
		Workspace:    "/tmp/ws",
		StartedAt:    completedAt,
		CompletedAt:  completedAt,
		Status:       "succeeded",
	}
}

func TestScanRunHistoryRange(t *testing.T) {
	t.Parallel()

	t.Run("half-open range: since inclusive, until exclusive", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		appendOrFatal(t, s, runAt("before", "2026-01-01T00:05:00Z"))
		appendOrFatal(t, s, runAt("at-since", "2026-01-02T00:05:00Z"))
		appendOrFatal(t, s, runAt("at-until", "2026-01-03T00:05:00Z"))
		appendOrFatal(t, s, runAt("after", "2026-01-04T00:05:00Z"))

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}

		since, err := time.Parse(time.RFC3339, "2026-01-02T00:05:00Z")
		if err != nil {
			t.Fatalf("time.Parse(since): %v", err)
		}
		until, err := time.Parse(time.RFC3339, "2026-01-03T00:05:00Z")
		if err != nil {
			t.Fatalf("time.Parse(until): %v", err)
		}

		var visited []string
		if err := s.ScanRunHistoryRange(ctx, caps, &since, &until, func(row RunStatsRow) error {
			visited = append(visited, row.CompletedAt)
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}

		want := []string{"2026-01-02T00:05:00Z"}
		if !slices.Equal(visited, want) {
			t.Errorf("ScanRunHistoryRange(since=%s, until=%s) visited = %v, want %v",
				since.Format(time.RFC3339), until.Format(time.RFC3339), visited, want)
		}
	})

	t.Run("both bounds unbounded returns every row", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		appendOrFatal(t, s, runAt("r1", "2026-01-01T00:00:00Z"))
		appendOrFatal(t, s, runAt("r2", "2026-01-02T00:00:00Z"))
		appendOrFatal(t, s, runAt("r3", "2026-01-03T00:00:00Z"))

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}

		var count int
		if err := s.ScanRunHistoryRange(ctx, caps, nil, nil, func(RunStatsRow) error {
			count++
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}
		if count != 3 {
			t.Errorf("ScanRunHistoryRange(nil, nil) visited %d rows, want 3", count)
		}
	})

	t.Run("rows visited in ascending id order; a visit error aborts and is returned unmodified", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		for _, tag := range []string{"order-1", "order-2", "order-3"} {
			appendOrFatal(t, s, RunHistory{
				IssueID: tag, Identifier: tag, Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z", Status: tag,
			})
		}

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}

		sentinelErr := errors.New("stop at second row")
		var visited []string
		err = s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
			visited = append(visited, row.Status)
			if row.Status == "order-2" {
				return sentinelErr
			}
			return nil
		})

		if !errors.Is(err, sentinelErr) {
			t.Fatalf("ScanRunHistoryRange() error = %v, want errors.Is match on %v", err, sentinelErr)
		}
		want := []string{"order-1", "order-2"}
		if !slices.Equal(visited, want) {
			t.Errorf("ScanRunHistoryRange visited = %v, want %v (ascending id order, aborted after the error)", visited, want)
		}
	})

	t.Run("full projection populates TokensMeasured", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		measured := runAt("measured-row", "2026-01-01T00:00:00Z")
		measured.Status = "measured"
		measured.TokensMeasured = true
		appendOrFatal(t, s, measured)

		unmeasured := runAt("unmeasured-row", "2026-01-02T00:00:00Z")
		unmeasured.Status = "unmeasured"
		unmeasured.TokensMeasured = false
		appendOrFatal(t, s, unmeasured)

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.Full() {
			t.Fatalf("caps.Full() = false, want true for a fully migrated store")
		}

		got := make(map[string]bool)
		if err := s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
			got[row.Status] = row.TokensMeasured
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}
		if !got["measured"] {
			t.Error(`RunStatsRow{Status: "measured"}.TokensMeasured = false, want true`)
		}
		if got["unmeasured"] {
			t.Error(`RunStatsRow{Status: "unmeasured"}.TokensMeasured = true, want false`)
		}
	})

	t.Run("base projection against the legacy table leaves optional fields zero", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		createLegacyRunHistoryTable(t, s)
		ctx := context.Background()

		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_history (issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status)
			 VALUES ('ISS-1', 'PROJ-1', 1, 'mock', '/tmp/ws', '2026-01-01T00:00:00Z', '2026-01-01T00:05:00Z', 'succeeded')`,
		); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if caps.Full() {
			t.Fatalf("caps.Full() = true, want false for the legacy table")
		}

		var rows []RunStatsRow
		if err := s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
			rows = append(rows, row)
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("ScanRunHistoryRange visited %d rows, want 1", len(rows))
		}

		got := rows[0]
		if got.Status != "succeeded" {
			t.Errorf("RunStatsRow.Status = %q, want %q", got.Status, "succeeded")
		}
		if got.AgentAdapter != "mock" {
			t.Errorf("RunStatsRow.AgentAdapter = %q, want %q", got.AgentAdapter, "mock")
		}
		if got.TurnsCompleted != 0 {
			t.Errorf("RunStatsRow.TurnsCompleted = %d, want 0 (base projection)", got.TurnsCompleted)
		}
		if got.ReviewMetadata != nil {
			t.Errorf("RunStatsRow.ReviewMetadata = %v, want nil (base projection)", *got.ReviewMetadata)
		}
		if got.RuleName != "" {
			t.Errorf("RunStatsRow.RuleName = %q, want empty (base projection)", got.RuleName)
		}
		if got.TemplateID != "" {
			t.Errorf("RunStatsRow.TemplateID = %q, want empty (base projection)", got.TemplateID)
		}
		if got.InputTokens != 0 || got.OutputTokens != 0 || got.TotalTokens != 0 || got.CacheReadTokens != 0 {
			t.Errorf("RunStatsRow token fields = %+v, want all zero (base projection)", got)
		}
	})
}
