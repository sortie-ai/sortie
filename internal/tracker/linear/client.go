package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// defaultTimeout bounds every GraphQL request.
const defaultTimeout = 30 * time.Second

// maxErrorBody is the maximum number of body bytes read from a non-200
// response when extracting errors for classification.
const maxErrorBody = 4096

// graphQLClient executes a single GraphQL operation and returns the raw
// response body and headers.
//
// Transport and HTTP-status failures are returned as errors; body-level error
// classification happens in the caller after decoding. Implementations must be
// safe for concurrent use.
type graphQLClient interface {
	Execute(ctx context.Context, query string, variables map[string]any) (body []byte, headers http.Header, err error)
}

// graphQLRequestBody is the wire shape of a Linear GraphQL POST body.
type graphQLRequestBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// linearClient is the production [graphQLClient] backed by an [httpkit.Client].
type linearClient struct {
	client *httpkit.Client
}

// newLinearClient constructs a [linearClient] for the given endpoint. The
// endpoint is the full GraphQL URL; the API key is sent verbatim in the
// Authorization header with no Bearer prefix.
func newLinearClient(endpoint, apiKey, userAgent string) *linearClient {
	client := httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: endpoint,
		Timeout: defaultTimeout,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", apiKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyResponseBody,
		ClassifyTransport: classifyTransport,
	})
	return &linearClient{client: client}
}

// Execute marshals the operation and posts it to the configured endpoint,
// returning the raw response body and headers. The path argument is empty
// because the endpoint carries the full URL.
func (c *linearClient) Execute(ctx context.Context, query string, variables map[string]any) ([]byte, http.Header, error) {
	raw, err := json.Marshal(graphQLRequestBody{Query: query, Variables: variables})
	if err != nil {
		return nil, nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to encode graphql request body",
			Err:     err,
		}
	}

	return c.client.SendWithHeaders(ctx, http.MethodPost, "", bytes.NewReader(raw))
}

// classifyResponseBody maps a non-2xx HTTP response to a
// [*domain.TrackerError]. Because Linear reports application errors inside
// response bodies on every status, it parses the body's errors array first and
// only falls back to the HTTP status when no errors array is present.
func classifyResponseBody(resp *http.Response, _, _ string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)

	var envelope struct {
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal(snippet, &envelope); err == nil && len(envelope.Errors) > 0 {
		return classifyGraphQLErrors(envelope.Errors)
	}

	return classifyHTTPStatus(resp.StatusCode, fmt.Sprintf("linear graphql: unexpected status %d", resp.StatusCode))
}

// classifyTransport maps a transport or body-read failure to a
// [*domain.TrackerError] of kind [domain.ErrTrackerTransport].
func classifyTransport(err error, _, _ string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerTransport,
		Message: "linear graphql: transport error",
		Err:     err,
	}
}

// rateLimitResetDivisor converts Linear's epoch-millisecond reset headers to
// epoch seconds for comparison against a Go time value.
const rateLimitResetDivisor = 1000

// recordRateLimit reads the rate-limit and complexity headers for logging. It
// emits a WARN naming the request reset time only when the window is exhausted.
// It never logs the API key.
func recordRateLimit(headers http.Header, log *slog.Logger) {
	if log == nil || headers == nil {
		return
	}

	remaining, ok := parseHeaderInt(headers.Get("x-ratelimit-requests-remaining"))
	if !ok || remaining > 0 {
		return
	}

	attrs := []slog.Attr{slog.Int("requests_remaining", remaining)}
	if resetSec, ok := parseResetSeconds(headers.Get("x-ratelimit-requests-reset")); ok {
		attrs = append(attrs, slog.Int64("requests_reset_unix", resetSec))
	}
	if retryAfter := headers.Get("Retry-After"); retryAfter != "" {
		attrs = append(attrs, slog.String("retry_after", retryAfter))
	}
	log.LogAttrs(context.Background(), slog.LevelWarn, "rate limit exhausted", attrs...)
}

// parseHeaderInt parses a header value as an integer.
func parseHeaderInt(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseResetSeconds parses an epoch-millisecond reset header and returns the
// equivalent epoch seconds.
func parseResetSeconds(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return ms / rateLimitResetDivisor, true
}
