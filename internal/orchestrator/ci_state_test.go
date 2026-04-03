package orchestrator

import (
	"context"
	"testing"
)

func TestCIFailureContext_RoundTrip(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"status":        "failing",
		"failing_count": 2,
		"ref":           "feature/abc",
	}

	ctx := WithCIFailureContext(context.Background(), data)
	got := CIFailureFromContext(ctx)

	if got == nil {
		t.Fatal("CIFailureFromContext() = nil; want non-nil map")
	}
	if got["status"] != "failing" {
		t.Errorf("CIFailureFromContext()[status] = %v, want %q", got["status"], "failing")
	}
	if got["failing_count"] != 2 {
		t.Errorf("CIFailureFromContext()[failing_count] = %v, want 2", got["failing_count"])
	}
	if got["ref"] != "feature/abc" {
		t.Errorf("CIFailureFromContext()[ref] = %v, want %q", got["ref"], "feature/abc")
	}
}

func TestCIFailureContext_AbsentReturnsNil(t *testing.T) {
	t.Parallel()

	got := CIFailureFromContext(context.Background())
	if got != nil {
		t.Errorf("CIFailureFromContext(plain context) = %v; want nil", got)
	}
}

func TestCIFailureContext_NilDataRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithCIFailureContext(context.Background(), nil)
	got := CIFailureFromContext(ctx)
	if got != nil {
		t.Errorf("CIFailureFromContext(nil data) = %v; want nil", got)
	}
}

func TestCIFailureContext_ParentContextNotAffected(t *testing.T) {
	t.Parallel()

	parent := context.Background()
	data := map[string]any{"ref": "main"}

	child := WithCIFailureContext(parent, data)

	// Child has the data.
	if CIFailureFromContext(child) == nil {
		t.Fatal("CIFailureFromContext(child) = nil; want non-nil")
	}
	// Parent is unmodified (context.WithValue creates a new context).
	if CIFailureFromContext(parent) != nil {
		t.Error("CIFailureFromContext(parent) != nil; parent context must not be affected")
	}
}
