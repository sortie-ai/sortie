//go:build unix

package clientprotocol

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func TestCreditsRecordedPerTurn(t *testing.T) {
	t.Parallel()

	var tests []struct {
		replayCase
		Resume string
		Turns  []float64
	}
	if err := json.Unmarshal(waittest.File(t, "testdata/credits/credit_cases.json"), &tests); err != nil {
		t.Fatalf("credit cases: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			adapter, err := NewClientProtocolAdapter()
			if err != nil {
				t.Fatalf("NewClientProtocolAdapter() error = %v", err)
			}
			start := replayStart(t, "testdata/credits", tt.replayCase)
			start.ResumeSessionID = tt.Resume
			ctx := waittest.CleanupContext(t)
			session, err := adapter.StartSession(ctx, start)
			if err != nil {
				t.Fatalf("StartSession(%s) error = %v", tt.Capture, err)
			}
			t.Cleanup(func() { _ = adapter.StopSession(ctx, session) })

			var total float64
			for i, amount := range tt.Turns {
				total += amount
				got, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
				if err != nil {
					t.Fatalf("RunTurn(%s) turn %d error = %v", tt.Capture, i+1, err)
				}

				want := []domain.ReportedAmount{{Unit: "credits", Amount: total}}
				if !slices.Equal(got.ReportedAmounts, want) {
					t.Errorf("RunTurn(%s) turn %d ReportedAmounts = %v, want %v", tt.Capture, i+1, got.ReportedAmounts, want)
				}
				if got.UsageMeasured || got.Usage != (domain.TokenUsage{}) || len(got.UsageProvenance) != 0 {
					t.Errorf("RunTurn(%s) turn %d token figure = (%v, %v, %v), want none", tt.Capture, i+1, got.UsageMeasured, got.Usage, got.UsageProvenance)
				}
			}
		})
	}
}
