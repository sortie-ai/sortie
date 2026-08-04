package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// preflightBackoff is the bounded exponential backoff applied to
// transient preflight failures. A config error fails construction
// immediately with no retry; these delays absorb a brief outage before
// construction fails.
var preflightBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// runPreflight validates the credential and the configured project before
// the first poll. It calls GET /personal_access_tokens/self once, with no
// retry, as an advisory introspection, then GET /projects/{projectPath}
// through [withRetry] as the authoritative gate.
//
// Token introspection failure never blocks construction; its result only
// informs the message on a not-found project error. The token never
// appears in a log record.
func runPreflight(ctx context.Context, client *httpkit.Client, projectPath string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	tokenValid := false

	body, _, err := client.Get(ctx, "/personal_access_tokens/self", nil)
	if err != nil {
		log.Debug("gitlab token introspection unavailable", slog.Any("error", err))
	} else {
		var info gitlabTokenInfo
		if err := json.Unmarshal(body, &info); err != nil {
			log.Debug("gitlab token introspection unavailable", slog.Any("error", err))
		} else {
			tokenValid = info.Active && !info.Revoked
			log.Debug("gitlab token introspected",
				slog.Any("scopes", info.Scopes),
				slog.Bool("active", info.Active),
				slog.Bool("revoked", info.Revoked),
				slog.String("expires_at", info.ExpiresAt))
			if info.Revoked || !info.Active {
				log.Warn("gitlab token reports revoked or inactive")
			}
		}
	}

	projectErr := withRetry(ctx, func() error {
		_, _, getErr := client.Get(ctx, "/projects/"+projectPath, nil)
		return getErr
	})
	if projectErr != nil {
		if domain.IsNotFound(projectErr) {
			return &domain.TrackerError{
				Kind: domain.ErrTrackerNotFound,
				Message: fmt.Sprintf(
					"gitlab: project not found or not accessible with the configured credential (token authenticated: %t)",
					tokenValid),
			}
		}
		return projectErr
	}
	return nil
}

// withRetry runs fn, retrying transient tracker errors with the bounded
// [preflightBackoff] schedule. Config errors return immediately without a
// retry.
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

// isRetryable reports whether err is a tracker error whose kind is
// retryable.
func isRetryable(err error) bool {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return false
	}
	return te.Kind.RetryClassification().Retryable
}
