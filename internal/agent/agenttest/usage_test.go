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
