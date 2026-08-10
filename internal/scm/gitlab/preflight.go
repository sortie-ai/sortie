package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// preflightBackoff is the bounded exponential backoff applied to
// transient preflight failures. A config error fails construction
// immediately with no retry; these delays absorb a brief outage before
// construction fails.
//
// preflightBackoff is a package variable, not a [httpkit.RetryWithBackoff]
// argument baked in at each call site, so a test can substitute a fast
// schedule for the retry-exhaustion subtests.
var preflightBackoff = httpkit.DefaultPreflightBackoff()

// runPreflight validates the credential, the configured project, and the
// canonical casing of every configured state label before the first
// poll. It calls GET /personal_access_tokens/self once, with no retry, as
// an advisory introspection, then GET /projects/{projectPath} through
// [httpkit.RetryWithBackoff] as the authoritative gate, then, when configuredStates is
// non-empty, pages the project label catalog through [httpkit.RetryWithBackoff] to
// resolve the stored casing of each configured name.
//
// Token introspection failure never blocks construction; its result only
// informs the message on a not-found project error. The token never
// appears in a log record. A configured state label absent from the
// catalog is not an error: it is the operator's intended new label,
// created on the first add_labels write.
func runPreflight(ctx context.Context, client *httpkit.Client, projectPath string, configuredStates []string, log *slog.Logger) (map[string]string, error) {
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

	projectErr := httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
		_, _, getErr := client.Get(ctx, "/projects/"+projectPath, nil)
		return getErr
	})
	if projectErr != nil {
		if domain.IsNotFound(projectErr) {
			return nil, &domain.TrackerError{
				Kind: domain.ErrTrackerNotFound,
				Message: fmt.Sprintf(
					"gitlab: project not found or not accessible with the configured credential (token authenticated: %t)",
					tokenValid),
			}
		}
		return nil, projectErr
	}

	if len(configuredStates) == 0 {
		return map[string]string{}, nil
	}

	var catalog []gitlabLabel
	catalogErr := httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
		fetched, fetchErr := fetchProjectLabels(ctx, client, projectPath, log)
		if fetchErr != nil {
			return fetchErr
		}
		catalog = fetched
		return nil
	})
	if catalogErr != nil {
		return nil, catalogErr
	}

	casing := resolveCasing(catalog, configuredStates)
	for _, name := range configuredStates {
		if _, ok := casing[name]; !ok {
			log.Debug("configured state label absent from project", slog.String("label", name))
		}
	}
	return casing, nil
}
