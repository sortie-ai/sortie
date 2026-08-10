package gitea

import (
	"context"
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// preflightBackoff is the bounded exponential backoff applied to transient
// preflight failures. A config error fails construction immediately with no
// retry; these delays absorb a brief outage before construction fails.
//
// preflightBackoff is a package variable, not a [httpkit.RetryWithBackoff]
// argument baked in at each call site, so a test can substitute a fast
// schedule for the retry-exhaustion subtests.
var preflightBackoff = httpkit.DefaultPreflightBackoff()

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
	if err := httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
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
	return httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
		_, _, err := client.Get(ctx, repoPath, nil)
		return err
	})
}
