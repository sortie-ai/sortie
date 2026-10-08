package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// issueLabelsPageSize is the largest page the issue labels route serves.
const issueLabelsPageSize = "100"

// githubIssueLabelOps returns the label calls for one issue or pull request.
// The tracker adapter and the source-control adapter both drive the shared
// label writers with it, so one request path serves every label operation.
//
// Removal deletes each matched spelling by name and reports the label list of
// the last response. The replace-style PUT on the labels route is never sent,
// because it drops a label a person adds between the read and the write.
func githubIssueLabelOps(client *httpkit.Client, owner, repo, issueNumber string) issuekit.IssueLabelOps {
	labelsPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/issues/" + url.PathEscape(issueNumber) + "/labels"

	return issuekit.IssueLabelOps{
		Read: func(ctx context.Context) ([]issuekit.IssueLabel, error) {
			return readIssueLabels(ctx, client, labelsPath)
		},
		Add: func(ctx context.Context, label string) (issuekit.LabelWrite, error) {
			payload, err := json.Marshal(map[string][]string{"labels": {label}})
			if err != nil {
				return issuekit.LabelWrite{}, &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to marshal label payload",
					Err:     err,
				}
			}
			body, err := client.Send(ctx, "POST", labelsPath, bytes.NewReader(payload))
			if err != nil {
				return issuekit.LabelWrite{}, err
			}
			return reportedLabelList(body), nil
		},
		Remove: func(ctx context.Context, labels []issuekit.IssueLabel) (issuekit.LabelWrite, error) {
			var write issuekit.LabelWrite
			var rejected error
			for _, label := range labels {
				body, err := client.Send(ctx, "DELETE", labelsPath+"/"+url.PathEscape(label.Name), nil)
				switch {
				case err == nil:
					write = reportedLabelList(body)
				case domain.IsNotFound(err) || isPayloadRejection(err):
					if rejected == nil {
						rejected = err
					}
				default:
					return issuekit.LabelWrite{}, err
				}
			}
			return write, rejected
		},
	}
}

func readIssueLabels(ctx context.Context, client *httpkit.Client, labelsPath string) ([]issuekit.IssueLabel, error) {
	paginator := httpkit.NewLinkPaginator(client, labelsPath, url.Values{"per_page": {issueLabelsPageSize}}, func(body []byte) ([]githubLabel, error) {
		var labels []githubLabel
		if err := json.Unmarshal(body, &labels); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue labels response",
				Err:     err,
			}
		}
		return labels, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/repos/{owner}/{repo}/issues/{issue_id}/labels"))
		},
	})

	labels, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}

	stored := make([]issuekit.IssueLabel, len(labels))
	for i, l := range labels {
		stored[i] = issuekit.IssueLabel{Name: l.Name}
	}
	return stored, nil
}

// reportedLabelList reads the label list a label write answers with. A body
// that is not a label list leaves the write unreported.
func reportedLabelList(body []byte) issuekit.LabelWrite {
	var labels []githubLabel
	if err := json.Unmarshal(body, &labels); err != nil || labels == nil {
		return issuekit.LabelWrite{}
	}
	return issuekit.LabelWrite{After: githubLabelNames(labels), Reported: true}
}

func isPayloadRejection(err error) bool {
	var te *domain.TrackerError
	return errors.As(err, &te) && te.Kind == domain.ErrTrackerPayload
}
