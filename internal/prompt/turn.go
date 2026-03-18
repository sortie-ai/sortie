package prompt

import (
	"fmt"
	"strings"
)

// DefaultContinuationPrompt is the fallback prompt returned when a
// workflow template produces empty output on a continuation turn
// (turnNumber > 1). Workflow authors should use
// {{ if .run.is_continuation }} branching to provide project-specific
// continuation guidance; this constant is a safety net for templates
// that omit such branching.
const DefaultContinuationPrompt = "Continue working on this task. Review the current state of your work, check what remains to be done, and proceed with the next step. If you believe the task is complete, verify your changes and confirm completion."

// BuildTurnPrompt returns the rendered prompt for a single turn within a
// worker session. The workflow template is always rendered so that
// authors can branch on run.is_continuation: turn 1 sets
// IsContinuation to false; turns 2+ set it to true. If a continuation
// turn renders to empty output (the template has no is_continuation
// branch), [DefaultContinuationPrompt] is returned as a fallback. Safe
// for concurrent use because the underlying [Template.Render] is safe.
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
