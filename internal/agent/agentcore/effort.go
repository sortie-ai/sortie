package agentcore

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/registry"
)

// EffortNotAppliedReason names why a configured reasoning level did not take
// effect in a session. The set is closed.
type EffortNotAppliedReason string

const (
	// EffortNoOption means the session offers no reasoning option to set.
	EffortNoOption EffortNotAppliedReason = "no_option"

	// EffortNotOffered means the session's reasoning option does not list
	// the configured level.
	EffortNotOffered EffortNotAppliedReason = "not_offered"

	// EffortRejected means the runtime refused the level.
	EffortRejected EffortNotAppliedReason = "rejected"

	// EffortNoAnswer means the runtime did not answer the request to set
	// the level within the read bound.
	EffortNoAnswer EffortNotAppliedReason = "no_answer"

	// EffortOtherValue means the runtime answered but reports a level other
	// than the configured one as current.
	EffortOtherValue EffortNotAppliedReason = "other_value"
)

// WarnEffortNotApplied writes the one warning record every agent kind uses
// when a configured level did not reach its runtime: kind, level and reason,
// then detail in order. A nil logger means [slog.Default]. It latches
// nothing, so each caller reports at most once per session.
func WarnEffortNotApplied(logger *slog.Logger, kind, level string, reason EffortNotAppliedReason, detail ...slog.Attr) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := append([]slog.Attr{
		slog.String("agent_kind", kind),
		slog.String(registry.EffortKey, level),
		slog.String("reason", string(reason)),
	}, detail...)
	logger.LogAttrs(context.Background(), slog.LevelWarn, "reasoning level not applied by the agent", attrs...)
}
