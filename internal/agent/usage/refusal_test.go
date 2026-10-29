package usage_test

import (
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestRefusalReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	soon := now.Add(time.Hour)
	w := func(status domain.UsageWindowStatus, percent *float64, resetsAt time.Time) domain.UsageWindow {
		return domain.UsageWindow{Status: status, UsedPercent: percent, ResetsAt: resetsAt}
	}
	tests := []struct {
		name    string
		windows []domain.UsageWindow
		want    time.Time
	}{
		{"windows that are not exhausted", []domain.UsageWindow{w(domain.UsageWindowAllowed, nil, soon), w("", new(99.9), soon)}, time.Time{}},
		{"resets that are not after now", []domain.UsageWindow{w(domain.UsageWindowExhausted, nil, now), w("", new(100.0), now.Add(-time.Minute))}, time.Time{}},
		{"exhausted status at a low percent", []domain.UsageWindow{w(domain.UsageWindowExhausted, new(40.0), soon)}, soon},
		{"100 percent without a status", []domain.UsageWindow{w("", new(100.0), soon)}, soon},
		{"earliest exhausted wins and is UTC", []domain.UsageWindow{
			w(domain.UsageWindowExhausted, nil, now.Add(48*time.Hour)),
			w(domain.UsageWindowAllowed, nil, now.Add(time.Minute)),
			w(domain.UsageWindowExhausted, nil, soon.In(time.FixedZone("UTC+3", 3*60*60))),
		}, soon},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usage.RefusalReset(tt.windows, now)

			if !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("RefusalReset(%s) = %v (%v), want %v (UTC)", tt.name, got, got.Location(), tt.want)
			}
		})
	}
}
