package toolresult

import (
	"encoding/json"
	"sort"
	"testing"
)

// decodeTop unmarshals raw into a map of raw JSON values keyed by field name.
func decodeTop(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decodeTop: unmarshal %q: %v", raw, err)
	}
	return m
}

// decodeBool unmarshals a JSON boolean.
func decodeBool(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decodeBool: %v (raw=%q)", err, raw)
	}
	return v
}

// decodeString unmarshals a JSON string.
func decodeString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decodeString: %v (raw=%q)", err, raw)
	}
	return v
}

// TestSuccess_TopLevelShape asserts that Success produces an object whose
// top-level keys are exactly {success, data} with success == true.
func TestSuccess_TopLevelShape(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"foo": "bar", "n": 42}

	raw, err := Success(payload)
	if err != nil {
		t.Fatalf("Success(payload) Go error = %v, want nil", err)
	}

	top := decodeTop(t, raw)

	if len(top) != 2 {
		t.Errorf("Success top-level keys = %v, want exactly {success, data}", keysOf(top))
	}
	if _, ok := top["success"]; !ok {
		t.Error("Success result missing key \"success\"")
	}
	if _, ok := top["data"]; !ok {
		t.Error("Success result missing key \"data\"")
	}
	if !decodeBool(t, top["success"]) {
		t.Errorf("Success result[\"success\"] = false, want true")
	}
}

// TestSuccess_DataCarriesPayload asserts that the value under data equals the
// supplied payload.
func TestSuccess_DataCarriesPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload any
	}{
		{"string value", "hello"},
		{"int value", 42},
		{"bool value", true},
		{"nil value", nil},
		{"object", map[string]any{"a": 1, "b": "two"}},
		{"array", []int{1, 2, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, err := Success(tt.payload)
			if err != nil {
				t.Fatalf("Success(%v) Go error = %v, want nil", tt.payload, err)
			}

			top := decodeTop(t, raw)
			wantData, _ := json.Marshal(tt.payload)
			if string(top["data"]) != string(wantData) {
				t.Errorf("Success(%v) data = %s, want %s", tt.payload, top["data"], wantData)
			}
		})
	}
}

// TestSuccess_GoErrorNilForJSONSafeInputs asserts that JSON-safe payloads
// never produce a non-nil Go error.
func TestSuccess_GoErrorNilForJSONSafeInputs(t *testing.T) {
	t.Parallel()

	inputs := []any{
		nil,
		"",
		0,
		true,
		map[string]any{},
		[]any{},
		map[string]any{"turn_number": 3, "max_turns": 20, "turns_remaining": 17},
	}

	for _, in := range inputs {
		_, err := Success(in)
		if err != nil {
			t.Errorf("Success(%T) Go error = %v, want nil", in, err)
		}
	}
}

// TestFailure_TopLevelShape asserts that Failure produces an object whose
// top-level keys are exactly {success, error} with success == false, and that
// error carries {kind, message}.
func TestFailure_TopLevelShape(t *testing.T) {
	t.Parallel()

	raw, err := Failure("state_unavailable", "state file is a symlink")
	if err != nil {
		t.Fatalf("Failure(kind, msg) Go error = %v, want nil", err)
	}

	top := decodeTop(t, raw)

	if len(top) != 2 {
		t.Errorf("Failure top-level keys = %v, want exactly {success, error}", keysOf(top))
	}
	if _, ok := top["success"]; !ok {
		t.Error("Failure result missing key \"success\"")
	}
	if _, ok := top["error"]; !ok {
		t.Error("Failure result missing key \"error\"")
	}
	if decodeBool(t, top["success"]) {
		t.Errorf("Failure result[\"success\"] = true, want false")
	}

	errFields := decodeTop(t, top["error"])
	if len(errFields) != 2 {
		t.Errorf("Failure error keys = %v, want exactly {kind, message}", keysOf(errFields))
	}
	if _, ok := errFields["kind"]; !ok {
		t.Error("Failure error object missing key \"kind\"")
	}
	if _, ok := errFields["message"]; !ok {
		t.Error("Failure error object missing key \"message\"")
	}
}

// TestFailure_KindAndMessagePreserved asserts that the kind and message are
// propagated verbatim.
func TestFailure_KindAndMessagePreserved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    string
		message string
	}{
		{"state_unavailable", "state_unavailable", "state file unavailable: no such file or directory"},
		{"state_unavailable symlink", "state_unavailable", "state file is a symlink"},
		{"state_unavailable oversized", "state_unavailable", "state file exceeds size limit"},
		{"state_malformed", "state_malformed", "state file malformed: unexpected end of JSON input"},
		{"state_malformed started_at", "state_malformed", "state file has invalid started_at: parsing time ..."},
		{"query_failed", "query_failed", "database is locked"},
		{"send_failed", "send_failed", "notification delivery failed: connection failure"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, goErr := Failure(tt.kind, tt.message)
			if goErr != nil {
				t.Fatalf("Failure(%q, %q) Go error = %v, want nil", tt.kind, tt.message, goErr)
			}

			top := decodeTop(t, raw)
			errFields := decodeTop(t, top["error"])

			if got := decodeString(t, errFields["kind"]); got != tt.kind {
				t.Errorf("Failure(%q, ...) error.kind = %q, want %q", tt.kind, got, tt.kind)
			}
			if got := decodeString(t, errFields["message"]); got != tt.message {
				t.Errorf("Failure(%q, %q) error.message = %q, want %q", tt.kind, tt.message, got, tt.message)
			}
		})
	}
}

// TestFailure_GoErrorNilForStringInputs asserts that string-only inputs never
// produce a non-nil Go error (strings are always JSON-safe).
func TestFailure_GoErrorNilForStringInputs(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{
		{"query_failed", "database is locked"},
		{"state_unavailable", "state file unavailable: open /x: permission denied"},
		{"state_malformed", "state file malformed: unexpected EOF"},
		{"send_failed", "notification delivery failed: timeout"},
		{"", ""},
	}

	for _, p := range pairs {
		_, err := Failure(p[0], p[1])
		if err != nil {
			t.Errorf("Failure(%q, %q) Go error = %v, want nil", p[0], p[1], err)
		}
	}
}

// TestSuccess_GoErrorOnUnmarshalablePayload asserts that Success returns a
// non-nil Go error when given a payload that encoding/json cannot marshal,
// and does not panic.
func TestSuccess_GoErrorOnUnmarshalablePayload(t *testing.T) {
	t.Parallel()

	_, err := Success(func() {})

	if err == nil {
		t.Errorf("Success(func(){}) Go error = nil, want non-nil marshal error")
	}
}

// TestSuccess_ByteShapeMatchesPriorSuccessResult asserts that Success(payload)
// produces identical bytes to the historic per-tool successResult pattern
// ({"success": true, "data": payload}) for a representative payload.
// This guards that the shared helper is byte-compatible with tracker_api's
// former local helper.
func TestSuccess_ByteShapeMatchesPriorSuccessResult(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"transitioned": true}

	got, err := Success(payload)
	if err != nil {
		t.Fatalf("Success(payload) error = %v", err)
	}

	// Reconstruct what tracker_api's former successResult produced.
	want, err := json.Marshal(map[string]any{"success": true, "data": payload})
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("Success(payload) = %s, want %s", got, want)
	}
}

// TestFailure_ByteShapeMatchesPriorErrorResult asserts that Failure(kind, msg)
// produces identical bytes to the historic per-tool errorResult pattern
// ({"success": false, "error": {"kind": ..., "message": ...}}) for a
// representative set of inputs.
// This guards that the shared helper is byte-compatible with the former
// Tier 2 local errorResult helpers.
func TestFailure_ByteShapeMatchesPriorErrorResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind    string
		message string
	}{
		{"invalid_input", "failed to parse input: unexpected EOF"},
		{"rate_limited", "per-session notification cap reached"},
		{"send_failed", "notification delivery failed: connection failure"},
		{"tracker_not_found", "issue not found"},
		{"internal_error", "unexpected nil"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()

			got, err := Failure(tt.kind, tt.message)
			if err != nil {
				t.Fatalf("Failure(%q, %q) error = %v", tt.kind, tt.message, err)
			}

			// Reconstruct what the former per-tool errorResult produced.
			want, err := json.Marshal(map[string]any{
				"success": false,
				"error": map[string]string{
					"kind":    tt.kind,
					"message": tt.message,
				},
			})
			if err != nil {
				t.Fatalf("marshal want: %v", err)
			}

			if string(got) != string(want) {
				t.Errorf("Failure(%q, %q) = %s, want %s", tt.kind, tt.message, got, want)
			}
		})
	}
}

// keysOf returns the keys of a map[string]json.RawMessage as a sorted slice.
func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
