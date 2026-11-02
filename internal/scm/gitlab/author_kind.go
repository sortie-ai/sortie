package gitlab

import (
	"context"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// authorKinds remembers the kind of each author by username. It is safe for
// concurrent use and never holds its mutex across a request.
type authorKinds struct {
	mu    sync.Mutex
	known map[string]domain.CommentAuthorKind // guarded by mu
}

func newAuthorKinds() *authorKinds {
	return &authorKinds{known: make(map[string]domain.CommentAuthorKind)}
}

func (k *authorKinds) get(authorID string) (domain.CommentAuthorKind, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	kind, ok := k.known[authorID]
	return kind, ok
}

func (k *authorKinds) remember(authorID string, kind domain.CommentAuthorKind) {
	k.mu.Lock()
	k.known[authorID] = kind
	k.mu.Unlock()
}

// LookupCommentAuthorKind implements [domain.CommentAuthorKindReader]. It
// resolves the username to an account and reads the bot marker through the
// same per-user lookup the change read uses, so both report one kind for an
// author. A username the search does not return answers person, as a
// not-found account does in the change read. Both requests count as one
// tracker operation.
func (a *GitLabAdapter) LookupCommentAuthorKind(ctx context.Context, c domain.Comment) (domain.CommentAuthorKind, error) {
	if c.AuthorID == "" {
		return domain.CommentAuthorUnknown, nil
	}
	if kind, ok := a.kinds.get(c.AuthorID); ok {
		return kind, nil
	}

	kind := domain.CommentAuthorPerson
	err := trackermetrics.Track(a.metrics, "fetch_author_kind", func() error {
		user, found, findErr := a.findUserByUsername(ctx, c.AuthorID)
		if findErr != nil || !found {
			return findErr
		}
		bot, botErr := a.bots.lookup(ctx, user.ID)
		if botErr != nil {
			return botErr
		}
		if bot {
			kind = domain.CommentAuthorBot
		}
		return nil
	})
	if err != nil {
		return domain.CommentAuthorUnknown, err
	}

	a.kinds.remember(c.AuthorID, kind)
	return kind, nil
}
