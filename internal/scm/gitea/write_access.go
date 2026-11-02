package gitea

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// CommentAuthorCanWrite reports whether the author of c can write to the
// configured repository.
//
// A grant is remembered for the adapter's lifetime and a refusal for that
// comment only. An empty AuthorID is refused without a request. A failure,
// including a token that cannot read the repository ([domain.ErrTrackerAuth]),
// is returned unchanged and remembers nothing.
func (a *GiteaAdapter) CommentAuthorCanWrite(ctx context.Context, c domain.Comment) (bool, error) {
	return a.access.CanWrite(ctx, c)
}

// lookupWriteAccess matches login against the repository's assignable
// accounts. The list is read for the repository, not for the author, so every
// failure, a 404 included, is a failure and never a refusal. The
// single-account assignee route is not used because it also accepts accounts
// that may only read pull requests.
func (a *GiteaAdapter) lookupWriteAccess(ctx context.Context, login string) (bool, error) {
	var granted bool
	err := trackermetrics.Track(a.metrics, "fetch_write_access", func() error {
		path := "/repos/" + url.PathEscape(a.owner) + "/" + url.PathEscape(a.repo) + "/assignees"

		body, _, err := a.client.Get(ctx, path, nil)
		if err != nil {
			return err
		}

		var assignees []giteaUser
		if err := json.Unmarshal(body, &assignees); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse assignees response",
				Err:     err,
			}
		}

		for _, u := range assignees {
			if strings.EqualFold(u.Login, login) {
				granted = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return granted, nil
}
