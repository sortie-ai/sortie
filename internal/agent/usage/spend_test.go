package usage_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

type byModel = map[string]domain.TokenUsage

type step struct {
	end   usage.TurnEnd
	live  *usage.SpendReading
	read  usage.RecordsRead
	needs bool
	want  usage.Settlement
}

var (
	replied   = usage.TurnEnd{Replied: true, ModelReached: true}
	cancelled = usage.TurnEnd{Replied: true, Cancelled: true, ModelReached: true}
	unread    = usage.RecordsRead{}
	withChild = usage.TurnEnd{Replied: true, ModelReached: true, Children: true, ChildrenSettled: true}

	hist = rec("h", "m0", 50, 5)
	ra   = rec("a", "m1", 20, 3)
	rb   = rec("b", "m2", 10, 1)
	rc   = rec("c", "m1", 5, 1)

	liveProv = domain.UsageProvenance{Source: usage.SourceProtocol, Strategy: usage.StrategyLive}
	recProv  = domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}
)

func u(in, out int64) domain.TokenUsage { return uc(in, out, 0) }

func uc(in, out, cr int64) domain.TokenUsage {
	return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, CacheReadTokens: cr}
}

func live(in, out int64) *usage.SpendReading { return liveWith("", in, out, 0) }

func liveWith(model string, in, out, cr int64) *usage.SpendReading {
	return &usage.SpendReading{Input: new(in), Output: new(out), CacheRead: new(cr), CacheWrite: new(int64(0)), Model: model}
}

func rec(id, model string, in, out int64) usage.SpendReading { return recCache(id, model, in, out, 0) }

func recCache(id, model string, in, out, cr int64) usage.SpendReading {
	return usage.SpendReading{Input: new(in), Output: new(out), CacheRead: new(cr), CacheWrite: new(int64(0)), Model: model, Identity: id}
}

func recs(readings ...usage.SpendReading) usage.RecordsRead {
	return usage.RecordsRead{Readings: readings, OK: true}
}

func turnWith(in, out, reasoning, cr int64) *usage.SpendReading {
	r := liveWith("", in, out, cr)
	r.Reasoning = new(reasoning)
	return r
}

func withKids(read usage.RecordsRead, kids ...usage.SpendReading) usage.RecordsRead {
	read.ChildReadings, read.ChildrenOK = kids, true
	return read
}

func newLedger(t *testing.T, withRecords, lowerBound, perTurn bool, start usage.RecordsRead) *usage.Ledger {
	t.Helper()

	liveSource := usage.Source{
		Category: usage.SourceProtocol,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, CacheInInput: true, ReasoningInOutput: true, LowerBound: lowerBound},
	}
	if perTurn {
		liveSource.Spend = usage.SpendDeclaration{Shape: usage.SpendPerTurn}
	}
	var recordsSource usage.Source
	if withRecords {
		recordsSource = usage.Source{
			Category: usage.SourceJournal,
			Strategy: usage.StrategyRecords,
			Spend:    usage.SpendDeclaration{Shape: usage.SpendPerRequest, CacheInInput: true, ReasoningInOutput: true, ServedModel: true},
		}
	}
	l, err := usage.NewLedger(liveSource, recordsSource)
	if err != nil {
		t.Fatalf("NewLedger() = %v, want nil", err)
	}
	l.Boundary(start)
	return l
}

func TestLedger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		liveOnly   bool
		lowerBound bool
		perTurn    bool
		created    bool
		start      usage.RecordsRead
		steps      []step
	}{
		{
			name:  "split at matching positions, then a model-reaching turn adding nothing has no figure",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}},
				},
			},
		},
		{
			name:  "positions that do not match leave the amount unsplit",
			start: recs(hist),
			steps: []step{{
				end: replied, live: liveWith("", 30, 4, 5), read: recs(hist, ra, rb), needs: true,
				want: usage.Settlement{Run: uc(30, 4, 5), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name:  "run is the same when the read finds nothing new",
			start: recs(hist),
			steps: []step{{
				end: replied, live: live(30, 4), read: recs(hist), needs: true,
				want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name:  "failed read after a live turn, then a cancelled turn the records settle, counts each request once",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: unread, needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: cancelled, read: recs(hist, ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(35, 5), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
				},
			},
		},
		{
			name:  "records-settled turn, then a live reading that includes it, counts it once",
			start: recs(hist),
			steps: []step{
				{
					end: replied, read: recs(hist, ra, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: recProv},
				},
				{
					end: replied, live: live(45, 6), read: recs(hist, ra, rb, rec("d", "m1", 15, 2)), needs: true,
					want: usage.Settlement{Run: u(45, 6), ByModel: byModel{"m1": u(35, 5), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
			},
		},
		{
			name:  "falling live reading counts no record until a complete live turn fixes a new boundary",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, live: live(10, 2), read: recs(hist, ra, rb, rec("e", "m2", 10, 2)), needs: true,
					want: usage.Settlement{Run: u(40, 6), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Figure: true, Spent: true, Provenance: liveProv},
				},
				{
					end: replied, read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1)), needs: true,
					want: usage.Settlement{Run: u(40, 6), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}},
				},
				{
					end: replied, live: live(30, 5), read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1)), needs: true,
					want: usage.Settlement{Run: u(60, 9), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1), rec("g", "m2", 7, 1)), needs: true,
					want: usage.Settlement{Run: u(67, 10), ByModel: byModel{"m1": u(20, 3), "m2": u(17, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: recProv},
				},
			},
		},
		{
			name: "first read without a boundary counts nothing, and an incomplete turn does not fix one",
			steps: []step{
				{
					end: cancelled, live: live(30, 4), read: recs(ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Provenance: liveProv},
				},
				{
					end: replied, read: recs(ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(30, 4)},
				},
			},
		},
		{
			name:  "live reading missing a count is settled by the records",
			start: recs(hist),
			steps: []step{{
				end:  replied,
				live: &usage.SpendReading{Input: new(int64(30)), Output: new(int64(4)), CacheRead: new(int64(0))}, read: recs(hist, ra, rb), needs: true,
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: recProv},
			}},
		},
		{
			name:  "live reading that contradicts its declaration and a failed read give no figure",
			start: recs(hist),
			steps: []step{{
				end: replied, live: liveWith("", 30, 4, 35), read: unread, needs: true,
				want: usage.Settlement{},
			}},
		},
		{
			name:     "live reading naming a model credits its delta, and an equal reading adds nothing",
			liveOnly: true,
			steps: []step{
				{
					end: replied, live: liveWith("m1", 30, 4, 0),
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, live: liveWith("m2", 50, 6, 0),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, live: live(50, 6),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}},
				},
				{
					end: usage.TurnEnd{Replied: true}, live: live(50, 6),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}, Figure: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
			},
		},
		{
			name:       "lower-bound live source settles a figure that is never complete, and an equal reading adds nothing",
			liveOnly:   true,
			lowerBound: true,
			steps: []step{
				{
					end: replied, live: live(30, 4),
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Provenance: liveProv},
				},
				{
					end: replied, live: live(30, 4),
					want: usage.Settlement{Run: u(30, 4)},
				},
			},
		},
		{
			name:  "floored delta leaves the turn incomplete",
			start: recs(hist),
			steps: []step{
				{
					end: replied, read: recs(hist, rec("big", "m1", 30, 4)), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: recProv},
				},
				{
					end: replied, live: live(40, 3), read: unread, needs: true,
					want: usage.Settlement{Run: u(40, 4), ByModel: byModel{"m1": u(30, 4)}, Figure: true, Spent: true, Provenance: liveProv},
				},
			},
		},
		{
			name:  "records differing only in cache give no figure and change no state",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: unread, needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
				{
					end: replied, read: recs(hist, recCache("a", "m1", 30, 4, 10)), needs: true,
					want: usage.Settlement{Run: u(30, 4)},
				},
				{
					end: replied, read: recs(hist, recCache("a", "m1", 30, 4, 10), rec("b", "m1", 5, 1)), needs: true,
					want: usage.Settlement{Run: uc(35, 5, 10), ByModel: byModel{"m1": uc(35, 5, 10)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
				},
			},
		},
		{
			name:  "a record that does not normalize stops the count and leaves the turn incomplete",
			start: recs(hist),
			steps: []step{{
				end: replied, read: recs(hist, ra, rec("z", "m1", 0, 0), rc), needs: true,
				want: usage.Settlement{Run: u(20, 3), ByModel: byModel{"m1": u(20, 3)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
			}},
		},
		{
			name:  "cancelled reply settles from the records once, and the next live reading adds only what is new",
			start: recs(hist),
			steps: []step{
				{
					end: cancelled, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
				},
				{
					end: replied, live: live(35, 5), read: recs(hist, ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(35, 5), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
			},
		},
		{
			name:     "per-turn readings count a falling series in full, cache outside input and reasoning outside output, refuse a missing or negative count, and never cover the session",
			liveOnly: true,
			perTurn:  true,
			steps: []step{
				{
					end: replied, live: turnWith(15681, 4, 66, 384),
					want: usage.Settlement{Run: uc(16065, 70, 384), Figure: true, Spent: true, Complete: true, Provenance: liveProv},
				},
				{end: replied, live: live(5, 1), want: usage.Settlement{Run: uc(16065, 70, 384)}},
				{end: replied, live: turnWith(5, 1, -1, 0), want: usage.Settlement{Run: uc(16065, 70, 384)}},
				{end: replied, live: turnWith(-1, 1, 0, 0), want: usage.Settlement{Run: uc(16065, 70, 384)}},
				{
					end: replied, live: turnWith(827, 3, 0, 0),
					want: usage.Settlement{Run: uc(16892, 73, 384), Figure: true, Spent: true, Complete: true, Provenance: liveProv},
				},
			},
		},
		{
			name:     "reasoning count of a source that puts reasoning inside output is never read",
			liveOnly: true,
			steps:    []step{{end: replied, live: turnWith(30, 4, -1, 0), want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv}}},
		},
		{
			name:       "lower-bound live source leaves a records-settled turn incomplete",
			lowerBound: true,
			start:      recs(hist),
			steps: []step{{
				end: replied, read: recs(hist, ra, rb), needs: true,
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
			}},
		},
		{
			name:    "created session settles its first records-settled turn complete without a boundary read",
			created: true,
			steps: []step{{
				end: replied, read: recs(ra, rb), needs: true,
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: recProv},
			}},
		},
		{
			name:  "child records count once per identity, the first alone under the records provenance, without moving the live position or the alignment",
			start: recs(hist),
			steps: []step{
				{
					end: withChild, read: withKids(recs(hist), rec("c1", "m3", 7, 2)), needs: true,
					want: usage.Settlement{Run: u(7, 2), ByModel: byModel{"m3": u(7, 2)}, Model: "m3", Figure: true, Spent: true, Provenance: recProv},
				},
				{
					end: withChild, live: live(35, 5), read: withKids(recs(hist, ra, rb, rc), rec("c1", "m3", 7, 2), rec("c2", "m3", 3, 1)), needs: true,
					want: usage.Settlement{Run: u(45, 8), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1), "m3": u(10, 3)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
				},
			},
		},
		{
			name:  "unsettled child, failed child read, and refused child record each keep the turn a lower bound and count what they read",
			start: recs(hist),
			steps: []step{
				{
					end: usage.TurnEnd{Replied: true, ModelReached: true, Children: true}, live: live(10, 1), read: withKids(recs(hist), rec("c1", "m3", 7, 2)), needs: true,
					want: usage.Settlement{Run: u(17, 3), ByModel: byModel{"m3": u(7, 2)}, Model: "m3", Figure: true, Spent: true, Provenance: liveProv},
				},
				{
					end: withChild, live: live(20, 2), needs: true,
					read: usage.RecordsRead{Readings: recs(hist).Readings, OK: true, ChildReadings: []usage.SpendReading{rec("c2", "m3", 7, 2)}},
					want: usage.Settlement{Run: u(34, 6), ByModel: byModel{"m3": u(14, 4)}, Model: "m3", Figure: true, Spent: true, Provenance: liveProv},
				},
				{
					end: withChild, live: live(30, 3), read: withKids(recs(hist), rec("c3", "m3", 7, 2), rec("z", "m3", 0, 0)), needs: true,
					want: usage.Settlement{Run: u(51, 9), ByModel: byModel{"m3": u(21, 6)}, Model: "m3", Figure: true, Spent: true, Provenance: liveProv},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLedger(t, !tt.liveOnly, tt.lowerBound, tt.perTurn, tt.start)
			if tt.created {
				l.SessionCreated()
			}

			for i, st := range tt.steps {
				if got := l.NeedsRecords(st.end, st.live); got != st.needs {
					t.Errorf("step %d: NeedsRecords(%+v) = %v, want %v", i, st.end, got, st.needs)
				}

				got := l.Settle(st.end, st.live, st.read)

				if !reflect.DeepEqual(got, st.want) {
					t.Fatalf("step %d: Settle(%+v) = %+v, want %+v", i, st.end, got, st.want)
				}
			}
		})
	}
}

func TestLedger_SettlementByModelIsACopy(t *testing.T) {
	t.Parallel()

	l := newLedger(t, true, false, false, recs(hist))
	first := l.Settle(replied, live(30, 4), recs(hist, ra, rb))
	if first.ByModel == nil {
		t.Fatalf("Settle() ByModel = nil, want a split")
	}
	first.ByModel["m1"] = u(999, 999)
	delete(first.ByModel, "m2")

	got := l.Settle(replied, live(30, 4), recs(hist, ra, rb))

	want := byModel{"m1": u(20, 3), "m2": u(10, 1)}
	if !reflect.DeepEqual(got.ByModel, want) {
		t.Errorf("Settle() ByModel after the caller edited an earlier one = %+v, want %+v", got.ByModel, want)
	}
}

func TestNewLedger_RefusesAdapterMapped(t *testing.T) {
	t.Parallel()

	_, err := usage.NewLedger(usage.Source{Strategy: usage.StrategyLive, Spend: usage.SpendDeclaration{Shape: usage.SpendAdapterMapped}}, usage.Source{})

	if !errors.Is(err, usage.ErrSpendUnsupported) {
		t.Errorf("NewLedger(adapter-mapped live) error = %v, want ErrSpendUnsupported", err)
	}
}
