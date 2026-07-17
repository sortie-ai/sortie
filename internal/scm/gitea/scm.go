package gitea

import (
	"context"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.SCMAdapters.Register("gitea", NewGiteaSCMAdapter)
}

// Compile-time interface satisfaction check.
var _ domain.SCMAdapter = (*GiteaSCMAdapter)(nil)

// GiteaSCMAdapter implements [domain.SCMAdapter] for Gitea over the REST API v1.
// The client is set once at construction and never mutated, so the adapter is
// safe for concurrent use.
type GiteaSCMAdapter struct {
	client *httpkit.Client
}

// NewGiteaSCMAdapter creates a [GiteaSCMAdapter] from adapter-specific
// configuration. Required config keys: "api_key" (access token) and "endpoint"
// (instance base URL; Gitea has no default host). Optional: "user_agent"
// (defaults to "sortie/dev").
//
// The endpoint is right-trimmed of trailing slashes and suffixed with "/api/v1"
// unless it already ends in it. Construction performs no network I/O: the SCM
// adapter is not project-scoped, so owner and repo arrive on every call and
// there is nothing to preflight. A missing "api_key" returns a [*domain.SCMError]
// of kind [domain.ErrSCMAuth]; a missing "endpoint" returns one of kind
// [domain.ErrSCMPayload]. The token travels only in the Authorization header set
// by the shared client and is never logged.
func NewGiteaSCMAdapter(adapterConfig map[string]any) (domain.SCMAdapter, error) {
	apiKey, _ := adapterConfig["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMAuth,
			Message: "missing required config key: api_key",
		}
	}

	endpoint, _ := adapterConfig["endpoint"].(string)
	if endpoint == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
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

	return &GiteaSCMAdapter{
		client: newGiteaClient(endpoint, apiKey, userAgent),
	}, nil
}

// MergePR reports that the Gitea SCM write path is not implemented.
//
// It issues no network request and returns a zero [domain.MergeResult] together
// with a [*domain.SCMError] of kind [domain.ErrSCMAPI], so the caller never
// mistakes the stub for a completed merge.
func (a *GiteaSCMAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	return domain.MergeResult{}, &domain.SCMError{
		Kind:    domain.ErrSCMAPI,
		Message: "gitea scm write operations are not implemented: merge",
	}
}

// DeleteBranch reports that the Gitea SCM write path is not implemented.
//
// It issues no network request and returns a [*domain.SCMError] of kind
// [domain.ErrSCMAPI], so the caller never mistakes the stub for a completed
// branch deletion.
func (a *GiteaSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	return &domain.SCMError{
		Kind:    domain.ErrSCMAPI,
		Message: "gitea scm write operations are not implemented: delete branch",
	}
}

// RemoveLabel reports that the Gitea SCM write path is not implemented.
//
// It issues no network request and returns a [*domain.SCMError] of kind
// [domain.ErrSCMAPI], so the caller never mistakes the stub for a completed
// label removal.
func (a *GiteaSCMAdapter) RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error {
	return &domain.SCMError{
		Kind:    domain.ErrSCMAPI,
		Message: "gitea scm write operations are not implemented: remove label",
	}
}

// parseUTC parses an RFC 3339 timestamp and returns it in UTC.
//
// A malformed timestamp yields the zero [time.Time] rather than an error, so a
// single unparseable field never fails an entire read. Gitea timestamps are
// server-generated and well-formed, so the fallback does not arise for valid
// responses.
func parseUTC(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
