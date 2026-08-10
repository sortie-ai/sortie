package agentcore

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TestDecideTurn_Rows pins every row of the seven-row decision table
// (R1 through R7) against a representative TurnEvidence, asserting the
// full TurnDisposition the table produces for each.
func TestDecideTurn_Rows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		ev               TurnEvidence
		wantRow          DispositionRow
		wantExitReason   domain.AgentEventType
		wantErrorKind    domain.AgentErrorKind
		wantEventMessage string
		wantErrorMessage string
	}{
		{
			name:             "R1 terminal cancelled with message",
			ev:               TurnEvidence{Terminal: TerminalCancelled, TerminalMessage: "context cancelled"},
			wantRow:          RowTerminalCancelled,
			wantExitReason:   domain.EventTurnCancelled,
			wantErrorKind:    domain.ErrTurnCancelled,
			wantEventMessage: "context cancelled",
			wantErrorMessage: "context cancelled",
		},
		{
			name:             "R2 terminal failure with kind and message",
			ev:               TurnEvidence{Terminal: TerminalFailure, TerminalErrorKind: domain.ErrResponseError, TerminalMessage: "auth rejected"},
			wantRow:          RowTerminalFailure,
			wantExitReason:   domain.EventTurnFailed,
			wantErrorKind:    domain.ErrResponseError,
			wantEventMessage: "auth rejected",
			wantErrorMessage: "auth rejected",
		},
		{
			name:             "R3 terminal success carries message on event only",
			ev:               TurnEvidence{Terminal: TerminalSuccess, TerminalMessage: "All done."},
			wantRow:          RowTerminalSuccess,
			wantExitReason:   domain.EventTurnCompleted,
			wantErrorKind:    "",
			wantEventMessage: "All done.",
			wantErrorMessage: "",
		},
		{
			name:             "R4 no exit observed",
			ev:               TurnEvidence{Terminal: TerminalAbsent, ExitObserved: false},
			wantRow:          RowNoExitObserved,
			wantExitReason:   domain.EventTurnFailed,
			wantErrorKind:    domain.ErrPortExit,
			wantEventMessage: "runtime ended without reporting a turn outcome",
			wantErrorMessage: "runtime ended without reporting a turn outcome",
		},
		{
			name:             "R5 non-zero exit differs between event and error message",
			ev:               TurnEvidence{Terminal: TerminalAbsent, ExitObserved: true, ExitCode: 7},
			wantRow:          RowNonZeroExit,
			wantExitReason:   domain.EventTurnFailed,
			wantErrorKind:    domain.ErrPortExit,
			wantEventMessage: "non-zero exit",
			wantErrorMessage: "exit code 7",
		},
		{
			name:             "R6 zero work with no detail",
			ev:               TurnEvidence{Terminal: TerminalAbsent, ExitObserved: true, ExitCode: 0, Work: WorkAbsent},
			wantRow:          RowZeroWork,
			wantExitReason:   domain.EventTurnFailed,
			wantErrorKind:    domain.ErrTurnFailed,
			wantEventMessage: "agent exited without producing output",
			wantErrorMessage: "agent exited without producing output",
		},
		{
			name:             "R7 work present completes",
			ev:               TurnEvidence{Terminal: TerminalAbsent, ExitObserved: true, ExitCode: 0, Work: WorkPresent},
			wantRow:          RowWorkPresent,
			wantExitReason:   domain.EventTurnCompleted,
			wantErrorKind:    "",
			wantEventMessage: "",
			wantErrorMessage: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DecideTurn(tt.ev)

			if got.Row != tt.wantRow {
				t.Errorf("DecideTurn(%+v).Row = %v, want %v", tt.ev, got.Row, tt.wantRow)
			}
			if got.ExitReason != tt.wantExitReason {
				t.Errorf("DecideTurn(%+v).ExitReason = %q, want %q", tt.ev, got.ExitReason, tt.wantExitReason)
			}
			if got.ErrorKind != tt.wantErrorKind {
				t.Errorf("DecideTurn(%+v).ErrorKind = %q, want %q", tt.ev, got.ErrorKind, tt.wantErrorKind)
			}
			if got.EventMessage != tt.wantEventMessage {
				t.Errorf("DecideTurn(%+v).EventMessage = %q, want %q", tt.ev, got.EventMessage, tt.wantEventMessage)
			}
			if got.ErrorMessage != tt.wantErrorMessage {
				t.Errorf("DecideTurn(%+v).ErrorMessage = %q, want %q", tt.ev, got.ErrorMessage, tt.wantErrorMessage)
			}
		})
	}
}

// TestDecideTurn_R1R2DefaultOnlyOnEmptyMessage pins that the "turn
// cancelled" and "turn failed" substitute texts are used only when
// TerminalMessage is empty; a supplied TerminalMessage is never
// overwritten.
func TestDecideTurn_R1R2DefaultOnlyOnEmptyMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ev          TurnEvidence
		wantMessage string
	}{
		{
			name:        "cancelled empty message defaults",
			ev:          TurnEvidence{Terminal: TerminalCancelled},
			wantMessage: "turn cancelled",
		},
		{
			name:        "cancelled supplied message is not overwritten",
			ev:          TurnEvidence{Terminal: TerminalCancelled, TerminalMessage: "operator stopped the session"},
			wantMessage: "operator stopped the session",
		},
		{
			name:        "failure empty message defaults",
			ev:          TurnEvidence{Terminal: TerminalFailure},
			wantMessage: "turn failed",
		},
		{
			name:        "failure supplied message is not overwritten",
			ev:          TurnEvidence{Terminal: TerminalFailure, TerminalMessage: "invalid api key"},
			wantMessage: "invalid api key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DecideTurn(tt.ev)

			if got.EventMessage != tt.wantMessage {
				t.Errorf("DecideTurn(%+v).EventMessage = %q, want %q", tt.ev, got.EventMessage, tt.wantMessage)
			}
			if got.ErrorMessage != tt.wantMessage {
				t.Errorf("DecideTurn(%+v).ErrorMessage = %q, want %q", tt.ev, got.ErrorMessage, tt.wantMessage)
			}
		})
	}
}

// TestDecideTurn_ZeroWorkRowBothWorkValuesFail pins that WorkAbsent and
// WorkUnobservable both reach RowZeroWork, so an adapter with no positive
// signal to offer never completes a turn by omission.
func TestDecideTurn_ZeroWorkRowBothWorkValuesFail(t *testing.T) {
	t.Parallel()

	for _, work := range []WorkReport{WorkAbsent, WorkUnobservable} {
		ev := TurnEvidence{Terminal: TerminalAbsent, ExitObserved: true, ExitCode: 0, Work: work}

		got := DecideTurn(ev)

		if got.Row != RowZeroWork {
			t.Errorf("DecideTurn(Work=%v).Row = %v, want %v", work, got.Row, RowZeroWork)
		}
		if got.ExitReason != domain.EventTurnFailed {
			t.Errorf("DecideTurn(Work=%v).ExitReason = %q, want %q", work, got.ExitReason, domain.EventTurnFailed)
		}
		if got.ErrorKind != domain.ErrTurnFailed {
			t.Errorf("DecideTurn(Work=%v).ErrorKind = %q, want %q", work, got.ErrorKind, domain.ErrTurnFailed)
		}
	}
}

// TestDecideTurn_ZeroWorkRowWorkDetailSuffix pins that the zero-work
// stem carries a ": "-prefixed WorkDetail suffix only when WorkDetail is
// non-empty, and both messages carry the same suffixed text.
func TestDecideTurn_ZeroWorkRowWorkDetailSuffix(t *testing.T) {
	t.Parallel()

	t.Run("empty detail produces the bare stem", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{Terminal: TerminalAbsent, ExitObserved: true, ExitCode: 0, Work: WorkAbsent})

		const want = "agent exited without producing output"
		if got.EventMessage != want {
			t.Errorf("EventMessage = %q, want %q", got.EventMessage, want)
		}
		if got.ErrorMessage != want {
			t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, want)
		}
	})

	t.Run("non-empty detail is appended with a colon-space separator", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{
			Terminal:     TerminalAbsent,
			ExitObserved: true,
			ExitCode:     0,
			Work:         WorkUnobservable,
			WorkDetail:   "no credits trailer on stderr",
		})

		const want = "agent exited without producing output: no credits trailer on stderr"
		if got.EventMessage != want {
			t.Errorf("EventMessage = %q, want %q", got.EventMessage, want)
		}
		if got.ErrorMessage != want {
			t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, want)
		}
	})
}

// TestDecideTurn_Total pins that DecideTurn is total over TurnEvidence:
// every distinct combination of the fields the table consults maps to
// exactly one non-zero Row.
func TestDecideTurn_Total(t *testing.T) {
	t.Parallel()

	terminals := []TerminalReport{TerminalAbsent, TerminalSuccess, TerminalFailure, TerminalCancelled}
	exitCodes := []int{0, 1}
	works := []WorkReport{WorkUnobservable, WorkAbsent, WorkPresent}

	for _, terminal := range terminals {
		for _, exitObserved := range []bool{false, true} {
			for _, exitCode := range exitCodes {
				for _, work := range works {
					ev := TurnEvidence{
						Terminal:     terminal,
						ExitObserved: exitObserved,
						ExitCode:     exitCode,
						Work:         work,
					}
					got := DecideTurn(ev)
					if got.Row == 0 {
						t.Fatalf("DecideTurn(%+v).Row = 0, want a non-zero row", ev)
					}
				}
			}
		}
	}
}
