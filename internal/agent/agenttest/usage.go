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

// AssertMeasurementAbsent fails t when the given events or result assert a
// usage measurement that a runtime reporting nothing must not produce: any
// event of type [domain.EventTokenUsage], any event carrying a non-zero
// Usage, or a true result.UsageMeasured.
func AssertMeasurementAbsent(t *testing.T, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()
	assertMeasurementAbsent(t, events, result)
}

func assertMeasurementAbsent(t contractReporter, events []domain.AgentEvent, result domain.TurnResult) {
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

func assertModelReported(t contractReporter, events []domain.AgentEvent, wantModel string) {
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

func assertUsageContract(t contractReporter, events []domain.AgentEvent) {
	t.Helper()

	var prev domain.TokenUsage
	for i, event := range events {
		usage := event.Usage
		if usage == (domain.TokenUsage{}) {
			continue
		}

		if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 ||
			usage.CacheWriteTokens < 0 || usage.TotalTokens < 0 {
			t.Errorf("event %d: Usage has a negative component: %+v", i, usage)
		}
		if usage.TotalTokens != usage.InputTokens+usage.OutputTokens {
			t.Errorf("event %d: TotalTokens = %d, want InputTokens+OutputTokens = %d",
				i, usage.TotalTokens, usage.InputTokens+usage.OutputTokens)
		}
		if usage.CacheReadTokens+usage.CacheWriteTokens > usage.InputTokens {
			t.Errorf("event %d: CacheReadTokens+CacheWriteTokens = %d, want <= InputTokens (%d)",
				i, usage.CacheReadTokens+usage.CacheWriteTokens, usage.InputTokens)
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
		if usage.CacheWriteTokens < prev.CacheWriteTokens {
			t.Errorf("event %d: CacheWriteTokens decreased from %d to %d", i, prev.CacheWriteTokens, usage.CacheWriteTokens)
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

func assertUsageReporting(t contractReporter, kind string, cases []UsageReportingCase) {
	t.Helper()

	meta, registered := lookupAgentMeta(t, kind)
	if !registered {
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
func assertResolvedUsageReporting(t contractReporter, tc UsageReportingCase, arrival registry.UsageArrival, attribution registry.UsageAttribution) {
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
			assertTurnEndFigures(t, tc)
		}
	default:
		t.Errorf("case %q: arrival %q is a value outside the declared set", tc.Name, arrival)
	}

	assertUsageAttribution(t, tc, arrival, attribution)
}

// assertTurnEndFigures checks the two rules a turn_end arrival adds to
// the events of one case: every usage event precedes a turn-terminal
// event, and the turn's final result dominates every figure it reported.
func assertTurnEndFigures(t contractReporter, tc UsageReportingCase) {
	t.Helper()

	for i, event := range tc.Events {
		// The ordering rule binds every token_usage event,
		// including one carrying an all-zero figure, which a
		// genuine zero spend produces.
		if event.Type == domain.EventTokenUsage && !followedByTerminalEvent(tc.Events, i) {
			t.Errorf("case %q: event %d: declared turn_end, the usage event has no later turn-terminal event",
				tc.Name, i)
		}
		if event.Usage == (domain.TokenUsage{}) {
			continue
		}
		if !dominates(tc.Result.Usage, event.Usage) {
			t.Errorf("case %q: event %d: declared turn_end, result.Usage %+v does not dominate a figure the turn reported %+v",
				tc.Name, i, tc.Result.Usage, event.Usage)
		}
	}
}

// assertUsageAttribution checks the events of one case against the
// attribution the kind declared for it.
func assertUsageAttribution(t contractReporter, tc UsageReportingCase, arrival registry.UsageArrival, attribution registry.UsageAttribution) {
	t.Helper()

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

// AssertUsageReportingCase fails t unless tc agrees with the pair kind
// resolves for it, applying the per-case check [AssertUsageReporting]
// applies to each of its cases. Unlike it, it requires no declared pair
// or session rule to be reached, so one case suffices.
func AssertUsageReportingCase(t *testing.T, kind string, tc UsageReportingCase) {
	t.Helper()
	assertUsageReportingCase(t, kind, tc)
}

func assertUsageReportingCase(t contractReporter, kind string, tc UsageReportingCase) {
	t.Helper()

	meta, registered := lookupAgentMeta(t, kind)
	if !registered {
		return
	}
	arrival, attribution := meta.UsageDisposition(tc.Passthrough, tc.Remote)
	assertResolvedUsageReporting(kindReporter{contractReporter: t, kind: kind}, tc, arrival, attribution)
}

// AssertLiveUsageCase fails t unless tc, the verification turn of a
// suite's working-credential case, agrees with the pair kind resolves
// for it. Under arrival none it asserts no measurement. Under a
// figure-reporting arrival a measured turn must report positive input
// and output tokens and satisfy the usage contract, and an unmeasured
// turn must flag its spend unaccounted and carry no measurement. Unlike
// [AssertUsageReportingCase] it does not require a tool result or a
// second usage event, which a verification turn does not produce.
func AssertLiveUsageCase(t *testing.T, kind string, tc UsageReportingCase) {
	t.Helper()
	assertLiveUsageCase(t, kind, tc)
}

func assertLiveUsageCase(t contractReporter, kind string, tc UsageReportingCase) {
	t.Helper()

	meta, registered := lookupAgentMeta(t, kind)
	if !registered {
		return
	}
	t = kindReporter{contractReporter: t, kind: kind}
	arrival, attribution := meta.UsageDisposition(tc.Passthrough, tc.Remote)

	switch arrival {
	case registry.UsageArrivalUndeclared:
		t.Errorf("case %q: want a kind that declares a usage arrival", tc.Name)
	case registry.UsageArrivalNone:
		assertMeasurementAbsent(t, tc.Events, tc.Result)
	case registry.UsageArrivalIncremental, registry.UsageArrivalTurnEnd:
		if !tc.Result.UsageMeasured {
			assertUnaccountedSpend(t, tc)
			return
		}
		if tc.Result.Usage.InputTokens <= 0 || tc.Result.Usage.OutputTokens <= 0 {
			t.Errorf("case %q: declared %s, result.Usage = %+v, want positive input and output tokens", tc.Name, arrival, tc.Result.Usage)
		}
		assertUsageContract(t, tc.Events)
		if arrival == registry.UsageArrivalTurnEnd {
			assertTurnEndFigures(t, tc)
		}
		assertUsageAttribution(t, tc, arrival, attribution)
	default:
		t.Errorf("case %q: arrival %q is a value outside the declared set", tc.Name, arrival)
	}
}

// assertUnaccountedSpend checks a turn no usage source measured: it
// must say so through SpendUnaccounted, and must not leak a measurement.
func assertUnaccountedSpend(t contractReporter, tc UsageReportingCase) {
	t.Helper()

	if !tc.Result.SpendUnaccounted {
		t.Errorf("case %q: the turn reported no measurement and did not flag its spend unaccounted", tc.Name)
	}
	assertMeasurementAbsent(t, tc.Events, tc.Result)
}

// lookupAgentMeta resolves kind's registered metadata, reporting an
// unregistered kind through t.
func lookupAgentMeta(t contractReporter, kind string) (registry.AgentMeta, bool) {
	t.Helper()

	meta, registered := registry.Agents.Meta(kind)
	if !registered {
		t.Errorf("kind %q is not registered", kind)
	}
	return meta, registered
}

// kindReporter names the kind in every failure a check reports.
type kindReporter struct {
	contractReporter
	kind string
}

func (r kindReporter) Errorf(format string, args ...any) {
	r.Helper()
	r.contractReporter.Errorf("kind %q: "+format, append([]any{r.kind}, args...)...)
}

// dominates reports whether result is componentwise greater than or
// equal to figure, the definition [registry.UsageArrivalTurnEnd]'s
// settled-at-end figure must satisfy against the turn's final result.
func dominates(result, figure domain.TokenUsage) bool {
	return result.InputTokens >= figure.InputTokens &&
		result.OutputTokens >= figure.OutputTokens &&
		result.TotalTokens >= figure.TotalTokens &&
		result.CacheReadTokens >= figure.CacheReadTokens &&
		result.CacheWriteTokens >= figure.CacheWriteTokens
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
