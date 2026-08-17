package gitlab

import (
	"encoding/json"
	"net/url"

	"github.com/sortie-ai/sortie/internal/domain"
)

// gitlabCommitStatus is one entry of the commit-status list. The list
// mixes runner jobs and externally reported statuses, and no field
// separates them, so ID is what the log-excerpt read tests against the
// pipeline's job set. PipelineID is what the scope filter tests against
// the pipeline the read asked for, and both the log-excerpt read and the
// merge gate apply that filter.
type gitlabCommitStatus struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure"`
	TargetURL    string `json:"target_url"`
	PipelineID   int64  `json:"pipeline_id"`
}

// mapJobOutcome maps a commit-status entry's status and allow_failure to
// its normalized conclusion and run status. recognized is false when
// status matches no known value, in which case the pair is the same
// [domain.CheckConclusionPending] / [domain.CheckRunStatusInProgress]
// deferral the "running" arm produces, so an unrecognized status never
// settles the aggregate.
func mapJobOutcome(status string, allowFailure bool) (domain.CheckConclusion, domain.CheckRunStatus, bool) {
	switch status {
	case "success":
		return domain.CheckConclusionSuccess, domain.CheckRunStatusCompleted, true
	case "failed":
		if allowFailure {
			return domain.CheckConclusionNeutral, domain.CheckRunStatusCompleted, true
		}
		return domain.CheckConclusionFailure, domain.CheckRunStatusCompleted, true
	case "canceled":
		return domain.CheckConclusionCancelled, domain.CheckRunStatusCompleted, true
	case "skipped":
		return domain.CheckConclusionSkipped, domain.CheckRunStatusCompleted, true
	case "manual":
		return domain.CheckConclusionNeutral, domain.CheckRunStatusCompleted, true
	case "created", "pending", "waiting_for_resource", "waiting_for_callback", "preparing", "scheduled":
		return domain.CheckConclusionPending, domain.CheckRunStatusQueued, true
	case "running", "canceling":
		return domain.CheckConclusionPending, domain.CheckRunStatusInProgress, true
	default:
		return domain.CheckConclusionPending, domain.CheckRunStatusInProgress, false
	}
}

// commitStatusesPath builds the commit-status route for an
// already-percent-encoded project segment and a commit SHA. sha is
// percent-encoded inside this function.
func commitStatusesPath(project, sha string) string {
	return "/projects/" + project + "/repository/commits/" + url.PathEscape(sha) + "/statuses"
}

// scopeToPipeline returns, in input order, every entry of entries whose
// PipelineID equals pipelineID. It accepts any input including nil and
// always returns a non-nil slice. docs/gitlab-adapter-notes.md records
// this filter as load-bearing rather than defensive: a commit carrying
// two pipelines returns both pipelines' entries unfiltered on the wire.
func scopeToPipeline(entries []gitlabCommitStatus, pipelineID int64) []gitlabCommitStatus {
	scoped := make([]gitlabCommitStatus, 0, len(entries))
	for _, entry := range entries {
		if entry.PipelineID == pipelineID {
			scoped = append(scoped, entry)
		}
	}
	return scoped
}

// normalizeCommitStatuses maps each entry of entries to a
// [domain.CheckRun] through [mapJobOutcome], preserving input order and
// length. runs is a non-nil slice, empty rather than nil for a nil or
// empty input. unrecognizedExample is the status of the first entry
// mapJobOutcome did not recognize, or the empty string when every entry
// was recognized; unrecognizedCount is how many entries it did not
// recognize. normalizeCommitStatuses takes no logger; the caller emits
// the WARN for an unrecognized status.
func normalizeCommitStatuses(entries []gitlabCommitStatus) (runs []domain.CheckRun, unrecognizedExample string, unrecognizedCount int) {
	runs = make([]domain.CheckRun, len(entries))
	for i, entry := range entries {
		conclusion, runStatus, recognized := mapJobOutcome(entry.Status, entry.AllowFailure)
		if !recognized {
			unrecognizedCount++
			if unrecognizedExample == "" {
				unrecognizedExample = entry.Status
			}
		}
		runs[i] = domain.CheckRun{
			Name:       entry.Name,
			Status:     runStatus,
			Conclusion: conclusion,
			DetailsURL: entry.TargetURL,
		}
	}
	return runs, unrecognizedExample, unrecognizedCount
}

// decodeCommitStatusPage JSON-decodes one page body of the commit-status
// route into its entries. A decode failure returns a *[domain.SCMError]
// of kind [domain.ErrSCMPayload]. This is the SCM-side counterpart to the
// CI provider's own inline [*domain.CIError] decoder for the same route:
// the two stay separate because the two readers return different error
// types.
func decodeCommitStatusPage(body []byte) ([]gitlabCommitStatus, error) {
	var page []gitlabCommitStatus
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse commit statuses response",
			Err:     err,
		}
	}
	return page, nil
}
