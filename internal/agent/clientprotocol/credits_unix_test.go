//go:build unix

package clientprotocol

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func TestCreditsRecordedPerTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		capture string
		resume  string
		turns   []float64
	}{
		{"default engine, completed turns", "kiro_v2_completed_turns.jsonl", "", []float64{0.01611050912106136, 0.016239179104477617}},
		{"v3 engine, session load", "kiro_v3_session_load.jsonl", "sess_9c5c5d89-7f78-4af5-a82f-d5472adc1f55", []float64{0.023755560530679933}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, err := NewClientProtocolAdapter()
			if err != nil {
				t.Fatalf("NewClientProtocolAdapter() error = %v", err)
			}
			start := replayStart(t, "testdata/credits", replayCase{Capture: tt.capture, Command: "kiro-cli acp"})
			start.ResumeSessionID = tt.resume
			ctx := waittest.CleanupContext(t)
			session, err := adapter.StartSession(ctx, start)
			if err != nil {
				t.Fatalf("StartSession(%s) error = %v", tt.capture, err)
			}
			t.Cleanup(func() { _ = adapter.StopSession(ctx, session) })

			var total float64
			for i, amount := range tt.turns {
				total += amount
				got, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
				if err != nil {
					t.Fatalf("RunTurn(%s) turn %d error = %v", tt.capture, i+1, err)
				}

				want := []domain.ReportedAmount{{Unit: "credits", Amount: total}}
				if !slices.Equal(got.ReportedAmounts, want) {
					t.Errorf("RunTurn(%s) turn %d ReportedAmounts = %v, want %v", tt.capture, i+1, got.ReportedAmounts, want)
				}
				if got.UsageMeasured || got.Usage != (domain.TokenUsage{}) || len(got.UsageProvenance) != 0 {
					t.Errorf("RunTurn(%s) turn %d token figure = (%v, %v, %v), want none", tt.capture, i+1, got.UsageMeasured, got.Usage, got.UsageProvenance)
				}
			}
		})
	}
}
