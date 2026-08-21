package gitea

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
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func init() {
	registry.SCMAdapters.Register("gitea", NewGiteaSCMAdapter)
}

// Compile-time interface satisfaction check.
var _ domain.SCMAdapter = (*GiteaSCMAdapter)(nil)

// GiteaSCMAdapter implements [domain.SCMAdapter] for Gitea over the REST API v1.
// The client and the optional owner/repo preflight hints are set once at
// construction and never mutated, so the adapter is safe for concurrent use.
//
// The preflight hints are read only by [GiteaSCMAdapter.VerifyAutoMergeScopes];
// the write methods take owner and repo per call, so the adapter stays
// project-agnostic on the write path.
type GiteaSCMAdapter struct {
	client         *httpkit.Client
	preflightOwner string
	preflightRepo  string
}

// NewGiteaSCMAdapter creates a [GiteaSCMAdapter] from adapter-specific
// configuration. Required config keys: "api_key" (access token) and "endpoint"
// (instance base URL; Gitea has no default host). Optional: "user_agent"
// (defaults to "sortie/dev") and "project" (the tracker "owner/repo", used only
// as an auto-merge preflight hint).
//
// The endpoint must parse as an absolute http(s) URL with a host; it is
// right-trimmed of trailing slashes and suffixed with "/api/v1" unless it
// already ends in it. An "owner/repo" project is parsed into the preflight
// hints that [GiteaSCMAdapter.VerifyAutoMergeScopes] reads; an absent or
// malformed value leaves both hints empty, so the preflight fails open and the
// write methods stay per-call. Construction performs no network I/O: splitting
// the project is a string parse, not a probe. A missing "api_key" returns a
// [*domain.SCMError] of kind [domain.ErrSCMAuth]; a missing "endpoint" or one
// that does not parse returns one of kind [domain.ErrSCMPayload]. The token
// travels only in the Authorization header set by the shared client and is
// never logged.
func NewGiteaSCMAdapter(adapterConfig map[string]any) (domain.SCMAdapter, error) {
	apiKey, _ := adapterConfig["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMAuth,
			Message: "missing required config key: api_key",
		}
	}

	endpointRaw, _ := adapterConfig["endpoint"].(string)
	if endpointRaw == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "missing required config key: endpoint",
		}
	}
	parsedEndpoint, ok := httpkit.ParseEndpoint(endpointRaw)
	if !ok {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: fmt.Sprintf("gitea: endpoint %q is not a valid absolute http(s) url", parsedEndpoint.Redacted),
		}
	}
	endpoint := parsedEndpoint.Base
	if !strings.HasSuffix(endpoint, "/api/v1") {
		endpoint += "/api/v1"
	}

	userAgent, _ := adapterConfig["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	project, _ := adapterConfig["project"].(string)
	preflightOwner, preflightRepo, ok := strings.Cut(project, "/")
	if !ok || preflightOwner == "" || preflightRepo == "" || strings.Contains(preflightRepo, "/") {
		preflightOwner, preflightRepo = "", ""
	}

	return &GiteaSCMAdapter{
		client:         newGiteaClient(endpoint, apiKey, userAgent),
		preflightOwner: preflightOwner,
		preflightRepo:  preflightRepo,
	}, nil
}

// MergePR merges the given pull request with the requested strategy via
// POST /repos/{owner}/{repo}/pulls/{index}/merge.
//
// An unsupported strategy returns a [*domain.SCMError] of kind
// [domain.ErrSCMPayload] before any request is issued. A non-empty
// expectedHeadSHA is sent as the stale-precondition SHA; empty commitTitle and
// commitMessage defer to Gitea's strategy-specific defaults. Gitea signals a
// successful merge with HTTP 200 and an empty body, so a successful
// [domain.MergeResult] has Merged true and an empty SHA.
//
// An HTTP 405 or 409 rejection maps to [domain.ErrSCMConflict]; the "already
// merged" marker is present only when a PR re-read confirms the merge landed. A
// 403 naming a required scope is enriched to a [domain.ErrSCMAuth] naming
// write:repository.
func (a *GiteaSCMAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	do := mapMergeStrategy(strategy)
	if do == "" {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: fmt.Sprintf("unsupported merge strategy: %q", strategy),
		}
	}

	payload, marshalErr := json.Marshal(giteaMergeOption{
		Do:           do,
		HeadCommitID: expectedHeadSHA,
		MergeTitle:   commitTitle,
		MergeMessage: commitMessage,
	})
	if marshalErr != nil {
		return domain.MergeResult{}, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to encode merge request body",
			Err:     marshalErr,
		}
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)
	if _, err := a.client.Send(ctx, http.MethodPost, path, bytes.NewReader(payload)); err != nil {
		scm := scmcore.AsMergeConflict(scmcore.ToSCMError(err))
		switch scm.Kind {
		case domain.ErrSCMConflict:
			return domain.MergeResult{}, a.resolveMergeConflict(ctx, prNumber, owner, repo, scm)
		case domain.ErrSCMAuth:
			return domain.MergeResult{}, enrichScopeError(scm)
		default:
			return domain.MergeResult{}, scm
		}
	}

	return domain.MergeResult{Merged: true}, nil
}

// DeleteBranch deletes the branch reference via
// DELETE /repos/{owner}/{repo}/branches/{branch}.
//
// The branch name is percent-encoded, so a slash-bearing name reaches Gitea as
// %2F. An already-gone branch (HTTP 404) returns a [*domain.SCMError] of kind
// [domain.ErrSCMNotFound]; the adapter returns this error rather than mapping it
// to nil, and the caller treats it as a successful no-op. A 403 naming a required
// scope is enriched to name write:repository. Returns nil on a 204.
func (a *GiteaSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		scm := scmcore.ToSCMError(err)
		if scm.Kind == domain.ErrSCMAuth {
			return enrichScopeError(scm)
		}
		return scm
	}
	return nil
}

// RemoveLabel removes the named label from the given pull request via
// DELETE /repos/{owner}/{repo}/issues/{index}/labels/{id}.
//
// Gitea's label-remove route is id-based, so the name is first resolved against
// the PR's own labels. A name absent from the PR issues no request and returns
// nil, the already-absent no-op the [domain.SCMAdapter.RemoveLabel] contract
// requires; a delete that races an external removal (HTTP 404) is likewise
// mapped to nil. Any other failure returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error {
	target := strings.ToLower(strings.TrimSpace(label))

	labels, err := a.fetchIssueLabels(ctx, prNumber, owner, repo)
	if err != nil {
		return err
	}

	id, found := resolveLabelID(labels, target)
	if !found {
		return nil
	}

	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s",
		url.PathEscape(owner), url.PathEscape(repo), prNumber, strconv.FormatInt(id, 10))
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		scm := scmcore.ToSCMError(err)
		if scm.Kind == domain.ErrSCMNotFound {
			return nil
		}
		return scm
	}
	return nil
}

// paginateSCM walks a page-number-paginated Gitea route through the shared
// paginator, using the same page and limit parameters and page ceiling every
// SCM-boundary reader shares, and converts the walk's result to the SCM
// boundary.
//
// decode returning a typed [*domain.SCMError] passes through unchanged; any
// other error the walk returns, a transport failure or a canceled context,
// converts through [scmcore.ToSCMError].
func paginateSCM[T any](ctx context.Context, client *httpkit.Client, path string, decode httpkit.LinkPageDecoder[T]) ([]T, error) {
	paginator := httpkit.NewPagePaginator(client, path, nil, decode, httpkit.PageOptions{
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

	items, err := paginator.All(ctx)
	if err != nil {
		return nil, scmcore.AsSCMError(err)
	}
	return items, nil
}
