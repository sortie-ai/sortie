package scmcore_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func TestToSCMError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    error
		wantKind domain.SCMErrorKind
	}{
		{
			name:     "ErrTrackerTransport → ErrSCMTransport",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "net"},
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "ErrTrackerAuth → ErrSCMAuth",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "auth"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "ErrTrackerAPI → ErrSCMAPI",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "api"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "ErrTrackerNotFound → ErrSCMNotFound",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "nf"},
			wantKind: domain.ErrSCMNotFound,
		},
		{
			name:     "ErrTrackerPayload → ErrSCMPayload",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "pl"},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name:     "unknown TrackerErrorKind → ErrSCMAPI",
			input:    &domain.TrackerError{Kind: "unknown_kind", Message: "x"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "non-TrackerError → ErrSCMTransport",
			input:    fmt.Errorf("some generic transport error"),
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "401 unauthorized classifies as an auth error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "invalid credentials"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "403 forbidden classifies as an auth error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "insufficient permissions"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "404 not found classifies as a not-found error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "not found"},
			wantKind: domain.ErrSCMNotFound,
		},
		{
			name:     "5xx server error classifies as a transport error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "server error"},
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "api error classifies as an api error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "rate limited"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "payload error classifies as a payload error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "bad request"},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name:     "a non-tracker error wraps as a transport error",
			input:    errors.New("dial tcp: connection refused"),
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "the word conflict alone, without the formatted marker, does not promote",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "GET /repos/acme/widgets: there was a conflict in the request"},
			wantKind: domain.ErrSCMAPI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := scmcore.ToSCMError(tt.input)
			if got.Kind != tt.wantKind {
				t.Errorf("ToSCMError(%v).Kind = %q, want %q", tt.input, got.Kind, tt.wantKind)
			}
			if got.Err != tt.input {
				t.Errorf("ToSCMError(%v).Err = %v, want the original error preserved", tt.input, got.Err)
			}
		})
	}
}

func TestAsMergeConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    *domain.TrackerError
		wantKind domain.SCMErrorKind
	}{
		{
			name: "405 method not allowed promotes to a conflict error",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "PUT /repos/o/r/pulls/1/merge: method not allowed: branch protection refuses",
				Status:  405,
			},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name: "409 conflict promotes to a conflict error",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "PUT /repos/o/r/pulls/1/merge: conflict: head sha mismatch",
				Status:  409,
			},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name: "gitea 405 method not allowed promotes to a conflict error",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "POST /repos/acme/widgets/pulls/6/merge: method not allowed: The PR is already merged",
				Status:  405,
			},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name: "gitea 409 conflict promotes to a conflict error",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "POST /repos/acme/widgets/pulls/6/merge: conflict: head out of date",
				Status:  409,
			},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name: "status zero with the word conflict in the message does not promote",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "GET /repos/acme/widgets: there was a conflict in the request",
				Status:  0,
			},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name: "404 not found does not promote",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: "not found",
				Status:  404,
			},
			wantKind: domain.ErrSCMNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := scmcore.AsMergeConflict(scmcore.ToSCMError(tt.input))
			if got.Kind != tt.wantKind {
				t.Errorf("AsMergeConflict(...).Kind = %q, want %q", got.Kind, tt.wantKind)
			}
		})
	}
}

func TestAlreadyMergedConflict(t *testing.T) {
	t.Parallel()

	cause := errors.New("head sha mismatch")
	got := scmcore.AlreadyMergedConflict("pull request 6", cause)

	if got.Kind != domain.ErrSCMConflict {
		t.Errorf("AlreadyMergedConflict(...).Kind = %q, want %q", got.Kind, domain.ErrSCMConflict)
	}
	if !strings.Contains(strings.ToLower(got.Message), "already merged") {
		t.Errorf("AlreadyMergedConflict(...).Message = %q, want it to contain %q", got.Message, "already merged")
	}
	if !errors.Is(got, cause) {
		t.Errorf("AlreadyMergedConflict(...) does not unwrap to the supplied cause")
	}
}
