package budget

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var noopQuery BudgetQueryFunc = func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
	return BudgetUsage{}, nil
}

// executeOK calls Execute, fails on a Go error or unparseable JSON, and
// returns the decoded data payload.
func executeOK(t *testing.T, tool *BudgetTool) costBudgetResponse {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var envelope struct {
		Success bool               `json:"success"`
		Data    costBudgetResponse `json:"data"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("Execute: unmarshal envelope %q: %v", out, err)
	}
	if !envelope.Success {
		t.Fatalf("Execute: envelope success = false, want true; raw: %s", out)
	}
	return envelope.Data
}

func TestBudgetTool_Name(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0, 0)

	if got := tool.Name(); got != "cost_budget" {
		t.Errorf("Name() = %q, want %q", got, "cost_budget")
	}
}

func TestBudgetTool_Description(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0, 0)

	got := tool.Description()
	if got == "" {
		t.Error(`Description() = "", want non-empty`)
	}
	if !strings.Contains(got, "used_tokens_complete") || !strings.Contains(got, "lower bound") {
		t.Errorf("Description() = %q, want it to state that a false used_tokens_complete makes used_tokens a lower bound", got)
	}
}

func TestBudgetTool_Description_WarningThreshold(t *testing.T) {
	t.Parallel()

	const appended = " A warning threshold is set below the token ceiling: warning_tokens is " +
		"that threshold, and warning_reached is true once used_tokens has reached it. When " +
		"warning_reached is true, wrap up or hand off the work in progress before the " +
		"ceiling stops this run."

	withThreshold := New(noopQuery, "10042", "sess-1", 0, 0, 100)
	withoutThreshold := New(noopQuery, "10042", "sess-1", 0, 0, 0)

	if strings.Contains(withoutThreshold.Description(), "warning_tokens") {
		t.Errorf("Description() = %q, want no mention of warning_tokens when warningTokens is 0", withoutThreshold.Description())
	}
	if got, want := withThreshold.Description(), withoutThreshold.Description()+appended; got != want {
		t.Errorf("Description() with warningTokens = %q, want %q", got, want)
	}
}

func TestBudgetTool_Execute_WarningThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		usage              BudgetUsage
		warningTokens      int
		wantWarningTokens  *int64
		wantWarningReached *bool
	}{
		{
			name:          "warningTokens zero omits both fields",
			usage:         BudgetUsage{CompletedTotalTokens: 900},
			warningTokens: 0,
		},
		{
			name:               "used_tokens below the threshold",
			usage:              BudgetUsage{CompletedTotalTokens: 799},
			warningTokens:      800,
			wantWarningTokens:  new(int64(800)),
			wantWarningReached: new(false),
		},
		{
			name:               "used_tokens exactly at the threshold",
			usage:              BudgetUsage{CompletedTotalTokens: 800},
			warningTokens:      800,
			wantWarningTokens:  new(int64(800)),
			wantWarningReached: new(true),
		},
		{
			name:               "used_tokens above the threshold",
			usage:              BudgetUsage{CompletedTotalTokens: 950},
			warningTokens:      800,
			wantWarningTokens:  new(int64(800)),
			wantWarningReached: new(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
				return tt.usage, nil
			}
			tool := New(query, "10042", "sess-1", 0, 0, tt.warningTokens)

			resp := executeOK(t, tool)

			switch {
			case tt.wantWarningTokens == nil && resp.WarningTokens != nil:
				t.Errorf("data.warning_tokens = %d, want absent", *resp.WarningTokens)
			case tt.wantWarningTokens != nil && resp.WarningTokens == nil:
				t.Errorf("data.warning_tokens absent, want %d", *tt.wantWarningTokens)
			case tt.wantWarningTokens != nil && *resp.WarningTokens != *tt.wantWarningTokens:
				t.Errorf("data.warning_tokens = %d, want %d", *resp.WarningTokens, *tt.wantWarningTokens)
			}
			switch {
			case tt.wantWarningReached == nil && resp.WarningReached != nil:
				t.Errorf("data.warning_reached = %v, want absent", *resp.WarningReached)
			case tt.wantWarningReached != nil && resp.WarningReached == nil:
				t.Errorf("data.warning_reached absent, want %v", *tt.wantWarningReached)
			case tt.wantWarningReached != nil && *resp.WarningReached != *tt.wantWarningReached:
				t.Errorf("data.warning_reached = %v, want %v", *resp.WarningReached, *tt.wantWarningReached)
			}
		})
	}
}

func TestBudgetTool_InputSchema_ValidJSON(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042", "sess-1", 0, 0, 0)

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

	tool := New(noopQuery, "10042", "sess-1", 0, 0, 0)

	schema1 := tool.InputSchema()

	for i := range schema1 {
		schema1[i] = 'X'
	}

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
			wantRemaining:      new(int64(250)),
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
			wantRemaining:      new(int64(0)),
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
			wantRemaining:      new(int64(0)),
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
			tool := New(query, "10042", "sess-1", tt.budgetTokens, tt.budgetSessions, 0)

			resp := executeOK(t, tool)

			if resp.UsedTokens != tt.wantUsedTokens {
				t.Errorf("data.used_tokens = %d, want %d", resp.UsedTokens, tt.wantUsedTokens)
			}
			if resp.BudgetTokens != tt.wantBudgetTokens {
				t.Errorf("data.budget_tokens = %d, want %d", resp.BudgetTokens, tt.wantBudgetTokens)
			}
			if resp.UsedSessions != tt.wantUsedSessions {
				t.Errorf("data.used_sessions = %d, want %d", resp.UsedSessions, tt.wantUsedSessions)
			}
			if resp.BudgetSessions != tt.wantBudgetSessions {
				t.Errorf("data.budget_sessions = %d, want %d", resp.BudgetSessions, tt.wantBudgetSessions)
			}
			switch {
			case tt.wantRemaining == nil && resp.RemainingTokens != nil:
				t.Errorf("data.remaining_tokens = %d, want null", *resp.RemainingTokens)
			case tt.wantRemaining != nil && resp.RemainingTokens == nil:
				t.Errorf("data.remaining_tokens = null, want %d", *tt.wantRemaining)
			case tt.wantRemaining != nil && *resp.RemainingTokens != *tt.wantRemaining:
				t.Errorf("data.remaining_tokens = %d, want %d", *resp.RemainingTokens, *tt.wantRemaining)
			}
		})
	}
}

func TestBudgetTool_Execute_UsedTokensComplete(t *testing.T) {
	t.Parallel()

	t.Run("a measured issue holding an unaccounted turn", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				RunningTotalTokens:   40,
				RunningMeasured:      true,
				UnaccountedTurns:     1,
			}, nil
		}
		tool := New(query, "10042", "dispatch-1", 1000, 5, 0)

		resp := executeOK(t, tool)

		if resp.UnmeasuredSessions != 0 {
			t.Errorf("data.unmeasured_sessions = %d, want 0: every session here was measured", resp.UnmeasuredSessions)
		}
		if resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = true, want false: a turn spent an unknown amount, " +
				"so the total is a lower bound even with every session measured")
		}
		if resp.UsedTokens != 540 {
			t.Errorf("data.used_tokens = %d, want 540: an unaccounted turn adds no number", resp.UsedTokens)
		}
	})

	t.Run("one unmeasured completed session", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				UnmeasuredSessions:   1,
			}, nil
		}
		tool := New(query, "10042", "", 1000, 5, 0)

		resp := executeOK(t, tool)

		if resp.UnmeasuredSessions != 1 {
			t.Errorf("data.unmeasured_sessions = %d, want 1", resp.UnmeasuredSessions)
		}
		if resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = true, want false (one session unmeasured)")
		}
		if resp.UsedTokens != 500 {
			t.Errorf("data.used_tokens = %d, want 500", resp.UsedTokens)
		}
		if resp.BudgetTokens != 1000 {
			t.Errorf("data.budget_tokens = %d, want 1000", resp.BudgetTokens)
		}
		if resp.RemainingTokens == nil || *resp.RemainingTokens != 500 {
			t.Errorf("data.remaining_tokens = %v, want 500", resp.RemainingTokens)
		}
		if resp.UsedSessions != 2 {
			t.Errorf("data.used_sessions = %d, want 2", resp.UsedSessions)
		}
		if resp.BudgetSessions != 5 {
			t.Errorf("data.budget_sessions = %d, want 5", resp.BudgetSessions)
		}
	})

	t.Run("all completed sessions measured and the running session has a matching row", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				UnmeasuredSessions:   0,
				RunningTotalTokens:   50,
				RunningMeasured:      true,
			}, nil
		}
		tool := New(query, "10042", "sess-running", 1000, 5, 0)

		resp := executeOK(t, tool)

		if resp.UnmeasuredSessions != 0 {
			t.Errorf("data.unmeasured_sessions = %d, want 0", resp.UnmeasuredSessions)
		}
		if !resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = false, want true")
		}
		if resp.UsedTokens != 550 {
			t.Errorf("data.used_tokens = %d, want 550 (completed sum plus the running session)", resp.UsedTokens)
		}
	})

	t.Run("running session reported a measurement of zero and nothing else", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				UnmeasuredSessions:   0,
				RunningTotalTokens:   0,
				RunningMeasured:      true,
			}, nil
		}
		tool := New(query, "10042", "sess-running", 1000, 5, 0)

		resp := executeOK(t, tool)

		if !resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = false, want true (the running session's row exists, reporting zero)")
		}
		if resp.UsedTokens != 500 {
			t.Errorf("data.used_tokens = %d, want 500 (completed sum alone)", resp.UsedTokens)
		}
	})

	t.Run("empty running dispatch id keeps the reading incomplete even when the query reports RunningMeasured", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				UnmeasuredSessions:   0,
				RunningTotalTokens:   0,
				RunningMeasured:      true,
			}, nil
		}
		tool := New(query, "10042", "", 1000, 5, 0)

		resp := executeOK(t, tool)

		if resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = true, want false (empty running dispatch id)")
		}
	})

	t.Run("running session id supplied but no matching row leaves the reading incomplete", func(t *testing.T) {
		t.Parallel()

		query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
			return BudgetUsage{
				CompletedTotalTokens: 500,
				CompletedSessions:    2,
				UnmeasuredSessions:   0,
				RunningMeasured:      false,
			}, nil
		}
		tool := New(query, "10042", "sess-running", 1000, 5, 0)

		resp := executeOK(t, tool)

		if resp.UsedTokensComplete {
			t.Error("data.used_tokens_complete = true, want false (no session_metadata row found for the running session)")
		}
	})
}

func TestBudgetTool_Execute_SuccessEnvelopeShape(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
		return BudgetUsage{CompletedTotalTokens: 10, CompletedSessions: 1}, nil
	}
	tool := New(query, "10042", "", 0, 0, 0)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}

	if len(top) != 2 {
		t.Errorf("Execute success top-level keys = %v, want exactly {success, data}", top)
	}
	if top["success"] != true {
		t.Errorf("Execute success[\"success\"] = %v, want true", top["success"])
	}
	data, ok := top["data"].(map[string]any)
	if !ok {
		t.Fatalf("Execute success[\"data\"] = %T %v, want map", top["data"], top["data"])
	}

	budgetKeys := []string{
		"used_tokens", "budget_tokens", "remaining_tokens", "used_sessions", "budget_sessions",
		"unmeasured_sessions", "used_tokens_complete",
	}
	for _, key := range budgetKeys {
		if _, ok := data[key]; !ok {
			t.Errorf("data missing key %q: %s", key, out)
		}
	}
	if len(data) != 7 {
		t.Errorf("data has %d keys, want 7: %s", len(data), out)
	}

	if got, ok := data["remaining_tokens"]; !ok || got != nil {
		t.Errorf("data.remaining_tokens = %v, want explicit null", got)
	}

	for _, payloadKey := range budgetKeys {
		if _, exists := top[payloadKey]; exists {
			t.Errorf("Execute success has payload key %q at top level, want it under data", payloadKey)
		}
	}
}

func TestBudgetTool_Execute_QueryError(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
		return BudgetUsage{}, fmt.Errorf("database is locked")
	}

	tool := New(query, "10042", "sess-1", 1000, 5, 0)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: expected nil Go error on query failure, got: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal error response %q: %v", out, err)
	}

	if top["success"] != false {
		t.Errorf("Execute failure[\"success\"] = %v, want false", top["success"])
	}

	errObj, ok := top["error"].(map[string]any)
	if !ok {
		t.Fatalf("Execute failure[\"error\"] = %T %v, want map", top["error"], top["error"])
	}
	if got, _ := errObj["kind"].(string); got != "query_failed" {
		t.Errorf("error.kind = %q, want %q", got, "query_failed")
	}
	msg, ok := errObj["message"].(string)
	if !ok {
		t.Fatalf("error.message is not a string: %T %v", errObj["message"], errObj["message"])
	}
	if msg != "database is locked" {
		t.Errorf("error.message = %q, want %q", msg, "database is locked")
	}
}

func TestBudgetTool_Execute_FailureEnvelopeShape(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ string) (BudgetUsage, error) {
		return BudgetUsage{}, fmt.Errorf("disk full")
	}
	tool := New(query, "10042", "sess-1", 0, 0, 0)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(top) != 2 {
		t.Errorf("Execute failure top-level keys = %v, want exactly {success, error}", top)
	}
	if top["success"] != false {
		t.Errorf("Execute failure[\"success\"] = %v, want false", top["success"])
	}
	errObj, ok := top["error"].(map[string]any)
	if !ok {
		t.Fatalf("Execute failure[\"error\"] = %T %v, want map", top["error"], top["error"])
	}
	if len(errObj) != 2 {
		t.Errorf("error object keys = %v, want exactly {kind, message}", errObj)
	}
}

func TestBudgetTool_Execute_PassesIdentity(t *testing.T) {
	t.Parallel()

	var gotIssueID, gotSessionID string
	query := func(_ context.Context, issueID string, runningSessionID string) (BudgetUsage, error) {
		gotIssueID = issueID
		gotSessionID = runningSessionID
		return BudgetUsage{}, nil
	}

	tool := New(query, "10042", "sess-live", 0, 0, 0)

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
	New(nil, "10042", "sess-1", 0, 0, 0)

}

func TestNew_PanicsOnEmptyIssueID(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error(`New(noopQuery, "", ...) did not panic`)
		}
	}()
	New(noopQuery, "", "sess-1", 0, 0, 0)

}
