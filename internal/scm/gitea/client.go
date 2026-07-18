package gitea

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// newGiteaClient builds an [httpkit.Client] for the Gitea REST API. baseURL is
// the instance base URL already suffixed with /api/v1; the token travels only in
// the Authorization header, never as a query parameter.
func newGiteaClient(baseURL, token, userAgent string) *httpkit.Client {
	trimmedBaseURL := strings.TrimRight(baseURL, "/")
	authorization := "token " + token

	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: trimmedBaseURL,
		Timeout: 30 * time.Second,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", authorization)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransportError,
	})
}

const maxErrorBody = 512

// classifyHTTPError maps a non-200 Gitea response to a [*domain.TrackerError].
// The message carries a bounded body snippet and the request path or URL, never
// the token, which this adapter sends only in the Authorization header.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	detail := errorDetail(snippet)

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnauthorized:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: invalid credentials", method, path),
		}

	case resp.StatusCode == http.StatusForbidden:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: insufficient permissions: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusNotFound:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found", method, path),
		}

	case resp.StatusCode == http.StatusMethodNotAllowed:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: method not allowed: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusConflict:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: conflict: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusPreconditionFailed:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: precondition failed: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusUnprocessableEntity:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: validation failed: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusLocked:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: locked: %s", method, path, detail),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		// Gitea itself sends no 429; only a fronting proxy does. Surface the
		// proxy's Retry-After but let the orchestrator own retry backoff.
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			slog.Default().Warn("rate limited", slog.String("retry_after", retryAfter))
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: rate limited", method, path),
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

// classifyTransportError maps a transport or body-read failure to a
// [*domain.TrackerError] of kind [domain.ErrTrackerTransport], preserving the
// underlying error for the chain.
func classifyTransportError(err error, method, path string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerTransport,
		Message: fmt.Sprintf("%s %s: transport error", method, path),
		Err:     err,
	}
}

// errorDetail extracts the Gitea error envelope message from a bounded body
// snippet, falling back to the raw snippet when it does not decode.
func errorDetail(snippet []byte) string {
	var body giteaErrorBody
	if err := json.Unmarshal(snippet, &body); err == nil && body.Message != "" {
		return body.Message
	}
	return string(snippet)
}
