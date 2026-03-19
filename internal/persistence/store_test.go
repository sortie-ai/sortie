package persistence

import (
	"context"
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
	ctx := context.Background()

	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer closeStore(t, s)
}

func TestOpen_WALEnabled(t *testing.T) {
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
	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	if err := s.DeleteRetryEntry(ctx, "no-such-id"); err != nil {
		t.Fatalf("DeleteRetryEntry on empty table returned error: %v", err)
	}
}

func TestLoadRetryEntries_Empty(t *testing.T) {
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
