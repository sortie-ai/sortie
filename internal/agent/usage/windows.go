package usage

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	minutesPerHour = 60
	minutesPerDay  = 24 * minutesPerHour
	minutesPerWeek = 7 * minutesPerDay
)

// Windows normalizes readings from source into usage windows reported at
// the given instant, or at a reading's own earlier observation time. It
// returns nil for no readings and never aliases the readings' values: each
// window gets a fresh amount and share.
func Windows(source Source, readings []WindowReading, at time.Time) []domain.UsageWindow {
	if len(readings) == 0 {
		return nil
	}
	reportedAt := at.UTC()
	windows := make([]domain.UsageWindow, 0, len(readings))
	for _, r := range readings {
		span, declared := source.Windows.Spans[r.Window]
		if !declared {
			span = r.Span
		}
		minutes := max(span.Minutes, 0)
		observed := observedAt(r.ObservedAt, reportedAt)

		var resetsAt time.Time
		switch {
		case r.ResetsAt > 0:
			resetsAt = time.Unix(r.ResetsAt, 0).UTC()
		case r.ResetsAfter > 0:
			resetsAt = observed.Add(r.ResetsAfter)
		}

		amount := amountOf(source.Windows.Units[r.Window], r)
		used := usedPercent(source.Windows, r.Share)
		if used == nil && amount != nil {
			ratio := amount.Used / amount.Limit * 100
			used = &ratio
		}

		windows = append(windows, domain.UsageWindow{
			Key:         r.Window,
			Label:       labelOf(lengthOf(span, minutes), strings.TrimSpace(span.Scope)),
			Minutes:     minutes,
			Model:       span.Model,
			UsedPercent: used,
			Status:      source.Windows.Statuses[r.Status],
			ResetsAt:    resetsAt,
			ReportedAt:  observed,
			Amount:      amount,
		})
	}
	return windows
}

// observedAt is the runtime's own observation time unless it is absent or
// lies after reportedAt, which a clock skew or a malformed report can cause.
func observedAt(unix int64, reportedAt time.Time) time.Time {
	if unix <= 0 {
		return reportedAt
	}
	observed := time.Unix(unix, 0).UTC()
	if observed.After(reportedAt) {
		return reportedAt
	}
	return observed
}

// amountOf returns the reading's amount in the declared unit, or nil when
// the unit is undeclared or a value is missing, non-finite, or out of range.
func amountOf(unit string, r WindowReading) *domain.UsageWindowAmount {
	if unit == "" || !finite(r.Used) || !finite(r.Limit) || *r.Used < 0 || *r.Limit <= 0 {
		return nil
	}
	return &domain.UsageWindowAmount{Used: *r.Used, Limit: *r.Limit, Unit: unit}
}

func finite(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0)
}

// usedPercent converts a share in the declared unit to the used percent.
// It never clamps, so a used share above the whole window stays above 100,
// while a remaining share above the whole yields nil.
func usedPercent(decl WindowDeclaration, share *float64) *float64 {
	if !finite(share) {
		return nil
	}
	var whole float64
	switch decl.Share {
	case ShareFraction:
		whole = 1
	case SharePercent:
		whole = 100
	default:
		return nil
	}
	s := *share
	if decl.ShareRemaining {
		s = whole - s
	}
	if s < 0 {
		return nil
	}
	percent := s
	if decl.Share == ShareFraction {
		percent = s * 100
	}
	return &percent
}

func labelOf(length, scope string) string {
	if length == "" && scope == "" {
		return "Usage limit"
	}
	words := make([]string, 0, 3)
	for _, w := range []string{length, scope, "limit"} {
		if w != "" {
			words = append(words, w)
		}
	}
	label := strings.Join(words, " ")
	first, size := utf8.DecodeRuneInString(label)
	return string(unicode.ToUpper(first)) + label[size:]
}

func lengthOf(span Span, minutes int64) string {
	if minutes > 0 {
		return lengthWord(minutes)
	}
	return strings.TrimSpace(span.Period)
}

func lengthWord(minutes int64) string {
	switch {
	case minutes == 0:
		return ""
	case minutes == minutesPerDay:
		return "Daily"
	case minutes == minutesPerWeek:
		return "Weekly"
	case minutes%minutesPerDay == 0:
		return strconv.FormatInt(minutes/minutesPerDay, 10) + "-day"
	case minutes%minutesPerHour == 0:
		return strconv.FormatInt(minutes/minutesPerHour, 10) + "-hour"
	default:
		return strconv.FormatInt(minutes, 10) + "-minute"
	}
}

// MergeWindows returns held updated with reported: a reported window
// replaces the held window of the same key unless it is an earlier report,
// and a held window the report omits is kept. The result is a new slice
// sorted by length (unstated lengths last), label, and key.
func MergeWindows(held, reported []domain.UsageWindow) []domain.UsageWindow {
	result := slices.Clone(held)
	for _, w := range reported {
		i := slices.IndexFunc(result, func(h domain.UsageWindow) bool { return h.Key == w.Key })
		switch {
		case i < 0:
			result = append(result, w)
		case !w.ReportedAt.Before(result[i].ReportedAt):
			result[i] = w
		}
	}
	slices.SortStableFunc(result, compareWindows)
	return result
}

func compareWindows(a, b domain.UsageWindow) int {
	if c := cmp.Compare(lengthUnstated(a), lengthUnstated(b)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Minutes, b.Minutes); c != 0 {
		return c
	}
	if c := strings.Compare(a.Label, b.Label); c != 0 {
		return c
	}
	return strings.Compare(a.Key, b.Key)
}

func lengthUnstated(w domain.UsageWindow) int {
	if w.Minutes == 0 {
		return 1
	}
	return 0
}

// CurrentWindows returns the windows that have not reset at now: those with
// an unknown reset and those whose reset lies after now. The result is a
// new non-nil slice.
func CurrentWindows(windows []domain.UsageWindow, now time.Time) []domain.UsageWindow {
	current := make([]domain.UsageWindow, 0, len(windows))
	for _, w := range windows {
		if w.ResetsAt.IsZero() || w.ResetsAt.After(now) {
			current = append(current, w)
		}
	}
	return current
}
