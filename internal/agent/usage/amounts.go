package usage

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// AmountShape is how a source counts an amount.
type AmountShape int

const (
	// AmountUndeclared leaves the amount unread.
	AmountUndeclared AmountShape = iota
	// AmountPerRequest is one model request's own amount; records sources only.
	AmountPerRequest
	// AmountPerTurn is one turn's own amount.
	AmountPerTurn
	// AmountSessionCumulative is a running total over the runtime's session,
	// across processes and resumes.
	AmountSessionCumulative
)

// AmountDeclaration is one amount a source reports beside or instead of token
// counts.
type AmountDeclaration struct {
	// Unit is domain.UnitUSD or a vendor unit's plural display name; empty
	// exactly when Currency is set.
	Unit string

	// Currency declares that each reading names its ISO 4217 code, which is
	// its unit.
	Currency bool

	// Shape is how the source counts the amount.
	Shape AmountShape
}

// AmountReading is one amount as a parser read it.
type AmountReading struct {
	// Unit is read only under a Currency declaration.
	Unit string

	// Amount is nil when the member is absent, null, not a JSON number, not
	// finite, or negative.
	Amount *float64
}

// amountState is what a ledger counted for one unit and served model.
type amountState struct {
	total      float64 // counted from every source
	counted    bool    // an amount, zero included, was counted
	lowerBound bool    // the run may have spent more than total

	previous    float64 // the last reading of a session-cumulative source
	hasPrevious bool
	history     float64 // what the session held before the run, per previous
}

// amountHistory is the amount a session held before the run, as the boundary
// fixed it.
type amountHistory struct {
	fixed   bool                  // a boundary or a created session fixed it before the first Settle
	created bool                  // the session was created by this run, so every unit's history is 0
	byKey   map[amountKey]float64 // the history records' amounts
	unknown map[string]struct{}   // units a history record left unread
}

// ObserveAmounts holds readings of the live source's declared amounts that
// reach the adapter outside a token reading; the next Settle counts and
// clears them, whichever turn they arrived in. It is not safe for concurrent
// use, like every Ledger method.
func (l *Ledger) ObserveAmounts(readings []AmountReading) {
	l.held = append(l.held, readings...)
}

// carriedAmount is an amount reading with what the zero rule needs of the
// reading that carries it.
type carriedAmount struct {
	AmountReading
	model    string // the carrying reading's served model; empty when it names none
	ownSpent bool   // the carrying reading's own input or output is above 0
	observed bool   // ObserveAmounts held it outside a token reading
}

// amountPass is what one Settle added in tokens and counted in amounts.
type amountPass struct {
	added   map[string]domain.TokenUsage
	spent   bool
	counted map[amountKey]struct{}
}

// spentFor reports whether tokens were spent for the zero rule: a running or
// observed amount stands for the settlement's tokens of its model, any other
// for its own reading's.
func (p *amountPass) spentFor(c carriedAmount, d AmountDeclaration) bool {
	switch {
	case d.Shape != AmountSessionCumulative && !c.observed:
		return c.ownSpent
	case c.model == "":
		return p.spent
	}
	return hasSpend(p.added[c.model])
}

func hasSpend(u domain.TokenUsage) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0
}

func (a *amountState) appears(unit string) bool {
	return a.total > 0 || (a.counted && unit != domain.UnitUSD)
}

func (l *Ledger) amountState(key amountKey) *amountState {
	if l.amounts == nil {
		l.amounts = make(map[amountKey]*amountState)
	}
	st := l.amounts[key]
	if st == nil {
		st = &amountState{}
		l.amounts[key] = st
	}
	return st
}

// settleAmounts counts the turn's amounts and marks the keys the turn left
// short. tokens is what the settlement added to the run.
func (l *Ledger) settleAmounts(end TurnEnd, live *SpendReading, s Settlement, added map[string]domain.TokenUsage, tokens domain.TokenUsage) {
	pass := &amountPass{added: added, spent: hasSpend(tokens), counted: make(map[amountKey]struct{})}
	// Records count first: a running total subtracts what they counted, while
	// a record counted after it would count the same request twice.
	l.countAmountGroup(l.records.Amounts, l.recordAmounts(), pass)
	l.countAmountGroup(l.live.Amounts, l.liveAmounts(live, s), pass)
	l.countAmountGroup(l.live.Amounts, l.observedAmounts(), pass)
	l.markAmountGaps(end, tokens, pass)
}

// markAmountGaps marks every key whose amount the turn left short. A mark
// lasts the rest of the session.
func (l *Ledger) markAmountGaps(end TurnEnd, tokens domain.TokenUsage, pass *amountPass) {
	attributed := Sum(pass.added)
	unattributed := tokens.InputTokens+tokens.OutputTokens > attributed.InputTokens+attributed.OutputTokens
	reachedModel := !l.countsTokens() && end.ModelReached
	units := l.amountUnits()
	for _, unit := range units {
		counted := pass.unitCounted(unit)
		switch {
		case l.namesModel(unit):
			for model, u := range pass.added {
				if _, ok := pass.counted[amountKey{unit, model}]; hasSpend(u) && !ok {
					l.amountState(amountKey{unit, model}).lowerBound = true
				}
			}
			if unattributed {
				l.markUnitForever(unit)
			}
		case pass.spent && !counted:
			l.markUnit(unit)
		}
		if reachedModel && !counted {
			l.markUnit(unit)
		}
	}
	for model := range l.childShares {
		for _, unit := range units {
			if _, declared := declarationOf(l.records.Amounts, unit); declared {
				l.amountState(amountKey{unit, model}).lowerBound = true
			}
		}
	}
}

// amountUnits returns every unit a source declares or the session has seen,
// sorted.
func (l *Ledger) amountUnits() []string {
	set := make(map[string]struct{})
	for _, decls := range [][]AmountDeclaration{l.live.Amounts, l.records.Amounts} {
		for _, d := range decls {
			if !d.Currency {
				set[d.Unit] = struct{}{}
			}
		}
	}
	for key := range l.amounts {
		set[key.unit] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// namesModel reports whether every source that declares unit reads it with
// the model that served the request.
func (l *Ledger) namesModel(unit string) bool {
	for _, src := range []SpendDeclaration{l.live, l.records} {
		if _, declared := declarationOf(src.Amounts, unit); declared && !src.ServedModel {
			return false
		}
	}
	return true
}

func (l *Ledger) markUnit(unit string) {
	for key, st := range l.amounts {
		if key.unit == unit {
			st.lowerBound = true
		}
	}
}

// markUnitForever marks every key of unit, those not yet seen included.
func (l *Ledger) markUnitForever(unit string) {
	if l.amountUnitsForever == nil {
		l.amountUnitsForever = make(map[string]struct{})
	}
	l.amountUnitsForever[unit] = struct{}{}
	l.markUnit(unit)
}

func (p *amountPass) unitCounted(unit string) bool {
	for key := range p.counted {
		if key.unit == unit {
			return true
		}
	}
	return false
}

func (l *Ledger) countAmountGroup(decls []AmountDeclaration, group []carriedAmount, pass *amountPass) {
	for _, c := range group {
		unit, ok := declaredUnit(decls, c.AmountReading)
		if !ok {
			continue
		}
		d, _ := declarationOf(decls, unit)
		key := amountKey{unit, c.model}
		st := l.amountState(key)
		if c.Amount == nil {
			st.lowerBound = true
			continue
		}
		amount := *c.Amount
		if d.Shape == AmountSessionCumulative {
			amount = l.increase(key, amount)
		}
		if amount == 0 && unit == domain.UnitUSD && pass.spentFor(c, d) {
			st.lowerBound = true
			continue
		}
		st.total += amount
		st.counted = true
		pass.counted[key] = struct{}{}
	}
}

// increase returns what a running reading adds to the amount already counted
// for key from any source. A first reading whose history is unknown, and a
// reading below the previous one, restart the running total at the reading
// and leave key a lower bound.
func (l *Ledger) increase(key amountKey, reading float64) float64 {
	st := l.amountState(key)
	switch history, known := l.historyOf(key); {
	case !st.hasPrevious && known:
		st.history = history
	case !st.hasPrevious, reading < st.previous:
		st.history = reading - st.total
		st.lowerBound = true
	}
	st.previous, st.hasPrevious = reading, true
	return max(0, reading-st.history-st.total)
}

// recordAmounts returns the amounts of the records the turn's settlement
// split by model.
func (l *Ledger) recordAmounts() []carriedAmount {
	var group []carriedAmount
	for _, rec := range l.turnRecords {
		spent := hasSpend(rec.usage)
		for _, a := range rec.amounts {
			group = append(group, carriedAmount{AmountReading: a, model: rec.model, ownSpent: spent})
		}
	}
	return group
}

// liveAmounts returns the amounts of the live reading and its per-model
// entries, when the live source settled the turn's token figure or settles no
// token figure at all.
func (l *Ledger) liveAmounts(live *SpendReading, s Settlement) []carriedAmount {
	if live == nil || (l.countsTokens() && !slices.Contains(s.Provenance, l.liveProvenance)) {
		return nil
	}
	group := carryAmounts(nil, *live)
	for _, entry := range live.ByModel {
		group = carryAmounts(group, entry)
	}
	return group
}

func carryAmounts(group []carriedAmount, r SpendReading) []carriedAmount {
	spent := (r.Input != nil && *r.Input > 0) || (r.Output != nil && *r.Output > 0)
	for _, a := range r.Amounts {
		group = append(group, carriedAmount{AmountReading: a, model: strings.TrimSpace(r.Model), ownSpent: spent})
	}
	return group
}

func (l *Ledger) observedAmounts() []carriedAmount {
	group := make([]carriedAmount, 0, len(l.held))
	for _, a := range l.held {
		group = append(group, carriedAmount{AmountReading: a, observed: true})
	}
	return group
}

// reportedAmounts returns the run-cumulative amounts, sorted by unit then
// model.
func (l *Ledger) reportedAmounts() []domain.ReportedAmount {
	var out []domain.ReportedAmount
	for key, st := range l.amounts {
		if !st.appears(key.unit) {
			continue
		}
		_, forever := l.amountUnitsForever[key.unit]
		out = append(out, domain.ReportedAmount{Unit: key.unit, Model: key.model, Amount: st.total, LowerBound: st.lowerBound || forever})
	}
	return MergeAmounts(out, nil)
}

// fixAmountHistory sets the amount history from the records the boundary read.
func (l *Ledger) fixAmountHistory(read RecordsRead) {
	history := amountHistory{fixed: true, byKey: make(map[amountKey]float64), unknown: make(map[string]struct{})}
	seen := make(map[string]struct{}, len(read.Readings))
	for _, r := range read.Readings {
		if _, repeat := seen[r.Identity]; repeat || r.Identity == "" {
			continue
		}
		seen[r.Identity] = struct{}{}
		for _, a := range r.Amounts {
			unit, ok := declaredUnit(l.records.Amounts, a)
			switch {
			case !ok:
			case a.Amount == nil:
				history.unknown[unit] = struct{}{}
			default:
				history.byKey[amountKey{unit, strings.TrimSpace(r.Model)}] += *a.Amount
			}
		}
	}
	l.amountHistory = history
}

// historyOf returns what the session held for key before the run, and false
// when that is unknown: no boundary fixed it, a history record left its unit
// unread, or the records source does not declare the unit.
func (l *Ledger) historyOf(key amountKey) (float64, bool) {
	h := l.amountHistory
	if !h.fixed {
		return 0, false
	}
	if _, unread := h.unknown[key.unit]; unread {
		return 0, false
	}
	if _, declared := declarationOf(l.records.Amounts, key.unit); !h.created && !declared {
		return 0, false
	}
	return h.byKey[key], true
}

// declaredUnit returns the unit a reading belongs to under decls. A reading
// names its unit when the source declares more than one, or declares a
// currency; a source that declares one fixed unit leaves it empty. It reports
// false for a reading no declaration covers and for a currency code that is
// not three ASCII letters.
func declaredUnit(decls []AmountDeclaration, r AmountReading) (string, bool) {
	for _, d := range decls {
		if !d.Currency && r.Unit != "" && d.Unit == r.Unit {
			return d.Unit, true
		}
	}
	for _, d := range decls {
		if d.Currency {
			return currencyCode(r.Unit)
		}
	}
	if len(decls) == 1 && r.Unit == "" {
		return decls[0].Unit, true
	}
	return "", false
}

// declarationOf returns the declaration that covers unit.
func declarationOf(decls []AmountDeclaration, unit string) (AmountDeclaration, bool) {
	for _, d := range decls {
		if !d.Currency && d.Unit == unit {
			return d, true
		}
	}
	for _, d := range decls {
		if d.Currency {
			return d, true
		}
	}
	return AmountDeclaration{}, false
}

// currencyCode returns the uppercased ISO 4217 code of s, or false when s is
// not three ASCII letters.
func currencyCode(s string) (string, bool) {
	if len(s) != 3 {
		return "", false
	}
	for i := range len(s) {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return "", false
		}
	}
	return strings.ToUpper(s), true
}

// FormatProvenance returns the text of a run's recorded provenance: the
// distinct members of p as source and strategy pairs, sorted and
// comma-joined, such as "journal:records,output:live". It returns "" for none.
func FormatProvenance(p []domain.UsageProvenance) string {
	pairs := make([]string, 0, len(p))
	for _, member := range p {
		pairs = append(pairs, string(member.Source)+":"+strategyName(member.Strategy))
	}
	slices.Sort(pairs)
	return strings.Join(slices.Compact(pairs), ",")
}

func strategyName(s Strategy) string {
	switch s {
	case StrategyPull:
		return "pull"
	case StrategyRecords:
		return "records"
	default:
		return "live"
	}
}

// MergeAmounts returns, per unit and model, the larger amount of a and b with
// either's lower-bound flag, sorted by unit then model. It returns nil when
// both are empty and never mutates its inputs.
func MergeAmounts(a, b []domain.ReportedAmount) []domain.ReportedAmount {
	return combineAmounts(a, b, func(x, y float64) float64 { return max(x, y) })
}

// AddAmounts returns, per unit and model, the sum of a and b with either's
// lower-bound flag, sorted by unit then model. It returns nil when both are
// empty and never mutates its inputs.
func AddAmounts(a, b []domain.ReportedAmount) []domain.ReportedAmount {
	return combineAmounts(a, b, func(x, y float64) float64 { return x + y })
}

type amountKey struct{ unit, model string }

func combineAmounts(a, b []domain.ReportedAmount, combine func(x, y float64) float64) []domain.ReportedAmount {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	byKey := make(map[amountKey]domain.ReportedAmount, len(a)+len(b))
	for _, amount := range slices.Concat(a, b) {
		key := amountKey{amount.Unit, amount.Model}
		prior, seen := byKey[key]
		if seen {
			amount.Amount = combine(prior.Amount, amount.Amount)
			amount.LowerBound = amount.LowerBound || prior.LowerBound
		}
		byKey[key] = amount
	}
	merged := make([]domain.ReportedAmount, 0, len(byKey))
	for _, amount := range byKey {
		merged = append(merged, amount)
	}
	slices.SortFunc(merged, func(x, y domain.ReportedAmount) int {
		return cmp.Or(cmp.Compare(x.Unit, y.Unit), cmp.Compare(x.Model, y.Model))
	})
	return merged
}
