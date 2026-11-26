package usagesource

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
	CopilotSessionErrorEvent = "session.error"
	CopilotQuotaError        = "quota"
	CopilotRateLimitError    = "rate_limit"
)

var errNotObject = errors.New("session error data is not a JSON object")

// CopilotSessionError is the data object of one session.error event.
type CopilotSessionError struct {
	// ErrorType is data.errorType; empty when absent or not a JSON string.
	ErrorType string

	// Message is data.message, trimmed; empty when absent or not a JSON string.
	Message string
}

// CopilotRefusal is what a refusing session error means for the run.
type CopilotRefusal struct {
	// Limit is model-only with no reset.
	Limit domain.UsageLimit

	// QuotaWindow reports that the runtime's pulled quota windows hold this
	// refusal's reset.
	QuotaWindow bool
}

// CopilotParseSessionError decodes the data object of a session.error event. It fails
// when data is not a JSON object; a member of another JSON type reads as empty.
func CopilotParseSessionError(data []byte) (CopilotSessionError, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return CopilotSessionError{}, fmt.Errorf("parse session error data: %w", err)
	}
	if members == nil {
		return CopilotSessionError{}, fmt.Errorf("parse session error data: %w", errNotObject)
	}
	return CopilotSessionError{
		ErrorType: stringMember(members["errorType"]),
		Message:   strings.TrimSpace(stringMember(members["message"])),
	}, nil
}

// CopilotRefusalOf reports whether e is a usage-limit refusal: an error type of
// exactly [CopilotQuotaError] or [CopilotRateLimitError]. Both are model-only, because a
// premium-request quota leaves the included models serving and some rate
// limits name one model. Only a quota refusal reads its reset from the pulled
// quota windows, since no quota snapshot reports a rate limit.
func CopilotRefusalOf(e CopilotSessionError) (CopilotRefusal, bool) {
	switch e.ErrorType {
	case CopilotQuotaError, CopilotRateLimitError:
		return CopilotRefusal{
			Limit:       domain.UsageLimit{ModelOnly: true},
			QuotaWindow: e.ErrorType == CopilotQuotaError,
		}, true
	}
	return CopilotRefusal{}, false
}

func stringMember(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
