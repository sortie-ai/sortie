// Package budget implements [domain.AgentTool] for the cost_budget tool.
// It reports cumulative per-issue token spend and the remaining token
// budget so agents can self-regulate before the orchestrator's hard
// token ceiling blocks a re-dispatch.
package budget

import (
	"context"
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.AgentTool = (*BudgetTool)(nil)

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// BudgetQueryFunc returns the per-issue completed-session token sum, the
// completed-session count, and the running session's recorded token total.
// The running-session total is taken from session_metadata only when its
// stored session ID matches runningSessionID; otherwise it is 0.
// Implementations make no external calls.
type BudgetQueryFunc func(ctx context.Context, issueID string, runningSessionID string) (BudgetUsage, error)

// BudgetUsage is the per-issue token accounting returned by a
// [BudgetQueryFunc].
type BudgetUsage struct {
	CompletedTotalTokens int64 // SUM(total_tokens) over run_history rows for the issue.
	CompletedSessions    int   // COUNT(*) of run_history rows for the issue.
	RunningTotalTokens   int64 // session_metadata.total_tokens when session_id matches; else 0.
}

// BudgetTool implements [domain.AgentTool] for the cost_budget tool.
// Construct via [New]; safe for concurrent use after construction.
type BudgetTool struct {
	query            BudgetQueryFunc
	issueID          string
	runningSessionID string
	budgetTokens     int
	budgetSessions   int
}

// costBudgetResponse is the JSON result of the cost_budget tool. The field
// names and null semantics are part of the tool's public contract.
type costBudgetResponse struct {
	UsedTokens      int64  `json:"used_tokens"`
	BudgetTokens    int64  `json:"budget_tokens"`
	RemainingTokens *int64 `json:"remaining_tokens"` // null when budget is unlimited.
	UsedSessions    int    `json:"used_sessions"`
	BudgetSessions  int    `json:"budget_sessions"`
}

// New returns a [BudgetTool] for the given issue and running session.
//
// budgetTokens and budgetSessions are the configured agent ceilings,
// where 0 means unlimited. runningSessionID is the live session ID
// delivered to the sidecar out of band; an empty value contributes no
// running-session spend. New panics if query is nil or issueID is empty
// (programming errors).
func New(query BudgetQueryFunc, issueID string, runningSessionID string, budgetTokens int, budgetSessions int) *BudgetTool {
	if query == nil {
		panic("budget.New: query must not be nil")
	}
	if issueID == "" {
		panic("budget.New: issueID must not be empty")
	}
	return &BudgetTool{
		query:            query,
		issueID:          issueID,
		runningSessionID: runningSessionID,
		budgetTokens:     budgetTokens,
		budgetSessions:   budgetSessions,
	}
}

// Name returns "cost_budget".
func (t *BudgetTool) Name() string { return "cost_budget" }

// Description returns a human-readable summary of the tool.
func (t *BudgetTool) Description() string {
	return "Returns cumulative token spend for the current issue and the remaining token " +
		"budget. Use this to decide whether to skip an expensive step, return partial work, " +
		"or hand off before the orchestrator blocks further sessions on the token ceiling."
}

// InputSchema returns the JSON Schema for cost_budget input.
// The tool accepts no parameters; the schema is an empty object.
// The returned slice is a defensive copy.
func (t *BudgetTool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

// Execute computes the five-field budget result for the current issue.
//
// Query failures are returned as a JSON error response with a nil Go
// error. Only internal marshal failures produce a non-nil Go error.
func (t *BudgetTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	usage, err := t.query(ctx, t.issueID, t.runningSessionID)
	if err != nil {
		return errorResponse(err.Error())
	}

	usedTokens := usage.CompletedTotalTokens + usage.RunningTotalTokens

	var remaining *int64
	if t.budgetTokens > 0 {
		r := int64(t.budgetTokens) - usedTokens
		if r < 0 {
			r = 0
		}
		remaining = &r
	}

	return json.Marshal(costBudgetResponse{
		UsedTokens:      usedTokens,
		BudgetTokens:    int64(t.budgetTokens),
		RemainingTokens: remaining,
		UsedSessions:    usage.CompletedSessions,
		BudgetSessions:  t.budgetSessions,
	})
}

func errorResponse(msg string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"error": msg})
}
