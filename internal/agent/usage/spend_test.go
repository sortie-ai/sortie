package usage_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

type byModel = map[string]domain.TokenUsage

type observation struct {
	reading         usage.SpendReading
	inTurn          bool
	wantProvisional domain.TokenUsage
	wantNew         bool
}

type step struct {
	observed []observation
	end      usage.TurnEnd
	live     *usage.SpendReading
	read     usage.RecordsRead
	needs    bool
	want     usage.Settlement
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
	rd   = rec("d", "m2", 4, 1)
	re   = rec("e", "m2", 7, 2)

	unnormalizable = usage.SpendReading{Output: new(int64(5)), CacheRead: new(int64(0)), CacheWrite: new(int64(0)), Model: "m", Identity: "h"}

	liveProv = []domain.UsageProvenance{{Source: usage.SourceProtocol, Strategy: usage.StrategyLive}}
	recProv  = []domain.UsageProvenance{{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}}

	liveRecProv = []domain.UsageProvenance{liveProv[0], recProv[0]}
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

func entry(model string, in, out int64) usage.SpendReading {
	r := turnWith(in, out, 0, 0)
	r.Model = model
	return *r
}

func entries(rs ...usage.SpendReading) *usage.SpendReading { return &usage.SpendReading{ByModel: rs} }

func withKids(read usage.RecordsRead, kids ...usage.SpendReading) usage.RecordsRead {
	read.ChildReadings, read.ChildrenOK = kids, true
	return read
}

func newLedger(t *testing.T, withRecords, lowerBound, perTurn, perRequest, cumulative, resumeFromRecords bool, start usage.RecordsRead) *usage.Ledger {
	t.Helper()

	liveSource := usage.Source{
		Category: usage.SourceProtocol,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, CacheInInput: true, ReasoningInOutput: true, LowerBound: lowerBound},
	}
	if perTurn {
		liveSource.Spend = usage.SpendDeclaration{Shape: usage.SpendPerTurn}
	}
	if perRequest {
		liveSource.Spend = usage.SpendDeclaration{Shape: usage.SpendPerRequest, CacheInInput: true, ReasoningInOutput: true}
	}
	if cumulative {
		liveSource.Spend = usage.SpendDeclaration{Shape: usage.SpendSessionCumulative, CacheInInput: true, ReasoningInOutput: true, ServedModel: true, Replay: usage.ReplayLastFigure}
		if resumeFromRecords {
			liveSource.Spend.Replay = usage.ReplayNone
		}
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
		perRequest bool
		cumulative bool
		replayNone bool
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
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
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
					want: usage.Settlement{Run: u(67, 10), ByModel: byModel{"m1": u(20, 3), "m2": u(17, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
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
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
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
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
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
			name:    "per-turn entries settle their sum split per trimmed entry model, over the records split and Model, and an unnamed entry adds to the counts only",
			perTurn: true,
			start:   recs(hist),
			steps: []step{{
				end: replied, needs: true, read: recs(hist, ra, rb),
				live: &usage.SpendReading{Model: "m9", ByModel: []usage.SpendReading{entry(" m1 ", 10, 2), entry("m2", 5, 1), entry("m1", 4, 1), entry("", 3, 1)}},
				want: usage.Settlement{Run: u(22, 5), ByModel: byModel{"m1": u(14, 3), "m2": u(5, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: liveProv},
			}},
		},
		{
			name:    "per-turn entry with a nil count is no figure, so the records settle the turn",
			perTurn: true,
			start:   recs(hist),
			steps: []step{{
				end: replied, needs: true, read: recs(hist, ra, rb),
				live: entries(entry("m1", 10, 2), usage.SpendReading{Model: "m2", Input: new(int64(5))}),
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
			}},
		},
		{
			name:    "per-turn entries naming no model leave the split to the records",
			perTurn: true,
			start:   recs(hist),
			steps: []step{{
				end: replied, needs: true, read: recs(hist, ra, rb),
				live: entries(entry("", 20, 3), entry(" ", 10, 1)),
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: liveProv},
			}},
		},
		{
			name:     "entries are not read under a process-cumulative declaration",
			liveOnly: true,
			steps: []step{{
				end: replied, live: &usage.SpendReading{Input: new(int64(30)), Output: new(int64(4)), CacheRead: new(int64(0)), CacheWrite: new(int64(0)), Model: "m1", ByModel: []usage.SpendReading{entry("m2", 1, 1)}},
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(30, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
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
				want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv},
			}},
		},
		{
			name:  "child records count once per identity, the first alone under the records provenance and later ones beside the live source, without moving the live position or the alignment",
			start: recs(hist),
			steps: []step{
				{
					end: withChild, read: withKids(recs(hist), rec("c1", "m3", 7, 2)), needs: true,
					want: usage.Settlement{Run: u(7, 2), ByModel: byModel{"m3": u(7, 2)}, Model: "m3", Figure: true, Spent: true, Provenance: recProv},
				},
				{
					end: withChild, live: live(35, 5), read: withKids(recs(hist, ra, rb, rc), rec("c1", "m3", 7, 2), rec("c2", "m3", 3, 1)), needs: true,
					want: usage.Settlement{Run: u(45, 8), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1), "m3": u(10, 3)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveRecProv},
				},
			},
		},
		{
			name:  "unsettled child, failed child read, and refused child record each keep the turn a lower bound and count what they read",
			start: recs(hist),
			steps: []step{
				{
					end: usage.TurnEnd{Replied: true, ModelReached: true, Children: true}, live: live(10, 1), read: withKids(recs(hist), rec("c1", "m3", 7, 2)), needs: true,
					want: usage.Settlement{Run: u(17, 3), ByModel: byModel{"m3": u(7, 2)}, Model: "m3", Figure: true, Spent: true, Provenance: liveRecProv},
				},
				{
					end: withChild, live: live(20, 2), needs: true,
					read: usage.RecordsRead{Readings: recs(hist).Readings, OK: true, ChildReadings: []usage.SpendReading{rec("c2", "m3", 7, 2)}},
					want: usage.Settlement{Run: u(34, 6), ByModel: byModel{"m3": u(14, 4)}, Model: "m3", Figure: true, Spent: true, Provenance: liveRecProv},
				},
				{
					end: withChild, live: live(30, 3), read: withKids(recs(hist), rec("c3", "m3", 7, 2), rec("z", "m3", 0, 0)), needs: true,
					want: usage.Settlement{Run: u(51, 9), ByModel: byModel{"m3": u(21, 6)}, Model: "m3", Figure: true, Spent: true, Provenance: liveRecProv},
				},
			},
		},
		{
			name: "per-request: the records replace the held readings, and unusable records leave a lower bound the next records settlement counts from", perRequest: true, start: recs(hist),
			steps: []step{
				{observed: []observation{{reading: ra, wantProvisional: u(20, 3), wantNew: true}, {reading: rb, wantProvisional: u(30, 4), wantNew: true}}, end: replied, read: unread, needs: true,
					want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Provenance: liveProv}},
				{observed: []observation{{reading: rc, wantProvisional: u(5, 1), wantNew: true}}, end: replied, read: recs(hist, ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(35, 5), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv}},
			},
		},
		{
			name: "per-request: the report that ended a replied turn settles it complete without covering the session, and an unknown held or settling reading blocks that for its turn only", perRequest: true, start: recs(hist),
			steps: []step{
				{observed: []observation{{reading: ra, wantProvisional: u(20, 3), wantNew: true}}, end: replied, live: &rb, read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
				{observed: []observation{{reading: rec("", "m1", 9, 1)}, {reading: rc, wantProvisional: u(5, 1), wantNew: true}}, end: replied, live: &rd, read: recs(hist, ra, rb, rc, rd), needs: true,
					want: usage.Settlement{Run: u(39, 6), ByModel: byModel{"m1": u(25, 4), "m2": u(14, 2)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv}},
				{end: replied, live: &re, read: recs(hist, ra, rb, rc, rd, re), needs: true,
					want: usage.Settlement{Run: u(46, 8), ByModel: byModel{"m1": u(25, 4), "m2": u(21, 4)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
				{observed: []observation{{reading: rec("f", "m1", 3, 1), wantProvisional: u(3, 1), wantNew: true}}, end: replied, live: &unnormalizable, read: recs(hist, ra, rb, rc, rd, re, rec("f", "m1", 3, 1)), needs: true,
					want: usage.Settlement{Run: u(49, 9), ByModel: byModel{"m1": u(28, 5), "m2": u(21, 4)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv}},
			},
		},
		{
			name: "per-request: a settling report on an unreplied or cancelled turn yields to the records", perRequest: true, start: recs(hist),
			steps: []step{
				{observed: []observation{{reading: ra, wantProvisional: u(20, 3), wantNew: true}}, end: usage.TurnEnd{ModelReached: true}, live: &rb, read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv}},
				{end: cancelled, live: &rc, read: recs(hist, ra, rb, rc), needs: true,
					want: usage.Settlement{Run: u(35, 5), ByModel: byModel{"m1": u(25, 4), "m2": u(10, 1)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv}},
			},
		},
		{
			name: "per-request: a cancelled turn whose records read failed is counted from the records a later live-settled turn finds them in", perRequest: true, start: recs(hist),
			steps: []step{
				{end: cancelled, read: unread, needs: true},
				{end: replied, live: &rd, read: recs(hist, ra, rd), needs: true,
					want: usage.Settlement{Run: u(24, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(4, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv}},
			},
		},
		{
			name: "per-turn: a cancelled turn whose records read failed is counted from the records a later live-settled turn finds them in", perTurn: true, start: recs(hist),
			steps: []step{
				{end: cancelled, read: unread, needs: true},
				{end: replied, live: turnWith(4, 1, 0, 0), read: recs(hist, ra, rd), needs: true,
					want: usage.Settlement{Run: u(24, 4), ByModel: byModel{"m1": u(20, 3), "m2": u(4, 1)}, Model: "m1", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: recProv}},
			},
		},
		{
			name: "per-request: a cancelled turn the records never held leaves the later live-settled turn counted once", perRequest: true, start: recs(hist),
			steps: []step{
				{end: cancelled, read: unread, needs: true},
				{end: replied, live: &rd, read: recs(hist, rd), needs: true,
					want: usage.Settlement{Run: u(4, 1), ByModel: byModel{"m2": u(4, 1)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
			},
		},
		{
			name: "per-request: a boundary fixed after the turn settled leaves it a lower bound, and no later settlement covers the session", perRequest: true,
			steps: []step{
				{observed: []observation{{reading: ra, wantProvisional: u(20, 3), wantNew: true}}, end: replied, read: recs(hist, ra), needs: true,
					want: usage.Settlement{Run: u(20, 3), Figure: true, Spent: true, Provenance: liveProv}},
				{observed: []observation{{reading: rb, wantProvisional: u(10, 1), wantNew: true}}, end: replied, read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(30, 4), ByModel: byModel{"m2": u(10, 1)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: recProv}},
			},
		},
		{
			name: "per-turn: a boundary that a live settlement fixes late leaves a complete records turn from covering the session", perTurn: true,
			steps: []step{
				{end: replied, live: turnWith(10, 2, 0, 0), read: recs(hist, ra), needs: true,
					want: usage.Settlement{Run: u(10, 2), Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
				{end: replied, read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(20, 3), ByModel: byModel{"m2": u(10, 1)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: recProv}},
			},
		},
		{
			name: "a late boundary fix leaves a cumulative live source able to cover the session",
			steps: []step{{
				end: replied, live: live(30, 4), read: recs(hist, ra, rb), needs: true,
				want: usage.Settlement{Run: u(30, 4), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name: "a turn short of child records leaves the session uncoverable, a complete records settlement included", start: recs(hist),
			steps: []step{
				{end: usage.TurnEnd{Replied: true, ModelReached: true, Children: true}, read: withKids(recs(hist, ra), rec("c1", "m3", 7, 2)), needs: true,
					want: usage.Settlement{Run: u(27, 5), ByModel: byModel{"m1": u(20, 3), "m3": u(7, 2)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv}},
				{end: replied, read: recs(hist, ra, rb), needs: true,
					want: usage.Settlement{Run: u(37, 6), ByModel: byModel{"m1": u(20, 3), "m2": u(10, 1), "m3": u(7, 2)}, Model: "m2", Figure: true, Spent: true, Complete: true, Provenance: recProv}},
			},
		},
		{
			name:       "session-cumulative: in-turn readings count one request each and the settled turn covers the session",
			cumulative: true, liveOnly: true, created: true,
			steps: []step{{
				observed: []observation{
					{reading: *liveWith("", 100, 10, 0), inTurn: true, wantProvisional: u(100, 10), wantNew: true},
					{reading: *liveWith("", 150, 12, 0), inTurn: true, wantProvisional: u(150, 12), wantNew: true},
					{reading: *liveWith("", 150, 12, 0), inTurn: true, wantProvisional: u(150, 12)},
				},
				end: replied, live: live(150, 12),
				want: usage.Settlement{Run: u(150, 12), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name:       "session-cumulative: an out-of-turn figure is history and counts nothing",
			cumulative: true, liveOnly: true, start: recs(),
			steps: []step{{
				observed: []observation{
					{reading: *liveWith("", 1000, 100, 0)},
					{reading: *liveWith("", 1100, 110, 0), inTurn: true, wantProvisional: u(100, 10), wantNew: true},
				},
				end: replied, live: live(1100, 110),
				want: usage.Settlement{Run: u(100, 10), Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name:       "session-cumulative: a history position derived from the reading less its own part is an estimate",
			cumulative: true, liveOnly: true,
			steps: []step{{
				observed: []observation{{reading: usage.SpendReading{Input: new(int64(1100)), Output: new(int64(110)), CacheRead: new(int64(0)), CacheWrite: new(int64(0)), Own: live(100, 10)}, inTurn: true, wantProvisional: u(100, 10), wantNew: true}},
				end:      replied, live: live(1100, 110),
				want: usage.Settlement{Run: u(100, 10), Figure: true, Spent: true, Estimated: true, Provenance: liveProv},
			}},
		},
		{
			name:       "session-cumulative: readings under two models split the settlement by their increases",
			cumulative: true, liveOnly: true, created: true,
			steps: []step{{
				observed: []observation{
					{reading: *liveWith("a", 100, 10, 0), inTurn: true, wantProvisional: u(100, 10), wantNew: true},
					{reading: *liveWith("b", 150, 18, 0), inTurn: true, wantProvisional: u(150, 18), wantNew: true},
				},
				end: replied, live: liveWith("b", 150, 18, 0),
				want: usage.Settlement{Run: u(150, 18), ByModel: byModel{"a": u(100, 10), "b": u(50, 8)}, Model: "a", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv},
			}},
		},
		{
			name:       "session-cumulative: an unreplied turn that reached the model settles the records' amount",
			cumulative: true, created: true,
			steps: []step{{
				observed: []observation{{reading: *liveWith("m1", 100, 10, 0), inTurn: true, wantProvisional: u(100, 10), wantNew: true}},
				end:      usage.TurnEnd{ModelReached: true}, live: liveWith("m1", 100, 10, 0),
				read: recs(rec("r1", "m1", 100, 10), rec("r2", "m1", 20, 2)), needs: true,
				want: usage.Settlement{Run: u(120, 12), ByModel: byModel{"m1": u(120, 12)}, Model: "m1", Figure: true, Spent: true, Provenance: recProv},
			}},
		},
		{
			name:       "session-cumulative: an unreplied turn whose records hold nothing settles the live amount as a lower bound",
			cumulative: true, created: true,
			steps: []step{{
				observed: []observation{{reading: *liveWith("m1", 100, 10, 0), inTurn: true, wantProvisional: u(100, 10), wantNew: true}},
				end:      usage.TurnEnd{ModelReached: true}, live: liveWith("m1", 100, 10, 0),
				read: recs(), needs: true,
				want: usage.Settlement{Run: u(100, 10), ByModel: byModel{"m1": u(100, 10)}, Model: "m1", Figure: true, Spent: true, Provenance: liveProv},
			}},
		},
		{
			name: "replay none: an unread boundary settles no figure, the next turn counts its increase without covering the session", cumulative: true, replayNone: true,
			steps: []step{
				{end: replied, live: liveWith("m", 1000, 10, 0), needs: true},
				{end: replied, live: liveWith("m", 1100, 12, 0), needs: true, want: usage.Settlement{Run: u(100, 2), ByModel: byModel{"m": u(100, 2)}, Model: "m", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
			},
		},
		{
			name: "replay none: a resumed session starts from the total of the records its boundary read holds", cumulative: true, replayNone: true, start: recs(hist),
			steps: []step{{end: replied, live: liveWith("m0", 90, 8, 0), read: recs(hist), want: usage.Settlement{Run: u(40, 3), ByModel: byModel{"m0": u(40, 3)}, Model: "m0", Figure: true, Spent: true, Complete: true, CoversSession: true, Provenance: liveProv}}},
		},
		{
			name: "replay none: records re-derive an unknown history and the next turn stays exact", cumulative: true, replayNone: true, start: recs(unnormalizable),
			steps: []step{
				{end: replied, live: liveWith("m", 1000, 10, 0), read: recs(unnormalizable, rec("r1", "m", 100, 2)), needs: true, want: usage.Settlement{Run: u(100, 2), ByModel: byModel{"m": u(100, 2)}, Model: "m", Figure: true, Spent: true, Complete: true, Provenance: recProv}},
				{end: replied, live: liveWith("m", 1150, 13, 0), read: recs(unnormalizable, rec("r1", "m", 100, 2)), want: usage.Settlement{Run: u(250, 5), ByModel: byModel{"m": u(250, 5)}, Model: "m", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
			},
		},
		{
			name: "replay none: a turn without a result settles from records, and later turns settle exactly without covering the session", cumulative: true, replayNone: true, start: recs(unnormalizable),
			steps: []step{
				{end: usage.TurnEnd{ModelReached: true}, live: liveWith("m", 950, 9, 0), read: recs(unnormalizable, rec("r1", "m", 50, 1), rec("r2", "m", 50, 1)), needs: true, want: usage.Settlement{Run: u(100, 2), ByModel: byModel{"m": u(100, 2)}, Model: "m", Figure: true, Spent: true, Provenance: recProv}},
				{end: replied, live: liveWith("m", 1150, 13, 0), read: recs(unnormalizable, rec("r1", "m", 50, 1), rec("r2", "m", 50, 1), rec("r3", "m", 150, 3)), needs: true, want: usage.Settlement{Run: u(250, 5), ByModel: byModel{"m": u(250, 5)}, Model: "m", Figure: true, Spent: true, Complete: true, Provenance: recProv}},
				{end: replied, live: liveWith("m", 1200, 14, 0), read: recs(unnormalizable, rec("r1", "m", 50, 1), rec("r2", "m", 50, 1), rec("r3", "m", 150, 3)), want: usage.Settlement{Run: u(300, 6), ByModel: byModel{"m": u(300, 6)}, Model: "m", Figure: true, Spent: true, Complete: true, Provenance: liveProv}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLedger(t, !tt.liveOnly, tt.lowerBound, tt.perTurn, tt.perRequest, tt.cumulative, tt.replayNone, tt.start)
			if tt.created {
				l.SessionCreated()
			}

			for i, st := range tt.steps {
				for j, o := range st.observed {
					got, gotNew := l.ObserveCumulative(o.reading, o.inTurn)
					if tt.perRequest {
						got, gotNew = l.ObserveRequest(o.reading)
					}
					if got != o.wantProvisional || gotNew != o.wantNew {
						t.Fatalf("step %d observation %d: ObserveCumulative(inTurn=%v) = %+v, %v, want %+v, %v", i, j, o.inTurn, got, gotNew, o.wantProvisional, o.wantNew)
					}
				}
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

func newObservingLedger(t *testing.T, duringTurn bool) *usage.Ledger {
	t.Helper()

	l, err := usage.NewLedger(usage.Source{
		Category: usage.SourceProtocol,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendPerTurn, ReasoningInOutput: true, DuringTurn: duringTurn},
	}, usage.Source{})
	if err != nil {
		t.Fatalf("NewLedger() = %v, want nil", err)
	}
	return l
}

func TestLedger_ObserveRequest(t *testing.T) {
	t.Parallel()

	type obs struct {
		settleFirst bool
		reading     usage.SpendReading
		want        domain.TokenUsage
		wantNew     bool
	}
	tests := []struct {
		name       string
		noDuring   bool
		observings []obs
	}{
		{
			name: "repeats hold the per-class maximum and report the identity new once",
			observings: []obs{
				{reading: rec("a", "m1", 10, 1), want: u(10, 1), wantNew: true},
				{reading: rec("a", "m1", 5, 4), want: u(10, 4)},
				{reading: rec("a", "m1", 8, 2), want: u(10, 4)},
				{reading: rec("b", "m1", 7, 2), want: u(17, 6), wantNew: true},
			},
		},
		{
			name: "a reading without identity changes nothing",
			observings: []obs{
				{reading: rec("", "m1", 10, 1)},
				{reading: rec("a", "m1", 3, 1), want: u(3, 1), wantNew: true},
			},
		},
		{
			name: "a reading that does not normalize changes nothing",
			observings: []obs{
				{reading: rec("a", "m1", 3, 1), want: u(3, 1), wantNew: true},
				{reading: usage.SpendReading{Identity: "b", Input: new(int64(9))}, want: u(3, 1)},
				{reading: rec("b", "m1", 2, 1), want: u(5, 2), wantNew: true},
			},
		},
		{
			name:     "a live declaration without DuringTurn changes nothing",
			noDuring: true,
			observings: []obs{
				{reading: rec("a", "m1", 3, 1)},
				{reading: rec("a", "m1", 3, 1)},
			},
		},
		{
			name: "the next turn's first reading starts from zero after Settle",
			observings: []obs{
				{reading: rec("a", "m1", 10, 1), want: u(10, 1), wantNew: true},
				{settleFirst: true, reading: rec("a", "m1", 4, 1), want: u(4, 1), wantNew: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newObservingLedger(t, !tt.noDuring)

			for i, o := range tt.observings {
				if o.settleFirst {
					l.Settle(replied, nil, unread)
				}

				got, gotNew := l.ObserveRequest(o.reading)

				if got != o.want || gotNew != o.wantNew {
					t.Fatalf("observation %d: ObserveRequest(%q) = %+v, %v, want %+v, %v", i, o.reading.Identity, got, gotNew, o.want, o.wantNew)
				}
			}
		})
	}
}

func amt(unit string, v float64) usage.AmountReading {
	return usage.AmountReading{Unit: unit, Amount: &v}
}

func spending(r usage.SpendReading, v float64) usage.SpendReading {
	r.Amounts = []usage.AmountReading{amt("", v)}
	return r
}

func liveSpending(model string, in, out int64, v float64) *usage.SpendReading {
	r := spending(*liveWith(model, in, out, 0), v)
	return &r
}

func reported(unit, model string, v float64, lowerBound bool) domain.ReportedAmount {
	return domain.ReportedAmount{Unit: unit, Model: model, Amount: v, LowerBound: lowerBound}
}

func TestLedger_Amounts(t *testing.T) {
	t.Parallel()

	const requests = "premium requests"
	usd := func(shape usage.AmountShape) []usage.AmountDeclaration {
		return []usage.AmountDeclaration{{Unit: domain.UnitUSD, Shape: shape}}
	}
	perRequests := func(shape usage.AmountShape) []usage.AmountDeclaration {
		return []usage.AmountDeclaration{{Unit: requests, Shape: shape}}
	}
	records := func(amounts []usage.AmountDeclaration) *usage.SpendDeclaration {
		return &usage.SpendDeclaration{Shape: usage.SpendPerRequest, ServedModel: true, Amounts: amounts}
	}
	cumulative := usage.SpendDeclaration{Shape: usage.SpendSessionCumulative, ServedModel: true, Amounts: perRequests(usage.AmountSessionCumulative)}
	currency := usage.SpendDeclaration{Amounts: []usage.AmountDeclaration{{Currency: true, Shape: usage.AmountSessionCumulative}}}

	type amountStep struct {
		observe     []usage.AmountReading
		outsideTurn bool
		end         usage.TurnEnd
		live        *usage.SpendReading
		read        usage.RecordsRead
		want        []domain.ReportedAmount
	}
	tests := []struct {
		name      string
		live      usage.SpendDeclaration
		records   *usage.SpendDeclaration
		created   bool
		start     usage.RecordsRead
		steps     []amountStep
		wantProvs []domain.UsageProvenance
	}{
		{
			name:    "per turn: costs sum per model, a zero cost beside tokens records none, and a records-settled turn marks its model",
			live:    usage.SpendDeclaration{Shape: usage.SpendPerTurn, ServedModel: true, Amounts: usd(usage.AmountPerTurn)},
			records: &usage.SpendDeclaration{Shape: usage.SpendPerRequest, ServedModel: true},
			start:   recs(hist),
			steps: []amountStep{
				{end: replied, live: entries(spending(entry("m1", 20, 3), 0.25), spending(entry("m2", 10, 1), 0)), read: recs(hist, ra, rb), want: []domain.ReportedAmount{reported("USD", "m1", 0.25, false)}},
				{end: replied, live: entries(spending(entry("m2", 4, 1), 0.125)), read: recs(hist, ra, rb, rd), want: []domain.ReportedAmount{reported("USD", "m1", 0.25, false), reported("USD", "m2", 0.125, true)}},
				{end: cancelled, read: recs(hist, ra, rb, rd, rc), want: []domain.ReportedAmount{reported("USD", "m1", 0.25, true), reported("USD", "m2", 0.125, true)}},
			},
		},
		{
			name:    "session cumulative: a resumed session counts only the increase over its history records",
			live:    cumulative,
			records: records(perRequests(usage.AmountPerRequest)),
			start:   recs(spending(hist, 3)),
			steps: []amountStep{
				{end: replied, live: liveSpending("m0", 90, 8, 5), read: recs(spending(hist, 3)), want: []domain.ReportedAmount{reported(requests, "m0", 2, false)}},
			},
		},
		{
			name:    "session cumulative: an interrupted turn the records settle, then a live turn, counts each request once",
			live:    cumulative,
			records: records(perRequests(usage.AmountPerRequest)),
			created: true,
			steps: []amountStep{
				{end: cancelled, read: recs(spending(ra, 2)), want: []domain.ReportedAmount{reported(requests, "m1", 2, false)}},
				{end: replied, live: liveSpending("m1", 30, 4, 5), read: recs(spending(ra, 2)), want: []domain.ReportedAmount{reported(requests, "m1", 5, false)}},
			},
		},
		{
			name:    "session cumulative: records and a running amount of the same requests in one settlement count once",
			live:    cumulative,
			records: records(perRequests(usage.AmountPerRequest)),
			created: true,
			steps: []amountStep{
				{end: replied, live: liveSpending("m1", 30, 4, 5), read: recs(spending(ra, 2), spending(rec("b", "m1", 10, 1), 3)), want: []domain.ReportedAmount{reported(requests, "m1", 5, false)}},
			},
		},
		{
			name: "session cumulative: a falling reading lowers nothing and marks its key",
			live: cumulative, created: true,
			steps: []amountStep{
				{end: replied, live: liveSpending("m1", 30, 4, 5), want: []domain.ReportedAmount{reported(requests, "m1", 5, false)}},
				{end: replied, live: liveSpending("m1", 40, 5, 3), want: []domain.ReportedAmount{reported(requests, "m1", 5, true)}},
			},
		},
		{
			name:      "records per request: history never counts and child records add no amount but mark their model",
			live:      usage.SpendDeclaration{Shape: usage.SpendPerTurn, ServedModel: true},
			records:   records(usd(usage.AmountPerRequest)),
			start:     recs(spending(hist, 9)),
			wantProvs: liveRecProv,
			steps: []amountStep{
				{
					end: withChild, live: live(30, 4), read: withKids(recs(spending(hist, 9), spending(ra, 0.25), spending(rb, 0.125)), spending(rec("c1", "m3", 7, 2), 4)),
					want: []domain.ReportedAmount{reported("USD", "m1", 0.25, false), reported("USD", "m2", 0.125, false)},
				},
				{
					end: replied, live: live(4, 1), read: recs(spending(hist, 9), spending(ra, 0.25), spending(rb, 0.125), spending(rec("d", "m3", 4, 1), 0.5)),
					want: []domain.ReportedAmount{reported("USD", "m1", 0.25, false), reported("USD", "m2", 0.125, false), reported("USD", "m3", 0.5, true)},
				},
			},
		},
		{
			name: "observed: a created session counts from 0 in the reading's own currency",
			live: currency, created: true,
			steps: []amountStep{{observe: []usage.AmountReading{amt("eur", 1.5)}, end: replied, want: []domain.ReportedAmount{reported("EUR", "", 1.5, false)}}},
		},
		{
			name:  "observed: a continued session's first reading counts nothing and marks its key",
			live:  currency,
			steps: []amountStep{{observe: []usage.AmountReading{amt("EUR", 1.5)}, end: replied, want: []domain.ReportedAmount{reported("EUR", "", 0, true)}}},
		},
		{
			name: "observed: a reading outside a turn counts nothing, the same reading in a turn counts",
			live: currency, created: true,
			steps: []amountStep{
				{observe: []usage.AmountReading{amt("EUR", 1.5)}, outsideTurn: true, end: replied},
				{observe: []usage.AmountReading{amt("EUR", 1.5)}, end: replied, want: []domain.ReportedAmount{reported("EUR", "", 1.5, false)}},
			},
		},
		{
			name: "observed: a currency-declared reading with no unit reads as absent",
			live: currency, created: true,
			steps: []amountStep{{observe: []usage.AmountReading{amt("", 1.5)}, end: replied}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.live.CacheInInput, tt.live.ReasoningInOutput = true, true
			liveSource := usage.Source{Category: usage.SourceProtocol, Strategy: usage.StrategyLive, Spend: tt.live}
			var recordsSource usage.Source
			if tt.records != nil {
				tt.records.CacheInInput, tt.records.ReasoningInOutput = true, true
				recordsSource = usage.Source{Category: usage.SourceJournal, Strategy: usage.StrategyRecords, Spend: *tt.records}
			}
			l, err := usage.NewLedger(liveSource, recordsSource)
			if err != nil {
				t.Fatalf("NewLedger() = %v, want nil", err)
			}
			l.Boundary(tt.start)
			if tt.created {
				l.SessionCreated()
			}

			for i, st := range tt.steps {
				l.ObserveAmounts(st.observe, !st.outsideTurn)

				got := l.Settle(st.end, st.live, st.read)

				if !reflect.DeepEqual(got.Amounts, st.want) {
					t.Fatalf("step %d: Settle(%+v).Amounts = %+v, want %+v", i, st.end, got.Amounts, st.want)
				}
				if tt.live.Shape == usage.SpendUndeclared && (got.Figure || got.Provenance != nil) {
					t.Errorf("step %d: Settle(%+v) = figure %v, provenance %v, want neither for a ledger that counts no tokens", i, st.end, got.Figure, got.Provenance)
				}
				if i == 0 && tt.wantProvs != nil && !reflect.DeepEqual(got.Provenance, tt.wantProvs) {
					t.Errorf("step %d: Settle(%+v).Provenance = %v, want %v", i, st.end, got.Provenance, tt.wantProvs)
				}
			}
		})
	}
}

func TestLedger_SettlementByModelIsACopy(t *testing.T) {
	t.Parallel()

	l := newLedger(t, true, false, false, false, false, false, recs(hist))
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

func TestNewLedger_RefusesUnsupportedDeclarations(t *testing.T) {
	t.Parallel()

	cumulative := usage.SpendDeclaration{Shape: usage.SpendSessionCumulative, Replay: usage.ReplayLastFigure}
	replaying := usage.SpendDeclaration{Shape: usage.SpendPerRequest, Replay: usage.ReplayLastFigure}
	tests := []struct {
		name    string
		live    usage.SpendDeclaration
		records usage.Source
	}{
		{"replay on a live source of another shape", usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, Replay: usage.ReplayLastFigure}, usage.Source{}},
		{"every-turn replay on a token-shaped live source", usage.SpendDeclaration{Shape: usage.SpendSessionCumulative, Replay: usage.ReplayEveryTurn}, usage.Source{}},
		{"replay on the records source", cumulative, usage.Source{Strategy: usage.StrategyRecords, Spend: replaying}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := usage.NewLedger(usage.Source{Strategy: usage.StrategyLive, Spend: tt.live}, tt.records)

			if !errors.Is(err, usage.ErrSpendUnsupported) {
				t.Errorf("NewLedger(%s) error = %v, want ErrSpendUnsupported", tt.name, err)
			}
		})
	}
}
