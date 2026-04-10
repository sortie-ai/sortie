package orchestrator

import (
	"context"
	"testing"
)

func TestContinuationContext_RoundTrip(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"status":        "failing",
		"failing_count": 2,
		"ref":           "feature/abc",
	}

	ctx := WithContinuationContext(context.Background(), data)
	got := ContinuationFromContext(ctx)

	if got == nil {
		t.Fatal("ContinuationFromContext() = nil; want non-nil map")
	}
	if got["status"] != "failing" {
		t.Errorf("ContinuationFromContext()[status] = %v, want %q", got["status"], "failing")
	}
	if got["failing_count"] != 2 {
		t.Errorf("ContinuationFromContext()[failing_count] = %v, want 2", got["failing_count"])
	}
	if got["ref"] != "feature/abc" {
		t.Errorf("ContinuationFromContext()[ref] = %v, want %q", got["ref"], "feature/abc")
	}
}

func TestContinuationContext_AbsentReturnsNil(t *testing.T) {
	t.Parallel()

	got := ContinuationFromContext(context.Background())
	if got != nil {
		t.Errorf("ContinuationFromContext(plain context) = %v; want nil", got)
	}
}

func TestContinuationContext_NilDataRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithContinuationContext(context.Background(), nil)
	got := ContinuationFromContext(ctx)
	if got != nil {
		t.Errorf("ContinuationFromContext(nil data) = %v; want nil", got)
	}
}

func TestContinuationContext_ParentContextNotAffected(t *testing.T) {
	t.Parallel()

	parent := context.Background()
	data := map[string]any{"ref": "main"}

	child := WithContinuationContext(parent, data)

	if ContinuationFromContext(child) == nil {
		t.Fatal("ContinuationFromContext(child) = nil; want non-nil")
	}
	if ContinuationFromContext(parent) != nil {
		t.Error("ContinuationFromContext(parent) != nil; parent context must not be affected")
	}
}
