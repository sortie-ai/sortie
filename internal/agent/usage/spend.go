package usage

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// SpendShape is how a source's reports count spend.
type SpendShape int

const (
	// SpendUndeclared leaves the source's spend unread.
	SpendUndeclared SpendShape = iota
	// SpendProcessCumulative is a running total since the runtime's process
	// started.
	SpendProcessCumulative
	// SpendPerRequest is one model request's own counts, with its identity.
	SpendPerRequest
	// SpendAdapterMapped is token counts the kind's adapter reads and maps
	// itself; the ledger refuses it.
	SpendAdapterMapped
	// SpendPerTurn is one turn's own counts: a delta over the model requests
	// of that turn and of no other.
	SpendPerTurn
)

// MaxRecordsBytes is the largest records document a reader accepts.
const MaxRecordsBytes = 64 << 20

// SpendDeclaration is a source's declaration of what its spend counts
// mean.
type SpendDeclaration struct {
	// Shape is how the source counts spend.
	Shape SpendShape

	// CacheInInput declares that cache reads and writes are inside the
	// input count.
	CacheInInput bool

	// ReasoningInOutput declares that reasoning tokens are inside the
	// output count.
	ReasoningInOutput bool

	// ServedModel declares that each reading names the model that served
	// its request.
	ServedModel bool

	// DuringTurn declares that the source reports after each model request
	// while the turn runs.
	DuringTurn bool

	// LowerBound declares that a figure the source settles is never
	// complete: the runtime may spend more than it reports.
	LowerBound bool

	// VendorUnit is the plural unit name of a report that carries no token
	// counts.
	VendorUnit string
}

// SpendReading is one report of spend as a parser read it. A nil count is
// absent, which is unknown, never zero.
type SpendReading struct {
	Input      *int64
	Output     *int64
	CacheRead  *int64
	CacheWrite *int64

	// Reasoning is the reasoning count a reading reports apart from Output.
	// It is read only under a declaration whose reasoning sits outside
	// output; nil there is unknown, never zero.
	Reasoning *int64

	// Model is the model that served the reading; empty when it names none.
	Model string

	// Identity is the request's identity; required for SpendPerRequest.
	Identity string
}

// TurnEnd is how a turn ended, as the transport knows it.
type TurnEnd struct {
	// Replied is true when a prompt result with a stop reason arrived.
	Replied bool

	// Cancelled is true when that result's stop reason is cancelled.
	Cancelled bool

	// ModelReached is true when the turn reached the model.
	ModelReached bool

	// Children is true when the turn showed a child session: a session the
	// runtime ran part of the turn's work in, whose requests the live source
	// does not count.
	Children bool

	// ChildrenSettled is true when every child session the turn showed
	// completed and named its identifier.
	ChildrenSettled bool
}

// RecordsRead is the outcome of reading a records source.
type RecordsRead struct {
	Readings []SpendReading

	// OK is true when the read completed; false when not made, failed, or
	// timed out.
	OK bool

	// ChildReadings are the records of the child sessions the turn named,
	// each read from that session's own records, in session order.
	// ChildrenOK is true when every one of those reads completed.
	ChildReadings []SpendReading
	ChildrenOK    bool
}

// Settlement is the ledger's account of one turn.
type Settlement struct {
	// Run is the run-cumulative usage after this turn.
	Run domain.TokenUsage

	// ByModel is the run-cumulative per-model split, a copy; nil when none.
	ByModel map[string]domain.TokenUsage

	// Model is the model of the largest share this settlement split; empty
	// when none.
	Model string

	// Figure is true when the turn produced a figure.
	Figure bool

	// Spent is true when the figure added input or output tokens.
	Spent bool

	// Complete is true when the figure covers the whole turn.
	Complete bool

	// CoversSession is true when the figure holds every earlier turn of the
	// session.
	CoversSession bool

	// Provenance is the settling source's Category and Strategy; zero when
	// Figure is false.
	Provenance domain.UsageProvenance
}

// ErrSpendUnsupported marks a spend declaration the ledger does not
// implement.
var ErrSpendUnsupported = errors.New("spend declaration unsupported")

// Ledger counts one session's spend from a live source and an optional
// records source, each request once. It is not safe for concurrent use.
type Ledger struct {
	live    SpendDeclaration
	records SpendDeclaration
	hasRecs bool

	liveProvenance    domain.UsageProvenance
	recordsProvenance domain.UsageProvenance

	run     domain.TokenUsage
	byModel map[string]domain.TokenUsage

	lastLive domain.TokenUsage
	covered  domain.TokenUsage
	split    domain.TokenUsage

	boundaryKnown bool
	boundary      map[string]struct{}
	base          domain.TokenUsage

	childSeen map[string]struct{}
}

// NewLedger returns a ledger for a live source of SpendProcessCumulative or
// SpendPerTurn shape and, when records is not the zero Source, a records
// source of SpendPerRequest shape. Either source may place cache inside or
// outside input and reasoning inside or outside output; any other
// declaration, SpendAdapterMapped included, returns an error wrapping
// ErrSpendUnsupported.
func NewLedger(live, records Source) (*Ledger, error) {
	if !declaresLive(live) {
		return nil, fmt.Errorf("live source: %w", ErrSpendUnsupported)
	}
	l := &Ledger{live: live.Spend, liveProvenance: provenanceOf(live)}
	if reflect.ValueOf(records).IsZero() {
		return l, nil
	}
	if !declaresRecords(records) {
		return nil, fmt.Errorf("records source: %w", ErrSpendUnsupported)
	}
	l.records, l.hasRecs, l.recordsProvenance = records.Spend, true, provenanceOf(records)
	return l, nil
}

func provenanceOf(src Source) domain.UsageProvenance {
	return domain.UsageProvenance{Source: src.Category, Strategy: src.Strategy}
}

func declaresLive(src Source) bool {
	return src.Strategy == StrategyLive &&
		(src.Spend.Shape == SpendProcessCumulative || src.Spend.Shape == SpendPerTurn)
}

func declaresRecords(src Source) bool {
	return src.Strategy == StrategyRecords && src.Spend.Shape == SpendPerRequest
}

// Boundary fixes the identities already present in a records read as
// history when the boundary is unknown and the read completed. It counts
// nothing.
func (l *Ledger) Boundary(read RecordsRead) {
	if l.boundaryKnown || !read.OK {
		return
	}
	l.boundary = make(map[string]struct{}, len(read.Readings))
	for _, r := range read.Readings {
		if r.Identity != "" {
			l.boundary[r.Identity] = struct{}{}
		}
	}
	l.boundaryKnown = true
	l.base = l.covered
	l.split = l.covered
}

// SessionCreated records that the runtime created the session the ledger
// counts, so no history exists: an unknown boundary becomes known and empty
// without a read. It is called once, before the first Settle.
func (l *Ledger) SessionCreated() {
	l.Boundary(RecordsRead{OK: true})
}

// NeedsRecords reports whether Settle needs a records read for a turn that
// ended as end with the live reading live. It changes no state.
func (l *Ledger) NeedsRecords(end TurnEnd, live *SpendReading) bool {
	if !l.hasRecs {
		return false
	}
	if end.Children || (end.Cancelled && end.ModelReached) {
		return true
	}
	if !l.evalLive(end, live).settles {
		return true
	}
	return !l.boundaryKnown || (!l.live.ServedModel && l.records.ServedModel)
}

// Settle accounts one turn from its live reading and records read. It
// never lowers the run total or a per-model entry. When the turn showed
// child sessions, the records of each child read count once per identity
// and the turn is complete only if every child was settled and read.
func (l *Ledger) Settle(end TurnEnd, live *SpendReading, read RecordsRead) Settlement {
	s, added := l.settleSession(end, live, read)
	if end.Children {
		s, added = l.countChildren(s, added, end, read)
	}
	return l.settlement(s, added)
}

// settleSession settles the session's own spend and returns the shares it
// added to the per-model split.
func (l *Ledger) settleSession(end TurnEnd, live *SpendReading, read RecordsRead) (Settlement, map[string]domain.TokenUsage) {
	arm := l.evalLive(end, live)
	// A cancelled reply's live reading and the records cover the same
	// requests, so the records replace the reading instead of adding to it.
	if end.Cancelled && end.ModelReached && arm.normalized && !arm.reset {
		if recs := l.recordsView(read); recs.usable && l.hasUncounted(recs) {
			l.lastLive = arm.reading
			return l.settleRecords(end, recs)
		}
	}
	if arm.normalized {
		l.lastLive = arm.reading
		l.covered = arm.covered
		if arm.reset {
			l.boundaryKnown = false
		}
	}
	recs := l.recordsView(read)
	switch {
	case arm.settles:
		return l.settleLive(end, live, read, arm, recs)
	case recs.usable:
		return l.settleRecords(end, recs)
	}
	return Settlement{}, nil
}

func (l *Ledger) settleLive(end TurnEnd, live *SpendReading, read RecordsRead, arm liveEval, recs recordsView) (Settlement, map[string]domain.TokenUsage) {
	l.run = Add(l.run, arm.delta)
	complete := end.Replied && !end.Cancelled && !arm.reset && !arm.floored

	var added map[string]domain.TokenUsage
	from, fromOK := recs.at(l.split)
	to, toOK := recs.at(l.covered)
	if model := strings.TrimSpace(live.Model); recs.usable && fromOK && toOK && from <= to {
		added = recs.byModel(from, to)
		l.split = l.covered
	} else if model != "" {
		added = map[string]domain.TokenUsage{model: arm.delta}
		l.split = l.covered
	}
	l.addByModel(added)

	if complete {
		l.Boundary(read)
	}
	return Settlement{
		Figure:        true,
		Spent:         arm.delta.InputTokens+arm.delta.OutputTokens > 0,
		Complete:      complete && !l.live.LowerBound,
		CoversSession: complete && !l.live.LowerBound && l.live.Shape != SpendPerTurn,
		Provenance:    l.liveProvenance,
	}, added
}

// hasUncounted reports whether the known records hold input or output beyond
// what the ledger already covers.
func (l *Ledger) hasUncounted(recs recordsView) bool {
	amount := Subtract(recs.positions[len(recs.known)], l.covered)
	return amount.InputTokens != 0 || amount.OutputTokens != 0
}

func (l *Ledger) settleRecords(end TurnEnd, recs recordsView) (Settlement, map[string]domain.TokenUsage) {
	top := recs.positions[len(recs.known)]
	amount := Subtract(top, l.covered)
	if !l.hasUncounted(recs) {
		return Settlement{}, nil
	}
	_, exact := recs.at(l.covered)
	exact = exact && len(recs.known) == recs.all
	l.run = Add(l.run, amount)
	l.covered = Add(l.covered, amount)

	var added map[string]domain.TokenUsage
	if from, ok := recs.at(l.split); ok {
		added = recs.byModel(from, len(recs.known))
		l.split = top
	}
	l.addByModel(added)
	return Settlement{
		Figure:     true,
		Spent:      true,
		Complete:   end.Replied && !end.Cancelled && exact && !l.live.LowerBound,
		Provenance: l.recordsProvenance,
	}, added
}

// countChildren adds the records of the turn's child sessions to s and
// added. Child records move neither the live position nor the records
// alignment, so the session's own readings and records stay one for one.
func (l *Ledger) countChildren(s Settlement, added map[string]domain.TokenUsage, end TurnEnd, read RecordsRead) (Settlement, map[string]domain.TokenUsage) {
	short := !end.ChildrenSettled || !read.ChildrenOK
	var total domain.TokenUsage
	var shares map[string]domain.TokenUsage
	counted := false
	for _, r := range read.ChildReadings {
		u, ok := Normalize(r, l.records)
		if !ok {
			short = true
			continue
		}
		if _, repeat := l.childSeen[r.Identity]; repeat {
			continue
		}
		if l.childSeen == nil {
			l.childSeen = make(map[string]struct{})
		}
		l.childSeen[r.Identity] = struct{}{}
		counted = true
		total = Add(total, u)
		if model := strings.TrimSpace(r.Model); model != "" {
			shares = AddByModel(shares, map[string]domain.TokenUsage{model: u})
		}
	}
	if counted {
		l.run = Add(l.run, total)
		l.addByModel(shares)
		added = AddByModel(added, shares)
		if !s.Figure {
			s.Provenance = l.recordsProvenance
		}
		s.Figure, s.Spent = true, true
	}
	if short {
		s.Complete, s.CoversSession = false, false
	}
	return s, added
}

func (l *Ledger) addByModel(added map[string]domain.TokenUsage) {
	if len(added) > 0 {
		l.byModel = AddByModel(l.byModel, added)
	}
}

// settlement completes s with the ledger's current totals and the model of
// the largest share in added.
func (l *Ledger) settlement(s Settlement, added map[string]domain.TokenUsage) Settlement {
	s.Run = l.run
	s.ByModel = maps.Clone(l.byModel)
	var best int64
	for name, u := range added {
		if u.TotalTokens > best || (u.TotalTokens == best && s.Model != "" && name < s.Model) {
			best, s.Model = u.TotalTokens, name
		}
	}
	return s
}

// liveEval is the evaluation of one live reading against the ledger.
type liveEval struct {
	normalized bool // the reading is usable; the ledger state follows it
	settles    bool // the reading also settles the turn
	reading    domain.TokenUsage
	covered    domain.TokenUsage
	delta      domain.TokenUsage
	reset      bool
	floored    bool
}

// evalLive evaluates live without changing the ledger.
func (l *Ledger) evalLive(end TurnEnd, live *SpendReading) liveEval {
	if live == nil {
		return liveEval{}
	}
	reading, ok := Normalize(*live, l.live)
	if !ok {
		return liveEval{}
	}
	arm := liveEval{normalized: true, reading: reading}
	switch {
	case l.live.Shape == SpendPerTurn:
		arm.delta, arm.covered = reading, Add(l.covered, reading)
	case FellBelow(reading, l.lastLive):
		arm.reset = true
		arm.delta, arm.covered = reading, reading
	default:
		arm.delta = Subtract(reading, l.covered)
		arm.floored = FellBelow(reading, l.covered)
		arm.covered = Add(l.covered, arm.delta)
	}
	addedNothing := arm.delta.InputTokens == 0 && arm.delta.OutputTokens == 0
	arm.settles = !end.ModelReached || !addedNothing
	return arm
}

type knownRecord struct {
	usage domain.TokenUsage
	model string
}

// recordsView is the part of a records read that counts.
type recordsView struct {
	usable    bool
	known     []knownRecord
	all       int                 // readings left after dropping history and repeats
	positions []domain.TokenUsage // positions[k] is the base plus the first k known records
}

func (l *Ledger) recordsView(read RecordsRead) recordsView {
	if !l.hasRecs || !read.OK || !l.boundaryKnown {
		return recordsView{}
	}
	v := recordsView{usable: true, positions: []domain.TokenUsage{l.base}}
	seen := make(map[string]struct{}, len(read.Readings))
	stillKnown := true
	for i := range read.Readings {
		r := &read.Readings[i]
		if _, inside := l.boundary[r.Identity]; inside {
			continue
		}
		if _, repeat := seen[r.Identity]; repeat {
			continue
		}
		seen[r.Identity] = struct{}{}
		v.all++
		if !stillKnown {
			continue
		}
		u, ok := Normalize(*r, l.records)
		if !ok {
			stillKnown = false
			continue
		}
		v.known = append(v.known, knownRecord{usage: u, model: strings.TrimSpace(r.Model)})
		v.positions = append(v.positions, Add(v.positions[len(v.positions)-1], u))
	}
	return v
}

// at returns the number of known records whose position is x in every
// count. Positions are unique because every known record adds input or
// output.
func (v recordsView) at(x domain.TokenUsage) (int, bool) {
	for k, p := range v.positions {
		if p.InputTokens == x.InputTokens && p.OutputTokens == x.OutputTokens &&
			p.CacheReadTokens == x.CacheReadTokens && p.CacheWriteTokens == x.CacheWriteTokens {
			return k, true
		}
	}
	return 0, false
}

// byModel sums known[from:to] per served model, leaving out records that
// name none.
func (v recordsView) byModel(from, to int) map[string]domain.TokenUsage {
	var sum map[string]domain.TokenUsage
	for _, r := range v.known[from:to] {
		if r.model == "" {
			continue
		}
		if sum == nil {
			sum = make(map[string]domain.TokenUsage)
		}
		sum[r.model] = Add(sum[r.model], r.usage)
	}
	return sum
}

// Normalize converts a reading to counts under spend, reporting false when
// any count the declaration maps is missing or negative, or the reading
// contradicts the declaration. Input counts every input token and output
// every output token whatever the source's placement; Total is their sum.
func Normalize(reading SpendReading, spend SpendDeclaration) (domain.TokenUsage, bool) {
	if reading.Input == nil || reading.Output == nil ||
		reading.CacheRead == nil || reading.CacheWrite == nil {
		return domain.TokenUsage{}, false
	}
	in, out, cr, cw := *reading.Input, *reading.Output, *reading.CacheRead, *reading.CacheWrite
	if in < 0 || out < 0 || cr < 0 || cw < 0 {
		return domain.TokenUsage{}, false
	}
	if !spend.ReasoningInOutput {
		if reading.Reasoning == nil || *reading.Reasoning < 0 {
			return domain.TokenUsage{}, false
		}
		out += *reading.Reasoning
	}
	if spend.CacheInInput {
		if cr+cw > in {
			return domain.TokenUsage{}, false
		}
	} else {
		in += cr + cw
	}
	if spend.Shape == SpendPerRequest && (reading.Identity == "" || (in == 0 && out == 0)) {
		return domain.TokenUsage{}, false
	}
	return domain.TokenUsage{
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  cr,
		CacheWriteTokens: cw,
		TotalTokens:      in + out,
	}, true
}
