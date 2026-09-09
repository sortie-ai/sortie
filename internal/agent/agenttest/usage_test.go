package agenttest

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Fixture kinds registered once so assertUsageReporting's tests can
// resolve a real registry.AgentMeta through registry.Agents.Meta,
// mirroring how a production kind package registers itself in init().
// These names are prefixed to avoid colliding with a real adapter kind.
const (
	usageTestKindIncremental = "agenttest-usage-incremental"
	usageTestKindTurnEnd     = "agenttest-usage-turn-end"
	usageTestKindNone        = "agenttest-usage-none"
	usageTestKindWithRule    = "agenttest-usage-with-rule"
)

func usageTestConstructor(map[string]any) (domain.AgentAdapter, error) {
	return nil, errors.New("agenttest: usage fixture kind has no real adapter")
}

func init() {
	registry.Agents.RegisterWithMeta(usageTestKindIncremental, usageTestConstructor, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionPerModel,
	})
	registry.Agents.RegisterWithMeta(usageTestKindTurnEnd, usageTestConstructor, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalTurnEnd,
		UsageAttribution: registry.UsageAttributionSessionTotal,
	})
	registry.Agents.RegisterWithMeta(usageTestKindNone, usageTestConstructor, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalNone,
		UsageAttribution: registry.UsageAttributionNone,
	})
	registry.Agents.RegisterWithMeta(usageTestKindWithRule, usageTestConstructor, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalTurnEnd,
		UsageAttribution: registry.UsageAttributionSessionTotal,
		UsageSessionRules: []registry.UsageSessionRule{
			{
				When:        func(passthrough map[string]any, remote bool) bool { return remote },
				Arrival:     registry.UsageArrivalNone,
				Attribution: registry.UsageAttributionNone,
			},
		},
	})
}

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

// TestAssertUsageReporting_Passing exercises the exported
// AssertUsageReporting entry point against a real *testing.T for each
// of the three arrival dispositions with a stream matching the
// witness table for that disposition. It must report no failures.
func TestAssertUsageReporting_Passing(t *testing.T) {
	t.Parallel()

	t.Run("incremental", func(t *testing.T) {
		t.Parallel()
		cases := []UsageReportingCase{
			{
				Name: "two figures, one per model API request",
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}, Model: "gpt-6-astra"},
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}, Model: "gpt-6-astra"},
				},
				Result: domain.TurnResult{UsageMeasured: true},
			},
		}
		AssertUsageReporting(t, usageTestKindIncremental, cases)
	})

	t.Run("turn_end", func(t *testing.T) {
		t.Parallel()
		cases := []UsageReportingCase{
			{
				Name: "one figure settled after the last tool result",
				Events: []domain.AgentEvent{
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
			},
		}
		AssertUsageReporting(t, usageTestKindTurnEnd, cases)
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		cases := []UsageReportingCase{
			{
				Name:   "no usage reported",
				Events: []domain.AgentEvent{{Type: domain.EventToolResult}, {Type: domain.EventTurnCompleted}},
				Result: domain.TurnResult{UsageMeasured: false},
			},
		}
		AssertUsageReporting(t, usageTestKindNone, cases)
	})
}

// TestAssertUsageReporting_UnregisteredKind proves the
// unregistered-kind arm: assertUsageReporting fails naming the kind
// when registry.Agents.Meta reports it unregistered.
func TestAssertUsageReporting_UnregisteredKind(t *testing.T) {
	t.Parallel()

	reporter := &fakeReporter{}
	assertUsageReporting(reporter, "agenttest-usage-does-not-exist", []UsageReportingCase{
		{Name: "irrelevant", Result: domain.TurnResult{}},
	})

	if len(reporter.errors) == 0 {
		t.Error("assertUsageReporting recorded no failures for an unregistered kind, want at least one")
	}
}

// TestAssertUsageReporting_EmptyCases proves assertUsageReporting
// fails when cases is empty, per the assertion's input validation.
func TestAssertUsageReporting_EmptyCases(t *testing.T) {
	t.Parallel()

	reporter := &fakeReporter{}
	assertUsageReporting(reporter, usageTestKindIncremental, nil)

	if len(reporter.errors) == 0 {
		t.Error("assertUsageReporting recorded no failures for an empty case slice, want at least one")
	}
}

// TestAssertUsageReporting_RuleCoverageArm proves the rule-coverage
// arm: a kind with one UsageSessionRules entry whose cases never
// exercise it (every case is local) is reported by rule index.
func TestAssertUsageReporting_RuleCoverageArm(t *testing.T) {
	t.Parallel()

	reporter := &fakeReporter{}
	assertUsageReporting(reporter, usageTestKindWithRule, []UsageReportingCase{
		{
			Name:   "local only, never triggers the remote rule",
			Remote: false,
			Events: []domain.AgentEvent{
				{Type: domain.EventToolResult},
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}},
			},
			Result: domain.TurnResult{UsageMeasured: true, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}},
		},
	})

	if len(reporter.errors) == 0 {
		t.Error("assertUsageReporting recorded no failures for an unreached rule, want at least one")
	}
}

// TestAssertResolvedUsageReporting_AdmissibilityGuard proves the
// admissibility guard: a stream with one usage figure and no tool
// result cannot tell an incremental disposition from a turn_end one,
// so both arms reject it.
func TestAssertResolvedUsageReporting_AdmissibilityGuard(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}},
	}
	result := domain.TurnResult{UsageMeasured: true, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}}

	for _, arrival := range []registry.UsageArrival{registry.UsageArrivalIncremental, registry.UsageArrivalTurnEnd} {
		t.Run(string(arrival), func(t *testing.T) {
			t.Parallel()
			reporter := &fakeReporter{}
			tc := UsageReportingCase{Name: "inadmissible", Events: events, Result: result}
			assertResolvedUsageReporting(reporter, tc, arrival, registry.UsageAttributionSessionTotal)

			if len(reporter.errors) == 0 {
				t.Errorf("assertResolvedUsageReporting(%s) recorded no failures for an inadmissible stream, want at least one", arrival)
			}
		})
	}
}

// TestAssertResolvedUsageReporting_IncrementalShapedFailsTurnEndArm
// proves that an incremental-shaped stream fails the TurnEnd arm both
// ways a stream can be incremental-shaped: a second figure, and a
// figure preceding the last tool result.
func TestAssertResolvedUsageReporting_IncrementalShapedFailsTurnEndArm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []domain.AgentEvent
	}{
		{
			name: "two figures",
			events: []domain.AgentEvent{
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 2, OutputTokens: 2, TotalTokens: 4}},
			},
		},
		{
			name: "a figure preceding the last tool result",
			events: []domain.AgentEvent{
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
				{Type: domain.EventToolResult},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			tc := UsageReportingCase{
				Name:   tt.name,
				Events: tt.events,
				Result: domain.TurnResult{UsageMeasured: true},
			}
			assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal)

			if len(reporter.errors) == 0 {
				t.Errorf("assertResolvedUsageReporting(turn_end, %s) recorded no failures, want at least one", tt.name)
			}
		})
	}
}

// TestAssertResolvedUsageReporting_TurnEndShapedFailsIncrementalArm
// proves that a turn-end-shaped stream (one figure settled after the
// last tool result) fails the Incremental arm.
func TestAssertResolvedUsageReporting_TurnEndShapedFailsIncrementalArm(t *testing.T) {
	t.Parallel()

	reporter := &fakeReporter{}
	tc := UsageReportingCase{
		Name: "one figure after the last tool result",
		Events: []domain.AgentEvent{
			{Type: domain.EventToolResult},
			{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, Model: "m"},
		},
		Result: domain.TurnResult{UsageMeasured: true},
	}
	assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalIncremental, registry.UsageAttributionPerModel)

	if len(reporter.errors) == 0 {
		t.Error("assertResolvedUsageReporting(incremental) recorded no failures for a turn-end-shaped stream, want at least one")
	}
}

// TestAssertResolvedUsageReporting_AttributionArms proves the
// PerModel, SessionTotal, and None attribution arms each fail on a
// violating event set.
func TestAssertResolvedUsageReporting_AttributionArms(t *testing.T) {
	t.Parallel()

	t.Run("per_model with no event naming a model", func(t *testing.T) {
		t.Parallel()
		reporter := &fakeReporter{}
		tc := UsageReportingCase{
			Name: "no model named",
			Events: []domain.AgentEvent{
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 2, OutputTokens: 2, TotalTokens: 4}},
			},
			Result: domain.TurnResult{UsageMeasured: true},
		}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalIncremental, registry.UsageAttributionPerModel)

		if len(reporter.errors) == 0 {
			t.Error("assertResolvedUsageReporting(per_model) recorded no failures, want at least one")
		}
	})

	t.Run("session_total with a usage-bearing event naming a model", func(t *testing.T) {
		t.Parallel()
		reporter := &fakeReporter{}
		tc := UsageReportingCase{
			Name: "model named",
			Events: []domain.AgentEvent{
				{Type: domain.EventToolResult},
				{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, Model: "m"},
			},
			Result: domain.TurnResult{UsageMeasured: true, Usage: domain.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
		}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal)

		if len(reporter.errors) == 0 {
			t.Error("assertResolvedUsageReporting(session_total) recorded no failures, want at least one")
		}
	})

	t.Run("none attribution paired with a non-none arrival", func(t *testing.T) {
		t.Parallel()
		reporter := &fakeReporter{}
		tc := UsageReportingCase{
			Name:   "INV-1 violation",
			Events: []domain.AgentEvent{{Type: domain.EventToolResult}},
			Result: domain.TurnResult{UsageMeasured: false},
		}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionNone)

		if len(reporter.errors) == 0 {
			t.Error("assertResolvedUsageReporting(none attribution, turn_end arrival) recorded no failures, want at least one")
		}
	})
}
