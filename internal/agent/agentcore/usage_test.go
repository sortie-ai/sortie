package agentcore

import (
	"math/rand/v2"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestUsageAccumulator_SnapshotZeroBeforeAnyCall(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	got := acc.Snapshot()
	if got != (domain.TokenUsage{}) {
		t.Errorf("Snapshot() = %+v, want zero", got)
	}
}

func TestUsageAccumulator_AddDelta_ReadyWhenOutputNonZero(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	snap, ready := acc.AddDelta(10, 5, 0)
	if !ready {
		t.Error("ready = false, want true when output > 0")
	}
	if snap.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", snap.OutputTokens)
	}
}

func TestUsageAccumulator_AddDelta_NotReadyWhenOutputZero(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	_, ready := acc.AddDelta(10, 0, 3)
	if ready {
		t.Error("ready = true, want false when output == 0")
	}
}

func TestUsageAccumulator_AddDelta_AccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	acc.AddDelta(10, 5, 2)
	snap, _ := acc.AddDelta(20, 10, 3)

	if snap.InputTokens != 30 {
		t.Errorf("InputTokens = %d, want 30", snap.InputTokens)
	}
	if snap.OutputTokens != 15 {
		t.Errorf("OutputTokens = %d, want 15", snap.OutputTokens)
	}
	if snap.CacheReadTokens != 5 {
		t.Errorf("CacheReadTokens = %d, want 5", snap.CacheReadTokens)
	}
	if snap.TotalTokens != 45 {
		t.Errorf("TotalTokens = %d, want 45 (InputTokens+OutputTokens)", snap.TotalTokens)
	}
}

func TestUsageAccumulator_AddDelta_ClampsNegatives(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	snap, _ := acc.AddDelta(-5, -10, -3)

	if snap.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 (clamped)", snap.InputTokens)
	}
	if snap.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0 (clamped)", snap.OutputTokens)
	}
	if snap.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", snap.TotalTokens)
	}
}

func TestUsageAccumulator_ReplaceCumulative_Overwrites(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	acc.AddDelta(100, 50, 10)

	in := domain.TokenUsage{
		InputTokens:     200,
		OutputTokens:    80,
		CacheReadTokens: 5,
		TotalTokens:     280,
	}
	got := acc.ReplaceCumulative(in)

	if got != in {
		t.Errorf("ReplaceCumulative() = %+v, want %+v", got, in)
	}
	if snap := acc.Snapshot(); snap != in {
		t.Errorf("Snapshot() after ReplaceCumulative = %+v, want %+v", snap, in)
	}
}

func TestUsageAccumulator_AddDelta_AfterReplaceCumulative(t *testing.T) {
	t.Parallel()

	acc := NewUsageAccumulator()
	acc.ReplaceCumulative(domain.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	})
	snap, _ := acc.AddDelta(10, 5, 0)

	if snap.InputTokens != 110 {
		t.Errorf("InputTokens = %d, want 110", snap.InputTokens)
	}
	if snap.TotalTokens != snap.InputTokens+snap.OutputTokens {
		t.Errorf("TotalTokens = %d, want InputTokens+OutputTokens = %d",
			snap.TotalTokens, snap.InputTokens+snap.OutputTokens)
	}
}

func TestUsageAccumulator_Property_TotalAlwaysInputPlusOutput(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(12345, 67890))
	acc := NewUsageAccumulator()

	var prevInput, prevOutput, prevCache int64

	for i := range 100 {
		input := rng.Int64N(500)
		output := rng.Int64N(500)
		cache := rng.Int64N(100)

		snap, _ := acc.AddDelta(input, output, cache)

		if snap.TotalTokens != snap.InputTokens+snap.OutputTokens {
			t.Errorf("iteration %d: TotalTokens = %d, want InputTokens(%d)+OutputTokens(%d)=%d",
				i, snap.TotalTokens, snap.InputTokens, snap.OutputTokens,
				snap.InputTokens+snap.OutputTokens)
		}
		if snap.InputTokens < prevInput {
			t.Errorf("iteration %d: InputTokens decreased from %d to %d",
				i, prevInput, snap.InputTokens)
		}
		if snap.OutputTokens < prevOutput {
			t.Errorf("iteration %d: OutputTokens decreased from %d to %d",
				i, prevOutput, snap.OutputTokens)
		}
		if snap.CacheReadTokens < prevCache {
			t.Errorf("iteration %d: CacheReadTokens decreased from %d to %d",
				i, prevCache, snap.CacheReadTokens)
		}
		prevInput = snap.InputTokens
		prevOutput = snap.OutputTokens
		prevCache = snap.CacheReadTokens
	}
}
