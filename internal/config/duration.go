package config

import (
	"math"
	"time"
)

// MaxDurationMS is the largest millisecond count whose conversion to a
// time.Duration stays positive.
const MaxDurationMS int64 = math.MaxInt64 / int64(time.Millisecond)
