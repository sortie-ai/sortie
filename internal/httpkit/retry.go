package httpkit

import (
	"context"
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// RetryWithBackoff runs fn, retrying while the returned error is a
// retryable [domain.TrackerError] and backoff has an unused entry left.
// The schedule is a parameter rather than package state, so a caller can
// substitute a fast schedule in a test without racing a shared variable.
//
// A non-retryable error, decided through
// [domain.TrackerErrorKind.RetryClassification], returns immediately
// without waiting. The wait between attempts honors ctx: a cancellation
// during a wait returns ctx.Err() without waiting for the remaining delay
// to elapse.
func RetryWithBackoff(ctx context.Context, backoff []time.Duration, fn func() error) error {
	err := fn()
	for attempt := 0; err != nil && attempt < len(backoff); attempt++ {
		if !isRetryable(err) {
			return err
		}
		if waitErr := sleepContext(ctx, backoff[attempt]); waitErr != nil {
			return waitErr
		}
		err = fn()
	}
	return err
}

// DefaultPreflightBackoff returns a copy of the schedule the adapter
// preflights share: one second, two seconds, four seconds. Each call
// returns a fresh slice so no caller can mutate a shared schedule.
func DefaultPreflightBackoff() []time.Duration {
	return []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
}

// sleepContext blocks for d or until ctx is cancelled, whichever comes
// first. It returns ctx.Err() on cancellation and nil once the full delay
// elapses.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryable reports whether err is a tracker error whose kind is retryable.
func isRetryable(err error) bool {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return false
	}
	return te.Kind.RetryClassification().Retryable
}
