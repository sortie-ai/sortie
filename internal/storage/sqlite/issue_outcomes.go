package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.IssueOutcomeStore = (*Store)(nil)

// RecordIssueOutcome stores o as the issue's outcome in one statement. An
// existing row is replaced only when o is a handoff, when the stored row is a
// handoff, or when a stored terminal outcome meets a merged one; in every
// other pair the stored row stays, so a merge is never replaced by a terminal
// read and a first observation keeps its time.
func (s *Store) RecordIssueOutcome(ctx context.Context, o storage.IssueOutcome) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO issue_outcomes (issue_id, kind, observed_at, state, merge_reaction_kind, pr_number, pr_repository)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			kind                = excluded.kind,
			observed_at         = excluded.observed_at,
			state               = excluded.state,
			merge_reaction_kind = excluded.merge_reaction_kind,
			pr_number           = excluded.pr_number,
			pr_repository       = excluded.pr_repository
		WHERE excluded.kind = 'handed_off'
		   OR issue_outcomes.kind = 'handed_off'
		   OR (issue_outcomes.kind = 'terminal' AND excluded.kind = 'merged')`,
		o.IssueID, o.Kind, o.ObservedAt, o.State, o.MergeReactionKind, o.PRNumber, o.PRRepository,
	)
	if err != nil {
		return fmt.Errorf("record issue outcome %q: %w", o.IssueID, err)
	}
	return nil
}

// DeleteIssueOutcome removes the issue's outcome. It is a no-op, not an
// error, when no row exists for that issue ID.
func (s *Store) DeleteIssueOutcome(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM issue_outcomes WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete issue outcome %q: %w", issueID, err)
	}
	return nil
}

// IssueOutcomes returns the stored outcome of each listed issue that has one,
// keyed by issue ID. An empty list reads nothing and returns an empty,
// non-nil map.
func (s *Store) IssueOutcomes(ctx context.Context, issueIDs []string) (map[string]storage.IssueOutcome, error) {
	outcomes := make(map[string]storage.IssueOutcome, len(issueIDs))
	if len(issueIDs) == 0 {
		return outcomes, nil
	}

	placeholders := strings.Repeat("?,", len(issueIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(issueIDs))
	for i, id := range issueIDs {
		args[i] = id
	}

	query := fmt.Sprintf( //nolint:gosec // placeholders is built only from len(issueIDs); values remain bound parameters
		`SELECT issue_id, kind, observed_at, state, merge_reaction_kind, pr_number, pr_repository
		FROM issue_outcomes
		WHERE issue_id IN (%s)`,
		placeholders,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query issue outcomes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var o storage.IssueOutcome
		if err := rows.Scan(&o.IssueID, &o.Kind, &o.ObservedAt, &o.State, &o.MergeReactionKind, &o.PRNumber, &o.PRRepository); err != nil {
			return nil, fmt.Errorf("scan issue outcome: %w", err)
		}
		outcomes[o.IssueID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query issue outcomes: %w", err)
	}
	return outcomes, nil
}
