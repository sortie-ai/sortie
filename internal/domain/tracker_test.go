package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Compile-time interface satisfaction check.
var _ TrackerAdapter = (*mockTrackerAdapter)(nil)

type mockTrackerAdapter struct{}

func (m *mockTrackerAdapter) FetchCandidateIssues(_ context.Context) ([]Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueByID(_ context.Context, _ string) (Issue, error) {
	return Issue{}, nil
}

func (m *mockTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]Comment, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockTrackerAdapter) CommentIssue(_ context.Context, _ string, _ string) error {
	return nil
}

func TestTrackerErrorKind_Values(t *testing.T) {
	t.Parallel()

	tests := []struct {
		constant TrackerErrorKind
		want     string
	}{
		{ErrUnsupportedTrackerKind, "unsupported_tracker_kind"},
		{ErrMissingTrackerAPIKey, "missing_tracker_api_key"},
		{ErrMissingTrackerProject, "missing_tracker_project"},
		{ErrTrackerTransport, "tracker_transport_error"},
		{ErrTrackerAuth, "tracker_auth_error"},
		{ErrTrackerAPI, "tracker_api_error"},
		{ErrTrackerNotFound, "tracker_not_found"},
		{ErrTrackerPayload, "tracker_payload_error"},
		{ErrTrackerMissingCursor, "tracker_missing_end_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if string(tt.constant) != tt.want {
				t.Errorf("TrackerErrorKind constant = %q, want %q", tt.constant, tt.want)
			}
		})
	}
	if len(tests) != 9 {
		t.Errorf("expected 9 tracker error kinds, got %d", len(tests))
	}
}

func TestTrackerError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  TrackerError
		want string
	}{
		{
			name: "without wrapped error",
			err:  TrackerError{Kind: ErrTrackerTransport, Message: "connection refused"},
			want: "tracker: tracker_transport_error: connection refused",
		},
		{
			name: "with wrapped error",
			err:  TrackerError{Kind: ErrTrackerTransport, Message: "connection failed", Err: fmt.Errorf("dial tcp: connect refused")},
			want: "tracker: tracker_transport_error: connection failed: dial tcp: connect refused",
		},
		{
			name: "auth error",
			err:  TrackerError{Kind: ErrTrackerAuth, Message: "invalid token"},
			want: "tracker: tracker_auth_error: invalid token",
		},
		{
			name: "api error",
			err:  TrackerError{Kind: ErrTrackerAPI, Message: "status 500"},
			want: "tracker: tracker_api_error: status 500",
		},
		{
			name: "payload error",
			err:  TrackerError{Kind: ErrTrackerPayload, Message: "unexpected field"},
			want: "tracker: tracker_payload_error: unexpected field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrackerError_Unwrap(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("underlying error")
	trackerErr := &TrackerError{
		Kind:    ErrTrackerAuth,
		Message: "invalid token",
		Err:     inner,
	}

	if trackerErr.Unwrap() != inner {
		t.Errorf("Unwrap() = %v, want %v", trackerErr.Unwrap(), inner)
	}

	// Verify errors.As works through a wrapping chain.
	wrapped := fmt.Errorf("outer: %w", trackerErr)
	var extracted *TrackerError
	if !errors.As(wrapped, &extracted) {
		t.Fatal("errors.As failed to extract *TrackerError from wrapped chain")
	}
	if extracted.Kind != ErrTrackerAuth {
		t.Errorf("extracted.Kind = %q, want %q", extracted.Kind, ErrTrackerAuth)
	}
}

func TestTrackerError_UnwrapNil(t *testing.T) {
	t.Parallel()

	err := &TrackerError{
		Kind:    ErrTrackerPayload,
		Message: "unexpected field",
	}
	if err.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", err.Unwrap())
	}
}

func TestTrackerErrorKind_RetryClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		kind          TrackerErrorKind
		wantRetryable bool
		wantBackoff   BackoffStrategy
	}{
		{"unsupported_tracker_kind", ErrUnsupportedTrackerKind, false, BackoffNone},
		{"missing_tracker_api_key", ErrMissingTrackerAPIKey, false, BackoffNone},
		{"missing_tracker_project", ErrMissingTrackerProject, false, BackoffNone},
		{"tracker_transport_error", ErrTrackerTransport, true, BackoffExponential},
		{"tracker_auth_error", ErrTrackerAuth, false, BackoffNone},
		{"tracker_api_error", ErrTrackerAPI, true, BackoffExponential},
		{"tracker_not_found", ErrTrackerNotFound, false, BackoffNone},
		{"tracker_payload_error", ErrTrackerPayload, false, BackoffNone},
		{"tracker_missing_end_cursor", ErrTrackerMissingCursor, true, BackoffExponential},
	}

	if len(tests) != 9 {
		t.Errorf("expected 9 tracker error kinds, got %d", len(tests))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.kind.RetryClassification()

			if got.Retryable != tt.wantRetryable {
				t.Errorf("%s.RetryClassification().Retryable = %v, want %v", tt.kind, got.Retryable, tt.wantRetryable)
			}
			if got.Backoff != tt.wantBackoff {
				t.Errorf("%s.RetryClassification().Backoff = %q, want %q", tt.kind, got.Backoff, tt.wantBackoff)
			}
		})
	}

	t.Run("unknown_kind", func(t *testing.T) {
		t.Parallel()

		got := TrackerErrorKind("unknown_future_kind").RetryClassification()

		if !got.Retryable {
			t.Errorf("unknown kind: Retryable = false, want true")
		}
		if got.Backoff != BackoffExponential {
			t.Errorf("unknown kind: Backoff = %q, want %q", got.Backoff, BackoffExponential)
		}
	})
}
