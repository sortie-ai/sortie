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

// giteaTimelineEntry is one entry from the issue timeline route. Only entries
// whose Type is "label" carry a Label; Body is "1" for a label add and "" for a
// label remove.
type giteaTimelineEntry struct {
	ID    int64     `json:"id"`
	Type  string    `json:"type"`
	Body  string    `json:"body"`
	User  giteaUser `json:"user"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`
	CreatedAt string `json:"created_at"`
}

// ListLabelEvents returns the label add and remove events from the given PR's
// timeline, normalized to [domain.LabelEvent] and oldest-first.
//
// Pull requests share the issue timeline route. Only "label" entries with a
// named label are retained; the label is lowercased and Added is true only for a
// body of "1". The timeline arrives oldest-first, so the result needs no re-sort.
// The returned slice is non-nil even when empty. A retained entry whose
// created_at does not parse as RFC 3339 fails the read with a
// [*domain.SCMError] of Kind [domain.ErrSCMPayload]; an entry skipped for its
// kind or its missing label name is never parsed and cannot fail the read.
func (a *GiteaSCMAdapter) ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]domain.LabelEvent, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)

	entries, err := paginateSCM(ctx, a.client, path, func(body []byte) ([]giteaTimelineEntry, error) {
		var batch []giteaTimelineEntry
		if jsonErr := json.Unmarshal(body, &batch); jsonErr != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse timeline response",
				Err:     jsonErr,
			}
		}
		return batch, nil
	})
	if err != nil {
		return nil, err
	}

	events := make([]domain.LabelEvent, 0, len(entries))
	for _, e := range entries {
		if e.Type != "label" {
			continue
		}
		if e.Label == nil || e.Label.Name == "" {
			continue
		}

		at, err := scmcore.ParseTimestamp("timeline entry created_at", e.CreatedAt)
		if err != nil {
			return nil, err
		}

		events = append(events, domain.LabelEvent{
			ID:    scmcore.SortableEventID(e.ID),
			Label: strings.ToLower(e.Label.Name),
			Actor: e.User.Login,
			Added: e.Body == "1",
			At:    at,
		})
	}

	return events, nil
}
