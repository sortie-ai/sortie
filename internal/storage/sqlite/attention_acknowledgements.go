package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.AttentionAcknowledgementStore = (*Store)(nil)

// SaveAttentionAcknowledgement persists an acknowledgement, replacing the row
// with the same item ID.
func (s *Store) SaveAttentionAcknowledgement(ctx context.Context, ack storage.AttentionAcknowledgement) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO attention_acknowledgements (item_id, acknowledged_at)
		VALUES (?, ?)
		ON CONFLICT (item_id) DO UPDATE SET
			acknowledged_at = excluded.acknowledged_at`,
		ack.ItemID, ack.AcknowledgedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save attention acknowledgement %q: %w", ack.ItemID, err)
	}
	return nil
}

// DeleteAttentionAcknowledgements removes the rows of the given item IDs in
// one statement. An empty list and an absent ID are no-ops, not errors.
func (s *Store) DeleteAttentionAcknowledgements(ctx context.Context, itemIDs []string) error {
	if len(itemIDs) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		args[i] = id
	}

	query := fmt.Sprintf( //nolint:gosec // placeholders is "?,?,..." built from len(itemIDs); values remain bound parameters
		`DELETE FROM attention_acknowledgements WHERE item_id IN (%s)`, placeholders)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete attention acknowledgements: %w", err)
	}
	return nil
}

// ListAttentionAcknowledgements returns every persisted acknowledgement.
// Ordering is unspecified. Returns an empty slice (not nil) when none exist. A
// row whose stored time does not parse is returned with a zero AcknowledgedAt,
// so one bad row does not fail the load of the others.
func (s *Store) ListAttentionAcknowledgements(ctx context.Context) ([]storage.AttentionAcknowledgement, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id, acknowledged_at FROM attention_acknowledgements`)
	if err != nil {
		return nil, fmt.Errorf("list attention acknowledgements: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	acknowledgements := []storage.AttentionAcknowledgement{}
	for rows.Next() {
		var ack storage.AttentionAcknowledgement
		var acknowledgedAt string
		if err := rows.Scan(&ack.ItemID, &acknowledgedAt); err != nil {
			return nil, fmt.Errorf("scan attention acknowledgement: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, acknowledgedAt); err == nil {
			ack.AcknowledgedAt = parsed.UTC()
		}
		acknowledgements = append(acknowledgements, ack)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attention acknowledgements: %w", err)
	}
	return acknowledgements, nil
}
