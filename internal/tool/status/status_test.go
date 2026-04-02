package status

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeStateFile creates <dir>/.sortie/state.json containing the JSON
// encoding of sf. Fails the test immediately on any I/O error.
func writeStateFile(t *testing.T, dir string, sf stateFile) {
	t.Helper()
	dotSortie := filepath.Join(dir, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dotSortie, err)
	}
	data, err := json.Marshal(sf)
	if err != nil {
		t.Fatalf("json.Marshal stateFile: %v", err)
	}
	dst := filepath.Join(dotSortie, "state.json")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", dst, err)
	}
}

// executeOK calls Execute and fails the test if either the Go error is
// non-nil or the JSON cannot be parsed. Returns the decoded map.
func executeOK(t *testing.T, tool *StatusTool) map[string]any {
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

func TestStatusTool_Name(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := New(dir)
	if got := tool.Name(); got != "sortie_status" {
		t.Errorf("Name() = %q, want %q", got, "sortie_status")
	}
}

func TestStatusTool_CorrectTurnAndBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStateFile(t, dir, stateFile{
		TurnNumber: 3,
		MaxTurns:   20,
		Attempt:    nil,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})

	tool := New(dir)
	m := executeOK(t, tool)

	if got, ok := m["turn_number"].(float64); !ok || int(got) != 3 {
		t.Errorf("turn_number = %v, want 3", m["turn_number"])
	}
	if got, ok := m["max_turns"].(float64); !ok || int(got) != 20 {
		t.Errorf("max_turns = %v, want 20", m["max_turns"])
	}
	if got, ok := m["turns_remaining"].(float64); !ok || int(got) != 17 {
		t.Errorf("turns_remaining = %v, want 17", m["turns_remaining"])
	}

	// nil Attempt → JSON null: key present but value nil.
	if attempt, exists := m["attempt"]; !exists {
		t.Error("attempt key missing from response")
	} else if attempt != nil {
		t.Errorf("attempt = %v, want null (nil)", attempt)
	}

	dur, ok := m["session_duration_seconds"].(float64)
	if !ok {
		t.Fatalf("session_duration_seconds is not a float64: %v", m["session_duration_seconds"])
	}
	if dur < 0 {
		t.Errorf("session_duration_seconds = %f, want >= 0", dur)
	}

	tokens, ok := m["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens is not an object: %v", m["tokens"])
	}
	if got, _ := tokens["input_tokens"].(float64); got != 0 {
		t.Errorf("tokens.input_tokens = %v, want 0", tokens["input_tokens"])
	}
}

func TestStatusTool_AttemptNullAndInteger(t *testing.T) {
	t.Parallel()

	t.Run("nil_attempt_is_json_null", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeStateFile(t, dir, stateFile{
			TurnNumber: 1,
			MaxTurns:   10,
			Attempt:    nil,
			StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		})
		m := executeOK(t, New(dir))
		if attempt, exists := m["attempt"]; !exists {
			t.Error("attempt key missing, want null")
		} else if attempt != nil {
			t.Errorf("attempt = %v, want null", attempt)
		}
	})

	t.Run("integer_attempt_is_preserved", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		n := 2
		writeStateFile(t, dir, stateFile{
			TurnNumber: 1,
			MaxTurns:   10,
			Attempt:    &n,
			StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		})
		m := executeOK(t, New(dir))
		got, ok := m["attempt"].(float64)
		if !ok {
			t.Fatalf("attempt = %v (%T), want float64", m["attempt"], m["attempt"])
		}
		if int(got) != 2 {
			t.Errorf("attempt = %v, want 2", got)
		}
	})
}

func TestStatusTool_TokenCounts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStateFile(t, dir, stateFile{
		TurnNumber:      5,
		MaxTurns:        20,
		StartedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		InputTokens:     15000,
		OutputTokens:    3000,
		TotalTokens:     18000,
		CacheReadTokens: 2000,
	})

	tool := New(dir)
	m := executeOK(t, tool)

	tokens, ok := m["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens is not an object: %v", m["tokens"])
	}

	checks := map[string]float64{
		"input_tokens":      15000,
		"output_tokens":     3000,
		"total_tokens":      18000,
		"cache_read_tokens": 2000,
	}
	for field, want := range checks {
		got, ok := tokens[field].(float64)
		if !ok {
			t.Errorf("tokens.%s = %v (%T), want float64", field, tokens[field], tokens[field])
			continue
		}
		if got != want {
			t.Errorf("tokens.%s = %v, want %v", field, got, want)
		}
	}
}

func TestStatusTool_TurnsRemainingFloor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStateFile(t, dir, stateFile{
		TurnNumber: 21,
		MaxTurns:   20,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})

	tool := New(dir)
	m := executeOK(t, tool)

	remaining, ok := m["turns_remaining"].(float64)
	if !ok {
		t.Fatalf("turns_remaining = %v (%T), want float64", m["turns_remaining"], m["turns_remaining"])
	}
	if remaining < 0 {
		t.Errorf("turns_remaining = %v, want >= 0 (floored at zero)", remaining)
	}
	if remaining != 0 {
		t.Errorf("turns_remaining = %v, want 0 when turn_number > max_turns", remaining)
	}
}

func TestStatusTool_MissingStateFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := New(dir)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}
	if _, hasErr := m["error"]; !hasErr {
		t.Errorf("response = %v, want 'error' key for missing state file", m)
	}
}

func TestStatusTool_MalformedJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dotSortie := filepath.Join(dir, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tool := New(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}
	if _, hasErr := m["error"]; !hasErr {
		t.Errorf("response = %v, want 'error' key for malformed JSON", m)
	}
}

func TestStatusTool_EmptyJSONInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStateFile(t, dir, stateFile{
		TurnNumber: 1,
		MaxTurns:   5,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})

	tool := New(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute({}): unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}
	if _, hasErr := m["error"]; hasErr {
		t.Errorf("response = %v, want no 'error' key for empty JSON input", m)
	}
	if _, ok := m["turn_number"]; !ok {
		t.Error("turn_number missing from success response")
	}
}

func TestStatusTool_NullInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStateFile(t, dir, stateFile{
		TurnNumber: 1,
		MaxTurns:   5,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})

	tool := New(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("Execute(null): unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
	}
	if _, hasErr := m["error"]; hasErr {
		t.Errorf("response = %v, want no 'error' key for null input", m)
	}
	if _, ok := m["turn_number"]; !ok {
		t.Error("turn_number missing from success response")
	}
}
