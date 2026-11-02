package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.ParkedIssueStore = (*Store)(nil)

// UpsertParkedIssue persists a park record using upsert semantics. If a
// record for entry.IssueID already exists, it is replaced entirely, release
// columns included.
func (s *Store) UpsertParkedIssue(ctx context.Context, entry storage.ParkedIssue) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO parked_issues (issue_id, identifier, display_id, reason, parked_state, label, label_applied, parked_at, issue_title, issue_url,
			rule_name, agent_kind, configured_model, stage_previous, stage_previous_outcome, chain_id, dispatched_at, issue_type, issue_labels,
			session_id, template_id, statement, last_prompt_at, released_at, release_gesture)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			identifier             = excluded.identifier,
			display_id             = excluded.display_id,
			reason                 = excluded.reason,
			parked_state           = excluded.parked_state,
			label                  = excluded.label,
			label_applied          = excluded.label_applied,
			parked_at              = excluded.parked_at,
			issue_title            = excluded.issue_title,
			issue_url              = excluded.issue_url,
			rule_name              = excluded.rule_name,
			agent_kind             = excluded.agent_kind,
			configured_model       = excluded.configured_model,
			stage_previous         = excluded.stage_previous,
			stage_previous_outcome = excluded.stage_previous_outcome,
			chain_id               = excluded.chain_id,
			dispatched_at          = excluded.dispatched_at,
			issue_type             = excluded.issue_type,
			issue_labels           = excluded.issue_labels,
			session_id             = excluded.session_id,
			template_id            = excluded.template_id,
			statement              = excluded.statement,
			last_prompt_at         = excluded.last_prompt_at,
			released_at            = excluded.released_at,
			release_gesture        = excluded.release_gesture`,
		entry.IssueID, entry.Identifier, entry.DisplayID, entry.Reason, entry.ParkedState,
		entry.Label, entry.LabelApplied, entry.ParkedAt, entry.IssueTitle, entry.IssueURL,
		entry.RuleName, entry.AgentKind, entry.ConfiguredModel, entry.StagePrevious,
		entry.StagePreviousOutcome, entry.ChainID, entry.DispatchedAt, entry.IssueType,
		encodeIssueLabels(entry.IssueLabels),
		entry.SessionID, entry.TemplateID, entry.Statement, entry.LastPromptAt, entry.ReleasedAt, entry.ReleaseGesture,
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
func (s *Store) ListParkedIssues(ctx context.Context) ([]storage.ParkedIssue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, identifier, display_id, reason, parked_state, label, label_applied, parked_at, issue_title, issue_url,
			rule_name, agent_kind, configured_model, stage_previous, stage_previous_outcome, chain_id, dispatched_at, issue_type, issue_labels,
			session_id, template_id, statement, last_prompt_at, released_at, release_gesture
		FROM parked_issues`)
	if err != nil {
		return nil, fmt.Errorf("list parked issues: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []storage.ParkedIssue{}
	for rows.Next() {
		var e storage.ParkedIssue
		var displayID sql.NullString
		var labels string
		if err := rows.Scan(&e.IssueID, &e.Identifier, &displayID, &e.Reason, &e.ParkedState,
			&e.Label, &e.LabelApplied, &e.ParkedAt, &e.IssueTitle, &e.IssueURL,
			&e.RuleName, &e.AgentKind, &e.ConfiguredModel, &e.StagePrevious, &e.StagePreviousOutcome,
			&e.ChainID, &e.DispatchedAt, &e.IssueType, &labels,
			&e.SessionID, &e.TemplateID, &e.Statement, &e.LastPromptAt, &e.ReleasedAt, &e.ReleaseGesture); err != nil {
			return nil, fmt.Errorf("scan parked issue: %w", err)
		}
		e.DisplayID = displayID.String
		e.IssueLabels = decodeIssueLabels(labels)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list parked issues: %w", err)
	}
	return entries, nil
}
