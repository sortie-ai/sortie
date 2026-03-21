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

	// Read a bounded portion of the response body for diagnostics.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	detail := string(snippet)

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		msg := fmt.Sprintf("%s %s: %d", method, path, resp.StatusCode)
		if resp.Header.Get("X-Seraph-LoginReason") == "AUTHENTICATION_DENIED" {
			msg += " (CAPTCHA challenge triggered — log in via browser to resolve)"
		}
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: msg,
		}

	case resp.StatusCode == http.StatusNotFound:
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found", method, path),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := resp.Header.Get("Retry-After")
		msg := fmt.Sprintf("%s %s: rate limited", method, path)
		if retryAfter != "" {
			msg += fmt.Sprintf(" (retry after %s seconds)", retryAfter)
		}
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: msg,
		}

	case resp.StatusCode >= 500:
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: server error %d: %s", method, path, resp.StatusCode, detail),
		}

	default:
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, detail),
		}
	}
}
