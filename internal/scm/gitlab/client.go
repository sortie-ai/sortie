package gitlab

import (
	"bytes"
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

// newGitLabClient builds an [httpkit.Client] for the GitLab REST API v4.
// baseURL is the instance base URL already suffixed with /api/v4; the
// token travels only in the PRIVATE-TOKEN header, never as a query
// parameter.
func newGitLabClient(baseURL, token, userAgent string) *httpkit.Client {
	trimmedBaseURL := strings.TrimRight(baseURL, "/")

	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: trimmedBaseURL,
		Timeout: 30 * time.Second,
		Authorize: func(req *http.Request) {
			req.Header.Set("PRIVATE-TOKEN", token)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: httpkit.ClassifyTransport,
	})
}

const maxErrorBody = 512

// classifyHTTPError maps a non-2xx GitLab response to a
// [*domain.TrackerError]. The message carries a bounded body snippet and
// the request method and path, never the token, which this adapter sends
// only in the PRIVATE-TOKEN header.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	detail := errorDetail(snippet)

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusUnauthorized:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: invalid credentials: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusForbidden:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: insufficient permissions: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusNotFound:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusConflict:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: conflict: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusRequestURITooLong:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: request uri too large: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusUnprocessableEntity:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: unprocessable entity: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		// GitLab does not sleep on 429 itself; the orchestrator owns retry
		// backoff through TrackerErrorKind.RetryClassification.
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			slog.Default().Warn("rate limited", slog.String("retry_after", retryAfter))
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: rate limited: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode >= 500:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: server error %d: %s", method, path, resp.StatusCode, detail),
			Status:  resp.StatusCode,
		}

	default:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, detail),
			Status:  resp.StatusCode,
		}
	}
}

// errorDetail extracts a diagnostic message from a bounded GitLab
// error-body snippet, covering all four observed envelope shapes: a
// string message, a non-string message, a bare error, and an error paired
// with an error_description. It falls back to the raw snippet when the
// body does not decode as JSON at all, because a 429 or 502 body may not
// be JSON.
func errorDetail(snippet []byte) string {
	var body gitlabErrorBody
	if err := json.Unmarshal(snippet, &body); err != nil {
		return string(snippet)
	}

	base := ""
	if len(body.Message) > 0 {
		var text string
		if err := json.Unmarshal(body.Message, &text); err == nil && text != "" {
			base = text
		} else {
			base = compactJSON(body.Message)
		}
	}
	if base == "" && body.Error != "" {
		base = body.Error
	}
	if body.ErrorDescription != "" {
		if base == "" {
			base = body.ErrorDescription
		} else {
			base += ": " + body.ErrorDescription
		}
	}
	if base == "" {
		base = string(snippet)
	}
	return base
}

// compactJSON returns raw with insignificant whitespace removed, or the
// raw bytes unchanged when they are not syntactically valid JSON.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
