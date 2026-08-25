package persistence

import (
	"context"
	"testing"
)

func TestMigrate_Migration014AppliesFromVersion13(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT    NOT NULL
		)`); err != nil {
		t.Fatalf("create schema_migrations table: %v", err)
	}
	for _, m := range migrations {
		if m.Version > 13 {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
	}

	var versionBefore int
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&versionBefore); err != nil {
		t.Fatalf("query schema version before Migrate: %v", err)
	}
	if versionBefore != 13 {
		t.Fatalf("schema version before Migrate = %d, want 13", versionBefore)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from version 13: %v", err)
	}

	var versionAfter int
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&versionAfter); err != nil {
		t.Fatalf("query schema version after Migrate: %v", err)
	}
	if versionAfter != 15 {
		t.Errorf("schema version after Migrate = %d, want 15", versionAfter)
	}

	var tableName string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'parked_issues'`,
	).Scan(&tableName); err != nil {
		t.Fatalf("parked_issues table not found after migration 014: %v", err)
	}
}

func TestUpsertParkedIssue_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	entry := ParkedIssue{
		IssueID:      "ISS-1",
		Identifier:   "PROJ-1",
		DisplayID:    "PROJ-1-display",
		Reason:       "agent_blocked",
		ParkedState:  "In Progress",
		Label:        "needs-human",
		LabelApplied: false,
		ParkedAt:     "2026-08-17T00:00:00Z",
	}
	if err := s.UpsertParkedIssue(ctx, entry); err != nil {
		t.Fatalf("UpsertParkedIssue: %v", err)
	}

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListParkedIssues() length = %d, want 1", len(rows))
	}
	if rows[0] != entry {
		t.Errorf("ListParkedIssues()[0] = %+v, want %+v", rows[0], entry)
	}
}

func TestUpsertParkedIssue_RewritesExistingRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	const issueID = "ISS-2"

	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: issueID, Identifier: "PROJ-2", Reason: "agent_blocked",
		ParkedState: "In Progress", Label: "needs-human", ParkedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue (initial): %v", err)
	}

	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: issueID, Identifier: "PROJ-2", Reason: "handoff_absence",
		ParkedState: "Blocked", Label: "operator-needed", LabelApplied: true,
		ParkedAt: "2026-08-17T01:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue (rewrite): %v", err)
	}

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListParkedIssues() length = %d, want 1 (rewrite, not a duplicate row)", len(rows))
	}
	if rows[0].Reason != "handoff_absence" || !rows[0].LabelApplied {
		t.Errorf("ListParkedIssues()[0] = %+v, want the rewritten row", rows[0])
	}
}

func TestMarkParkedIssueLabelApplied_SetsFlag(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	const issueID = "ISS-3"

	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: issueID, Identifier: "PROJ-3", Reason: "agent_blocked",
		ParkedState: "In Progress", Label: "needs-human", ParkedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue: %v", err)
	}

	if err := s.MarkParkedIssueLabelApplied(ctx, issueID); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied: %v", err)
	}

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if len(rows) != 1 || !rows[0].LabelApplied {
		t.Errorf("ListParkedIssues() = %+v, want LabelApplied = true", rows)
	}
}

func TestMarkParkedIssueLabelApplied_AbsentRowIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.MarkParkedIssueLabelApplied(ctx, "MISSING"); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied on absent row returned error: %v", err)
	}
}

func TestDeleteParkedIssue_RemovesRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	const issueID = "ISS-4"

	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: issueID, Identifier: "PROJ-4", Reason: "agent_blocked",
		ParkedState: "In Progress", Label: "needs-human", ParkedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue: %v", err)
	}

	if err := s.DeleteParkedIssue(ctx, issueID); err != nil {
		t.Fatalf("DeleteParkedIssue: %v", err)
	}

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListParkedIssues() length = %d, want 0 after delete", len(rows))
	}
}

func TestDeleteParkedIssue_AbsentRowIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.DeleteParkedIssue(ctx, "MISSING"); err != nil {
		t.Fatalf("DeleteParkedIssue on absent row returned error: %v", err)
	}
}

func TestListParkedIssues_EmptyReturnsEmptySliceNotNil(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if rows == nil {
		t.Error("ListParkedIssues() = nil, want an empty non-nil slice")
	}
	if len(rows) != 0 {
		t.Errorf("ListParkedIssues() length = %d, want 0", len(rows))
	}
}

func TestListParkedIssues_MultipleRowsIndependent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: "ISS-5", Identifier: "PROJ-5", Reason: "agent_blocked",
		ParkedState: "In Progress", Label: "needs-human", ParkedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue(ISS-5): %v", err)
	}
	if err := s.UpsertParkedIssue(ctx, ParkedIssue{
		IssueID: "ISS-6", Identifier: "PROJ-6", Reason: "handoff_absence",
		ParkedState: "Blocked", Label: "needs-human", ParkedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue(ISS-6): %v", err)
	}

	if err := s.DeleteParkedIssue(ctx, "ISS-5"); err != nil {
		t.Fatalf("DeleteParkedIssue(ISS-5): %v", err)
	}

	rows, err := s.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}
	if len(rows) != 1 || rows[0].IssueID != "ISS-6" {
		t.Errorf("ListParkedIssues() = %+v, want only ISS-6 to remain", rows)
	}
}
