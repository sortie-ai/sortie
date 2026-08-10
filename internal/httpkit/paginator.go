package httpkit

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// TokenPageDecoder decodes a token-paginated response page.
type TokenPageDecoder[T any] func(body []byte) (items []T, nextToken string, err error)

// LinkPageDecoder decodes a Link-header paginated response page.
type LinkPageDecoder[T any] func(body []byte) ([]T, error)

// PaginatorOptions configures page limits and limit notifications.
type PaginatorOptions struct {
	// MaxPages limits the number of pages fetched when greater than zero.
	MaxPages int

	// OnLimitReached is invoked once when a next page exists but MaxPages blocks it.
	OnLimitReached func(limit int)
}

// PageOptions configures a page-number-paginated walk: the query parameter
// names the route expects, the page size, and the same limit and
// limit-notification controls [PaginatorOptions] offers the token and Link
// modes.
type PageOptions struct {
	// PageParam is the query key carrying the one-based page number
	// (for example "page").
	PageParam string

	// SizeParam is the query key carrying the requested page size (for
	// example "per_page" or "limit").
	SizeParam string

	// PageSize is the number of elements requested per page. A page
	// returning fewer than PageSize elements ends the walk.
	PageSize int

	// MaxPages limits the number of pages fetched when greater than zero.
	MaxPages int

	// OnLimitReached is invoked once when a next page exists but MaxPages blocks it.
	OnLimitReached func(limit int)
}

// paginationMode selects which walk [Paginator.All] runs. Each constructor
// sets it explicitly so a third mode is never reached by testing which
// decoder field happens to be non-nil.
type paginationMode int

const (
	paginationModeToken paginationMode = iota
	paginationModeLink
	paginationModePage
)

// Paginator accumulates items across token-based, Link-header, or
// page-number pagination.
type Paginator[T any] struct {
	client      *Client
	path        string
	baseParams  url.Values
	mode        paginationMode
	tokenParam  string
	tokenDecode TokenPageDecoder[T]
	linkDecode  LinkPageDecoder[T]
	pageDecode  LinkPageDecoder[T]
	pageOptions PageOptions
	options     PaginatorOptions
}

// NewTokenPaginator constructs a [Paginator] for APIs that expose a next-page token.
func NewTokenPaginator[T any](client *Client, path string, baseParams url.Values, tokenParam string, decode TokenPageDecoder[T], opts PaginatorOptions) *Paginator[T] {
	return &Paginator[T]{
		client:      client,
		path:        path,
		baseParams:  baseParams,
		mode:        paginationModeToken,
		tokenParam:  tokenParam,
		tokenDecode: decode,
		options:     opts,
	}
}

// NewLinkPaginator constructs a [Paginator] for APIs that expose pagination in Link headers.
func NewLinkPaginator[T any](client *Client, path string, baseParams url.Values, decode LinkPageDecoder[T], opts PaginatorOptions) *Paginator[T] {
	return &Paginator[T]{
		client:     client,
		path:       path,
		baseParams: baseParams,
		mode:       paginationModeLink,
		linkDecode: decode,
		options:    opts,
	}
}

// NewPagePaginator constructs a [Paginator] for APIs that expose
// page-number pagination: the walk requests page 1, 2, 3, ... through
// opts.PageParam and opts.SizeParam, and stops once a page returns fewer
// than opts.PageSize elements. decode reads one page's body, so an
// object-shaped response (for example a combined status carrying a nested
// array) decodes as readily as a top-level array.
func NewPagePaginator[T any](client *Client, path string, baseParams url.Values, decode LinkPageDecoder[T], opts PageOptions) *Paginator[T] {
	return &Paginator[T]{
		client:      client,
		path:        path,
		baseParams:  baseParams,
		mode:        paginationModePage,
		pageDecode:  decode,
		pageOptions: opts,
	}
}

// All fetches every page and returns the accumulated items.
func (p *Paginator[T]) All(ctx context.Context) ([]T, error) {
	switch p.mode {
	case paginationModeLink:
		return p.allLinks(ctx)
	case paginationModePage:
		return p.allPages(ctx)
	default:
		return p.allTokens(ctx)
	}
}

func (p *Paginator[T]) allTokens(ctx context.Context) ([]T, error) {
	items := make([]T, 0)
	pageCount := 0
	nextToken := ""

	for {
		params := cloneValues(p.baseParams)
		if nextToken != "" {
			params.Set(p.tokenParam, nextToken)
		}

		body, _, err := p.client.Get(ctx, p.path, params)
		if err != nil {
			return nil, err
		}
		pageCount++

		pageItems, token, err := p.tokenDecode(body)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)

		if token == "" {
			return items, nil
		}

		if p.options.MaxPages > 0 && pageCount == p.options.MaxPages {
			if p.options.OnLimitReached != nil {
				p.options.OnLimitReached(p.options.MaxPages)
			}
			return items, nil
		}

		nextToken = token
	}
}

func (p *Paginator[T]) allLinks(ctx context.Context) ([]T, error) {
	items := make([]T, 0)
	pageCount := 0
	nextURL := ""
	useFullURL := false

	for {
		var (
			body    []byte
			headers http.Header
			err     error
		)

		if useFullURL {
			body, headers, err = p.client.GetURL(ctx, nextURL)
		} else {
			body, headers, err = p.client.Get(ctx, p.path, p.baseParams)
		}
		if err != nil {
			return nil, err
		}
		pageCount++

		pageItems, err := p.linkDecode(body)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)

		nextURL = parseLinkNext(headers.Get("Link"))
		if nextURL == "" {
			return items, nil
		}

		if p.options.MaxPages > 0 && pageCount == p.options.MaxPages {
			if p.options.OnLimitReached != nil {
				p.options.OnLimitReached(p.options.MaxPages)
			}
			return items, nil
		}

		useFullURL = true
	}
}

// allPages walks a page-number-paginated route, requesting page 1, 2, 3,
// ... until a page returns fewer than pageOptions.PageSize elements or the
// pageOptions.MaxPages ceiling stops the walk. A classified client error is
// returned unconverted, so a caller on the source-control path and a caller
// on the CI path can each apply their own conversion.
func (p *Paginator[T]) allPages(ctx context.Context) ([]T, error) {
	items := make([]T, 0)
	page := 1
	pageCount := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		params := cloneValues(p.baseParams)
		params.Set(p.pageOptions.PageParam, strconv.Itoa(page))
		params.Set(p.pageOptions.SizeParam, strconv.Itoa(p.pageOptions.PageSize))

		body, _, err := p.client.Get(ctx, p.path, params)
		if err != nil {
			return nil, err
		}
		pageCount++

		pageItems, err := p.pageDecode(body)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)

		if len(pageItems) < p.pageOptions.PageSize {
			return items, nil
		}

		if p.pageOptions.MaxPages > 0 && pageCount == p.pageOptions.MaxPages {
			if p.pageOptions.OnLimitReached != nil {
				p.pageOptions.OnLimitReached(p.pageOptions.MaxPages)
			}
			return items, nil
		}

		page++
	}
}

func cloneValues(values url.Values) url.Values {
	if values == nil {
		return url.Values{}
	}
	clone := make(url.Values, len(values))
	for key, src := range values {
		clone[key] = append([]string(nil), src...)
	}
	return clone
}

func parseLinkNext(header string) string {
	return ParseLinkRel(header, "next")
}

// ParseLinkRel returns the URL carried by the given rel token (for
// example "next", "prev", or "last") in an RFC 8288 Link header, or the
// empty string when the header is empty or carries no matching relation.
//
// The rel attribute value may be quoted or bare and may list several
// space-separated relation types (RFC 8288 permits rel="next prev"); the
// match against rel is case-insensitive on each token.
func ParseLinkRel(header, rel string) string {
	if header == "" {
		return ""
	}

	for segment := range strings.SplitSeq(header, ",") {
		parts := strings.Split(segment, ";")
		if len(parts) < 2 {
			continue
		}

		hasRel := false
		for _, param := range parts[1:] {
			name, value, ok := strings.Cut(param, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
				continue
			}
			// RFC 8288 permits a bare or quoted value and multiple
			// space-separated relation tokens (rel="next prev").
			value = strings.Trim(strings.TrimSpace(value), `"`)
			if slices.ContainsFunc(strings.Fields(value), func(token string) bool {
				return strings.EqualFold(token, rel)
			}) {
				hasRel = true
				break
			}
		}
		if !hasRel {
			continue
		}

		urlPart := strings.TrimSpace(parts[0])
		start := strings.Index(urlPart, "<")
		if start == -1 {
			continue
		}
		end := strings.Index(urlPart[start:], ">")
		if end == -1 {
			continue
		}
		return urlPart[start+1 : start+end]
	}

	return ""
}
