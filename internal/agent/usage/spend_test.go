package usage_test

import (
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

	hist = rec("h", "m0", 50, 5)
	ra   = rec("a", "m1", 20, 3)
	rb   = rec("b", "m2", 10, 1)
	rc   = rec("c", "m1", 5, 1)
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

func newLedger(t *testing.T, withRecords bool, start usage.RecordsRead) *usage.Ledger {
	t.Helper()

	liveSource := usage.Source{
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, CacheInInput: true, ReasoningInOutput: true},
	}
	var recordsSource usage.Source
	if withRecords {
		recordsSource = usage.Source{
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
		name     string
		liveOnly bool
		start    usage.RecordsRead
		steps    []step
	}{
		{
			name:  "split at matching positions, then a model-reaching turn adding nothing has no figure",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true},
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
				want: usage.Settlement{Run: uc(30, 4, 5), Figure: true, Spent: true, Complete: true, CoversSession: true},
			}},
		},
		{
			name:  "run is the same when the read finds nothing new",
			start: recs(hist),
			steps: []step{{
				end: replied, live: live(30, 4), read: recs(hist), needs: true,
				want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true},
			}},
		},
		{
			name:  "failed read after a live turn, then a cancelled turn the records settle, counts each request once",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: unread, needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: cancelled, read: recs(hist, ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(35, 5), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true},
				},
			},
		},
		{
			name:  "records-settled turn, then a live reading that includes it, counts it once",
			start: recs(hist),
			steps: []step{
				{
					end: replied, read: recs(hist, ra, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true},
				},
				{
					end: replied, live: live(45, 6), read: recs(hist, ra, rb, rec("d", "m1", 15, 2)), needs: true,
					want: usage.Settlement{Run: u(45, 6), ByModel: byModel{"m1": u(35, 5), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
			},
		},
		{
			name:  "falling live reading counts no record until a complete live turn fixes a new boundary",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: replied, live: live(10, 2), read: recs(hist, ra, rb, rec("e", "m2", 10, 2)), needs: true,
					want: usage.Settlement{Run: u(40, 6), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Figure: true, Spent: true},
				},
				{
					end: replied, read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1)), needs: true,
					want: usage.Settlement{Run: u(40, 6), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}},
				},
				{
					end: replied, live: live(30, 5), read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1)), needs: true,
					want: usage.Settlement{Run: u(60, 9), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: replied, read: recs(hist, ra, rb, rec("e", "m2", 10, 2), rec("f", "m2", 5, 1), rec("g", "m2", 7, 1)), needs: true,
					want: usage.Settlement{Run: u(67, 10), ByModel: byModel{"m1": u(20, 3), "m2": u(17, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true},
				},
			},
		},
		{
			name: "first read without a boundary counts nothing, and an incomplete turn does not fix one",
			steps: []step{
				{
					end: cancelled, live: live(30, 4), read: recs(ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true},
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
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true},
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
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: replied, live: liveWith("m2", 50, 6, 0),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: replied, live: live(50, 6),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}},
				},
				{
					end: usage.TurnEnd{Replied: true}, live: live(50, 6),
					want: usage.Settlement{Run: u(50, 6), ByModel: byModel{"m1": u(30, 4), "m2": u(20, 2)}, Figure: true, Complete: true, CoversSession: true},
				},
			},
		},
		{
			name:  "floored delta leaves the turn incomplete",
			start: recs(hist),
			steps: []step{
				{
					end: replied, read: recs(hist, rec("big", "m1", 30, 4)), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true},
				},
				{
					end: replied, live: live(40, 3), read: unread, needs: true,
					want: usage.Settlement{Run: u(40, 4), ByModel: byModel{"m1": u(30, 4)}, Figure: true, Spent: true},
				},
			},
		},
		{
			name:  "records differing only in cache give no figure and change no state",
			start: recs(hist),
			steps: []step{
				{
					end: replied, live: live(30, 4), read: unread, needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true},
				},
				{
					end: replied, read: recs(hist, recCache("a", "m1", 30, 4, 10)), needs: true,
					want: usage.Settlement{Run: u(30, 4)},
				},
				{
					end: replied, read: recs(hist, recCache("a", "m1", 30, 4, 10), rec("b", "m1", 5, 1)), needs: true,
					want: usage.Settlement{Run: uc(35, 5, 10), ByModel: byModel{"m1": uc(35, 5, 10)}, Model: "m1", Figure: true, Spent: true},
				},
			},
		},
		{
			name:  "a record that does not normalize stops the count and leaves the turn incomplete",
			start: recs(hist),
			steps: []step{{
				end: replied, read: recs(hist, ra, rec("z", "m1", 0, 0), rc), needs: true,
				want: usage.Settlement{Run: u(20, 3), ByModel: byModel{"m1": u(20, 3)}, Model: "m1", Figure: true, Spent: true},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLedger(t, !tt.liveOnly, tt.start)

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

	l := newLedger(t, true, recs(hist))
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
