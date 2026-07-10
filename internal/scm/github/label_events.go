package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// maxLabelEventPages caps the number of journal pages read for a single
// pull request. The issue-events endpoint is oldest-first with no
// server-side since-filter, so the read walks from the tail (newest)
// backward and stops at this cap to bound event-heavy PRs.
const maxLabelEventPages = 20

// githubIssueEvent is one entry from the per-issue events endpoint. Only
// labeled and unlabeled events carry a label; actor is nullable for
// events attributed to a deleted account.
type githubIssueEvent struct {
	ID    int64  `json:"id"`
	Event string `json:"event"`
	Actor *struct {
		Login string `json:"login"`
	} `json:"actor"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`
	CreatedAt string `json:"created_at"`
}

// ListLabelEvents returns the labeled and unlabeled entries from the
// pull request's issue-event journal, normalized to [domain.LabelEvent]
// and oldest-first within the retained window.
//
// The endpoint returns all issue events oldest-first with no since-filter,
// so the command and its retraction signal live on the newest pages. This
// method retains the NEWEST events within maxLabelEventPages: it follows
// the Link "last" relation to the final page and walks backward via the
// "prev" relation. When the journal exceeds the cap, a Warn records the PR
// number and that truncation occurred. Returns a [*domain.SCMError] on any
// transport, API, or payload failure.
func (a *GitHubSCMAdapter) ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]domain.LabelEvent, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/events",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)
	params := url.Values{"per_page": {"100"}}

	body, headers, err := a.client.Get(ctx, path, params)
	if err != nil {
		return nil, toSCMError(err)
	}

	lastURL := httpkit.ParseLinkRel(headers.Get("Link"), "last")
	if lastURL == "" {
		return decodeLabelEvents(body)
	}

	// Multi-page journal: walk from the newest page backward, retaining at
	// most maxLabelEventPages pages, then re-flatten to oldest-first.
	pagesReversed := make([][]domain.LabelEvent, 0, maxLabelEventPages)
	nextURL := lastURL
	pageCount := 0
	truncated := false
	for {
		pageBody, pageHeaders, pageErr := a.client.GetURL(ctx, nextURL)
		if pageErr != nil {
			return nil, toSCMError(pageErr)
		}
		pageEvents, decErr := decodeLabelEvents(pageBody)
		if decErr != nil {
			return nil, decErr
		}
		pagesReversed = append(pagesReversed, pageEvents)
		pageCount++

		prevURL := httpkit.ParseLinkRel(pageHeaders.Get("Link"), "prev")
		if prevURL == "" {
			break
		}
		if pageCount >= maxLabelEventPages {
			truncated = true
			break
		}
		nextURL = prevURL
	}

	if truncated {
		slog.WarnContext(ctx, "label events response truncated at page limit",
			slog.String("owner", owner),
			slog.String("repo", repo),
			slog.Int("pr_number", prNumber),
			slog.Int("max_pages", maxLabelEventPages),
		)
	}

	result := make([]domain.LabelEvent, 0)
	for _, page := range slices.Backward(pagesReversed) {
		result = append(result, page...)
	}
	return result, nil
}

// decodeLabelEvents parses one page of issue events, retaining only
// labeled and unlabeled entries and normalizing each to a
// [domain.LabelEvent] with a lowercased label name and a UTC timestamp.
// Returns a non-nil empty slice when the page carries no label events and
// a [*domain.SCMError] with Kind [domain.ErrSCMPayload] on a malformed
// payload.
func decodeLabelEvents(body []byte) ([]domain.LabelEvent, error) {
	var raw []githubIssueEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse issue events response",
			Err:     err,
		}
	}

	events := make([]domain.LabelEvent, 0, len(raw))
	for _, e := range raw {
		if e.Event != "labeled" && e.Event != "unlabeled" {
			continue
		}
		// A labeled or unlabeled event without a label name carries no
		// command signal, so it is skipped defensively.
		if e.Label == nil || e.Label.Name == "" {
			continue
		}

		actor := ""
		if e.Actor != nil {
			actor = e.Actor.Login
		}

		at, parseErr := time.Parse(time.RFC3339, e.CreatedAt)
		if parseErr != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse issue event created_at",
				Err:     parseErr,
			}
		}

		events = append(events, domain.LabelEvent{
			// Normalize the id so the orchestrator's lexical (At, id) mark
			// comparison matches chronological order even for events that
			// share a timestamp.
			ID:    sortableEventID(e.ID),
			Label: strings.ToLower(e.Label.Name),
			Actor: actor,
			Added: e.Event == "labeled",
			At:    at.UTC(),
		})
	}
	return events, nil
}

// sortableEventID formats a numeric journal-event id as a fixed-width,
// zero-padded decimal so lexical string comparison matches numeric order.
// The width covers the maximum int64.
func sortableEventID(id int64) string {
	return fmt.Sprintf("%019d", id)
}

// RemoveLabel deletes the named label from the given pull request. An
// already-absent label (HTTP 404) is a successful no-op. Returns a
// [*domain.SCMError] on any other failure.
func (a *GitHubSCMAdapter) RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s",
		url.PathEscape(owner), url.PathEscape(repo), prNumber, url.PathEscape(label))
	if err := a.client.SendNoBody(ctx, http.MethodDelete, path); err != nil {
		scm := toSCMError(err)
		if scm.Kind == domain.ErrSCMNotFound {
			return nil
		}
		return scm
	}
	return nil
}
