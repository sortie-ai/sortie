package httpkit

import (
	"context"
	"net/http"
	"net/url"
	"slices"
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

// Paginator accumulates items across token-based or Link-header pagination.
type Paginator[T any] struct {
	client      *Client
	path        string
	baseParams  url.Values
	tokenParam  string
	tokenDecode TokenPageDecoder[T]
	linkDecode  LinkPageDecoder[T]
	options     PaginatorOptions
}

// NewTokenPaginator constructs a [Paginator] for APIs that expose a next-page token.
func NewTokenPaginator[T any](client *Client, path string, baseParams url.Values, tokenParam string, decode TokenPageDecoder[T], opts PaginatorOptions) *Paginator[T] {
	return &Paginator[T]{
		client:      client,
		path:        path,
		baseParams:  baseParams,
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
		linkDecode: decode,
		options:    opts,
	}
}

// All fetches every page and returns the accumulated items.
func (p *Paginator[T]) All(ctx context.Context) ([]T, error) {
	if p.linkDecode != nil {
		return p.allLinks(ctx)
	}
	return p.allTokens(ctx)
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
