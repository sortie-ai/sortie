package budget

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- Helpers ---

var noopQuery BudgetQueryFunc = func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
	return BudgetUsage{}, nil
}

func executeOK(t *testing.T, tool *BudgetTool) costBudgetResponse {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}
	var resp costBudgetResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("Execute: unmarshal response %q: %v", out, err)
	}
	return resp
}

func int64Ptr(v int64) *int64 { return &v }

// --- Tests ---

func TestBudgetTool_Name(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0)
	if got := tool.Name(); got != "cost_budget" {
		t.Errorf("Name() = %q, want %q", got, "cost_budget")
	}
}

func TestBudgetTool_Description(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0)
	if got := tool.Description(); got == "" {
		t.Error(`Description() = "", want non-empty`)
	}
}

func TestBudgetTool_InputSchema_ValidJSON(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0)
	schema := tool.InputSchema()

	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		t.Fatalf("InputSchema() unmarshal: %v", err)
	}

	if got, _ := m["type"].(string); got != "object" {
		t.Errorf("schema type = %v, want %q", m["type"], "object")
	}
	v, ok := m["additionalProperties"]
	if !ok {
		t.Fatal("additionalProperties key missing from schema")
	}
	if v != false {
		t.Errorf("additionalProperties = %v, want false", v)
	}
}

func TestBudgetTool_InputSchema_DefensiveCopy(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0)
	schema1 := tool.InputSchema()

	// Overwrite every byte of the first copy.
	for i := range schema1 {
		schema1[i] = 'X'
	}

	// The second call must still return valid JSON.
	schema2 := tool.InputSchema()
	var m map[string]any
	if err := json.Unmarshal(schema2, &m); err != nil {
		t.Fatalf("InputSchema() after mutation: unmarshal: %v", err)
	}
}

func TestBudgetTool_Execute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		usage          BudgetUsage
		budgetTokens   int
		budgetSessions int

		wantUsedTokens     int64
		wantBudgetTokens   int64
		wantRemaining      *int64 // nil means JSON null (unlimited budget)
		wantUsedSessions   int
		wantBudgetSessions int
	}{
		{
			name:               "used tokens sums completed and running, used sessions counts completed only",
			usage:              BudgetUsage{CompletedTotalTokens: 600, CompletedSessions: 3, RunningTotalTokens: 150},
			budgetTokens:       1000,
			budgetSessions:     5,
			wantUsedTokens:     750,
			wantBudgetTokens:   1000,
			wantRemaining:      int64Ptr(250),
			wantUsedSessions:   3,
			wantBudgetSessions: 5,
		},
		{
			name:               "remaining floored at zero when over budget",
			usage:              BudgetUsage{CompletedTotalTokens: 900, CompletedSessions: 2, RunningTotalTokens: 200},
			budgetTokens:       1000,
			budgetSessions:     0,
			wantUsedTokens:     1100,
			wantBudgetTokens:   1000,
			wantRemaining:      int64Ptr(0),
			wantUsedSessions:   2,
			wantBudgetSessions: 0,
		},
		{
			name:               "remaining zero at exact budget consumption",
			usage:              BudgetUsage{CompletedTotalTokens: 1000, CompletedSessions: 4},
			budgetTokens:       1000,
			budgetSessions:     4,
			wantUsedTokens:     1000,
			wantBudgetTokens:   1000,
			wantRemaining:      int64Ptr(0),
			wantUsedSessions:   4,
			wantBudgetSessions: 4,
		},
		{
			name:               "unlimited token budget reports null remaining",
			usage:              BudgetUsage{CompletedTotalTokens: 600, CompletedSessions: 3, RunningTotalTokens: 150},
			budgetTokens:       0,
			budgetSessions:     5,
			wantUsedTokens:     750,
			wantBudgetTokens:   0,
			wantRemaining:      nil,
			wantUsedSessions:   3,
			wantBudgetSessions: 5,
		},
		{
			name:               "zero usage under unlimited budgets",
			usage:              BudgetUsage{},
			budgetTokens:       0,
			budgetSessions:     0,
			wantUsedTokens:     0,
			wantBudgetTokens:   0,
			wantRemaining:      nil,
			wantUsedSessions:   0,
			wantBudgetSessions: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
				return tt.usage, nil
			}
			tool := New(query, "10042", "sess-1", tt.budgetTokens, tt.budgetSessions)

			resp := executeOK(t, tool)

			if resp.UsedTokens != tt.wantUsedTokens {
				t.Errorf("used_tokens = %d, want %d", resp.UsedTokens, tt.wantUsedTokens)
			}
			if resp.BudgetTokens != tt.wantBudgetTokens {
				t.Errorf("budget_tokens = %d, want %d", resp.BudgetTokens, tt.wantBudgetTokens)
			}
			if resp.UsedSessions != tt.wantUsedSessions {
				t.Errorf("used_sessions = %d, want %d", resp.UsedSessions, tt.wantUsedSessions)
			}
			if resp.BudgetSessions != tt.wantBudgetSessions {
				t.Errorf("budget_sessions = %d, want %d", resp.BudgetSessions, tt.wantBudgetSessions)
			}
			switch {
			case tt.wantRemaining == nil && resp.RemainingTokens != nil:
				t.Errorf("remaining_tokens = %d, want null", *resp.RemainingTokens)
			case tt.wantRemaining != nil && resp.RemainingTokens == nil:
				t.Errorf("remaining_tokens = null, want %d", *tt.wantRemaining)
			case tt.wantRemaining != nil && *resp.RemainingTokens != *tt.wantRemaining:
				t.Errorf("remaining_tokens = %d, want %d", *resp.RemainingTokens, *tt.wantRemaining)
			}
		})
	}
}

// TestBudgetTool_Execute_FiveFieldShape pins the public JSON contract:
// all five field names are present, and remaining_tokens is an explicit
// null (not omitted) under an unlimited token budget.
func TestBudgetTool_Execute_FiveFieldShape(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
		return BudgetUsage{CompletedTotalTokens: 10, CompletedSessions: 1}, nil
	}
	tool := New(query, "10042", "", 0, 0)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}

	for _, key := range []string{"used_tokens", "budget_tokens", "remaining_tokens", "used_sessions", "budget_sessions"} {
		if _, ok := m[key]; !ok {
			t.Errorf("response missing key %q: %s", key, out)
		}
	}
	if len(m) != 5 {
		t.Errorf("response has %d keys, want 5: %s", len(m), out)
	}
	if got, ok := m["remaining_tokens"]; !ok || got != nil {
		t.Errorf("remaining_tokens = %v, want explicit null", got)
	}
}

func TestBudgetTool_Execute_QueryError(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
		return BudgetUsage{}, fmt.Errorf("database is locked")
	}

	tool := New(query, "10042", "sess-1", 1000, 5)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: expected nil Go error on query failure, got: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal error response %q: %v", out, err)
	}

	errStr, ok := m["error"].(string)
	if !ok {
		t.Fatalf("error value is not a string: %T %v", m["error"], m["error"])
	}
	if !strings.Contains(errStr, "database is locked") {
		t.Errorf("error value = %q, want to contain %q", errStr, "database is locked")
	}
}

// TestBudgetTool_Execute_PassesIdentity verifies the tool forwards its
// construction-time issue ID and running session ID to the query.
func TestBudgetTool_Execute_PassesIdentity(t *testing.T) {
	t.Parallel()

	var gotIssueID, gotSessionID string
	query := func(_ context.Context, issueID string, runningSessionID string) (BudgetUsage, error) {
		gotIssueID = issueID
		gotSessionID = runningSessionID
		return BudgetUsage{}, nil
	}

	tool := New(query, "10042", "sess-live", 0, 0)
	executeOK(t, tool)

	if gotIssueID != "10042" {
		t.Errorf("query issueID = %q, want %q", gotIssueID, "10042")
	}
	if gotSessionID != "sess-live" {
		t.Errorf("query runningSessionID = %q, want %q", gotSessionID, "sess-live")
	}
}

func TestNew_PanicsOnNilQuery(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error(`New(nil, "10042", ...) did not panic`)
		}
	}()
	New(nil, "10042", "sess-1", 0, 0)
}

func TestNew_PanicsOnEmptyIssueID(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error(`New(noopQuery, "", ...) did not panic`)
		}
	}()
	New(noopQuery, "", "sess-1", 0, 0)
}
