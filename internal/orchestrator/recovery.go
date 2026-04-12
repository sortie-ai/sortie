package orchestrator

import "github.com/sortie-ai/sortie/internal/persistence"

// PopulateRetries loads persisted retry entries into state maps without
// starting timers. Each entry is stored in [State.RetryAttempts] with
// TimerHandle == nil and its computed remaining delay preserved in
// scheduledDelayMS for later activation. The issue is marked as Claimed
// to prevent double-dispatch on the first poll tick.
//
// Must be called before [NewOrchestrator] so the retry timer channel
// buffer can be sized to accommodate immediate-fire entries.
func PopulateRetries(state *State, entries []persistence.PendingRetry) {
	for _, pending := range entries {
		e := pending.Entry

		var errStr string
		if e.Error != nil {
			errStr = *e.Error
		}

		var sessionID string
		if e.SessionID != nil {
			sessionID = *e.SessionID
		}

		state.RetryAttempts[e.IssueID] = &RetryEntry{
			IssueID:          e.IssueID,
			Identifier:       e.Identifier,
			SessionID:        sessionID,
			Attempt:          e.Attempt,
			DueAtMS:          e.DueAtMs,
			Error:            errStr,
			scheduledDelayMS: pending.RemainingMs,
		}
		state.Claimed[e.IssueID] = struct{}{}
	}
}
