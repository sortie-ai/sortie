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
// non-nil or the JSON cannot be parsed. Returns the decoded envelope map.
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

// dataFields extracts the "data" field from an envelope map and asserts it is
// a JSON object. Returns the decoded data map.
func dataFields(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	d, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope[\"data\"] = %T %v, want map[string]any", m["data"], m["data"])
	}
	return d
}

// assertSuccessEnvelope asserts that m has success==true and a "data" key, and
// that no payload field is present at the top level.
func assertSuccessEnvelope(t *testing.T, m map[string]any) {
	t.Helper()
	if m["success"] != true {
		t.Errorf("envelope[\"success\"] = %v, want true", m["success"])
	}
	if _, ok := m["data"]; !ok {
		t.Error("envelope missing key \"data\"")
	}
}

// assertFailureEnvelope asserts that m has success==false, an "error" object
// with the expected kind, and returns the error object.
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

// executeFailure calls Execute, asserts a nil Go error, and returns the
// decoded envelope. Does not assert on success/failure shape.
func executeFailure(t *testing.T, tool *StatusTool) map[string]any {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", out, err)
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	if got, ok := d["turn_number"].(float64); !ok || int(got) != 3 {
		t.Errorf("data.turn_number = %v, want 3", d["turn_number"])
	}
	if got, ok := d["max_turns"].(float64); !ok || int(got) != 20 {
		t.Errorf("data.max_turns = %v, want 20", d["max_turns"])
	}
	if got, ok := d["turns_remaining"].(float64); !ok || int(got) != 17 {
		t.Errorf("data.turns_remaining = %v, want 17", d["turns_remaining"])
	}

	// nil Attempt → JSON null: key present but value nil.
	if attempt, exists := d["attempt"]; !exists {
		t.Error("data.attempt key missing from response")
	} else if attempt != nil {
		t.Errorf("data.attempt = %v, want null (nil)", attempt)
	}

	dur, ok := d["session_duration_seconds"].(float64)
	if !ok {
		t.Fatalf("data.session_duration_seconds is not a float64: %v", d["session_duration_seconds"])
	}
	if dur < 0 {
		t.Errorf("data.session_duration_seconds = %f, want >= 0", dur)
	}

	tokens, ok := d["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("data.tokens is not an object: %v", d["tokens"])
	}
	if got, _ := tokens["input_tokens"].(float64); got != 0 {
		t.Errorf("data.tokens.input_tokens = %v, want 0", tokens["input_tokens"])
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
		assertSuccessEnvelope(t, m)
		d := dataFields(t, m)
		if attempt, exists := d["attempt"]; !exists {
			t.Error("data.attempt key missing, want null")
		} else if attempt != nil {
			t.Errorf("data.attempt = %v, want null", attempt)
		}
	})

	t.Run("integer_attempt_is_preserved", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeStateFile(t, dir, stateFile{
			TurnNumber: 1,
			MaxTurns:   10,
			Attempt:    new(2),
			StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		})
		m := executeOK(t, New(dir))
		assertSuccessEnvelope(t, m)
		d := dataFields(t, m)
		got, ok := d["attempt"].(float64)
		if !ok {
			t.Fatalf("data.attempt = %v (%T), want float64", d["attempt"], d["attempt"])
		}
		if int(got) != 2 {
			t.Errorf("data.attempt = %v, want 2", got)
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	tokens, ok := d["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("data.tokens is not an object: %v", d["tokens"])
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
			t.Errorf("data.tokens.%s = %v (%T), want float64", field, tokens[field], tokens[field])
			continue
		}
		if got != want {
			t.Errorf("data.tokens.%s = %v, want %v", field, got, want)
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
	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	remaining, ok := d["turns_remaining"].(float64)
	if !ok {
		t.Fatalf("data.turns_remaining = %v (%T), want float64", d["turns_remaining"], d["turns_remaining"])
	}
	if remaining < 0 {
		t.Errorf("data.turns_remaining = %v, want >= 0 (floored at zero)", remaining)
	}
	if remaining != 0 {
		t.Errorf("data.turns_remaining = %v, want 0 when turn_number > max_turns", remaining)
	}
}

// TestStatusTool_SuccessEnvelopeShape pins the success-envelope contract: top-level keys
// are exactly {success, data}, payload fields are absent at the top level.
func TestStatusTool_SuccessEnvelopeShape(t *testing.T) {
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
		t.Fatalf("Execute: unexpected Go error: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(top) != 2 {
		t.Errorf("Execute success top-level keys = %v, want exactly {success, data}", top)
	}
	if top["success"] != true {
		t.Errorf("Execute success[\"success\"] = %v, want true", top["success"])
	}
	if _, ok := top["data"]; !ok {
		t.Error("Execute success missing key \"data\"")
	}
	// Payload fields must NOT appear at the top level.
	for _, payloadKey := range []string{"turn_number", "max_turns", "turns_remaining", "tokens", "session_duration_seconds"} {
		if _, exists := top[payloadKey]; exists {
			t.Errorf("Execute success has payload key %q at top level, want it under data", payloadKey)
		}
	}
}

// TestStatusTool_MissingStateFile asserts that a missing state file returns
// success==false, error.kind=="state_unavailable", a non-empty error.message,
// and a nil Go error.
func TestStatusTool_MissingStateFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := New(dir)

	m := executeFailure(t, tool)
	errObj := assertFailureEnvelope(t, m, "state_unavailable")

	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Error("error.message is empty for missing state file, want non-empty")
	}
}

// TestStatusTool_MalformedJSON asserts that malformed state JSON returns
// success==false, error.kind=="state_malformed", and a nil Go error.
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
	m := executeFailure(t, tool)
	errObj := assertFailureEnvelope(t, m, "state_malformed")

	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Error("error.message is empty for malformed JSON, want non-empty")
	}
}

// TestStatusTool_InvalidStartedAt asserts that a state file whose started_at
// cannot be parsed returns success==false, error.kind=="state_malformed", and
// a nil Go error.
func TestStatusTool_InvalidStartedAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dotSortie := filepath.Join(dir, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	state := []byte(`{"turn_number":1,"max_turns":10,"started_at":"not-a-timestamp"}`)
	if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), state, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tool := New(dir)
	m := executeFailure(t, tool)
	errObj := assertFailureEnvelope(t, m, "state_malformed")

	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Error("error.message is empty for invalid started_at, want non-empty")
	}
}

// TestStatusTool_OversizedStateFile asserts that a state file that exceeds the
// size limit returns success==false, error.kind=="state_unavailable", and a nil
// Go error.
func TestStatusTool_OversizedStateFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dotSortie := filepath.Join(dir, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	bigContent := make([]byte, maxStateFileBytes+1)
	if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), bigContent, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tool := New(dir)
	m := executeFailure(t, tool)
	assertFailureEnvelope(t, m, "state_unavailable")
}

// TestStatusTool_FailureEnvelopeShape pins the failure-envelope contract: top-level keys
// are exactly {success, error}, with error carrying {kind, message}.
func TestStatusTool_FailureEnvelopeShape(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := New(dir) // no state file → state_unavailable failure

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
	if _, ok := errObj["kind"]; !ok {
		t.Error("error object missing key \"kind\"")
	}
	if _, ok := errObj["message"]; !ok {
		t.Error("error object missing key \"message\"")
	}
}

// TestStatusTool_ErrorMessageExactStrings asserts that every failure site
// emits the exact message string the tool produced before this change.
func TestStatusTool_ErrorMessageExactStrings(t *testing.T) {
	t.Parallel()

	t.Run("missing_file_message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tool := New(dir)
		m := executeFailure(t, tool)
		errObj := assertFailureEnvelope(t, m, "state_unavailable")
		msg, _ := errObj["message"].(string)
		want := "state file unavailable: "
		if len(msg) < len(want) || msg[:len(want)] != want {
			t.Errorf("error.message = %q, want prefix %q", msg, want)
		}
	})

	t.Run("malformed_json_message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dotSortie := filepath.Join(dir, ".sortie")
		if err := os.MkdirAll(dotSortie, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), []byte(`{bad`), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		tool := New(dir)
		m := executeFailure(t, tool)
		errObj := assertFailureEnvelope(t, m, "state_malformed")
		msg, _ := errObj["message"].(string)
		want := "state file malformed: "
		if len(msg) < len(want) || msg[:len(want)] != want {
			t.Errorf("error.message = %q, want prefix %q", msg, want)
		}
	})

	t.Run("oversized_message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dotSortie := filepath.Join(dir, ".sortie")
		if err := os.MkdirAll(dotSortie, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		bigContent := make([]byte, maxStateFileBytes+1)
		if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), bigContent, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		tool := New(dir)
		m := executeFailure(t, tool)
		errObj := assertFailureEnvelope(t, m, "state_unavailable")
		msg, _ := errObj["message"].(string)
		if msg != "state file exceeds size limit" {
			t.Errorf("error.message = %q, want %q", msg, "state file exceeds size limit")
		}
	})

	t.Run("invalid_started_at_message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dotSortie := filepath.Join(dir, ".sortie")
		if err := os.MkdirAll(dotSortie, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		state := []byte(`{"turn_number":1,"max_turns":10,"started_at":"not-valid"}`)
		if err := os.WriteFile(filepath.Join(dotSortie, "state.json"), state, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		tool := New(dir)
		m := executeFailure(t, tool)
		errObj := assertFailureEnvelope(t, m, "state_malformed")
		msg, _ := errObj["message"].(string)
		want := "state file has invalid started_at: "
		if len(msg) < len(want) || msg[:len(want)] != want {
			t.Errorf("error.message = %q, want prefix %q", msg, want)
		}
	})
}

// TestStatusTool_EmptyJSONInput asserts that {} input with a valid state file
// succeeds and data carries turn_number.
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

	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	if _, ok := d["turn_number"]; !ok {
		t.Error("data.turn_number missing from success response")
	}
}

// TestStatusTool_NullInput asserts that null input with a valid state file
// succeeds and data carries turn_number.
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

	assertSuccessEnvelope(t, m)
	d := dataFields(t, m)

	if _, ok := d["turn_number"]; !ok {
		t.Error("data.turn_number missing from success response")
	}
}
