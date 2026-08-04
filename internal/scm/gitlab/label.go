package gitlab

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// gitlabLabel is one entry of the project label catalog. Only Name is
// consumed: GitLab's add_labels and remove_labels parameters address
// labels by name, so the catalog exists solely to recover the stored
// casing of a configured label name.
type gitlabLabel struct {
	Name string `json:"name"`
}

// fetchProjectLabels pages GET /projects/{projectPath}/labels to
// exhaustion through [httpkit.NewLinkPaginator] and returns every label
// name the project can see, project-level and group-level alike. A
// group-level state label is attachable and filterable exactly like a
// project label, so omitting it would send the configured spelling for a
// label whose stored spelling differs.
//
// A page decode failure returns [domain.ErrTrackerPayload].
func fetchProjectLabels(ctx context.Context, client *httpkit.Client, projectPath string, log *slog.Logger) ([]gitlabLabel, error) {
	path := "/projects/" + projectPath + "/labels"
	params := url.Values{"per_page": {"100"}}

	paginator := httpkit.NewLinkPaginator(client, path, params, func(body []byte) ([]gitlabLabel, error) {
		var raw []gitlabLabel
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse labels response",
				Err:     err,
			}
		}
		return raw, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			log.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", path))
		},
	})

	return paginator.All(ctx)
}

// resolveCasing returns a lowercased-name to stored-casing map for the
// subset of wanted that appears in catalog. A name that matches no
// catalog entry case-insensitively is omitted.
//
// The result does not depend on catalog order. When two or more catalog
// entries share a lowercased name, the entry byte-identical to the
// wanted spelling wins; otherwise the byte-wise smallest matching name
// wins, which only matters when the project already holds a duplicate.
func resolveCasing(catalog []gitlabLabel, wanted []string) map[string]string {
	byLower := make(map[string][]string, len(catalog))
	for _, l := range catalog {
		lowered := strings.ToLower(l.Name)
		byLower[lowered] = append(byLower[lowered], l.Name)
	}

	result := make(map[string]string, len(wanted))
	for _, name := range wanted {
		lowered := strings.ToLower(name)
		variants, ok := byLower[lowered]
		if !ok {
			continue
		}
		result[lowered] = pickCasing(variants, name)
	}
	return result
}

// pickCasing chooses the winning stored casing among variants, all of
// which share the same lowercased form as wanted. The entry
// byte-identical to wanted wins outright; otherwise the byte-wise
// smallest variant wins.
func pickCasing(variants []string, wanted string) string {
	if slices.Contains(variants, wanted) {
		return wanted
	}
	return slices.Min(variants)
}
