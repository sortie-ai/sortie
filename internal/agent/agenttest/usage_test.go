package agenttest

import (
	"errors"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Prefixed so the fixture kinds cannot collide with a real adapter kind.
const (
	usageTestKindIncremental = "agenttest-usage-incremental"
	usageTestKindTurnEnd     = "agenttest-usage-turn-end"
	usageTestKindNone        = "agenttest-usage-none"
	usageTestKindWithRule    = "agenttest-usage-with-rule"
	usageTestKindUndeclared  = "agenttest-usage-undeclared"
)

func usageTestConstructor() (domain.AgentAdapter, error) {
	return nil, errors.New("agenttest: usage fixture kind has no real adapter")
}

func init() {
	registry.Agents.RegisterWithMeta(usageTestKindUndeclared, usageTestConstructor, registry.AgentMeta{})
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

type fakeReporter struct {
	errors []string
}

func (f *fakeReporter) Helper() {}

func (f *fakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

const kindPrefix = "kind %q: "

func usageOf(input, output int64) domain.TokenUsage {
	return domain.TokenUsage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}
}

func requireFailures(t *testing.T, got, wantPrefixes []string) {
	t.Helper()

	if len(got) != len(wantPrefixes) {
		t.Fatalf("recorded failures = %q, want %d failures starting %q", got, len(wantPrefixes), wantPrefixes)
	}
	for i, format := range got {
		if !strings.HasPrefix(format, wantPrefixes[i]) {
			t.Errorf("failure %d = %q, want prefix %q", i, format, wantPrefixes[i])
		}
	}
}

func TestAssertUsageContract_Passing(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventNotification},
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

func TestAssertMeasurementAbsent_Passing(t *testing.T) {
	t.Parallel()

	events := []domain.AgentEvent{
		{Type: domain.EventNotification},
		{Type: domain.EventTurnCompleted},
	}
	result := domain.TurnResult{UsageMeasured: false}

	AssertMeasurementAbsent(t, events, result)
}

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
				Name: "one figure settled after the last tool result, followed by the terminal event",
				Events: []domain.AgentEvent{
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
					{Type: domain.EventTurnCompleted},
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

func TestAssertUsageReporting_EmptyCases(t *testing.T) {
	t.Parallel()

	reporter := &fakeReporter{}
	assertUsageReporting(reporter, usageTestKindIncremental, nil)

	if len(reporter.errors) == 0 {
		t.Error("assertUsageReporting recorded no failures for an empty case slice, want at least one")
	}
}

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

func TestAssertResolvedUsageReporting_TurnEndTerminalOrdering(t *testing.T) {
	t.Parallel()

	usageStream := func() []domain.AgentEvent {
		return []domain.AgentEvent{
			{Type: domain.EventToolResult},
			{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}},
		}
	}
	result := domain.TurnResult{UsageMeasured: true, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}}

	t.Run("no trailing terminal event fails", func(t *testing.T) {
		t.Parallel()

		reporter := &fakeReporter{}
		tc := UsageReportingCase{Name: "no terminal event", Events: usageStream(), Result: result}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal)

		if len(reporter.errors) == 0 {
			t.Error("assertResolvedUsageReporting(turn_end) recorded no failures for a usage event with no trailing terminal event, want at least one")
		}
	})

	t.Run("turn_completed appended passes", func(t *testing.T) {
		t.Parallel()

		reporter := &fakeReporter{}
		events := append(usageStream(), domain.AgentEvent{Type: domain.EventTurnCompleted})
		tc := UsageReportingCase{Name: "terminal event appended", Events: events, Result: result}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal)

		if len(reporter.errors) != 0 {
			t.Errorf("assertResolvedUsageReporting(turn_end) recorded failures %v for a usage event followed by turn_completed, want none", reporter.errors)
		}
	})

	t.Run("zero-payload usage event with no terminal event fails", func(t *testing.T) {
		t.Parallel()

		reporter := &fakeReporter{}
		tc := UsageReportingCase{
			Name: "zero figure, no terminal event",
			Events: []domain.AgentEvent{
				{Type: domain.EventToolResult},
				{Type: domain.EventTokenUsage},
			},
			Result: domain.TurnResult{UsageMeasured: true},
		}
		assertResolvedUsageReporting(reporter, tc, registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal)

		if len(reporter.errors) == 0 {
			t.Error("assertResolvedUsageReporting(turn_end) recorded no failures for a zero-payload usage event with no trailing terminal event, want at least one")
		}
	})
}

func TestAssertLiveUsageCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		kind         string
		tc           UsageReportingCase
		wantFailures []string
	}{
		{
			name: "incremental accepts one model-naming figure and no tool result",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
		},
		{
			name: "turn_end accepts one figure before the terminal event and no tool result",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
		},
		{
			name: "none accepts a turn with no measurement",
			kind: usageTestKindNone,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTurnCompleted}},
			},
		},
		{
			name: "unmeasured turn flagged unaccounted passes",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTurnCompleted}},
				Result: domain.TurnResult{SpendUnaccounted: true},
			},
		},
		{
			name: "local session under a remote rule resolves the declared pair",
			kind: usageTestKindWithRule,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
		},
		{
			name: "remote session resolves the rule's none arrival",
			kind: usageTestKindWithRule,
			tc: UsageReportingCase{
				Remote: true,
				Events: []domain.AgentEvent{{Type: domain.EventTurnCompleted}},
			},
		},
		{
			name:         "none arrival with a measurement",
			kind:         usageTestKindNone,
			tc:           UsageReportingCase{Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)}},
			wantFailures: []string{kindPrefix + "result.UsageMeasured = true"},
		},
		{
			name: "remote session measured under the rule's none arrival",
			kind: usageTestKindWithRule,
			tc: UsageReportingCase{
				Remote: true,
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "result.UsageMeasured = true"},
		},
		{
			name: "zero input tokens",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(0, 3), Model: "gpt-6-astra"}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(0, 3)},
			},
			wantFailures: []string{kindPrefix + "case %q: declared %s, result.Usage = %+v, want positive input and output tokens"},
		},
		{
			name: "zero output tokens",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(5, 0), Model: "gpt-6-astra"}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(5, 0)},
			},
			wantFailures: []string{kindPrefix + "case %q: declared %s, result.Usage = %+v, want positive input and output tokens"},
		},
		{
			name: "usage contract violation in an event",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{
					Type:  domain.EventTokenUsage,
					Usage: domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 99},
					Model: "gpt-6-astra",
				}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "event %d: TotalTokens = %d, want InputTokens+OutputTokens"},
		},
		{
			name: "per_model with no usage-bearing event naming a model",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "case %q: declared per_model, no usage-bearing event named a model"},
		},
		{
			name: "session_total with a usage-bearing event naming a model",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "case %q: event %d: declared session_total, a usage-bearing event named a model"},
		},
		{
			name: "turn_end usage event with no later terminal event",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "case %q: event %d: declared turn_end, the usage event has no later turn-terminal event"},
		},
		{
			name: "turn_end result that does not dominate a reported figure",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(5, 1)},
			},
			wantFailures: []string{kindPrefix + "case %q: event %d: declared turn_end, result.Usage %+v does not dominate"},
		},
		{
			name:         "unmeasured turn not flagged unaccounted",
			kind:         usageTestKindIncremental,
			tc:           UsageReportingCase{Events: []domain.AgentEvent{{Type: domain.EventTurnCompleted}}},
			wantFailures: []string{kindPrefix + "case %q: the turn reported no measurement and did not flag its spend unaccounted"},
		},
		{
			name: "unmeasured turn that leaked a usage event",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage}},
				Result: domain.TurnResult{SpendUnaccounted: true},
			},
			wantFailures: []string{kindPrefix + "event %d: type = %q, want no token_usage event"},
		},
		{
			name:         "kind that declares no usage arrival",
			kind:         usageTestKindUndeclared,
			tc:           UsageReportingCase{Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)}},
			wantFailures: []string{kindPrefix + "case %q: want a kind that declares a usage arrival"},
		},
		{
			name:         "unregistered kind",
			kind:         "agenttest-usage-does-not-exist",
			tc:           UsageReportingCase{},
			wantFailures: []string{"kind %q is not registered"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertLiveUsageCase(reporter, tt.kind, tt.tc)

			requireFailures(t, reporter.errors, tt.wantFailures)
		})
	}
}

func TestAssertLiveUsageCase_ExportedEntryPoint(t *testing.T) {
	t.Parallel()

	conforming := UsageReportingCase{
		Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"}},
		Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
	}
	violating := UsageReportingCase{Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)}}

	AssertLiveUsageCase(t, usageTestKindIncremental, conforming)

	stand := new(testing.T)
	AssertLiveUsageCase(stand, usageTestKindIncremental, violating)
	if !stand.Failed() {
		t.Error("AssertLiveUsageCase(violating case) did not fail its *testing.T, want failure")
	}
}

func TestAssertUsageReportingCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		kind         string
		tc           UsageReportingCase
		wantFailures []string
	}{
		{
			name: "incremental with a figure per model request passes",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"},
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: usageOf(20, 4), Model: "gpt-6-astra"},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(20, 4)},
			},
		},
		{
			name: "turn_end with one figure settled after the last tool result passes",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
		},
		{
			name: "none with no usage reported passes",
			kind: usageTestKindNone,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventToolResult}, {Type: domain.EventTurnCompleted}},
			},
		},
		{
			name: "one local case passes without reaching the session rule",
			kind: usageTestKindWithRule,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: usageOf(5, 1)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(5, 1)},
			},
		},
		{
			name: "one remote case passes without reaching the declared pair",
			kind: usageTestKindWithRule,
			tc: UsageReportingCase{
				Remote: true,
				Events: []domain.AgentEvent{{Type: domain.EventToolResult}, {Type: domain.EventTurnCompleted}},
			},
		},
		{
			name:         "unregistered kind",
			kind:         "agenttest-usage-does-not-exist",
			tc:           UsageReportingCase{},
			wantFailures: []string{"kind %q is not registered"},
		},
		{
			name: "turn_end kind whose stream carries two figures",
			kind: usageTestKindTurnEnd,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2)},
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: usageOf(20, 4)},
					{Type: domain.EventTurnCompleted},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(20, 4)},
			},
			wantFailures: []string{kindPrefix + "case %q: declared turn_end, the turn reported a figure while it was still working"},
		},
		{
			name: "incremental kind whose stream settles one figure after its last tool result",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{
					{Type: domain.EventToolResult},
					{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"},
				},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "case %q: declared incremental, the turn settled one figure after its last tool result"},
		},
		{
			name: "stream with neither a tool result nor a second figure",
			kind: usageTestKindIncremental,
			tc: UsageReportingCase{
				Events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usageOf(10, 2), Model: "gpt-6-astra"}},
				Result: domain.TurnResult{UsageMeasured: true, Usage: usageOf(10, 2)},
			},
			wantFailures: []string{kindPrefix + "case %q: the stream carries neither a tool result nor a second usage event"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertUsageReportingCase(reporter, tt.kind, tt.tc)

			requireFailures(t, reporter.errors, tt.wantFailures)
		})
	}
}

func TestAssertUsageReportingCase_ExportedEntryPoint(t *testing.T) {
	t.Parallel()

	conforming := UsageReportingCase{
		Events: []domain.AgentEvent{{Type: domain.EventToolResult}, {Type: domain.EventTurnCompleted}},
	}
	violating := UsageReportingCase{
		Events: []domain.AgentEvent{{Type: domain.EventToolResult}, {Type: domain.EventTurnCompleted}},
		Result: domain.TurnResult{UsageMeasured: true},
	}

	AssertUsageReportingCase(t, usageTestKindNone, conforming)

	stand := new(testing.T)
	AssertUsageReportingCase(stand, usageTestKindNone, violating)
	if !stand.Failed() {
		t.Error("AssertUsageReportingCase(violating case) did not fail its *testing.T, want failure")
	}
}
