package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// maxErrorBody is the maximum number of bytes read from non-success
// response bodies for diagnostic error messages.
const maxErrorBody = 512

type githubClient struct {
	httpClient *http.Client
	baseURL    string
	authHeader string
	userAgent  string
	apiVersion string
}

func newGitHubClient(baseURL, token, userAgent string) *githubClient {
	return &githubClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Bearer " + token,
		userAgent:  userAgent,
		apiVersion: "2026-03-10",
	}
}

// setHeaders applies the common GitHub API headers to a request.
func (c *githubClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", c.apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
}

// do executes an HTTP request against the GitHub REST API and returns
// the response body and the Link rel="next" URL on success. Non-200
// responses are classified via [classifyHTTPError]. Context
// cancellation is propagated directly.
func (c *githubClient) do(ctx context.Context, method, path string, params url.Values) ([]byte, string, error) { //nolint:unparam // method is GET today but the signature mirrors doJSON for consistency
	reqURL := c.baseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil) //nolint:gosec // URL is constructed from operator-configured base URL + internal API paths, not user data
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("failed to build request: %s %s", method, path),
			Err:     err,
		}
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req) //nolint:gosec // controlled URL, see above
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: network error", method, path),
			Err:     err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", &domain.TrackerError{
				Kind:    domain.ErrTrackerTransport,
				Message: "failed to read response body",
				Err:     err,
			}
		}
		linkNext := parseLinkNext(resp.Header.Get("Link"))
		return body, linkNext, nil
	}

	return nil, "", classifyHTTPError(resp, method, path)
}

// doURL executes a GET request using a full URL (typically from a Link
// header). Sets the same authentication and API version headers as
// [githubClient.do]. Used exclusively for pagination.
func (c *githubClient) doURL(ctx context.Context, fullURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil) //nolint:gosec // fullURL comes from GitHub Link headers, not user input
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("failed to build pagination request: %s", fullURL),
			Err:     err,
		}
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req) //nolint:gosec // controlled URL from Link header
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("GET %s: network error", fullURL),
			Err:     err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", &domain.TrackerError{
				Kind:    domain.ErrTrackerTransport,
				Message: "failed to read response body",
				Err:     err,
			}
		}
		linkNext := parseLinkNext(resp.Header.Get("Link"))
		return body, linkNext, nil
	}

	return nil, "", classifyHTTPError(resp, http.MethodGet, fullURL)
}

// doJSON executes an HTTP request with a JSON body. Successful
// responses (200-299) return the response body. Non-success responses
// are classified via [classifyHTTPError].
func (c *githubClient) doJSON(ctx context.Context, method, path string, body io.Reader) ([]byte, error) { //nolint:unparam // callers may inspect response body in future operations
	reqURL := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("failed to build request: %s %s", method, path),
			Err:     err,
		}
	}

	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: network error", method, path),
			Err:     err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerTransport,
				Message: "failed to read response body",
				Err:     err,
			}
		}
		return respBody, nil
	}

	return nil, classifyHTTPError(resp, method, path)
}

// doNoBody executes an HTTP request without a request body and
// discards the response body. Accepts 200 and 204 as success. Used
// for label removal (DELETE).
func (c *githubClient) doNoBody(ctx context.Context, method, path string) error { //nolint:unparam // DELETE is the only caller today; method kept for interface consistency
	reqURL := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("failed to build request: %s %s", method, path),
			Err:     err,
		}
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: network error", method, path),
			Err:     err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	return classifyHTTPError(resp, method, path)
}

// classifyHTTPError maps a non-success HTTP response to a
// [*domain.TrackerError] with GitHub-specific 403 disambiguation.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	detail := string(snippet)

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnauthorized:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: bad credentials", method, path),
		}

	case resp.StatusCode == http.StatusForbidden:
		// Three-tier 403 disambiguation per spec Section 3.5.
		if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: fmt.Sprintf("%s %s: rate limited (primary)", method, path),
			}
		}
		if strings.Contains(strings.ToLower(detail), "rate limit") {
			msg := fmt.Sprintf("%s %s: rate limited (secondary)", method, path)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				msg += fmt.Sprintf(" (retry after %s seconds)", ra)
			}
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: msg,
			}
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: insufficient permissions", method, path),
		}

	case resp.StatusCode == http.StatusNotFound:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found", method, path),
		}

	case resp.StatusCode == http.StatusGone:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: gone (410): %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnprocessableEntity:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: validation failed: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		msg := fmt.Sprintf("%s %s: rate limited", method, path)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			msg += fmt.Sprintf(" (retry after %s seconds)", ra)
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: msg,
		}

	case resp.StatusCode >= 500:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: server error %d: %s", method, path, resp.StatusCode, detail),
		}

	default:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, detail),
		}
	}
}

// parseLinkNext extracts the URL with rel="next" from the Link header
// value. Returns an empty string when absent or malformed.
func parseLinkNext(header string) string {
	if header == "" {
		return ""
	}

	for _, segment := range strings.Split(header, ",") {
		parts := strings.Split(segment, ";")
		if len(parts) < 2 {
			continue
		}

		hasNext := false
		for _, attr := range parts[1:] {
			attr = strings.TrimSpace(attr)
			if strings.EqualFold(attr, `rel="next"`) {
				hasNext = true
				break
			}
		}
		if !hasNext {
			continue
		}

		urlPart := strings.TrimSpace(parts[0])
		if start := strings.Index(urlPart, "<"); start != -1 {
			if end := strings.Index(urlPart[start:], ">"); end != -1 {
				return urlPart[start+1 : start+end]
			}
		}
	}

	return ""
}
