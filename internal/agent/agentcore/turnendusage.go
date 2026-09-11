package agentcore

import (
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// RecoveredUsage is the usage figure one turn's recovery produced after
// the turn's work was over.
type RecoveredUsage struct {
	// Run is the session's run-cumulative usage as the recovery computed
	// it: every turn since the session started, excluding usage a
	// resumed session accumulated earlier.
	Run domain.TokenUsage

	// Model names the model that produced Run, or is empty when the
	// recovery's source names none. Finalize trims surrounding
	// whitespace before reporting it.
	Model string
}

// TurnEndUsage is the usage state of one session of a kind whose figure
// settles only after a turn's work is over. It owns the session's
// run-cumulative snapshot and measurement verdict, and reports each
// recovered figure as the turn's one usage event. One value serves one
// session; it is not safe for concurrent use.
type TurnEndUsage struct {
	acc      RunUsage
	measured bool
}

// NewTurnEndUsage returns the usage state of one new session: a zero
// snapshot and no measurement.
func NewTurnEndUsage() *TurnEndUsage {
	return &TurnEndUsage{}
}

// Snapshot returns the session's run-cumulative snapshot: the highest
// figure any turn has settled, or the zero value before the first.
func (u *TurnEndUsage) Snapshot() domain.TokenUsage {
	return u.acc.Snapshot()
}

// Finalize ends one turn. recovered is nil when the turn produced no
// figure. When recovered is non-nil, Finalize settles it into the
// session's run-cumulative snapshot, latches the measured verdict, and
// emits exactly one domain.EventTokenUsage event carrying that snapshot
// and the trimmed model name, before delegating to [FinalizeTurn] for
// the turn's terminal event.
//
// Finalize panics when emit is nil. It substitutes [slog.Default] when
// logger is nil. It is not idempotent; callers MUST call it at most once
// per turn.
func (u *TurnEndUsage) Finalize(
	emit func(domain.AgentEvent),
	logger *slog.Logger,
	ev TurnEvidence,
	sessionID string,
	apiDurationMS int64,
	recovered *RecoveredUsage,
) (domain.TurnResult, *domain.AgentError) {
	if emit == nil {
		panic("agentcore: TurnEndUsage.Finalize: emit must be non-nil")
	}

	if recovered != nil {
		snapshot := u.acc.SetRunCumulative(recovered.Run)
		u.measured = true
		emit(domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     snapshot,
			Model:     strings.TrimSpace(recovered.Model),
		})
	}

	meta := TurnMeta{
		SessionID:     sessionID,
		Usage:         u.acc.Snapshot(),
		UsageMeasured: u.measured,
		APIDurationMS: apiDurationMS,
	}
	return FinalizeTurn(emit, logger, ev, meta)
}
