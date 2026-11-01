package sentence

import (
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/storage"
)

const (
	contextCanceled = "context canceled"
	contextDeadline = "context deadline exceeded"
)

var (
	agentTurnEnded = regexp.MustCompile(`(?s)^agent turn (\d+) ended: (.*)$`)
	agentTurn      = regexp.MustCompile(`(?s)^agent turn (\d+): (.*)$`)
	agentKindChain = regexp.MustCompile(`(?s)^agent: ([a-z_]+): (.*)$`)
	turnStage      = regexp.MustCompile(`(?s)^(prompt render|issue state refresh) \(turn (\d+)\): (.*)$`)

	// errorStems are the prefixes a worker and an adapter put in front of an
	// error's own text; a person reads the sentence in their place.
	errorStems = regexp.MustCompile(`^(?:agent turn \d+: |agent session start: |agent: [a-z_]+: )+`)

	ciFailure    = regexp.MustCompile(`^CI checks failed on ref (.+)$`)
	moneyCeiling = regexp.MustCompile(`^money ceiling reached: used (\S+) of (\S+)$`)
	tokenCeiling = regexp.MustCompile(`^token ceiling reached: used (\d+) of (\d+) tokens$`)
	nonWord      = regexp.MustCompile(`[^\pL\pN]+`)
)

// fillerWords carry no information of their own when a detail is compared
// with the sentence it would follow.
var fillerWords = map[string]bool{
	"a": true, "an": true, "the": true, "you": true, "your": true, "youve": true, "it": true, "its": true,
	"is": true, "are": true, "was": true, "has": true, "have": true, "been": true, "be": true,
	"of": true, "to": true, "hit": true,
}

// agentErrorKinds are the adapter error kinds that have a run-error sentence.
var agentErrorKinds = []domain.AgentErrorKind{
	domain.ErrAgentNotFound, domain.ErrInvalidWorkspaceCwd, domain.ErrResponseTimeout,
	domain.ErrTurnTimeout, domain.ErrPortExit, domain.ErrResponseError, domain.ErrTurnFailed,
	domain.ErrTurnCancelled, domain.ErrTurnInputRequired, domain.ErrTurnIncomplete,
	domain.ErrTurnRefused, domain.ErrTurnOutcomeUnknown, domain.ErrTurnTokenLimit,
	domain.ErrTurnRequestLimit, domain.ErrCredentialUnverified, domain.ErrUsageLimit,
}

// stagePrefixes maps the stage a worker named in front of an error to what
// the worker was doing.
var stagePrefixes = []struct{ prefix, doing string }{
	{"workspace preparation: ", "preparing the workspace"},
	{"mcp config generation: ", "setting up the agent's tool configuration"},
}

// RunError returns the text a person reads for a stored run or retry error.
// A withheld handoff, an agent turn error chain, a worker stage error, a
// failed CI check, a budget stop and the reason the orchestrator cancelled a
// run read as one plain sentence, also when a retry entry wrapped them as a
// worker exit; any other error is returned unchanged. The stored value stays
// the key that run history queries match on.
func RunError(stored string) string {
	unwrapped := strings.TrimPrefix(stored, orchestrator.WorkerExitedErrorPrefix)
	if text, ok := withheldHandoffText(unwrapped); ok {
		return text
	}
	if text, ok := workerErrorText(unwrapped); ok {
		return text
	}
	if text, ok := orchestratorStopText(unwrapped); ok {
		return text
	}
	return stored
}

// orchestratorStopText words the errors the orchestrator itself records: a
// failed CI check, a budget stop, and the reason it cancelled a run.
func orchestratorStopText(stored string) (string, bool) {
	if m := ciFailure.FindStringSubmatch(stored); m != nil {
		return render("run_error_ci_failed", "text", map[string]any{"Ref": m[1]}), true
	}
	if m := moneyCeiling.FindStringSubmatch(stored); m != nil {
		return render("run_error_money_ceiling", "text", map[string]any{"Used": m[1], "Budget": m[2]}), true
	}
	if m := tokenCeiling.FindStringSubmatch(stored); m != nil {
		used, usedErr := strconv.ParseInt(m[1], 10, 64)
		budget, budgetErr := strconv.ParseInt(m[2], 10, 64)
		if usedErr == nil && budgetErr == nil {
			return render("run_error_token_ceiling", "text", map[string]any{"Used": &used, "Budget": &budget}), true
		}
	}
	switch stored {
	case orchestrator.StallCancelError:
		return render("run_error_stall", "text", nil), true
	case orchestrator.ShutdownCancelError:
		return render("run_error_shutdown", "text", nil), true
	case strings.TrimSuffix(orchestrator.InactiveCancelErrorPrefix, ": "):
		return render("run_error_inactive", "text", map[string]any{"State": ""}), true
	}
	if state, ok := strings.CutPrefix(stored, orchestrator.TerminalCancelErrorPrefix); ok {
		return render("run_error_terminal", "text", map[string]any{"State": state}), true
	}
	if state, ok := strings.CutPrefix(stored, orchestrator.InactiveCancelErrorPrefix); ok {
		return render("run_error_inactive", "text", map[string]any{"State": state}), true
	}
	return "", false
}

func withheldHandoffText(stored string) (string, bool) {
	rest, withheld := strings.CutPrefix(stored, storage.HandoffAbsenceErrorPrefix)
	if !withheld {
		return "", false
	}
	switch {
	case strings.HasPrefix(rest, storage.HandoffVerdictAbsence):
		return render("run_error_absence", "text", nil), true
	case strings.HasPrefix(rest, storage.HandoffVerdictUndetermined):
		return render("run_error_undetermined", "text", nil), true
	}
	return renderRunError("run_error_withheld", map[string]any{"Detail": detailOf(rest)}), true
}

func workerErrorText(stored string) (string, bool) {
	if m := agentTurnEnded.FindStringSubmatch(stored); m != nil {
		return turnEndedText(turnWords(m[1]), m[2]), true
	}
	if m := agentTurn.FindStringSubmatch(stored); m != nil {
		return agentErrorText(turnWords(m[1]), false, m[2]), true
	}
	if rest, ok := strings.CutPrefix(stored, "agent session start: "); ok {
		return agentErrorText("the first turn", true, rest), true
	}
	if m := turnStage.FindStringSubmatch(stored); m != nil {
		doing := "building the prompt for turn " + m[2]
		if m[1] == "issue state refresh" {
			doing = "checking the issue's state after turn " + m[2]
		}
		return stageText(doing, m[3]), true
	}
	for _, stage := range stagePrefixes {
		if rest, ok := strings.CutPrefix(stored, stage.prefix); ok {
			return stageText(stage.doing, rest), true
		}
	}
	if rest, ok := strings.CutPrefix(stored, "worker panic: "); ok {
		return renderRunError("run_error_panic", map[string]any{"Detail": detailOf(rest)}), true
	}
	switch stored {
	case contextCanceled:
		return render("run_error_stopped", "text", nil), true
	case contextDeadline:
		return render("run_error_out_of_time", "text", nil), true
	}
	return "", false
}

func turnWords(number string) string {
	return "turn " + number
}

// agentErrorText words the error chain an adapter returned for a turn or a
// session start. An error that is no adapter error reads as a generic agent
// error with the chain's text.
func agentErrorText(turn string, start bool, chain string) string {
	data := map[string]any{"Turn": turn, "Start": start, "Detail": detailOf(chain)}
	m := agentKindChain.FindStringSubmatch(chain)
	if m == nil {
		switch {
		case hasContextError(chain, contextCanceled):
			return render("run_error_turn_cancelled", "text", data)
		case hasContextError(chain, contextDeadline):
			return render("run_error_out_of_time", "text", nil)
		}
		return renderRunError("run_error_agent_other", data)
	}
	data["Detail"] = detailOf(m[2])
	if !slices.Contains(agentErrorKinds, domain.AgentErrorKind(m[1])) {
		return renderRunError("run_error_agent_other", data)
	}
	return renderRunError("run_error_"+m[1], data)
}

func turnEndedText(turn, reason string) string {
	switch reason {
	case "turn_cancelled":
		return render("run_error_turn_cancelled", "text", map[string]any{"Turn": turn})
	case "turn_failed":
		return render("run_error_turn_failed", "text", map[string]any{"Turn": turn})
	}
	return render("run_error_turn_ended", "text", map[string]any{
		"Turn":   turn,
		"Detail": strings.ReplaceAll(reason, "_", " "),
	})
}

func stageText(doing, cause string) string {
	data := map[string]any{"Doing": doing, "Detail": detailOf(cause)}
	switch {
	case hasContextError(cause, contextCanceled):
		return renderRunError("run_error_stage_stopped", data)
	case hasContextError(cause, contextDeadline):
		return renderRunError("run_error_stage_timeout", data)
	}
	return renderRunError("run_error_stage", data)
}

func hasContextError(chain, noise string) bool {
	return chain == noise || strings.HasSuffix(chain, ": "+noise)
}

// detailOf returns the text of an error chain worth showing a person: the
// chain without the worker and adapter stems in front of it, without a closing
// context error, which only says why the chain ended, and without a closing
// period, which the sentence supplies.
func detailOf(chain string) string {
	chain = errorStems.ReplaceAllString(chain, "")
	for _, noise := range []string{contextCanceled, contextDeadline} {
		if chain == noise {
			return ""
		}
		chain = strings.TrimSuffix(chain, ": "+noise)
	}
	return strings.TrimSuffix(strings.TrimSpace(chain), ".")
}

// renderRunError renders a run-error sentence that ends in its detail. A
// detail that only restates the sentence is left out, so the reader gets the
// reason once.
func renderRunError(name string, data map[string]any) string {
	detail, _ := data["Detail"].(string)
	if detail != "" {
		data["Detail"] = ""
		if !restates(render(name, "text", data), detail) {
			data["Detail"] = detail
		}
	}
	return render(name, "text", data)
}

// restates reports whether every word of detail, filler aside, already
// appears in sentence.
func restates(sentence, detail string) bool {
	known := make(map[string]bool)
	for word := range words(sentence) {
		known[word] = true
	}
	for word := range words(detail) {
		if !fillerWords[word] && !known[word] {
			return false
		}
	}
	return true
}

func words(text string) iter.Seq[string] {
	return slices.Values(strings.Fields(nonWord.ReplaceAllString(strings.ReplaceAll(strings.ToLower(text), "'", ""), " ")))
}
