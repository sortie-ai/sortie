package persistence

import (
	"context"
	"fmt"
)

// StageHop is one row of the stage_hops table: an issue's consecutive
// automatic stage hops since the last reset, and the latest hop.
type StageHop struct {
	IssueID          string
	Identifier       string
	HopCount         int
	SourceRule       string
	TargetRule       string
	TargetLabel      string
	PreviousOutcome  string // "succeeded" or "no_change"
	SourceDispatchID string // dispatch of the run whose exit made the hop
	TargetObserved   bool   // a read has shown TargetLabel on the issue
	HoppedAt         string // RFC 3339
}

// RecordStageHop upserts the hop record for hop.IssueID and deletes the
// issue's retry entry in one transaction. When the stored row carries the
// same SourceDispatchID, its HopCount is kept so one source run is counted
// once.
func (s *Store) RecordStageHop(ctx context.Context, hop StageHop) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record stage hop %q: %w", hop.IssueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stage_hops (issue_id, identifier, hop_count, source_rule, target_rule, target_label, previous_outcome, source_dispatch_id, target_observed, hopped_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			identifier         = excluded.identifier,
			hop_count          = CASE WHEN stage_hops.source_dispatch_id = excluded.source_dispatch_id
			                          THEN stage_hops.hop_count ELSE excluded.hop_count END,
			source_rule        = excluded.source_rule,
			target_rule        = excluded.target_rule,
			target_label       = excluded.target_label,
			previous_outcome   = excluded.previous_outcome,
			source_dispatch_id = excluded.source_dispatch_id,
			target_observed    = excluded.target_observed,
			hopped_at          = excluded.hopped_at`,
		hop.IssueID, hop.Identifier, hop.HopCount, hop.SourceRule, hop.TargetRule, hop.TargetLabel,
		hop.PreviousOutcome, hop.SourceDispatchID, hop.TargetObserved, hop.HoppedAt,
	); err != nil {
		return fmt.Errorf("upsert stage hop %q: %w", hop.IssueID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retry_entries WHERE issue_id = ?`, hop.IssueID); err != nil {
		return fmt.Errorf("delete retry entry for stage hop %q: %w", hop.IssueID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record stage hop %q: %w", hop.IssueID, err)
	}
	return nil
}

// MarkStageHopObserved sets target_observed for the given issue ID. It is a
// no-op, not an error, when no row exists for that issue ID.
func (s *Store) MarkStageHopObserved(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE stage_hops SET target_observed = 1 WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("mark stage hop observed %q: %w", issueID, err)
	}
	return nil
}

// DeleteStageHop removes the hop record for the given issue ID. It is a
// no-op, not an error, when no row exists for that issue ID.
func (s *Store) DeleteStageHop(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM stage_hops WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete stage hop %q: %w", issueID, err)
	}
	return nil
}

// ListStageHops returns every persisted hop record ordered by issue_id.
// Returns an empty slice (not nil) when no records exist.
func (s *Store) ListStageHops(ctx context.Context) ([]StageHop, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, identifier, hop_count, source_rule, target_rule, target_label, previous_outcome, source_dispatch_id, target_observed, hopped_at
		FROM stage_hops
		ORDER BY issue_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list stage hops: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	hops := []StageHop{}
	for rows.Next() {
		var h StageHop
		if err := rows.Scan(&h.IssueID, &h.Identifier, &h.HopCount, &h.SourceRule, &h.TargetRule,
			&h.TargetLabel, &h.PreviousOutcome, &h.SourceDispatchID, &h.TargetObserved, &h.HoppedAt); err != nil {
			return nil, fmt.Errorf("scan stage hop: %w", err)
		}
		hops = append(hops, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stage hops: %w", err)
	}
	return hops, nil
}
