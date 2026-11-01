package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.ReactionTriageOutcomeStore = (*Store)(nil)

// RecordReactionTriageOutcome appends one triage answer. Rows are never
// updated or removed.
func (s *Store) RecordReactionTriageOutcome(ctx context.Context, outcome storage.ReactionTriageOutcome) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO reaction_triage_outcomes
			(issue_id, kind, fingerprint, disposition, fallback, elapsed_ms, chain_id, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		outcome.IssueID, outcome.Kind, outcome.Fingerprint, outcome.Disposition, outcome.Fallback,
		outcome.Elapsed.Milliseconds(), outcome.ChainID, outcome.RecordedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record reaction triage outcome %q of %q: %w", outcome.Disposition, outcome.IssueID, err)
	}
	return nil
}

// LatestReactionTriageDisposition returns the disposition of the newest answer
// recorded for exactly the given issue, kind, and fingerprint. found is false,
// with a nil error, when none was recorded.
func (s *Store) LatestReactionTriageDisposition(ctx context.Context, issueID, kind, fingerprint string) (disposition string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT disposition FROM reaction_triage_outcomes
		WHERE issue_id = ? AND kind = ? AND fingerprint = ?
		ORDER BY id DESC LIMIT 1`,
		issueID, kind, fingerprint,
	).Scan(&disposition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read reaction triage outcome of %q: %w", issueID, err)
	}
	return disposition, true, nil
}
