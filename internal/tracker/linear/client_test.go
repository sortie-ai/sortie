package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestLinearClientExecute(t *testing.T) {
	t.Parallel()

	t.Run("sends verbatim auth header and returns body with headers", func(t *testing.T) {
		t.Parallel()

		var gotAuth, gotContentType, gotUserAgent string
		var gotBody graphQLRequestBody

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotContentType = r.Header.Get("Content-Type")
			gotUserAgent = r.Header.Get("User-Agent")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)

			w.Header().Set("x-complexity", "95")
			w.Header().Set("x-ratelimit-requests-remaining", "2499")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"u"}}}`)
		}))
		defer srv.Close()

		client := newLinearClient(srv.URL, "lin_api_secret_token", "sortie/test")

		body, headers, err := client.Execute(context.Background(), "query Me { viewer { id } }", map[string]any{"k": "v"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}

		if gotAuth != "lin_api_secret_token" {
			t.Errorf("Authorization = %q, want verbatim key with no Bearer prefix", gotAuth)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
		}
		if gotUserAgent != "sortie/test" {
			t.Errorf("User-Agent = %q, want %q", gotUserAgent, "sortie/test")
		}
		if gotBody.Query != "query Me { viewer { id } }" {
			t.Errorf("request query = %q, want the verbatim document", gotBody.Query)
		}
		if gotBody.Variables["k"] != "v" {
			t.Errorf("request variables = %v, want k=v", gotBody.Variables)
		}
		if string(body) != `{"data":{"viewer":{"id":"u"}}}` {
			t.Errorf("response body = %q, want the server payload", body)
		}
		if got := headers.Get("x-complexity"); got != "95" {
			t.Errorf("returned header x-complexity = %q, want %q", got, "95")
		}
		if got := headers.Get("x-ratelimit-requests-remaining"); got != "2499" {
			t.Errorf("returned header x-ratelimit-requests-remaining = %q, want %q", got, "2499")
		}
	})

	t.Run("non-2xx classification is body-first", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			status int
			body   string
			want   domain.TrackerErrorKind
		}{
			{
				name:   "body not-found error wins over 400 status",
				status: http.StatusBadRequest,
				body:   `{"errors":[{"message":"Entity not found: Issue","extensions":{"type":"invalid input","code":"INPUT_ERROR"}}],"data":null}`,
				want:   domain.ErrTrackerNotFound,
			},
			{
				name:   "ratelimited body on 400 is retryable api",
				status: http.StatusBadRequest,
				body:   `{"errors":[{"message":"rate limited","extensions":{"type":"ratelimited","code":"RATELIMITED"}}]}`,
				want:   domain.ErrTrackerAPI,
			},
			{
				name:   "auth body wins on 401",
				status: http.StatusUnauthorized,
				body:   `{"errors":[{"message":"Authentication required","extensions":{"type":"authentication error","code":"AUTHENTICATION_ERROR"}}]}`,
				want:   domain.ErrTrackerAuth,
			},
			{
				name:   "status fallback when no errors array on 401",
				status: http.StatusUnauthorized,
				body:   `{"message":"unauthorized","data":null}`,
				want:   domain.ErrTrackerAuth,
			},
			{
				name:   "status fallback to transport on 503",
				status: http.StatusServiceUnavailable,
				body:   `gateway down`,
				want:   domain.ErrTrackerTransport,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
				}))
				defer srv.Close()

				client := newLinearClient(srv.URL, "key", "sortie/test")

				_, _, err := client.Execute(context.Background(), "query Me { viewer { id } }", nil)

				assertTrackerErrorKind(t, err, tt.want)
			})
		}
	})

	t.Run("transport failure maps to transport error", func(t *testing.T) {
		t.Parallel()

		client := newLinearClient("http://127.0.0.1:0", "key", "sortie/test")

		_, _, err := client.Execute(context.Background(), "query Me { viewer { id } }", nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
	})

	t.Run("cancelled context is honored", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := newLinearClient(srv.URL, "key", "sortie/test")

		_, _, err := client.Execute(ctx, "query Me { viewer { id } }", nil)
		if err == nil {
			t.Fatal("Execute with cancelled context = nil, want error")
		}
	})
}

func TestAPIKeyNeverLogged(t *testing.T) {
	t.Parallel()

	const secret = "lin_api_super_secret_value_42"

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("x-ratelimit-requests-remaining", "0")
		w.Header().Set("x-ratelimit-requests-reset", "1781361577787")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer srv.Close()

	var buf strings.Builder
	log := newTextLogger(&buf)

	client := newLinearClient(srv.URL, secret, "sortie/test")
	body, headers, err := client.Execute(context.Background(), queryCandidateIssues, map[string]any{
		"teamKey": "SOR", "states": []string{"Todo"}, "first": topLevelPageSize,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	recordRateLimit(headers, log)
	if _, _, decErr := decodeIssuesPage(body); decErr != nil {
		t.Fatalf("decodeIssuesPage: %v", decErr)
	}

	if gotAuth != secret {
		t.Fatalf("Authorization = %q, want the verbatim key (test precondition)", gotAuth)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("captured log output contains the API key\noutput: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected the rate-limit WARN to have fired (otherwise the assertion is vacuous)\noutput: %s", buf.String())
	}
}
