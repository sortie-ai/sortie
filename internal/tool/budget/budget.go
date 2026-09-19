// Package budget implements [domain.AgentTool] for the cost_budget tool,
// reporting cumulative per-issue token spend and the remaining budget so
// agents can self-regulate before the ceiling stops the run.
package budget

import (
	"context"
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tool/toolresult"
)

var _ domain.AgentTool = (*BudgetTool)(nil)

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// BudgetQueryFunc returns per-issue token accounting. The running-session
// total is taken from session_metadata only when its stored dispatch ID
// matches runningDispatchID; otherwise it is 0. Implementations make no
// external calls.
type BudgetQueryFunc func(ctx context.Context, issueID string, runningDispatchID string) (BudgetUsage, error)

// BudgetUsage is the per-issue token accounting returned by a
// [BudgetQueryFunc].
type BudgetUsage struct {
	CompletedTotalTokens int64
	CompletedSessions    int
	RunningTotalTokens   int64 // session_metadata.total_tokens when dispatch_id matches; else 0.

	UnmeasuredSessions int

	// UnaccountedTurns is the summed count of turns that spent tokens no
	// figure was proven to cover, so a non-zero value makes
	// CompletedTotalTokens a lower bound even when every session was
	// measured.
	UnaccountedTurns int

	// RunningMeasured is true when a session_metadata row matched the
	// running dispatch ID, the same condition that gates
	// RunningTotalTokens.
	RunningMeasured bool
}

// BudgetTool implements [domain.AgentTool] for the cost_budget tool.
// Construct via [New]; safe for concurrent use after construction.
type BudgetTool struct {
	query             BudgetQueryFunc
	issueID           string
	runningDispatchID string
	budgetTokens      int
	budgetSessions    int
}

// costBudgetResponse is the JSON result of the cost_budget tool. The field
// names and null semantics are part of the tool's public contract.
type costBudgetResponse struct {
	UsedTokens         int64  `json:"used_tokens"`
	BudgetTokens       int64  `json:"budget_tokens"`
	RemainingTokens    *int64 `json:"remaining_tokens"` // null when budget is unlimited.
	UsedSessions       int    `json:"used_sessions"`
	BudgetSessions     int    `json:"budget_sessions"`
	UnmeasuredSessions int    `json:"unmeasured_sessions"`
	UsedTokensComplete bool   `json:"used_tokens_complete"`
}

// New returns a [BudgetTool] for the given issue and running dispatch.
// budgetTokens and budgetSessions are the configured ceilings, where 0
// means unlimited. An empty runningDispatchID contributes no
// running-session spend and makes used_tokens_complete false. New panics
// if query is nil or issueID is empty.
func New(query BudgetQueryFunc, issueID string, runningDispatchID string, budgetTokens int, budgetSessions int) *BudgetTool {
	if query == nil {
		panic("budget.New: query must not be nil")
	}
	if issueID == "" {
		panic("budget.New: issueID must not be empty")
	}
	return &BudgetTool{
		query:             query,
		issueID:           issueID,
		runningDispatchID: runningDispatchID,
		budgetTokens:      budgetTokens,
		budgetSessions:    budgetSessions,
	}
}

func (t *BudgetTool) Name() string { return "cost_budget" }

func (t *BudgetTool) Description() string {
	return "Returns cumulative token spend for the current issue and the remaining token " +
		"budget. Use this to decide whether to skip an expensive step, return partial work, " +
		"or hand off before the token ceiling stops this run in flight or blocks a further " +
		"session. A false used_tokens_complete means some sessions could not be measured, a " +
		"turn spent an amount that was never fully reported, or the running session's spend " +
		"is not included yet, so used_tokens is a lower bound."
}

// InputSchema returns the JSON Schema for cost_budget input; the tool
// accepts no parameters. The returned slice is a defensive copy.
func (t *BudgetTool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

// Execute computes the budget result for the current issue. Query
// failures return a JSON error response with a nil Go error; only marshal
// failures produce a non-nil Go error.
func (t *BudgetTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	usage, err := t.query(ctx, t.issueID, t.runningDispatchID)
	if err != nil {
		return toolresult.Failure("query_failed", err.Error())
	}

	usedTokens := usage.CompletedTotalTokens + usage.RunningTotalTokens

	var remaining *int64
	if t.budgetTokens > 0 {
		remaining = new(max(int64(t.budgetTokens)-usedTokens, 0))
	}

	usedTokensComplete := usage.UnmeasuredSessions == 0 && usage.UnaccountedTurns == 0 &&
		t.runningDispatchID != "" && usage.RunningMeasured

	return toolresult.Success(costBudgetResponse{
		UsedTokens:         usedTokens,
		BudgetTokens:       int64(t.budgetTokens),
		RemainingTokens:    remaining,
		UsedSessions:       usage.CompletedSessions,
		BudgetSessions:     t.budgetSessions,
		UnmeasuredSessions: usage.UnmeasuredSessions,
		UsedTokensComplete: usedTokensComplete,
	})
}
