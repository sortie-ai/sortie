package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func init() {
	registry.CIProviders.Register("gitlab", NewGitLabCIProvider)
}

// Compile-time interface satisfaction check.
var _ domain.CIStatusProvider = (*GitLabCIProvider)(nil)

// maxTraceBytes caps a single job-trace read. GitLab ignores the Range
// header on this route, so a trace larger than the cap yields the tail of
// its first mebibyte rather than the true tail.
const maxTraceBytes int64 = 1 << 20

// runnerPrefixPattern matches the timestamp and stream-token prefix a
// GitLab runner writes at the start of every trace line, including the
// continuation form that ends in "+" with no trailing space.
var runnerPrefixPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z \d{2}[OE]\+? ?`)

// ansiEscapePattern matches ANSI CSI and OSC escape sequences a trace line
// may carry.
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b\].*?\a`)

// sectionMarkerPattern matches a section_start or section_end marker
// token. The token ends at the carriage return that terminates it
// mid-line, which \S+ stops at without consuming.
var sectionMarkerPattern = regexp.MustCompile(`section_(?:start|end):\d+:\S+`)

// gitlabResolvedCommit is the commit-resolution response. Two fields are
// consumed: the canonical full SHA, and the id of the pipeline the
// platform reports as the commit's current one.
type gitlabResolvedCommit struct {
	ID           string                `json:"id"`
	LastPipeline *gitlabCommitPipeline `json:"last_pipeline"`
}

// gitlabCommitPipeline is the last_pipeline sub-object. Only the id is
// consumed; the pipeline's own status is not, because the aggregate is
// computed from the normalized run set and never from a platform
// aggregate.
type gitlabCommitPipeline struct {
	ID int64 `json:"id"`
}

// gitlabCommitStatus is one entry of the commit-status list. The list
// mixes runner jobs and externally reported statuses, and no field
// separates them, so ID is what the log-excerpt read tests against the
// pipeline's job set. PipelineID is what the scope filter tests against
// the pipeline the read asked for.
type gitlabCommitStatus struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure"`
	TargetURL    string `json:"target_url"`
	PipelineID   int64  `json:"pipeline_id"`
}

// gitlabPipelineJob is one entry of the pipeline-jobs list, read only to
// decide whether a failing commit-status entry is a job whose trace can
// be fetched.
type gitlabPipelineJob struct {
	ID int64 `json:"id"`
}

// GitLabCIProvider implements [domain.CIStatusProvider] for GitLab CI
// over the commit-status route. All fields are set once at construction
// and never mutated, so the provider is safe for concurrent use.
type GitLabCIProvider struct {
	client      *httpkit.Client
	project     string
	maxLogLines int
	log         *slog.Logger
}

// NewGitLabCIProvider creates a [GitLabCIProvider] from the CI log-line
// budget and the merged adapter config. maxLogLines caps the
// failing-check log excerpt; a value of zero or less disables log
// fetching.
//
// Required config keys: "api_key" (access token) and "project" (a
// numeric project ID or an unescaped namespace path of any depth, such
// as "group/subgroup/project", which the constructor percent-encodes
// once as a whole and never splits on "/"). "endpoint" defaults to
// "https://gitlab.com", is trimmed of trailing slashes, and is suffixed
// with "/api/v4" unless already present; a value that does not parse as
// an absolute http or https URL with a host returns a [*domain.CIError]
// of kind [domain.ErrCIPayload]. "user_agent" defaults to "sortie/dev".
// A missing or empty "api_key" returns a [*domain.CIError] of kind
// [domain.ErrCIAuth]; a missing or empty "project" returns one of kind
// [domain.ErrCIPayload]. Construction performs no network request.
func NewGitLabCIProvider(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error) {
	apiKey, _ := adapterConfig["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIAuth,
			Message: "missing required config key: api_key",
		}
	}

	project, _ := adapterConfig["project"].(string)
	if project == "" {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "missing required config key: project",
		}
	}

	endpointRaw, _ := adapterConfig["endpoint"].(string)
	endpoint := strings.TrimSpace(endpointRaw)
	if endpoint == "" {
		endpoint = "https://gitlab.com"
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") || parsedEndpoint.Host == "" {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: fmt.Sprintf("gitlab: endpoint %q is not a valid absolute http(s) url", httpkit.RedactURLUserinfo(endpoint)),
		}
	}
	baseURL := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(baseURL, "/api/v4") {
		baseURL += "/api/v4"
	}

	userAgent, _ := adapterConfig["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &GitLabCIProvider{
		client:      newGitLabClient(baseURL, apiKey, userAgent),
		project:     url.PathEscape(project),
		maxLogLines: maxLogLines,
		log:         slog.Default(),
	}, nil
}

// resolveCommit resolves ref to the full commit SHA the status read
// addresses and the id of the pipeline the commit response names as
// current. pipelineID is zero when the commit carries no pipeline. An
// empty ref, a decode failure, or a commit response carrying no id
// returns a [*domain.CIError] of kind [domain.ErrCIPayload] without
// issuing a request in the empty-ref case. Any other error from the
// commit-resolution request is returned unconverted; the caller applies
// [scmcore.ToCIError].
func (p *GitLabCIProvider) resolveCommit(ctx context.Context, ref string) (string, int64, error) {
	if ref == "" {
		return "", 0, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "ci ref is empty",
		}
	}

	path := "/projects/" + p.project + "/repository/commits/" + url.PathEscape(ref)
	body, _, err := p.client.Get(ctx, path, nil)
	if err != nil {
		return "", 0, err
	}

	var commit gitlabResolvedCommit
	if err := json.Unmarshal(body, &commit); err != nil {
		return "", 0, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "failed to parse commit response",
			Err:     err,
		}
	}
	if commit.ID == "" {
		return "", 0, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "commit response carried no id",
		}
	}

	var pipelineID int64
	if commit.LastPipeline != nil {
		pipelineID = commit.LastPipeline.ID
	}
	return commit.ID, pipelineID, nil
}

// fetchCommitStatuses walks every page of the commit-status route for sha
// through [httpkit.NewLinkPaginator], scoped to pipelineID both on the
// wire, via the pipeline_id query parameter, and again after decoding, by
// discarding any entry whose own pipeline_id differs. The post-decode
// filter is what keeps the scope safe against a deployment that ignores
// the query parameter. A JSON decode failure on any page returns a
// [*domain.CIError] of kind [domain.ErrCIPayload]; any other error the
// walk returns is passed through unconverted for the caller to apply
// [scmcore.ToCIError].
func (p *GitLabCIProvider) fetchCommitStatuses(ctx context.Context, sha string, pipelineID int64) ([]gitlabCommitStatus, error) {
	path := "/projects/" + p.project + "/repository/commits/" + url.PathEscape(sha) + "/statuses"
	params := url.Values{
		"per_page":    {"100"},
		"pipeline_id": {strconv.FormatInt(pipelineID, 10)},
	}

	paginator := httpkit.NewLinkPaginator(p.client, path, params, func(body []byte) ([]gitlabCommitStatus, error) {
		var page []gitlabCommitStatus
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.CIError{
				Kind:    domain.ErrCIPayload,
				Message: "failed to parse commit statuses response",
				Err:     err,
			}
		}
		return page, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			p.log.Warn("pagination limit reached",
				slog.String("endpoint", path),
				slog.Int("max_pages", limit))
		},
	})

	all, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}

	kept := make([]gitlabCommitStatus, 0, len(all))
	for _, entry := range all {
		if entry.PipelineID == pipelineID {
			kept = append(kept, entry)
		}
	}
	return kept, nil
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

// fetchPipelineJobIDs walks every page of the pipeline-jobs route for
// pipelineID through [httpkit.NewLinkPaginator] and returns the set of job
// ids. Job membership decided from this set, rather than by probing the
// trace route, is what keeps a speculative 404 off an external status
// that shares the commit-status identifier space with runner jobs.
func (p *GitLabCIProvider) fetchPipelineJobIDs(ctx context.Context, pipelineID int64) (map[int64]struct{}, error) {
	path := "/projects/" + p.project + "/pipelines/" + strconv.FormatInt(pipelineID, 10) + "/jobs"
	params := url.Values{"per_page": {"100"}}

	paginator := httpkit.NewLinkPaginator(p.client, path, params, func(body []byte) ([]gitlabPipelineJob, error) {
		var page []gitlabPipelineJob
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.CIError{
				Kind:    domain.ErrCIPayload,
				Message: "failed to parse pipeline jobs response",
				Err:     err,
			}
		}
		return page, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			p.log.Warn("pagination limit reached",
				slog.String("endpoint", path),
				slog.Int("max_pages", limit))
		},
	})

	jobs, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}

	ids := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		ids[job.ID] = struct{}{}
	}
	return ids, nil
}

// logExcerpt selects the first failing entry of statuses that is a member
// of pipelineID's job set and returns a sanitized tail of its trace.
// statuses and runs MUST be index-aligned. It returns the empty string,
// never a non-nil error, whenever the job set cannot be resolved, no
// failing entry is a job, or the trace read fails; a log-path failure
// MUST NOT change the verdict [GitLabCIProvider.FetchCIStatus] returns.
func (p *GitLabCIProvider) logExcerpt(ctx context.Context, statuses []gitlabCommitStatus, runs []domain.CheckRun, pipelineID int64) string {
	jobIDs, err := p.fetchPipelineJobIDs(ctx, pipelineID)
	if err != nil {
		p.log.Warn("failed to resolve pipeline jobs for log excerpt", slog.Any("error", err))
		return ""
	}

	idx := -1
	for i := range statuses {
		if !scmcore.IsFailingConclusion(runs[i].Conclusion) {
			continue
		}
		if _, ok := jobIDs[statuses[i].ID]; !ok {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		p.log.Debug("no failing check has a job trace")
		return ""
	}

	path := "/projects/" + p.project + "/jobs/" + strconv.FormatInt(statuses[idx].ID, 10) + "/trace"
	raw, err := p.client.GetRaw(ctx, path, maxTraceBytes)
	if err != nil {
		p.log.Warn("failed to fetch job trace", slog.Any("error", err))
		return ""
	}

	return traceExcerpt(raw, p.maxLogLines)
}

// traceExcerpt sanitizes raw into at most maxLines surviving lines,
// joined by the newline character. It returns the empty string when
// maxLines is zero or less. Each line has its runner-prefix, ANSI escape,
// and section-marker markup stripped, every carriage return removed, and
// trailing spaces and tabs trimmed; a line left empty after sanitization
// is dropped before the line-count limit is applied, and the excerpt
// keeps the last surviving lines rather than the first.
func traceExcerpt(raw []byte, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}

	lines := strings.Split(string(raw), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = runnerPrefixPattern.ReplaceAllString(line, "")
		line = ansiEscapePattern.ReplaceAllString(line, "")
		line = sectionMarkerPattern.ReplaceAllString(line, "")
		line = strings.ReplaceAll(line, "\r", "")
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}

	if len(kept) > maxLines {
		kept = kept[len(kept)-maxLines:]
	}
	return strings.Join(kept, "\n")
}

// FetchCIStatus returns the aggregate CI status for ref by resolving it to
// a commit SHA and its current pipeline, reading that pipeline's commit
// statuses to exhaustion, and normalizing each entry to a
// [domain.CheckRun]. CheckRuns is a non-nil slice, empty when the commit
// carries no pipeline or the pipeline carries no statuses. Status and
// FailingCount come from [scmcore.AggregateCIStatus] and
// [scmcore.FailingCount]; Ref echoes the caller's ref verbatim, never the
// resolved SHA. On a failing verdict with log fetching enabled, LogExcerpt
// carries a sanitized tail of the first failing job's trace, or the empty
// string when no failing entry is a job or the trace could not be read. A
// failure returns a [*domain.CIError]; a context cancellation or deadline
// error is returned without conversion to one, so a caller can still
// match it with [errors.Is].
func (p *GitLabCIProvider) FetchCIStatus(ctx context.Context, ref string) (domain.CIResult, error) {
	sha, pipelineID, err := p.resolveCommit(ctx, ref)
	if err != nil {
		return domain.CIResult{}, scmcore.ToCIError(err)
	}

	var statuses []gitlabCommitStatus
	if pipelineID != 0 {
		statuses, err = p.fetchCommitStatuses(ctx, sha, pipelineID)
		if err != nil {
			return domain.CIResult{}, scmcore.ToCIError(err)
		}
	}

	runs := make([]domain.CheckRun, len(statuses))
	var unknownCount int
	var unknownExample string
	for i, entry := range statuses {
		conclusion, runStatus, recognized := mapJobOutcome(entry.Status, entry.AllowFailure)
		if !recognized {
			unknownCount++
			if unknownExample == "" {
				unknownExample = entry.Status
			}
		}
		runs[i] = domain.CheckRun{
			Name:       entry.Name,
			Status:     runStatus,
			Conclusion: conclusion,
			DetailsURL: entry.TargetURL,
		}
	}
	if unknownCount > 0 {
		p.log.Warn("unrecognized gitlab job status",
			slog.String("status", unknownExample),
			slog.Int("count", unknownCount))
	}

	status := scmcore.AggregateCIStatus(runs)

	var excerpt string
	if status == domain.CIStatusFailing && p.maxLogLines > 0 {
		excerpt = p.logExcerpt(ctx, statuses, runs, pipelineID)
	}

	return domain.CIResult{
		Status:       status,
		CheckRuns:    runs,
		LogExcerpt:   excerpt,
		FailingCount: scmcore.FailingCount(runs),
		Ref:          ref,
	}, nil
}
