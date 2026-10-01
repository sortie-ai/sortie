package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/cilog"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.CIProviders.Register("github", NewGitHubCIProvider)
}

// Compile-time interface satisfaction check.
var _ domain.CIStatusProvider = (*GitHubCIProvider)(nil)

const maxCIPages = 10

type checkRunsResponse struct {
	TotalCount int              `json:"total_count"`
	CheckRuns  []githubCheckRun `json:"check_runs"`
}

type githubCheckRun struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	Conclusion *string      `json:"conclusion"`
	HTMLURL    string       `json:"html_url"`
	App        *checkRunApp `json:"app"`
}

type checkRunApp struct {
	Slug string `json:"slug"`
}

// GitHubCIProvider implements [domain.CIStatusProvider] for the GitHub
// Checks API. Safe for concurrent use.
type GitHubCIProvider struct {
	client      *httpkit.Client
	owner       string
	repo        string
	maxLogLines int
}

// NewGitHubCIProvider creates a [GitHubCIProvider] from primitives and
// the GitHub adapter pass-through config. maxLogLines controls the
// maximum number of log lines in the excerpt returned for failing checks
// (0 disables log fetching). Required adapter config keys: "api_key",
// "project" (owner/repo format). Optional: "endpoint" (defaults to
// https://api.github.com; a value that does not parse as an absolute
// http or https URL with a host returns a [*domain.CIError] of kind
// [domain.ErrCIPayload]), "user_agent".
func NewGitHubCIProvider(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error) {
	apiKey, fault := typeutil.StringField(adapterConfig, "api_key")
	if fault != nil {
		return nil, &domain.CIError{Kind: domain.ErrCIPayload, Message: fault.Error()}
	}
	if apiKey == "" {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIAuth,
			Message: "missing required config key: api_key",
		}
	}

	project, fault := typeutil.StringField(adapterConfig, "project")
	if fault != nil {
		return nil, &domain.CIError{Kind: domain.ErrCIPayload, Message: fault.Error()}
	}
	if project == "" {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "missing required config key: project",
		}
	}

	owner, repo, ok := strings.Cut(project, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "project must be in owner/repo format",
		}
	}

	endpointRaw, fault := typeutil.StringField(adapterConfig, "endpoint")
	if fault != nil {
		return nil, &domain.CIError{Kind: domain.ErrCIPayload, Message: fault.Error()}
	}
	endpoint, redactedEndpoint, endpointOK := resolveEndpoint(endpointRaw)
	if !endpointOK {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: fmt.Sprintf("github: endpoint %q is not a valid absolute http(s) url", redactedEndpoint),
		}
	}

	userAgent, fault := typeutil.StringField(adapterConfig, "user_agent")
	if fault != nil {
		return nil, &domain.CIError{Kind: domain.ErrCIPayload, Message: fault.Error()}
	}
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &GitHubCIProvider{
		client:      newGitHubClient(endpoint, apiKey, userAgent),
		owner:       owner,
		repo:        repo,
		maxLogLines: maxLogLines,
	}, nil
}

// FetchCIStatus returns the aggregate CI pipeline status for the given
// git ref by querying the GitHub Checks API. Check runs are mapped to
// domain types, aggregate status is computed, and a log excerpt is built
// from the first failing GitHub Actions check run when maxLogLines is
// positive. The excerpt holds the output of the step that failed, or the
// end of the job log when that step cannot be located, and opens with a
// note line saying which.
func (p *GitHubCIProvider) FetchCIStatus(ctx context.Context, ref string) (domain.CIResult, error) {
	raw, err := p.fetchAllCheckRuns(ctx, ref)
	if err != nil {
		return domain.CIResult{}, scmcore.ToCIError(fmt.Errorf("fetching checks for ref %q: %w", ref, err))
	}

	runs := make([]domain.CheckRun, len(raw))
	for i, gh := range raw {
		runs[i] = domain.CheckRun{
			Name:       gh.Name,
			Status:     mapCheckRunStatus(gh.Status),
			Conclusion: mapCheckConclusion(gh.Conclusion),
			DetailsURL: gh.HTMLURL,
		}
	}

	status := scmcore.AggregateCIStatus(runs)
	failCount := scmcore.FailingCount(runs)

	var logExcerpt string
	if status == domain.CIStatusFailing && p.maxLogLines > 0 {
		for _, gh := range raw {
			if scmcore.IsFailingConclusion(mapCheckConclusion(gh.Conclusion)) {
				if gh.App != nil && gh.App.Slug == "github-actions" {
					logExcerpt = p.fetchLogExcerpt(ctx, gh)
					break
				}
			}
		}
	}

	return domain.CIResult{
		Status:       status,
		CheckRuns:    runs,
		LogExcerpt:   logExcerpt,
		FailingCount: failCount,
		Ref:          ref,
	}, nil
}

func (p *GitHubCIProvider) fetchAllCheckRuns(ctx context.Context, ref string) ([]githubCheckRun, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", p.owner, p.repo, url.PathEscape(ref))
	params := url.Values{"per_page": {"100"}}

	paginator := httpkit.NewLinkPaginator(p.client, path, params, func(body []byte) ([]githubCheckRun, error) {
		var resp checkRunsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse check runs response",
				Err:     err,
			}
		}
		return resp.CheckRuns, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxCIPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "check runs response truncated at page limit",
				slog.Int("max_pages", limit),
				slog.String("ref", ref))
		},
	})

	return paginator.All(ctx)
}

func (p *GitHubCIProvider) fetchLogExcerpt(ctx context.Context, failing githubCheckRun) string {
	if p.maxLogLines <= 0 {
		return ""
	}

	if failing.App == nil || failing.App.Slug != "github-actions" {
		return ""
	}

	// GitHub Actions creates check runs 1:1 with workflow jobs, so the
	// check run ID doubles as the job ID for the Actions endpoints.
	selected, reason := p.locateFailingStep(ctx, failing.ID)

	builder := cilog.NewBuilder(p.maxLogLines)
	scanner := newStepLogScanner(selected, builder)

	var (
		complete bool
		scanErr  error
	)
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", p.owner, p.repo, failing.ID)
	err := p.client.GetStream(ctx, path, func(body io.Reader) error {
		complete, scanErr = cilog.Scan(body, scanner.line)
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch job log",
			slog.Int64("job_id", failing.ID),
			slog.Any("error", err))
		return ""
	}
	if scanErr != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "failed to fetch job log",
				slog.Int64("job_id", failing.ID),
				slog.Any("error", scanErr))
			return ""
		}
		slog.WarnContext(ctx, "job log read ended early",
			slog.Int64("job_id", failing.ID),
			slog.Any("error", scanErr))
	}

	text, fallback := builder.Excerpt(complete)
	if fallback != cilog.NoFallback {
		if selected != nil {
			reason = string(fallback)
		}
		slog.DebugContext(ctx, "log excerpt fell back to the job tail",
			slog.Int64("job_id", failing.ID),
			slog.String("reason", reason))
	}
	return text
}

// locateFailingStep reads the job object and picks the step the excerpt is
// anchored on. It returns nil and the reason when there is none; a failure to
// read the job object degrades the excerpt to the job tail rather than
// failing the check.
func (p *GitHubCIProvider) locateFailingStep(ctx context.Context, jobID int64) (*failingStep, string) {
	steps, err := p.fetchJobSteps(ctx, jobID)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch job steps for log excerpt",
			slog.Int64("job_id", jobID),
			slog.Any("error", err))
		return nil, "job_steps_unavailable"
	}

	step, ok := selectFailingStep(steps)
	if !ok {
		return nil, "no_failing_step"
	}
	return &step, ""
}

func mapCheckRunStatus(s string) domain.CheckRunStatus {
	switch s {
	case "queued":
		return domain.CheckRunStatusQueued
	case "in_progress":
		return domain.CheckRunStatusInProgress
	case "completed":
		return domain.CheckRunStatusCompleted
	default:
		return domain.CheckRunStatusQueued
	}
}

func mapCheckConclusion(c *string) domain.CheckConclusion {
	if c == nil {
		return domain.CheckConclusionPending
	}
	switch *c {
	case "success":
		return domain.CheckConclusionSuccess
	case "failure":
		return domain.CheckConclusionFailure
	case "cancelled":
		return domain.CheckConclusionCancelled
	case "timed_out":
		return domain.CheckConclusionTimedOut
	case "neutral":
		return domain.CheckConclusionNeutral
	case "skipped":
		return domain.CheckConclusionSkipped
	case "action_required":
		// GitHub sets action_required on completed check runs that need
		// manual approval (e.g. code scanning alerts). The agent cannot
		// perform UI actions, so this is a blocking failure.
		return domain.CheckConclusionFailure
	case "stale":
		// A stale check run was superseded by a newer push. Treat as
		// pending because the replacement check run will provide the
		// authoritative conclusion.
		return domain.CheckConclusionPending
	default:
		return domain.CheckConclusionPending
	}
}
