package httpkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

func assertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v, want *domain.TrackerError", err)
	}
	if te.Kind != want {
		t.Errorf("error kind = %q, want %q", te.Kind, want)
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()

	t.Run("returns nil once the delay elapses", func(t *testing.T) {
		t.Parallel()

		if err := sleepContext(context.Background(), time.Microsecond); err != nil {
			t.Fatalf("sleepContext(_, 1us) = %v, want nil", err)
		}
	})

	t.Run("returns ctx.Err on cancellation without waiting the delay", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := sleepContext(ctx, time.Hour)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("sleepContext(cancelled, 1h) = %v, want context.Canceled", err)
		}
	})
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"transport error is retryable", &domain.TrackerError{Kind: domain.ErrTrackerTransport}, true},
		{"api error is retryable", &domain.TrackerError{Kind: domain.ErrTrackerAPI}, true},
		{"auth error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerAuth}, false},
		{"not found error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerNotFound}, false},
		{"payload error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerPayload}, false},
		{"non-tracker error is not retryable", errors.New("boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isRetryable(tt.err)
			if got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDefaultPreflightBackoff(t *testing.T) {
	t.Parallel()

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	got := DefaultPreflightBackoff()
	if len(got) != len(want) {
		t.Fatalf("DefaultPreflightBackoff() = %v, want %v", got, want)
	}
	for i, d := range want {
		if got[i] != d {
			t.Errorf("DefaultPreflightBackoff()[%d] = %v, want %v", i, got[i], d)
		}
	}

	got[0] = time.Hour
	again := DefaultPreflightBackoff()
	if again[0] != time.Second {
		t.Errorf("DefaultPreflightBackoff() after caller mutation = %v, want a fresh copy unaffected by the earlier mutation", again)
	}
}

func TestWithRetry(t *testing.T) {
	t.Run("succeeds on first try", func(t *testing.T) {
		backoff := []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}

		var calls int
		err := RetryWithBackoff(context.Background(), backoff, func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("RetryWithBackoff: %v", err)
		}
		if calls != 1 {
			t.Errorf("call count = %d, want 1", calls)
		}
	})

	t.Run("non-retryable error returns immediately without retry", func(t *testing.T) {
		backoff := []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}

		var calls int
		err := RetryWithBackoff(context.Background(), backoff, func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "bad token"}
		})
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
		if calls != 1 {
			t.Errorf("call count = %d, want 1 (non-retryable must not retry)", calls)
		}
	})

	t.Run("retryable error retries the full schedule then fails", func(t *testing.T) {
		backoff := []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}

		var calls int
		err := RetryWithBackoff(context.Background(), backoff, func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}
		})
		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		wantCalls := len(backoff) + 1
		if calls != wantCalls {
			t.Errorf("call count = %d, want %d (initial + one per backoff entry)", calls, wantCalls)
		}
	})

	t.Run("retryable error then success recovers", func(t *testing.T) {
		backoff := []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}

		var calls int
		err := RetryWithBackoff(context.Background(), backoff, func() error {
			calls++
			if calls < 2 {
				return &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "rate limited"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("RetryWithBackoff should recover after one transient failure: %v", err)
		}
		if calls != 2 {
			t.Errorf("call count = %d, want 2 (one retry then success)", calls)
		}
	})

	t.Run("cancellation during backoff returns ctx.Err without waiting the schedule", func(t *testing.T) {
		// Uses the production backoff schedule deliberately: the context is
		// already cancelled before the call, so sleepContext returns
		// immediately regardless of the configured delay.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var calls int
		err := RetryWithBackoff(ctx, DefaultPreflightBackoff(), func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RetryWithBackoff error = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("call count = %d, want 1 (cancellation must stop before a retry)", calls)
		}
	})
}
