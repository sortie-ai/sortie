package github

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

func newGitHubClient(baseURL, token, userAgent string) *httpkit.Client {
	trimmedBaseURL := strings.TrimRight(baseURL, "/")
	authorization := "Bearer " + token

	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: trimmedBaseURL,
		Timeout: 30 * time.Second,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", authorization)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransportError,
	})
}

const maxErrorBody = 512

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
		if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: fmt.Sprintf("%s %s: rate limited (primary)", method, path),
			}
		}
		if strings.Contains(strings.ToLower(detail), "rate limit") {
			message := fmt.Sprintf("%s %s: rate limited (secondary)", method, path)
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				message += fmt.Sprintf(" (retry after %s seconds)", retryAfter)
			}
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: message,
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
		message := fmt.Sprintf("%s %s: rate limited", method, path)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			message += fmt.Sprintf(" (retry after %s seconds)", retryAfter)
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: message,
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
