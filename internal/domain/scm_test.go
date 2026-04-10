package domain

import (
	"context"
	"errors"
	"testing"
)

// --- TestSCMError_Error ---

func TestSCMError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *SCMError
		want string
	}{
		{
			name: "kind and message only",
			err: &SCMError{
				Kind:    ErrSCMAPI,
				Message: "rate limited",
			},
			want: "scm: scm_api_error: rate limited",
		},
		{
			name: "with underlying error",
			err: &SCMError{
				Kind:    ErrSCMTransport,
				Message: "connection refused",
				Err:     errors.New("dial tcp: refused"),
			},
			want: "scm: scm_transport_error: connection refused: dial tcp: refused",
		},
		{
			name: "auth error without underlying",
			err: &SCMError{
				Kind:    ErrSCMAuth,
				Message: "invalid token",
			},
			want: "scm: scm_auth_error: invalid token",
		},
		{
			name: "not found error",
			err: &SCMError{
				Kind:    ErrSCMNotFound,
				Message: "PR not found",
			},
			want: "scm: scm_not_found: PR not found",
		},
		{
			name: "payload error",
			err: &SCMError{
				Kind:    ErrSCMPayload,
				Message: "malformed response",
			},
			want: "scm: scm_payload_error: malformed response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.err.Error()
			if got != tt.want {
				t.Errorf("SCMError.Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- TestSCMError_Unwrap ---

func TestSCMError_Unwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel error")

	t.Run("wraps underlying error", func(t *testing.T) {
		t.Parallel()
		se := &SCMError{
			Kind:    ErrSCMTransport,
			Message: "network failure",
			Err:     sentinel,
		}
		if !errors.Is(se, sentinel) {
			t.Errorf("errors.Is(SCMError, sentinel) = false, want true")
		}
		unwrapped := se.Unwrap()
		if unwrapped != sentinel {
			t.Errorf("SCMError.Unwrap() = %v, want %v", unwrapped, sentinel)
		}
	})

	t.Run("nil underlying error", func(t *testing.T) {
		t.Parallel()
		se := &SCMError{
			Kind:    ErrSCMAuth,
			Message: "bad token",
		}
		if se.Unwrap() != nil {
			t.Errorf("SCMError.Unwrap() = %v, want nil", se.Unwrap())
		}
	})

	t.Run("errors.As traverses chain", func(t *testing.T) {
		t.Parallel()
		inner := &SCMError{Kind: ErrSCMTransport, Message: "inner"}
		outer := &SCMError{Kind: ErrSCMAPI, Message: "outer", Err: inner}

		var got *SCMError
		if !errors.As(outer, &got) {
			t.Fatal("errors.As(outer, *SCMError) = false, want true")
		}
		if got.Kind != ErrSCMAPI {
			t.Errorf("errors.As extracted Kind = %q, want %q", got.Kind, ErrSCMAPI)
		}
	})
}

// --- TestSCMAdapter_InterfaceCompliance ---

// Compile-time check: mockSCMAdapter satisfies the SCMAdapter interface.
var _ SCMAdapter = (*mockSCMAdapter)(nil)

type mockSCMAdapter struct{}

func (m *mockSCMAdapter) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]ReviewComment, error) {
	return nil, nil
}
