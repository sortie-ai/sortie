package sqlite

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"

	"github.com/sortie-ai/sortie/internal/storage"
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
func TestOpen_BasicCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer closeStore(t, s)

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

	sessionID := "sess-abc"
	base := storage.RetryEntry{
		IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000,
		ConfiguredModel: "provider/cheap", IssueType: "Bug", IssueState: "In Progress", DispatchedAt: "2026-03-19T10:00:00Z",
	}
	withLabels := func(labels []string) storage.RetryEntry { e := base; e.IssueLabels = labels; return e }
	withSession := base
	withSession.SessionID = &sessionID
	withNotified := base
	withNotified.UsageLimitNotified = true
	stale := storage.RetryEntry{
		IssueID: "ISS-1", Identifier: "stale", ConfiguredModel: "stale", IssueType: "stale",
		IssueLabels: []string{"stale"}, IssueState: "stale", DispatchedAt: "stale",
	}

	tests := []struct {
		name  string
		entry storage.RetryEntry
	}{
		{"nil labels", withLabels(nil)},
		{"empty labels", withLabels([]string{})},
		{"two labels", withLabels([]string{"backend", "needs-review"})},
		{"session id", withSession},
		{"usage limit notified", withNotified},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ctx := context.Background()
			for _, entry := range []storage.RetryEntry{stale, tt.entry} {
				if err := s.SaveRetryEntry(ctx, entry); err != nil {
					t.Fatalf("SaveRetryEntry(%+v): %v", entry, err)
				}
			}

			got, err := s.LoadRetryEntries(ctx)

			if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], tt.entry) {
				t.Errorf("LoadRetryEntries = %+v, %v, want [%+v], nil", got, err, tt.entry)
			}
		})
	}
}

func TestSaveRetryEntry_Upsert(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry1 := storage.RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    1,
		DueAtMs:    1000,
		Error:      nil,
		ChainID:    "chain-1",
	}
	if err := s.SaveRetryEntry(ctx, entry1); err != nil {
		t.Fatalf("SaveRetryEntry (first): %v", err)
	}

	entry2 := storage.RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    2,
		DueAtMs:    2000,
		Error:      new("retry failed"),
		ChainID:    "chain-2",
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
	if got.ChainID != "chain-2" {
		t.Errorf("ChainID = %q, want %q", got.ChainID, "chain-2")
	}
}

func TestRecordStageHop_ChainFollowsLatestHop(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	first := storage.StageHop{
		IssueID: "ISS-1", Identifier: "PROJ-1", HopCount: 1, SourceRule: "specify", TargetRule: "plan", SourceDispatchID: "d1", ChainID: "chain-1",
		DisplayID: "owner/repo#1", IssueTitle: "Old title", IssueURL: "https://example.test/old", DispatchedAt: "2026-03-19T10:00:00Z",
		IssueType: "Bug", IssueLabels: []string{"stage-specify"}, IssueState: "To Do",
	}
	latest := first
	latest.SourceDispatchID, latest.ChainID = "d2", "chain-2"
	latest.DisplayID, latest.IssueTitle, latest.IssueURL = "owner/repo#2", "New title", "https://example.test/new"
	latest.DispatchedAt = "2026-03-19T11:00:00Z"
	latest.IssueType, latest.IssueLabels, latest.IssueState = "Story", []string{"feature", "plan"}, "In Review"
	other := latest
	other.IssueID, other.IssueLabels = "ISS-2", []string{}
	recorded := other
	recorded.IssueID, recorded.IssueLabels, recorded.RunDispatched = "ISS-3", nil, true
	for _, hop := range []storage.StageHop{first, latest, other, recorded} {
		if err := s.RecordStageHop(ctx, hop); err != nil {
			t.Fatalf("RecordStageHop(%+v): %v", hop, err)
		}
	}

	for _, issueID := range []string{"ISS-1", "ISS-ABSENT"} {
		if err := s.MarkStageHopDispatched(ctx, issueID); err != nil {
			t.Fatalf("MarkStageHopDispatched(%q) = %v, want nil", issueID, err)
		}
	}
	latest.RunDispatched = true

	hops, err := s.ListStageHops(ctx)
	if err != nil || !reflect.DeepEqual(hops, []storage.StageHop{latest, other, recorded}) {
		t.Errorf("ListStageHops = %+v, %v, want [%+v %+v %+v], nil", hops, err, latest, other, recorded)
	}
}

func TestSaveRetryEntry_UpsertClearsError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry1 := storage.RetryEntry{
		IssueID:    "ISS-1",
		Identifier: "PROJ-1",
		Attempt:    1,
		DueAtMs:    1000,
		Error:      new("something went wrong"),
	}
	if err := s.SaveRetryEntry(ctx, entry1); err != nil {
		t.Fatalf("SaveRetryEntry (with error): %v", err)
	}

	entry2 := storage.RetryEntry{
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

	e1 := storage.RetryEntry{IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000}
	e2 := storage.RetryEntry{IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 1, DueAtMs: 2000}
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

	for _, e := range []storage.RetryEntry{
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

	err := s.SaveRetryEntry(ctx, storage.RetryEntry{
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
	for _, e := range []storage.RetryEntry{
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

func TestSaveRetryEntry_Upsert_SessionID(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry1 := storage.RetryEntry{
		IssueID:    "ISS-UPS",
		Identifier: "PROJ-UPS",
		Attempt:    1,
		DueAtMs:    3000,
		SessionID:  nil,
	}
	if err := s.SaveRetryEntry(ctx, entry1); err != nil {
		t.Fatalf("SaveRetryEntry (first): %v", err)
	}

	sessID := "sess-updated"
	entry2 := storage.RetryEntry{
		IssueID:    "ISS-UPS",
		Identifier: "PROJ-UPS",
		Attempt:    2,
		DueAtMs:    6000,
		SessionID:  &sessID,
	}
	if err := s.SaveRetryEntry(ctx, entry2); err != nil {
		t.Fatalf("SaveRetryEntry (upsert): %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("LoadRetryEntries count = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.Attempt != 2 {
		t.Errorf("SaveRetryEntry_Upsert_SessionID: Attempt = %d, want 2", got.Attempt)
	}
	if got.SessionID == nil {
		t.Fatal("SaveRetryEntry_Upsert_SessionID: SessionID = nil after upsert, want non-nil")
	}
	if *got.SessionID != sessID {
		t.Errorf("SaveRetryEntry_Upsert_SessionID: SessionID = %q, want %q", *got.SessionID, sessID)
	}
}

// index for uniqueness. Error is nil (successful run).
func newTestRun(i int) storage.RunHistory {
	return storage.RunHistory{
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

func appendOrFatal(t *testing.T, s *Store, run storage.RunHistory) storage.RunHistory {
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

	run := newTestRun(1)
	run.Status = "failed"
	run.Error = new("agent crashed")
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

	tests := []struct {
		name     string
		issueID  string
		wantRuns []int
	}{
		{name: "newest first, other issues left out", issueID: "ISS-A", wantRuns: []int{2, 1}},
		{name: "unknown issue is an empty slice", issueID: "no-such-issue", wantRuns: []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ids := make([]int64, 0, 3)
			for i, issueID := range []string{"ISS-A", "ISS-A", "ISS-B"} {
				run := newTestRun(i + 1)
				run.IssueID = issueID
				ids = append(ids, appendOrFatal(t, s, run).ID)
			}

			got, err := s.QueryRunHistoryByIssue(context.Background(), tt.issueID)
			if err != nil {
				t.Fatalf("QueryRunHistoryByIssue(%q): %v", tt.issueID, err)
			}

			if got == nil {
				t.Fatalf("QueryRunHistoryByIssue(%q) = nil, want a non-nil slice", tt.issueID)
			}
			gotRuns := []int{}
			for _, r := range got {
				gotRuns = append(gotRuns, slices.Index(ids, r.ID)+1)
			}
			if !slices.Equal(gotRuns, tt.wantRuns) {
				t.Errorf("QueryRunHistoryByIssue(%q) runs = %v, want %v", tt.issueID, gotRuns, tt.wantRuns)
			}
		})
	}
}

func readBackByEveryReader(t *testing.T, s *Store, issueID string) map[string]storage.RunHistory {
	t.Helper()
	ctx := context.Background()

	byIssue, err := s.QueryRunHistoryByIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue(%q): %v", issueID, err)
	}
	filtered, err := s.QueryRunHistory(ctx, storage.RunHistoryFilter{IssueID: issueID, Limit: 10})
	if err != nil {
		t.Fatalf("QueryRunHistory(%q): %v", issueID, err)
	}
	if len(byIssue) != 1 || len(filtered) != 1 {
		t.Fatalf("rows for %q = %d by issue, %d filtered, want 1 each", issueID, len(byIssue), len(filtered))
	}
	return map[string]storage.RunHistory{"QueryRunHistoryByIssue": byIssue[0], "QueryRunHistory": filtered[0]}
}

func TestQueryRunHistory_Paging(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rows   int
		limit  int
		before int
		want   []int
	}{
		{name: "newest first", rows: 5, limit: 3, want: []int{5, 4, 3}},
		{name: "cursor row is excluded", rows: 5, limit: 2, before: 4, want: []int{3, 2}},
		{name: "last page is short", rows: 5, limit: 2, before: 2, want: []int{1}},
		{name: "limit above the row count", rows: 2, limit: 10, want: []int{2, 1}},
		{name: "zero limit reads as one", rows: 3, limit: 0, want: []int{3}},
		{name: "negative limit reads as one", rows: 3, limit: -100, want: []int{3}},
		{name: "no rows", rows: 0, limit: 10, want: []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ids := make([]int64, tt.rows)
			for i := range ids {
				ids[i] = appendOrFatal(t, s, newTestRun(i+1)).ID
			}
			var beforeID int64
			if tt.before > 0 {
				beforeID = ids[tt.before-1]
			}

			got, err := s.QueryRunHistory(context.Background(), storage.RunHistoryFilter{BeforeID: beforeID, Limit: tt.limit})
			if err != nil {
				t.Fatalf("QueryRunHistory(before=%d, limit=%d): %v", tt.before, tt.limit, err)
			}

			if got == nil {
				t.Fatalf("QueryRunHistory(before=%d, limit=%d) = nil, want a non-nil slice", tt.before, tt.limit)
			}
			gotRows := []int{}
			for _, r := range got {
				gotRows = append(gotRows, slices.Index(ids, r.ID)+1)
			}
			if !slices.Equal(gotRows, tt.want) {
				t.Errorf("QueryRunHistory(before=%d, limit=%d) rows = %v, want %v", tt.before, tt.limit, gotRows, tt.want)
			}
		})
	}
}

type issueGroupSeed struct {
	issue, identifier, display, status, title string
}

func TestQueryIssueGroups(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seeds       []issueGroupSeed
		filter      storage.IssueGroupFilter
		between     *issueGroupSeed
		wantPages   [][]string
		wantRestart []string
	}{
		{
			name:      "an issue holding most runs is served once",
			seeds:     []issueGroupSeed{{issue: "A"}, {issue: "B"}, {issue: "A"}, {issue: "C"}, {issue: "A"}, {issue: "D"}, {issue: "A"}, {issue: "A"}},
			filter:    storage.IssueGroupFilter{Limit: 2},
			wantPages: [][]string{{"A:8", "D:6"}, {"C:4", "B:2"}},
		},
		{
			name:        "a run appended between pages moves its issue to the first page only",
			seeds:       []issueGroupSeed{{issue: "A"}, {issue: "B"}, {issue: "C"}, {issue: "D"}},
			filter:      storage.IssueGroupFilter{Limit: 2},
			between:     &issueGroupSeed{issue: "B"},
			wantPages:   [][]string{{"D:4", "C:3"}, {"A:1"}},
			wantRestart: []string{"B:5", "D:4"},
		},
		{
			name:      "a status filter keys a group on its newest matching run",
			seeds:     []issueGroupSeed{{issue: "A", status: "failed"}, {issue: "B", status: "failed"}, {issue: "A", status: "succeeded"}},
			filter:    storage.IssueGroupFilter{Status: "failed", Limit: 10},
			wantPages: [][]string{{"B:2", "A:1"}},
		},
		{
			name:      "an identifier selects its issue",
			seeds:     []issueGroupSeed{{issue: "A", identifier: "9", display: "o/r#9"}, {issue: "B", identifier: "10", display: "o/r#10"}},
			filter:    storage.IssueGroupFilter{IssueKey: "9", Limit: 10},
			wantPages: [][]string{{"A:1"}},
		},
		{
			name:      "a display identifier selects its issue",
			seeds:     []issueGroupSeed{{issue: "A", identifier: "9", display: "o/r#9"}, {issue: "B", identifier: "10", display: "o/r#10"}},
			filter:    storage.IssueGroupFilter{IssueKey: "o/r#10", Limit: 10},
			wantPages: [][]string{{"B:2"}},
		},
		{
			name:      "a key recorded on an older run still keys the group on the newest run",
			seeds:     []issueGroupSeed{{issue: "A", identifier: "OLD-1"}, {issue: "B", identifier: "OTHER"}, {issue: "A", identifier: "NEW-1"}},
			filter:    storage.IssueGroupFilter{IssueKey: "OLD-1", Limit: 10},
			wantPages: [][]string{{"A:3"}},
		},
		{
			name:      "search text matches part of a key or a title in any case",
			seeds:     []issueGroupSeed{{issue: "A", identifier: "BP-701"}, {issue: "B", identifier: "BP-810", title: "Fix the Retry banner"}, {issue: "C", identifier: "BP-702"}},
			filter:    storage.IssueGroupFilter{Search: "retry", Limit: 10},
			wantPages: [][]string{{"B:2"}},
		},
		{
			name:      "search text treats a percent sign as a literal",
			seeds:     []issueGroupSeed{{issue: "A", identifier: "BP-1", title: "Cut cost 50%"}, {issue: "B", identifier: "BP-2", title: "Cut cost 50 times"}},
			filter:    storage.IssueGroupFilter{Search: "50%", Limit: 10},
			wantPages: [][]string{{"A:1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := waittest.Context(t)
			s := openTestStore(t)
			migrateOrFatal(t, s)
			var ids []int64
			appendSeed := func(seed issueGroupSeed) {
				run := newTestRun(1)
				run.IssueID = seed.issue
				run.Identifier = cmp.Or(seed.identifier, "PROJ-"+seed.issue)
				run.DisplayID = seed.display
				run.IssueTitle = seed.title
				run.Status = cmp.Or(seed.status, "succeeded")
				ids = append(ids, appendOrFatal(t, s, run).ID)
			}
			readPage := func(f storage.IssueGroupFilter) []storage.IssueGroup {
				page, err := s.QueryIssueGroups(ctx, f)
				if err != nil {
					t.Fatalf("QueryIssueGroups(%+v): %v", f, err)
				}
				if page == nil {
					t.Fatalf("QueryIssueGroups(%+v) = nil, want a non-nil slice", f)
				}
				return page
			}
			labels := func(page []storage.IssueGroup) []string {
				out := []string{}
				for _, g := range page {
					out = append(out, fmt.Sprintf("%s:%d", g.IssueID, slices.Index(ids, g.NewestRunID)+1))
				}
				return out
			}
			for _, seed := range tt.seeds {
				appendSeed(seed)
			}

			var gotPages [][]string
			f := tt.filter
			for range len(tt.seeds) + 2 {
				page := readPage(f)
				if len(page) == 0 {
					break
				}
				gotPages = append(gotPages, labels(page))
				f.BeforeID = page[len(page)-1].NewestRunID
				if tt.between != nil && len(gotPages) == 1 {
					appendSeed(*tt.between)
				}
			}

			if !reflect.DeepEqual(gotPages, tt.wantPages) {
				t.Errorf("QueryIssueGroups(%+v) pages = %v, want %v", tt.filter, gotPages, tt.wantPages)
			}
			if tt.between != nil {
				if got := labels(readPage(tt.filter)); !slices.Equal(got, tt.wantRestart) {
					t.Errorf("QueryIssueGroups(%+v) restarted = %v, want %v", tt.filter, got, tt.wantRestart)
				}
			}
		})
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

func TestAppendRunHistory_WorkflowFile(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	run := newTestRun(1)
	run.WorkflowFile = "backend.WORKFLOW.md"
	appendOrFatal(t, s, run)

	for reader, row := range readBackByEveryReader(t, s, run.IssueID) {
		if row.WorkflowFile != "backend.WORKFLOW.md" {
			t.Errorf("%s WorkflowFile = %q, want %q", reader, row.WorkflowFile, "backend.WORKFLOW.md")
		}
	}
}

func TestAppendRunHistory_WorkflowFile_EmptyStoredAsNull(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// newTestRun leaves WorkflowFile empty; must be stored as NULL and
	// read back as empty string (not "<nil>" or similar).
	run := newTestRun(2)
	appendOrFatal(t, s, run)

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].WorkflowFile != "" {
		t.Errorf("WorkflowFile = %q, want empty string for NULL column", entries[0].WorkflowFile)
	}
}

func TestAppendRunHistory_TurnsCompleted(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	run := newTestRun(1)
	run.TurnsCompleted = 7
	got := appendOrFatal(t, s, run)

	if got.TurnsCompleted != 7 {
		t.Errorf("AppendRunHistory returned TurnsCompleted = %d, want 7", got.TurnsCompleted)
	}
	for reader, row := range readBackByEveryReader(t, s, run.IssueID) {
		if row.TurnsCompleted != 7 {
			t.Errorf("%s TurnsCompleted = %d, want 7", reader, row.TurnsCompleted)
		}
	}
}

func TestAppendRunHistory_TurnsCompleted_Zero(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Zero turns (e.g. workspace prep failed immediately) must be stored and
	// read back as 0, not as NULL or some other sentinel.
	run := newTestRun(1)
	run.TurnsCompleted = 0
	appendOrFatal(t, s, run)

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].TurnsCompleted != 0 {
		t.Errorf("QueryRunHistoryByIssue TurnsCompleted = %d, want 0", entries[0].TurnsCompleted)
	}
}

func TestAppendRunHistory_DisplayIDRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	run := newTestRun(1)
	run.DisplayID = "owner/repo#1"
	got := appendOrFatal(t, s, run)

	if got.DisplayID != "owner/repo#1" {
		t.Errorf("AppendRunHistory returned DisplayID = %q, want %q", got.DisplayID, "owner/repo#1")
	}
	for reader, row := range readBackByEveryReader(t, s, run.IssueID) {
		if row.DisplayID != "owner/repo#1" {
			t.Errorf("%s DisplayID = %q, want %q", reader, row.DisplayID, "owner/repo#1")
		}
	}
}

func TestAppendRunHistory_EmptyDisplayIDRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Jira-style run: DisplayID is empty (Identifier is self-explanatory).
	run := newTestRun(1)
	got := appendOrFatal(t, s, run)

	if got.DisplayID != "" {
		t.Errorf("AppendRunHistory returned DisplayID = %q, want empty string", got.DisplayID)
	}

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].DisplayID != "" {
		t.Errorf("QueryRunHistoryByIssue DisplayID = %q, want empty string for NULL column", entries[0].DisplayID)
	}
}

func TestUpsertSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := storage.SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-abc",
		AgentPID:     new("12345"),
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

	meta1 := storage.SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-1",
		AgentPID:     new("111"),
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
		UpdatedAt:    "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta1); err != nil {
		t.Fatalf("UpsertSessionMetadata (first): %v", err)
	}

	meta2 := storage.SessionMetadata{
		IssueID:      "ISS-1",
		SessionID:    "sess-2",
		AgentPID:     new("222"),
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

	meta := storage.SessionMetadata{
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

// TestUpsertSessionMetadata_APIRequestsMeasuredRoundTrip asserts that, at
// the storage boundary, a row written with the verdict true round-trips
// its raw count, including a genuine zero the measurement itself
// produced, and a row written with the verdict false round-trips the
// zero count the two orchestrator writers store for it, so the
// persisted row can never contradict its own qualifier.
func TestUpsertSessionMetadata_APIRequestsMeasuredRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		measured        bool
		apiRequestCount int
	}{
		{"measured row round-trips its raw count", true, 9},
		{"measured row round-trips a genuine zero count", true, 0},
		{"unmeasured row round-trips a zero count", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ctx := context.Background()

			meta := storage.SessionMetadata{
				IssueID:             "ISS-REQV",
				SessionID:           "sess-reqv",
				APIRequestCount:     tt.apiRequestCount,
				APIRequestsMeasured: tt.measured,
				UpdatedAt:           "2026-03-19T10:00:00Z",
			}
			if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
				t.Fatalf("UpsertSessionMetadata: %v", err)
			}

			got, found, err := s.LoadSessionMetadata(ctx, "ISS-REQV")
			if err != nil {
				t.Fatalf("LoadSessionMetadata: %v", err)
			}
			if !found {
				t.Fatal("expected found=true, got false")
			}
			if got.APIRequestsMeasured != tt.measured {
				t.Errorf("APIRequestsMeasured = %v, want %v", got.APIRequestsMeasured, tt.measured)
			}
			if got.APIRequestCount != tt.apiRequestCount {
				t.Errorf("APIRequestCount = %d, want %d", got.APIRequestCount, tt.apiRequestCount)
			}
		})
	}
}

// TestUpsertSessionMetadata_DispatchIDRoundTrip asserts that DispatchID
// round-trips through UpsertSessionMetadata and both LoadSessionMetadata
// and LoadAllSessionMetadata, and that an update carrying "" replaces a
// stored non-empty value.
func TestUpsertSessionMetadata_DispatchIDRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := storage.SessionMetadata{
		IssueID:    "ISS-DISPATCH",
		SessionID:  "sess-abc",
		DispatchID: "dispatch-abc",
		UpdatedAt:  "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	got, found, err := s.LoadSessionMetadata(ctx, "ISS-DISPATCH")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.DispatchID != "dispatch-abc" {
		t.Errorf("DispatchID = %q, want %q", got.DispatchID, "dispatch-abc")
	}

	all, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadAllSessionMetadata() len = %d, want 1", len(all))
	}
	if all[0].DispatchID != "dispatch-abc" {
		t.Errorf("LoadAllSessionMetadata()[0].DispatchID = %q, want %q", all[0].DispatchID, "dispatch-abc")
	}

	meta.DispatchID = ""
	meta.UpdatedAt = "2026-03-19T11:00:00Z"
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata (clear): %v", err)
	}

	got, found, err = s.LoadSessionMetadata(ctx, "ISS-DISPATCH")
	if err != nil {
		t.Fatalf("LoadSessionMetadata (after clear): %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.DispatchID != "" {
		t.Errorf("DispatchID after an update carrying \"\" = %q, want empty", got.DispatchID)
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
	if got != (storage.SessionMetadata{}) {
		t.Errorf("expected zero-value storage.SessionMetadata, got %+v", got)
	}
}

func TestLoadAllSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entries := []storage.SessionMetadata{
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

// TestLoadAllSessionMetadata_NonNilAgentPID verifies that a non-null
// agent_pid column round-trips through LoadAllSessionMetadata, not just
// through the single-row LoadSessionMetadata reader.
func TestLoadAllSessionMetadata_NonNilAgentPID(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := storage.SessionMetadata{
		IssueID:   "ISS-1",
		SessionID: "sess-abc",
		AgentPID:  new("54321"),
		UpdatedAt: "2026-03-19T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	got, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].AgentPID == nil {
		t.Fatal("AgentPID = nil, want non-nil")
	}
	if *got[0].AgentPID != "54321" {
		t.Errorf("AgentPID = %q, want %q", *got[0].AgentPID, "54321")
	}
}

func TestDeleteSessionMetadata(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	e1 := storage.SessionMetadata{IssueID: "ISS-1", SessionID: "s1", UpdatedAt: "2026-03-19T10:00:00Z"}
	e2 := storage.SessionMetadata{IssueID: "ISS-2", SessionID: "s2", UpdatedAt: "2026-03-19T10:01:00Z"}
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

func TestUpsertAggregateMetrics(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	metrics := storage.AggregateMetrics{
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

	m1 := storage.AggregateMetrics{
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

	m2 := storage.AggregateMetrics{
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
	if got != (storage.AggregateMetrics{}) {
		t.Errorf("expected zero-value storage.AggregateMetrics, got %+v", got)
	}
}

func TestUpsertAggregateMetrics_SecondsRunningPrecision(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	want := 1834.567
	metrics := storage.AggregateMetrics{
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

	for _, e := range []storage.RetryEntry{
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

	for _, e := range []storage.RetryEntry{
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

	for _, e := range []storage.RetryEntry{
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

	// Past entry (DueAtMs=1000, nowMs=5000) -> 0.
	if pending[0].RemainingMs != 0 {
		t.Errorf("pending[0].RemainingMs = %d, want 0", pending[0].RemainingMs)
	}
	// Exact-now entry (DueAtMs=5000, nowMs=5000) -> 0.
	if pending[1].RemainingMs != 0 {
		t.Errorf("pending[1].RemainingMs = %d, want 0", pending[1].RemainingMs)
	}
	// Future entry (DueAtMs=9000, nowMs=5000) -> 4000.
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

	entry := storage.RetryEntry{
		IssueID:    "ISS-42",
		Identifier: "PROJ-42",
		Attempt:    3,
		DueAtMs:    7500,
		Error:      new("agent timeout"),
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

	t.Run("usage_limited row is not counted", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i, status := range []string{"failed", "usage_limited"} {
			run := newTestRun(i + 1)
			run.IssueID = "ISS-REFUSED"
			run.Status = status
			appendOrFatal(t, s, run)
		}

		count, err := s.CountRunHistoryByIssue(context.Background(), "ISS-REFUSED")
		if err != nil || count != 1 {
			t.Errorf("CountRunHistoryByIssue(ISS-REFUSED) = %d, %v, want 1, nil", count, err)
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

func TestQueryBudgetExhaustedIssues(t *testing.T) {
	t.Parallel()

	t.Run("empty candidateIDs returns empty slice without DB query", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty slice", result)
		}
	})

	t.Run("candidates with no run history returns empty", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-A", "ISS-B"}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty", result)
		}
	})

	t.Run("candidate with count below maxSessions not returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 2; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-UNDER"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-UNDER"}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty (count 2 < maxSessions 3)", result)
		}
	})

	t.Run("candidate with count equal to maxSessions is returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 3; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-EXACT"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-EXACT"}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := result["ISS-EXACT"]; !ok || len(result) != 1 {
			t.Errorf("result = %v, want {ISS-EXACT: ...}", result)
		}
	})

	t.Run("candidate with count exceeding maxSessions is returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-OVER"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-OVER"}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := result["ISS-OVER"]; !ok || len(result) != 1 {
			t.Errorf("result = %v, want {ISS-OVER: ...}", result)
		}
	})

	t.Run("over-ceiling candidate reports its run-history count, under-ceiling candidate is absent", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		for i := 1; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-OVER-COUNT"
			appendOrFatal(t, s, run)
		}
		for i := 6; i <= 7; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-UNDER-COUNT"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(
			context.Background(),
			[]string{"ISS-OVER-COUNT", "ISS-UNDER-COUNT"},
			3,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, want := result["ISS-OVER-COUNT"], 5; got != want {
			t.Errorf("result[ISS-OVER-COUNT] = %d, want %d (run-history row count)", got, want)
		}
		if _, ok := result["ISS-UNDER-COUNT"]; ok {
			t.Errorf("result[ISS-UNDER-COUNT] = %v, want absent (count 2 < maxSessions 3)", result["ISS-UNDER-COUNT"])
		}
		if len(result) != 1 {
			t.Errorf("len(result) = %d, want 1", len(result))
		}
	})

	t.Run("mixed candidates only qualifying ones returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		// ISS-EXHAUST: 3 runs (exhausted), ISS-OK: 2 runs (not), ISS-NONE: 0 runs.
		for i := 1; i <= 3; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-EXHAUST"
			appendOrFatal(t, s, run)
		}
		for i := 4; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-OK"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(
			context.Background(),
			[]string{"ISS-EXHAUST", "ISS-OK", "ISS-NONE"},
			3,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := result["ISS-EXHAUST"]; !ok || len(result) != 1 {
			t.Errorf("result = %v, want {ISS-EXHAUST: ...}", result)
		}
	})

	t.Run("maxSessions 1 returns candidate with one or more runs", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		run := newTestRun(1)
		run.IssueID = "ISS-SINGLE"
		appendOrFatal(t, s, run)

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-SINGLE"}, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := result["ISS-SINGLE"]; !ok || len(result) != 1 {
			t.Errorf("result = %v, want {ISS-SINGLE: ...}", result)
		}
	})

	t.Run("non-candidate issues in run_history are excluded", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		// ISS-NOTASKED has run history but is NOT in candidateIDs.
		for i := 1; i <= 5; i++ {
			run := newTestRun(i)
			run.IssueID = "ISS-NOTASKED"
			appendOrFatal(t, s, run)
		}

		result, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-CANDIDATE"}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty (ISS-NOTASKED not in candidateIDs)", result)
		}
	})

	t.Run("closed DB returns error", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		if err := s.db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		_, err := s.QueryBudgetExhaustedIssues(context.Background(), []string{"ISS-A"}, 3)
		if err == nil {
			t.Fatal("expected error from closed DB, got nil")
		}
	})
}

func TestPing_OpenStore(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping() on open store = %v, want nil", err)
	}
}

func TestPing_ClosedStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping() on closed store = nil, want error")
	}
}

// TestUpsertSessionMetadata_ExtendedFields verifies that the new
// CacheReadTokens, ModelName, and APIRequestCount fields survive a
// round-trip through Upsert -> Load.
func TestUpsertSessionMetadata_ExtendedFields(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := storage.SessionMetadata{
		IssueID:         "EXT-1",
		SessionID:       "sess-ext",
		AgentPID:        new("9876"),
		InputTokens:     500,
		OutputTokens:    200,
		TotalTokens:     700,
		CacheReadTokens: 3000,
		ModelName:       "claude-sonnet-4-20250514",
		APIRequestCount: 42,
		UpdatedAt:       "2026-04-01T12:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	got, found, err := s.LoadSessionMetadata(ctx, "EXT-1")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got.CacheReadTokens != 3000 {
		t.Errorf("CacheReadTokens = %d, want 3000", got.CacheReadTokens)
	}
	if got.ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("ModelName = %q, want %q", got.ModelName, "claude-sonnet-4-20250514")
	}
	if got.APIRequestCount != 42 {
		t.Errorf("APIRequestCount = %d, want 42", got.APIRequestCount)
	}
	// The fixture never sets APIRequestsMeasured, so it round-trips
	// false beside the non-zero count. That pairing is not one the
	// orchestrator writes, and this row is not the legacy shape
	// migration 016 leaves behind either; it is here because the store
	// applies no invariant between the two columns and must return
	// what it was handed. A regression that let the qualifier drift
	// from what was written would still pass on the count alone.
	if got.APIRequestsMeasured {
		t.Error("APIRequestsMeasured = true, want false (fixture never sets it)")
	}
}

// TestUpsertSessionMetadata_ExtendedFieldsUpdate verifies that updating
// extended fields overwrites old values correctly.
func TestUpsertSessionMetadata_ExtendedFieldsUpdate(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	m1 := storage.SessionMetadata{
		IssueID:         "EXT-UPD",
		SessionID:       "sess-1",
		CacheReadTokens: 100,
		ModelName:       "model-a",
		APIRequestCount: 5,
		UpdatedAt:       "2026-04-01T10:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, m1); err != nil {
		t.Fatalf("UpsertSessionMetadata (first): %v", err)
	}

	m2 := storage.SessionMetadata{
		IssueID:         "EXT-UPD",
		SessionID:       "sess-2",
		CacheReadTokens: 5000,
		ModelName:       "model-b",
		APIRequestCount: 18,
		UpdatedAt:       "2026-04-01T11:00:00Z",
	}
	if err := s.UpsertSessionMetadata(ctx, m2); err != nil {
		t.Fatalf("UpsertSessionMetadata (second): %v", err)
	}

	got, _, err := s.LoadSessionMetadata(ctx, "EXT-UPD")
	if err != nil {
		t.Fatalf("LoadSessionMetadata: %v", err)
	}
	if got.CacheReadTokens != 5000 {
		t.Errorf("CacheReadTokens = %d, want 5000", got.CacheReadTokens)
	}
	if got.ModelName != "model-b" {
		t.Errorf("ModelName = %q, want %q", got.ModelName, "model-b")
	}
	if got.APIRequestCount != 18 {
		t.Errorf("APIRequestCount = %d, want 18", got.APIRequestCount)
	}
	if got.APIRequestsMeasured {
		t.Error("APIRequestsMeasured = true, want false (fixture never sets it)")
	}
}

// TestLoadAllSessionMetadata_ExtendedFields verifies that extended fields
// are visible via LoadAllSessionMetadata.
func TestLoadAllSessionMetadata_ExtendedFields(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	if err := s.UpsertSessionMetadata(ctx, storage.SessionMetadata{
		IssueID:         "ALL-1",
		SessionID:       "s1",
		CacheReadTokens: 111,
		ModelName:       "model-x",
		APIRequestCount: 3,
		UpdatedAt:       "2026-04-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	all, err := s.LoadAllSessionMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadAllSessionMetadata: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d entries, want 1", len(all))
	}
	if all[0].CacheReadTokens != 111 {
		t.Errorf("CacheReadTokens = %d, want 111", all[0].CacheReadTokens)
	}
	if all[0].ModelName != "model-x" {
		t.Errorf("ModelName = %q, want %q", all[0].ModelName, "model-x")
	}
	if all[0].APIRequestCount != 3 {
		t.Errorf("APIRequestCount = %d, want 3", all[0].APIRequestCount)
	}
	if all[0].APIRequestsMeasured {
		t.Error("APIRequestsMeasured = true, want false (fixture never sets it)")
	}
}

// TestUpsertAggregateMetrics_CacheReadTokens verifies that the new
// CacheReadTokens field survives a round-trip.
func TestUpsertAggregateMetrics_CacheReadTokens(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	metrics := storage.AggregateMetrics{
		Key:             "agent_totals",
		InputTokens:     1000,
		OutputTokens:    500,
		TotalTokens:     1500,
		CacheReadTokens: 7500,
		SecondsRunning:  120.5,
		UpdatedAt:       "2026-04-01T12:00:00Z",
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
	if got.CacheReadTokens != 7500 {
		t.Errorf("CacheReadTokens = %d, want 7500", got.CacheReadTokens)
	}
}

// TestUpsertAggregateMetrics_CacheReadTokensUpdate verifies that
// updating CacheReadTokens overwrites the old value.
func TestUpsertAggregateMetrics_CacheReadTokensUpdate(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	m1 := storage.AggregateMetrics{Key: "agent_totals", CacheReadTokens: 100, UpdatedAt: "2026-04-01T10:00:00Z"}
	if err := s.UpsertAggregateMetrics(ctx, m1); err != nil {
		t.Fatalf("UpsertAggregateMetrics (first): %v", err)
	}

	m2 := storage.AggregateMetrics{Key: "agent_totals", CacheReadTokens: 9999, UpdatedAt: "2026-04-01T11:00:00Z"}
	if err := s.UpsertAggregateMetrics(ctx, m2); err != nil {
		t.Fatalf("UpsertAggregateMetrics (second): %v", err)
	}

	got, _, err := s.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if got.CacheReadTokens != 9999 {
		t.Errorf("CacheReadTokens = %d, want 9999", got.CacheReadTokens)
	}
}

// TestUpsertAggregateMetrics_UnmeasuredSessions verifies the field survives a round-trip.
func TestUpsertAggregateMetrics_UnmeasuredSessions(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	metrics := storage.AggregateMetrics{
		Key:                "agent_totals",
		InputTokens:        1000,
		UnmeasuredSessions: 42,
		UpdatedAt:          "2026-04-01T12:00:00Z",
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
	if got.UnmeasuredSessions != 42 {
		t.Errorf("UnmeasuredSessions = %d, want 42", got.UnmeasuredSessions)
	}
}

// TestUpsertAggregateMetrics_UnmeasuredSessionsUpdate verifies a second update overwrites the value.
func TestUpsertAggregateMetrics_UnmeasuredSessionsUpdate(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	m1 := storage.AggregateMetrics{Key: "agent_totals", UnmeasuredSessions: 3, UpdatedAt: "2026-04-01T10:00:00Z"}
	if err := s.UpsertAggregateMetrics(ctx, m1); err != nil {
		t.Fatalf("UpsertAggregateMetrics (first): %v", err)
	}

	m2 := storage.AggregateMetrics{Key: "agent_totals", UnmeasuredSessions: 7, UpdatedAt: "2026-04-01T11:00:00Z"}
	if err := s.UpsertAggregateMetrics(ctx, m2); err != nil {
		t.Fatalf("UpsertAggregateMetrics (second): %v", err)
	}

	got, _, err := s.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics: %v", err)
	}
	if got.UnmeasuredSessions != 7 {
		t.Errorf("UnmeasuredSessions = %d, want 7", got.UnmeasuredSessions)
	}
}

func TestOpenReadOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create and migrate a file-backed database via the standard Open path.
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	closeStore(t, s)

	// Re-open in read-only mode and verify the connection is alive.
	ro, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dbPath, err)
	}
	defer closeStore(t, ro)

	if err := ro.Ping(ctx); err != nil {
		t.Fatalf("Ping on read-only store: %v", err)
	}
}

func TestOpenReadOnly_RejectWrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	closeStore(t, s)

	// Re-open in read-only mode.
	ro, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dbPath, err)
	}
	defer closeStore(t, ro)

	// Any mutation must be rejected by the driver with an SQLITE_READONLY error.
	_, writeErr := ro.db.ExecContext(ctx,
		`INSERT INTO run_history
			(issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status)
		VALUES ('x', 'x', 1, 'mock', '/ws', '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z', 'succeeded')`)
	if writeErr == nil {
		t.Fatal("ExecContext on read-only store succeeded, want SQLITE_READONLY error")
	}
	if !strings.Contains(strings.ToLower(writeErr.Error()), "readonly") {
		t.Errorf("write error = %q, want to contain %q", writeErr.Error(), "readonly")
	}
}

func TestOpenReadOnly_ReadsExistingData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	run := newTestRun(1)
	if _, err := s.AppendRunHistory(ctx, run); err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}
	closeStore(t, s)

	ro, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dbPath, err)
	}
	defer closeStore(t, ro)

	entries, err := ro.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].IssueID != run.IssueID {
		t.Errorf("IssueID = %q, want %q", entries[0].IssueID, run.IssueID)
	}
	if entries[0].Identifier != run.Identifier {
		t.Errorf("Identifier = %q, want %q", entries[0].Identifier, run.Identifier)
	}
}

func TestAppendRunHistory_ReviewMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := `{"enabled":true,"iterations":[],"total_iterations":1,"final_verdict":"pass","cap_reached":false}`
	run := newTestRun(100)
	run.ReviewMetadata = &meta

	got := appendOrFatal(t, s, run)
	if got.ReviewMetadata == nil {
		t.Fatal("ReviewMetadata = nil after insert, want non-nil")
	}
	if *got.ReviewMetadata != meta {
		t.Errorf("ReviewMetadata = %q, want %q", *got.ReviewMetadata, meta)
	}

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].ReviewMetadata == nil {
		t.Fatal("queried ReviewMetadata = nil, want non-nil")
	}
	if *entries[0].ReviewMetadata != meta {
		t.Errorf("queried ReviewMetadata = %q, want %q", *entries[0].ReviewMetadata, meta)
	}
}

func TestAppendRunHistory_ReviewMetadata_Null(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := newTestRun(101)
	// ReviewMetadata is nil; self-review did not run.

	got := appendOrFatal(t, s, run)
	if got.ReviewMetadata != nil {
		t.Errorf("ReviewMetadata = %q, want nil", *got.ReviewMetadata)
	}

	entries, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].ReviewMetadata != nil {
		t.Errorf("queried ReviewMetadata = %q, want nil", *entries[0].ReviewMetadata)
	}
}

// recoveryRefTime is a fixed reference for recovery query tests.
var recoveryRefTime = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// recentCompletedAt returns an RFC3339 timestamp for a run completed daysAgo
// days before recoveryRefTime.
func recentCompletedAt(daysAgo int) string {
	return recoveryRefTime.Add(-time.Duration(daysAgo) * 24 * time.Hour).UTC().Format(time.RFC3339)
}

// appendRecoveryRun inserts a recovery-eligible (succeeded, non-empty workspace) run and returns it.
func appendRecoveryRun(t *testing.T, s *Store, issueID, identifier string, attempt int, completedAt string) storage.RunHistory {
	t.Helper()
	run := storage.RunHistory{
		IssueID:      issueID,
		Identifier:   identifier,
		Attempt:      attempt,
		AgentAdapter: "mock",
		Workspace:    "/ws/" + identifier,
		StartedAt:    completedAt,
		CompletedAt:  completedAt,
		Status:       "succeeded",
	}
	return appendOrFatal(t, s, run)
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_Empty(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)
	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(context.Background(), cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if got == nil {
		t.Fatal("result = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 0", len(got))
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_LatestPerIssue(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	// Three runs for ISS-A; the third has the highest id and is the expected result.
	appendRecoveryRun(t, s, "ISS-A", "PROJ-1", 1, recentCompletedAt(5))
	appendRecoveryRun(t, s, "ISS-A", "PROJ-1", 2, recentCompletedAt(4))
	latestA := appendRecoveryRun(t, s, "ISS-A", "PROJ-1", 3, recentCompletedAt(3))
	// One run for ISS-B.
	latestB := appendRecoveryRun(t, s, "ISS-B", "PROJ-2", 1, recentCompletedAt(2))

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 2", len(got))
	}
	// Results are ordered newest id first.
	if got[0].ID != latestB.ID {
		t.Errorf("got[0].ID = %d, want %d (latest ISS-B)", got[0].ID, latestB.ID)
	}
	if got[1].ID != latestA.ID {
		t.Errorf("got[1].ID = %d, want %d (latest ISS-A)", got[1].ID, latestA.ID)
	}
	if got[1].Attempt != 3 {
		t.Errorf("got[1].Attempt = %d, want 3 (latest attempt for ISS-A)", got[1].Attempt)
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_BoundsByCutoffAndLimit(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	// Insert 3 recent issues (within cutoff) and 1 old issue (outside cutoff).
	r1 := appendRecoveryRun(t, s, "ISS-1", "PROJ-1", 1, recentCompletedAt(10))
	r2 := appendRecoveryRun(t, s, "ISS-2", "PROJ-2", 1, recentCompletedAt(8))
	r3 := appendRecoveryRun(t, s, "ISS-3", "PROJ-3", 1, recentCompletedAt(3))
	// Old run: completed_at is 35 days ago, older than the 30-day cutoff.
	appendRecoveryRun(t, s, "ISS-OLD", "PROJ-OLD", 1, recentCompletedAt(35))

	// Limit=2: should return the 2 newest (r3, r2); r1 is capped out; ISS-OLD is excluded.
	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery(limit=2) len = %d, want 2", len(got))
	}
	// Ordered newest-first.
	if got[0].ID != r3.ID {
		t.Errorf("got[0].ID = %d, want %d (ISS-3, newest)", got[0].ID, r3.ID)
	}
	if got[1].ID != r2.ID {
		t.Errorf("got[1].ID = %d, want %d (ISS-2)", got[1].ID, r2.ID)
	}
	// r1 excluded by cap.
	for _, e := range got {
		if e.IssueID == r1.IssueID {
			t.Errorf("ISS-1 (r1) should be excluded by limit=2, but appears in result")
		}
		if e.IssueID == "ISS-OLD" {
			t.Errorf("ISS-OLD should be excluded by cutoff, but appears in result")
		}
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_IgnoresFailedAndEmptyWorkspace(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	// Excluded: failed run.
	appendOrFatal(t, s, storage.RunHistory{
		IssueID: "ISS-FAIL", Identifier: "PROJ-F", Attempt: 1,
		AgentAdapter: "mock", Workspace: "/ws/PROJ-F",
		StartedAt: recentCompletedAt(2), CompletedAt: recentCompletedAt(2),
		Status: "failed",
	})
	// Excluded: cancelled run.
	appendOrFatal(t, s, storage.RunHistory{
		IssueID: "ISS-CANCEL", Identifier: "PROJ-C", Attempt: 1,
		AgentAdapter: "mock", Workspace: "/ws/PROJ-C",
		StartedAt: recentCompletedAt(2), CompletedAt: recentCompletedAt(2),
		Status: "cancelled",
	})
	// Excluded: succeeded but empty workspace.
	appendOrFatal(t, s, storage.RunHistory{
		IssueID: "ISS-NOPATH", Identifier: "PROJ-NP", Attempt: 1,
		AgentAdapter: "mock", Workspace: "",
		StartedAt: recentCompletedAt(2), CompletedAt: recentCompletedAt(2),
		Status: "succeeded",
	})
	// Included: succeeded with non-empty workspace.
	eligible := appendRecoveryRun(t, s, "ISS-OK", "PROJ-OK", 1, recentCompletedAt(2))

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 1", len(got))
	}
	if got[0].IssueID != eligible.IssueID {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery().IssueID = %q, want %q", got[0].IssueID, eligible.IssueID)
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_PrefersProducingRunOverLaterFailedRun(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	producing := appendRecoveryRun(t, s, "ISS-EVIDENCE", "PROJ-E", 1, recentCompletedAt(2))
	appendOrFatal(t, s, storage.RunHistory{
		IssueID:      "ISS-EVIDENCE",
		Identifier:   "PROJ-E",
		Attempt:      2,
		AgentAdapter: "mock",
		Workspace:    "/ws/PROJ-E",
		StartedAt:    recentCompletedAt(1),
		CompletedAt:  recentCompletedAt(1),
		Status:       "failed",
		Error:        new("handoff withheld: absence of work observed under observed policy"),
	})

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 1", len(got))
	}
	if got[0].ID != producing.ID || got[0].Attempt != producing.Attempt {
		t.Errorf("recovery run = (id=%d, attempt=%d), want earlier successful run (id=%d, attempt=%d)",
			got[0].ID, got[0].Attempt, producing.ID, producing.Attempt)
	}
}

// TestLoadLatestSuccessfulRunsForReactionRecovery_SuppressedRunBecomesCandidate
// guards the MAX(id) selection this query already performs against a
// later change that adds a discriminator excluding a suppressed row (one
// whose handoff was withheld by evidence but suppressed by a verified
// terminal state, and which records "succeeded" with a nil error like
// any other run taking the terminal disposition). The selection itself
// is unchanged by that population's existence: this test exists to fail
// loudly if a later change carves it out of the query, not to endorse
// which run's attribution a rebuilt reaction entry inherits.
func TestLoadLatestSuccessfulRunsForReactionRecovery_SuppressedRunBecomesCandidate(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	appendRecoveryRun(t, s, "ISS-SUPPRESSED", "PROJ-S", 1, recentCompletedAt(2))
	suppressed := appendRecoveryRun(t, s, "ISS-SUPPRESSED", "PROJ-S", 2, recentCompletedAt(1))

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 1", len(got))
	}
	if got[0].ID != suppressed.ID || got[0].Attempt != suppressed.Attempt {
		t.Errorf("recovery run = (id=%d, attempt=%d), want the later succeeded run (id=%d, attempt=%d)",
			got[0].ID, got[0].Attempt, suppressed.ID, suppressed.Attempt)
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_DisplayIDRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	run := storage.RunHistory{
		IssueID:      "ISS-DISP",
		Identifier:   "PROJ-100",
		DisplayID:    "owner/repo#42",
		Attempt:      3,
		AgentAdapter: "mock",
		Workspace:    "/ws/PROJ-100",
		StartedAt:    recentCompletedAt(1),
		CompletedAt:  recentCompletedAt(1),
		Status:       "succeeded",
	}
	appendOrFatal(t, s, run)

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 1", len(got))
	}
	if got[0].DisplayID != "owner/repo#42" {
		t.Errorf("DisplayID = %q, want %q", got[0].DisplayID, "owner/repo#42")
	}
	if got[0].Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", got[0].Attempt)
	}
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_DBError(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)
	_, err := s.LoadLatestSuccessfulRunsForReactionRecovery(context.Background(), cutoff, 200)
	if err == nil {
		t.Fatal("LoadLatestSuccessfulRunsForReactionRecovery on closed DB = nil, want error")
	}
	if !strings.Contains(err.Error(), "load recovery runs") {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery error = %q, want wrapped 'load recovery runs'", err.Error())
	}
}

func TestSaveRetryEntry_DispatchColumnsRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	entry := storage.RetryEntry{
		IssueID:    "ISS-99",
		Identifier: "PROJ-99",
		Attempt:    2,
		DueAtMs:    9000,
		RuleName:   "bug-rule",
		TemplateID: "/abs/path/bug.md",
		AgentKind:  "claude-code",
	}
	if err := s.SaveRetryEntry(ctx, entry); err != nil {
		t.Fatalf("SaveRetryEntry: %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("LoadRetryEntries() count = %d, want 1", len(entries))
	}

	got := entries[0]
	if got.RuleName != "bug-rule" {
		t.Errorf("RuleName = %q, want %q", got.RuleName, "bug-rule")
	}
	if got.TemplateID != "/abs/path/bug.md" {
		t.Errorf("TemplateID = %q, want %q", got.TemplateID, "/abs/path/bug.md")
	}
	if got.AgentKind != "claude-code" {
		t.Errorf("AgentKind = %q, want %q", got.AgentKind, "claude-code")
	}
}

func TestSaveRetryEntry_LegacyRowsDefaultToEmptyDispatchFields(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Insert without dispatch columns to simulate legacy rows.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms)
		 VALUES ('legacy-1', 'MT-1', 1, 1000)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("LoadRetryEntries() count = %d, want 1", len(entries))
	}

	got := entries[0]
	if got.RuleName != "" {
		t.Errorf("legacy RuleName = %q, want empty string", got.RuleName)
	}
	if got.TemplateID != "" {
		t.Errorf("legacy TemplateID = %q, want empty string", got.TemplateID)
	}
	if got.AgentKind != "" {
		t.Errorf("legacy AgentKind = %q, want empty string", got.AgentKind)
	}
}

func TestAppendRunHistory_RuleNameRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := storage.RunHistory{
		IssueID:      "ISS-77",
		Identifier:   "PROJ-77",
		Attempt:      1,
		AgentAdapter: "claude-code",
		Workspace:    "/tmp/ws",
		StartedAt:    "2026-01-01T00:00:00Z",
		CompletedAt:  "2026-01-01T00:05:00Z",
		Status:       "succeeded",
		RuleName:     "docs-rule",
	}
	inserted, err := s.AppendRunHistory(ctx, run)
	if err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}

	entries, err := s.QueryRunHistoryByIssue(ctx, "ISS-77")
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("QueryRunHistoryByIssue() count = %d, want 1", len(entries))
	}

	got := entries[0]
	if got.RuleName != "docs-rule" {
		t.Errorf("RuleName = %q, want %q", got.RuleName, "docs-rule")
	}
	if got.ID != inserted.ID {
		t.Errorf("ID = %d, want %d", got.ID, inserted.ID)
	}
}
