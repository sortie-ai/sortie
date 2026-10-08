package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// labelPageSize is the page size of the issue labels route.
const labelPageSize = 50

// giteaIssueLabelOps returns the label calls for one issue or pull request.
// The tracker adapter and the source-control adapter both drive the shared
// label writers with it, so one request path serves every label operation.
//
// Gitea removes and attaches a label by id, and the issue's own label list is
// the only place that names the id of an organization label, so removal takes
// ids from that list and never from the repository catalog. Neither write
// reads the issue's labels back from a removal; a rejected delete is settled
// by the confirming read. labelID resolves or creates the label an add
// attaches; a caller that only removes labels passes nil.
func giteaIssueLabelOps(client *httpkit.Client, owner, repo, issueIndex string, labelID func(ctx context.Context, lowered string) (int64, error)) issuekit.IssueLabelOps {
	labelsPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/issues/" + url.PathEscape(issueIndex) + "/labels"

	return issuekit.IssueLabelOps{
		Read: func(ctx context.Context) ([]issuekit.IssueLabel, error) {
			labels, err := readIssueLabels(ctx, client, labelsPath)
			if err != nil {
				return nil, err
			}
			stored := make([]issuekit.IssueLabel, len(labels))
			for i, l := range labels {
				stored[i] = issuekit.IssueLabel{Name: l.Name, ID: strconv.FormatInt(l.ID, 10)}
			}
			return stored, nil
		},
		Add: func(ctx context.Context, label string) (issuekit.LabelWrite, error) {
			id, err := labelID(ctx, strings.ToLower(label))
			if err != nil {
				return issuekit.LabelWrite{}, err
			}
			payload, err := json.Marshal(map[string][]int64{"labels": {id}})
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
			var rejected error
			for _, label := range labels {
				err := client.SendNoBody(ctx, "DELETE", labelsPath+"/"+url.PathEscape(label.ID))
				switch {
				case err == nil:
				case domain.IsNotFound(err) || isPayloadRejection(err):
					if rejected == nil {
						rejected = err
					}
				default:
					return issuekit.LabelWrite{}, err
				}
			}
			return issuekit.LabelWrite{}, rejected
		},
	}
}

func readIssueLabels(ctx context.Context, client *httpkit.Client, labelsPath string) ([]giteaLabel, error) {
	paginator := httpkit.NewPagePaginator(client, labelsPath, nil, func(body []byte) ([]giteaLabel, error) {
		var batch []giteaLabel
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue labels response",
				Err:     err,
			}
		}
		return batch, nil
	}, httpkit.PageOptions{
		PageParam: "page",
		SizeParam: "limit",
		PageSize:  labelPageSize,
		MaxPages:  scmMaxPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "response truncated at page limit",
				slog.String("path", labelsPath),
				slog.Int("max_pages", limit))
		},
	})
	return paginator.All(ctx)
}

// reportedLabelList reads the label list an attach answers with. A body that
// is not a label list leaves the write unreported.
func reportedLabelList(body []byte) issuekit.LabelWrite {
	var labels []giteaLabel
	if err := json.Unmarshal(body, &labels); err != nil || labels == nil {
		return issuekit.LabelWrite{}
	}
	return issuekit.LabelWrite{After: giteaLabelNames(labels), Reported: true}
}

// isPayloadRejection reports whether err is the tracker refusing a write as
// invalid, the status Gitea answers a delete of a label id that vanished
// between the read and the delete.
func isPayloadRejection(err error) bool {
	var te *domain.TrackerError
	return errors.As(err, &te) && te.Kind == domain.ErrTrackerPayload
}
