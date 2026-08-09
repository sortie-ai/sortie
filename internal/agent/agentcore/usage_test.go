package agentcore

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestRunUsage_SnapshotZeroBeforeAnyCall(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	got := u.Snapshot()
	if got != (domain.TokenUsage{}) {
		t.Errorf("Snapshot() = %+v, want zero", got)
	}
}

func TestRunUsage_SetTurnProvisional_ReplacesInFlightContribution(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 10, OutputTokens: 5})
	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 12, OutputTokens: 6})

	if got.InputTokens != 12 || got.OutputTokens != 6 {
		t.Errorf("got %+v, want provisional replaced (12, 6)", got)
	}
	if got.TotalTokens != got.InputTokens+got.OutputTokens {
		t.Errorf("TotalTokens = %d, want InputTokens+OutputTokens", got.TotalTokens)
	}
}

func TestRunUsage_AddTurn_AddsToSettledAndClearsProvisional(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 100, OutputTokens: 50})
	got := u.AddTurn(domain.TokenUsage{InputTokens: 100, OutputTokens: 50})

	if got.InputTokens != 100 || got.OutputTokens != 50 {
		t.Errorf("got %+v, want settled (100, 50)", got)
	}

	// Provisional is cleared: the next snapshot reflects only settled.
	next := u.SetTurnProvisional(domain.TokenUsage{})
	if next.InputTokens != 100 || next.OutputTokens != 50 {
		t.Errorf("after AddTurn, provisional not cleared: got %+v", next)
	}
}

func TestRunUsage_ClampsNegativeComponents(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	got := u.SetRunCumulative(domain.TokenUsage{InputTokens: -5, OutputTokens: -10, CacheReadTokens: -3})

	if got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheReadTokens != 0 {
		t.Errorf("got %+v, want all zero (clamped)", got)
	}
}

func TestRunUsage_TotalAlwaysInputPlusOutput(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	got := u.AddTurn(domain.TokenUsage{InputTokens: 30, OutputTokens: 12, TotalTokens: 999})

	if got.TotalTokens != got.InputTokens+got.OutputTokens {
		t.Errorf("TotalTokens = %d, want InputTokens+OutputTokens = %d",
			got.TotalTokens, got.InputTokens+got.OutputTokens)
	}
}

func TestRunUsage_NeverLowersASnapshot(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.AddTurn(domain.TokenUsage{InputTokens: 100, OutputTokens: 50})

	// A later provisional estimate smaller than the settled high-water
	// mark must not lower the reported snapshot.
	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 1, OutputTokens: 1})

	if got.InputTokens < 100 || got.OutputTokens < 50 {
		t.Errorf("Snapshot decreased: got %+v, want componentwise >= (100, 50)", got)
	}
	if snap := u.Snapshot(); snap != got {
		t.Errorf("Snapshot() = %+v, want %+v", snap, got)
	}
}

// --- Exhaustive property coverage (negative clamping, total invariant,
// provisional replace-not-accumulate, settle-and-clear semantics, and
// the componentwise-monotonic snapshot guarantee) ---

func TestRunUsage_ClampsNegativeComponents_AllMethods(t *testing.T) {
	t.Parallel()

	negative := domain.TokenUsage{InputTokens: -5, OutputTokens: -10, CacheReadTokens: -3}

	tests := []struct {
		name string
		call func(u *RunUsage) domain.TokenUsage
	}{
		{"SetTurnProvisional", func(u *RunUsage) domain.TokenUsage { return u.SetTurnProvisional(negative) }},
		{"AddTurn", func(u *RunUsage) domain.TokenUsage { return u.AddTurn(negative) }},
		{"SetRunCumulative", func(u *RunUsage) domain.TokenUsage { return u.SetRunCumulative(negative) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := NewRunUsage()
			got := tt.call(u)
			if got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheReadTokens != 0 {
				t.Errorf("%s(%+v) = %+v, want all components clamped to 0", tt.name, negative, got)
			}
			if got.TotalTokens != 0 {
				t.Errorf("%s(%+v).TotalTokens = %d, want 0", tt.name, negative, got.TotalTokens)
			}
		})
	}
}

func TestRunUsage_TotalAlwaysInputPlusOutput_AllMethods(t *testing.T) {
	t.Parallel()

	// TotalTokens is deliberately set to a bogus value on the input to
	// prove every method recomputes it rather than trusting a
	// caller-supplied total.
	in := domain.TokenUsage{InputTokens: 30, OutputTokens: 12, TotalTokens: 999}

	tests := []struct {
		name string
		call func(u *RunUsage) domain.TokenUsage
	}{
		{"SetTurnProvisional", func(u *RunUsage) domain.TokenUsage { return u.SetTurnProvisional(in) }},
		{"AddTurn", func(u *RunUsage) domain.TokenUsage { return u.AddTurn(in) }},
		{"SetRunCumulative", func(u *RunUsage) domain.TokenUsage { return u.SetRunCumulative(in) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.call(NewRunUsage())
			if got.TotalTokens != got.InputTokens+got.OutputTokens {
				t.Errorf("%s(%+v).TotalTokens = %d, want InputTokens+OutputTokens = %d",
					tt.name, in, got.TotalTokens, got.InputTokens+got.OutputTokens)
			}
		})
	}
}

// TestRunUsage_SetTurnProvisional_DoesNotAccumulate verifies that three
// successive SetTurnProvisional calls each replace the in-flight
// contribution rather than summing it: the third call's snapshot must
// equal its own argument (settled is still zero), not the sum of all
// three arguments.
func TestRunUsage_SetTurnProvisional_DoesNotAccumulate(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 10, OutputTokens: 5})
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 20, OutputTokens: 10})
	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 30, OutputTokens: 15})

	want := domain.TokenUsage{InputTokens: 30, OutputTokens: 15, TotalTokens: 45}
	if got != want {
		t.Errorf("third SetTurnProvisional() = %+v, want %+v (replaced, not accumulated)", got, want)
	}
}

// TestRunUsage_AddTurn_AccumulatesAcrossMultipleTurns verifies that
// AddTurn adds each argument to the running settled total rather than
// replacing it.
func TestRunUsage_AddTurn_AccumulatesAcrossMultipleTurns(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.AddTurn(domain.TokenUsage{InputTokens: 100, OutputTokens: 50})
	u.AddTurn(domain.TokenUsage{InputTokens: 30, OutputTokens: 10})
	got := u.AddTurn(domain.TokenUsage{InputTokens: 5, OutputTokens: 2})

	want := domain.TokenUsage{InputTokens: 135, OutputTokens: 62, TotalTokens: 197}
	if got != want {
		t.Errorf("AddTurn() after three calls = %+v, want %+v (settled total)", got, want)
	}
}

// TestRunUsage_SetRunCumulative_ReplacesSettledAndClearsProvisional
// verifies both halves of R10's SetRunCumulative contract: it replaces
// the settled total outright (an earlier AddTurn contribution does not
// survive), and it clears any in-flight provisional contribution (a
// subsequent SetTurnProvisional(zero) reports only the replaced total).
func TestRunUsage_SetRunCumulative_ReplacesSettledAndClearsProvisional(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 9, OutputTokens: 9})

	got := u.SetRunCumulative(domain.TokenUsage{InputTokens: 200, OutputTokens: 80, CacheReadTokens: 5})
	want := domain.TokenUsage{InputTokens: 200, OutputTokens: 80, CacheReadTokens: 5, TotalTokens: 280}
	if got != want {
		t.Errorf("SetRunCumulative() = %+v, want %+v (replaces settled outright)", got, want)
	}

	// The stale provisional contribution from before SetRunCumulative must
	// not resurface: a zero provisional now reports exactly the replaced
	// total.
	next := u.SetTurnProvisional(domain.TokenUsage{})
	if next != want {
		t.Errorf("after SetRunCumulative, SetTurnProvisional(zero) = %+v, want %+v (provisional cleared)", next, want)
	}
}

// TestRunUsage_AddTurn_ClearsProvisional mirrors the
// SetRunCumulative case for AddTurn: a provisional contribution set
// before AddTurn must not survive it.
func TestRunUsage_AddTurn_ClearsProvisional(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 40, OutputTokens: 40})
	got := u.AddTurn(domain.TokenUsage{InputTokens: 100, OutputTokens: 50})

	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	if got != want {
		t.Errorf("AddTurn() after provisional set = %+v, want %+v (provisional discarded, not added)", got, want)
	}
}

// TestRunUsage_MonotonicSnapshot_AcrossMixedCallSequence drives every
// method in a sequence engineered to produce a componentwise-lower raw
// value at several points, checking after each call that the returned
// snapshot equals Snapshot() at that instant and is componentwise
// greater than or equal to the immediately preceding snapshot. The
// final snapshot must equal the componentwise maximum observed across
// the whole sequence: both components peak when SetTurnProvisional(300,
// 5) is added to the settled total left by the prior SetRunCumulative
// (50, 200), reporting (350, 205).
func TestRunUsage_MonotonicSnapshot_AcrossMixedCallSequence(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	var prev domain.TokenUsage

	checkStep := func(t *testing.T, label string, got domain.TokenUsage) {
		t.Helper()
		if got.InputTokens < prev.InputTokens || got.OutputTokens < prev.OutputTokens || got.CacheReadTokens < prev.CacheReadTokens {
			t.Errorf("%s: snapshot %+v is componentwise lower than the prior snapshot %+v", label, got, prev)
		}
		if snap := u.Snapshot(); got != snap {
			t.Errorf("%s: returned snapshot %+v != Snapshot() %+v", label, got, snap)
		}
		prev = got
	}

	checkStep(t, "AddTurn(100,50)", u.AddTurn(domain.TokenUsage{InputTokens: 100, OutputTokens: 50}))
	checkStep(t, "SetTurnProvisional(1,1) lower", u.SetTurnProvisional(domain.TokenUsage{InputTokens: 1, OutputTokens: 1}))
	checkStep(t, "SetRunCumulative(50,200) mixed", u.SetRunCumulative(domain.TokenUsage{InputTokens: 50, OutputTokens: 200}))
	checkStep(t, "SetTurnProvisional(300,5) mixed", u.SetTurnProvisional(domain.TokenUsage{InputTokens: 300, OutputTokens: 5}))
	checkStep(t, "AddTurn(0,0) no-op delta", u.AddTurn(domain.TokenUsage{}))

	want := domain.TokenUsage{InputTokens: 350, OutputTokens: 205}
	want.TotalTokens = want.InputTokens + want.OutputTokens
	if final := u.Snapshot(); final != want {
		t.Errorf("final Snapshot() = %+v, want %+v (componentwise max across the sequence)", final, want)
	}
}
