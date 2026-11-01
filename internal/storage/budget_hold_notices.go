package storage

import (
	"context"
)

// BudgetHoldNotice is the stored tracker notice already posted for the
// current budget hold on one issue.
type BudgetHoldNotice struct {
	IssueID   string
	Reason    string // "session_budget", "token_budget", or "money_budget"
	NoticedAt string // RFC 3339
}

// BudgetHoldNoticeStore records the budget hold notices posted for issues.
type BudgetHoldNoticeStore interface {
	UpsertBudgetHoldNotice(ctx context.Context, notice BudgetHoldNotice) error
	DeleteBudgetHoldNotice(ctx context.Context, issueID string) error
	DeleteAllBudgetHoldNotices(ctx context.Context) error
	ListBudgetHoldNotices(ctx context.Context) ([]BudgetHoldNotice, error)
}
