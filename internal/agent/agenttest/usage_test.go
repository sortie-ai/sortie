package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fakeReporter is a minimal [usageContractReporter] double that records
// Errorf calls instead of failing the enclosing test, so a
// deliberately-violating input can be driven through
// assertUsageContract without reddening the smoke test itself.
type fakeReporter struct {
	errors []string
}

func (f *fakeReporter) Helper() {}

func (f *fakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

// TestAssertUsageContract_Passing exercises the exported
// AssertUsageContract entry point against a real *testing.T with a
// sequence of events whose usage components are non-negative,
// internally consistent (TotalTokens == InputTokens + OutputTokens,
// CacheReadTokens <= InputTokens), and componentwise non-decreasing
// across the run. It must report no failures.
func TestAssertUsageContract_Passing(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventNotification}, // zero Usage, ignored by the contract
		{
			Type: domain.EventTokenUsage,
			Usage: domain.TokenUsage{
				InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10, TotalTokens: 120,
			},
		},
		{
			Type: domain.EventTurnCompleted,
			Usage: domain.TokenUsage{
				InputTokens: 150, OutputTokens: 40, CacheReadTokens: 10, TotalTokens: 190,
			},
		},
	}

	AssertUsageContract(t, events)
}

// TestAssertUsageContract_Violating drives the same checking logic via
// assertUsageContract and a fakeReporter, over a sequence whose second
// event lowers TotalTokens relative to the first. It asserts at least
// one Errorf call was recorded, proving the helper actually inspects
// its input rather than trivially passing.
func TestAssertUsageContract_Violating(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{
			Type: domain.EventTokenUsage,
			Usage: domain.TokenUsage{
				InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			},
		},
		{
			Type: domain.EventTurnCompleted,
			Usage: domain.TokenUsage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			},
		},
	}

	reporter := &fakeReporter{}
	assertUsageContract(reporter, events)

	if len(reporter.errors) == 0 {
		t.Error("assertUsageContract recorded no failures on a monotonicity-violating sequence, want at least one")
	}
}

// TestAssertMeasurementAbsent_Passing exercises the exported
// AssertMeasurementAbsent entry point against a real *testing.T with an
// event slice carrying no token_usage event and no non-zero Usage, and a
// result whose UsageMeasured is false. It must report no failures.
func TestAssertMeasurementAbsent_Passing(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventNotification},
		{Type: domain.EventTurnCompleted},
	}
	result := domain.TurnResult{UsageMeasured: false}

	AssertMeasurementAbsent(t, events, result)
}

// TestAssertMeasurementAbsent_Violating drives assertMeasurementAbsent
// against a fakeReporter for each of the three ways a runtime that
// reported nothing must not assert a measurement: a token_usage event, a
// non-zero Usage on a differently typed event, and a true
// result.UsageMeasured. Each case must record at least one failure.
func TestAssertMeasurementAbsent_Violating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []domain.AgentEvent
		result domain.TurnResult
	}{
		{
			name:   "token_usage event present",
			events: []domain.AgentEvent{{Type: domain.EventTokenUsage}},
			result: domain.TurnResult{},
		},
		{
			name: "non-zero usage on a non-token_usage event",
			events: []domain.AgentEvent{
				{Type: domain.EventNotification, Usage: domain.TokenUsage{TotalTokens: 5, OutputTokens: 5}},
			},
			result: domain.TurnResult{},
		},
		{
			name:   "UsageMeasured true with no events",
			events: nil,
			result: domain.TurnResult{UsageMeasured: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertMeasurementAbsent(reporter, tt.events, tt.result)

			if len(reporter.errors) == 0 {
				t.Errorf("assertMeasurementAbsent(%s) recorded no failures, want at least one", tt.name)
			}
		})
	}
}

// TestAssertModelReported_Passing exercises the exported
// AssertModelReported entry point against a real *testing.T with a
// sequence of token_usage events that all carry the wanted model. It
// must report no failures.
func TestAssertModelReported_Passing(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventNotification},
		{Type: domain.EventTokenUsage, Model: "claude-sonnet-5"},
		{Type: domain.EventTurnCompleted},
		{Type: domain.EventTokenUsage, Model: "claude-sonnet-5"},
	}

	AssertModelReported(t, events, "claude-sonnet-5")
}

// TestAssertModelReported_Violating drives assertModelReported against
// a fakeReporter for each way a slice can fail to report the wanted
// model: no token_usage event at all, a token_usage event carrying the
// wrong model, and a token_usage event carrying a non-empty model when
// wantModel is the empty string. Each case must record at least one
// failure.
func TestAssertModelReported_Violating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		events    []domain.AgentEvent
		wantModel string
	}{
		{
			name:      "no token_usage event present",
			events:    []domain.AgentEvent{{Type: domain.EventTurnCompleted}},
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "token_usage event carries the wrong model",
			events:    []domain.AgentEvent{{Type: domain.EventTokenUsage, Model: "gpt-5.6-sol"}},
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "token_usage event carries a model when wantModel is empty",
			events:    []domain.AgentEvent{{Type: domain.EventTokenUsage, Model: "claude-sonnet-5"}},
			wantModel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertModelReported(reporter, tt.events, tt.wantModel)

			if len(reporter.errors) == 0 {
				t.Errorf("assertModelReported(%s) recorded no failures, want at least one", tt.name)
			}
		})
	}
}
