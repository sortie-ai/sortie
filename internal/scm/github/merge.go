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
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
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
		return pullRequestResponse{}, scmcore.ToSCMError(err)
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

// combinedStatusRun is one entry of the GitHub combined commit-status
// response's statuses array.
type combinedStatusRun struct {
	State string `json:"state"`
}

// combinedStatusResponse captures the GitHub combined commit-status
// response (state plus the list of statuses).
type combinedStatusResponse struct {
	State    string              `json:"state"`
	Statuses []combinedStatusRun `json:"statuses"`
}

// decodeCombinedStatusPage decodes one page of the combined commit-status
// response and returns its statuses array.
func decodeCombinedStatusPage(body []byte) ([]combinedStatusRun, error) {
	var combined combinedStatusResponse
	if err := json.Unmarshal(body, &combined); err != nil {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse combined status response",
			Err:     err,
		}
	}
	return combined.Statuses, nil
}

// decodeCheckRunsPage decodes one page of the check-runs response and
// returns its check runs array.
func decodeCheckRunsPage(body []byte) ([]githubCheckRun, error) {
	var checks checkRunsResponse
	if err := json.Unmarshal(body, &checks); err != nil {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse check runs response",
			Err:     err,
		}
	}
	return checks.CheckRuns, nil
}

// asSCMError returns err unchanged when it is already a [*domain.SCMError]
// (a decode failure, already classified at the point of failure), and
// converts it through [scmcore.ToSCMError] otherwise (a transport or HTTP
// failure, which [httpkit.Paginator.All] returns unconverted).
func asSCMError(err error) *domain.SCMError {
	var scmErr *domain.SCMError
	if errors.As(err, &scmErr) {
		return scmErr
	}
	return scmcore.ToSCMError(err)
}

// mapCombinedStatusRun normalizes one combined-status entry's state to a
// [domain.CheckRun]. It is distinct from [mapCheckConclusion], the
// check-runs vocabulary: "error" completes as failing here, which
// [mapCheckConclusion] would default to pending, since the two routes use
// different wire vocabularies for the same underlying gate. Any state
// other than "success", "failure", or "error", including "pending" and an
// empty value, defers rather than passing.
func mapCombinedStatusRun(state string) domain.CheckRun {
	switch strings.ToLower(state) {
	case "success":
		return domain.CheckRun{Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionSuccess}
	case "failure", "error":
		return domain.CheckRun{Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure}
	default:
		return domain.CheckRun{Status: domain.CheckRunStatusInProgress, Conclusion: domain.CheckConclusionPending}
	}
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

	// Combined commit status, walked across every page so a commit
	// carrying more than one page of statuses contributes all of them to
	// the verdict below, not just the first page.
	statusPath := fmt.Sprintf("/repos/%s/%s/commits/%s/status",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.Head.SHA))
	statusPaginator := httpkit.NewPagePaginator(a.client, statusPath, url.Values{}, decodeCombinedStatusPage, httpkit.PageOptions{
		PageParam: "page",
		SizeParam: "per_page",
		PageSize:  100,
		MaxPages:  maxCIPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", statusPath),
				slog.Int("max_pages", limit))
		},
	})
	statuses, statusErr := statusPaginator.All(ctx)
	if statusErr != nil {
		return "", asSCMError(statusErr)
	}

	// Check runs, walked the same way and for the same reason.
	checksPath := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.Head.SHA))
	checksPaginator := httpkit.NewPagePaginator(a.client, checksPath, url.Values{}, decodeCheckRunsPage, httpkit.PageOptions{
		PageParam: "page",
		SizeParam: "per_page",
		PageSize:  100,
		MaxPages:  maxCIPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", checksPath),
				slog.Int("max_pages", limit))
		},
	})
	checkRuns, checksErr := checksPaginator.All(ctx)
	if checksErr != nil {
		return "", asSCMError(checksErr)
	}

	runs := make([]domain.CheckRun, 0, len(statuses)+len(checkRuns))
	for _, s := range statuses {
		runs = append(runs, mapCombinedStatusRun(s.State))
	}
	for _, cr := range checkRuns {
		runs = append(runs, domain.CheckRun{
			Status:     mapCheckRunStatus(cr.Status),
			Conclusion: mapCheckConclusion(cr.Conclusion),
		})
	}

	return string(scmcore.MergeGate(runs)), nil
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

	// The REST pull request object no longer carries merge_commit_sha under
	// the pinned API version, so the identifier is sourced from a second,
	// GraphQL read. The Merged gate that used to guard against reading
	// GitHub's test-merge value out of merge_commit_sha is retained here: it
	// now also bounds the extra request to merged pull requests only. The
	// test-merge commit itself is structurally unreachable through this
	// path, because fetchMergeCommitOID's query requests only mergeCommit.
	var mergeCommitSHA string
	if pr.Merged {
		mergeCommitSHA, err = a.fetchMergeCommitOID(ctx, prNumber, owner, repo)
		if err != nil {
			return domain.PRMergeStatus{}, err
		}
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

// fetchMergeCommitOID returns the merge commit identifier for a merged
// pull request, read from GraphQL PullRequest.mergeCommit.oid. This
// substitutes for the merge_commit_sha REST property, which the pinned
// API version removes from every pull request payload. The empty string
// is returned, with a nil error, when the provider reports no merge
// commit for the pull request.
func (a *GitHubSCMAdapter) fetchMergeCommitOID(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	variables := map[string]any{
		"owner":  owner,
		"repo":   repo,
		"number": prNumber,
	}
	envelope := graphqlResponseEnvelope[mergeCommitResponseData]{}
	if err := a.postGraphQL(ctx, mergeCommitQuery, variables, &envelope); err != nil {
		return "", err
	}

	pr := envelope.Data.Repository.PullRequest
	if pr == nil {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "graphql response missing pullRequest payload",
		}
	}
	if pr.MergeCommit == nil {
		return "", nil
	}

	return pr.MergeCommit.OID, nil
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
		scm := scmcore.AsMergeConflict(scmcore.ToSCMError(err))
		if scm.Kind == domain.ErrSCMConflict {
			return domain.MergeResult{}, a.resolveMergeConflict(ctx, prNumber, owner, repo, scm)
		}
		return domain.MergeResult{}, scm
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

// resolveMergeConflict re-reads the PR after a merge conflict and returns a
// conflict carrying the canonical already-merged marker when the PR is in
// fact merged.
//
// The marker is gated on the observed merged state rather than on GitHub's
// rejection wording, so it survives a reworded rejection and never fires on
// a stale-head 409. A failed re-read returns the original conflict
// unchanged, at most delaying the outcome by one retry cycle.
func (a *GitHubSCMAdapter) resolveMergeConflict(ctx context.Context, prNumber int, owner, repo string, scm *domain.SCMError) *domain.SCMError {
	pr, readErr := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if readErr == nil && pr.Merged {
		return scmcore.AlreadyMergedConflict(scm.Message, scm.Err)
	}
	return scm
}

// DeleteBranch deletes the branch reference. Returns nil on success.
// Returns *SCMError with Kind ErrSCMNotFound when the branch is
// already gone; callers treat this as a successful no-op.
func (a *GitHubSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		scm := scmcore.ToSCMError(err)
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
		return nil, nil, scmcore.ToSCMError(err)
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
