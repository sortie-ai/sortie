package storage

import (
	"context"
)

// ReactionHandoffStore records the comment IDs handed to an issue's reaction
// runs.
type ReactionHandoffStore interface {
	AddReactionHandedOffComments(ctx context.Context, issueID, kind string, commentIDs []string) error
	ListReactionHandedOffComments(ctx context.Context, issueID, kind string) ([]string, error)
}
