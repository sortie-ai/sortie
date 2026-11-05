package runstats

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

// statusRunning is the status a running session's turns carry.
const statusRunning = "running"

// ruleBuckets is the most buckets the period rule lets a range list.
const ruleBuckets = 168

var periodRules = []struct {
	period Period
	length time.Duration
}{
	{PeriodFiveMinutes, 5 * time.Minute},
	{PeriodQuarterHour, 15 * time.Minute},
	{PeriodHour, time.Hour},
	{PeriodDay, 24 * time.Hour},
}

// SpendPeriodOf returns the period a spend series over r is bucketed by: the
// finest one that keeps the range to at most 168 buckets, and months for any
// longer range and for an open one. An Until after generatedAt counts in full.
func SpendPeriodOf(r Range, generatedAt time.Time) Period {
	if r.Since == nil {
		return PeriodMonth
	}
	end := generatedAt
	if r.Until != nil {
		end = *r.Until
	}
	span := end.Sub(*r.Since)
	for _, rule := range periodRules {
		if span <= ruleBuckets*rule.length {
			return rule.period
		}
	}
	return PeriodMonth
}

// CheckSpendPeriod returns an error when a series over r in period p cannot be
// served: r has no Since, or the series would list more than [MaxSpendBuckets]
// buckets. The error text reads after the parameter name. It computes the
// listing from the bounds alone and reads nothing.
func CheckSpendPeriod(r Range, p Period, generatedAt time.Time) error {
	if r.Since == nil {
		return errors.New("needs since")
	}
	count := listingLength(r, p, generatedAt)
	if count <= MaxSpendBuckets {
		return nil
	}
	msg := fmt.Sprintf("lists %d buckets for this range, over the limit of %d", count, MaxSpendBuckets)
	if finest, ok := finestFittingPeriod(r, generatedAt); ok {
		msg += "; the finest period that fits is " + string(finest)
	}
	return errors.New(msg)
}

// finestFittingPeriod returns the finest period whose series over r stays
// within [MaxSpendBuckets].
func finestFittingPeriod(r Range, generatedAt time.Time) (Period, bool) {
	for _, p := range []Period{PeriodFiveMinutes, PeriodQuarterHour, PeriodHour, PeriodDay, PeriodMonth} {
		if listingLength(r, p, generatedAt) <= MaxSpendBuckets {
			return p, true
		}
	}
	return "", false
}

func lengthOf(p Period) time.Duration {
	for _, rule := range periodRules {
		if rule.period == p {
			return rule.length
		}
	}
	return 0
}

// startOf returns the first instant of the period of p that holds t.
// Truncate counts from the zero time, which is 00:00 UTC, and every sub-day
// length divides a day.
func startOf(p Period, t time.Time) time.Time {
	t = t.UTC()
	if p == PeriodMonth {
		year, month, _ := t.Date()
		return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	}
	return t.Truncate(lengthOf(p))
}

// nextOf returns the start of the period after the one starting at start.
func nextOf(p Period, start time.Time) time.Time {
	if p == PeriodMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.Add(lengthOf(p))
}

// window returns the half-open instants a series in period p counts: a nil ws
// is open. A duration Since starts the window at the first boundary at or
// after it, so a preset range lists the same number of whole buckets at any
// clock.
func window(r Range, p Period, now time.Time) (ws *time.Time, we time.Time) {
	we = now.UTC()
	if r.Until != nil && r.Until.Before(we) {
		we = r.Until.UTC()
	}
	if r.Since == nil {
		return nil, we
	}
	since := r.Since.UTC()
	if r.SinceIsDuration {
		if start := startOf(p, since); !start.Equal(since) {
			since = nextOf(p, start)
		}
	}
	return &since, we
}

// listing returns the start of every bucket from the one holding the window's
// first instant through the one holding its last. An open window starts at the
// bucket of earliest, the first contribution, and lists nothing when none
// contributed.
func listing(ws *time.Time, we time.Time, p Period, earliest *time.Time) []time.Time {
	var first time.Time
	switch {
	case ws != nil:
		if !we.After(*ws) {
			return nil
		}
		first = startOf(p, *ws)
	case earliest != nil:
		first = startOf(p, *earliest)
	default:
		return nil
	}
	last := startOf(p, we.Add(-time.Nanosecond))
	var starts []time.Time
	for start := first; !start.After(last); start = nextOf(p, start) {
		starts = append(starts, start)
	}
	return starts
}

// listingLength counts the buckets listing returns for a bounded window
// without building them.
func listingLength(r Range, p Period, now time.Time) int64 {
	ws, we := window(r, p, now)
	if ws == nil || !we.After(*ws) {
		return 0
	}
	first, last := startOf(p, *ws), startOf(p, we.Add(-time.Nanosecond))
	if p == PeriodMonth {
		firstYear, firstMonth, _ := first.Date()
		lastYear, lastMonth, _ := last.Date()
		return int64(lastYear-firstYear)*12 + int64(lastMonth-firstMonth) + 1
	}
	return int64(last.Sub(first)/lengthOf(p)) + 1
}

// runIdentity tells runs apart across stored rows and running sessions.
type runIdentity struct {
	runID      int64
	dispatchID string // set for a running session only
}

func compareRuns(a, b runIdentity) int {
	return cmp.Or(cmp.Compare(a.runID, b.runID), cmp.Compare(a.dispatchID, b.dispatchID))
}

// contribution is one stored turn, running turn, or untracked run, reduced to
// what a bucket folds.
type contribution struct {
	run        runIdentity
	at         time.Time
	untracked  bool
	toolCalls  int
	errors     int
	measured   bool
	tokens     Tokens
	lowerBound bool
	cost       costOutcome
	kinds      KindCosts
}

// runFacts are the fields of a turn's run that classify its turns.
type runFacts struct {
	status, costStatus, completeness string
	cost, agentPriced                *float64
}

func tokensSpent(t Tokens) bool {
	return t.Input != 0 || t.Output != 0 || t.Total != 0 || t.CacheRead != 0 || t.CacheWrite != 0
}

// turnContribution classifies one turn record. The turn's agent-priced part is
// its cost times its run's agent-priced proportion, because no price source is
// stored per turn.
func turnContribution(caps storage.RunHistoryCapabilities, run runIdentity, facts runFacts, turn storage.RunTurn) contribution {
	c := contribution{
		run:        run,
		at:         turn.EndedAt,
		toolCalls:  turn.ToolCalls,
		errors:     turn.Errors,
		measured:   turn.TokensMeasured,
		lowerBound: facts.completeness == storage.UsageLowerBound,
	}
	if !turn.TokensMeasured {
		return c
	}
	for _, tokens := range turn.Models {
		c.tokens.Input += tokens.InputTokens
		c.tokens.Output += tokens.OutputTokens
		c.tokens.CacheRead += tokens.CacheReadTokens
		c.tokens.CacheWrite += tokens.CacheWriteTokens
	}
	c.tokens.Total = c.tokens.Input + c.tokens.Output
	if !caps.RecordedCost() {
		return c
	}
	c.cost = classifyCost(facts.status, tokensSpent(c.tokens), turn.EstimatedCostUSD, facts.costStatus)
	if c.cost.priced {
		c.kinds = KindCosts{
			FreshInputUSD: turn.InputCostUSD,
			OutputUSD:     turn.OutputCostUSD,
			CacheWriteUSD: turn.CacheWriteCostUSD,
			CacheReadUSD:  turn.CacheReadCostUSD,
		}
		c.cost.agentUSD, c.cost.agentPriced = agentShareOf(c.cost.cost, facts)
	}
	return c
}

func agentShareOf(cost float64, facts runFacts) (usd float64, ok bool) {
	if facts.agentPriced == nil {
		return 0, false
	}
	if facts.cost != nil && *facts.cost > 0 {
		return cost * min(1, *facts.agentPriced / *facts.cost), true
	}
	return 0, true
}

func untrackedContribution(caps storage.RunHistoryCapabilities, row storage.UntrackedRunRow) contribution {
	c := contribution{
		run:       runIdentity{runID: row.RunID},
		at:        row.CompletedAt,
		untracked: true,
		measured:  row.TokensMeasured,
		tokens: Tokens{
			Input:      row.InputTokens,
			Output:     row.OutputTokens,
			Total:      row.TotalTokens,
			CacheRead:  row.CacheReadTokens,
			CacheWrite: row.CacheWriteTokens,
		},
		lowerBound: row.UnaccountedTurns > 0,
	}
	if !row.TokensMeasured || !caps.RecordedCost() {
		return c
	}
	c.cost = classifyCost(row.Status, tokensSpent(c.tokens), row.EstimatedCostUSD, row.CostStatus)
	if c.cost.priced && row.AgentPricedUSD != nil {
		c.cost.agentUSD, c.cost.agentPriced = *row.AgentPricedUSD, true
	}
	return c
}

func storedTurnContribution(caps storage.RunHistoryCapabilities, row storage.RunTurnRow) contribution {
	return turnContribution(caps, runIdentity{runID: row.RunID}, runFacts{
		status:       row.Status,
		costStatus:   row.CostStatus,
		completeness: row.UsageCompleteness,
		cost:         row.RunCostUSD,
		agentPriced:  row.RunAgentPricedUSD,
	}, row.Turn)
}

func runningTurnContribution(caps storage.RunHistoryCapabilities, session RunningSession, turn storage.RunTurn) contribution {
	return turnContribution(caps, runIdentity{dispatchID: session.DispatchID}, runFacts{
		status:       statusRunning,
		costStatus:   session.CostStatus,
		completeness: session.UsageCompleteness,
		cost:         session.CostUSD,
		agentPriced:  session.AgentPricedUSD,
	}, turn)
}

// foldRunning calls fold for each turn of the running sessions that no stored
// row named, ascending by dispatch ID and then Seq. A session a stored row
// named has exited since the snapshot, and the store holds all its turns.
func foldRunning(running []RunningSession, stored map[string]struct{}, fold func(RunningSession, storage.RunTurn)) {
	byDispatch := func(a, b RunningSession) int { return cmp.Compare(a.DispatchID, b.DispatchID) }
	bySeq := func(a, b storage.RunTurn) int { return cmp.Compare(a.Seq, b.Seq) }
	for _, session := range slices.SortedStableFunc(slices.Values(running), byDispatch) {
		if _, exited := stored[session.DispatchID]; exited {
			continue
		}
		for _, turn := range slices.SortedStableFunc(slices.Values(session.Turns), bySeq) {
			fold(session, turn)
		}
	}
}

// runFlags records, for one run in one bucket, which classes its
// contributions fell into, so every run counts once per class.
type runFlags struct {
	unmeasured, unpriced, partial, lowerBound bool
}

// bucketAccum folds the contributions of one bucket, or of one session.
type bucketAccum struct {
	runs          map[runIdentity]*runFlags
	contributions int
	turns         int
	toolCalls     int
	errors        int
	unrecorded    int

	tokens   Tokens
	measured int

	costUSD         float64
	priced          int
	kinds           KindCosts
	untrackedPriced bool
	agentUSD        float64
	hasAgentPriced  bool
}

func (b *bucketAccum) add(c contribution, caps storage.RunHistoryCapabilities) {
	if b.runs == nil {
		b.runs = make(map[runIdentity]*runFlags)
	}
	flags := b.runs[c.run]
	if flags == nil {
		flags = &runFlags{}
		b.runs[c.run] = flags
	}
	b.contributions++
	if c.untracked {
		b.unrecorded++
	} else {
		b.turns++
		b.toolCalls += c.toolCalls
		b.errors += c.errors
	}
	if !caps.Full() {
		return
	}
	if !c.measured {
		flags.unmeasured = true
		return
	}
	b.tokens.Input += c.tokens.Input
	b.tokens.Output += c.tokens.Output
	b.tokens.Total += c.tokens.Total
	b.tokens.CacheRead += c.tokens.CacheRead
	b.tokens.CacheWrite += c.tokens.CacheWrite
	b.measured++
	flags.lowerBound = flags.lowerBound || c.lowerBound
	flags.unpriced = flags.unpriced || c.cost.unpriced
	flags.partial = flags.partial || c.cost.partial
	if !c.cost.priced {
		return
	}
	b.costUSD += c.cost.cost
	b.priced++
	if c.untracked {
		b.untrackedPriced = true
	} else {
		b.kinds.FreshInputUSD += c.kinds.FreshInputUSD
		b.kinds.OutputUSD += c.kinds.OutputUSD
		b.kinds.CacheWriteUSD += c.kinds.CacheWriteUSD
		b.kinds.CacheReadUSD += c.kinds.CacheReadUSD
	}
	if c.cost.agentPriced {
		b.agentUSD += c.cost.agentUSD
		b.hasAgentPriced = true
	}
}

// report returns the bucket starting at start. A bucket with no contribution
// reports zero members, so it never reads as unmeasured or unpriced.
func (b *bucketAccum) report(caps storage.RunHistoryCapabilities, start time.Time, inProgress bool) SpendBucket {
	out := SpendBucket{
		Start:               start.Format(time.RFC3339),
		Runs:                len(b.runs),
		Turns:               b.turns,
		ToolCalls:           b.toolCalls,
		Errors:              b.errors,
		TurnsUnrecordedRuns: b.unrecorded,
		InProgress:          inProgress,
	}
	if !caps.Full() {
		return out
	}
	var unmeasured, unpriced, partial, lowerBound int
	for _, flags := range b.runs {
		if flags.unmeasured {
			unmeasured++
		}
		if flags.unpriced {
			unpriced++
		}
		if flags.partial {
			partial++
		}
		if flags.lowerBound {
			lowerBound++
		}
	}
	if b.contributions == 0 || b.measured > 0 {
		out.Tokens = new(withDerived(b.tokens))
	}
	if caps.RecordedCost() && (b.contributions == 0 || b.priced > 0) {
		out.CostUSD = new(round9(b.costUSD))
		if !b.untrackedPriced {
			out.CostByKind = &KindCosts{
				FreshInputUSD: round9(b.kinds.FreshInputUSD),
				OutputUSD:     round9(b.kinds.OutputUSD),
				CacheWriteUSD: round9(b.kinds.CacheWriteUSD),
				CacheReadUSD:  round9(b.kinds.CacheReadUSD),
			}
		}
	}
	flags := lowerBoundFlagsOf(out.Tokens, out.CostUSD, unmeasured,
		&costAccum{
			costUnpricedRuns: unpriced,
			costPartialRuns:  partial,
			agentPricedUSD:   b.agentUSD,
			hasAgentPriced:   b.hasAgentPriced,
		},
		&provenanceAccum{lowerBoundRuns: lowerBound})
	out.TokensUnmeasuredRuns = unmeasured
	out.CostUnpricedRuns = unpriced
	out.CostPartialRuns = partial
	out.TokensLowerBoundRuns = flags.tokensLowerBoundRuns
	out.TokensLowerBound = flags.tokensLowerBound
	out.CostLowerBound = flags.costLowerBound
	out.AgentPricedUSD = flags.agentPricedUSD
	return out
}

// series folds contributions into the buckets of one spend window.
type series struct {
	caps        storage.RunHistoryCapabilities
	period      Period
	generatedAt time.Time
	ws          *time.Time
	we          time.Time

	buckets  map[time.Time]*bucketAccum
	earliest *time.Time

	stored        map[string]struct{}
	running       []RunningSession
	runningFolded bool
}

func newSeries(caps storage.RunHistoryCapabilities, r Range, p Period, generatedAt time.Time, running []RunningSession) *series {
	if p == "" {
		p = SpendPeriodOf(r, generatedAt)
	}
	ws, we := window(r, p, generatedAt)
	return &series{
		caps:        caps,
		period:      p,
		generatedAt: generatedAt,
		ws:          ws,
		we:          we,
		buckets:     make(map[time.Time]*bucketAccum),
		stored:      make(map[string]struct{}),
		running:     running,
	}
}

func (s *series) fold(c contribution) {
	if (s.ws != nil && c.at.Before(*s.ws)) || !c.at.Before(s.we) {
		return
	}
	start := startOf(s.period, c.at)
	bucket := s.buckets[start]
	if bucket == nil {
		bucket = &bucketAccum{}
		s.buckets[start] = bucket
	}
	bucket.add(c, s.caps)
	if s.earliest == nil || c.at.Before(*s.earliest) {
		s.earliest = new(c.at)
	}
}

func (s *series) addTurn(row storage.RunTurnRow) {
	if row.DispatchID != "" {
		s.stored[row.DispatchID] = struct{}{}
	}
	s.fold(storedTurnContribution(s.caps, row))
}

func (s *series) addUntrackedRun(row storage.UntrackedRunRow) {
	s.fold(untrackedContribution(s.caps, row))
}

// report lists every bucket of the window. It folds the running sessions on its
// first call, which MUST follow every stored row.
func (s *series) report() Spend {
	if !s.runningFolded {
		s.runningFolded = true
		foldRunning(s.running, s.stored, func(session RunningSession, turn storage.RunTurn) {
			s.fold(runningTurnContribution(s.caps, session, turn))
		})
	}
	out := Spend{Period: s.period, Buckets: []SpendBucket{}}
	var empty bucketAccum
	for _, start := range listing(s.ws, s.we, s.period, s.earliest) {
		bucket := s.buckets[start]
		if bucket == nil {
			bucket = &empty
		}
		inProgress := nextOf(s.period, start).After(s.generatedAt)
		out.Buckets = append(out.Buckets, bucket.report(s.caps, start, inProgress))
	}
	return out
}
