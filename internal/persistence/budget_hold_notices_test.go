package persistence

import (
	"context"
	"testing"
)

func TestUpsertBudgetHoldNotice_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	notice := BudgetHoldNotice{
		IssueID:   "ISS-1",
		Reason:    "session_budget",
		NoticedAt: "2026-08-17T00:00:00Z",
	}
	if err := s.UpsertBudgetHoldNotice(ctx, notice); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice: %v", err)
	}

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListBudgetHoldNotices() length = %d, want 1", len(rows))
	}
	if rows[0] != notice {
		t.Errorf("ListBudgetHoldNotices()[0] = %+v, want %+v", rows[0], notice)
	}
}

func TestUpsertBudgetHoldNotice_RewritesExistingRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	const issueID = "ISS-2"

	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: issueID, Reason: "session_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice (initial): %v", err)
	}

	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: issueID, Reason: "token_budget", NoticedAt: "2026-08-17T01:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice (rewrite): %v", err)
	}

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListBudgetHoldNotices() length = %d, want 1 (rewrite, not a duplicate row)", len(rows))
	}
	if rows[0].Reason != "token_budget" || rows[0].NoticedAt != "2026-08-17T01:00:00Z" {
		t.Errorf("ListBudgetHoldNotices()[0] = %+v, want the rewritten row", rows[0])
	}
}

func TestDeleteBudgetHoldNotice_RemovesRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	const issueID = "ISS-3"

	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: issueID, Reason: "session_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice: %v", err)
	}

	if err := s.DeleteBudgetHoldNotice(ctx, issueID); err != nil {
		t.Fatalf("DeleteBudgetHoldNotice: %v", err)
	}

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListBudgetHoldNotices() length = %d, want 0 after delete", len(rows))
	}
}

func TestDeleteBudgetHoldNotice_AbsentRowIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.DeleteBudgetHoldNotice(ctx, "MISSING"); err != nil {
		t.Fatalf("DeleteBudgetHoldNotice on absent row returned error: %v", err)
	}
}

func TestDeleteAllBudgetHoldNotices_EmptiesPopulatedTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: "ISS-4", Reason: "session_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice(ISS-4): %v", err)
	}
	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: "ISS-5", Reason: "token_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice(ISS-5): %v", err)
	}

	if err := s.DeleteAllBudgetHoldNotices(ctx); err != nil {
		t.Fatalf("DeleteAllBudgetHoldNotices: %v", err)
	}

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListBudgetHoldNotices() length = %d, want 0 after DeleteAllBudgetHoldNotices", len(rows))
	}
}

func TestDeleteAllBudgetHoldNotices_EmptyTableIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.DeleteAllBudgetHoldNotices(ctx); err != nil {
		t.Fatalf("DeleteAllBudgetHoldNotices on empty table returned error: %v", err)
	}
}

func TestListBudgetHoldNotices_EmptyReturnsEmptySliceNotNil(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if rows == nil {
		t.Error("ListBudgetHoldNotices() = nil, want an empty non-nil slice")
	}
	if len(rows) != 0 {
		t.Errorf("ListBudgetHoldNotices() length = %d, want 0", len(rows))
	}
}

func TestListBudgetHoldNotices_MultipleRowsIndependent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: "ISS-6", Reason: "session_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice(ISS-6): %v", err)
	}
	if err := s.UpsertBudgetHoldNotice(ctx, BudgetHoldNotice{
		IssueID: "ISS-7", Reason: "token_budget", NoticedAt: "2026-08-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertBudgetHoldNotice(ISS-7): %v", err)
	}

	if err := s.DeleteBudgetHoldNotice(ctx, "ISS-6"); err != nil {
		t.Fatalf("DeleteBudgetHoldNotice(ISS-6): %v", err)
	}

	rows, err := s.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices: %v", err)
	}
	if len(rows) != 1 || rows[0].IssueID != "ISS-7" {
		t.Errorf("ListBudgetHoldNotices() = %+v, want only ISS-7 to remain", rows)
	}
}
