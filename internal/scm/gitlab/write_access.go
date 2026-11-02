package gitlab

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// developerAccessLevel is the lowest project access level that can push:
// Developer. Maintainer and Owner rank above it.
const developerAccessLevel = 30

// gitlabMember is the part of a project member the access check reads.
type gitlabMember struct {
	AccessLevel int `json:"access_level"`
}

// CommentAuthorCanWrite implements [domain.CommentAuthorAccessReader]. A
// grant is remembered for the adapter's lifetime and a refusal for the one
// comment, so only a first or refused lookup sends requests.
func (a *GitLabAdapter) CommentAuthorCanWrite(ctx context.Context, c domain.Comment) (bool, error) {
	return a.access.CanWrite(ctx, c)
}

// lookupWriteAccess resolves the username to a numeric id, because the
// membership route takes only an id, then reads the inherited-aware
// membership so a group owner who is not a direct member still counts. The
// username is resolved on every call and never cached. Both requests count
// as one tracker operation.
func (a *GitLabAdapter) lookupWriteAccess(ctx context.Context, authorID string) (bool, error) {
	var granted bool
	err := trackermetrics.Track(a.metrics, "fetch_write_access", func() error {
		body, _, getErr := a.client.Get(ctx, "/users", url.Values{"username": {authorID}})
		if getErr != nil {
			return getErr
		}

		var users []gitlabUser
		if err := json.Unmarshal(body, &users); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse user search response",
				Err:     err,
			}
		}

		id := int64(-1)
		for _, u := range users {
			if strings.EqualFold(u.Username, authorID) {
				id = u.ID
				break
			}
		}
		if id < 0 {
			return nil
		}

		path := "/projects/" + a.projectPath + "/members/all/" + strconv.FormatInt(id, 10)
		body, _, getErr = a.client.Get(ctx, path, nil)
		if getErr != nil {
			if domain.IsNotFound(getErr) {
				return nil
			}
			return getErr
		}

		var member gitlabMember
		if err := json.Unmarshal(body, &member); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse project member response",
				Err:     err,
			}
		}
		granted = member.AccessLevel >= developerAccessLevel
		return nil
	})
	if err != nil {
		return false, err
	}
	return granted, nil
}
