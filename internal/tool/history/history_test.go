package history

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// --- Helpers ---

var noopQuery QueryFunc = func(_ context.Context, _ string, _ int) ([]Entry, error) {
	return []Entry{}, nil
}

// executeOK calls Execute and fails the test if either the Go error is
// non-nil or the JSON cannot be parsed. Returns the decoded envelope map.
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

// assertSuccessEnvelope checks that m has success==true and a "data" key.
func assertSuccessEnvelope(t *testing.T, m map[string]any) {
	t.Helper()
	if m["success"] != true {
		t.Errorf("envelope[\"success\"] = %v, want true", m["success"])
	}
	if _, ok := m["data"]; !ok {
		t.Error("envelope missing key \"data\"")
	}
}

// dataFields extracts the "data" map from an envelope.
func dataFields(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	d, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope[\"data\"] = %T %v, want map[string]any", m["data"], m["data"])
	}
	return d
}

// assertFailureEnvelope checks success==false and the error object's kind.
func assertFailureEnvelope(t *testing.T, m map[string]any, wantKind string) map[string]any {
	t.Helper()
	if m["success"] != false {
		t.Errorf("envelope[\"success\"] = %v, want false", m["success"])
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope[\"error\"] = %T %v, want map[string]any", m["error"], m["error"])
	}
	if got, _ := errObj["kind"].(string); got != wantKind {
		t.Errorf("error.kind = %q, want %q", got, wantKind)
	}
	return errObj
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

// TestHistoryTool_Execute_SuccessEnvelopeShape asserts that a success response
// has top-level keys {success, data} and that data carries issue_id and entries.
func TestHistoryTool_Execute_SuccessEnvelopeShape(t *testing.T) {
	t.Parallel()

	tool := New(noopQuery, "10042")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(top) != 2 {
		t.Errorf("Execute success top-level keys = %v, want exactly {success, data}", top)
	}
	assertSuccessEnvelope(t, top)
	d := dataFields(t, top)

	if _, ok := d["issue_id"]; !ok {
		t.Error("data missing key \"issue_id\"")
	}
	if _, ok := d["entries"]; !ok {
		t.Error("data missing key \"entries\"")
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	if got, ok := d["issue_id"].(string); !ok || got != "10042" {
		t.Errorf("data.issue_id = %v, want %q", d["issue_id"], "10042")
	}

	rawEntries, ok := d["entries"].([]any)
	if !ok {
		t.Fatalf("data.entries is not an array: %T %v", d["entries"], d["entries"])
	}
	if len(rawEntries) != 3 {
		t.Fatalf("len(data.entries) = %d, want 3", len(rawEntries))
	}

	// entries[0]: succeeded, nil error → JSON null.
	e0, ok := rawEntries[0].(map[string]any)
	if !ok {
		t.Fatalf("data.entries[0] is not an object: %T", rawEntries[0])
	}
	if got, _ := e0["status"].(string); got != "succeeded" {
		t.Errorf("data.entries[0].status = %q, want %q", got, "succeeded")
	}
	if e0["error"] != nil {
		t.Errorf("data.entries[0].error = %v, want null", e0["error"])
	}

	// entries[1]: failed, non-nil error → JSON string under data.entries[i].error.
	e1, ok := rawEntries[1].(map[string]any)
	if !ok {
		t.Fatalf("data.entries[1] is not an object: %T", rawEntries[1])
	}
	if got, _ := e1["status"].(string); got != "failed" {
		t.Errorf("data.entries[1].status = %q, want %q", got, "failed")
	}
	if got, ok := e1["error"].(string); !ok || got != "agent crashed" {
		t.Errorf("data.entries[1].error = %v, want %q", e1["error"], "agent crashed")
	}
}

// TestHistoryTool_Execute_EmptyEntries asserts that an empty history returns
// data.entries as [] (not null).
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	// data.entries must be a JSON array ([]), not null.
	entries, ok := d["entries"].([]any)
	if !ok {
		t.Fatalf("data.entries is not an array: %T %v", d["entries"], d["entries"])
	}
	if len(entries) != 0 {
		t.Errorf("len(data.entries) = %d, want 0", len(entries))
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	if !called {
		t.Fatal("QueryFunc was not called")
	}
	rawEntries, ok := d["entries"].([]any)
	if !ok {
		t.Fatalf("data.entries is not an array: %T %v", d["entries"], d["entries"])
	}
	if len(rawEntries) != maxEntries {
		t.Errorf("len(data.entries) = %d, want %d", len(rawEntries), maxEntries)
	}
}

// TestHistoryTool_Execute_QueryError asserts that a query failure returns
// success==false, error.kind=="query_failed", error.message equal to the query
// error string, and a nil Go error.
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

	errObj := assertFailureEnvelope(t, m, "query_failed")

	msg, ok := errObj["message"].(string)
	if !ok {
		t.Fatalf("error.message is not a string: %T %v", errObj["message"], errObj["message"])
	}
	if msg != "database is locked" {
		t.Errorf("error.message = %q, want %q", msg, "database is locked")
	}
}

// TestHistoryTool_Execute_FailureEnvelopeShape pins the failure-envelope contract: the
// failure response has top-level keys exactly {success, error} with error
// carrying {kind, message}, and no "data" key.
func TestHistoryTool_Execute_FailureEnvelopeShape(t *testing.T) {
	t.Parallel()

	query := func(_ context.Context, _ string, _ int) ([]Entry, error) {
		return nil, fmt.Errorf("connection reset")
	}

	tool := New(query, "10042")
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

// TestHistoryTool_Execute_PerEntryErrorIsUnderData asserts that the per-entry
// error field is under data.entries[i].error and not conflated with the
// envelope error.
func TestHistoryTool_Execute_PerEntryErrorIsUnderData(t *testing.T) {
	t.Parallel()

	errMsg := "syntax error near line 12"
	query := func(_ context.Context, _ string, _ int) ([]Entry, error) {
		return []Entry{
			{Attempt: 1, Status: "failed", Error: &errMsg},
		}, nil
	}

	tool := New(query, "10042")
	m := executeOK(t, tool)
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	entries, ok := d["entries"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("data.entries = %v, want non-empty array", d["entries"])
	}
	e0, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("data.entries[0] is not an object: %T", entries[0])
	}
	if got, ok := e0["error"].(string); !ok || got != errMsg {
		t.Errorf("data.entries[0].error = %v, want %q", e0["error"], errMsg)
	}
	// The envelope must NOT have an "error" key at the top level.
	if _, hasErr := m["error"]; hasErr {
		t.Error("envelope has top-level \"error\" key on success path, want absent")
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
