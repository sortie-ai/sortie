package agentcore

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/registry"
)

// NotAppliedReason names why a configured session value did not take effect
// in a session. The set is closed.
type NotAppliedReason string

const (
	// NotAppliedNoOption means the session offers no option to set.
	NotAppliedNoOption NotAppliedReason = "no_option"

	// NotAppliedNotOffered means the session's option does not list the
	// configured value.
	NotAppliedNotOffered NotAppliedReason = "not_offered"

	// NotAppliedRejected means the runtime refused the value.
	NotAppliedRejected NotAppliedReason = "rejected"

	// NotAppliedNoAnswer means the runtime did not answer the request to set
	// the value within the read bound.
	NotAppliedNoAnswer NotAppliedReason = "no_answer"

	// NotAppliedOtherValue means the runtime answered but reports a value
	// other than the configured one as current.
	NotAppliedOtherValue NotAppliedReason = "other_value"
)

const (
	effortNotAppliedMessage        = "reasoning level not applied by the agent"
	sessionOptionNotAppliedMessage = "session option not applied by the agent"
)

// WarnEffortNotApplied writes the one warning record every agent kind uses
// when a configured level did not reach its runtime: kind, level and reason,
// then detail in order. A nil logger means [slog.Default]. It latches
// nothing, so each caller reports at most once per session.
func WarnEffortNotApplied(logger *slog.Logger, kind, level string, reason NotAppliedReason, detail ...slog.Attr) {
	attrs := append([]slog.Attr{
		slog.String("agent_kind", kind),
		slog.String(registry.EffortKey, level),
		slog.String("reason", string(reason)),
	}, detail...)
	loggerOrDefault(logger).LogAttrs(context.Background(), slog.LevelWarn, effortNotAppliedMessage, attrs...)
}

// WarnSessionOptionNotApplied writes the one warning record every agent kind
// uses when a configured session option value did not reach its runtime:
// kind, option, value and reason, then detail in order. A nil logger means
// [slog.Default]. It latches nothing, so each caller reports at most once per
// option and session.
func WarnSessionOptionNotApplied(logger *slog.Logger, kind, option, value string, reason NotAppliedReason, detail ...slog.Attr) {
	attrs := append([]slog.Attr{
		slog.String("agent_kind", kind),
		slog.String("option", option),
		slog.String("value", value),
		slog.String("reason", string(reason)),
	}, detail...)
	loggerOrDefault(logger).LogAttrs(context.Background(), slog.LevelWarn, sessionOptionNotAppliedMessage, attrs...)
}

func loggerOrDefault(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}
