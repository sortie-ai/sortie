package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/issuekit"
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

// gitlabLabelOps returns the label calls for the issue or merge request
// served at resourcePath. The tracker adapter and the source-control adapter
// both drive the shared label writers with it, so one request path serves
// every label operation. issueRoute marks the issue route, where an entity
// whose issue_type is set and is not "issue" does not exist for the caller.
//
// Both writes are one delta PUT (add_labels or remove_labels) and report the
// labels of the response. The replace-style labels parameter is never sent,
// because it drops a label a person adds between the read and the write.
func gitlabLabelOps(client *httpkit.Client, log *slog.Logger, projectPath, resourcePath string, issueRoute bool) issuekit.IssueLabelOps {
	return issuekit.IssueLabelOps{
		Read: func(ctx context.Context) ([]issuekit.IssueLabel, error) {
			body, _, err := client.Get(ctx, resourcePath, nil)
			if err != nil {
				return nil, err
			}
			var resource gitlabIssue
			if err := json.Unmarshal(body, &resource); err != nil {
				return nil, &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to parse labels response",
					Err:     err,
				}
			}
			if issueRoute && resource.IssueType != "" && resource.IssueType != "issue" {
				return nil, &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: "not an issue: " + resourcePath,
				}
			}
			stored := make([]issuekit.IssueLabel, len(resource.Labels))
			for i, name := range resource.Labels {
				stored[i] = issuekit.IssueLabel{Name: name}
			}
			return stored, nil
		},
		Add: func(ctx context.Context, label string) (issuekit.LabelWrite, error) {
			name := strings.TrimSpace(label)
			catalog, err := fetchProjectLabels(ctx, client, projectPath, log)
			if err != nil {
				log.Warn("gitlab label catalog unavailable; attaching the configured spelling",
					slog.Any("error", err))
			} else if stored, ok := resolveCasing(catalog, []string{name})[strings.ToLower(name)]; ok {
				name = stored
			}
			return putLabels(ctx, client, resourcePath, gitlabIssueUpdate{AddLabels: []string{name}})
		},
		Remove: func(ctx context.Context, labels []issuekit.IssueLabel) (issuekit.LabelWrite, error) {
			names := make([]string, len(labels))
			for i, l := range labels {
				names[i] = l.Name
			}
			return putLabels(ctx, client, resourcePath, gitlabIssueUpdate{RemoveLabels: names})
		},
	}
}

// putLabels sends one label update and reports the labels the response
// carries. A response that holds no label list leaves the write unreported.
func putLabels(ctx context.Context, client *httpkit.Client, resourcePath string, update gitlabIssueUpdate) (issuekit.LabelWrite, error) {
	payload, err := json.Marshal(update)
	if err != nil {
		return issuekit.LabelWrite{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal label payload",
			Err:     err,
		}
	}

	body, err := client.Send(ctx, http.MethodPut, resourcePath, bytes.NewReader(payload))
	if err != nil {
		return issuekit.LabelWrite{}, err
	}

	var resource gitlabIssue
	if err := json.Unmarshal(body, &resource); err != nil || resource.Labels == nil {
		return issuekit.LabelWrite{}, nil
	}
	return issuekit.LabelWrite{After: resource.Labels, Reported: true}, nil
}
