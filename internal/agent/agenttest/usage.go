package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// AssertUsageContract fails t if events, taken in order, violates the
// normalized token-usage contract: every component non-negative,
// TotalTokens equal to InputTokens plus OutputTokens, CacheReadTokens
// no greater than InputTokens, and every component monotonically
// non-decreasing across the sequence of events that carry a non-zero
// Usage.
func AssertUsageContract(t *testing.T, events []domain.AgentEvent) {
	t.Helper()
	assertUsageContract(t, events)
}

// usageContractReporter is the minimal reporting surface
// assertUsageContract needs; [*testing.T] satisfies it. Splitting the
// check out from [AssertUsageContract] lets a package-internal test
// drive the same failure-detection logic against a lightweight double,
// since a *testing.T's own failure state cannot itself be inspected
// without failing the enclosing test.
type usageContractReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func assertUsageContract(t usageContractReporter, events []domain.AgentEvent) {
	t.Helper()

	var prev domain.TokenUsage
	for i, event := range events {
		usage := event.Usage
		if usage == (domain.TokenUsage{}) {
			continue
		}

		if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 || usage.TotalTokens < 0 {
			t.Errorf("event %d: Usage has a negative component: %+v", i, usage)
		}
		if usage.TotalTokens != usage.InputTokens+usage.OutputTokens {
			t.Errorf("event %d: TotalTokens = %d, want InputTokens+OutputTokens = %d",
				i, usage.TotalTokens, usage.InputTokens+usage.OutputTokens)
		}
		if usage.CacheReadTokens > usage.InputTokens {
			t.Errorf("event %d: CacheReadTokens = %d, want <= InputTokens (%d)",
				i, usage.CacheReadTokens, usage.InputTokens)
		}
		if usage.InputTokens < prev.InputTokens {
			t.Errorf("event %d: InputTokens decreased from %d to %d", i, prev.InputTokens, usage.InputTokens)
		}
		if usage.OutputTokens < prev.OutputTokens {
			t.Errorf("event %d: OutputTokens decreased from %d to %d", i, prev.OutputTokens, usage.OutputTokens)
		}
		if usage.TotalTokens < prev.TotalTokens {
			t.Errorf("event %d: TotalTokens decreased from %d to %d", i, prev.TotalTokens, usage.TotalTokens)
		}
		if usage.CacheReadTokens < prev.CacheReadTokens {
			t.Errorf("event %d: CacheReadTokens decreased from %d to %d", i, prev.CacheReadTokens, usage.CacheReadTokens)
		}
		prev = usage
	}
}
