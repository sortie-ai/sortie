package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
// when a commit has no CI, so the empty case is detected by total_count, not
// state.
type giteaCombinedStatus struct {
	State      string             `json:"state"`
	TotalCount int                `json:"total_count"`
	Statuses   []giteaCommitState `json:"statuses"`
}

// giteaCommitState is one entry of the combined status statuses array.
type giteaCommitState struct {
	Status string `json:"status"`
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

	return domain.PRMergeStatus{
		Draft:        pr.Draft,
		Mergeability: mapMergeability(pr),
		HeadSHA:      pr.Head.SHA,
		BranchName:   pr.Head.Ref,
		BaseBranch:   pr.Base.Ref,
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
// The empty case is detected by a zero total_count, not the top-level state,
// which Gitea reports as "pending" even for a commit with no CI. A failure or
// error status makes the result failing; otherwise a pending status makes it
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
	body, _, statusErr := a.client.Get(ctx, statusPath, nil)
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

	if combined.TotalCount == 0 {
		return "", nil
	}

	anyFailing := false
	anyPending := false
	for _, s := range combined.Statuses {
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
