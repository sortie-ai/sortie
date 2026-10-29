// Package usagelimits names the Copilot CLI session errors that are a provider
// usage-limit refusal. The copilot-cli kind reads the event from the runtime's
// output lines, and the agent-client-protocol kind reads it from a
// notification of the turn; both carry the same data object, so both route
// through this package. It registers no kind and holds no adapter. Start from
// [ParseSessionError] and [RefusalOf].
package usagelimits

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// The runtime's own names for the session error event and for the error types
// that are a usage-limit refusal.
const (
	SessionErrorEvent = "session.error"
	QuotaError        = "quota"
	RateLimitError    = "rate_limit"
)

var errNotObject = errors.New("session error data is not a JSON object")

// SessionError is the data object of one session.error event.
type SessionError struct {
	// ErrorType is data.errorType; empty when absent or not a JSON string.
	ErrorType string

	// Message is data.message, trimmed; empty when absent or not a JSON string.
	Message string
}

// Refusal is what a refusing session error means for the run.
type Refusal struct {
	// Limit is model-only with no reset.
	Limit domain.UsageLimit

	// QuotaWindow reports that the runtime's pulled quota windows hold this
	// refusal's reset.
	QuotaWindow bool
}

// ParseSessionError decodes the data object of a session.error event. It fails
// when data is not a JSON object; a member of another JSON type reads as empty.
func ParseSessionError(data []byte) (SessionError, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return SessionError{}, fmt.Errorf("parse session error data: %w", err)
	}
	if members == nil {
		return SessionError{}, fmt.Errorf("parse session error data: %w", errNotObject)
	}
	return SessionError{
		ErrorType: stringMember(members["errorType"]),
		Message:   strings.TrimSpace(stringMember(members["message"])),
	}, nil
}

// RefusalOf reports whether e is a usage-limit refusal: an error type of
// exactly [QuotaError] or [RateLimitError]. Both are model-only, because a
// premium-request quota leaves the included models serving and some rate
// limits name one model. Only a quota refusal reads its reset from the pulled
// quota windows, since no quota snapshot reports a rate limit.
func RefusalOf(e SessionError) (Refusal, bool) {
	switch e.ErrorType {
	case QuotaError, RateLimitError:
		return Refusal{
			Limit:       domain.UsageLimit{ModelOnly: true},
			QuotaWindow: e.ErrorType == QuotaError,
		}, true
	}
	return Refusal{}, false
}

func stringMember(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
