package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- Helpers ---

var noopQuery QueryFunc = func(_ context.Context, _ string, _ int) ([]Entry, error) {
	return []Entry{}, nil
}

func executeOK(t *testing.T, tool *HistoryTool) map[string]any {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("Execute: unmarshal response %q: %v", out, err)
	}
	return m
}

// --- Tests ---

func TestHistoryTool_Name(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042")
	if got := tool.Name(); got != "workspace_history" {
		t.Errorf("Name() = %q, want %q", got, "workspace_history")
	}
}

func TestHistoryTool_Description(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042")
	if got := tool.Description(); got == "" {
		t.Error(`Description() = "", want non-empty`)
	}
}

func TestHistoryTool_InputSchema_ValidJSON(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042")
	schema := tool.InputSchema()

	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		t.Fatalf("InputSchema() unmarshal: %v", err)
	}

	v, ok := m["additionalProperties"]
	if !ok {
		t.Fatal("additionalProperties key missing from schema")
	}
	if v != false {
		t.Errorf("additionalProperties = %v, want false", v)
	}
}

func TestHistoryTool_InputSchema_DefensiveCopy(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042")
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

func TestHistoryTool_Execute_EntriesReturned(t *testing.T) {
	t.Parallel()

	errMsg := "agent crashed"
	canned := []Entry{
		{Attempt: 1, AgentAdapter: "claude-code", StartedAt: "2026-03-01T10:00:00Z", CompletedAt: "2026-03-01T10:30:00Z", Status: "succeeded", Error: nil},
		{Attempt: 2, AgentAdapter: "claude-code", StartedAt: "2026-03-02T10:00:00Z", CompletedAt: "2026-03-02T10:05:00Z", Status: "failed", Error: &errMsg},
		{Attempt: 3, AgentAdapter: "mock", StartedAt: "2026-03-03T10:00:00Z", CompletedAt: "2026-03-03T10:15:00Z", Status: "succeeded", Error: nil},
	}
	query := func(_ context.Context, _ string, _ int) ([]Entry, error) {
		return canned, nil
	}

	tool := New(query, "10042")
	m := executeOK(t, tool)

	if got, ok := m["issue_id"].(string); !ok || got != "10042" {
		t.Errorf("issue_id = %v, want %q", m["issue_id"], "10042")
	}

	rawEntries, ok := m["entries"].([]any)
	if !ok {
		t.Fatalf("entries is not an array: %T %v", m["entries"], m["entries"])
	}
	if len(rawEntries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(rawEntries))
	}

	// entries[0]: succeeded, nil error → JSON null.
	e0, ok := rawEntries[0].(map[string]any)
	if !ok {
		t.Fatalf("entries[0] is not an object: %T", rawEntries[0])
	}
	if got, _ := e0["status"].(string); got != "succeeded" {
		t.Errorf("entries[0].status = %q, want %q", got, "succeeded")
	}
	if e0["error"] != nil {
		t.Errorf("entries[0].error = %v, want null", e0["error"])
	}

	// entries[1]: failed, non-nil error → JSON string.
	e1, ok := rawEntries[1].(map[string]any)
	if !ok {
		t.Fatalf("entries[1] is not an object: %T", rawEntries[1])
	}
	if got, _ := e1["status"].(string); got != "failed" {
		t.Errorf("entries[1].status = %q, want %q", got, "failed")
	}
	if got, ok := e1["error"].(string); !ok || got != "agent crashed" {
		t.Errorf("entries[1].error = %v, want %q", e1["error"], "agent crashed")
	}
}

func TestHistoryTool_Execute_EmptyEntries(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ int) ([]Entry, error) {
		return []Entry{}, nil
	}

	tool := New(query, "10042")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// entries must be a JSON array ([]), not null.
	entries, ok := m["entries"].([]any)
	if !ok {
		t.Fatalf("entries is not an array: %T %v", m["entries"], m["entries"])
	}
	if len(entries) != 0 {
		t.Errorf("len(entries) = %d, want 0", len(entries))
	}
}

func TestHistoryTool_Execute_LimitPassthrough(t *testing.T) {
	t.Parallel()

	called := false
	query := func(_ context.Context, _ string, limit int) ([]Entry, error) {
		called = true
		if limit != maxEntries {
			t.Errorf("QueryFunc limit = %d, want %d", limit, maxEntries)
		}
		out := make([]Entry, maxEntries)
		for i := range out {
			out[i] = Entry{Attempt: i + 1, AgentAdapter: "mock", Status: "succeeded"}
		}
		return out, nil
	}

	tool := New(query, "10042")
	m := executeOK(t, tool)

	if !called {
		t.Fatal("QueryFunc was not called")
	}
	rawEntries, ok := m["entries"].([]any)
	if !ok {
		t.Fatalf("entries is not an array: %T %v", m["entries"], m["entries"])
	}
	if len(rawEntries) != maxEntries {
		t.Errorf("len(entries) = %d, want %d", len(rawEntries), maxEntries)
	}
}

func TestHistoryTool_Execute_QueryError(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ int) ([]Entry, error) {
		return nil, fmt.Errorf("database is locked")
	}

	tool := New(query, "10042")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: expected nil Go error on query failure, got: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal error response %q: %v", out, err)
	}

	if _, ok := m["error"]; !ok {
		t.Fatal(`response missing "error" key`)
	}
	errStr, ok := m["error"].(string)
	if !ok {
		t.Fatalf("error value is not a string: %T %v", m["error"], m["error"])
	}
	if !strings.Contains(errStr, "database is locked") {
		t.Errorf("error value = %q, want to contain %q", errStr, "database is locked")
	}
}

func TestNew_PanicsOnNilQuery(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error(`New(nil, "10042") did not panic`)
		}
	}()
	New(nil, "10042")
}

func TestNew_PanicsOnEmptyIssueID(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error(`New(noopQuery, "") did not panic`)
		}
	}()
	New(noopQuery, "")
}
