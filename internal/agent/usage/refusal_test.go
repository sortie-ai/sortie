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
	reported := now.Add(-2 * time.Hour)
	soon := now.Add(time.Hour)
	week := now.Add(72 * time.Hour)
	w := func(status domain.UsageWindowStatus, percent *float64, resetsAt time.Time) domain.UsageWindow {
		return domain.UsageWindow{Status: status, UsedPercent: percent, ReportedAt: reported, ResetsAt: resetsAt}
	}
	ex := func(model string, resetsAt time.Time) domain.UsageWindow {
		window := w(domain.UsageWindowExhausted, nil, resetsAt)
		window.Model = model
		return window
	}
	tests := []struct {
		name    string
		model   string
		windows []domain.UsageWindow
		want    time.Time
	}{
		{name: "windows that are not exhausted never count", windows: []domain.UsageWindow{
			ex("", soon), w(domain.UsageWindowAllowed, nil, week), w("", new(99.9), time.Time{}),
		}, want: soon},
		{name: "exhausted status at a low percent", windows: []domain.UsageWindow{w(domain.UsageWindowExhausted, new(40.0), soon)}, want: soon},
		{name: "100 percent without a status", windows: []domain.UsageWindow{w("", new(100.0), soon)}, want: soon},
		{name: "latest wins, short window first", windows: []domain.UsageWindow{ex("", soon), ex("", week)}, want: week},
		{name: "latest wins and is UTC, long window first", windows: []domain.UsageWindow{
			ex("", week.In(time.FixedZone("UTC+3", 3*60*60))), ex("", soon),
		}, want: week},
		{name: "unknown reset beside a future one", windows: []domain.UsageWindow{ex("", week), ex("", time.Time{})}},
		{name: "reset at its report beside a future one", windows: []domain.UsageWindow{ex("", week), ex("", reported)}},
		{name: "reset since the report beside a future one", windows: []domain.UsageWindow{ex("", now.Add(-time.Minute)), ex("", soon)}, want: soon},
		{name: "another model's later window is ignored", model: "m1", windows: []domain.UsageWindow{ex("m1", soon), ex("m2", week)}, want: soon},
		{name: "a window naming no model gates", model: "m1", windows: []domain.UsageWindow{ex("m1", soon), ex("", week)}, want: week},
		{name: "no configured model gates every window", windows: []domain.UsageWindow{ex("m1", soon), ex("m2", week)}, want: week},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usage.RefusalReset(tt.windows, tt.model, now)

			if !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("RefusalReset(%s, model %q) = %v (%v), want %v (UTC)", tt.name, tt.model, got, got.Location(), tt.want)
			}
		})
	}
}
