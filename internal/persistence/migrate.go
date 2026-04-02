package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Migrate applies all pending schema migrations in order. It creates the
// schema_migrations tracking table if it does not exist. Each migration runs
// inside its own transaction for atomicity. Already-applied migrations are
// skipped, making Migrate safe to call on every startup. Migrate must be
// called before any other [Store] operations.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT    NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	versions := make([]int, len(migrations))
	for i, m := range migrations {
		versions[i] = m.Version
	}
	if !slices.IsSorted(versions) {
		return fmt.Errorf("migrations are not sorted by version: %v", versions)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	maxRegistered := versions[len(versions)-1]
	for v := range applied {
		if v > maxRegistered {
			return fmt.Errorf("database has migration %d, but this binary only knows up to %d — refusing to run against a newer schema", v, maxRegistered)
		}
	}

	for _, m := range migrations {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	applied := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func (s *Store) applyMigration(ctx context.Context, m Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d (%s): %w", m.Version, m.Description, err)
	}
	// Rollback is a no-op after successful Commit.
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	if err := execStatements(ctx, tx, m.SQL); err != nil {
		return fmt.Errorf("execute migration %d (%s): %w", m.Version, m.Description, err)
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		m.Version, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("record migration %d (%s): %w", m.Version, m.Description, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d (%s): %w", m.Version, m.Description, err)
	}
	return nil
}

// execStatements splits a multi-statement SQL string on semicolons and
// executes each non-empty statement individually within the given transaction.
// This is required because database/sql drivers have inconsistent support for
// multi-statement Exec calls.
//
// Limitation: the naive split on ";" does not handle semicolons inside string
// literals. This is safe for DDL statements (CREATE TABLE, CREATE INDEX) but
// would mis-split DML containing literal semicolons.
func execStatements(ctx context.Context, tx *sql.Tx, rawSQL string) error {
	for _, stmt := range strings.Split(rawSQL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
