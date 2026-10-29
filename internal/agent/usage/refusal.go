package usage

import (
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// RefusalReset returns the earliest future reset among the exhausted windows,
// in UTC, or the zero time when no window is exhausted with a reset after now.
// A window is exhausted when its status says so or its used percent reaches
// 100. The earliest reset wins: a later one that belongs to another allowance
// would hold a model past its real reset.
func RefusalReset(windows []domain.UsageWindow, now time.Time) time.Time {
	var earliest time.Time
	for _, w := range windows {
		if !isExhausted(w) || !w.ResetsAt.After(now) {
			continue
		}
		if earliest.IsZero() || w.ResetsAt.Before(earliest) {
			earliest = w.ResetsAt
		}
	}
	return earliest.UTC()
}

func isExhausted(w domain.UsageWindow) bool {
	return w.Status == domain.UsageWindowExhausted || (w.UsedPercent != nil && *w.UsedPercent >= 100)
}
