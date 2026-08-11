package gitlab

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
)

// VerifyAutoMergeScopes reports whether the configured credential can
// perform GitLab's merge, branch-delete, and label-write routes, reading
// GET /personal_access_tokens/self once.
//
// requireContents is accepted and ignored: GitLab has no contents and
// pull-request scope split, so the single coarse api scope covers every
// write this adapter performs. A non-empty scopes with an empty missing
// means the credential can perform the writes; a non-empty missing
// (always ["api"]) is a verified gap. (nil, nil, nil) means no usable
// scope report is available and the caller fails open: this covers a
// granular token, whose Scopes carry no usable permission detail, an
// empty Scopes array, an unreadable response body, and an instance whose
// introspection route answers 404, which never distinguishes an absent
// route from a credential that cannot see it. Any other failure is
// returned as err.
func (a *GitLabSCMAdapter) VerifyAutoMergeScopes(ctx context.Context, requireContents bool) (scopes []string, missing []string, err error) {
	body, _, getErr := a.client.Get(ctx, "/personal_access_tokens/self", nil)
	if getErr != nil {
		scm := asSCMError(getErr)
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
