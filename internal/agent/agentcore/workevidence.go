package agentcore

// workAbsentBothDetail, workAbsentAssistantOnlyDetail, and
// workAbsentToolOnlyDetail are the compile-time constant WorkDetail
// strings Report returns, keyed on which signals an adapter declared and
// found unobserved. Naming them here, rather than at the call site, keeps
// every adapter's zero-work message identical for the same declaration.
const (
	workAbsentBothDetail          = "no message from the agent and no tool call"
	workAbsentAssistantOnlyDetail = "no message from the agent"
	workAbsentToolOnlyDetail      = "no tool call"
)

// WorkSignals declares which production facts one adapter's runtime lets
// it observe for a single turn. At least one field must be true;
// NewWorkObserver rejects a declaration that sets neither.
type WorkSignals struct {
	// AssistantOutput declares that the runtime reports model-authored
	// content for the turn: a message, a text block, a reasoning block,
	// or a delta whose own event type names the assistant message.
	AssistantOutput bool

	// ToolActivity declares that the runtime reports tool calls the model
	// requested for the turn, at any point in a call's life.
	ToolActivity bool
}

// WorkObserver accumulates one turn's work observations and reports them
// as the pair agentcore.TurnEvidence carries. One observer serves one
// turn. It is not safe for concurrent use.
type WorkObserver struct {
	signals  WorkSignals
	observed bool
}

// NewWorkObserver returns an observer for one turn under the given
// declaration. signals states what the adapter's runtime exposes, not
// what this turn happened to carry. It panics when signals sets no
// field, because an adapter with nothing to observe does not construct
// an observer at all.
func NewWorkObserver(signals WorkSignals) *WorkObserver {
	if !signals.AssistantOutput && !signals.ToolActivity {
		panic("agentcore: NewWorkObserver: signals must set at least one field")
	}
	return &WorkObserver{signals: signals}
}

// ObserveAssistantOutput records that the runtime reported model-authored
// content for this turn. It panics when the adapter did not declare
// AssistantOutput.
func (o *WorkObserver) ObserveAssistantOutput() {
	if o == nil {
		panic("agentcore: WorkObserver.ObserveAssistantOutput: nil receiver")
	}
	if !o.signals.AssistantOutput {
		panic("agentcore: WorkObserver.ObserveAssistantOutput: AssistantOutput not declared")
	}
	o.observed = true
}

// ObserveToolActivity records that the runtime reported a tool call the
// model requested for this turn. It panics when the adapter did not
// declare ToolActivity.
func (o *WorkObserver) ObserveToolActivity() {
	if o == nil {
		panic("agentcore: WorkObserver.ObserveToolActivity: nil receiver")
	}
	if !o.signals.ToolActivity {
		panic("agentcore: WorkObserver.ObserveToolActivity: ToolActivity not declared")
	}
	o.observed = true
}

// Observed reports whether at least one declared signal was observed
// this turn.
func (o *WorkObserver) Observed() bool {
	if o == nil {
		panic("agentcore: WorkObserver.Observed: nil receiver")
	}
	return o.observed
}

// Report returns the work evidence for this turn and the compile-time
// constant detail naming the signal that was looked for and not found.
// The detail is empty when the report is WorkPresent. Report never
// returns WorkUnobservable: a constructed observer always carries a
// declaration.
func (o *WorkObserver) Report() (WorkReport, string) {
	if o == nil {
		panic("agentcore: WorkObserver.Report: nil receiver")
	}
	if o.observed {
		return WorkPresent, ""
	}
	switch {
	case o.signals.AssistantOutput && o.signals.ToolActivity:
		return WorkAbsent, workAbsentBothDetail
	case o.signals.AssistantOutput:
		return WorkAbsent, workAbsentAssistantOnlyDetail
	default:
		return WorkAbsent, workAbsentToolOnlyDetail
	}
}
