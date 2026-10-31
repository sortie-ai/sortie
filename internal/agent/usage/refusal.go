package usage

import (
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// RefusalReset returns the instant a usage-limit refusal that carries no reset
// of its own lifts, in UTC: the latest future reset among the exhausted windows
// that gate the refused request. A window gates when model is empty, when it
// names no model, or when it names model; a window of another model does not.
// A window is exhausted when its status says so or its used percent reaches
// 100. A request passes only when every window that applies to it has room, so
// the latest reset is the first instant it can pass.
//
// An exhausted gating window whose reset is unknown, or was already at or
// before the window's report, yields the zero time: no instant is known. An
// exhausted gating window whose reset passed after it was reported no longer
// counts. The result is also the zero time when no window is exhausted with a
// future reset.
func RefusalReset(windows []domain.UsageWindow, model string, now time.Time) time.Time {
	var latest time.Time
	for _, w := range windows {
		if !gates(w, model) || !isExhausted(w) {
			continue
		}
		if w.ResetsAt.After(now) {
			if w.ResetsAt.After(latest) {
				latest = w.ResetsAt
			}
			continue
		}
		if !w.ResetsAt.IsZero() && w.ResetsAt.After(w.ReportedAt) {
			continue
		}
		return time.Time{}
	}
	return latest.UTC()
}

func gates(w domain.UsageWindow, model string) bool {
	return model == "" || w.Model == "" || w.Model == model
}

func isExhausted(w domain.UsageWindow) bool {
	return w.Status == domain.UsageWindowExhausted || (w.UsedPercent != nil && *w.UsedPercent >= 100)
}
