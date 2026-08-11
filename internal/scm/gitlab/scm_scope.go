package gitlab

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// VerifyAutoMergeScopes reports whether the configured credential can
// perform GitLab's merge, branch-delete, and label-write routes, reading
// GET /personal_access_tokens/self once.
//
// requireContents is accepted and ignored, because GitLab has one coarse
// api scope covering every write this adapter performs rather than a
// contents and pull-request split. A non-empty scopes slice with an empty
// missing list means the credential can perform those writes; a non-empty
// missing list (always ["api"]) is a verified gap. The (nil, nil, nil)
// sentinel means no usable scope report exists and the caller fails open,
// which covers a granular token whose scopes carry no permission detail,
// an empty scopes array, an unreadable response body, and an instance
// answering 404 on the introspection route, a status that never separates
// an absent route from a credential that cannot see it. Any other failure
// is returned as err.
func (a *GitLabSCMAdapter) VerifyAutoMergeScopes(ctx context.Context, requireContents bool) (scopes []string, missing []string, err error) {
	body, _, getErr := a.client.Get(ctx, "/personal_access_tokens/self", nil)
	if getErr != nil {
		scm := scmcore.AsSCMError(getErr)
		if scm.Kind == domain.ErrSCMNotFound {
			a.log.Warn("gitlab token introspection route unavailable")
			return nil, nil, nil
		}
		return nil, nil, scm
	}

	var info gitlabTokenInfo
	if jsonErr := json.Unmarshal(body, &info); jsonErr != nil {
		a.log.Warn("gitlab token introspection unreadable")
		return nil, nil, nil
	}

	if info.Revoked || !info.Active {
		a.log.Warn("gitlab token reports revoked or inactive")
	}

	if info.Granular || slices.Contains(info.Scopes, "granular") || len(info.Scopes) == 0 {
		return nil, nil, nil
	}
	if slices.Contains(info.Scopes, "api") {
		return info.Scopes, nil, nil
	}
	return nil, []string{"api"}, nil
}
