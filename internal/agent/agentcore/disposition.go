package agentcore

import (
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
)

// zeroWorkMessageStem is the shared diagnostic stem every adapter's
// zero-work row uses, so an operator can grep one string across the whole
// agent family.
const zeroWorkMessageStem = "agent exited without producing output"

// turnInputRequiredMessageStem is the shared diagnostic stem every
// adapter's human-input-required row uses, so an operator can grep one
// string across the whole agent family.
const turnInputRequiredMessageStem = "agent asked for a decision only a person can make"

// TerminalReport is the adapter-normalized end-of-turn report for one turn:
// what the runtime said about the outcome, or what the adapter determined
// when it aborted the turn itself.
type TerminalReport uint8

const (
	// TerminalAbsent means neither the runtime nor the adapter has an
	// outcome to report, so the disposition rests on the process exit and
	// the work evidence.
	TerminalAbsent TerminalReport = iota

	// TerminalSuccess means the runtime, or the adapter on its behalf,
	// reported that the turn completed successfully.
	TerminalSuccess

	// TerminalFailure means the runtime, or the adapter on its behalf,
	// reported that the turn ended in failure.
	TerminalFailure

	// TerminalCancelled means the orchestrator initiated the turn's
	// cancellation, through context cancellation or StopSession.
	TerminalCancelled

	// TerminalHumanInputRequired means the runtime asked for a decision
	// only a person could give and the shared posture ends the attempt.
	// An adapter reports it after classifying the request; it never
	// decides the consequence itself.
	TerminalHumanInputRequired
)

// WorkReport is the adapter-normalized evidence that the model produced
// something during this turn.
type WorkReport uint8

const (
	// WorkUnobservable means the runtime exposes no per-turn work signal
	// the adapter can read.
	WorkUnobservable WorkReport = iota

	// WorkAbsent means the adapter observed a per-turn work signal and it
	// reported nothing produced.
	WorkAbsent

	// WorkPresent means the adapter observed a per-turn work signal and it
	// reported something produced.
	WorkPresent
)

// TurnEvidence is everything the shared decision needs to know about how
// one turn ended. Adapters populate it from their own wire format; it has
// no adapter-shaped field, so an adapter that cannot express its situation
// in these fields has found a gap in the shared contract rather than a
// reason to decide the disposition itself.
type TurnEvidence struct {
	// Terminal is the runtime's end-of-turn report, or the adapter's own
	// determination when it aborted the turn. An adapter MUST NOT report
	// TerminalSuccess or TerminalFailure for a turn whose runtime produced
	// no end-of-turn report and whose process exited 0; that combination
	// belongs to the rows keyed on ExitObserved, ExitCode, and Work.
	Terminal TerminalReport

	// TerminalErrorKind classifies a TerminalFailure. Ignored for every
	// other Terminal value. The zero value selects domain.ErrTurnFailed.
	TerminalErrorKind domain.AgentErrorKind

	// TerminalMessage describes the outcome in the runtime's own words, or
	// in the adapter's when the adapter ended the turn. Carried onto both
	// the emitted event and the returned error for a cancelled, failed,
	// successful, or human-input-required terminal report. May be empty.
	//
	// On the human-input-required report alone, the field is appended
	// after the shared message stem rather than used as the whole
	// message, and it MUST be a compile-time constant string that never
	// interpolates stderr, stdout, a file path, or any other runtime
	// value, for the same reason WorkDetail must be.
	TerminalMessage string

	// Cause is the underlying Go error the adapter recovered, copied onto
	// domain.AgentError.Err so the unwrap chain survives. Nil when the
	// adapter has no underlying error.
	Cause error

	// ExitObserved reports whether this turn had its own process exit to
	// observe. False for adapters whose subprocess outlives the turn.
	ExitObserved bool

	// ExitCode is the turn's process exit code. Meaningful only when
	// ExitObserved is true.
	ExitCode int

	// Work is the per-turn work evidence. Consulted only when the runtime
	// reported no terminal outcome.
	Work WorkReport

	// WorkDetail names the signal the adapter looked for and did not find,
	// for the operator's benefit. Appended to both messages of the
	// zero-work row. May be empty. MUST be a compile-time constant string;
	// it MUST NOT interpolate stderr, stdout, a file path, or any other
	// runtime value.
	WorkDetail string
}

// DispositionRow identifies the decision-table row that produced a
// TurnDisposition. It exists so a caller can attach a per-row side effect,
// and so a test can assert which rule fired, without re-deriving the
// decision.
type DispositionRow uint8

const (
	// RowTerminalCancelled is the row selected when Terminal is
	// TerminalCancelled.
	RowTerminalCancelled DispositionRow = iota + 1

	// RowTerminalFailure is the row selected when Terminal is
	// TerminalFailure.
	RowTerminalFailure

	// RowTerminalSuccess is the row selected when Terminal is
	// TerminalSuccess.
	RowTerminalSuccess

	// RowNoExitObserved is the row selected when Terminal is
	// TerminalAbsent and no process exit was observed.
	RowNoExitObserved

	// RowNonZeroExit is the row selected when Terminal is TerminalAbsent,
	// a process exit was observed, and the exit code is non-zero.
	RowNonZeroExit

	// RowZeroWork is the row selected when Terminal is TerminalAbsent, the
	// process exited 0, and Work is not WorkPresent.
	RowZeroWork

	// RowWorkPresent is the row selected when Terminal is TerminalAbsent,
	// the process exited 0, and Work is WorkPresent.
	RowWorkPresent

	// RowHumanInputRequired is the row selected when Terminal is
	// TerminalHumanInputRequired.
	RowHumanInputRequired
)

// TurnDisposition is the normalized outcome of one turn.
type TurnDisposition struct {
	// Row is the decision-table row that matched.
	Row DispositionRow

	// ExitReason is the normalized event type for the turn.
	ExitReason domain.AgentEventType

	// ErrorKind is empty when and only when ExitReason is
	// domain.EventTurnCompleted.
	ErrorKind domain.AgentErrorKind

	// EventMessage is the description carried on the emitted terminal
	// event. May be empty.
	EventMessage string

	// ErrorMessage is the description carried on the returned
	// domain.AgentError. Empty when and only when ErrorKind is empty.
	ErrorMessage string
}

// TurnMeta carries the per-turn values the finalizer copies onto the
// terminal event and the TurnResult without interpreting them.
type TurnMeta struct {
	// SessionID is the adapter's current session identifier.
	SessionID string

	// Usage is the session's run-cumulative token usage snapshot.
	Usage domain.TokenUsage

	// UsageMeasured reports whether the adapter has observed at least one
	// usage measurement for the session by the end of this turn.
	UsageMeasured bool

	// APIDurationMS is the LLM API response wait time in milliseconds for
	// this turn, or 0 when unavailable.
	APIDurationMS int64
}

// DecideTurn returns the normalized disposition for one agent turn. It is
// pure: no I/O, no logging, no emission, no state. Every TurnEvidence value
// maps to exactly one of eight rows, evaluated in order and returned on the
// first match.
//
// A positive terminal report is authoritative: when Terminal is
// TerminalCancelled, TerminalFailure, or TerminalSuccess, DecideTurn does
// not consult Work. Work is consulted only to fill the gap left by a
// runtime that reported no terminal outcome and whose process exited 0.
func DecideTurn(ev TurnEvidence) TurnDisposition {
	switch ev.Terminal {
	case TerminalCancelled:
		message := ev.TerminalMessage
		if message == "" {
			message = "turn cancelled"
		}
		return TurnDisposition{
			Row:          RowTerminalCancelled,
			ExitReason:   domain.EventTurnCancelled,
			ErrorKind:    domain.ErrTurnCancelled,
			EventMessage: message,
			ErrorMessage: message,
		}
	case TerminalHumanInputRequired:
		message := turnInputRequiredMessageStem
		if ev.TerminalMessage != "" {
			message += ": " + ev.TerminalMessage
		}
		return TurnDisposition{
			Row:          RowHumanInputRequired,
			ExitReason:   domain.EventTurnInputRequired,
			ErrorKind:    domain.ErrTurnInputRequired,
			EventMessage: message,
			ErrorMessage: message,
		}
	case TerminalFailure:
		kind := ev.TerminalErrorKind
		if kind == "" {
			kind = domain.ErrTurnFailed
		}
		message := ev.TerminalMessage
		if message == "" {
			message = "turn failed"
		}
		return TurnDisposition{
			Row:          RowTerminalFailure,
			ExitReason:   domain.EventTurnFailed,
			ErrorKind:    kind,
			EventMessage: message,
			ErrorMessage: message,
		}
	case TerminalSuccess:
		return TurnDisposition{
			Row:          RowTerminalSuccess,
			ExitReason:   domain.EventTurnCompleted,
			EventMessage: ev.TerminalMessage,
		}
	}

	if !ev.ExitObserved {
		return TurnDisposition{
			Row:          RowNoExitObserved,
			ExitReason:   domain.EventTurnFailed,
			ErrorKind:    domain.ErrPortExit,
			EventMessage: "runtime reported no turn outcome",
			ErrorMessage: "runtime reported no turn outcome",
		}
	}

	if ev.ExitCode != 0 {
		return TurnDisposition{
			Row:          RowNonZeroExit,
			ExitReason:   domain.EventTurnFailed,
			ErrorKind:    domain.ErrPortExit,
			EventMessage: "non-zero exit",
			ErrorMessage: fmt.Sprintf("exit code %d", ev.ExitCode),
		}
	}

	if ev.Work != WorkPresent {
		message := zeroWorkMessageStem
		if ev.WorkDetail != "" {
			message += ": " + ev.WorkDetail
		}
		return TurnDisposition{
			Row:          RowZeroWork,
			ExitReason:   domain.EventTurnFailed,
			ErrorKind:    domain.ErrTurnFailed,
			EventMessage: message,
			ErrorMessage: message,
		}
	}

	return TurnDisposition{
		Row:        RowWorkPresent,
		ExitReason: domain.EventTurnCompleted,
	}
}

// FinalizeTurn applies DecideTurn to ev, emits the matching terminal event
// through emit, logs a warn message when the decision selects RowZeroWork
// or RowHumanInputRequired, and returns the paired TurnResult and error.
// The returned *domain.AgentError is nil if and only if the disposition is
// domain.EventTurnCompleted.
//
// FinalizeTurn panics when emit is nil. It substitutes slog.Default() when
// logger is nil. It is not idempotent, because it emits; callers MUST call
// it at most once per turn.
func FinalizeTurn(
	emit func(domain.AgentEvent),
	logger *slog.Logger,
	ev TurnEvidence,
	meta TurnMeta,
) (domain.TurnResult, *domain.AgentError) {
	if emit == nil {
		panic("agentcore: FinalizeTurn: emit must be non-nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	disposition := DecideTurn(ev)

	switch disposition.ExitReason {
	case domain.EventTurnCompleted:
		EmitTurnCompleted(emit, disposition.EventMessage, meta.APIDurationMS, meta.Usage)
	case domain.EventTurnFailed:
		EmitTurnFailed(emit, disposition.EventMessage, meta.APIDurationMS, meta.Usage)
	case domain.EventTurnCancelled:
		EmitTurnCancelled(emit, disposition.EventMessage, meta.Usage)
	case domain.EventTurnInputRequired:
		EmitTurnInputRequired(emit, disposition.EventMessage, meta.Usage)
	}

	if disposition.Row == RowZeroWork {
		logger.Warn("agent exited without producing output, treating as failure")
	}
	if disposition.Row == RowHumanInputRequired {
		logger.Warn("agent asked for a decision only a person can make, ending the attempt")
	}

	result := domain.TurnResult{
		SessionID:     meta.SessionID,
		Usage:         meta.Usage,
		UsageMeasured: meta.UsageMeasured,
		ExitReason:    disposition.ExitReason,
	}

	if disposition.ErrorKind == "" {
		return result, nil
	}
	return result, &domain.AgentError{
		Kind:    disposition.ErrorKind,
		Message: disposition.ErrorMessage,
		Err:     ev.Cause,
	}
}
