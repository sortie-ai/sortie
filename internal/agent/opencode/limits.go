package opencode

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const freeLimitType = "provider.free-limit"

// freeLimitSource declares the free usage limit a run's error event
// reports.
var freeLimitSource = usage.Source{
	Windows: usage.WindowDeclaration{
		Statuses: map[string]domain.UsageWindowStatus{freeLimitType: domain.UsageWindowExhausted},
		Spans:    map[string]usage.Span{freeLimitType: {Scope: "free usage"}},
	},
}

// freeLimitReadings returns the one reading a free usage limit error
// carries, or nil for any other error. The retry delay is relative to the
// failure and in milliseconds.
func freeLimitReadings(runErr *rawRunError) []usage.WindowReading {
	if runErr == nil || runErr.Type != freeLimitType {
		return nil
	}
	reading := usage.WindowReading{Window: freeLimitType, Status: freeLimitType}
	if runErr.RetryAfterMs != nil {
		reading.ResetsAfter = time.Duration(*runErr.RetryAfterMs) * time.Millisecond
	}
	return []usage.WindowReading{reading}
}
