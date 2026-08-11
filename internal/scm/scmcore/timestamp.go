package scmcore

import (
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ParseTimestamp parses value as an RFC 3339 timestamp and returns it in
// UTC. field names the wire field for the error message, for example
// "issue event created_at"; it is the only forge-specific vocabulary
// that crosses into this package.
//
// On success ParseTimestamp returns a nil error interface, never a typed
// nil [*domain.SCMError], so a caller comparing the result against nil
// never takes the failure branch on a well-formed timestamp. On failure,
// including an empty value, it returns the zero [time.Time] and a
// [*domain.SCMError] of Kind [domain.ErrSCMPayload] whose Err wraps the
// underlying parse error. ParseTimestamp accepts no layout other than
// [time.RFC3339], has no side effects, and is safe for concurrent use.
func ParseTimestamp(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse " + field,
			Err:     err,
		}
	}
	return t.UTC(), nil
}

// ParseTimestampOrZero parses value as an RFC 3339 timestamp and returns
// it in UTC, returning the zero [time.Time] when value is empty or is
// not RFC 3339 rather than failing. ParseTimestampOrZero has no side
// effects and is safe for concurrent use.
func ParseTimestampOrZero(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
