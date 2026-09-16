package sshutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// IsEnvName reports whether name is a valid POSIX shell environment
// variable name: non-empty, holding only ASCII letters, ASCII digits,
// and underscore, and not starting with a digit.
func IsEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// IsReservedEnvName reports whether name is one the SSH carrier uses
// for itself, so a launch must not carry it even though [IsEnvName]
// accepts it.
func IsReservedEnvName(name string) bool {
	return name == completionMarkerName
}

// EnvVar is one environment variable carried into a remote agent
// launch. Every rendering of an EnvVar - through [fmt], through a
// [log/slog] handler, or through [encoding/json.Marshal] - carries
// Name and never Value, so a carried credential never reaches a log
// record.
type EnvVar struct {
	Name  string
	Value string
}

// Format implements [fmt.Formatter], rendering only v.Name for every
// verb so a carried value never reaches a formatted log line or
// error.
func (v EnvVar) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sshutil.EnvVar{Name:%q}", v.Name) //nolint:errcheck // best-effort formatting
}

// LogValue implements [slog.LogValuer], rendering only v.Name so a
// carried value never reaches a structured log record.
func (v EnvVar) LogValue() slog.Value {
	return slog.StringValue(v.Name)
}

// MarshalJSON implements [json.Marshaler], rendering only v.Name so a
// carried value never reaches a JSON-encoded log record.
func (v EnvVar) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name string `json:"name"`
	}{Name: v.Name})
}
