package persistence

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer closeStore(t, s)
}

func TestOpen_WALEnabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// WAL requires a file-backed database; in-memory always reports "memory".
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	defer closeStore(t, s)

	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

// TestOpen_BasicCRUD is a smoke test for the modernc.org/sqlite driver.
// It verifies that basic SQL operations work through the configured connection.
// This test will be superseded by CRUD method tests in tasks 2.3–2.5.
func TestOpen_BasicCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer closeStore(t, s)

	// Create.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE test_items (
		id   INTEGER PRIMARY KEY,
		name TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// Insert.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO test_items (id, name) VALUES (1, 'alpha')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Select.
	var id int
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT id, name FROM test_items WHERE id = 1`).Scan(&id, &name); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if id != 1 || name != "alpha" {
		t.Fatalf("got (%d, %q), want (1, %q)", id, name, "alpha")
	}

	// Delete.
	res, err := s.db.ExecContext(ctx, `DELETE FROM test_items WHERE id = 1`)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
}

func TestSaveAndLoadRetryEntry(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry := RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    1,
		DueAtMs:    1000,
		Error:      nil,
	}
	if err := s.SaveRetryEntry(ctx, entry); err != nil {
		t.Fatalf("SaveRetryEntry: %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.IssueID != "ISS-1" {
		t.Errorf("IssueID = %q, want %q", got.IssueID, "ISS-1")
	}
	if got.Identifier != "PROJ-1" {
		t.Errorf("Identifier = %q, want %q", got.Identifier, "PROJ-1")
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.DueAtMs != 1000 {
		t.Errorf("DueAtMs = %d, want 1000", got.DueAtMs)
	}
	if got.Error != nil {
		t.Errorf("Error = %v, want nil", got.Error)
	}
}

func TestSaveRetryEntry_Upsert(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry1 := RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    1,
		DueAtMs:    1000,
		Error:      nil,
	}
	if err := s.SaveRetryEntry(ctx, entry1); err != nil {
		t.Fatalf("SaveRetryEntry (first): %v", err)
	}

	errMsg := "retry failed"
	entry2 := RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    2,
		DueAtMs:    2000,
		Error:      &errMsg,
	}
	if err := s.SaveRetryEntry(ctx, entry2); err != nil {
		t.Fatalf("SaveRetryEntry (upsert): %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", got.Attempt)
	}
	if got.DueAtMs != 2000 {
		t.Errorf("DueAtMs = %d, want 2000", got.DueAtMs)
	}
	if got.Error == nil {
		t.Fatal("Error = nil, want non-nil")
	}
	if *got.Error != "retry failed" {
		t.Errorf("Error = %q, want %q", *got.Error, "retry failed")
	}
}

func TestSaveRetryEntry_UpsertClearsError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	errMsg := "something went wrong"
	entry1 := RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    1,
		DueAtMs:    1000,
		Error:      &errMsg,
	}
	if err := s.SaveRetryEntry(ctx, entry1); err != nil {
		t.Fatalf("SaveRetryEntry (with error): %v", err)
	}

	entry2 := RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    2,
		DueAtMs:    2000,
		Error:      nil,
	}
	if err := s.SaveRetryEntry(ctx, entry2); err != nil {
		t.Fatalf("SaveRetryEntry (clear error): %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", got.Attempt)
	}
	if got.Error != nil {
		t.Errorf("Error = %q, want nil", *got.Error)
	}
}

func TestDeleteRetryEntry(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	e1 := RetryEntry{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000}
	e2 := RetryEntry{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 1, DueAtMs: 2000}
	if err := s.SaveRetryEntry(ctx, e1); err != nil {
		t.Fatalf("SaveRetryEntry (ISS-1): %v", err)
	}
	if err := s.SaveRetryEntry(ctx, e2); err != nil {
		t.Fatalf("SaveRetryEntry (ISS-2): %v", err)
	}

	if err := s.DeleteRetryEntry(ctx, "ISS-1"); err != nil {
		t.Fatalf("DeleteRetryEntry: %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].IssueID != "ISS-2" {
		t.Errorf("IssueID = %q, want %q", entries[0].IssueID, "ISS-2")
	}
}

func TestDeleteRetryEntry_Nonexistent(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	if err := s.DeleteRetryEntry(ctx, "no-such-id"); err != nil {
		t.Fatalf("DeleteRetryEntry on empty table returned error: %v", err)
	}
}

func TestLoadRetryEntries_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if entries == nil {
		t.Fatal("returned nil slice, want non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestLoadRetryEntries_OrderByDueAtMs(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	for _, e := range []RetryEntry{
		{IssueID: "ISS-3", Identifier: "PROJ-3", Attempt: 1, DueAtMs: 3000},
		{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000},
		{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 1, DueAtMs: 2000},
	} {
		if err := s.SaveRetryEntry(ctx, e); err != nil {
			t.Fatalf("SaveRetryEntry(%s): %v", e.IssueID, err)
		}
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	wantDue := []int64{1000, 2000, 3000}
	for i, want := range wantDue {
		if entries[i].DueAtMs != want {
			t.Errorf("entries[%d].DueAtMs = %d, want %d", i, entries[i].DueAtMs, want)
		}
	}
}

func TestSaveRetryEntry_DBError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Close the underlying DB to force an error on exec.
	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := s.SaveRetryEntry(ctx, RetryEntry{
		IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000,
	})
	if err == nil {
		t.Fatal("expected error from SaveRetryEntry on closed DB, got nil")
	}
}

func TestLoadRetryEntries_QueryError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Close the underlying DB to force an error on query.
	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := s.LoadRetryEntries(ctx)
	if err == nil {
		t.Fatal("expected error from LoadRetryEntries on closed DB, got nil")
	}
}

func TestLoadRetryEntries_ScanError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Insert a row with a non-numeric value in the attempt column so that
	// rows.Scan fails on type conversion (attempt is scanned into an int).
	// We bypass SaveRetryEntry to inject the invalid data directly.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms, error)
		VALUES ('ISS-1', 'PROJ-1', 'not-a-number', 1000, NULL)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	_, err := s.LoadRetryEntries(ctx)
	if err == nil {
		t.Fatal("expected scan error from LoadRetryEntries, got nil")
	}
}

func TestDeleteRetryEntry_DBError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Close the underlying DB to force an error on exec.
	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := s.DeleteRetryEntry(ctx, "ISS-1")
	if err == nil {
		t.Fatal("expected error from DeleteRetryEntry on closed DB, got nil")
	}
}

func TestLoadRetryEntries_DeterministicTieBreak(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// All entries share the same due_at_ms; order must be deterministic by issue_id.
	for _, e := range []RetryEntry{
		{IssueID: "C", Identifier: "PROJ-3", Attempt: 1, DueAtMs: 5000},
		{IssueID: "A", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 5000},
		{IssueID: "B", Identifier: "PROJ-2", Attempt: 1, DueAtMs: 5000},
	} {
		if err := s.SaveRetryEntry(ctx, e); err != nil {
			t.Fatalf("SaveRetryEntry(%s): %v", e.IssueID, err)
		}
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	wantIDs := []string{"A", "B", "C"}
	for i, want := range wantIDs {
		if entries[i].IssueID != want {
			t.Errorf("entries[%d].IssueID = %q, want %q", i, entries[i].IssueID, want)
		}
	}
}

// --- Run History Tests ---

// newTestRun returns a RunHistory with all fields populated using the given
// index for uniqueness. Error is nil (successful run).
func newTestRun(i int) RunHistory {
	return RunHistory{
		IssueID:      fmt.Sprintf("ISS-%d", i),
		Identifier:   fmt.Sprintf("PROJ-%d", i),
		Attempt:      i,
		AgentAdapter: "mock",
		Workspace:    fmt.Sprintf("/tmp/ws/PROJ-%d", i),
		StartedAt:    fmt.Sprintf("2026-03-19T10:%02d:00Z", i),
		CompletedAt:  fmt.Sprintf("2026-03-19T10:%02d:30Z", i),
		Status:       "succeeded",
	}
}

func appendOrFatal(t *testing.T, s *Store, run RunHistory) RunHistory {
	t.Helper()
	got, err := s.AppendRunHistory(context.Background(), run)
	if err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}
	return got
}

func TestAppendRunHistory(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	run := newTestRun(1)
	got := appendOrFatal(t, s, run)

	if got.ID <= 0 {
		t.Fatalf("ID = %d, want > 0", got.ID)
	}
	if got.IssueID != run.IssueID {
		t.Errorf("IssueID = %q, want %q", got.IssueID, run.IssueID)
	}
	if got.Identifier != run.Identifier {
		t.Errorf("Identifier = %q, want %q", got.Identifier, run.Identifier)
	}
	if got.Attempt != run.Attempt {
		t.Errorf("Attempt = %d, want %d", got.Attempt, run.Attempt)
	}
	if got.AgentAdapter != run.AgentAdapter {
		t.Errorf("AgentAdapter = %q, want %q", got.AgentAdapter, run.AgentAdapter)
	}
	if got.Workspace != run.Workspace {
		t.Errorf("Workspace = %q, want %q", got.Workspace, run.Workspace)
	}
	if got.StartedAt != run.StartedAt {
		t.Errorf("StartedAt = %q, want %q", got.StartedAt, run.StartedAt)
	}
	if got.CompletedAt != run.CompletedAt {
		t.Errorf("CompletedAt = %q, want %q", got.CompletedAt, run.CompletedAt)
	}
	if got.Status != run.Status {
		t.Errorf("Status = %q, want %q", got.Status, run.Status)
	}
	if got.Error != nil {
		t.Errorf("Error = %q, want nil", *got.Error)
	}
}

func TestAppendRunHistory_WithError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	errMsg := "agent crashed"
	run := newTestRun(1)
	run.Status = "failed"
	run.Error = &errMsg
	appendOrFatal(t, s, run)

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Error == nil {
		t.Fatal("Error = nil, want non-nil")
	}
	if *got.Error != "agent crashed" {
		t.Errorf("Error = %q, want %q", *got.Error, "agent crashed")
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want %q", got.Status, "failed")
	}
}

func TestAppendRunHistory_NilError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := newTestRun(1)
	appendOrFatal(t, s, run)

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Error != nil {
		t.Errorf("Error = %q, want nil", *entries[0].Error)
	}
}

func TestAppendRunHistory_MultipleAppendsAutoIncrement(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	var ids [3]int64
	for i := range ids {
		got := appendOrFatal(t, s, newTestRun(i+1))
		ids[i] = got.ID
	}

	if ids[0] >= ids[1] || ids[1] >= ids[2] {
		t.Fatalf("IDs not monotonically increasing: %v", ids)
	}
}

func TestQueryRunHistoryByIssue(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Insert runs for two different issues.
	r1 := newTestRun(1)
	r1.IssueID = "ISS-A"
	r2 := newTestRun(2)
	r2.IssueID = "ISS-A"
	r3 := newTestRun(3)
	r3.IssueID = "ISS-B"

	a1 := appendOrFatal(t, s, r1)
	a2 := appendOrFatal(t, s, r2)
	appendOrFatal(t, s, r3)

	entries, err := s.QueryRunHistoryByIssue(ctx, "ISS-A")
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// Most recent first (descending id).
	if entries[0].ID != a2.ID {
		t.Errorf("entries[0].ID = %d, want %d", entries[0].ID, a2.ID)
	}
	if entries[1].ID != a1.ID {
		t.Errorf("entries[1].ID = %d, want %d", entries[1].ID, a1.ID)
	}
}

func TestQueryRunHistoryByIssue_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entries, err := s.QueryRunHistoryByIssue(ctx, "no-such-issue")
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if entries == nil {
		t.Fatal("returned nil slice, want non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestQueryRecentRunHistory_FirstPage(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	var allIDs [5]int64
	for i := range allIDs {
		got := appendOrFatal(t, s, newTestRun(i+1))
		allIDs[i] = got.ID
	}

	entries, err := s.QueryRecentRunHistory(ctx, 3, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	// Most recent first: ids[4], ids[3], ids[2].
	if entries[0].ID != allIDs[4] {
		t.Errorf("entries[0].ID = %d, want %d", entries[0].ID, allIDs[4])
	}
	if entries[1].ID != allIDs[3] {
		t.Errorf("entries[1].ID = %d, want %d", entries[1].ID, allIDs[3])
	}
	if entries[2].ID != allIDs[2] {
		t.Errorf("entries[2].ID = %d, want %d", entries[2].ID, allIDs[2])
	}
}

func TestQueryRecentRunHistory_Pagination(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	var allIDs [5]int64
	for i := range allIDs {
		got := appendOrFatal(t, s, newTestRun(i+1))
		allIDs[i] = got.ID
	}

	// First page: 2 most recent.
	page1, err := s.QueryRecentRunHistory(ctx, 2, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1: got %d entries, want 2", len(page1))
	}
	if page1[0].ID != allIDs[4] || page1[1].ID != allIDs[3] {
		t.Errorf("page1 IDs = [%d, %d], want [%d, %d]",
			page1[0].ID, page1[1].ID, allIDs[4], allIDs[3])
	}

	// Second page: use smallest ID from page1 as cursor.
	cursor := page1[len(page1)-1].ID
	page2, err := s.QueryRecentRunHistory(ctx, 2, cursor)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2: got %d entries, want 2", len(page2))
	}
	if page2[0].ID != allIDs[2] || page2[1].ID != allIDs[1] {
		t.Errorf("page2 IDs = [%d, %d], want [%d, %d]",
			page2[0].ID, page2[1].ID, allIDs[2], allIDs[1])
	}

	// Third page: one remaining.
	cursor = page2[len(page2)-1].ID
	page3, err := s.QueryRecentRunHistory(ctx, 2, cursor)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory page3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3: got %d entries, want 1", len(page3))
	}
	if page3[0].ID != allIDs[0] {
		t.Errorf("page3[0].ID = %d, want %d", page3[0].ID, allIDs[0])
	}
}

func TestQueryRecentRunHistory_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entries, err := s.QueryRecentRunHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if entries == nil {
		t.Fatal("returned nil slice, want non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestQueryRecentRunHistory_LimitExceedsRows(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	appendOrFatal(t, s, newTestRun(1))
	appendOrFatal(t, s, newTestRun(2))

	entries, err := s.QueryRecentRunHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
}

func TestQueryRecentRunHistory_NonPositiveLimit(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	appendOrFatal(t, s, newTestRun(1))
	appendOrFatal(t, s, newTestRun(2))
	appendOrFatal(t, s, newTestRun(3))

	for _, limit := range []int{0, -1, -100} {
		entries, err := s.QueryRecentRunHistory(ctx, limit, 0)
		if err != nil {
			t.Fatalf("QueryRecentRunHistory(limit=%d): %v", limit, err)
		}
		if len(entries) != 1 {
			t.Errorf("QueryRecentRunHistory(limit=%d): got %d entries, want 1", limit, len(entries))
		}
	}
}

func TestAppendRunHistory_DBError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := s.AppendRunHistory(context.Background(), newTestRun(1))
	if err == nil {
		t.Fatal("expected error from AppendRunHistory on closed DB, got nil")
	}
}

// --- Session Metadata Tests ---

func TestUpsertSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	pid := "12345"
	meta := SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-abc",
		AgentPID:     &pid,
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		UpdatedAt:    "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	got, found, err := s.LoadSessionMetadata(ctx, "ISS-1")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.IssueID != "ISS-1" {
		t.Errorf("IssueID = %q, want %q", got.IssueID, "ISS-1")
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-abc")
	}
	if got.AgentPID == nil {
		t.Fatal("AgentPID = nil, want non-nil")
	}
	if *got.AgentPID != "12345" {
		t.Errorf("AgentPID = %q, want %q", *got.AgentPID, "12345")
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
	}
	if got.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", got.TotalTokens)
	}
	if got.UpdatedAt != "2026-03-19T10:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-03-19T10:00:00Z")
	}
}

func TestUpsertSessionMetadata_Update(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	pid1 := "111"
	meta1 := SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-1",
		AgentPID:     &pid1,
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
		UpdatedAt:    "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta1); err != nil {
		t.Fatalf("UpsertSessionMetadata (first): %v", err)
	}

	pid2 := "222"
	meta2 := SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-2",
		AgentPID:     &pid2,
		InputTokens:  200,
		OutputTokens: 100,
		TotalTokens:  300,
		UpdatedAt:    "2026-03-19T11:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta2); err != nil {
		t.Fatalf("UpsertSessionMetadata (second): %v", err)
	}

	got, found, err := s.LoadSessionMetadata(ctx, "ISS-1")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.SessionID != "sess-2" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-2")
	}
	if got.AgentPID == nil || *got.AgentPID != "222" {
		t.Errorf("AgentPID = %v, want %q", got.AgentPID, "222")
	}
	if got.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200", got.InputTokens)
	}
	if got.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100", got.OutputTokens)
	}
	if got.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300", got.TotalTokens)
	}
	if got.UpdatedAt != "2026-03-19T11:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-03-19T11:00:00Z")
	}
}

func TestUpsertSessionMetadata_NilAgentPID(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-abc",
		AgentPID:     nil,
		InputTokens:  0,
		OutputTokens: 0,
		TotalTokens:  0,
		UpdatedAt:    "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	got, found, err := s.LoadSessionMetadata(ctx, "ISS-1")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.AgentPID != nil {
		t.Errorf("AgentPID = %q, want nil", *got.AgentPID)
	}
}

func TestLoadSessionMetadata_NotFound(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	got, found, err := s.LoadSessionMetadata(ctx, "no-such-id")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if found {
		t.Fatal("expected found=false, got true")
	}
	if got != (SessionMetadata{}) {
		t.Errorf("expected zero-value SessionMetadata, got %+v", got)
	}
}

func TestLoadAllSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entries := []SessionMetadata{
		{IssueID: "ISS-1", SessionID: "s1", UpdatedAt: "2026-03-19T10:01:00Z"},
		{IssueID: "ISS-2", SessionID: "s2", UpdatedAt: "2026-03-19T10:03:00Z"},
		{IssueID: "ISS-3", SessionID: "s3", UpdatedAt: "2026-03-19T10:02:00Z"},
	}
	for _, e := range entries {
		if err := s.UpsertSessionMetadata(ctx, e); err != nil {
			t.Fatalf("UpsertSessionMetadata(%s): %v", e.IssueID, err)
		}
	}

	got, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}

	// Descending updated_at order: ISS-2 (10:03), ISS-3 (10:02), ISS-1 (10:01).
	wantIDs := []string{"ISS-2", "ISS-3", "ISS-1"}
	for i, want := range wantIDs {
		if got[i].IssueID != want {
			t.Errorf("got[%d].IssueID = %q, want %q", i, got[i].IssueID, want)
		}
	}
}

func TestLoadAllSessionMetadata_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	got, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if got == nil {
		t.Fatal("returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0", len(got))
	}
}

func TestDeleteSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	e1 := SessionMetadata{IssueID: "ISS-1", SessionID: "s1", UpdatedAt: "2026-03-19T10:00:00Z"}
	e2 := SessionMetadata{IssueID: "ISS-2", SessionID: "s2", UpdatedAt: "2026-03-19T10:01:00Z"}
	if err := s.UpsertSessionMetadata(ctx, e1); err != nil {
		t.Fatalf("UpsertSessionMetadata(ISS-1): %v", err)
	}
	if err := s.UpsertSessionMetadata(ctx, e2); err != nil {
		t.Fatalf("UpsertSessionMetadata(ISS-2): %v", err)
	}

	if err := s.DeleteSessionMetadata(ctx, "ISS-1"); err != nil {
		t.Fatalf("DeleteSessionMetadata: %v", err)
	}

	got, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].IssueID != "ISS-2" {
		t.Errorf("IssueID = %q, want %q", got[0].IssueID, "ISS-2")
	}
}

func TestDeleteSessionMetadata_Nonexistent(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	if err := s.DeleteSessionMetadata(ctx, "no-such-id"); err != nil {
		t.Fatalf("DeleteSessionMetadata on empty table returned error: %v", err)
	}
}

// --- Aggregate Metrics Tests ---

func TestUpsertAggregateMetrics(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	metrics := AggregateMetrics{
		Key:            "agent_totals",
		InputTokens:    5000,
		OutputTokens:   2400,
		TotalTokens:    7400,
		SecondsRunning: 1834.2,
		UpdatedAt:      "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertAggregateMetrics(ctx, metrics); err != nil {
		t.Fatalf("UpsertAggregateMetrics: %v", err)
	}

	got, found, err := s.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.Key != "agent_totals" {
		t.Errorf("Key = %q, want %q", got.Key, "agent_totals")
	}
	if got.InputTokens != 5000 {
		t.Errorf("InputTokens = %d, want 5000", got.InputTokens)
	}
	if got.OutputTokens != 2400 {
		t.Errorf("OutputTokens = %d, want 2400", got.OutputTokens)
	}
	if got.TotalTokens != 7400 {
		t.Errorf("TotalTokens = %d, want 7400", got.TotalTokens)
	}
	if math.Abs(got.SecondsRunning-1834.2) > 1e-9 {
		t.Errorf("SecondsRunning = %v, want 1834.2", got.SecondsRunning)
	}
	if got.UpdatedAt != "2026-03-19T10:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-03-19T10:00:00Z")
	}
}

func TestUpsertAggregateMetrics_Update(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	m1 := AggregateMetrics{
		Key:            "agent_totals",
		InputTokens:    100,
		OutputTokens:   50,
		TotalTokens:    150,
		SecondsRunning: 10.5,
		UpdatedAt:      "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertAggregateMetrics(ctx, m1); err != nil {
		t.Fatalf("UpsertAggregateMetrics (first): %v", err)
	}

	m2 := AggregateMetrics{
		Key:            "agent_totals",
		InputTokens:    5000,
		OutputTokens:   2400,
		TotalTokens:    7400,
		SecondsRunning: 1834.2,
		UpdatedAt:      "2026-03-19T11:00:00Z",
	}
	if err := s.UpsertAggregateMetrics(ctx, m2); err != nil {
		t.Fatalf("UpsertAggregateMetrics (second): %v", err)
	}

	got, found, err := s.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.InputTokens != 5000 {
		t.Errorf("InputTokens = %d, want 5000", got.InputTokens)
	}
	if got.OutputTokens != 2400 {
		t.Errorf("OutputTokens = %d, want 2400", got.OutputTokens)
	}
	if got.TotalTokens != 7400 {
		t.Errorf("TotalTokens = %d, want 7400", got.TotalTokens)
	}
	if math.Abs(got.SecondsRunning-1834.2) > 1e-9 {
		t.Errorf("SecondsRunning = %v, want 1834.2", got.SecondsRunning)
	}
	if got.UpdatedAt != "2026-03-19T11:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-03-19T11:00:00Z")
	}
}

func TestLoadAggregateMetrics_NotFound(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	got, found, err := s.LoadAggregateMetrics(ctx, "no-such-key")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if found {
		t.Fatal("expected found=false, got true")
	}
	if got != (AggregateMetrics{}) {
		t.Errorf("expected zero-value AggregateMetrics, got %+v", got)
	}
}

func TestUpsertAggregateMetrics_SecondsRunningPrecision(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	want := 1834.567
	metrics := AggregateMetrics{
		Key:            "agent_totals",
		SecondsRunning: want,
		UpdatedAt:      "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertAggregateMetrics(ctx, metrics); err != nil {
		t.Fatalf("UpsertAggregateMetrics: %v", err)
	}

	got, found, err := s.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if math.Abs(got.SecondsRunning-want) > 1e-9 {
		t.Errorf("SecondsRunning = %v, want %v (diff=%v)", got.SecondsRunning, want, math.Abs(got.SecondsRunning-want))
	}
}

// --- Startup Recovery Tests ---

func TestLoadRetryEntriesForRecovery_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	pending, err := s.LoadRetryEntriesForRecovery(ctx, 5000)
	if err != nil {
		t.Fatalf("LoadRetryEntriesForRecovery: %v", err)
	}
	if pending == nil {
		t.Fatal("returned nil slice, want non-nil empty slice")
	}
	if len(pending) != 0 {
		t.Fatalf("got %d entries, want 0", len(pending))
	}
}

func TestLoadRetryEntriesForRecovery_FutureDueAt(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	for _, e := range []RetryEntry{
		{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 5000},
		{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 2, DueAtMs: 8000},
	} {
		if err := s.SaveRetryEntry(ctx, e); err != nil {
			t.Fatalf("SaveRetryEntry(%s): %v", e.IssueID, err)
		}
	}

	pending, err := s.LoadRetryEntriesForRecovery(ctx, 3000)
	if err != nil {
		t.Fatalf("LoadRetryEntriesForRecovery: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d entries, want 2", len(pending))
	}
	if pending[0].RemainingMs != 2000 {
		t.Errorf("pending[0].RemainingMs = %d, want 2000", pending[0].RemainingMs)
	}
	if pending[1].RemainingMs != 5000 {
		t.Errorf("pending[1].RemainingMs = %d, want 5000", pending[1].RemainingMs)
	}
}

func TestLoadRetryEntriesForRecovery_PastDueAt(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	for _, e := range []RetryEntry{
		{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000},
		{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 2, DueAtMs: 2000},
	} {
		if err := s.SaveRetryEntry(ctx, e); err != nil {
			t.Fatalf("SaveRetryEntry(%s): %v", e.IssueID, err)
		}
	}

	pending, err := s.LoadRetryEntriesForRecovery(ctx, 5000)
	if err != nil {
		t.Fatalf("LoadRetryEntriesForRecovery: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d entries, want 2", len(pending))
	}
	for i, p := range pending {
		if p.RemainingMs != 0 {
			t.Errorf("pending[%d].RemainingMs = %d, want 0", i, p.RemainingMs)
		}
	}
}

func TestLoadRetryEntriesForRecovery_Mixed(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	for _, e := range []RetryEntry{
		{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000},
		{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 2, DueAtMs: 5000},
		{IssueID: "ISS-3", Identifier: "PROJ-3", Attempt: 3, DueAtMs: 9000},
	} {
		if err := s.SaveRetryEntry(ctx, e); err != nil {
			t.Fatalf("SaveRetryEntry(%s): %v", e.IssueID, err)
		}
	}

	pending, err := s.LoadRetryEntriesForRecovery(ctx, 5000)
	if err != nil {
		t.Fatalf("LoadRetryEntriesForRecovery: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("got %d entries, want 3", len(pending))
	}

	// Past entry (DueAtMs=1000, nowMs=5000) → 0.
	if pending[0].RemainingMs != 0 {
		t.Errorf("pending[0].RemainingMs = %d, want 0", pending[0].RemainingMs)
	}
	// Exact-now entry (DueAtMs=5000, nowMs=5000) → 0.
	if pending[1].RemainingMs != 0 {
		t.Errorf("pending[1].RemainingMs = %d, want 0", pending[1].RemainingMs)
	}
	// Future entry (DueAtMs=9000, nowMs=5000) → 4000.
	if pending[2].RemainingMs != 4000 {
		t.Errorf("pending[2].RemainingMs = %d, want 4000", pending[2].RemainingMs)
	}

	// Ordering must be due_at_ms ascending.
	wantDue := []int64{1000, 5000, 9000}
	for i, want := range wantDue {
		if pending[i].Entry.DueAtMs != want {
			t.Errorf("pending[%d].Entry.DueAtMs = %d, want %d", i, pending[i].Entry.DueAtMs, want)
		}
	}
}

func TestLoadRetryEntriesForRecovery_PreservesEntryFields(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	errMsg := "agent timeout"
	entry := RetryEntry{
		IssueID:    "ISS-42",
		Identifier: "PROJ-42",
		Attempt:    3,
		DueAtMs:    7500,
		Error:      &errMsg,
	}
	if err := s.SaveRetryEntry(ctx, entry); err != nil {
		t.Fatalf("SaveRetryEntry: %v", err)
	}

	pending, err := s.LoadRetryEntriesForRecovery(ctx, 5000)
	if err != nil {
		t.Fatalf("LoadRetryEntriesForRecovery: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d entries, want 1", len(pending))
	}

	got := pending[0].Entry
	if got.IssueID != "ISS-42" {
		t.Errorf("IssueID = %q, want %q", got.IssueID, "ISS-42")
	}
	if got.Identifier != "PROJ-42" {
		t.Errorf("Identifier = %q, want %q", got.Identifier, "PROJ-42")
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", got.Attempt)
	}
	if got.DueAtMs != 7500 {
		t.Errorf("DueAtMs = %d, want 7500", got.DueAtMs)
	}
	if got.Error == nil {
		t.Fatal("Error = nil, want non-nil")
	}
	if *got.Error != "agent timeout" {
		t.Errorf("Error = %q, want %q", *got.Error, "agent timeout")
	}
	if pending[0].RemainingMs != 2500 {
		t.Errorf("RemainingMs = %d, want 2500", pending[0].RemainingMs)
	}
}

func TestLoadRetryEntriesForRecovery_DBError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := s.LoadRetryEntriesForRecovery(context.Background(), 5000)
	if err == nil {
		t.Fatal("expected error from LoadRetryEntriesForRecovery on closed DB, got nil")
	}
}

// --- CountRunHistoryByIssue Tests ---

func TestCountRunHistoryByIssue(t *testing.T) {
	t.Parallel()

	t.Run("zero entries returns zero", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		count, err := s.CountRunHistoryByIssue(context.Background(), "ISS-NONEXISTENT")
		if err != nil {
			t.Fatalf("CountRunHistoryByIssue(ISS-NONEXISTENT) unexpected error: %v", err)
		}
		if count != 0 {
			t.Errorf("CountRunHistoryByIssue(ISS-NONEXISTENT) = %d, want 0", count)
		}
	})

	t.Run("returns correct count for N entries", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-COUNT"
			appendOrFatal(t, s, run)
		}

		count, err := s.CountRunHistoryByIssue(context.Background(), "ISS-COUNT")
		if err != nil {
			t.Fatalf("CountRunHistoryByIssue(ISS-COUNT) unexpected error: %v", err)
		}
		if count != 5 {
			t.Errorf("CountRunHistoryByIssue(ISS-COUNT) = %d, want 5", count)
		}
	})

	t.Run("independent counts per issue", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 3; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-A"
			appendOrFatal(t, s, run)
		}
		for i := 4; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-B"
			appendOrFatal(t, s, run)
		}

		ctx := context.Background()
		countA, err := s.CountRunHistoryByIssue(ctx, "ISS-A")
		if err != nil {
			t.Fatalf("CountRunHistoryByIssue(ISS-A) unexpected error: %v", err)
		}
		if countA != 3 {
			t.Errorf("CountRunHistoryByIssue(ISS-A) = %d, want 3", countA)
		}

		countB, err := s.CountRunHistoryByIssue(ctx, "ISS-B")
		if err != nil {
			t.Fatalf("CountRunHistoryByIssue(ISS-B) unexpected error: %v", err)
		}
		if countB != 2 {
			t.Errorf("CountRunHistoryByIssue(ISS-B) = %d, want 2", countB)
		}
	})
}
