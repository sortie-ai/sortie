package gitea

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
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func init() {
	registry.CIProviders.Register("gitea", NewGiteaCIProvider)
}

// Compile-time interface satisfaction check.
var _ domain.CIStatusProvider = (*GiteaCIProvider)(nil)

// scmMaxPages caps the number of pages fetched for a single combined-status
// walk, whether the reader is the CI provider or the source-control merge
// gate. At 50 entries per page this bounds a read at 2500 entries, generous
// for the short-lived, low-traffic pull requests Sortie manages.
const scmMaxPages = 50

// ansiPattern matches the ANSI escape sequences a CI-produced status
// description may carry (CSI color and cursor codes, and OSC sequences).
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b\].*?\a`)

// GiteaCIProvider implements [domain.CIStatusProvider] for Gitea over the
// combined commit-status route. All fields are set once at construction and
// never mutated, so the provider is safe for concurrent use.
type GiteaCIProvider struct {
	client      *httpkit.Client
	owner       string
	repo        string
	maxLogLines int
}

// NewGiteaCIProvider creates a [GiteaCIProvider] from the CI log-line budget and
// the merged adapter config. maxLogLines caps the failing-status log excerpt; a
// value of zero or less disables it.
//
// Required config keys: "api_key" (access token) and "project" (owner/repo).
// "endpoint" (instance base URL) is required because Gitea has no default host;
// it is right-trimmed of slashes and suffixed with "/api/v1" unless it already
// ends in it. "user_agent" defaults to "sortie/dev". A missing "api_key" returns
// a [*domain.CIError] of kind [domain.ErrCIAuth]; a missing or malformed
// "project" or a missing "endpoint" returns one of kind [domain.ErrCIPayload].
// Construction performs no network I/O: splitting the project is a string parse.
// The token travels only in the Authorization header set by the shared client
// and is never logged.
func NewGiteaCIProvider(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error) {
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
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "missing required config key: endpoint",
		}
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(endpoint, "/api/v1") {
		endpoint += "/api/v1"
	}

	userAgent, _ := adapterConfig["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &GiteaCIProvider{
		client:      newGiteaClient(endpoint, apiKey, userAgent),
		owner:       owner,
		repo:        repo,
		maxLogLines: maxLogLines,
	}, nil
}

// FetchCIStatus returns the aggregate CI status for the given git ref by reading
// every page of Gitea's combined commit status and normalizing it to a
// [domain.CIResult].
//
// The ref is percent-encoded into GET /repos/{owner}/{repo}/commits/{ref}/status
// and read directly, so no PR fetch or SHA resolution occurs. The aggregate is
// computed from the per-status entries, never the top-level state, which Gitea
// reports as pending for a commit with no CI. CheckRuns is a non-nil slice, empty
// when the ref has no statuses. A failure maps to a [*domain.CIError]; a
// context cancellation or deadline error is returned without conversion to one,
// so a caller can still match it with [errors.Is].
func (p *GiteaCIProvider) FetchCIStatus(ctx context.Context, ref string) (domain.CIResult, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/status",
		url.PathEscape(p.owner), url.PathEscape(p.repo), url.PathEscape(ref))

	paginator := httpkit.NewPagePaginator(p.client, path, nil, decodeGiteaCombinedStatusForCI, httpkit.PageOptions{
		PageParam: "page",
		SizeParam: "limit",
		PageSize:  50,
		MaxPages:  scmMaxPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", path),
				slog.Int("max_pages", limit))
		},
	})

	statuses, err := paginator.All(ctx)
	if err != nil {
		var ciErr *domain.CIError
		if errors.As(err, &ciErr) {
			return domain.CIResult{}, ciErr
		}
		return domain.CIResult{}, giteaToCIError(fmt.Errorf("fetching ci status for ref %q: %w", ref, err))
	}

	runs := make([]domain.CheckRun, len(statuses))
	for i, s := range statuses {
		runs[i] = domain.CheckRun{
			Name:       s.Context,
			Status:     mapRunStatus(s.Status),
			Conclusion: mapConclusion(s.Status),
			DetailsURL: s.TargetURL,
		}
	}

	status := scmcore.AggregateCIStatus(runs)

	var logExcerpt string
	if status == domain.CIStatusFailing {
		logExcerpt = buildLogExcerpt(statuses, p.maxLogLines)
	}

	return domain.CIResult{
		Status:       status,
		CheckRuns:    runs,
		LogExcerpt:   logExcerpt,
		FailingCount: scmcore.FailingCount(runs),
		Ref:          ref,
	}, nil
}

// decodeGiteaCombinedStatusForCI decodes one page of the combined-status
// response for the CI provider boundary. A decode failure is returned as a
// [*domain.CIError] of kind [domain.ErrCIPayload], since the shared
// page-number walk returns whatever the decoder produces unconverted.
func decodeGiteaCombinedStatusForCI(body []byte) ([]giteaCommitState, error) {
	var combined giteaCombinedStatus
	if err := json.Unmarshal(body, &combined); err != nil {
		return nil, &domain.CIError{
			Kind:    domain.ErrCIPayload,
			Message: "failed to parse combined status response",
			Err:     err,
		}
	}
	return combined.Statuses, nil
}

// mapRunStatus maps a Gitea commit-status value to a [domain.CheckRunStatus],
// lowercasing first so a differently-cased value classifies as it does on the
// merge-gate read. A terminal value is completed; pending, unknown, and empty
// defer to in progress rather than counting as passing.
func mapRunStatus(status string) domain.CheckRunStatus {
	switch strings.ToLower(status) {
	case "success", "failure", "error", "warning":
		return domain.CheckRunStatusCompleted
	default:
		return domain.CheckRunStatusInProgress
	}
}

// mapConclusion maps a Gitea commit-status value to a [domain.CheckConclusion],
// lowercasing first to match the merge-gate read. error folds to failure and
// warning to neutral; pending, unknown, and empty defer to pending.
func mapConclusion(status string) domain.CheckConclusion {
	switch strings.ToLower(status) {
	case "success":
		return domain.CheckConclusionSuccess
	case "failure", "error":
		return domain.CheckConclusionFailure
	case "warning":
		return domain.CheckConclusionNeutral
	default:
		return domain.CheckConclusionPending
	}
}

// buildLogExcerpt assembles a truncated excerpt from the first failing commit
// status, drawing only on text already in the authenticated combined-status
// response. It never fetches TargetURL, so an operator-configured or third-party
// run URL cannot expand the request beyond the Gitea API. It returns the empty
// string when log fetching is disabled (maxLogLines is zero or less), no status
// is failing, or the failing status carries neither a description nor a target.
func buildLogExcerpt(statuses []giteaCommitState, maxLogLines int) string {
	if maxLogLines <= 0 {
		return ""
	}

	for _, s := range statuses {
		if mapConclusion(s.Status) != domain.CheckConclusionFailure {
			continue
		}
		excerpt := strings.TrimSpace(s.Description)
		if s.TargetURL != "" {
			if excerpt == "" {
				excerpt = s.TargetURL
			} else {
				excerpt += "\n" + s.TargetURL
			}
		}
		if excerpt == "" {
			return ""
		}
		return truncateLines(stripANSI(excerpt), maxLogLines)
	}
	return ""
}

func truncateLines(raw string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}

	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, "\r")
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// giteaToCIError converts an error from the shared Gitea transport into a
// [*domain.CIError] at the CI boundary.
//
// A context cancellation or deadline error is returned as-is, without
// conversion to a [*domain.CIError], so a caller can match it with [errors.Is].
// A [*domain.TrackerError] is mapped by its Kind onto the matching CI kind,
// preserving the chain; any other error becomes [domain.ErrCIAPI].
func giteaToCIError(err error) error {
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
		case domain.ErrTrackerAuth:
			kind = domain.ErrCIAuth
		case domain.ErrTrackerAPI:
			kind = domain.ErrCIAPI
		case domain.ErrTrackerNotFound:
			kind = domain.ErrCINotFound
		case domain.ErrTrackerPayload:
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
