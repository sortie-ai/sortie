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

// AssertMeasurementAbsent fails t when the given events or result assert a
// usage measurement that a runtime reporting nothing must not produce: any
// event of type [domain.EventTokenUsage], any event carrying a non-zero
// Usage, or a true result.UsageMeasured.
func AssertMeasurementAbsent(t *testing.T, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()
	assertMeasurementAbsent(t, events, result)
}

func assertMeasurementAbsent(t usageContractReporter, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()

	for i, event := range events {
		if event.Type == domain.EventTokenUsage {
			t.Errorf("event %d: type = %q, want no token_usage event when the runtime reported nothing", i, event.Type)
		}
		if event.Usage != (domain.TokenUsage{}) {
			t.Errorf("event %d: Usage = %+v, want zero value when the runtime reported nothing", i, event.Usage)
		}
	}
	if result.UsageMeasured {
		t.Errorf("result.UsageMeasured = true, want false when the runtime reported nothing")
	}
}

// AssertModelReported fails t when events contains no event of type
// [domain.EventTokenUsage], or when any such event carries a Model
// other than wantModel. Passing the empty string as wantModel asserts
// that every emitted token_usage event reports no model.
func AssertModelReported(t *testing.T, events []domain.AgentEvent, wantModel string) {
	t.Helper()
	assertModelReported(t, events, wantModel)
}

func assertModelReported(t usageContractReporter, events []domain.AgentEvent, wantModel string) {
	t.Helper()

	seen := false
	for i, event := range events {
		if event.Type != domain.EventTokenUsage {
			continue
		}
		seen = true
		if event.Model != wantModel {
			t.Errorf("event %d: Model = %q, want %q", i, event.Model, wantModel)
		}
	}
	if !seen {
		t.Errorf("events contain no token_usage event")
	}
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
