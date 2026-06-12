// Package toolresult marshals the uniform result envelope every built-in
// tool returns. On success a tool returns {"success": true, "data": ...};
// on a domain failure it returns
// {"success": false, "error": {"kind": "...", "message": "..."}}. Both
// marshalers are pure and safe for concurrent use; the non-nil Go error is
// reserved for an internal marshal failure.
package toolresult

import "encoding/json"

// Success marshals the success envelope {"success": true, "data": data}.
//
// It returns a non-nil error only when data fails to marshal.
func Success(data any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"success": true,
		"data":    data,
	})
}

// Failure marshals the failure envelope
// {"success": false, "error": {"kind": kind, "message": message}}.
//
// It returns a non-nil error only on an internal marshal failure, which
// cannot occur for string inputs.
func Failure(kind, message string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"success": false,
		"error": map[string]string{
			"kind":    kind,
			"message": message,
		},
	})
}
