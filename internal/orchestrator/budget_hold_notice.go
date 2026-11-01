package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/storage"
)

// budgetHoldNoticeParams carries the inputs of one notice, in the shape
// parkIssueParams uses for one park.
type budgetHoldNoticeParams struct {
	IssueID string
	Entry   *BudgetExhaustedEntry

	Store    storage.BudgetHoldNoticeStore
	Delivery route.Delivery // non-empty: the caller routed the event and found a destination
	Metrics  domain.Metrics
	Logger   *slog.Logger
	Ctx      context.Context
}

// Pacing constants for the budget hold notice. At most
// maxBudgetHoldNoticesPerWindow notices are posted in any
// budgetHoldNoticeWindow, counted across both the rebuild and the retry
// lane.
const (
	maxBudgetHoldNoticesPerWindow = 10
	budgetHoldNoticeWindow        = 30 * time.Second
)

// budgetHoldNoticeAllowed reports whether the pacing window has room for
// one more notice, opening a fresh window when the current one has
// elapsed. It mutates state.BudgetHoldNoticeWindowStart and
// state.BudgetHoldNoticesInWindow. The slot is consumed at this decision
// point, not at a successful write, so a caller that decides to post but
// then fails to persist the row still spends the slot.
func budgetHoldNoticeAllowed(state *State, now time.Time) bool {
	if now.Sub(state.BudgetHoldNoticeWindowStart) >= budgetHoldNoticeWindow {
		state.BudgetHoldNoticeWindowStart = now
		state.BudgetHoldNoticesInWindow = 0
	}
	if state.BudgetHoldNoticesInWindow >= maxBudgetHoldNoticesPerWindow {
		return false
	}
	state.BudgetHoldNoticesInWindow++
	return true
}

// postBudgetHoldNotice writes the durable notice row, records the memory
// entry, and starts the detached delivery. It makes no decision about
// whether a notice is due; the caller has already decided.
func postBudgetHoldNotice(state *State, params budgetHoldNoticeParams) {
	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	row := storage.BudgetHoldNotice{
		IssueID:   params.IssueID,
		Reason:    params.Entry.Reason,
		NoticedAt: params.Entry.ExhaustedAt.Format(time.RFC3339),
	}
	if err := params.Store.UpsertBudgetHoldNotice(ctx, row); err != nil {
		log.Error("failed to persist budget hold notice", slog.Any("error", err))
		return
	}
	state.BudgetHoldNoticed[params.IssueID] = params.Entry.Reason

	m := params.Metrics
	noticeLog := log

	deliverDetached(ctx, &state.TrackerOpsWg, params.Delivery, log, func(received bool, err error) {
		if !received {
			return
		}
		if err != nil {
			noticeLog.Warn("budget hold notice failed", slog.Any("error", err))
			m.IncTrackerComments("budget_hold", "error")
			return
		}
		noticeLog.Info("budget hold notice posted")
		m.IncTrackerComments("budget_hold", "success")
	})
}

// releaseBudgetHoldNotice deletes the durable row and the memory entry for
// one issue. It returns without a store call when the memory holds no
// reason for the issue. The memory entry is dropped only once the durable
// delete succeeds, so a failed delete leaves the entry in place and the
// next release pass retries it rather than stranding the row.
func releaseBudgetHoldNotice(ctx context.Context, state *State, store storage.BudgetHoldNoticeStore, issueID string, log *slog.Logger) {
	if _, ok := state.BudgetHoldNoticed[issueID]; !ok {
		return
	}
	if err := store.DeleteBudgetHoldNotice(ctx, issueID); err != nil {
		log.Error("failed to delete budget hold notice", slog.Any("error", err))
		return
	}
	delete(state.BudgetHoldNoticed, issueID)
}

// releaseAllBudgetHoldNotices deletes every durable row in one statement
// and then empties the memory. It returns without a store call when the
// memory is already empty. The memory is cleared only once the durable
// delete succeeds, so a failed delete leaves the entries in place and the
// next release pass retries it rather than stranding the rows.
func releaseAllBudgetHoldNotices(ctx context.Context, state *State, store storage.BudgetHoldNoticeStore, log *slog.Logger) {
	if len(state.BudgetHoldNoticed) == 0 {
		return
	}
	if err := store.DeleteAllBudgetHoldNotices(ctx); err != nil {
		log.Error("failed to delete budget hold notices", slog.Any("error", err))
		return
	}
	state.BudgetHoldNoticed = make(map[string]string)
}

// PopulateBudgetHoldNotices loads persisted budget hold notice records
// into state.BudgetHoldNoticed. Called during startup recovery, before the
// event loop starts, beside [PopulateParked].
func PopulateBudgetHoldNotices(state *State, rows []storage.BudgetHoldNotice, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	for _, row := range rows {
		if row.IssueID == "" {
			log.Warn("skipping malformed budget hold notice record")
			continue
		}
		state.BudgetHoldNoticed[row.IssueID] = row.Reason
	}
}
