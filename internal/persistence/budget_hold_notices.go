package persistence

import (
	"context"
	"fmt"
)

// BudgetHoldNotice is one row of the budget_hold_notices table: the
// tracker notice already posted for the current budget hold on one issue.
type BudgetHoldNotice struct {
	IssueID   string
	Reason    string // "session_budget" or "token_budget"
	NoticedAt string // RFC 3339
}

// UpsertBudgetHoldNotice persists a budget hold notice record using upsert
// semantics. If a record for notice.IssueID already exists, it is replaced
// entirely, so a reason change overwrites in place.
func (s *Store) UpsertBudgetHoldNotice(ctx context.Context, notice BudgetHoldNotice) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO budget_hold_notices (issue_id, reason, noticed_at)
		VALUES (?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			reason     = excluded.reason,
			noticed_at = excluded.noticed_at`,
		notice.IssueID, notice.Reason, notice.NoticedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert budget hold notice %q: %w", notice.IssueID, err)
	}
	return nil
}

// DeleteBudgetHoldNotice removes the budget hold notice record for the
// given issue ID. It is a no-op, not an error, when no row exists for that
// issue ID.
func (s *Store) DeleteBudgetHoldNotice(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM budget_hold_notices WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete budget hold notice %q: %w", issueID, err)
	}
	return nil
}

// DeleteAllBudgetHoldNotices empties the budget_hold_notices table in one
// statement. It is a no-op, not an error, when the table is already empty.
func (s *Store) DeleteAllBudgetHoldNotices(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM budget_hold_notices`)
	if err != nil {
		return fmt.Errorf("delete all budget hold notices: %w", err)
	}
	return nil
}

// ListBudgetHoldNotices returns every persisted budget hold notice record.
// Ordering is unspecified. Returns an empty slice (not nil) when no records
// exist.
func (s *Store) ListBudgetHoldNotices(ctx context.Context) ([]BudgetHoldNotice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, reason, noticed_at FROM budget_hold_notices`)
	if err != nil {
		return nil, fmt.Errorf("list budget hold notices: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	notices := []BudgetHoldNotice{}
	for rows.Next() {
		var n BudgetHoldNotice
		if err := rows.Scan(&n.IssueID, &n.Reason, &n.NoticedAt); err != nil {
			return nil, fmt.Errorf("scan budget hold notice: %w", err)
		}
		notices = append(notices, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list budget hold notices: %w", err)
	}
	return notices, nil
}
