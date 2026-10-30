package claude

import (
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// rawRateLimitInfo is one rate-limit report. ResetsAt is Unix seconds; Utilization is
// the used share as a fraction, nil when the report carries none.
type rawRateLimitInfo struct {
	Status        string   `json:"status"`
	ResetsAt      int64    `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
	OverageStatus string   `json:"overageStatus"`
	Utilization   *float64 `json:"utilization"`
}

// windowReadings returns the report as window readings; nil for a nil receiver.
func (i *rawRateLimitInfo) windowReadings() []usage.WindowReading {
	if i == nil {
		return nil
	}
	return []usage.WindowReading{{
		Window:   i.RateLimitType,
		Share:    i.Utilization,
		Status:   i.Status,
		ResetsAt: i.ResetsAt,
	}}
}

// windowTable holds each window's length and scope in one place so that the
// declared spans and the one-model mark cannot drift apart.
var windowTable = map[string]struct {
	span      usage.Span
	modelOnly bool
}{
	"five_hour":                  {span: usage.Span{Minutes: 300}},
	"seven_day":                  {span: usage.Span{Minutes: 10080}},
	"seven_day_opus":             {span: usage.Span{Minutes: 10080, Scope: "Opus"}, modelOnly: true},
	"seven_day_sonnet":           {span: usage.Span{Minutes: 10080, Scope: "Sonnet"}, modelOnly: true},
	"seven_day_overage_included": {span: usage.Span{Minutes: 10080}},
	"overage":                    {span: usage.Span{Scope: "extra usage"}},
}

// rateLimitWindows declares the share unit, status vocabulary, and windows of every
// report. The report names windows without stating their length.
var rateLimitWindows = usage.WindowDeclaration{
	Share: usage.ShareFraction,
	Statuses: map[string]domain.UsageWindowStatus{
		"allowed":         domain.UsageWindowAllowed,
		"allowed_warning": domain.UsageWindowWarning,
		"rejected":        domain.UsageWindowExhausted,
	},
	Spans: windowSpans(),
}

// modelOnlyWindow reports whether the window covers a single model's usage.
func modelOnlyWindow(window string) bool {
	return windowTable[window].modelOnly
}

func windowSpans() map[string]usage.Span {
	spans := make(map[string]usage.Span, len(windowTable))
	for id, w := range windowTable {
		spans[id] = w.span
	}
	return spans
}
