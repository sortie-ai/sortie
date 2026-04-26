package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.CIProviders.Register("github", NewGitHubCIProvider)
}

// Compile-time interface satisfaction check.
var _ domain.CIStatusProvider = (*GitHubCIProvider)(nil)

const maxCIPages = 10

const maxLogBytes int64 = 1 << 20

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b\].*?\a`)

var timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z `)

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
// maximum number of log tail lines returned for failing checks (0
// disables log fetching). Required adapter config keys: "api_key",
// "project" (owner/repo format). Optional: "endpoint" (defaults to
// https://api.github.com), "user_agent".
func NewGitHubCIProvider(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error) {
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

	owner, repo, ok := strings.Cut(project, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "project must be in owner/repo format",
		}
	}

	endpoint, _ := adapterConfig["endpoint"].(string)
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	userAgent, _ := adapterConfig["user_agent"].(string)
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
// domain types, aggregate status is computed, and a truncated log
// excerpt is fetched from the first failing GitHub Actions check run
// when maxLogLines is positive.
func (p *GitHubCIProvider) FetchCIStatus(ctx context.Context, ref string) (domain.CIResult, error) {
	raw, err := p.fetchAllCheckRuns(ctx, ref)
	if err != nil {
		return domain.CIResult{}, toCIError(fmt.Errorf("fetching checks for ref %q: %w", ref, err))
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

	status := computeAggregateStatus(runs)
	failCount := computeFailingCount(runs)

	var logExcerpt string
	if status == domain.CIStatusFailing && p.maxLogLines > 0 {
		for _, gh := range raw {
			c := mapCheckConclusion(gh.Conclusion)
			if c == domain.CheckConclusionFailure || c == domain.CheckConclusionTimedOut || c == domain.CheckConclusionCancelled {
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
	// check run ID doubles as the job ID for the Actions logs endpoint.
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", p.owner, p.repo, failing.ID)

	body, err := p.client.GetRaw(ctx, path, maxLogBytes)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch job log",
			slog.Int64("job_id", failing.ID),
			slog.Any("error", err))
		return ""
	}

	return truncateLog(string(body), p.maxLogLines)
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

func computeAggregateStatus(runs []domain.CheckRun) domain.CIStatus {
	if len(runs) == 0 {
		return domain.CIStatusPending
	}

	allCompleted := true
	anyFailed := false

	for _, run := range runs {
		if run.Status != domain.CheckRunStatusCompleted {
			allCompleted = false
		}
		switch run.Conclusion {
		case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut, domain.CheckConclusionCancelled:
			anyFailed = true
		}
	}

	if anyFailed {
		return domain.CIStatusFailing
	}
	if allCompleted {
		return domain.CIStatusPassing
	}
	return domain.CIStatusPending
}

func computeFailingCount(runs []domain.CheckRun) int {
	count := 0
	for _, run := range runs {
		switch run.Conclusion {
		case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut, domain.CheckConclusionCancelled:
			count++
		}
	}
	return count
}

func toCIError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var te *domain.TrackerError
	if errors.As(err, &te) {
		var kind domain.CIErrorKind
		switch te.Kind {
		case domain.ErrTrackerTransport:
			kind = domain.ErrCITransport
		case domain.ErrTrackerAuth, domain.ErrMissingTrackerAPIKey:
			kind = domain.ErrCIAuth
		case domain.ErrTrackerAPI:
			kind = domain.ErrCIAPI
		case domain.ErrTrackerNotFound:
			kind = domain.ErrCINotFound
		case domain.ErrTrackerPayload, domain.ErrMissingTrackerProject:
			kind = domain.ErrCIPayload
		default:
			kind = domain.ErrCIAPI
		}
		return &domain.CIError{
			Kind:    kind,
			Message: te.Message,
			Err:     err,
		}
	}

	return &domain.CIError{
		Kind:    domain.ErrCIAPI,
		Message: err.Error(),
		Err:     err,
	}
}

func stripANSI(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = timestampPattern.ReplaceAllString(s, "")
	return s
}

func truncateLog(raw string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}

	lines := strings.Split(raw, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		cleaned = append(cleaned, stripANSI(line))
	}

	if len(cleaned) > maxLines {
		cleaned = cleaned[len(cleaned)-maxLines:]
	}

	return strings.Join(cleaned, "\n")
}
