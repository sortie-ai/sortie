package gitlab

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// gitlabMergeRequest carries the fields of a GitLab merge request object
// consumed by the mergeability, CI, and note-normalization reads.
type gitlabMergeRequest struct {
	IID                 int             `json:"iid"`
	State               string          `json:"state"`
	Draft               bool            `json:"draft"`
	SHA                 string          `json:"sha"`
	SourceBranch        string          `json:"source_branch"`
	TargetBranch        string          `json:"target_branch"`
	MergeCommitSHA      string          `json:"merge_commit_sha"`
	DetailedMergeStatus string          `json:"detailed_merge_status"`
	HeadPipeline        *gitlabPipeline `json:"head_pipeline"`
}

// gitlabPipeline is the head_pipeline object embedded in a merge
// request. The sibling top-level pipeline field is never decoded: it can
// report null while head_pipeline is populated.
type gitlabPipeline struct {
	Status string `json:"status"`
}

// fetchMergeRequest issues GET /projects/{projectPath}/merge_requests/{prNumber}
// and decodes the response into a [gitlabMergeRequest].
//
// A transport or HTTP failure is mapped through [asSCMError]; a decode
// failure returns a [*domain.SCMError] of kind [domain.ErrSCMPayload].
func (a *GitLabSCMAdapter) fetchMergeRequest(ctx context.Context, prNumber int, owner, repo string) (gitlabMergeRequest, error) {
	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		return gitlabMergeRequest{}, asSCMError(err)
	}

	var mr gitlabMergeRequest
	if jsonErr := json.Unmarshal(body, &mr); jsonErr != nil {
		return gitlabMergeRequest{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse merge request response",
			Err:     jsonErr,
		}
	}
	return mr, nil
}

// mapMergeability classifies a GitLab detailed_merge_status value into a
// [domain.MergeabilityState]. recognized is false for any value outside
// the four documented arms, including an empty or absent field, which
// still maps to [domain.MergeabilityBlocked] because every remaining
// value in the live enum is a blocking reason.
// [domain.MergeabilityUnstable] is never returned.
func mapMergeability(status string) (state domain.MergeabilityState, recognized bool) {
	switch status {
	case "mergeable":
		return domain.MergeabilityClean, true
	case "conflict":
		return domain.MergeabilityDirty, true
	case "unchecked", "checking", "preparing", "approvals_syncing":
		return domain.MergeabilityUnknown, true
	default:
		return domain.MergeabilityBlocked, false
	}
}

// GetMergeability returns the merge precondition state for the given PR,
// populating Draft, Mergeability, HeadSHA, BranchName, and BaseBranch
// from a single merge-request read. ReviewDecision and CIConclusion are
// left unset for the caller's dedicated reads.
//
// MergeCommitSHA is populated only when the merge request's state is
// "merged", so an identifier reported on an open merge request is never
// asserted as a merge.
func (a *GitLabSCMAdapter) GetMergeability(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	mr, err := a.fetchMergeRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return domain.PRMergeStatus{}, err
	}

	merged := mr.State == "merged"
	var mergeCommitSHA string
	if merged {
		mergeCommitSHA = mr.MergeCommitSHA
	}

	state, recognized := mapMergeability(mr.DetailedMergeStatus)
	if !recognized {
		a.log.Warn("unrecognized gitlab detailed_merge_status value",
			slog.String("detailed_merge_status", mr.DetailedMergeStatus))
	}

	return domain.PRMergeStatus{
		Draft:          mr.Draft,
		Mergeability:   state,
		HeadSHA:        mr.SHA,
		BranchName:     mr.SourceBranch,
		BaseBranch:     mr.TargetBranch,
		Merged:         merged,
		MergeCommitSHA: mergeCommitSHA,
	}, nil
}

// GetCIStatus returns the merge-gate CI conclusion for the PR head,
// mapped from the merge request's head_pipeline status: one of
// [scmcore.CIGateSuccess], [scmcore.CIGatePending], or
// [scmcore.CIGateFailing], or the empty [scmcore.CIGateAbsent] when the
// head carries no pipeline. The platform already folds externally
// reported commit statuses into head_pipeline, so no second request is
// issued.
func (a *GitLabSCMAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	mr, err := a.fetchMergeRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}
	if mr.HeadPipeline == nil {
		return string(scmcore.CIGateAbsent), nil
	}

	switch mr.HeadPipeline.Status {
	case "success":
		return string(scmcore.CIGateSuccess), nil
	case "failed", "canceled":
		return string(scmcore.CIGateFailing), nil
	default:
		return string(scmcore.CIGatePending), nil
	}
}
