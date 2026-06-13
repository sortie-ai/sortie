package linear

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
)

// maxPages bounds the cursor-pagination loop. At 50 items per page this allows
// up to 10,000 results before the loop stops and returns what it has.
const maxPages = 200

// pageDecoder decodes one connection page into its items and page info.
type pageDecoder[T any] func(body []byte) (items []T, info linearPageInfo, err error)

// paginate runs the POST cursor-pagination loop. It carries the cursor in
// variables["after"], records rate-limit headers per page, classifies
// body-level errors, and accumulates items until the connection is exhausted.
//
// It returns [domain.ErrTrackerMissingCursor] when a page reports another page
// but omits the end cursor. When the page count reaches [maxPages] with more
// pages available, it logs a WARN and returns the accumulated items.
func paginate[T any](ctx context.Context, client graphQLClient, query string, variables map[string]any, decode pageDecoder[T], log *slog.Logger) ([]T, error) {
	items := make([]T, 0)
	pageCount := 0
	after := ""

	for {
		if after == "" {
			variables["after"] = nil
		} else {
			variables["after"] = after
		}

		body, headers, err := client.Execute(ctx, query, variables)
		if err != nil {
			return nil, err
		}
		recordRateLimit(headers, log)

		pageItems, info, err := decode(body)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)
		pageCount++

		if !info.HasNextPage {
			return items, nil
		}

		if info.EndCursor == "" {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerMissingCursor,
				Message: "linear graphql: next page reported without an end cursor",
			}
		}

		if pageCount == maxPages {
			if log != nil {
				log.Warn("pagination limit reached", slog.Int("max_pages", maxPages))
			}
			return items, nil
		}

		after = info.EndCursor
	}
}

// decodeIssuesPage decodes a candidate or by-states issues connection page.
func decodeIssuesPage(body []byte) ([]linearIssue, linearPageInfo, error) {
	var resp graphQLResponse[issuesData]
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, linearPageInfo{}, payloadError(err)
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return nil, linearPageInfo{}, classified
	}
	return resp.Data.Issues.Nodes, resp.Data.Issues.PageInfo, nil
}

// decodeCommentsPage decodes a comment continuation page. It runs the body-first
// classifier on every page because the continuation re-selects the parent
// issue, so a not-found mid-pagination surfaces as [domain.ErrTrackerNotFound].
func decodeCommentsPage(body []byte) ([]linearComment, linearPageInfo, error) {
	var resp graphQLResponse[commentData]
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, linearPageInfo{}, payloadError(err)
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return nil, linearPageInfo{}, classified
	}
	if resp.Data.Issue == nil {
		return nil, linearPageInfo{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: "linear graphql: issue not found",
		}
	}
	return resp.Data.Issue.Comments.Nodes, resp.Data.Issue.Comments.PageInfo, nil
}

// payloadError wraps a JSON unmarshal failure as a payload error.
func payloadError(err error) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "linear graphql: failed to parse response",
		Err:     err,
	}
}
