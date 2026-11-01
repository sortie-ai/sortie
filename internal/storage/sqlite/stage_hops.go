package sqlite

import (
	"context"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.StageHopStore = (*Store)(nil)

// RecordStageHop upserts the hop record for hop.IssueID and deletes the
// issue's retry entry in one transaction. When the stored row carries the
// same SourceDispatchID, its HopCount is kept so one source run is counted
// once.
func (s *Store) RecordStageHop(ctx context.Context, hop storage.StageHop) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record stage hop %q: %w", hop.IssueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stage_hops (issue_id, identifier, hop_count, source_rule, target_rule, target_label, previous_outcome, source_dispatch_id, target_observed, hopped_at, chain_id, display_identifier, issue_title, issue_url, dispatched_at, run_dispatched, issue_type, issue_labels, issue_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			hopped_at          = excluded.hopped_at,
			chain_id           = excluded.chain_id,
			display_identifier = excluded.display_identifier,
			issue_title        = excluded.issue_title,
			issue_url          = excluded.issue_url,
			dispatched_at      = excluded.dispatched_at,
			run_dispatched     = excluded.run_dispatched,
			issue_type         = excluded.issue_type,
			issue_labels       = excluded.issue_labels,
			issue_state        = excluded.issue_state`,
		hop.IssueID, hop.Identifier, hop.HopCount, hop.SourceRule, hop.TargetRule, hop.TargetLabel,
		hop.PreviousOutcome, hop.SourceDispatchID, hop.TargetObserved, hop.HoppedAt, hop.ChainID,
		hop.DisplayID, hop.IssueTitle, hop.IssueURL, hop.DispatchedAt, hop.RunDispatched,
		hop.IssueType, encodeIssueLabels(hop.IssueLabels), hop.IssueState,
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

// MarkStageHopDispatched sets run_dispatched for the given issue ID. It is a
// no-op, not an error, when no row exists for that issue ID.
func (s *Store) MarkStageHopDispatched(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE stage_hops SET run_dispatched = 1 WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("mark stage hop dispatched %q: %w", issueID, err)
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
func (s *Store) ListStageHops(ctx context.Context) ([]storage.StageHop, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, identifier, hop_count, source_rule, target_rule, target_label, previous_outcome, source_dispatch_id, target_observed, hopped_at, chain_id,
			display_identifier, issue_title, issue_url, dispatched_at, run_dispatched, issue_type, issue_labels, issue_state
		FROM stage_hops
		ORDER BY issue_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list stage hops: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	hops := []storage.StageHop{}
	for rows.Next() {
		var h storage.StageHop
		var labels string
		if err := rows.Scan(&h.IssueID, &h.Identifier, &h.HopCount, &h.SourceRule, &h.TargetRule,
			&h.TargetLabel, &h.PreviousOutcome, &h.SourceDispatchID, &h.TargetObserved, &h.HoppedAt, &h.ChainID,
			&h.DisplayID, &h.IssueTitle, &h.IssueURL, &h.DispatchedAt, &h.RunDispatched,
			&h.IssueType, &labels, &h.IssueState); err != nil {
			return nil, fmt.Errorf("scan stage hop: %w", err)
		}
		h.IssueLabels = decodeIssueLabels(labels)
		hops = append(hops, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stage hops: %w", err)
	}
	return hops, nil
}
