package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

// sendError is a classified notifier failure. It carries a category
// (timeout, connection failure, an HTTP status class) and deliberately
// omits the endpoint URL, the request body, and the response body so
// the secret-bearing URL never reaches a log or an agent-visible result.
type sendError struct {
	Category string
	Err      error
}

// Error returns the category only.
func (e *sendError) Error() string { return e.Category }

// Unwrap exposes the underlying transport error for [errors.Is] and
// [errors.As] without surfacing its text into the category.
func (e *sendError) Unwrap() error { return e.Err }

// classifyHTTPError maps a non-2xx response to a [sendError] whose
// category is the HTTP status class. The response body is drained and
// discarded, never read into the error.
func classifyHTTPError(resp *http.Response, _, _ string) error {
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &sendError{Category: fmt.Sprintf("unauthorized (HTTP %d)", resp.StatusCode)}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &sendError{Category: "rate limited (HTTP 429)"}
	case resp.StatusCode >= 500:
		return &sendError{Category: fmt.Sprintf("server error (HTTP %d)", resp.StatusCode)}
	default:
		return &sendError{Category: fmt.Sprintf("unexpected response (HTTP %d)", resp.StatusCode)}
	}
}

// classifyTransportError maps a request, network, or body-read failure
// to a [sendError] with a transport category. The underlying error is
// wrapped for chain inspection but is never rendered into the category,
// so the host and URL it embeds stay out of logs and agent-visible
// results.
func classifyTransportError(err error, _, _ string) error {
	return &sendError{Category: transportCategory(err), Err: err}
}

// transportCategory derives a redaction-safe category from a transport
// error without rendering the error text, which embeds the host or URL.
func transportCategory(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "connection failure"
}
