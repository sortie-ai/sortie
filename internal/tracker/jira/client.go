package jira

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// newJiraClient constructs the shared Jira transport.
func newJiraClient(baseURL, email, token, userAgent string) *httpkit.Client {
	trimmedBaseURL := strings.TrimRight(baseURL, "/")
	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))

	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: trimmedBaseURL,
		Timeout: 30 * time.Second,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", authHeader)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransportError,
	})
}

// maxErrorBody is the maximum number of bytes read from non-200
// response bodies for diagnostic error messages.
const maxErrorBody = 512

// classifyHTTPError maps a non-success HTTP response to a
// [domain.TrackerError]. The method and path are included in the
// error message for diagnostics. The response body is read up to
// [maxErrorBody] bytes for the error detail.
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

func classifyTransportError(err error, method, path string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerTransport,
		Message: fmt.Sprintf("%s %s: transport error", method, path),
		Err:     err,
	}
}
