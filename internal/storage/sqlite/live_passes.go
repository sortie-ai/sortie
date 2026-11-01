package sqlite

import (
	"context"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.LivePassStore = (*Store)(nil)

// UpsertLivePass stores pass, replacing the row of its issue and kind.
func (s *Store) UpsertLivePass(ctx context.Context, pass storage.LivePass) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO live_passes (issue_id, kind, chain_id, rule_name, stage_previous)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (issue_id, kind) DO UPDATE SET
			chain_id       = excluded.chain_id,
			rule_name      = excluded.rule_name,
			stage_previous = excluded.stage_previous`,
		pass.IssueID, pass.Kind, pass.ChainID, pass.RuleName, pass.StagePrevious)
	if err != nil {
		return fmt.Errorf("upsert live pass %q %q: %w", pass.IssueID, pass.Kind, err)
	}
	return nil
}

// DeleteLivePass removes the row of the issue and kind. It is a no-op, not an
// error, when no row exists.
func (s *Store) DeleteLivePass(ctx context.Context, issueID, kind string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM live_passes WHERE issue_id = ? AND kind = ?`, issueID, kind)
	if err != nil {
		return fmt.Errorf("delete live pass %q %q: %w", issueID, kind, err)
	}
	return nil
}

// DeleteAllLivePasses removes every live pass row.
func (s *Store) DeleteAllLivePasses(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM live_passes`); err != nil {
		return fmt.Errorf("delete all live passes: %w", err)
	}
	return nil
}

// ListLivePasses returns every live_passes row and, as kind [storage.LivePassRetry],
// every retry_entries row, ordered by issue ID then kind. Both tables are
// read in one statement so the list is a single instant. It selects only the
// columns named here, so a read-only open of a database that a later
// migration extends still answers. Returns an empty slice (not nil) when none
// exist.
func (s *Store) ListLivePasses(ctx context.Context) ([]storage.LivePass, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, kind, chain_id, rule_name, stage_previous FROM live_passes
		UNION ALL
		SELECT issue_id, ?, chain_id, rule_name, stage_previous FROM retry_entries
		ORDER BY 1, 2`, storage.LivePassRetry)
	if err != nil {
		return nil, fmt.Errorf("list live passes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	passes := []storage.LivePass{}
	for rows.Next() {
		var p storage.LivePass
		if err := rows.Scan(&p.IssueID, &p.Kind, &p.ChainID, &p.RuleName, &p.StagePrevious); err != nil {
			return nil, fmt.Errorf("scan live pass: %w", err)
		}
		passes = append(passes, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list live passes: %w", err)
	}
	return passes, nil
}
