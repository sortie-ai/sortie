package gitlab

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// botFlags resolves whether a GitLab account is a platform bot through the
// per-user route and remembers every answer for its own lifetime, because
// an account's bot marker does not change. The tracker and source-control
// adapters each hold one bound to their own client. It is safe for
// concurrent use.
type botFlags struct {
	client *httpkit.Client

	mu    sync.Mutex
	known map[int64]bool // guarded by mu
}

func newBotFlags(client *httpkit.Client) *botFlags {
	return &botFlags{client: client, known: make(map[int64]bool)}
}

// lookup reports whether userID belongs to a platform bot account. A
// userID that is not positive answers false with no request, and a
// not-found answer is cached and answers false.
//
// Any other failure is returned unconverted and never cached, so a route
// that recovers classifies the user on the next call: the client's error
// as it classified it, or a [*domain.TrackerError] of kind
// [domain.ErrTrackerPayload] for a body that does not decode. Callers
// convert it to their own error family.
func (b *botFlags) lookup(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}

	b.mu.Lock()
	cached, hit := b.known[userID]
	b.mu.Unlock()
	if hit {
		return cached, nil
	}

	body, _, err := b.client.Get(ctx, "/users/"+strconv.FormatInt(userID, 10), nil)
	if err != nil {
		if domain.IsNotFound(err) {
			b.remember(userID, false)
			return false, nil
		}
		return false, err
	}

	var detail gitlabUserDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return false, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse user response",
			Err:     err,
		}
	}

	b.remember(userID, detail.Bot)
	return detail.Bot, nil
}

func (b *botFlags) remember(userID int64, bot bool) {
	b.mu.Lock()
	b.known[userID] = bot
	b.mu.Unlock()
}
