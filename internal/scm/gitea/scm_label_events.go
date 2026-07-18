package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
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

// sortableEventID formats a numeric timeline id as a fixed-width, zero-padded
// decimal so lexical string comparison matches numeric order. The width covers
// the maximum int64, keeping (At, ID) string ordering consistent with journal
// order for entries that share a timestamp.
func sortableEventID(id int64) string {
	return fmt.Sprintf("%019d", id)
}

// ListLabelEvents returns the label add and remove events from the given PR's
// timeline, normalized to [domain.LabelEvent] and oldest-first.
//
// Pull requests share the issue timeline route. Only "label" entries with a
// named label are retained; the label is lowercased and Added is true only for a
// body of "1". The timeline arrives oldest-first, so the result needs no re-sort.
// The returned slice is non-nil even when empty; a failure returns a
// [*domain.SCMError].
func (a *GiteaSCMAdapter) ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]domain.LabelEvent, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)

	entries, err := paginatePages(ctx, a.client, path, func(body []byte) ([]giteaTimelineEntry, error) {
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

		events = append(events, domain.LabelEvent{
			ID:    sortableEventID(e.ID),
			Label: strings.ToLower(e.Label.Name),
			Actor: e.User.Login,
			Added: e.Body == "1",
			At:    parseUTC(e.CreatedAt),
		})
	}

	return events, nil
}

// fetchIssueLabels returns the labels currently attached to the given pull
// request via GET /repos/{owner}/{repo}/issues/{prNumber}/labels.
//
// The read is paginated through [paginatePages]; a transport or HTTP failure is
// mapped through [giteaToSCMError] and a decode failure to a [*domain.SCMError]
// of kind [domain.ErrSCMPayload]. The returned slice is non-nil even when the PR
// carries no labels.
func (a *GiteaSCMAdapter) fetchIssueLabels(ctx context.Context, prNumber int, owner, repo string) ([]giteaLabel, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)

	return paginatePages(ctx, a.client, path, func(body []byte) ([]giteaLabel, error) {
		var batch []giteaLabel
		if jsonErr := json.Unmarshal(body, &batch); jsonErr != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse issue labels response",
				Err:     jsonErr,
			}
		}
		return batch, nil
	})
}

// resolveLabelID returns the id of the first label whose name matches target
// case-insensitively, reporting whether a match was found.
//
// Case-insensitive matching bridges Gitea's original-cased stored names and
// Sortie's lowercased configured label, so an unresolved name is the
// already-absent no-op that issues no delete.
func resolveLabelID(labels []giteaLabel, target string) (int64, bool) {
	for _, l := range labels {
		if strings.EqualFold(l.Name, target) {
			return l.ID, true
		}
	}
	return 0, false
}
