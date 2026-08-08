package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// GetReviewDecision returns the platform's authoritative review
// decision for the given PR. The value is read from
// PullRequest.reviewDecision on the GraphQL endpoint and mapped onto
// the four domain.ReviewDecision constants. A null platform answer is
// mapped to ReviewDecisionNotRequired; an unknown enum value is mapped
// to ReviewDecisionNotRequired and logged at warn level so an operator
// can detect a platform-side enum extension.
func (a *GitHubSCMAdapter) GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error) {
	variables := map[string]any{
		"owner":  owner,
		"repo":   repo,
		"number": prNumber,
	}
	envelope := graphqlResponseEnvelope[reviewDecisionResponseData]{}
	if err := a.postGraphQL(ctx, reviewDecisionQuery, variables, &envelope); err != nil {
		return "", err
	}

	pr := envelope.Data.Repository.PullRequest
	if pr == nil {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "graphql response missing pullRequest payload",
		}
	}

	mapped, known := mapReviewDecision(pr.ReviewDecision)
	if !known {
		slog.WarnContext(ctx, "graphql review decision returned unknown value",
			slog.Int("pr_number", prNumber),
			slog.String("owner", owner),
			slog.String("repo", repo),
			slog.String("decision", *pr.ReviewDecision),
		)
	}
	return mapped, nil
}

// pullRequestResponse captures the subset of GitHub pull request fields
// needed for mergeability, draft state, head SHA, and head branch.
type pullRequestResponse struct {
	Draft          bool   `json:"draft"`
	MergeableState string `json:"mergeable_state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// fetchPullRequest issues GET /repos/{owner}/{repo}/pulls/{prNumber}
// and decodes the response into pullRequestResponse.
func (a *GitHubSCMAdapter) fetchPullRequest(ctx context.Context, prNumber int, owner, repo string) (pullRequestResponse, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		return pullRequestResponse{}, toSCMError(err)
	}
	var pr pullRequestResponse
	if jsonErr := json.Unmarshal(body, &pr); jsonErr != nil {
		return pullRequestResponse{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse pull request response",
			Err:     jsonErr,
		}
	}
	return pr, nil
}

// combinedStatusResponse captures the GitHub combined commit-status
// response (state plus the list of statuses).
type combinedStatusResponse struct {
	State    string `json:"state"`
	Statuses []struct {
		State string `json:"state"`
	} `json:"statuses"`
}

// GetCIStatus returns the aggregated CI conclusion string for the PR
// head ref. Empty string means no required checks exist on the PR.
func (a *GitHubSCMAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
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

	// Combined commit status.
	statusPath := fmt.Sprintf("/repos/%s/%s/commits/%s/status",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.Head.SHA))
	statusBody, _, statusErr := a.client.Get(ctx, statusPath, nil)
	if statusErr != nil {
		return "", toSCMError(statusErr)
	}
	var combined combinedStatusResponse
	if jsonErr := json.Unmarshal(statusBody, &combined); jsonErr != nil {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse combined status response",
			Err:     jsonErr,
		}
	}

	// Check runs.
	checksPath := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.Head.SHA))
	checksBody, _, checksErr := a.client.Get(ctx, checksPath, nil)
	if checksErr != nil {
		return "", toSCMError(checksErr)
	}
	var checks checkRunsResponse
	if jsonErr := json.Unmarshal(checksBody, &checks); jsonErr != nil {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse check runs response",
			Err:     jsonErr,
		}
	}

	hasAnySignal := len(combined.Statuses) > 0 || checks.TotalCount > 0
	if !hasAnySignal {
		return "", nil
	}

	anyFailing := false
	anyPending := false

	for _, s := range combined.Statuses {
		switch strings.ToLower(s.State) {
		case "failure", "error":
			anyFailing = true
		case "pending":
			anyPending = true
		}
	}

	for _, cr := range checks.CheckRuns {
		status := strings.ToLower(cr.Status)
		if status == "in_progress" || status == "queued" || status == "pending" {
			anyPending = true
			continue
		}
		if cr.Conclusion == nil {
			continue
		}
		switch strings.ToLower(*cr.Conclusion) {
		case "failure", "timed_out", "cancelled", "action_required":
			anyFailing = true
		case "success", "neutral", "skipped":
			// passing or non-failing conclusion; no signal.
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

// GetMergeability returns the PR merge precondition status. The
// ReviewDecision and CIConclusion fields are left unset; callers
// obtain those values from the dedicated reads.
func (a *GitHubSCMAdapter) GetMergeability(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	pr, err := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return domain.PRMergeStatus{}, err
	}

	mergeability := mapMergeableState(pr.MergeableState)

	// GitHub reports a test-merge commit in merge_commit_sha for an open
	// PR, so the value is gated on Merged to avoid asserting a merge
	// that has not happened.
	var mergeCommitSHA string
	if pr.Merged {
		mergeCommitSHA = pr.MergeCommitSHA
	}

	return domain.PRMergeStatus{
		Draft:          pr.Draft,
		Mergeability:   mergeability,
		HeadSHA:        pr.Head.SHA,
		BranchName:     pr.Head.Ref,
		BaseBranch:     pr.Base.Ref,
		Merged:         pr.Merged,
		MergeCommitSHA: mergeCommitSHA,
	}, nil
}

// mapMergeableState translates the GitHub mergeable_state field to the
// domain MergeabilityState enum.
func mapMergeableState(s string) domain.MergeabilityState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "clean":
		return domain.MergeabilityClean
	case "unstable":
		return domain.MergeabilityUnstable
	case "blocked", "behind", "draft":
		return domain.MergeabilityBlocked
	case "dirty":
		return domain.MergeabilityDirty
	default:
		return domain.MergeabilityUnknown
	}
}

// mergeRequestBody is the JSON request body sent to PUT
// /repos/{owner}/{repo}/pulls/{prNumber}/merge. Optional fields are
// omitted when empty so GitHub uses its strategy-specific defaults.
type mergeRequestBody struct {
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	SHA           string `json:"sha,omitempty"`
	MergeMethod   string `json:"merge_method"`
}

// mergeResponse captures the GitHub merge response payload.
type mergeResponse struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// MergePR merges the given PR with the requested strategy. The
// expectedHeadSHA is sent as the precondition SHA when non-empty so
// the server rejects with HTTP 409 if the PR head moved.
func (a *GitHubSCMAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	body := mergeRequestBody{
		CommitTitle:   commitTitle,
		CommitMessage: commitMessage,
		SHA:           expectedHeadSHA,
		MergeMethod:   string(strategy),
	}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to encode merge request body",
			Err:     marshalErr,
		}
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)
	respBody, err := a.client.Send(ctx, http.MethodPut, path, bytes.NewReader(payload))
	if err != nil {
		return domain.MergeResult{}, toSCMError(err)
	}

	var mr mergeResponse
	if jsonErr := json.Unmarshal(respBody, &mr); jsonErr != nil {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse merge response",
			Err:     jsonErr,
		}
	}

	if !mr.Merged {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMConflict,
			Message: "merge endpoint returned merged=false",
		}
	}

	return domain.MergeResult{
		SHA:     mr.SHA,
		Merged:  true,
		Message: mr.Message,
	}, nil
}

// DeleteBranch deletes the branch reference. Returns nil on success.
// Returns *SCMError with Kind ErrSCMNotFound when the branch is
// already gone; callers treat this as a successful no-op.
func (a *GitHubSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		scm := toSCMError(err)
		return scm
	}
	return nil
}

// AutoMergeRequiredPullRequestsScope is the canonical scope name for
// the merge endpoint on GitHub fine-grained tokens.
const AutoMergeRequiredPullRequestsScope = "pull_requests:write"

// AutoMergeRequiredContentsScope is the canonical scope name for the
// branch-delete endpoint on GitHub fine-grained tokens.
const AutoMergeRequiredContentsScope = "contents:write"

// legacyRepoScope is the classic PAT scope that covers both
// AutoMergeRequiredPullRequestsScope and AutoMergeRequiredContentsScope.
const legacyRepoScope = "repo"

// VerifyAutoMergeScopes calls GET /rate_limit and inspects the
// X-OAuth-Scopes response header to determine whether the configured
// token carries the scopes required for auto-merge. The first return
// is the full granted scope list; the second is the subset of required
// scopes that are missing. An empty missing list with a nil scopes
// slice (when the header is absent or empty) signals "unable to
// verify": the caller MUST fail open and rely on runtime auth checks.
// A non-empty scopes slice with an empty missing list means the token
// has sufficient scope.
func (a *GitHubSCMAdapter) VerifyAutoMergeScopes(ctx context.Context, requireContents bool) ([]string, []string, error) {
	_, header, err := a.client.Get(ctx, "/rate_limit", nil)
	if err != nil {
		// Unwrap to ensure callers can detect transport vs API class.
		var scmErr *domain.SCMError
		if errors.As(err, &scmErr) {
			return nil, nil, scmErr
		}
		return nil, nil, toSCMError(err)
	}

	// Fine-grained PATs and GitHub App installation tokens do not
	// populate X-OAuth-Scopes; treat absent or empty as "unable to
	// verify" by returning nil scopes and nil missing. The caller
	// distinguishes this from a verified result by checking len(scopes).
	scopesHeader := header.Get("X-OAuth-Scopes")
	if strings.TrimSpace(scopesHeader) == "" {
		return nil, nil, nil
	}
	scopes := splitScopes(scopesHeader)
	if len(scopes) == 0 {
		return nil, nil, nil
	}

	required := []string{AutoMergeRequiredPullRequestsScope}
	if requireContents {
		required = append(required, AutoMergeRequiredContentsScope)
	}

	scopeSet := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		scopeSet[s] = struct{}{}
	}

	// Legacy repo scope is a superset that covers both fine-grained
	// permissions; treat its presence as satisfying every requirement.
	if _, hasRepo := scopeSet[legacyRepoScope]; hasRepo {
		return scopes, nil, nil
	}

	var missing []string
	for _, want := range required {
		if _, ok := scopeSet[want]; !ok {
			missing = append(missing, want)
		}
	}
	return scopes, missing, nil
}

// splitScopes parses a comma-separated X-OAuth-Scopes header value
// into a normalized, trimmed list of scope tokens. Empty entries are
// dropped.
func splitScopes(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
