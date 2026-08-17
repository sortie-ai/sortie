package persistence

import (
	"context"
	"database/sql"
	"fmt"
)

// ParkedIssue is one row of the parked_issues table: an issue held out of
// primary dispatch until the orchestrator observes that a person acted.
type ParkedIssue struct {
	IssueID      string
	Identifier   string
	DisplayID    string // empty when the tracker needs no qualified form
	Reason       string // "agent_blocked" or "handoff_absence"
	ParkedState  string // tracker state observed at park time; empty when unobserved
	Label        string // parking label the orchestrator applied
	LabelApplied bool
	ParkedAt     string // RFC 3339
}

// UpsertParkedIssue persists a park record using upsert semantics. If a
// record for entry.IssueID already exists, it is replaced entirely.
func (s *Store) UpsertParkedIssue(ctx context.Context, entry ParkedIssue) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO parked_issues (issue_id, identifier, display_id, reason, parked_state, label, label_applied, parked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			identifier    = excluded.identifier,
			display_id    = excluded.display_id,
			reason        = excluded.reason,
			parked_state  = excluded.parked_state,
			label         = excluded.label,
			label_applied = excluded.label_applied,
			parked_at     = excluded.parked_at`,
		entry.IssueID, entry.Identifier, entry.DisplayID, entry.Reason, entry.ParkedState,
		entry.Label, entry.LabelApplied, entry.ParkedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert parked issue %q: %w", entry.IssueID, err)
	}
	return nil
}

// MarkParkedIssueLabelApplied sets label_applied for the given issue ID. It
// is a no-op, not an error, when no row exists for that issue ID.
func (s *Store) MarkParkedIssueLabelApplied(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE parked_issues SET label_applied = 1 WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("mark parked issue label applied %q: %w", issueID, err)
	}
	return nil
}

// DeleteParkedIssue removes the park record for the given issue ID. It is a
// no-op, not an error, when no row exists for that issue ID.
func (s *Store) DeleteParkedIssue(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM parked_issues WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete parked issue %q: %w", issueID, err)
	}
	return nil
}

// ListParkedIssues returns every persisted park record. Ordering is
// unspecified. Returns an empty slice (not nil) when no records exist.
func (s *Store) ListParkedIssues(ctx context.Context) ([]ParkedIssue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, identifier, display_id, reason, parked_state, label, label_applied, parked_at
		FROM parked_issues`)
	if err != nil {
		return nil, fmt.Errorf("list parked issues: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []ParkedIssue{}
	for rows.Next() {
		var e ParkedIssue
		var displayID sql.NullString
		if err := rows.Scan(&e.IssueID, &e.Identifier, &displayID, &e.Reason, &e.ParkedState,
			&e.Label, &e.LabelApplied, &e.ParkedAt); err != nil {
			return nil, fmt.Errorf("scan parked issue: %w", err)
		}
		e.DisplayID = displayID.String
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list parked issues: %w", err)
	}
	return entries, nil
}
