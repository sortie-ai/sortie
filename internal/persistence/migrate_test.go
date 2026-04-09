package persistence

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { closeStore(t, s) })
	return s
}

func migrateOrFatal(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

func TestMigrate_FreshDB(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	want := map[string]bool{
		"schema_migrations": false,
		"retry_entries":     false,
		"run_history":       false,
		"session_metadata":  false,
		"aggregate_metrics": false,
		"sqlite_sequence":   false, // created by AUTOINCREMENT
	}

	rows, err := s.db.QueryContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close() //nolint:errcheck // test helper

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	for _, name := range got {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}

	for name, found := range want {
		if name == "sqlite_sequence" {
			continue // optional internal table
		}
		if !found {
			t.Errorf("table %q not found after Migrate", name)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var count int
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Errorf("schema_migrations row count = %d, want %d", count, len(migrations))
	}
}

func TestMigrate_SchemaMigrationsTracking(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	var version int
	var appliedAt string
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT version, applied_at FROM schema_migrations WHERE version = 1",
	).Scan(&version, &appliedAt); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}

	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if appliedAt == "" {
		t.Fatal("applied_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, appliedAt); err != nil {
		t.Errorf("applied_at %q is not valid RFC 3339: %v", appliedAt, err)
	}
}

// columnInfo mirrors the output of PRAGMA table_info.
type columnInfo struct {
	CID       int
	Name      string
	Type      string
	NotNull   bool
	DefaultAt *string
	PK        int
}

func tableColumns(t *testing.T, s *Store, table string) []columnInfo {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		"PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close() //nolint:errcheck // test helper

	var cols []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &c.DefaultAt, &c.PK); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration table_info(%s): %v", table, err)
	}
	return cols
}

func TestMigrate_ColumnCorrectness(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	type colSpec struct {
		Name    string
		Type    string
		NotNull bool
		PK      int // 0 = not PK, 1+ = PK position
	}

	tests := []struct {
		table string
		cols  []colSpec
	}{
		{
			table: "retry_entries",
			cols: []colSpec{
				{"issue_id", "TEXT", false, 1},
				{"identifier", "TEXT", true, 0},
				{"attempt", "INTEGER", true, 0},
				{"due_at_ms", "INTEGER", true, 0},
				{"error", "TEXT", false, 0},
			},
		},
		{
			table: "run_history",
			cols: []colSpec{
				{"id", "INTEGER", false, 1},
				{"issue_id", "TEXT", true, 0},
				{"identifier", "TEXT", true, 0},
				{"attempt", "INTEGER", true, 0},
				{"agent_adapter", "TEXT", true, 0},
				{"workspace", "TEXT", true, 0},
				{"started_at", "TEXT", true, 0},
				{"completed_at", "TEXT", true, 0},
				{"status", "TEXT", true, 0},
				{"error", "TEXT", false, 0},
				{"workflow_file", "TEXT", false, 0},
				{"turns_completed", "INTEGER", true, 0},
				{"display_identifier", "TEXT", false, 0},
				{"review_metadata", "TEXT", false, 0},
			},
		},
		{
			table: "session_metadata",
			cols: []colSpec{
				{"issue_id", "TEXT", false, 1},
				{"session_id", "TEXT", true, 0},
				{"agent_pid", "TEXT", false, 0},
				{"input_tokens", "INTEGER", true, 0},
				{"output_tokens", "INTEGER", true, 0},
				{"total_tokens", "INTEGER", true, 0},
				{"updated_at", "TEXT", true, 0},
				{"cache_read_tokens", "INTEGER", true, 0},
				{"model_name", "TEXT", true, 0},
				{"api_request_count", "INTEGER", true, 0},
			},
		},
		{
			table: "aggregate_metrics",
			cols: []colSpec{
				{"key", "TEXT", false, 1},
				{"input_tokens", "INTEGER", true, 0},
				{"output_tokens", "INTEGER", true, 0},
				{"total_tokens", "INTEGER", true, 0},
				{"seconds_running", "REAL", true, 0},
				{"updated_at", "TEXT", true, 0},
				{"cache_read_tokens", "INTEGER", true, 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			got := tableColumns(t, s, tt.table)

			if len(got) != len(tt.cols) {
				var names []string
				for _, c := range got {
					names = append(names, c.Name)
				}
				t.Fatalf("column count = %d, want %d; got columns: %v",
					len(got), len(tt.cols), names)
			}

			for i, want := range tt.cols {
				g := got[i]
				if g.Name != want.Name {
					t.Errorf("column[%d].Name = %q, want %q", i, g.Name, want.Name)
				}
				if !strings.EqualFold(g.Type, want.Type) {
					t.Errorf("column %q Type = %q, want %q", g.Name, g.Type, want.Type)
				}
				if g.NotNull != want.NotNull {
					t.Errorf("column %q NotNull = %v, want %v", g.Name, g.NotNull, want.NotNull)
				}
				if g.PK != want.PK {
					t.Errorf("column %q PK = %d, want %d", g.Name, g.PK, want.PK)
				}
			}
		})
	}
}

func TestMigrate_RunHistoryAutoincrement(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	var ddl string
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT sql FROM sqlite_master WHERE name='run_history'",
	).Scan(&ddl); err != nil {
		t.Fatalf("query run_history DDL: %v", err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		t.Errorf("run_history DDL missing AUTOINCREMENT:\n%s", ddl)
	}
}

func TestMigrate_DefaultValues(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	ctx := context.Background()

	// Insert minimal rows to exercise defaults.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_metadata (issue_id, session_id, updated_at)
		 VALUES ('test-1', 'sess-1', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert session_metadata: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO aggregate_metrics (key, updated_at)
		 VALUES ('agent_totals', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert aggregate_metrics: %v", err)
	}

	var inTok, outTok, totTok int
	if err := s.db.QueryRowContext(ctx,
		"SELECT input_tokens, output_tokens, total_tokens FROM session_metadata WHERE issue_id='test-1'",
	).Scan(&inTok, &outTok, &totTok); err != nil {
		t.Fatalf("query session_metadata defaults: %v", err)
	}
	if inTok != 0 || outTok != 0 || totTok != 0 {
		t.Errorf("session_metadata token defaults = (%d, %d, %d), want (0, 0, 0)", inTok, outTok, totTok)
	}

	var secRunning float64
	if err := s.db.QueryRowContext(ctx,
		"SELECT seconds_running FROM aggregate_metrics WHERE key='agent_totals'",
	).Scan(&secRunning); err != nil {
		t.Fatalf("query aggregate_metrics defaults: %v", err)
	}
	if secRunning != 0.0 {
		t.Errorf("aggregate_metrics.seconds_running default = %f, want 0.0", secRunning)
	}
}

func TestMigrate_NullConstraints(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	ctx := context.Background()

	// retry_entries.error is nullable.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms, error)
		 VALUES ('re-1', 'MT-1', 1, 1000, NULL)`); err != nil {
		t.Errorf("retry_entries.error should accept NULL: %v", err)
	}

	// run_history.error is nullable.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO run_history (issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status, error)
		 VALUES ('rh-1', 'MT-1', 1, 'mock', '/tmp', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', 'succeeded', NULL)`); err != nil {
		t.Errorf("run_history.error should accept NULL: %v", err)
	}

	// session_metadata.agent_pid is nullable.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_metadata (issue_id, session_id, agent_pid, updated_at)
		 VALUES ('sm-1', 'sess-1', NULL, '2026-01-01T00:00:00Z')`); err != nil {
		t.Errorf("session_metadata.agent_pid should accept NULL: %v", err)
	}

	// retry_entries.identifier is NOT NULL — insertion without it must fail.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms)
		 VALUES ('re-2', NULL, 1, 1000)`)
	if err == nil {
		t.Error("retry_entries.identifier should reject NULL")
	}
}

func TestMigrate_PrimaryKeyConstraints(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	ctx := context.Background()

	// Insert first row.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms)
		 VALUES ('pk-1', 'MT-1', 1, 1000)`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Duplicate PK must fail.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms)
		 VALUES ('pk-1', 'MT-2', 2, 2000)`)
	if err == nil {
		t.Error("duplicate primary key should be rejected")
	}
}

// Verify the migrations slice is correctly ordered and non-empty.
func TestMigrations_Registry(t *testing.T) {
	t.Parallel()

	if len(migrations) == 0 {
		t.Fatal("migrations slice is empty")
	}

	versions := make([]int, len(migrations))
	for i, m := range migrations {
		versions[i] = m.Version
		if m.SQL == "" {
			t.Errorf("migration %d has empty SQL", m.Version)
		}
		if m.Description == "" {
			t.Errorf("migration %d has empty Description", m.Version)
		}
	}

	if !sort.IntsAreSorted(versions) {
		t.Errorf("migrations not sorted by version: %v", versions)
	}
	if versions[0] != 1 {
		t.Errorf("first migration version = %d, want 1", versions[0])
	}
}

// TestMigrate_Migration002_Defaults verifies that migration 002 adds the
// extended token metric columns with correct defaults. Rows inserted after
// migration 002 that omit these columns receive zero/empty defaults.
func TestMigrate_Migration002_Defaults(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	// Insert minimal rows exercising only the original columns.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_metadata (issue_id, session_id, updated_at)
		 VALUES ('m2-1', 'sess-1', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert session_metadata: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO aggregate_metrics (key, updated_at)
		 VALUES ('agent_totals', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert aggregate_metrics: %v", err)
	}

	// Verify session_metadata new column defaults.
	var cacheRead int64
	var modelName string
	var apiReqCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT cache_read_tokens, model_name, api_request_count
		 FROM session_metadata WHERE issue_id='m2-1'`,
	).Scan(&cacheRead, &modelName, &apiReqCount); err != nil {
		t.Fatalf("query session_metadata new columns: %v", err)
	}
	if cacheRead != 0 {
		t.Errorf("session_metadata.cache_read_tokens default = %d, want 0", cacheRead)
	}
	if modelName != "" {
		t.Errorf("session_metadata.model_name default = %q, want empty", modelName)
	}
	if apiReqCount != 0 {
		t.Errorf("session_metadata.api_request_count default = %d, want 0", apiReqCount)
	}

	// Verify aggregate_metrics new column default.
	var aggCacheRead int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT cache_read_tokens FROM aggregate_metrics WHERE key='agent_totals'`,
	).Scan(&aggCacheRead); err != nil {
		t.Fatalf("query aggregate_metrics new column: %v", err)
	}
	if aggCacheRead != 0 {
		t.Errorf("aggregate_metrics.cache_read_tokens default = %d, want 0", aggCacheRead)
	}
}

// TestMigrate_Migration002_SchemaMigrationsTracking verifies that
// migration 002 is tracked in the schema_migrations table.
func TestMigrate_Migration002_SchemaMigrationsTracking(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)

	var version int
	var appliedAt string
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT version, applied_at FROM schema_migrations WHERE version = 2",
	).Scan(&version, &appliedAt); err != nil {
		t.Fatalf("query schema_migrations version 2: %v", err)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if appliedAt == "" {
		t.Fatal("applied_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, appliedAt); err != nil {
		t.Errorf("applied_at %q is not valid RFC 3339: %v", appliedAt, err)
	}
}
