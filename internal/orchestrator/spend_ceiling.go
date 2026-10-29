package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
)

// SpendWindow names a calendar window of the instance spend ceiling.
type SpendWindow string

const (
	SpendWindowDaily   SpendWindow = "daily"
	SpendWindowWeekly  SpendWindow = "weekly"
	SpendWindowMonthly SpendWindow = "monthly"
)

// spendWindowDef holds every attribute of one spend window, so a window
// cannot be added without all of them.
type spendWindowDef struct {
	window  SpendWindow
	setting string
	reason  SkipReason
	ceiling func(config.AgentConfig) float64

	// bounds returns UTC instants, end exclusive.
	bounds func(now time.Time) (start, end time.Time)
}

// spendWindowDefs is the only place a window's attributes live, in ascending
// window length: the evaluation order, the order of State.SpendCeilings, and
// the tie-break order of the hold reason.
var spendWindowDefs = []spendWindowDef{
	{
		window:  SpendWindowDaily,
		setting: "agent.max_daily_cost_usd",
		reason:  SkipDailySpendCeiling,
		ceiling: func(a config.AgentConfig) float64 { return a.MaxDailyCostUSD },
		bounds:  dayBounds,
	},
	{
		window:  SpendWindowWeekly,
		setting: "agent.max_weekly_cost_usd",
		reason:  SkipWeeklySpendCeiling,
		ceiling: func(a config.AgentConfig) float64 { return a.MaxWeeklyCostUSD },
		bounds:  weekBounds,
	},
	{
		window:  SpendWindowMonthly,
		setting: "agent.max_monthly_cost_usd",
		reason:  SkipMonthlySpendCeiling,
		ceiling: func(a config.AgentConfig) float64 { return a.MaxMonthlyCostUSD },
		bounds:  monthBounds,
	},
}

// spendWindowDefOf panics for a window the table lacks: a lookup that
// returned a zero value such as SkipNone would release a hold silently.
func spendWindowDefOf(window SpendWindow) spendWindowDef {
	for _, def := range spendWindowDefs {
		if def.window == window {
			return def
		}
	}
	panic("orchestrator: no spend window definition for " + string(window))
}

// SpendWindows returns every spend window in table order.
func SpendWindows() []SpendWindow {
	windows := make([]SpendWindow, 0, len(spendWindowDefs))
	for _, def := range spendWindowDefs {
		windows = append(windows, def.window)
	}
	return windows
}

// Setting returns the front-matter key of the window's ceiling.
func (w SpendWindow) Setting() string {
	return spendWindowDefOf(w).setting
}

// SkipReason returns the reason the window holds dispatch under.
func (w SpendWindow) SkipReason() SkipReason {
	return spendWindowDefOf(w).reason
}

// spendCeilingsOf returns the ceiling of every window in agent.
func spendCeilingsOf(agent config.AgentConfig) map[SpendWindow]float64 {
	ceilings := make(map[SpendWindow]float64, len(spendWindowDefs))
	for _, def := range spendWindowDefs {
		ceilings[def.window] = def.ceiling(agent)
	}
	return ceilings
}

// isSpendCeilingReason reports whether r is the hold reason of a spend window.
func isSpendCeilingReason(r SkipReason) bool {
	for _, def := range spendWindowDefs {
		if def.reason == r {
			return true
		}
	}
	return false
}

// SpendCeilingStatus is the latest evaluation of one configured window.
type SpendCeilingStatus struct {
	Window SpendWindow

	// BudgetUSD is the setting the evaluation read; always above 0.
	BudgetUSD float64

	// UsedUSD is the window's estimated cost, a lower bound unless
	// UsedComplete.
	UsedUSD      float64
	UsedComplete bool

	// Holding is pricing.CeilingReached(UsedUSD, BudgetUSD).
	Holding bool

	// WindowStart and ResetsAt bound the window in UTC; ResetsAt is its
	// exclusive end.
	WindowStart time.Time
	ResetsAt    time.Time

	// HoldingSince is the UTC instant the hold began; zero unless Holding.
	HoldingSince time.Time

	// incompleteWarned is true once the lower-bound Warn ran for this
	// WindowStart.
	incompleteWarned bool
}

// spendStore is the read evaluateSpendCeiling needs.
type spendStore interface {
	UsageCompletedBetween(ctx context.Context, since, until time.Time) (persistence.IssueTokenUsage, error)
}

// spendCeilingParams carries the inputs of one evaluateSpendCeiling call.
type spendCeilingParams struct {
	// Ctx nil reads context.Background().
	Ctx   context.Context
	Store spendStore

	// Ceilings is the setting of each window; a window absent or at 0 is off.
	Ceilings map[SpendWindow]float64
	Rates    *pricing.Rates
	Now      time.Time

	// Logger is unscoped so the records carry no issue fields; nil reads
	// slog.Default().
	Logger *slog.Logger
}

// spendWindowBounds returns the UTC calendar window of now: start inclusive,
// end exclusive. end is the reset time.
func spendWindowBounds(window SpendWindow, now time.Time) (start, end time.Time) {
	return spendWindowDefOf(window).bounds(now)
}

func dayBounds(now time.Time) (start, end time.Time) {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// weekBounds derives the Monday-based week from the UTC date alone, because
// the ISO year of a date differs from its calendar year around 1 January.
func weekBounds(now time.Time) (start, end time.Time) {
	y, m, d := now.UTC().Date()
	back := (int(now.UTC().Weekday()) + 6) % 7
	return time.Date(y, m, d-back, 0, 0, 0, 0, time.UTC), time.Date(y, m, d-back+7, 0, 0, 0, 0, time.UTC)
}

func monthBounds(now time.Time) (start, end time.Time) {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC), time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}

// windowUsage folds the running sessions into the usage of the window's
// finished runs. A session that has reported nothing yet adds no cost and
// keeps the spend complete; a session that can carry no token figure makes it
// a lower bound.
func windowUsage(rows persistence.IssueTokenUsage, running map[string]*RunningEntry, rates *pricing.Rates) persistence.IssueTokenUsage {
	u := rows
	for _, entry := range running {
		u.UnaccountedTurns += entry.UnaccountedTurns
		if !entry.CarriesTokenFigure {
			u.UnmeasuredSessions++
			continue
		}
		if !entry.UsageMeasured {
			continue
		}
		s := entry.sessionCost(rates)
		switch s.Status {
		case pricing.CostEstimated, pricing.CostPartial:
			if s.USD != nil {
				u.RecordedCostUSD += *s.USD
			}
			if s.Spent {
				u.PricedSessions++
			}
			if s.Status == pricing.CostPartial {
				u.PartialSessions++
			}
		case pricing.CostUnpriced:
			if s.Spent {
				u.UnpricedSessions++
			}
		}
	}
	return u
}

func findSpendStatus(statuses []SpendCeilingStatus, window SpendWindow) (SpendCeilingStatus, bool) {
	for _, s := range statuses {
		if s.Window == window {
			return s, true
		}
	}
	return SpendCeilingStatus{}, false
}

// holdsSpendWindow reports whether statuses holds window at start.
func holdsSpendWindow(statuses []SpendCeilingStatus, window SpendWindow, start time.Time) bool {
	s, ok := findSpendStatus(statuses, window)
	return ok && s.Holding && s.WindowStart.Equal(start)
}

// evaluateSpendCeiling refreshes state.SpendCeilings from one read per
// configured window and returns the skip reason of the holding window whose
// reset is latest, since dispatch resumes only once every holding window
// clears; on equal resets the later table row wins. It reads nothing while no
// window's setting is above 0. Must run on the single-writer event loop.
func evaluateSpendCeiling(state *State, p spendCeilingParams) SkipReason {
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}

	prev := state.SpendCeilings
	var next []SpendCeilingStatus
	var usages []persistence.IssueTokenUsage

	for _, def := range spendWindowDefs {
		ceiling := p.Ceilings[def.window]
		if ceiling <= 0 {
			continue
		}
		start, end := spendWindowBounds(def.window, p.Now)
		old, had := findSpendStatus(prev, def.window)
		same := had && old.WindowStart.Equal(start)

		rows, err := p.Store.UsageCompletedBetween(ctx, start, end)
		if err != nil {
			log.Warn("spend ceiling check failed, keeping the last result",
				slog.String("window", string(def.window)),
				slog.Any("error", err),
			)
			status := SpendCeilingStatus{Window: def.window, BudgetUSD: ceiling, WindowStart: start, ResetsAt: end}
			if same {
				status = old
			}
			next = append(next, status)
			usages = append(usages, persistence.IssueTokenUsage{})
			continue
		}

		u := windowUsage(rows, state.Running, p.Rates)
		used, costStatus := pricing.IssueCost(recordedOf(u), pricing.SessionCost{}, !u.SpendIncomplete())
		status := SpendCeilingStatus{
			Window:       def.window,
			BudgetUSD:    ceiling,
			UsedUSD:      used,
			UsedComplete: costStatus == pricing.CostEstimated,
			Holding:      pricing.CeilingReached(used, ceiling),
			WindowStart:  start,
			ResetsAt:     end,
		}
		switch {
		case status.Holding && same && old.Holding:
			status.HoldingSince = old.HoldingSince
		case status.Holding:
			status.HoldingSince = p.Now.UTC()
		case !status.UsedComplete:
			status.incompleteWarned = same && old.incompleteWarned
		}
		next = append(next, status)
		usages = append(usages, u)
	}
	state.SpendCeilings = next

	reason := SkipNone
	var best SpendCeilingStatus
	for _, s := range next {
		if s.Holding && (reason == SkipNone || !s.ResetsAt.Before(best.ResetsAt)) {
			best = s
			reason = s.Window.SkipReason()
		}
	}

	for _, e := range prev {
		if !e.Holding || holdsSpendWindow(next, e.Window, e.WindowStart) {
			continue
		}
		attrs := []any{slog.String("window", string(e.Window)), slog.Float64("budget_usd", 0)}
		if cur, ok := findSpendStatus(next, e.Window); ok {
			attrs = []any{
				slog.String("window", string(e.Window)),
				slog.Float64("budget_usd", cur.BudgetUSD),
				slog.Float64("used_usd", cur.UsedUSD),
			}
		}
		attrs = append(attrs,
			slog.String("ceiling_setting", e.Window.Setting()),
			slog.Bool("dispatch_held", reason != SkipNone),
		)
		log.Info("spend ceiling hold ended", attrs...)
	}

	for i := range next {
		s := &next[i]
		switch {
		case s.Holding:
			if !holdsSpendWindow(prev, s.Window, s.WindowStart) {
				log.Warn("spend ceiling reached, holding dispatch",
					slog.String("window", string(s.Window)),
					slog.Float64("used_usd", s.UsedUSD),
					slog.Bool("used_usd_complete", s.UsedComplete),
					slog.Float64("budget_usd", s.BudgetUSD),
					slog.String("ceiling_setting", s.Window.Setting()),
					slog.Time("resets_at", s.ResetsAt.UTC()),
				)
			}
		case !s.UsedComplete && !s.incompleteWarned:
			s.incompleteWarned = true
			u := usages[i]
			log.Warn("spend ceiling cannot be fully evaluated, allowing dispatch",
				slog.String("window", string(s.Window)),
				slog.Float64("used_usd", s.UsedUSD),
				slog.Float64("budget_usd", s.BudgetUSD),
				slog.Int("unmeasured_sessions", u.UnmeasuredSessions),
				slog.Int("unaccounted_turns", u.UnaccountedTurns),
				slog.Int("partial_sessions", u.PartialSessions),
				slog.Int("unpriced_sessions", u.UnpricedSessions),
			)
		}
	}
	return reason
}
