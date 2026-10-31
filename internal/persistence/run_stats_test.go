package persistence

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

	t.Run("configured settings columns follow migration 021 and stay outside the schema tier", func(t *testing.T) {
		t.Parallel()

		current, before := openTestStore(t), openTestStore(t)
		migrateOrFatal(t, current)
		migrateToVersion(t, before, 20)

		for s, want := range map[*Store]bool{current: true, before: false} {
			caps, err := s.RunHistoryCapabilities(context.Background())
			if err != nil {
				t.Fatalf("RunHistoryCapabilities: %v", err)
			}
			if !caps.Full() || caps.HasConfiguredSettings != want || caps.ModelBreakdown() != want {
				t.Errorf("RunHistoryCapabilities() = %+v, ModelBreakdown() = %v, want Full() and HasConfiguredSettings, ModelBreakdown() both %v", caps, caps.ModelBreakdown(), want)
			}
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

func TestRunHistoryCapabilities_Breakdowns(t *testing.T) {
	t.Parallel()

	full := RunHistoryCapabilities{HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true, HasTokens: true, HasTokenMeasurement: true}
	withSettings := full
	withSettings.HasConfiguredSettings = true
	withChains := withSettings
	withChains.HasStageChains = true
	withTrigger := withChains
	withTrigger.HasTrigger = true
	chainsWithoutSettings := full
	chainsWithoutSettings.HasStageChains = true
	recorded := withTrigger
	recorded.HasFirstAfterHop, recorded.HasAgentBlocked, recorded.HasRecordedCost = true, true, true
	withSkills := recorded
	withSkills.HasSkills = true
	skillsWithoutCost := withTrigger
	skillsWithoutCost.HasSkills = true
	tests := []struct {
		name        string
		caps        RunHistoryCapabilities
		wantModel   bool
		wantChain   bool
		wantTrigger bool
		wantSkills  bool
	}{
		{"recorded cost and the skills column", withSkills, true, true, true, true},
		{"recorded cost without the skills column", recorded, true, true, true, false},
		{"skills column without recorded cost", skillsWithoutCost, true, true, true, false},
		{"full tier with settings, stage chains and trigger", withTrigger, true, true, true, false},
		{"full tier with configured settings and stage chains", withChains, true, true, false, false},
		{"full tier with configured settings", withSettings, true, false, false, false},
		{"full tier without configured settings", full, false, false, false, false},
		{"stage chains without configured settings", chainsWithoutSettings, false, false, false, false},
		{"trigger without stage chains", RunHistoryCapabilities{HasTrigger: true}, false, false, false, false},
		{"configured settings without the full tier", RunHistoryCapabilities{HasConfiguredSettings: true, HasStageChains: true, HasTrigger: true}, false, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.caps.ModelBreakdown(); got != tt.wantModel {
				t.Errorf("%+v.ModelBreakdown() = %v, want %v", tt.caps, got, tt.wantModel)
			}
			if got := tt.caps.ChainBreakdown(); got != tt.wantChain {
				t.Errorf("%+v.ChainBreakdown() = %v, want %v", tt.caps, got, tt.wantChain)
			}
			if got := tt.caps.TriggerBreakdown(); got != tt.wantTrigger {
				t.Errorf("%+v.TriggerBreakdown() = %v, want %v", tt.caps, got, tt.wantTrigger)
			}
			if got := tt.caps.SkillBreakdown(); got != tt.wantSkills {
				t.Errorf("%+v.SkillBreakdown() = %v, want %v", tt.caps, got, tt.wantSkills)
			}
		})
	}
}

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

	t.Run("full projection populates ReviewMetadata", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		meta := `{"enabled":true,"iterations":[],"total_iterations":1,"final_verdict":"pass","cap_reached":false}`
		row := runAt("with-meta", "2026-01-01T00:00:00Z")
		row.ReviewMetadata = &meta
		appendOrFatal(t, s, row)

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.Full() {
			t.Fatalf("caps.Full() = false, want true for a fully migrated store")
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
		if rows[0].ReviewMetadata == nil || *rows[0].ReviewMetadata != meta {
			t.Errorf("RunStatsRow.ReviewMetadata = %v, want %q", rows[0].ReviewMetadata, meta)
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

	t.Run("full projection with HasCacheWriteTokens reads a non-zero cache-write figure", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		row := runAt("cache-write-row", "2026-01-01T00:00:00Z")
		row.CacheReadTokens = 154053
		row.CacheWriteTokens = 38948
		row.TokensMeasured = true
		appendOrFatal(t, s, row)

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		if !caps.HasCacheWriteTokens {
			t.Fatalf("HasCacheWriteTokens = false, want true for a freshly migrated store")
		}
		if !caps.Full() {
			t.Fatalf("caps.Full() = false, want true (HasCacheWriteTokens is not part of the schema-tier decision)")
		}

		var got []RunStatsRow
		if err := s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
			got = append(got, row)
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ScanRunHistoryRange visited %d rows, want 1", len(got))
		}
		if got[0].CacheWriteTokens != 38948 {
			t.Errorf("RunStatsRow.CacheWriteTokens = %d, want 38948", got[0].CacheWriteTokens)
		}
	})

	t.Run("each run is visited once with its parts ordered by model and its unaccounted turns", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		withParts := runAt("with-parts", "2026-01-01T00:00:00Z")
		withParts.DispatchID, withParts.UnaccountedTurns = new("dispatch-1"), 3
		withParts.TotalTokens, withParts.TokensMeasured = 330, true
		withParts.UsageCompleteness, withParts.UsageProvenance = UsageLowerBound, "journal:records,output:live"
		withParts.ReportedAmounts = []ReportedAmount{
			{Unit: "credits", Model: "model-b", Amount: 2},
			{Unit: "USD", Model: "model-a", Amount: 1},
			{Unit: "credits", Model: "model-a", Amount: 3},
		}
		withParts.ModelUsage = map[string]ModelTokens{
			"model-b": {InputTokens: 200, OutputTokens: 20},
			"model-a": {InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CacheWriteTokens: 5},
		}
		withParts.ModelCosts = map[string]ModelCost{
			"model-b": {CostStatus: "unpriced"},
			"model-a": {EstimatedCostUSD: new(1.5), CostStatus: "estimated"},
		}
		if _, err := s.AppendRunHistoryWithEvents(ctx, withParts, nil, unboundedRunEvents); err != nil {
			t.Fatalf("AppendRunHistoryWithEvents: %v", err)
		}
		appendOrFatal(t, s, runAt("no-parts", "2026-01-02T00:00:00Z"))

		caps, err := s.RunHistoryCapabilities(ctx)
		if err != nil {
			t.Fatalf("RunHistoryCapabilities: %v", err)
		}
		var got []RunStatsRow
		if err := s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
			got = append(got, row)
			return nil
		}); err != nil {
			t.Fatalf("ScanRunHistoryRange: %v", err)
		}

		if len(got) != 2 || got[0].IssueID != "with-parts" || got[1].IssueID != "no-parts" {
			t.Fatalf("ScanRunHistoryRange visited %+v, want with-parts then no-parts, once each", got)
		}
		wantParts := []RunModelPart{
			{Model: "model-a", Tokens: ModelTokens{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CacheWriteTokens: 5}, EstimatedCostUSD: new(1.5), CostStatus: "estimated"},
			{Model: "model-b", Tokens: ModelTokens{InputTokens: 200, OutputTokens: 20}, CostStatus: "unpriced"},
		}
		if !reflect.DeepEqual(got[0].Parts, wantParts) {
			t.Errorf("RunStatsRow{with-parts}.Parts = %+v, want %+v", got[0].Parts, wantParts)
		}
		if got[0].UnaccountedTurns != 3 || got[0].TotalTokens != 330 {
			t.Errorf("RunStatsRow{with-parts} UnaccountedTurns, TotalTokens = %d, %d, want 3, 330", got[0].UnaccountedTurns, got[0].TotalTokens)
		}
		if got[0].UsageCompleteness != UsageLowerBound || got[0].UsageProvenance != "journal:records,output:live" || !slices.Equal(got[0].ReportedUnits, []string{"USD", "credits"}) {
			t.Errorf("RunStatsRow{with-parts} UsageCompleteness, UsageProvenance, ReportedUnits = %q, %q, %q, want %q, %q, %q",
				got[0].UsageCompleteness, got[0].UsageProvenance, got[0].ReportedUnits, UsageLowerBound, "journal:records,output:live", []string{"USD", "credits"})
		}
		if got[1].Parts != nil || got[1].UnaccountedTurns != 0 || got[1].ReportedUnits != nil {
			t.Errorf("RunStatsRow{no-parts} Parts, UnaccountedTurns, ReportedUnits = %+v, %d, %q, want nil, 0, nil", got[1].Parts, got[1].UnaccountedTurns, got[1].ReportedUnits)
		}
	})

	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, s *Store)
	}{
		{"a database before migration 050", func(t *testing.T, s *Store) { migrateToVersion(t, s, 49) }},
		{"a database whose reported amounts table is missing", func(t *testing.T, s *Store) {
			migrateOrFatal(t, s)
			if _, err := s.db.ExecContext(context.Background(), "DROP TABLE run_reported_amounts"); err != nil {
				t.Fatalf("drop run_reported_amounts: %v", err)
			}
		}},
	} {
		t.Run(tt.name+" reads empty completeness, provenance and units", func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			tt.setup(t, s)
			insertRunHistoryRow(t, s, "no-provenance", "succeeded", true)
			ctx := context.Background()
			caps, err := s.RunHistoryCapabilities(ctx)
			if err != nil {
				t.Fatalf("RunHistoryCapabilities: %v", err)
			}

			var got []RunStatsRow
			err = s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
				got = append(got, row)
				return nil
			})

			if err != nil || caps.UsageProvenance() || len(got) != 1 {
				t.Fatalf("ScanRunHistoryRange = %d rows, UsageProvenance %v, error %v, want 1, false, nil", len(got), caps.UsageProvenance(), err)
			}
			if got[0].UsageCompleteness != "" || got[0].UsageProvenance != "" || got[0].ReportedUnits != nil {
				t.Errorf("RunStatsRow UsageCompleteness, UsageProvenance, ReportedUnits = %q, %q, %q, want empty", got[0].UsageCompleteness, got[0].UsageProvenance, got[0].ReportedUnits)
			}
		})
	}
}

func TestScanRunHistoryRange_OptionalColumns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, s *Store)
		want  []string
	}{
		{"filled when the model breakdown is available", func(t *testing.T, s *Store) {
			migrateOrFatal(t, s)
			row := runAt("with-model", "2026-01-01T00:00:00Z")
			row.ConfiguredModel, row.DisplayID = "provider/cheap", "PROJ-1"
			row.ChainID, row.StagePrevious, row.StageResult = "chain-1", "specify", "advanced"
			row.Trigger, row.ReactionKind = new("ci_fix"), "review"
			appendOrFatal(t, s, row)
			appendOrFatal(t, s, runAt("no-model", "2026-01-02T00:00:00Z"))
			empty := runAt("empty-trigger", "2026-01-03T00:00:00Z")
			empty.Trigger = new("")
			appendOrFatal(t, s, empty)
			first := runAt("first-after-hop", "2026-01-04T00:00:00Z")
			first.FirstAfterHop = true
			appendOrFatal(t, s, first)
		}, []string{
			"with-model|provider/cheap|with-model|PROJ-1|chain-1|specify|advanced|\"ci_fix\"|review|false|false|false|true",
			"no-model||no-model|||||<nil>||false|false|false|true",
			"empty-trigger||empty-trigger|||||\"\"||false|false|false|true",
			"first-after-hop||first-after-hop|||||<nil>||true|false|false|true",
		}},
		{"origin left empty without error on a database upgraded from before migration 028", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 27)
			insertBareRun(t, s, "pre-028")
			migrateOrFatal(t, s)
		}, []string{"pre-028||MT-1|||||<nil>||false|false|false|true"}},
		{"first_after_hop left false without error on a database before migration 032", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 31)
			insertBareRun(t, s, "pre-032")
		}, []string{"pre-032||MT-1|||||<nil>||false|false|false|false"}},
		{"trigger left nil without error on a database before migration 028", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 27)
			insertBareRun(t, s, "pre-028")
		}, []string{"pre-028||MT-1|||||<nil>||false|false|false|false"}},
		{"left empty without error on a database before migration 021", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 20)
			insertBareRun(t, s, "old")
		}, []string{"old|||||||<nil>||false|false|false|false"}},
		{"chain columns left empty without error on a database before migration 024", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 23)
			insertBareRun(t, s, "old")
		}, []string{"old|||||||<nil>||false|false|false|false"}},
		{"funnel columns hidden without error on a database before migration 037", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 36)
			insertBareRun(t, s, "pre-037")
		}, []string{"pre-037||MT-1|||||<nil>||false|false|false|false"}},
		{"a row from before migration 037 reads as not blocked after migrating", func(t *testing.T, s *Store) {
			migrateToVersion(t, s, 36)
			insertBareRun(t, s, "pre-037")
			migrateOrFatal(t, s)
		}, []string{"pre-037||MT-1|||||<nil>||false|false|false|true"}},
		{"blocked run and handoff-withheld failure flagged by status and error prefix", func(t *testing.T, s *Store) {
			migrateOrFatal(t, s)
			withError := func(id, completedAt, status string, errText *string) RunHistory {
				row := runAt(id, completedAt)
				row.Status, row.Error = status, errText
				return row
			}
			blocked := runAt("blocked", "2026-01-01T00:00:00Z")
			blocked.AgentBlocked = true
			appendOrFatal(t, s, blocked)
			appendOrFatal(t, s, withError("withheld", "2026-01-02T00:00:00Z", "failed", new(HandoffAbsenceErrorPrefix+"no label")))
			appendOrFatal(t, s, withError("other-error", "2026-01-03T00:00:00Z", "failed", new("agent crashed")))
			appendOrFatal(t, s, withError("no-error", "2026-01-04T00:00:00Z", "failed", nil))
			appendOrFatal(t, s, withError("not-failed", "2026-01-05T00:00:00Z", "succeeded", new(HandoffAbsenceErrorPrefix+"no label")))
		}, []string{
			"blocked||blocked|||||<nil>||false|true|false|true",
			"withheld||withheld|||||<nil>||false|false|true|true",
			"other-error||other-error|||||<nil>||false|false|false|true",
			"no-error||no-error|||||<nil>||false|false|false|true",
			"not-failed||not-failed|||||<nil>||false|false|false|true",
		}},
		{"skills read back as stored, and NULL as nil, beside the model column", func(t *testing.T, s *Store) {
			migrateOrFatal(t, s)
			stored := runAt("stored", "2026-01-01T00:00:00Z")
			stored.ConfiguredModel, stored.Skills = "provider/cheap", new(`[{"skill":"plan"}]`)
			unrecorded := runAt("unrecorded", "2026-01-02T00:00:00Z")
			unrecorded.ConfiguredModel = "provider/cheap"
			appendOrFatal(t, s, stored)
			appendOrFatal(t, s, unrecorded)
		}, []string{
			`stored|provider/cheap|stored|||||<nil>||false|false|false|true|[{"skill":"plan"}]`,
			"unrecorded|provider/cheap|unrecorded|||||<nil>||false|false|false|true",
		}},
		{"a legacy table with only the base columns still scans", createLegacyRunHistoryTable, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			tt.setup(t, s)
			ctx := context.Background()
			caps, err := s.RunHistoryCapabilities(ctx)
			if err != nil {
				t.Fatalf("RunHistoryCapabilities: %v", err)
			}

			var got []string
			err = s.ScanRunHistoryRange(ctx, caps, nil, nil, func(row RunStatsRow) error {
				trigger := "<nil>"
				if row.Trigger != nil {
					trigger = strconv.Quote(*row.Trigger)
				}
				entry := strings.Join([]string{row.IssueID, row.ConfiguredModel, row.Identifier, row.DisplayID, row.ChainID, row.StagePrevious, row.StageResult, trigger, row.ReactionKind, strconv.FormatBool(row.FirstAfterHop), strconv.FormatBool(row.AgentBlocked), strconv.FormatBool(row.HandoffWithheld), strconv.FormatBool(caps.FunnelBreakdown())}, "|")
				if row.Skills != nil {
					entry += "|" + *row.Skills
				}
				got = append(got, entry)
				return nil
			})

			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("ScanRunHistoryRange optional column values, error = %q, %v, want %q, nil", got, err, tt.want)
			}
		})
	}
}
