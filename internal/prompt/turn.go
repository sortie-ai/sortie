package prompt

import (
	"fmt"
	"strings"
)

// DefaultContinuationPrompt is the fallback prompt returned when a
// workflow template produces empty output on a continuation turn
// (turnNumber > 1). Templates that include
// {{ if .run.is_continuation }} branching provide project-specific
// continuation guidance; this constant covers templates that omit
// such branching.
const DefaultContinuationPrompt = "Continue working on this task. Review the current state of your work, check what remains to be done, and proceed with the next step. If you believe the task is complete, verify your changes and confirm completion."

// RuntimeStatusSuffix is a fixed instruction string appended to the agent
// prompt on the first turn of each worker run. It informs the agent of
// the A2O status-signaling protocol for reporting blocked or
// review-needed status via the .sortie/status file.
//
// Continuation turns omit this suffix because the instruction persists
// in the agent's conversation history from turn 1.
const RuntimeStatusSuffix = `If you determine that you cannot make further progress on this task without human
intervention, or if your work is complete and requires human review, signal the
orchestrator by running:

    mkdir -p .sortie && echo "blocked" > .sortie/status

Use "blocked" when you cannot proceed. Use "needs-human-review" when your work is
complete and awaiting review. Do not write this file during normal productive work.`

// BuildTurnPrompt returns the rendered prompt for a single turn within a
// worker session. turnNumber 1 is the initial turn; turnNumber 2 and above
// are continuation turns. If a continuation turn renders to empty output,
// [DefaultContinuationPrompt] is returned as a fallback.
//
// Safe for concurrent use because the underlying [Template.Render] is safe.
func BuildTurnPrompt(tmpl *Template, issue map[string]any, attempt any, turnNumber, maxTurns int) (string, error) {
	if turnNumber < 1 {
		return "", fmt.Errorf("invalid turn number %d: must be >= 1", turnNumber)
	}

	isContinuation := turnNumber > 1
	rc := RunContext{
		TurnNumber:     turnNumber,
		MaxTurns:       maxTurns,
		IsContinuation: isContinuation,
	}

	rendered, err := tmpl.Render(issue, attempt, rc)
	if err != nil {
		return "", err
	}

	if isContinuation && strings.TrimSpace(rendered) == "" {
		return DefaultContinuationPrompt, nil
	}

	return rendered, nil
}
