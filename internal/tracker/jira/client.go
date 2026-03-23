package jira

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// jiraClient wraps net/http.Client with Jira-specific authentication,
// request building, and HTTP status to TrackerError mapping.
type jiraClient struct {
	httpClient *http.Client
	baseURL    string
	authHeader string
}

// newJiraClient constructs a jiraClient with Basic authentication
// derived from the email and API token. The baseURL is stripped of
// any trailing slash.
func newJiraClient(baseURL, email, token string) *jiraClient {
	return &jiraClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
	}
}

// maxErrorBody is the maximum number of bytes read from non-200
// response bodies for diagnostic error messages.
const maxErrorBody = 512

// classifyHTTPError maps a non-success HTTP response to a
// [*domain.TrackerError]. The method and path are included in the
// error message for diagnostics. The response body is read up to
// [maxErrorBody] bytes for the error detail.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	detail := string(snippet)

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		msg := fmt.Sprintf("%s %s: %d", method, path, resp.StatusCode)
		if resp.Header.Get("X-Seraph-LoginReason") == "AUTHENTICATION_DENIED" {
			msg += " (CAPTCHA challenge triggered — log in via browser to resolve)"
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: msg,
		}

	case resp.StatusCode == http.StatusNotFound:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found", method, path),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := resp.Header.Get("Retry-After")
		msg := fmt.Sprintf("%s %s: rate limited", method, path)
		if retryAfter != "" {
			msg += fmt.Sprintf(" (retry after %s seconds)", retryAfter)
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

// do executes an HTTP request against the Jira REST API and returns
// the response body on success. Non-200 responses are mapped to
// [domain.TrackerError] by status code. Context cancellation is
// propagated directly without wrapping in TrackerError.
func (c *jiraClient) do(ctx context.Context, method, path string, params url.Values) ([]byte, error) { //nolint:unparam // method will accept POST for future comment/transition operations
	reqURL := c.baseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
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

	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
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

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerTransport,
				Message: "failed to read response body",
				Err:     err,
			}
		}
		return body, nil
	}

	return nil, classifyHTTPError(resp, method, path)
}

// doJSON executes an HTTP request with a JSON request body against the
// Jira REST API. Successful responses (200-299) return the response
// body (which may be empty for 204 No Content). Non-success responses
// are mapped to [domain.TrackerError] by status code, using the same
// classification as [jiraClient.do].
func (c *jiraClient) doJSON(ctx context.Context, method, path string, body io.Reader) ([]byte, error) { //nolint:unparam // method is POST today but the signature mirrors do for consistency
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

	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
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
