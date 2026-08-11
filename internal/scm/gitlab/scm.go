package gitlab

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func init() {
	registry.SCMAdapters.Register("gitlab", NewGitLabSCMAdapter)
}

// Compile-time interface satisfaction check.
var _ domain.SCMAdapter = (*GitLabSCMAdapter)(nil)

// GitLabSCMAdapter implements [domain.SCMAdapter] for GitLab merge
// requests over the REST API v4. The client and logger are set once at
// construction and never mutated; botCache is the only mutable state,
// guarded by a mutex held only around map access. The adapter is safe
// for concurrent use.
type GitLabSCMAdapter struct {
	client *httpkit.Client
	log    *slog.Logger

	mu       sync.Mutex
	botCache map[int64]bool
}

// NewGitLabSCMAdapter creates a [GitLabSCMAdapter] from adapter-specific
// configuration. The required config key is "api_key"; an absent or
// empty value returns a [*domain.SCMError] of kind [domain.ErrSCMAuth].
// "endpoint" defaults to "https://gitlab.com" and is trimmed of trailing
// slashes and suffixed with "/api/v4" unless already present; a value
// that does not parse as an absolute http or https URL with a host
// returns a [*domain.SCMError] of kind [domain.ErrSCMPayload].
// "user_agent" defaults to "sortie/dev". Any "project" key is ignored:
// owner and repo arrive per call.
//
// Construction performs no network request.
func NewGitLabSCMAdapter(adapterConfig map[string]any) (domain.SCMAdapter, error) {
	apiKey, _ := adapterConfig["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMAuth,
			Message: "missing required config key: api_key",
		}
	}

	endpointRaw, _ := adapterConfig["endpoint"].(string)
	endpoint := strings.TrimSpace(endpointRaw)
	if endpoint == "" {
		endpoint = "https://gitlab.com"
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") || parsedEndpoint.Host == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: fmt.Sprintf("gitlab: endpoint %q is not a valid absolute http(s) url", endpoint),
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

	return &GitLabSCMAdapter{
		client:   newGitLabClient(baseURL, apiKey, userAgent),
		log:      slog.Default(),
		botCache: make(map[int64]bool),
	}, nil
}

// projectPath returns the percent-encoded owner/repo join GitLab's
// project-scoped routes address, encoding every "/" including those
// inside a nested namespace. owner and repo are never split, validated,
// or encoded separately.
func projectPath(owner, repo string) string {
	return url.PathEscape(owner + "/" + repo)
}

// asSCMError normalizes err to a [*domain.SCMError]. An error that
// already unwraps to one is returned unchanged, preserving a page
// decoder's own constructed kind; any other error converts through
// [scmcore.ToSCMError].
func asSCMError(err error) *domain.SCMError {
	var scmErr *domain.SCMError
	if errors.As(err, &scmErr) {
		return scmErr
	}
	return scmcore.ToSCMError(err)
}

// paginateSCM walks a Link-header-paginated GitLab route through the
// shared paginator, requesting the server maximum of 100 items per page
// and stopping at the package's maxPages ceiling.
//
// decode returning a typed [*domain.SCMError] passes through unchanged;
// any other error the walk returns converts through [scmcore.ToSCMError].
func paginateSCM[T any](ctx context.Context, client *httpkit.Client, log *slog.Logger, path string, params url.Values, decode httpkit.LinkPageDecoder[T]) ([]T, error) {
	pageParams := url.Values{}
	for key, values := range params {
		pageParams[key] = append([]string(nil), values...)
	}
	pageParams.Set("per_page", "100")

	paginator := httpkit.NewLinkPaginator(client, path, pageParams, decode, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			log.Warn("pagination limit reached",
				slog.String("endpoint", path),
				slog.Int("max_pages", limit))
		},
	})

	items, err := paginator.All(ctx)
	if err != nil {
		return nil, asSCMError(err)
	}
	return items, nil
}
