package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// writeRepositoryScope is Gitea's single coarse token scope covering both the
// merge and branch-delete write routes. Gitea has no contents/pull_request
// split, so this one value names every write requirement.
const writeRepositoryScope = "write:repository"

// enrichScopeError rewrites a [domain.ErrSCMAuth] message to name the required
// Gitea scope when the 403 body reports a scope rejection.
//
// It fires only when the message carries Gitea's scope-rejection marker and
// otherwise returns scm unchanged. The named scope is the first entry parsed from
// the required=[...] list, falling back to the literal write:repository when a
// bounded body snippet truncated the list. It changes only the message, never the
// kind, so the auth-class handling upstream is preserved.
func enrichScopeError(scm *domain.SCMError) *domain.SCMError {
	if !strings.Contains(scm.Message, "does not have at least one of required scope") {
		return scm
	}

	required := parseBracketList(scm.Message, "required=[")
	tokenScopes := parseBracketList(scm.Message, "token scope=[")

	requiredScope := writeRepositoryScope
	if len(required) > 0 {
		requiredScope = required[0]
	}

	scm.Message = "gitea token missing required scope " + requiredScope +
		" (token scope=[" + strings.Join(tokenScopes, " ") + "]); grant " + writeRepositoryScope
	return scm
}

// parseBracketList extracts the bracketed, delimiter-separated tokens that follow
// prefix in detail, for example the scope names inside "required=[a b]".
//
// prefix must include the opening bracket; parsing runs from the character after
// it to the next closing bracket. A missing prefix or missing closing bracket
// (a truncated snippet) yields nil. Tokens are split on commas and whitespace,
// with empties dropped.
func parseBracketList(detail, prefix string) []string {
	start := strings.Index(detail, prefix)
	if start < 0 {
		return nil
	}
	start += len(prefix)

	end := strings.IndexByte(detail[start:], ']')
	if end < 0 {
		return nil
	}

	inner := strings.ReplaceAll(detail[start:start+end], ",", " ")
	return strings.Fields(inner)
}

// giteaRepoPermissions decodes the permissions block of
// GET /repos/{owner}/{repo} for the auto-merge push gate.
//
// Push reports whether the token's user has write access to the repository. It
// is a user-role signal, not the token's scope: a read-scoped admin token still
// reports push true, so this gate catches a wrong-owner or read-only token but
// not a missing write scope.
type giteaRepoPermissions struct {
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// VerifyAutoMergeScopes runs the Gitea auto-merge preflight so the orchestrator's
// startup scope check runs for Gitea instead of being skipped.
//
// Gitea cannot verify a token's own write scope at startup: it exposes no scope
// header and no side-effect-free scope probe, and permissions.push reports the
// user's role rather than the token's scope. So scope verification fails open,
// returning the (nil, nil, nil) "unable to verify" sentinel, and the runtime 403
// on the first write names the missing scope instead. The requireContents flag
// is accepted but inert, because Gitea has one coarse write:repository scope
// covering both merge and branch delete rather than a contents and
// pull-request split.
//
// When an owner/repo preflight hint is present, the method adds a user-role push
// gate: it reads permissions.push from GET /repos/{owner}/{repo} and, when push
// is false, returns write:repository in missing. That missing entry reflects a
// permissions.push role gap (a wrong-owner or read-only token), not a verified
// scope gap. A probe failure returns a [*domain.SCMError] so the caller can
// classify its transport class; an absent hint returns the fail-open sentinel.
func (a *GiteaSCMAdapter) VerifyAutoMergeScopes(ctx context.Context, requireContents bool) (scopes []string, missing []string, err error) {
	if a.preflightOwner == "" || a.preflightRepo == "" {
		return nil, nil, nil
	}

	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(a.preflightOwner), url.PathEscape(a.preflightRepo))
	body, _, getErr := a.client.Get(ctx, path, nil)
	if getErr != nil {
		return nil, nil, scmcore.ToSCMError(getErr)
	}

	var perms giteaRepoPermissions
	if jsonErr := json.Unmarshal(body, &perms); jsonErr != nil {
		return nil, nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse repository permissions response",
			Err:     jsonErr,
		}
	}

	if !perms.Permissions.Push {
		return nil, []string{writeRepositoryScope}, nil
	}
	return nil, nil, nil
}
