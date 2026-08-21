package agentcore

import "fmt"

// HumanRequestClass names the two classes of runtime request that only a
// person could answer. The class is determined by what the runtime asked
// for, never by which runtime asked.
type HumanRequestClass uint8

const (
	// ClassPermission is a request for consent to act: to run a command,
	// to change a file, to use a tool, to widen a sandbox.
	ClassPermission HumanRequestClass = iota + 1

	// ClassHumanInput is a genuine question addressed to a person.
	ClassHumanInput
)

// HumanRequestAnswer reports whether the recognized request is still open
// when the adapter observes it. Some runtimes answer such a request
// themselves before Sortie sees it, which is a different situation from a
// request nobody has answered.
type HumanRequestAnswer uint8

const (
	// AnswerPending means nothing has answered the request. Whether a
	// refusal can be transmitted is then a property of the protocol,
	// reported separately.
	AnswerPending HumanRequestAnswer = iota + 1

	// AnswerRuntimeRefused means the runtime refused the request on its
	// own and reported the refusal to the adapter. Nothing is left to
	// answer, and no consent was granted.
	AnswerRuntimeRefused
)

// noticePermissionContinuable, noticePermissionNoReplyChannel,
// noticePermissionRuntimeRefused, noticeHumanInputRequired, and
// noticeHumanInputRuntimeRefused are the operator-facing Notice values
// [DecideHumanRequest] returns, one per distinct posture. Rows 4 and 5 of
// the posture table share noticeHumanInputRequired: they differ only in
// wire mechanics an operator has no use for.
const (
	noticePermissionContinuable    = "refused a permission request because this run is unattended and no one can approve it"
	noticePermissionNoReplyChannel = "the agent needs a permission this unattended run cannot grant"
	noticePermissionRuntimeRefused = "the agent runtime refused a permission request on its own because this run is unattended"
	noticeHumanInputRequired       = "the agent asked for something only a person could give, and this unattended run has no one to give it"
	noticeHumanInputRuntimeRefused = "the agent runtime refused a request only a person could answer, and this unattended run has no one to answer it"
)

// RefusalPosture is the shared layer's decision for one recognized request.
// An adapter reads it and acts; it never constructs one.
type RefusalPosture struct {
	// Transmit reports whether the adapter writes a refusal on the
	// runtime's reply channel. False when the runtime offers none and
	// when the request already carries a refusal.
	Transmit bool

	// Continuable reports whether the transmitted refusal must take the
	// form that permits the agent to continue the turn by another route.
	// Meaningful only when Transmit is true.
	Continuable bool

	// EndAttempt reports whether the turn ends now with the
	// human-input-required outcome.
	EndAttempt bool

	// Notice is the operator-facing stem explaining the refusal, on every
	// posture row. The caller emits it as a domain.EventNotification,
	// alone when it has no detail for this request and joined to that
	// detail by ": " when it has one, using the same detail it passes to
	// [HumanInputEvidence]. It is a stem rather than a whole sentence
	// because the specific ask is adapter knowledge that
	// [DecideHumanRequest]'s three inputs do not carry, and a stem that
	// named the ask would be false on some request its row admits. No
	// permission-refusal form in the fleet has a field that could carry
	// it to the agent, so it is operator-facing text and is named for
	// that reader. It is a compile-time constant and MUST NOT interpolate
	// stderr, stdout, a file path, or any other runtime value.
	Notice string
}

// NoticeWithDetail joins p.Notice with detail. It returns the bare notice
// when detail is empty, and the notice followed by ": " and detail
// otherwise. detail MUST be a compile-time constant string that never
// interpolates stderr, stdout, a file path, or any other runtime value,
// the same rule [RefusalPosture.Notice] follows.
func (p RefusalPosture) NoticeWithDetail(detail string) string {
	if detail == "" {
		return p.Notice
	}
	return p.Notice + ": " + detail
}

// DecideHumanRequest returns the shared refusal posture for one recognized
// runtime request that only a person could answer. It is pure: no I/O, no
// logging, no adapter-specific vocabulary. Every adapter reads its posture
// from this function; none constructs one directly.
//
// DecideHumanRequest panics when class or answer is not one of its own two
// declared constants, including the zero value either type takes when a
// call site is only partially initialized: a posture returned for such a
// call would hand it an end-the-attempt or continue-the-turn decision it
// never asked for.
func DecideHumanRequest(class HumanRequestClass, replyChannel bool, answer HumanRequestAnswer) RefusalPosture {
	if class != ClassPermission && class != ClassHumanInput {
		panic(fmt.Sprintf("agentcore: DecideHumanRequest: invalid HumanRequestClass %d", class))
	}
	if answer != AnswerPending && answer != AnswerRuntimeRefused {
		panic(fmt.Sprintf("agentcore: DecideHumanRequest: invalid HumanRequestAnswer %d", answer))
	}

	switch {
	case class == ClassPermission && answer == AnswerRuntimeRefused:
		return RefusalPosture{Notice: noticePermissionRuntimeRefused}
	case class == ClassPermission && replyChannel:
		return RefusalPosture{Transmit: true, Continuable: true, Notice: noticePermissionContinuable}
	case class == ClassPermission:
		return RefusalPosture{EndAttempt: true, Notice: noticePermissionNoReplyChannel}
	case class == ClassHumanInput && answer == AnswerRuntimeRefused:
		return RefusalPosture{EndAttempt: true, Notice: noticeHumanInputRuntimeRefused}
	case replyChannel:
		return RefusalPosture{Transmit: true, EndAttempt: true, Notice: noticeHumanInputRequired}
	default:
		return RefusalPosture{EndAttempt: true, Notice: noticeHumanInputRequired}
	}
}

// HumanInputEvidence returns the [TurnEvidence] an adapter passes to
// [FinalizeTurn] once a [RefusalPosture] read from [DecideHumanRequest]
// has EndAttempt true. detail is appended to the shared message stem
// [FinalizeTurn] and [DecideTurn] build for [TerminalHumanInputRequired];
// it MUST be a compile-time constant string that never interpolates
// stderr, stdout, a file path, or any other runtime value, the same rule
// [RefusalPosture.Notice] follows.
func HumanInputEvidence(detail string) TurnEvidence {
	return TurnEvidence{
		Terminal:        TerminalHumanInputRequired,
		TerminalMessage: detail,
	}
}
