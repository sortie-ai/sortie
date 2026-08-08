package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// giteaPullRequest carries the fields of a Gitea pull request object consumed by
// the mergeability, CI, and review-decision reads. Gitea's mergeable is a plain
// bool with no tri-state signal, so a false value conflates a conflict with an
// in-progress recheck. requested_reviewers is the authoritative pending-review
// signal.
type giteaPullRequest struct {
	Mergeable          bool        `json:"mergeable"`
	Merged             bool        `json:"merged"`
	MergeCommitSHA     *string     `json:"merge_commit_sha"`
	Draft              bool        `json:"draft"`
	RequestedReviewers []giteaUser `json:"requested_reviewers"`
	Head               struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// giteaCombinedStatus is the combined commit-status response. Gitea reports a
// zero total_count with a null statuses array and a spurious "pending" state
// when a commit has no CI, so the empty case is detected by the accumulated
// status count, not total_count or state.
type giteaCombinedStatus struct {
	State      string             `json:"state"`
	TotalCount int                `json:"total_count"`
	Statuses   []giteaCommitState `json:"statuses"`
}

// giteaCommitState is one entry of the combined status statuses array. Status is
// the per-entry outcome read by GetCIStatus; Context, TargetURL, and Description
// are consumed by the CI status provider, which maps each entry to a check run
// and draws the failing-status log excerpt from Description and TargetURL.
type giteaCommitState struct {
	Status      string `json:"status"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

// fetchPullRequest issues GET /repos/{owner}/{repo}/pulls/{prNumber} and decodes
// the response into a [giteaPullRequest].
//
// A transport or HTTP failure is mapped through [giteaToSCMError]; a decode
// failure returns a [*domain.SCMError] of kind [domain.ErrSCMPayload].
func (a *GiteaSCMAdapter) fetchPullRequest(ctx context.Context, prNumber int, owner, repo string) (giteaPullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		return giteaPullRequest{}, giteaToSCMError(err)
	}

	var pr giteaPullRequest
	if jsonErr := json.Unmarshal(body, &pr); jsonErr != nil {
		return giteaPullRequest{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse pull request response",
			Err:     jsonErr,
		}
	}
	return pr, nil
}

// GetMergeability returns the merge precondition state for the given PR,
// populating Draft, Mergeability, HeadSHA, BranchName, and BaseBranch from a
// single pull request read. ReviewDecision and CIConclusion are left unset for
// the caller's dedicated reads.
func (a *GiteaSCMAdapter) GetMergeability(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	pr, err := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return domain.PRMergeStatus{}, err
	}

	// The upstream field is a nullable pointer, so an unmerged PR
	// serializes it as null or omits it; the value is also gated on
	// Merged so a stale or misreported commit id on an open PR is never
	// asserted as a merge.
	var mergeCommitSHA string
	if pr.Merged && pr.MergeCommitSHA != nil {
		mergeCommitSHA = *pr.MergeCommitSHA
	}

	return domain.PRMergeStatus{
		Draft:          pr.Draft,
		Mergeability:   mapMergeability(pr),
		HeadSHA:        pr.Head.SHA,
		BranchName:     pr.Head.Ref,
		BaseBranch:     pr.Base.Ref,
		Merged:         pr.Merged,
		MergeCommitSHA: mergeCommitSHA,
	}, nil
}

// mapMergeability classifies a Gitea pull request into a
// [domain.MergeabilityState]. A draft is blocked and a mergeable PR is clean.
// Any other state is unknown: Gitea's single mergeable bool cannot distinguish a
// conflict from an in-progress recheck, so a non-mergeable non-draft PR is
// reported as unknown rather than dirty.
func mapMergeability(pr giteaPullRequest) domain.MergeabilityState {
	if pr.Draft {
		return domain.MergeabilityBlocked
	}
	if pr.Mergeable {
		return domain.MergeabilityClean
	}
	return domain.MergeabilityUnknown
}

// GetCIStatus returns the aggregated CI conclusion for the PR head commit: one
// of "success", "failing", or "pending", or an empty string when the head
// commit carries no statuses.
//
// The empty case is detected by the accumulated status count, not the top-level
// state, which Gitea reports as "pending" even for a commit with no CI. A failure
// or error status makes the result failing; otherwise a pending status makes it
// pending; success and warning are non-failing. Returns a [*domain.SCMError] on
// failure, including [domain.ErrSCMPayload] when the PR response omits the head
// SHA.
func (a *GiteaSCMAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	pr, err := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}
	if pr.Head.SHA == "" {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "pull request response missing head sha",
		}
	}

	statusPath := fmt.Sprintf("/repos/%s/%s/commits/%s/status",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.Head.SHA))

	const limit = 50
	statuses := make([]giteaCommitState, 0)
	page := 1
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", giteaToSCMError(ctxErr)
		}

		params := url.Values{
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(limit)},
		}
		body, _, statusErr := a.client.Get(ctx, statusPath, params)
		if statusErr != nil {
			return "", giteaToSCMError(statusErr)
		}

		var combined giteaCombinedStatus
		if jsonErr := json.Unmarshal(body, &combined); jsonErr != nil {
			return "", &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse combined status response",
				Err:     jsonErr,
			}
		}

		statuses = append(statuses, combined.Statuses...)
		if len(combined.Statuses) < limit {
			break
		}

		page++
		if page > scmMaxPages {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", statusPath),
				slog.Int("max_pages", scmMaxPages))
			break
		}
	}

	if len(statuses) == 0 {
		return "", nil
	}

	anyFailing := false
	anyPending := false
	for _, s := range statuses {
		switch strings.ToLower(s.Status) {
		case "failure", "error":
			anyFailing = true
		case "pending":
			anyPending = true
		case "success", "warning":
			// A non-failing status contributes no blocking signal.
		}
	}

	if anyFailing {
		return "failing", nil
	}
	if anyPending {
		return "pending", nil
	}
	return "success", nil
}

// giteaMergeOption is the request body for POST
// /repos/{owner}/{repo}/pulls/{index}/merge.
//
// Do carries the merge style and HeadCommitID is the stale-precondition SHA.
// MergeTitle and MergeMessage use Gitea's PascalCase JSON field names and are
// omitted when empty so Gitea applies its strategy-specific defaults.
type giteaMergeOption struct {
	Do           string `json:"Do"`
	HeadCommitID string `json:"head_commit_id,omitempty"`
	MergeTitle   string `json:"MergeTitleField,omitempty"`
	MergeMessage string `json:"MergeMessageField,omitempty"`
}

// mapMergeStrategy maps a [domain.MergeStrategy] to Gitea's merge-style token.
//
// It returns the empty string for any unsupported value so the caller rejects it
// with [domain.ErrSCMPayload] before issuing a request rather than sending a
// token Gitea would refuse.
func mapMergeStrategy(strategy domain.MergeStrategy) string {
	switch strategy {
	case domain.StrategyMerge:
		return "merge"
	case domain.StrategySquash:
		return "squash"
	case domain.StrategyRebase:
		return "rebase"
	default:
		return ""
	}
}

// resolveMergeConflict re-reads the PR after a merge conflict and returns a
// conflict carrying the canonical "already merged" marker when the PR is in fact
// merged.
//
// The marker is gated on the observed merged state rather than on Gitea's 405
// phrasing, so it survives a reworded rejection and never fires on a stale-head
// 409. A failed re-read returns the original conflict unchanged, at most delaying
// the outcome by one retry cycle.
func (a *GiteaSCMAdapter) resolveMergeConflict(ctx context.Context, prNumber int, owner, repo string, scm *domain.SCMError) *domain.SCMError {
	pr, readErr := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if readErr == nil && pr.Merged {
		return &domain.SCMError{
			Kind:    domain.ErrSCMConflict,
			Message: "pull request already merged: " + scm.Message,
			Err:     scm.Err,
		}
	}
	return scm
}
