package linear

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func ext(typ, code, upm string, userErr bool) graphQLErrorExtensions {
	return graphQLErrorExtensions{Type: typ, Code: code, UserPresentableMessage: upm, UserError: userErr}
}

func TestClassifyGraphQLErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		errs []graphQLError
		want domain.TrackerErrorKind
	}{
		{
			name: "empty errors returns nil",
			errs: nil,
		},
		{
			name: "not-found by message checked before type",
			errs: []graphQLError{{Message: "Entity not found: Issue", Extensions: ext("invalid input", "INPUT_ERROR", "Could not find referenced Issue.", true)}},
			want: domain.ErrTrackerNotFound,
		},
		{
			name: "not-found is case-insensitive",
			errs: []graphQLError{{Message: "ENTITY NOT FOUND: Issue", Extensions: ext("invalid input", "INPUT_ERROR", "", true)}},
			want: domain.ErrTrackerNotFound,
		},
		{
			name: "ratelimited by code",
			errs: []graphQLError{{Message: "rate limited", Extensions: ext("invalid input", "RATELIMITED", "", false)}},
			want: domain.ErrTrackerAPI,
		},
		{
			name: "ratelimited by type",
			errs: []graphQLError{{Message: "slow down", Extensions: ext("ratelimited", "SOMETHING_ELSE", "", false)}},
			want: domain.ErrTrackerAPI,
		},
		{
			name: "authentication error maps to auth",
			errs: []graphQLError{{Message: "auth", Extensions: ext("authentication error", "AUTHENTICATION_ERROR", "", false)}},
			want: domain.ErrTrackerAuth,
		},
		{
			name: "forbidden maps to auth",
			errs: []graphQLError{{Message: "no", Extensions: ext("forbidden", "FORBIDDEN", "", false)}},
			want: domain.ErrTrackerAuth,
		},
		{
			name: "feature not accessible maps to auth",
			errs: []graphQLError{{Message: "plan limit", Extensions: ext("feature not accessible", "FEATURE_NOT_ACCESSIBLE", "", false)}},
			want: domain.ErrTrackerAuth,
		},
		{
			name: "invalid input maps to payload",
			errs: []graphQLError{{Message: "bad", Extensions: ext("invalid input", "INVALID_INPUT", "", false)}},
			want: domain.ErrTrackerPayload,
		},
		{
			name: "user error maps to payload",
			errs: []graphQLError{{Message: "bad", Extensions: ext("user error", "USER_ERROR", "", false)}},
			want: domain.ErrTrackerPayload,
		},
		{
			name: "graphql error maps to payload",
			errs: []graphQLError{{Message: "bad", Extensions: ext("graphql error", "GRAPHQL_ERROR", "", false)}},
			want: domain.ErrTrackerPayload,
		},
		{
			name: "userError flag without known type maps to payload",
			errs: []graphQLError{{Message: "bad", Extensions: ext("", "", "", true)}},
			want: domain.ErrTrackerPayload,
		},
		{
			name: "internal error maps to transport",
			errs: []graphQLError{{Message: "boom", Extensions: ext("internal error", "INTERNAL", "", false)}},
			want: domain.ErrTrackerTransport,
		},
		{
			name: "lock timeout maps to transport",
			errs: []graphQLError{{Message: "boom", Extensions: ext("lock timeout", "LOCK", "", false)}},
			want: domain.ErrTrackerTransport,
		},
		{
			name: "unknown type maps to api",
			errs: []graphQLError{{Message: "weird", Extensions: ext("usage limit exceeded", "USAGE", "", false)}},
			want: domain.ErrTrackerAPI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := classifyGraphQLErrors(tt.errs)

			if tt.want == "" {
				if err != nil {
					t.Errorf("classifyGraphQLErrors(empty) = %v, want nil", err)
				}
				return
			}
			assertTrackerErrorKind(t, err, tt.want)
		})
	}
}

func TestClassifyGraphQLErrorsMessage(t *testing.T) {
	t.Parallel()

	t.Run("prefers userPresentableMessage", func(t *testing.T) {
		t.Parallel()

		errs := []graphQLError{{Message: "raw message", Extensions: ext("invalid input", "INVALID_INPUT", "Operator-friendly text.", false)}}

		err := classifyGraphQLErrors(errs)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Message != "Operator-friendly text." {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "Operator-friendly text.")
		}
	})

	t.Run("falls back to message", func(t *testing.T) {
		t.Parallel()

		errs := []graphQLError{{Message: "raw message", Extensions: ext("invalid input", "INVALID_INPUT", "", false)}}

		err := classifyGraphQLErrors(errs)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Message != "raw message" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "raw message")
		}
	})
}

func TestClassifyTransport(t *testing.T) {
	t.Parallel()

	underlying := errors.New("dial tcp: connection refused")

	err := classifyTransport(underlying, http.MethodPost, "")

	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
	if !errors.Is(err, underlying) {
		t.Errorf("classifyTransport error chain does not wrap the underlying error: %v", err)
	}
}

func TestClassifyHTTPStatusFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   domain.TrackerErrorKind
	}{
		{"400 maps to payload", http.StatusBadRequest, domain.ErrTrackerPayload},
		{"401 maps to auth", http.StatusUnauthorized, domain.ErrTrackerAuth},
		{"403 maps to auth", http.StatusForbidden, domain.ErrTrackerAuth},
		{"429 maps to api", http.StatusTooManyRequests, domain.ErrTrackerAPI},
		{"500 maps to transport", http.StatusInternalServerError, domain.ErrTrackerTransport},
		{"503 maps to transport", http.StatusServiceUnavailable, domain.ErrTrackerTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := classifyHTTPStatus(tt.status, "linear graphql: unexpected status")

			assertTrackerErrorKind(t, err, tt.want)
		})
	}
}

func TestRecordRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("converts epoch milliseconds to seconds and warns when exhausted", func(t *testing.T) {
		t.Parallel()

		var buf strings.Builder
		log := newTextLogger(&buf)

		headers := http.Header{}
		headers.Set("x-ratelimit-requests-remaining", "0")
		headers.Set("x-ratelimit-requests-reset", "1781361577787")
		headers.Set("Retry-After", "30")

		recordRateLimit(headers, log)

		output := buf.String()
		if !strings.Contains(output, "level=WARN") {
			t.Errorf("expected WARN when exhausted\noutput: %s", output)
		}
		if !strings.Contains(output, "requests_reset_unix=1781361577") {
			t.Errorf("expected epoch-seconds conversion 1781361577 (ms/1000)\noutput: %s", output)
		}
		if !strings.Contains(output, "retry_after=30") {
			t.Errorf("expected retry_after attribute\noutput: %s", output)
		}
	})

	t.Run("no warn when requests remain", func(t *testing.T) {
		t.Parallel()

		var buf strings.Builder
		log := newTextLogger(&buf)

		headers := http.Header{}
		headers.Set("x-ratelimit-requests-remaining", "1500")
		headers.Set("x-ratelimit-requests-reset", "1781361577787")

		recordRateLimit(headers, log)

		if out := buf.String(); strings.Contains(out, "level=WARN") {
			t.Errorf("did not expect WARN when requests remain\noutput: %s", out)
		}
	})

	t.Run("no warn when remaining header absent", func(t *testing.T) {
		t.Parallel()

		var buf strings.Builder
		log := newTextLogger(&buf)

		recordRateLimit(http.Header{}, log)

		if out := buf.String(); strings.Contains(out, "level=WARN") {
			t.Errorf("did not expect WARN with no rate-limit headers\noutput: %s", out)
		}
	})

	t.Run("nil logger is a no-op", func(t *testing.T) {
		t.Parallel()

		headers := http.Header{}
		headers.Set("x-ratelimit-requests-remaining", "0")

		recordRateLimit(headers, nil)
	})
}
