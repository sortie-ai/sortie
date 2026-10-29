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

	// ByModel is Run's per-model split, or nil when the source names none.
	ByModel map[string]domain.TokenUsage

	// Model names the model that produced Run, or is empty when the
	// recovery's source names none. Finalize trims surrounding
	// whitespace before reporting it.
	Model string
}

// TurnEndUsage is the usage state of one session of a kind whose figure
// settles only after a turn's work is over. It owns the session's
// run-cumulative snapshot and measurement verdict, and reports each
// recovered figure, or the live counts no recovered figure covers yet, as
// the turn's one usage event. One value serves one session; it is not safe
// for concurrent use.
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

// ByModel returns a copy of the snapshot's per-model split, or nil.
func (u *TurnEndUsage) ByModel() map[string]domain.TokenUsage {
	return u.acc.ByModel()
}

// Measured reports whether a figure has been reported yet, by Settle with
// a non-nil recovered figure or by ReportLive with counts that add input or
// output tokens. It is false for a value from NewTurnEndUsage until then,
// and true from that call on.
func (u *TurnEndUsage) Measured() bool {
	return u.measured
}

// Settle folds recovered into the session's run-cumulative snapshot, latches
// the measured verdict, and emits exactly one [domain.EventTokenUsage] event
// carrying that snapshot, its per-model figure, and the trimmed model name.
// A nil recovered does nothing.
func (u *TurnEndUsage) Settle(emit func(domain.AgentEvent), recovered *RecoveredUsage) {
	if recovered == nil {
		return
	}
	snapshot := u.acc.SetRunCumulative(recovered.Run, recovered.ByModel)
	u.measured = true
	emit(domain.AgentEvent{
		Type:         domain.EventTokenUsage,
		Timestamp:    time.Now().UTC(),
		Usage:        snapshot,
		UsageByModel: u.acc.ByModel(),
		Model:        strings.TrimSpace(recovered.Model),
	})
}

// ReportLive reports the live counts no settled figure covers yet and emits
// one [domain.EventTokenUsage] event carrying the resulting snapshot and the
// settled per-model split, with no model name. Counts that add no input and
// no output tokens report nothing: no event, and the snapshot and the
// measured verdict stay as they were, so they never become a measured zero.
//
// A later [TurnEndUsage.Settle] with a figure replaces these counts instead of
// adding to them, and the snapshot never falls.
func (u *TurnEndUsage) ReportLive(emit func(domain.AgentEvent), sinceSettled domain.TokenUsage) {
	if sinceSettled.InputTokens == 0 && sinceSettled.OutputTokens == 0 {
		return
	}
	snapshot := u.acc.SetTurnProvisional(sinceSettled)
	u.measured = true
	emit(domain.AgentEvent{
		Type:         domain.EventTokenUsage,
		Timestamp:    time.Now().UTC(),
		Usage:        snapshot,
		UsageByModel: u.acc.ByModel(),
	})
}

// Finalize ends one turn. recovered is nil when the turn produced no
// figure; otherwise Finalize reports it through [TurnEndUsage.Settle]
// before delegating to [FinalizeTurn] for the turn's terminal event.
//
// spend is the turn's spend evidence and is handed to [FinalizeTurn].
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
	spend TurnSpend,
) (domain.TurnResult, *domain.AgentError) {
	if emit == nil {
		panic("agentcore: TurnEndUsage.Finalize: emit must be non-nil")
	}

	u.Settle(emit, recovered)

	meta := TurnMeta{
		SessionID:     sessionID,
		Usage:         u.acc.Snapshot(),
		UsageByModel:  u.acc.ByModel(),
		UsageMeasured: u.measured,
		APIDurationMS: apiDurationMS,
	}
	return FinalizeTurn(emit, logger, ev, meta, spend)
}
