package runstats

import (
	"cmp"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

// Span folds the contributions of one stretch of time into a [SpanReport]. It
// is not safe for concurrent use; a caller builds one per report.
type Span struct {
	caps        storage.RunHistoryCapabilities
	since       time.Time
	until       *time.Time
	generatedAt time.Time
	end         time.Time
	period      Period

	totals   bucketAccum
	sessions map[runIdentity]*sessionAccum

	stored        map[string]struct{}
	running       []RunningSession
	runningFolded bool
}

// sessionAccum folds the contributions of one run.
type sessionAccum struct {
	bucketAccum
	run       runIdentity
	untracked bool
	dispatch  string
	meta      SpanSession
}

// NewSpan returns a Span over [since, min(until, generatedAt)); a nil until
// is open. The window is exact and never aligned to a bucket. running is the
// snapshot taken before the stored turns are read, and the Span does not
// modify it.
func NewSpan(caps storage.RunHistoryCapabilities, since time.Time, until *time.Time, generatedAt time.Time, running []RunningSession) *Span {
	since = since.UTC()
	end := generatedAt.UTC()
	if until != nil && until.Before(end) {
		end = until.UTC()
	}
	return &Span{
		caps:        caps,
		since:       since,
		until:       until,
		generatedAt: generatedAt,
		end:         end,
		period:      SpendPeriodOf(Range{Since: &since, Until: until}, generatedAt),
		sessions:    make(map[runIdentity]*sessionAccum),
		stored:      make(map[string]struct{}),
		running:     running,
	}
}

// Window returns the half-open instants to scan stored turns and untracked
// runs over.
func (s *Span) Window() (since, until *time.Time) {
	return new(s.since), new(s.end)
}

// AddTurn folds one stored turn record into the span. It is the visit
// callback [storage.RunTurnStore.ScanRunTurnsRange] calls once per record; it
// calls no Store method and always returns nil.
func (s *Span) AddTurn(row storage.RunTurnRow) error {
	if row.DispatchID != "" {
		s.stored[row.DispatchID] = struct{}{}
	}
	s.fold(storedTurnContribution(s.caps, row), func() SpanSession {
		return SpanSession{
			Identifier:        row.Identifier,
			DisplayIdentifier: row.DisplayID,
			IssueTitle:        row.IssueTitle,
			RuleName:          row.RuleName,
			AgentKind:         row.AgentAdapter,
			Status:            row.Status,
		}
	}, row.DispatchID)
	return nil
}

// AddUntrackedRun folds one run recorded without turn records into the span.
// It is the visit callback [storage.RunTurnStore.ScanUntrackedRunsRange] calls
// once per run; it calls no Store method and always returns nil.
func (s *Span) AddUntrackedRun(row storage.UntrackedRunRow) error {
	s.fold(untrackedContribution(s.caps, row), func() SpanSession {
		return SpanSession{
			Identifier:        row.Identifier,
			DisplayIdentifier: row.DisplayID,
			IssueTitle:        row.IssueTitle,
			RuleName:          row.RuleName,
			AgentKind:         row.AgentAdapter,
			Status:            row.Status,
		}
	}, row.DispatchID)
	return nil
}

func (s *Span) fold(c contribution, describe func() SpanSession, dispatchID string) {
	if c.at.Before(s.since) || !c.at.Before(s.end) {
		return
	}
	s.totals.add(c, s.caps)
	session := s.sessions[c.run]
	if session == nil {
		session = &sessionAccum{run: c.run, untracked: c.untracked, dispatch: dispatchID, meta: describe()}
		s.sessions[c.run] = session
	}
	session.add(c, s.caps)
}

func (s *Span) foldRunningSessions() {
	foldRunning(s.running, s.stored, func(session RunningSession, turn storage.RunTurn) {
		s.fold(runningTurnContribution(s.caps, session, turn), func() SpanSession {
			return SpanSession{
				Identifier:        session.Identifier,
				DisplayIdentifier: session.DisplayID,
				IssueTitle:        session.IssueTitle,
				RuleName:          session.RuleName,
				AgentKind:         session.AgentKind,
				Status:            statusRunning,
			}
		}, session.DispatchID)
	})
}

// Report returns the span report. It folds the running sessions on its first
// call, which MUST follow every stored row.
func (s *Span) Report() SpanReport {
	if !s.runningFolded {
		s.runningFolded = true
		s.foldRunningSessions()
	}
	report := SpanReport{
		GeneratedAt: s.generatedAt.Format(time.RFC3339),
		Since:       s.since.Format(time.RFC3339),
		Period:      s.period,
		Totals:      s.totals.report(s.caps, s.since, s.until == nil || s.until.After(s.generatedAt)),
		Sessions:    s.sessionReports(),
	}
	if s.until != nil {
		report.Until = new(s.until.UTC().Format(time.RFC3339))
	}
	return report
}

// sessionReports lists each session once, ordered by cost share, token share,
// and turns, each descending with null last, then by dispatch ID and run
// identity. Sums run in run identity order so a share does not depend on map
// iteration.
func (s *Span) sessionReports() []SpanSession {
	accums := make([]*sessionAccum, 0, len(s.sessions))
	for _, session := range s.sessions {
		accums = append(accums, session)
	}
	slices.SortFunc(accums, func(a, b *sessionAccum) int { return compareRuns(a.run, b.run) })

	sessions := make([]SpanSession, len(accums))
	costs := make([]*float64, len(accums))
	var costSum float64
	var tokensSum int64
	for i, accum := range accums {
		bucket := accum.report(s.caps, s.since, false)
		session := accum.meta
		session.Tokens = bucket.Tokens
		session.CostUSD = bucket.CostUSD
		session.CostUnpriced = accum.runs[accum.run].unpriced
		if accum.dispatch != "" {
			session.DispatchID = new(accum.dispatch)
		}
		if !accum.untracked {
			session.Turns = new(accum.turns)
		}
		if bucket.CostUSD != nil {
			costs[i] = new(accum.costUSD)
			costSum += accum.costUSD
		}
		if bucket.Tokens != nil {
			tokensSum += bucket.Tokens.Uncached
		}
		sessions[i] = session
	}
	for i := range sessions {
		if costs[i] != nil && costSum != 0 {
			sessions[i].CostShare = new(*costs[i] / costSum)
		}
		if tokens := sessions[i].Tokens; tokens != nil && tokensSum != 0 {
			sessions[i].TokensShare = new(float64(tokens.Uncached) / float64(tokensSum))
		}
	}

	order := make([]int, len(sessions))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		a, b := sessions[i], sessions[j]
		return cmp.Or(
			compareDescNullsLast(a.CostShare, b.CostShare),
			compareDescNullsLast(a.TokensShare, b.TokensShare),
			compareDescNullsLast(a.Turns, b.Turns),
			cmp.Compare(accums[i].dispatch, accums[j].dispatch),
			compareRuns(accums[i].run, accums[j].run),
		)
	})
	ordered := make([]SpanSession, len(sessions))
	for i, index := range order {
		ordered[i] = sessions[index]
	}
	return ordered
}

// compareDescNullsLast orders larger values first and nil after every value.
func compareDescNullsLast[T cmp.Ordered](a, b *T) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*b, *a)
}
