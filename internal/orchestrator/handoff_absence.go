package orchestrator

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	defaultHandoffAbsenceCeiling = 3
	defaultHandoffParkingLabel   = "needs-human"
	reviewCommentsConfigKey      = "review_comments"
)

// handoffAbsenceCeiling resolves the configured consecutive-absence
// ceiling, falling back to defaultHandoffAbsenceCeiling when configured
// is not positive. The fallback exists for a configuration value that
// did not come through the parse path, such as a zero-value
// config.ServiceConfig{}, which is where zero can still arrive despite
// the parsed configuration always carrying a positive value.
func handoffAbsenceCeiling(configured int) int {
	if configured > 0 {
		return configured
	}
	return defaultHandoffAbsenceCeiling
}

// resolveHandoffParkingLabel captures only the review-comments escalation
// label name. The reaction need not be active and its escalation action does
// not participate in primary-dispatch parking.
func resolveHandoffParkingLabel(reactions map[string]config.ReactionConfig) string {
	if review, ok := reactions[reviewCommentsConfigKey]; ok && review.EscalationLabel != "" {
		return review.EscalationLabel
	}
	return defaultHandoffParkingLabel
}

// parkHandoffAbsence records an absence park through [parkIssue].
// observedState is empty only on the retry lane, which parks before its
// tracker fetch.
func parkHandoffAbsence(
	state *State,
	ctx context.Context,
	store ParkStore,
	tracker domain.TrackerAdapter,
	metrics domain.Metrics,
	issueID, identifier, displayID, observedState string,
	count, ceiling int,
	label string,
	log *slog.Logger,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	if label == "" {
		label = defaultHandoffParkingLabel
	}

	parkIssue(state, parkIssueParams{
		IssueID:        issueID,
		Identifier:     identifier,
		DisplayID:      displayID,
		ObservedState:  observedState,
		Reason:         parkReasonHandoffAbsence,
		Label:          label,
		Absences:       count,
		Ceiling:        ceiling,
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        metrics,
		Logger:         log,
		Ctx:            ctx,
	})
}
