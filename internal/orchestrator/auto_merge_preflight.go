package orchestrator

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
)

// AutoMergeScopeVerifier is an optional capability check satisfied by
// SCM adapters that can inspect the token's scope grant. The
// auto-merge preflight type-asserts the configured SCMAdapter against
// this interface; adapters that do not implement it are exempted from
// the scope check.
type AutoMergeScopeVerifier interface {
	// VerifyAutoMergeScopes returns the granted scopes, the subset of
	// required scopes that are missing, and any transport-class
	// failure. An empty missing list with a nil error means the token
	// has sufficient scope for auto-merge.
	VerifyAutoMergeScopes(ctx context.Context, requireContents bool) (scopes []string, missing []string, err error)
}

// RunAutoMergePreflight validates the configured token's scope grant
// for auto-merge at orchestrator startup. The returned passed flag
// reports whether the token has sufficient scope; missing carries the
// per-required-scope subset that is absent; err carries a transport
// or API failure. A nil err with passed=false means the token is
// missing one or more scopes (auth-class failure).
func RunAutoMergePreflight(ctx context.Context, adapter domain.SCMAdapter, requireContents bool, log *slog.Logger) (bool, []string, error) {
	if log == nil {
		log = slog.Default()
	}

	verifier, ok := adapter.(AutoMergeScopeVerifier)
	if !ok {
		log.Warn("auto_merge preflight skipped: adapter does not implement scope verification")
		return true, nil, nil
	}

	scopes, missing, err := verifier.VerifyAutoMergeScopes(ctx, requireContents)
	if err != nil {
		log.Warn("auto_merge preflight transport failure, scheduled one-shot retry",
			slog.Any("error", err),
			slog.Duration("retry_delay", AutoMergePreflightRetryDelay),
		)
		return false, nil, err
	}

	if len(missing) > 0 {
		log.Error("auto_merge preflight failed: insufficient token scope",
			slog.String("missing_scope", missing[0]),
			slog.Any("required_scopes", buildRequiredScopeList(requireContents)),
			slog.String("docs_url", autoMergeDocsURL),
		)
		return false, missing, nil
	}

	log.Info("auto_merge preflight passed",
		slog.Any("scopes", scopes),
	)
	return true, nil, nil
}

// RunAutoMergePreflightRetry re-runs the scope verification after a
// transport-class startup failure. The retry path uses distinct log
// messages from the initial preflight so operators can correlate the
// outcome with the scheduled retry contract. Runs at most once per
// orchestrator lifetime; the caller is responsible for clearing the
// scheduled timestamp before invoking this function.
func RunAutoMergePreflightRetry(ctx context.Context, adapter domain.SCMAdapter, requireContents bool, log *slog.Logger) (bool, []string, error) {
	if log == nil {
		log = slog.Default()
	}

	verifier, ok := adapter.(AutoMergeScopeVerifier)
	if !ok {
		log.Warn("auto_merge preflight retry skipped: adapter does not implement scope verification")
		return true, nil, nil
	}

	scopes, missing, err := verifier.VerifyAutoMergeScopes(ctx, requireContents)
	if err != nil {
		log.Warn("auto_merge preflight retry transport failure, no further retries this lifetime",
			slog.Any("error", err),
		)
		return false, nil, err
	}

	if len(missing) > 0 {
		log.Error("auto_merge preflight retry failed: insufficient token scope",
			slog.String("missing_scope", missing[0]),
			slog.Any("required_scopes", buildRequiredScopeList(requireContents)),
			slog.String("docs_url", autoMergeDocsURL),
		)
		return false, missing, nil
	}

	log.Info("auto_merge preflight retry succeeded",
		slog.Any("scopes", scopes),
	)
	return true, nil, nil
}

// buildRequiredScopeList returns the canonical required-scope list
// for an auto-merge deployment. Includes the branch-delete scope only
// when requireContents is true.
func buildRequiredScopeList(requireContents bool) []string {
	required := []string{"pull_requests:write"}
	if requireContents {
		required = append(required, "contents:write")
	}
	return required
}

// IsAutoMergePreflightTransportClass reports whether err matches a
// transport-class or API-class SCM failure, indicating that the
// initial preflight should schedule a one-shot retry on the existing
// reconcile tick.
func IsAutoMergePreflightTransportClass(err error) bool {
	if err == nil {
		return false
	}
	var scmErr *domain.SCMError
	if !errors.As(err, &scmErr) {
		return false
	}
	return scmErr.Kind == domain.ErrSCMTransport || scmErr.Kind == domain.ErrSCMAPI
}
