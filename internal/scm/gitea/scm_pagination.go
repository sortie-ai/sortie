package gitea

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/sortie-ai/sortie/internal/httpkit"
)

// scmMaxPages caps the number of pages fetched for a single SCM list read. At 50
// entries per page this bounds a read at 2500 entries, generous for the
// short-lived, low-traffic pull requests Sortie manages.
const scmMaxPages = 50

// paginatePages fetches every page of a page-number-paginated Gitea route and
// returns the accumulated elements decoded by decode.
//
// It requests fixed-size pages starting at page 1 and stops when a page is
// shorter than the page size. The walk is capped at [scmMaxPages]; on reaching
// the cap it logs a warning naming the route and returns the pages read so far.
// Cancellation is checked before each page and surfaced through
// [giteaToSCMError], as is a transport or HTTP failure; a decode failure is
// returned as produced by decode. The returned slice is non-nil even when the
// route yields no elements.
func paginatePages[T any](ctx context.Context, client *httpkit.Client, path string, decode func([]byte) ([]T, error)) ([]T, error) {
	const limit = 50

	acc := make([]T, 0)
	page := 1
	for {
		if err := ctx.Err(); err != nil {
			return nil, giteaToSCMError(err)
		}

		params := url.Values{
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(limit)},
		}
		body, _, err := client.Get(ctx, path, params)
		if err != nil {
			return nil, giteaToSCMError(err)
		}

		batch, err := decode(body)
		if err != nil {
			return nil, err
		}

		acc = append(acc, batch...)
		if len(batch) < limit {
			break
		}

		page++
		if page > scmMaxPages {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", path),
				slog.Int("max_pages", scmMaxPages))
			break
		}
	}

	return acc, nil
}
