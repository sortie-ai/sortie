package storage

import (
	"context"
	"time"
)

// AttentionAcknowledgement is one row of attention_acknowledgements: when a
// person acknowledged one attention item.
type AttentionAcknowledgement struct {
	ItemID         string
	AcknowledgedAt time.Time // stored as RFC 3339 with nanoseconds, UTC
}

// AttentionAcknowledgementStore records the acknowledged attention items.
type AttentionAcknowledgementStore interface {
	SaveAttentionAcknowledgement(ctx context.Context, ack AttentionAcknowledgement) error
	DeleteAttentionAcknowledgements(ctx context.Context, itemIDs []string) error
	ListAttentionAcknowledgements(ctx context.Context) ([]AttentionAcknowledgement, error)
}
