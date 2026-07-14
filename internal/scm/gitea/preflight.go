package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// preflightBackoff is the bounded exponential backoff applied to transient
// preflight failures. A config error fails construction immediately with no
// retry; these delays absorb a brief outage before construction fails.
var preflightBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// runPreflight validates the token and the configured repository before the
// first poll. It runs GET /user (credential check) and GET /repos/{owner}/{repo}
// (project check) through the same client the read methods use.
//
// A config error (401 or 403 auth, 404 not found) fails construction
// immediately; a transient error (5xx or transport) is retried with the bounded
// backoff. The returned user login is not consumed on the read path. Failures
// return a classified [*domain.TrackerError], except that a context cancellation
// or deadline surfaces as the context error.
func runPreflight(ctx context.Context, client *httpkit.Client, owner, repo string) error {
	if err := withRetry(ctx, func() error {
		body, _, err := client.Get(ctx, "/user", nil)
		if err != nil {
			return err
		}
		var user giteaUser
		if err := json.Unmarshal(body, &user); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to decode gitea user response",
				Err:     err,
			}
		}
		return nil
	}); err != nil {
		return err
	}

	repoPath := "/repos/" + owner + "/" + repo
	return withRetry(ctx, func() error {
		_, _, err := client.Get(ctx, repoPath, nil)
		return err
	})
}

// withRetry runs fn, retrying transient tracker errors with the bounded
// preflight backoff. Config errors return immediately without a retry.
//
// The backoff wait honors ctx: a cancellation during a backoff returns
// ctx.Err() without waiting for the delay to elapse.
func withRetry(ctx context.Context, fn func() error) error {
	err := fn()
	for attempt := 0; err != nil && attempt < len(preflightBackoff); attempt++ {
		if !isRetryable(err) {
			return err
		}
		if waitErr := sleepContext(ctx, preflightBackoff[attempt]); waitErr != nil {
			return waitErr
		}
		err = fn()
	}
	return err
}

// sleepContext blocks for d or until ctx is cancelled, whichever comes first.
// It returns ctx.Err() on cancellation and nil once the full delay elapses.
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
