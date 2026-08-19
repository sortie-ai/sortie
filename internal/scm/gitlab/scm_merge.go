package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

	// Labels carries the merge request's label names in their stored
	// casing, which RemoveLabel matches case-insensitively before it
	// sends the stored spelling back.
	Labels []string `json:"labels"`
}

// gitlabPipeline is the head_pipeline object embedded in a merge
// request. The sibling top-level pipeline field is never decoded: it can
// report null while head_pipeline is populated.
//
// ID and SHA address the pipeline's own job set. The merge request's own
// sha is not interchangeable with SHA: nothing re-validates head_pipeline
// against the current head, so the two can disagree.
//
// Ref and Source together recognize a pipeline the platform generated for
// this merge request and ran on a ref whose commit exists in neither
// branch. Neither field carries that meaning alone: a branch name can
// imitate a generated ref and the platform reports Ref verbatim, and a
// detached merge-request pipeline also reports Source as a
// platform-generated value while its SHA equals the merge request's own.
type gitlabPipeline struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Ref    string `json:"ref"`
	Source string `json:"source"`
}

// fetchMergeRequest issues GET /projects/{projectPath}/merge_requests/{prNumber}
// and decodes the response into a [gitlabMergeRequest].
//
// A transport or HTTP failure is mapped through [scmcore.AsSCMError]; a
// decode failure returns a [*domain.SCMError] of kind [domain.ErrSCMPayload].
func (a *GitLabSCMAdapter) fetchMergeRequest(ctx context.Context, prNumber int, owner, repo string) (gitlabMergeRequest, error) {
	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		return gitlabMergeRequest{}, scmcore.AsSCMError(err)
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

// gitlabExpectedBlockingStatuses holds the wire values GitLab's REST
// surface reports for its mergeability checks. These are the check
// services' own identifiers, not the names their GraphQL enum counterparts
// carry, so a future edit correcting a spelling toward the GraphQL
// vocabulary would reintroduce the drift this set closes.
var gitlabExpectedBlockingStatuses = map[string]struct{}{
	"ci_must_pass":                   {},
	"ci_still_running":               {},
	"commits_status":                 {},
	"discussions_not_resolved":       {},
	"draft_status":                   {},
	"locked_lfs_files":               {},
	"merge_time":                     {},
	"need_rebase":                    {},
	"not_open":                       {},
	"jira_association_missing":       {},
	"locked_paths":                   {},
	"merge_request_blocked":          {},
	"not_approved":                   {},
	"requested_changes":              {},
	"security_policy_pipeline_check": {},
	"security_policy_violations":     {},
	"status_checks_must_pass":        {},
	"title_regex":                    {},
}

// mapMergeability classifies a GitLab detailed_merge_status value into a
// [domain.MergeabilityState]. The classification covers every value the
// platform documents for this field: expected reports whether status is
// one of them. A value outside that set still maps to
// [domain.MergeabilityBlocked], because every documented value other
// than the affirmative and computing ones is a blocking reason.
// [domain.MergeabilityUnstable] is never returned.
func mapMergeability(status string) (state domain.MergeabilityState, expected bool) {
	switch status {
	case "mergeable":
		return domain.MergeabilityClean, true
	case "conflict":
		return domain.MergeabilityDirty, true
	case "unchecked", "checking", "preparing", "approvals_syncing":
		return domain.MergeabilityUnknown, true
	}
	if _, ok := gitlabExpectedBlockingStatuses[status]; ok {
		return domain.MergeabilityBlocked, true
	}
	return domain.MergeabilityBlocked, false
}

// GetMergeability returns the merge precondition state for the given PR,
// populating Draft, Mergeability, HeadSHA, BranchName, and BaseBranch
// from a single merge-request read. ReviewDecision and CIConclusion are
// left unset for the caller's dedicated reads.
//
// MergeCommitSHA is populated only when the merge request's state is
// "merged", so an identifier reported on an open merge request is never
// asserted as a merge.
//
// A detailed_merge_status value outside the platform's documented set,
// including an absent or empty field, produces one WARN naming the
// observed value. A documented value that resolves to
// [domain.MergeabilityBlocked] produces one Debug record naming the
// observed value and the merge request; the record reports what GitLab
// returned, not that a block exists, since the same value can be the one
// a later read observes on a merge request that has since merged.
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

	state, expected := mapMergeability(mr.DetailedMergeStatus)
	if !expected {
		a.log.Warn("unrecognized gitlab detailed_merge_status value",
			slog.String("detailed_merge_status", mr.DetailedMergeStatus))
	} else if state == domain.MergeabilityBlocked {
		a.log.Debug("gitlab reported a non-mergeable detailed_merge_status",
			slog.String("detailed_merge_status", mr.DetailedMergeStatus),
			slog.Int("pr_number", prNumber))
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

// fetchPipelineStatuses walks every page of the commit-status route for
// sha, scoped to pipelineID both on the wire, via the pipeline_id query
// parameter, and again after decoding, through [scopeToPipeline].
//
// sha is percent-encoded inside [commitStatusesPath]. A decode failure
// returns a [*domain.SCMError] of kind [domain.ErrSCMPayload]; any other
// error is whatever [scmcore.AsSCMError] yields for the underlying
// transport or HTTP failure, propagated unchanged from [paginateSCM].
func (a *GitLabSCMAdapter) fetchPipelineStatuses(ctx context.Context, owner, repo, sha string, pipelineID int64) ([]gitlabCommitStatus, error) {
	path := commitStatusesPath(projectPath(owner, repo), sha)
	params := url.Values{"pipeline_id": {strconv.FormatInt(pipelineID, 10)}}

	entries, err := paginateSCM(ctx, a.client, a.log, path, params, decodeCommitStatusPage)
	if err != nil {
		return nil, err
	}
	return scopeToPipeline(entries, pipelineID), nil
}

// mapPipelineStatus classifies a GitLab head_pipeline.status value into
// the merge-gate vocabulary. recognized is false for any value outside
// the platform's pipeline-status enum, including the empty string, in
// which case the returned gate still holds at [scmcore.CIGatePending]
// rather than guessing a more permissive verdict.
//
// manual sits in the CIGatePending arm by name rather than by falling
// through the default: it is the conservative answer this classifier
// gives when asked about a status its merge-gate caller, [GitLabSCMAdapter.GetCIStatus],
// resolves for itself from that pipeline's own job set, rather than a
// value the gate holds until the shape settles on its own.
//
// canceled reports no conclusion about the code: the pipeline reached no
// result, so the gate answers what folding that pipeline's own jobs
// would answer, which is [scmcore.CIGatePending] unless a sibling job
// failed.
func mapPipelineStatus(status string) (gate scmcore.CIGate, recognized bool) {
	switch status {
	case "success", "skipped":
		return scmcore.CIGateSuccess, true
	case "failed":
		return scmcore.CIGateFailing, true
	case "created", "waiting_for_resource", "preparing", "waiting_for_callback",
		"pending", "running", "canceling", "canceled", "scheduled", "manual":
		return scmcore.CIGatePending, true
	default:
		return scmcore.CIGatePending, false
	}
}

// isGeneratedMergeRefPipeline reports whether pipeline is one the
// platform generated for merge request prNumber and ran on a ref whose
// commit exists in neither branch: a merged-results pipeline or a
// merge-train pipeline. It returns false for every other non-nil
// pipeline, including one with an empty ref or an empty source, and one
// generated for a different merge request. pipeline must not be nil.
//
// The comparison is an exact whole-string match on both source and ref,
// never a prefix or suffix test over ref alone: a branch can be named
// after a generated ref, and only source is a value a repository writer
// cannot forge.
func isGeneratedMergeRefPipeline(pipeline *gitlabPipeline, prNumber int) bool {
	if pipeline.Source != "merge_request_event" {
		return false
	}
	prefix := "refs/merge-requests/" + strconv.Itoa(prNumber) + "/"
	return pipeline.Ref == prefix+"merge" || pipeline.Ref == prefix+"train"
}

// GetCIStatus returns the merge-gate CI conclusion for the PR head,
// mapped from the merge request's head_pipeline status through
// [mapPipelineStatus]: one of [scmcore.CIGateSuccess],
// [scmcore.CIGatePending], or [scmcore.CIGateFailing], or the empty
// [scmcore.CIGateAbsent] when the head carries no pipeline. The mapping
// applies only to a head pipeline that describes the merge request's
// current head, established by comparing head_pipeline.sha against the
// merge request's own sha; a merged-results or merge-train pipeline
// generated for this merge request is exempt from that comparison,
// through [isGeneratedMergeRefPipeline], because no REST field relates
// its SHA to the merge request head. A head pipeline that is not exempt
// and describes a superseded commit resolves to CIGatePending with one
// WARN naming both SHAs and the pipeline id, before the manual arm is
// considered. A merge request whose response carries a head
// pipeline but no head SHA of its own fails as a [*domain.SCMError] of
// kind [domain.ErrSCMPayload], since head identity cannot be established
// against an absent head. A skipped pipeline resolves to CIGateSuccess,
// since the platform reports it only for a job set with no failing
// conclusion. A manual pipeline resolves to the verdict computed over
// that pipeline's own job set, through [GitLabSCMAdapter.manualGate]; an
// empty job set holds at CIGatePending rather than reporting the absent
// value. A status outside the recognized set logs one WARN naming the
// observed value so a stalled gate is diagnosable at the tick it stalls.
// The platform already folds externally reported commit statuses into
// head_pipeline for the twelve other statuses, so no second request is
// issued for them; manual is the exception that pays for one job-set
// read.
func (a *GitLabSCMAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	mr, err := a.fetchMergeRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}
	if mr.HeadPipeline == nil {
		return string(scmcore.CIGateAbsent), nil
	}
	if mr.SHA == "" {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "merge request response missing head sha",
		}
	}

	if isGeneratedMergeRefPipeline(mr.HeadPipeline, prNumber) {
		a.log.Debug("head pipeline runs on a generated merge-request ref",
			slog.String("pipeline_ref", mr.HeadPipeline.Ref),
			slog.Int64("pipeline_id", mr.HeadPipeline.ID),
			slog.String("pipeline_source", mr.HeadPipeline.Source))
	} else if !strings.EqualFold(mr.HeadPipeline.SHA, mr.SHA) {
		a.log.Warn("head pipeline does not describe the merge request head",
			slog.String("merge_request_sha", mr.SHA),
			slog.String("head_pipeline_sha", mr.HeadPipeline.SHA),
			slog.Int64("pipeline_id", mr.HeadPipeline.ID))
		return string(scmcore.CIGatePending), nil
	}

	if mr.HeadPipeline.Status == "manual" {
		return a.manualGate(ctx, owner, repo, mr.HeadPipeline)
	}

	gate, recognized := mapPipelineStatus(mr.HeadPipeline.Status)
	if !recognized {
		a.log.Warn("unrecognized gitlab pipeline status",
			slog.String("pipeline_status", mr.HeadPipeline.Status))
	}
	return string(gate), nil
}

// manualGate resolves the merge-gate verdict for a manual head pipeline
// from that pipeline's own job set, folded through [scmcore.MergeGate].
//
// Its caller, [GitLabSCMAdapter.GetCIStatus], establishes head identity,
// the head-sha comparison and the generated-merge-ref exemption, before
// calling manualGate, so on the ordinary matching path the empty-sha half
// of the guard below is defense in depth rather than the route by which
// this function is ordinarily reached. The exempt path skips that
// comparison entirely and is the ordinary way to reach manualGate with an
// empty pipeline sha.
//
// A missing sha or a zero id returns a [*domain.SCMError] of kind
// [domain.ErrSCMPayload] and issues no request: the platform answers a
// zero pipeline scope with an empty set rather than an error, which would
// otherwise make the anomaly invisible. A job-set read failure is
// returned unchanged, never degraded to a verdict. An unrecognized job
// status logs one WARN naming it. A scoped job set that folds to
// [scmcore.CIGateAbsent] logs one WARN naming the pipeline id and holds
// at [scmcore.CIGatePending] rather than reporting the absent value,
// because the platform never reports manual for a job set with no
// entries, so an empty scoped set here means the read was mis-addressed.
func (a *GitLabSCMAdapter) manualGate(ctx context.Context, owner, repo string, pipeline *gitlabPipeline) (string, error) {
	if pipeline.SHA == "" || pipeline.ID == 0 {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "head pipeline missing sha or id",
		}
	}

	entries, err := a.fetchPipelineStatuses(ctx, owner, repo, pipeline.SHA, pipeline.ID)
	if err != nil {
		return "", err
	}

	runs, unrecognizedExample, unrecognizedCount := normalizeCommitStatuses(entries)
	if unrecognizedCount > 0 {
		a.log.Warn("unrecognized gitlab job status",
			slog.String("status", unrecognizedExample),
			slog.Int("count", unrecognizedCount))
	}

	gate := scmcore.MergeGate(runs)
	if gate == scmcore.CIGateAbsent {
		a.log.Warn("manual head pipeline carried no job status",
			slog.Int64("pipeline_id", pipeline.ID))
		return string(scmcore.CIGatePending), nil
	}
	return string(gate), nil
}

// gitlabMergeAccept is the JSON body of PUT
// /projects/{project}/merge_requests/{iid}/merge. SHA is always sent so
// the platform rejects a moved head. Squash is set only for the squash
// strategy, and exactly one message field is set, so an unset field
// defers to the platform's own strategy default.
type gitlabMergeAccept struct {
	SHA                 string `json:"sha"`
	Squash              bool   `json:"squash,omitempty"`
	MergeCommitMessage  string `json:"merge_commit_message,omitempty"`
	SquashCommitMessage string `json:"squash_commit_message,omitempty"`
}

// composeMessage joins title and body into the one message field GitLab
// takes per merge strategy. Either side being empty yields the other
// side unchanged, so both empty leaves the composed value empty and the
// platform applies its own default.
func composeMessage(title, body string) string {
	if title == "" {
		return body
	}
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

// buildMergeAccept encodes strategy, the composed commit message, and
// expectedHeadSHA into the merge-accept body. ok is false for a strategy
// outside the three domain constants, in which case the returned body is
// the zero value and the caller must reject the call before issuing any
// request.
//
// Squash is true only in the squash arm; the merge and rebase arms leave
// it at its zero value so the project's own squash setting governs.
// buildMergeAccept issues no request and logs nothing: the caller logs
// the rebase governance warning, since only the caller owns a logger.
func buildMergeAccept(strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (gitlabMergeAccept, bool) {
	message := composeMessage(commitTitle, commitMessage)

	switch strategy {
	case domain.StrategyMerge:
		return gitlabMergeAccept{SHA: expectedHeadSHA, MergeCommitMessage: message}, true
	case domain.StrategySquash:
		return gitlabMergeAccept{SHA: expectedHeadSHA, Squash: true, SquashCommitMessage: message}, true
	case domain.StrategyRebase:
		return gitlabMergeAccept{SHA: expectedHeadSHA, MergeCommitMessage: message}, true
	default:
		return gitlabMergeAccept{}, false
	}
}

// MergePR merges the given merge request with the requested strategy via
// PUT /projects/{project}/merge_requests/{prNumber}/merge.
//
// An empty expectedHeadSHA or an unsupported strategy returns a
// [*domain.SCMError] of kind [domain.ErrSCMPayload] before any request is
// issued. GitLab expresses rebase-on-merge as the target project's own
// merge_method setting rather than a per-call parameter, so
// [domain.StrategyRebase] issues the same merge call as
// [domain.StrategyMerge] and logs one WARN naming that governance.
//
// A 405 or 409 rejection promotes to [domain.ErrSCMConflict] through
// [scmcore.AsMergeConflict]; the already-merged marker is present only
// after a merge-request re-read confirms the merge landed. A 401 or 403
// keeps kind [domain.ErrSCMAuth] with a message naming both the
// invalid-credential and branch-protection-refusal causes, since GitLab
// answers a branch-protection refusal on this route with 401. A 200
// whose decoded state is not "merged" returns [domain.ErrSCMConflict]
// directly, with no already-merged marker.
func (a *GitLabSCMAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	if expectedHeadSHA == "" {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "merge precondition sha is required",
		}
	}

	body, ok := buildMergeAccept(strategy, commitTitle, commitMessage, expectedHeadSHA)
	if !ok {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: fmt.Sprintf("unsupported merge strategy: %q", strategy),
		}
	}
	if strategy == domain.StrategyRebase {
		a.log.Warn("gitlab rebase strategy is governed by the project merge method")
	}

	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to encode merge request body",
			Err:     marshalErr,
		}
	}

	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber) + "/merge"
	respBody, sendErr := a.client.Send(ctx, http.MethodPut, path, bytes.NewReader(payload))
	if sendErr != nil {
		scm := scmcore.AsMergeConflict(scmcore.AsSCMError(sendErr))
		switch scm.Kind {
		case domain.ErrSCMConflict:
			return domain.MergeResult{}, a.resolveMergeConflict(ctx, prNumber, owner, repo, scm)
		case domain.ErrSCMAuth:
			return domain.MergeResult{}, enrichMergeAuthError(scm)
		default:
			return domain.MergeResult{}, scm
		}
	}

	var mr gitlabMergeRequest
	if jsonErr := json.Unmarshal(respBody, &mr); jsonErr != nil {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse merge response",
			Err:     jsonErr,
		}
	}
	if mr.State != "merged" {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMConflict,
			Message: fmt.Sprintf("merge endpoint returned state %q", mr.State),
		}
	}

	return domain.MergeResult{SHA: mr.MergeCommitSHA, Merged: true}, nil
}

// resolveMergeConflict re-reads the merge request after a promoted
// conflict and returns a conflict carrying the canonical "already
// merged" marker when the re-read observes state "merged".
//
// The marker is gated on the observed state rather than on GitLab's
// rejection wording, which is a constant carrying no reason, so it
// survives a reworded rejection and never fires on a stale-head 409. A
// failed re-read returns scm unchanged, at most delaying the outcome by
// one retry cycle.
func (a *GitLabSCMAdapter) resolveMergeConflict(ctx context.Context, prNumber int, owner, repo string, scm *domain.SCMError) *domain.SCMError {
	mr, readErr := a.fetchMergeRequest(ctx, prNumber, owner, repo)
	if readErr == nil && mr.State == "merged" {
		return scmcore.AlreadyMergedConflict(scm.Message, scm.Err)
	}
	return scm
}

// enrichMergeAuthError rewrites the message of a 401 or 403 from the
// merge route to name both possible causes. GitLab returns 401 on this
// route when branch protection forbids the caller from merging into the
// target branch, which deviates from the rest of the API, where 401
// otherwise means an invalid credential. The kind stays
// [domain.ErrSCMAuth], preserving the caller's immediate escalation.
func enrichMergeAuthError(scm *domain.SCMError) *domain.SCMError {
	scm.Message = "gitlab refused the merge: the credential is invalid, or it may not merge into the target branch: " + scm.Message
	return scm
}

// DeleteBranch deletes the branch reference via DELETE
// /projects/{project}/repository/branches/{branch}.
//
// branch is percent-encoded into the path, so a slash-bearing name
// reaches GitLab as %2F. An already-gone branch (HTTP 404) returns a
// [*domain.SCMError] of kind [domain.ErrSCMNotFound]; DeleteBranch
// returns that error rather than mapping it to nil, and the caller
// treats it as a successful no-op. Returns nil on a 200 or 204.
func (a *GitLabSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := "/projects/" + projectPath(owner, repo) + "/repository/branches/" + url.PathEscape(branch)
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		return scmcore.AsSCMError(err)
	}
	return nil
}
