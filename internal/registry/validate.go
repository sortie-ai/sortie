package registry

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// DiagSeverity is the severity level of a [ValidationDiag].
type DiagSeverity string

const (
	// SeverityError marks a diagnostic that blocks dispatch.
	SeverityError DiagSeverity = "error"

	// SeverityWarning marks a diagnostic that is advisory only.
	SeverityWarning DiagSeverity = "warning"
)

// DiagOwnerRepoProject reports the owner/repo format faults of a
// tracker.project value: a value that is not exactly one slash-separated
// owner and repo segment, or a segment that contains whitespace. An empty
// project yields no diagnostic, because the required-field check runs
// earlier in the preflight pipeline.
func DiagOwnerRepoProject(project string) []ValidationDiag {
	if project == "" {
		return nil
	}

	if strings.Count(project, "/") != 1 {
		return []ValidationDiag{{
			Severity: string(SeverityError),
			Check:    "tracker.project.format",
			Message:  `tracker.project must be in owner/repo format (e.g. "sortie-ai/sortie")`,
		}}
	}

	owner, repo, _ := strings.Cut(project, "/")

	if owner == "" || repo == "" {
		return []ValidationDiag{{
			Severity: string(SeverityError),
			Check:    "tracker.project.format",
			Message:  `tracker.project must be in owner/repo format (e.g. "sortie-ai/sortie")`,
		}}
	}

	if typeutil.HasWhitespace(owner) || typeutil.HasWhitespace(repo) {
		return []ValidationDiag{{
			Severity: string(SeverityError),
			Check:    "tracker.project.format",
			Message:  "tracker.project owner and repo must not contain whitespace",
		}}
	}

	return nil
}

// DiagStateLabelElements reports an empty element and an untrimmed
// element of one configured state list: an element that is empty or
// whitespace-only, and, separately, a non-empty element carrying leading
// or trailing whitespace. Both faults never match an issue state, but a
// caller states its own severity, because whether the fault blocks
// construction is a fact about that adapter, not about the check itself.
func DiagStateLabelElements(field string, states []string, sev DiagSeverity) []ValidationDiag {
	var diags []ValidationDiag
	for i, s := range states {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			diags = append(diags, ValidationDiag{
				Severity: string(sev),
				Check:    field + ".empty_element",
				Message:  fmt.Sprintf("%s[%d]: empty state value never matches an issue state", field, i),
			})
			continue
		}
		if trimmed != s {
			diags = append(diags, ValidationDiag{
				Severity: string(sev),
				Check:    field + ".untrimmed_element",
				Message:  fmt.Sprintf("%s[%d]: state value has leading or trailing whitespace and never matches an issue state", field, i),
			})
		}
	}
	return diags
}

// DiagStateOverlap reports labels shared between
// [TrackerConfigFields.ActiveStates] and
// [TrackerConfigFields.TerminalStates], compared case-insensitively.
// Collisions involving a handoff or in-progress state are rejected by the
// generic config validation earlier in the pipeline, so this check does
// not consider them.
func DiagStateOverlap(fields TrackerConfigFields) []ValidationDiag {
	var diags []ValidationDiag

	activeSet := typeutil.LowerSet(fields.ActiveStates)
	terminalSet := typeutil.LowerSet(fields.TerminalStates)

	var overlap []string
	for label := range activeSet {
		if strings.TrimSpace(label) == "" {
			continue
		}
		if _, ok := terminalSet[label]; ok {
			overlap = append(overlap, label)
		}
	}
	slices.Sort(overlap)
	for _, label := range overlap {
		diags = append(diags, ValidationDiag{
			Severity: string(SeverityWarning),
			Check:    "tracker.states.overlap",
			Message:  fmt.Sprintf("tracker.active_states and tracker.terminal_states overlap on %q; an issue in state %q would match both sets", label, label),
		})
	}

	return diags
}
