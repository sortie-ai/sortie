// Package httpkit provides shared REST transport helpers for integration adapters.
//
// Start with [NewClient] for authenticated requests and [Paginator] for
// multi-page collection endpoints.
package httpkit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Authorizer mutates an outgoing request before dispatch.
type Authorizer func(*http.Request)

// ErrorClassifier maps a non-success HTTP response to a caller-defined error.
type ErrorClassifier func(resp *http.Response, method, path string) error

// TransportClassifier maps a transport or body-read failure to a caller-defined error.
type TransportClassifier func(err error, method, path string) error

// ClientOptions configures a [Client].
type ClientOptions struct {
	// BaseURL is prepended to relative request paths.
	BaseURL string

	// Timeout controls the underlying [http.Client] timeout.
	Timeout time.Duration

	// Authorize applies authentication and shared headers to each request.
	Authorize Authorizer

	// ClassifyError maps non-success HTTP responses to caller-defined errors.
	ClassifyError ErrorClassifier

	// ClassifyTransport maps request, network, and body-read failures.
	ClassifyTransport TransportClassifier
}

// Client executes HTTP requests with caller-provided authorization and error mapping.
type Client struct {
	httpClient        *http.Client
	baseURL           string
	authorize         Authorizer
	classifyError     ErrorClassifier
	classifyTransport TransportClassifier
}

// NewClient constructs a [Client] from the provided options.
func NewClient(opts ClientOptions) *Client {
	return &Client{
		httpClient:        newHTTPClient(opts.Timeout),
		baseURL:           opts.BaseURL,
		authorize:         opts.Authorize,
		classifyError:     opts.ClassifyError,
		classifyTransport: opts.ClassifyTransport,
	}
}

func newHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		client.Transport = defaultTransport.Clone()
	}
	return client
}

// Get issues a GET request against BaseURL plus path and returns the body and headers on HTTP 200.
func (c *Client) Get(ctx context.Context, path string, params url.Values) ([]byte, http.Header, error) {
	reqURL := c.buildURL(path, params)
	resp, err := c.do(ctx, http.MethodGet, path, reqURL, nil, "", "")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode != http.StatusOK {
		return nil, nil, c.classifyResponse(resp, http.MethodGet, path)
	}

	body, err := c.readBody(ctx, resp.Body, http.MethodGet, path)
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Header.Clone(), nil
}

// GetURL issues a GET request against a fully qualified URL and returns the body and headers on HTTP 200.
func (c *Client) GetURL(ctx context.Context, fullURL string) ([]byte, http.Header, error) {
	resp, err := c.do(ctx, http.MethodGet, fullURL, fullURL, nil, "", "")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode != http.StatusOK {
		return nil, nil, c.classifyResponse(resp, http.MethodGet, fullURL)
	}

	body, err := c.readBody(ctx, resp.Body, http.MethodGet, fullURL)
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Header.Clone(), nil
}

// GetConditional issues a conditional GET request and reports whether the response was not modified.
func (c *Client) GetConditional(ctx context.Context, path, ifNoneMatch string, params url.Values) (body []byte, etag string, notModified bool, err error) {
	reqURL := c.buildURL(path, params)
	resp, err := c.do(ctx, http.MethodGet, path, reqURL, nil, "", ifNoneMatch)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, "", true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, c.classifyResponse(resp, http.MethodGet, path)
	}

	body, err = c.readBody(ctx, resp.Body, http.MethodGet, path)
	if err != nil {
		return nil, "", false, err
	}
	return body, resp.Header.Get("ETag"), false, nil
}

// GetRaw issues a GET request and returns up to maxBytes from the response body on HTTP 200.
func (c *Client) GetRaw(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}

	reqURL := c.baseURL + path
	resp, err := c.do(ctx, http.MethodGet, path, reqURL, nil, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode != http.StatusOK {
		return nil, c.classifyResponse(resp, http.MethodGet, path)
	}

	body, err := c.readBody(ctx, io.LimitReader(resp.Body, maxBytes), http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Send issues a request with a JSON body and returns the response body on any HTTP 2xx status.
func (c *Client) Send(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	reqURL := c.baseURL + path
	resp, err := c.do(ctx, method, path, reqURL, body, "application/json", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, c.classifyResponse(resp, method, path)
	}

	respBody, err := c.readBody(ctx, resp.Body, method, path)
	if err != nil {
		return nil, err
	}
	return respBody, nil
}

// SendNoBody issues a request without a body and succeeds only on HTTP 200 or 204.
func (c *Client) SendNoBody(ctx context.Context, method, path string) error {
	reqURL := c.baseURL + path
	resp, err := c.do(ctx, method, path, reqURL, nil, "", "")
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup on response body

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return c.classifyResponse(resp, method, path)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) do(ctx context.Context, method, path, reqURL string, body io.Reader, contentType, ifNoneMatch string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, c.classifyTransportFailure(ctx, err, method, path)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if c.authorize != nil {
		c.authorize(req)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.classifyTransportFailure(ctx, err, method, path)
	}
	return resp, nil
}

func (c *Client) buildURL(path string, params url.Values) string {
	reqURL := c.baseURL + path
	if len(params) == 0 {
		return reqURL
	}

	query := params.Encode()
	if query == "" {
		return reqURL
	}

	separator := "?"
	if strings.Contains(reqURL, "?") {
		separator = "&"
	}
	return reqURL + separator + query
}

func (c *Client) classifyResponse(resp *http.Response, method, path string) error {
	if c.classifyError != nil {
		return c.classifyError(resp, method, path)
	}
	return fmt.Errorf("%s %s: unexpected status %d", method, path, resp.StatusCode)
}

func (c *Client) classifyTransportFailure(ctx context.Context, err error, method, path string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.classifyTransport != nil {
		return c.classifyTransport(err, method, path)
	}
	return err
}

func (c *Client) readBody(ctx context.Context, body io.Reader, method, path string) ([]byte, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, c.classifyTransportFailure(ctx, err, method, path)
	}
	return data, nil
}
