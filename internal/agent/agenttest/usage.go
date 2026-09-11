package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
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

// UsageReportingCase is one turn of one kind, captured under the
// passthrough and launch mode the fixture was built with.
type UsageReportingCase struct {
	Name        string
	Passthrough map[string]any
	Remote      bool
	Events      []domain.AgentEvent
	Result      domain.TurnResult
}

// AssertUsageReporting fails t unless every case agrees with the pair
// kind resolves for it, and unless the cases together reach the
// declared pair and every entry in [registry.AgentMeta.UsageSessionRules].
// It resolves kind's registered [registry.AgentMeta] through
// [registry.Agents.Meta] itself, so a caller cannot hand it a pair the
// kind did not register.
func AssertUsageReporting(t *testing.T, kind string, cases []UsageReportingCase) {
	t.Helper()
	assertUsageReporting(t, kind, cases)
}

// unreachedRuleIndex is the sentinel reached-set member standing for
// the declared pair: a case reaches it when no UsageSessionRules
// entry's When matched.
const unreachedRuleIndex = -1

func assertUsageReporting(t usageContractReporter, kind string, cases []UsageReportingCase) {
	t.Helper()

	meta, registered := registry.Agents.Meta(kind)
	if !registered {
		t.Errorf("kind %q is not registered", kind)
		return
	}
	if len(cases) == 0 {
		t.Errorf("want at least one case, got none")
		return
	}

	reached := make(map[int]bool)
	for _, tc := range cases {
		ruleIndex := unreachedRuleIndex
		for i, rule := range meta.UsageSessionRules {
			if rule.When != nil && rule.When(tc.Passthrough, tc.Remote) {
				ruleIndex = i
				break
			}
		}
		reached[ruleIndex] = true

		arrival, attribution := meta.UsageDisposition(tc.Passthrough, tc.Remote)
		assertResolvedUsageReporting(t, tc, arrival, attribution)
	}

	for i := unreachedRuleIndex; i < len(meta.UsageSessionRules); i++ {
		if !reached[i] {
			if i == unreachedRuleIndex {
				t.Errorf("no case reaches the declared pair (kind %q)", kind)
				continue
			}
			t.Errorf("no case reaches UsageSessionRules[%d] (kind %q)", i, kind)
		}
	}
}

// assertResolvedUsageReporting checks one case's events and result
// against the pair meta.UsageDisposition resolved for it, per the two
// admissible shapes UsageArrivalIncremental and UsageArrivalTurnEnd
// are exact complements of on a stream that can tell them apart.
func assertResolvedUsageReporting(t usageContractReporter, tc UsageReportingCase, arrival registry.UsageArrival, attribution registry.UsageAttribution) {
	t.Helper()

	var usageIdx []int
	lastToolResult := -1
	for i, event := range tc.Events {
		if event.Type == domain.EventTokenUsage {
			usageIdx = append(usageIdx, i)
		}
		if event.Type == domain.EventToolResult {
			lastToolResult = i
		}
	}
	turnEndShaped := len(usageIdx) <= 1 && (len(usageIdx) == 0 || usageIdx[0] > lastToolResult)

	switch arrival {
	case registry.UsageArrivalUndeclared:
		t.Errorf("case %q: want a kind that declares a usage arrival", tc.Name)
	case registry.UsageArrivalNone:
		assertMeasurementAbsent(t, tc.Events, tc.Result)
	case registry.UsageArrivalIncremental, registry.UsageArrivalTurnEnd:
		if lastToolResult < 0 && len(usageIdx) < 2 {
			t.Errorf("case %q: the stream carries neither a tool result nor a second usage event, so it cannot show which arrival produced it", tc.Name)
			return
		}
		if arrival == registry.UsageArrivalIncremental && turnEndShaped {
			t.Errorf("case %q: declared incremental, the turn settled one figure after its last tool result", tc.Name)
		}
		if arrival == registry.UsageArrivalTurnEnd && !turnEndShaped {
			t.Errorf("case %q: declared turn_end, the turn reported a figure while it was still working, or reported more than one", tc.Name)
		}
		if !tc.Result.UsageMeasured {
			t.Errorf("case %q: declared %s, the turn reported no measurement", tc.Name, arrival)
		}
		if arrival == registry.UsageArrivalTurnEnd {
			for i, event := range tc.Events {
				if event.Usage == (domain.TokenUsage{}) {
					continue
				}
				if !dominates(tc.Result.Usage, event.Usage) {
					t.Errorf("case %q: event %d: declared turn_end, result.Usage %+v does not dominate a figure the turn reported %+v",
						tc.Name, i, tc.Result.Usage, event.Usage)
				}
				if event.Type == domain.EventTokenUsage && !followedByTerminalEvent(tc.Events, i) {
					t.Errorf("case %q: event %d: declared turn_end, the usage event has no later turn-terminal event",
						tc.Name, i)
				}
			}
		}
	default:
		t.Errorf("case %q: arrival %q is a value outside the declared set", tc.Name, arrival)
	}

	switch attribution {
	case registry.UsageAttributionUndeclared:
		t.Errorf("case %q: want a kind that declares a usage attribution", tc.Name)
	case registry.UsageAttributionNone:
		if arrival != registry.UsageArrivalNone {
			t.Errorf("case %q: INV-1 violated: attribution none requires arrival none, got %q", tc.Name, arrival)
		}
	case registry.UsageAttributionPerModel:
		named := false
		for _, event := range tc.Events {
			if event.Usage != (domain.TokenUsage{}) && event.Model != "" {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("case %q: declared per_model, no usage-bearing event named a model", tc.Name)
		}
	case registry.UsageAttributionSessionTotal:
		for i, event := range tc.Events {
			if event.Usage != (domain.TokenUsage{}) && event.Model != "" {
				t.Errorf("case %q: event %d: declared session_total, a usage-bearing event named a model %q", tc.Name, i, event.Model)
			}
		}
	default:
		t.Errorf("case %q: attribution %q is a value outside the declared set", tc.Name, attribution)
	}
}

// dominates reports whether result is componentwise greater than or
// equal to figure, the definition [registry.UsageArrivalTurnEnd]'s
// settled-at-end figure must satisfy against the turn's final result.
func dominates(result, figure domain.TokenUsage) bool {
	return result.InputTokens >= figure.InputTokens &&
		result.OutputTokens >= figure.OutputTokens &&
		result.TotalTokens >= figure.TotalTokens &&
		result.CacheReadTokens >= figure.CacheReadTokens
}

// turnTerminalEventTypes lists the event types that end a turn, the set
// a turn_end kind's usage event MUST precede.
var turnTerminalEventTypes = map[domain.AgentEventType]bool{
	domain.EventTurnCompleted:      true,
	domain.EventTurnFailed:         true,
	domain.EventTurnCancelled:      true,
	domain.EventTurnEndedWithError: true,
	domain.EventTurnInputRequired:  true,
}

// followedByTerminalEvent reports whether events holds a turn-terminal
// event at some index after usageIdx.
func followedByTerminalEvent(events []domain.AgentEvent, usageIdx int) bool {
	for _, event := range events[usageIdx+1:] {
		if turnTerminalEventTypes[event.Type] {
			return true
		}
	}
	return false
}
