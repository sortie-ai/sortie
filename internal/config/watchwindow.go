package config

import (
	"fmt"
	"math"
	"time"
)

// MaxWatchWindowMS is the largest watch_window_ms value whose conversion to
// a time.Duration stays positive.
const MaxWatchWindowMS int64 = math.MaxInt64 / int64(time.Millisecond)

// ValidateWatchWindowMS reports why ms is not a usable watch window in
// milliseconds, and nil when it is.
//
// ms is an already-coerced millisecond count; ValidateWatchWindowMS performs
// no type coercion of its own. The returned error carries no field name or
// key name, so a caller composes it with whatever identifier its own
// diagnostic uses. ValidateWatchWindowMS is a pure function of its argument
// and is safe for concurrent use.
func ValidateWatchWindowMS(ms int) error {
	if ms < 0 {
		return fmt.Errorf("must be non-negative, got %d", ms)
	}
	if int64(ms) > MaxWatchWindowMS {
		return fmt.Errorf("must not exceed %d (about 292 years); use 0 for no time limit, got %d", MaxWatchWindowMS, ms)
	}
	return nil
}
