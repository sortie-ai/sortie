package scmcore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func TestToCIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    *domain.TrackerError
		wantKind domain.CIErrorKind
	}{
		{"ErrTrackerTransport maps to ErrCITransport", &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "net"}, domain.ErrCITransport},
		{"ErrTrackerAuth maps to ErrCIAuth", &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "auth"}, domain.ErrCIAuth},
		{"ErrMissingTrackerAPIKey maps to ErrCIAuth", &domain.TrackerError{Kind: domain.ErrMissingTrackerAPIKey, Message: "key"}, domain.ErrCIAuth},
		{"ErrTrackerAPI maps to ErrCIAPI", &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "api"}, domain.ErrCIAPI},
		{"ErrTrackerNotFound maps to ErrCINotFound", &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "nf"}, domain.ErrCINotFound},
		{"ErrTrackerPayload maps to ErrCIPayload", &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "pl"}, domain.ErrCIPayload},
		{"ErrMissingTrackerProject maps to ErrCIPayload", &domain.TrackerError{Kind: domain.ErrMissingTrackerProject, Message: "proj"}, domain.ErrCIPayload},
		{"an unrecognized TrackerErrorKind falls back to ErrCIAPI", &domain.TrackerError{Kind: domain.TrackerErrorKind("unreachable_kind"), Message: "x"}, domain.ErrCIAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := scmcore.ToCIError(tt.input)

			var ce *domain.CIError
			if !errors.As(got, &ce) {
				t.Fatalf("ToCIError(%v) returned %T, want *domain.CIError", tt.input, got)
			}
			if ce.Kind != tt.wantKind {
				t.Errorf("CIError.Kind = %q, want %q", ce.Kind, tt.wantKind)
			}
			if ce.Message != tt.input.Message {
				t.Errorf("CIError.Message = %q, want %q", ce.Message, tt.input.Message)
			}
			if !errors.Is(got, tt.input) {
				t.Error("ToCIError: underlying TrackerError not preserved in the error chain")
			}
		})
	}
}

func TestToCIError_NilInput(t *testing.T) {
	t.Parallel()

	got := scmcore.ToCIError(nil)

	if got != nil {
		t.Errorf("ToCIError(nil) = %v, want nil", got)
	}
}

func TestToCIError_ContextCanceled(t *testing.T) {
	t.Parallel()

	got := scmcore.ToCIError(fmt.Errorf("fetching ci status: %w", context.Canceled))

	if !errors.Is(got, context.Canceled) {
		t.Errorf("ToCIError(context.Canceled) = %v, want context.Canceled passthrough", got)
	}
	var ce *domain.CIError
	if errors.As(got, &ce) {
		t.Errorf("ToCIError(context.Canceled) = %v, want no CIError conversion", ce)
	}
}

func TestToCIError_DeadlineExceeded(t *testing.T) {
	t.Parallel()

	got := scmcore.ToCIError(fmt.Errorf("fetching ci status: %w", context.DeadlineExceeded))

	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("ToCIError(context.DeadlineExceeded) = %v, want context.DeadlineExceeded passthrough", got)
	}
	var ce *domain.CIError
	if errors.As(got, &ce) {
		t.Errorf("ToCIError(context.DeadlineExceeded) = %v, want no CIError conversion", ce)
	}
}

func TestToCIError_CIErrorPassthrough(t *testing.T) {
	t.Parallel()

	original := fmt.Errorf("decode page: %w", &domain.CIError{Kind: domain.ErrCINotFound, Message: "already typed"})

	got := scmcore.ToCIError(original)

	if got != original {
		t.Errorf("ToCIError(%v) = %v, want the original error returned unchanged", original, got)
	}
	var ce *domain.CIError
	if !errors.As(got, &ce) {
		t.Fatalf("ToCIError returned %T, want it to still unwrap to *domain.CIError", got)
	}
	if ce.Kind != domain.ErrCINotFound {
		t.Errorf("CIError.Kind = %q, want %q (the original kind must survive unmodified)", ce.Kind, domain.ErrCINotFound)
	}
}

func TestToCIError_NonTrackerError(t *testing.T) {
	t.Parallel()

	base := errors.New("boom")

	got := scmcore.ToCIError(fmt.Errorf("wrapped: %w", base))

	var ce *domain.CIError
	if !errors.As(got, &ce) {
		t.Fatalf("ToCIError returned %T, want *domain.CIError", got)
	}
	if ce.Kind != domain.ErrCIAPI {
		t.Errorf("CIError.Kind = %q, want %q", ce.Kind, domain.ErrCIAPI)
	}
	if !errors.Is(got, base) {
		t.Error("ToCIError: underlying error not preserved in the error chain")
	}
}
