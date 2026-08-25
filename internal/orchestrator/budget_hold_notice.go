package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// budgetHoldNoticeStore is the persistence contract [postBudgetHoldNotice]
// needs. Both the rebuild and the retry lane call it.
type budgetHoldNoticeStore interface {
	UpsertBudgetHoldNotice(ctx context.Context, notice persistence.BudgetHoldNotice) error
}

// budgetHoldNoticeReleaseStore is the persistence contract the rebuild's
// release paths need. Only the rebuild ever releases a notice; the retry
// lane posts notices and never releases one.
type budgetHoldNoticeReleaseStore interface {
	DeleteBudgetHoldNotice(ctx context.Context, issueID string) error
	DeleteAllBudgetHoldNotices(ctx context.Context) error
}

// budgetHoldNoticeParams carries the inputs of one notice, in the shape
// parkIssueParams uses for one park.
type budgetHoldNoticeParams struct {
	IssueID string
	Entry   *BudgetExhaustedEntry

	Store          budgetHoldNoticeStore
	TrackerAdapter domain.TrackerAdapter // nil skips the notice entirely
	Metrics        domain.Metrics
	Logger         *slog.Logger
	Ctx            context.Context
}

// budgetHoldCeilingWords names each budget-hold reason in the words the
// notice uses. A reason absent from this map renders no Ceiling: line at
// all, rather than a machine value or an invented phrase.
var budgetHoldCeilingWords = map[string]string{
	budgetReasonSession: "session budget",
	budgetReasonToken:   "token budget",
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
// entry, and starts the detached comment write. It makes no decision
// about whether a notice is due; the caller has already decided.
func postBudgetHoldNotice(state *State, params budgetHoldNoticeParams) {
	if params.TrackerAdapter == nil {
		return
	}
	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	row := persistence.BudgetHoldNotice{
		IssueID:   params.IssueID,
		Reason:    params.Entry.Reason,
		NoticedAt: params.Entry.ExhaustedAt.Format(time.RFC3339),
	}
	if err := params.Store.UpsertBudgetHoldNotice(ctx, row); err != nil {
		log.Error("failed to persist budget hold notice", slog.Any("error", err))
		return
	}
	state.BudgetHoldNoticed[params.IssueID] = params.Entry.Reason

	issueID := params.IssueID
	text := buildBudgetHoldComment(params.Entry)
	tracker := params.TrackerAdapter
	m := params.Metrics
	noticeLog := log

	state.TrackerOpsWg.Go(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := tracker.CommentIssue(dctx, issueID, text); err != nil {
			noticeLog.Warn("budget hold notice failed", slog.Any("error", err))
			m.IncTrackerComments("budget_hold", "error")
		} else {
			noticeLog.Info("budget hold notice posted")
			m.IncTrackerComments("budget_hold", "success")
		}
	})
}

// releaseBudgetHoldNotice deletes the memory entry and the durable row for
// one issue. It returns without a store call when the memory holds no
// reason for the issue.
func releaseBudgetHoldNotice(ctx context.Context, state *State, store budgetHoldNoticeReleaseStore, issueID string, log *slog.Logger) {
	if _, ok := state.BudgetHoldNoticed[issueID]; !ok {
		return
	}
	delete(state.BudgetHoldNoticed, issueID)
	if err := store.DeleteBudgetHoldNotice(ctx, issueID); err != nil {
		log.Error("failed to delete budget hold notice", slog.Any("error", err))
	}
}

// releaseAllBudgetHoldNotices empties the memory and deletes every durable
// row in one statement. It returns without a store call when the memory is
// already empty.
func releaseAllBudgetHoldNotices(ctx context.Context, state *State, store budgetHoldNoticeReleaseStore, log *slog.Logger) {
	if len(state.BudgetHoldNoticed) == 0 {
		return
	}
	state.BudgetHoldNoticed = make(map[string]string)
	if err := store.DeleteAllBudgetHoldNotices(ctx); err != nil {
		log.Error("failed to delete budget hold notices", slog.Any("error", err))
	}
}

// buildBudgetHoldComment renders the plain-text notice body from the
// entry alone. Pure: no clock, no config lookup, no tracker call. The
// body is a past-tense event record and MUST NOT assert a present state,
// use imperative or sufficiency-claiming phrasing, or interpolate the
// issue's identifier, which the comment is already attached to.
func buildBudgetHoldComment(entry *BudgetExhaustedEntry) string {
	lines := []string{
		"Sortie stopped dispatching this issue.",
		fmt.Sprintf("Recorded: %s", entry.ExhaustedAt.Format(time.RFC3339)),
	}

	word, hasWord := budgetHoldCeilingWords[entry.Reason]
	setting, hasSetting := ceilingSettingByBudgetReason[entry.Reason]
	if hasWord && hasSetting {
		lines = append(lines, fmt.Sprintf("Ceiling: %s (%s)", word, setting))
	}

	if entry.BudgetTokens > 0 && entry.UsedTokens != nil {
		lines = append(lines, fmt.Sprintf("Tokens: %d of %d", *entry.UsedTokens, entry.BudgetTokens))
	}
	if entry.BudgetSessions > 0 {
		lines = append(lines, fmt.Sprintf("Sessions: %d of %d", entry.UsedSessions, entry.BudgetSessions))
	}

	if hasSetting {
		lines = append(lines, fmt.Sprintf("Raising %s raises this ceiling.", setting))
	}

	return strings.Join(lines, "\n")
}

// PopulateBudgetHoldNotices loads persisted budget hold notice records
// into state.BudgetHoldNoticed. Called during startup recovery, before the
// event loop starts, beside [PopulateParked].
func PopulateBudgetHoldNotices(state *State, rows []persistence.BudgetHoldNotice, log *slog.Logger) {
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
