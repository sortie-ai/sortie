package opencode

import (
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// freeLimitSource declares the free usage limit a run's error event
// reports.
var freeLimitSource = usage.Source{
	Windows: usage.WindowDeclaration{
		Statuses: map[string]domain.UsageWindowStatus{usagesource.OpenCodeFreeLimitError: domain.UsageWindowExhausted},
		Spans:    map[string]usage.Span{usagesource.OpenCodeFreeLimitError: {Scope: "free usage"}},
	},
}

// freeLimitReadings returns the one reading a free usage limit error
// carries, or nil for any other error. The window resets at the same instant
// the turn's usage_limit does: the retry delay after the failure the event's
// timestamp (Unix milliseconds) records.
func freeLimitReadings(runErr *rawRunError, timestampMS int64) []usage.WindowReading {
	if runErr == nil || runErr.Type != usagesource.OpenCodeFreeLimitError {
		return nil
	}
	reading := usage.WindowReading{Window: usagesource.OpenCodeFreeLimitError, Status: usagesource.OpenCodeFreeLimitError}
	if resetsAt := usagesource.OpenCodeRetryAt(timestampMS, runErr.RetryAfterMs); !resetsAt.IsZero() {
		reading.ResetsAt = resetsAt.Unix()
	}
	return []usage.WindowReading{reading}
}
