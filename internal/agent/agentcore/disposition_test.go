package agentcore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
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
			wantEventMessage: "runtime reported no turn outcome",
			wantErrorMessage: "runtime reported no turn outcome",
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
		{
			name:             "terminal incomplete with message",
			ev:               TurnEvidence{Terminal: TerminalIncomplete, TerminalMessage: "some detail"},
			wantRow:          RowTerminalIncomplete,
			wantExitReason:   domain.EventTurnFailed,
			wantErrorKind:    domain.ErrTurnIncomplete,
			wantEventMessage: incompleteMessageStem + ": some detail",
			wantErrorMessage: incompleteMessageStem + ": some detail",
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

// TestDecideTurn_HumanInputRequiredRow pins the human-input-required row:
// the bare stem when TerminalMessage is empty, the stem joined to a
// non-empty TerminalMessage with ": ", the row's fixed ExitReason and
// ErrorKind, and that TerminalCancelled still wins when both Terminal
// values could apply (case ordering, not an added precedence flag,
// decides it).
func TestDecideTurn_HumanInputRequiredRow(t *testing.T) {
	t.Parallel()

	const stem = "agent asked for a decision only a person can make"

	t.Run("empty detail produces the bare stem", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{Terminal: TerminalHumanInputRequired})

		if got.Row != RowHumanInputRequired {
			t.Errorf("DecideTurn().Row = %v, want %v", got.Row, RowHumanInputRequired)
		}
		if got.ExitReason != domain.EventTurnInputRequired {
			t.Errorf("DecideTurn().ExitReason = %q, want %q", got.ExitReason, domain.EventTurnInputRequired)
		}
		if got.ErrorKind != domain.ErrTurnInputRequired {
			t.Errorf("DecideTurn().ErrorKind = %q, want %q", got.ErrorKind, domain.ErrTurnInputRequired)
		}
		if got.EventMessage != stem {
			t.Errorf("DecideTurn().EventMessage = %q, want %q", got.EventMessage, stem)
		}
		if got.ErrorMessage != stem {
			t.Errorf("DecideTurn().ErrorMessage = %q, want %q", got.ErrorMessage, stem)
		}
	})

	t.Run("non-empty detail is appended with a colon-space separator", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{Terminal: TerminalHumanInputRequired, TerminalMessage: "an answer to a question"})

		const want = stem + ": an answer to a question"
		if got.EventMessage != want {
			t.Errorf("DecideTurn().EventMessage = %q, want %q", got.EventMessage, want)
		}
		if got.ErrorMessage != want {
			t.Errorf("DecideTurn().ErrorMessage = %q, want %q", got.ErrorMessage, want)
		}
	})

	t.Run("ErrorKind is non-empty, preserving the empty-iff-completed invariant", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{Terminal: TerminalHumanInputRequired})

		if got.ErrorKind == "" {
			t.Error("DecideTurn().ErrorKind is empty, want non-empty since ExitReason is not EventTurnCompleted")
		}
	})

	t.Run("cancelled still wins when both terminal reports could apply", func(t *testing.T) {
		t.Parallel()

		// TerminalReport is a single field, so no TurnEvidence value can
		// literally set both at once; this asserts the case ordering
		// itself, cancelled evaluated before human-input-required, by
		// confirming cancelled's row is selected whenever Terminal is
		// TerminalCancelled regardless of TerminalMessage shape used by
		// the human-input-required row.
		got := DecideTurn(TurnEvidence{Terminal: TerminalCancelled, TerminalMessage: "an answer to a question"})

		if got.Row != RowTerminalCancelled {
			t.Errorf("DecideTurn().Row = %v, want %v", got.Row, RowTerminalCancelled)
		}
		if got.ExitReason != domain.EventTurnCancelled {
			t.Errorf("DecideTurn().ExitReason = %q, want %q", got.ExitReason, domain.EventTurnCancelled)
		}
	})
}

// TestDecideTurn_IncompleteRowMessageSuffix pins that the incomplete
// stem carries a ": "-prefixed TerminalMessage suffix only when
// TerminalMessage is non-empty, and both messages carry the same
// suffixed text.
func TestDecideTurn_IncompleteRowMessageSuffix(t *testing.T) {
	t.Parallel()

	t.Run("empty detail produces the bare stem", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{Terminal: TerminalIncomplete})

		if got.EventMessage != incompleteMessageStem {
			t.Errorf("EventMessage = %q, want %q", got.EventMessage, incompleteMessageStem)
		}
		if got.ErrorMessage != incompleteMessageStem {
			t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, incompleteMessageStem)
		}
	})

	t.Run("non-empty detail is appended with a colon-space separator", func(t *testing.T) {
		t.Parallel()

		got := DecideTurn(TurnEvidence{
			Terminal:        TerminalIncomplete,
			TerminalMessage: "raise copilot-cli.max_autopilot_continues if the turn needs more steps",
		})

		const want = incompleteMessageStem + ": raise copilot-cli.max_autopilot_continues if the turn needs more steps"
		if got.EventMessage != want {
			t.Errorf("EventMessage = %q, want %q", got.EventMessage, want)
		}
		if got.ErrorMessage != want {
			t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, want)
		}
	})
}

// TestDecideTurn_IncompleteRowFieldsIndependent pins that
// TerminalErrorKind, ExitCode, and Work never move a TerminalIncomplete
// evidence value away from RowTerminalIncomplete: DecideTurn does not
// consult them once Terminal carries an authoritative report.
func TestDecideTurn_IncompleteRowFieldsIndependent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   TurnEvidence
	}{
		{
			name: "TerminalErrorKind set",
			ev:   TurnEvidence{Terminal: TerminalIncomplete, TerminalErrorKind: domain.ErrResponseError},
		},
		{
			name: "ExitCode non-zero",
			ev:   TurnEvidence{Terminal: TerminalIncomplete, ExitObserved: true, ExitCode: 7},
		},
		{
			name: "Work present",
			ev:   TurnEvidence{Terminal: TerminalIncomplete, Work: WorkPresent},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DecideTurn(tt.ev)

			if got.Row != RowTerminalIncomplete {
				t.Errorf("DecideTurn(%+v).Row = %v, want %v", tt.ev, got.Row, RowTerminalIncomplete)
			}
		})
	}
}

// TestFinalizeTurn_TerminalIncompleteLogsWarn pins the exact warn line
// FinalizeTurn emits for RowTerminalIncomplete, and that it carries no
// attributes.
func TestFinalizeTurn_TerminalIncompleteLogsWarn(t *testing.T) {
	// No t.Parallel(): installs a global slog default, matching every
	// other test in this package that installs the log spy.
	spy := agenttest.InstallLogSpy(t)

	_, _ = FinalizeTurn(func(domain.AgentEvent) {}, nil, TurnEvidence{Terminal: TerminalIncomplete}, TurnMeta{})

	const want = "agent stopped without reporting the task complete, treating as failure"

	var warnEntries []agenttest.LogSpyEntry
	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn {
			warnEntries = append(warnEntries, e)
		}
	}
	if len(warnEntries) != 1 {
		t.Fatalf("FinalizeTurn() logged %d WARN lines, want 1: %+v", len(warnEntries), warnEntries)
	}
	if warnEntries[0].Msg != want {
		t.Errorf("FinalizeTurn() warn message = %q, want %q", warnEntries[0].Msg, want)
	}
	if warnEntries[0].Line != "" {
		t.Errorf("FinalizeTurn() warn carries a %q attribute, want no attributes", warnEntries[0].Line)
	}
}

// terminalReportValues parses disposition.go's own source and returns
// every value declared in the TerminalReport const block that starts at
// TerminalAbsent. Every value in that block is required to be an
// appended, contiguous iota, so counting the block's ValueSpecs is
// sufficient to enumerate it: declaring a further TerminalReport value
// there makes it appear here without editing this test. It fails loudly
// when the block cannot be located, so a rename breaks this test rather
// than silently covering nothing.
func terminalReportValues(t *testing.T) []TerminalReport {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "disposition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing disposition.go: %v", err)
	}

	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		first, ok := genDecl.Specs[0].(*ast.ValueSpec)
		if !ok || len(first.Names) != 1 || first.Names[0].Name != "TerminalAbsent" {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != "TerminalReport" {
			continue
		}

		values := make([]TerminalReport, len(genDecl.Specs))
		for i := range genDecl.Specs {
			values[i] = TerminalReport(i)
		}
		return values
	}

	t.Fatal("disposition.go: could not locate the TerminalReport const block starting at TerminalAbsent")
	return nil
}

// TestDecideTurn_Total pins that DecideTurn is total over TurnEvidence:
// every distinct combination of the fields the table consults maps to
// exactly one non-zero Row.
func TestDecideTurn_Total(t *testing.T) {
	t.Parallel()

	terminals := terminalReportValues(t)
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
