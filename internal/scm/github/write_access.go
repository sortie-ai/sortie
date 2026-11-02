package github

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// collaboratorPermission is the part of the collaborator permission
// response the lookup reads.
type collaboratorPermission struct {
	Permission string `json:"permission"`
}

// CommentAuthorCanWrite reports whether the author of c has write access
// to the configured repository. A grant is remembered for the adapter's
// lifetime and a refusal for c alone, so an edited comment is not asked
// about again. An empty AuthorID is refused without a request. A token
// that cannot ask fails with [domain.ErrTrackerAuth], and nothing is
// remembered on error.
func (a *GitHubAdapter) CommentAuthorCanWrite(ctx context.Context, c domain.Comment) (bool, error) {
	return a.access.CanWrite(ctx, c)
}

// lookupWriteAccess asks the collaborator permission route about login.
// The route's permission field reports the legacy base roles, so a
// maintainer reads as write and a triager as read; role_name is not read.
func (a *GitHubAdapter) lookupWriteAccess(ctx context.Context, login string) (bool, error) {
	path := "/repos/" + url.PathEscape(a.owner) + "/" + url.PathEscape(a.repo) +
		"/collaborators/" + url.PathEscape(login) + "/permission"

	var granted bool
	err := trackermetrics.Track(a.metrics, "fetch_write_access", func() error {
		body, _, err := a.client.Get(ctx, path, nil)
		if err != nil {
			if domain.IsNotFound(err) {
				return nil
			}
			return err
		}

		var level collaboratorPermission
		if err := json.Unmarshal(body, &level); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse collaborator permission response",
				Err:     err,
			}
		}
		granted = level.Permission == "admin" || level.Permission == "write"
		return nil
	})
	if err != nil {
		return false, err
	}
	return granted, nil
}
