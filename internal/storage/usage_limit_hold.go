package storage

import (
	"context"
)

// UsageLimitHold is one persisted usage-limit hold.
type UsageLimitHold struct {
	AgentKind   string
	ModelOnly   bool   // Stored as 0 or 1.
	Model       string // Empty for a kind-wide hold and for the runtime's default model.
	ResetsAtMs  int64  // Unix epoch milliseconds.
	HeldSinceMs int64  // Unix epoch milliseconds.
}

// UsageLimitHoldStore records the usage-limit holds of agent kinds and models.
type UsageLimitHoldStore interface {
	SaveUsageLimitHold(ctx context.Context, hold UsageLimitHold) error
	LoadUsageLimitHolds(ctx context.Context) ([]UsageLimitHold, error)
	DeleteUsageLimitHold(ctx context.Context, agentKind string, modelOnly bool, model string) error
}
