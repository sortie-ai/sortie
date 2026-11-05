package runstats

import (
	"fmt"
	"time"
)

// ParseRangeBound parses a since or until value into a UTC time bound. An
// empty raw returns nil, nil, meaning the bound is absent. raw MUST be an
// RFC 3339 timestamp, a YYYY-MM-DD date (read as 00:00:00Z on that date), or
// a positive Go duration (read as now minus that duration); any other input,
// including a zero or negative duration, returns an error.
func ParseRangeBound(raw string, now time.Time) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return new(t.UTC()), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return new(t.UTC()), nil
	}
	if d, ok := parseDurationBound(raw); ok {
		return new(now.UTC().Add(-d)), nil
	}
	return nil, fmt.Errorf(
		"invalid range bound %q: accepts an RFC3339 timestamp (2026-07-01T00:00:00Z), "+
			"a date (2026-07-01), or a positive duration (24h)", raw)
}

// IsDurationBound reports whether text is a positive duration that
// [ParseRangeBound] accepts, as opposed to an RFC 3339 timestamp or a date.
func IsDurationBound(text string) bool {
	_, ok := parseDurationBound(text)
	return ok
}

func parseDurationBound(raw string) (time.Duration, bool) {
	d, err := time.ParseDuration(raw)
	return d, err == nil && d > 0
}

// ParsePeriod parses a spend period name. text MUST be one of the [Period]
// values; any other input, the empty string included, returns an error.
func ParsePeriod(text string) (Period, error) {
	switch p := Period(text); p {
	case PeriodFiveMinutes, PeriodQuarterHour, PeriodHour, PeriodDay, PeriodMonth:
		return p, nil
	}
	return "", fmt.Errorf("invalid period %q: accepts five_minutes, quarter_hour, hour, day, or month", text)
}
