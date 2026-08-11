package gitlab

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// gitlabLabelEventLabel is the label object embedded in a resource label
// event, decoding only the name. GitLab renders this key null for an
// event whose label was later deleted from the project.
type gitlabLabelEventLabel struct {
	Name string `json:"name"`
}

// gitlabResourceLabelEvent is one entry from the merge request's
// resource label-event journal.
type gitlabResourceLabelEvent struct {
	ID        int64                  `json:"id"`
	User      gitlabUser             `json:"user"`
	CreatedAt string                 `json:"created_at"`
	Action    string                 `json:"action"`
	Label     *gitlabLabelEventLabel `json:"label"`
}

// ListLabelEvents returns the label add and remove events from the
// given merge request's resource label-event journal, normalized to
// [domain.LabelEvent] and stable-sorted ascending by (At, ID).
//
// An entry whose label is null or unnamed describes a label later
// deleted from the project and is skipped, since it carries no name to
// normalize. Added is true only for action "add". The returned slice is
// non-nil even when empty; a failure returns a [*domain.SCMError].
func (a *GitLabSCMAdapter) ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]domain.LabelEvent, error) {
	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber) + "/resource_label_events"

	entries, err := paginateSCM(ctx, a.client, a.log, path, nil, func(body []byte) ([]gitlabResourceLabelEvent, error) {
		var batch []gitlabResourceLabelEvent
		if jsonErr := json.Unmarshal(body, &batch); jsonErr != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse label events response",
				Err:     jsonErr,
			}
		}
		return batch, nil
	})
	if err != nil {
		return nil, err
	}

	events := make([]domain.LabelEvent, 0, len(entries))
	for _, e := range entries {
		if e.Label == nil || e.Label.Name == "" {
			continue
		}
		events = append(events, domain.LabelEvent{
			ID:    scmcore.SortableEventID(e.ID),
			Label: strings.ToLower(e.Label.Name),
			Actor: e.User.Username,
			Added: e.Action == "add",
			At:    parseUTC(e.CreatedAt),
		})
	}

	slices.SortStableFunc(events, func(x, y domain.LabelEvent) int {
		if c := x.At.Compare(y.At); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})

	return events, nil
}

// gitlabMergeRequestUpdate is the JSON body of PUT
// /projects/{project}/merge_requests/{iid}, carrying only the label
// removal [GitLabSCMAdapter.RemoveLabel] performs.
type gitlabMergeRequestUpdate struct {
	RemoveLabels []string `json:"remove_labels,omitempty"`
}

// RemoveLabel removes every case variant of label from the given merge
// request via PUT /projects/{project}/merge_requests/{iid}.
//
// label is matched against the merge request's stored labels
// case-insensitively, and every matching variant is sent in
// remove_labels, so a stored "Sortie:Review" is removed even when label
// is configured in lowercase; GitLab matches label names
// case-sensitively. When no stored label matches, RemoveLabel returns
// nil and issues no write. A [domain.ErrSCMNotFound] reading or writing
// the merge request maps to nil, since on GitLab that status can only
// mean the merge request or the project is gone or invisible, never an
// absent label.
func (a *GitLabSCMAdapter) RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error {
	target := strings.ToLower(strings.TrimSpace(label))
	if target == "" {
		return nil
	}

	mr, err := a.fetchMergeRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return a.labelNotFoundToNil(err, prNumber)
	}

	variants := labelVariants(mr.Labels, target)
	if len(variants) == 0 {
		return nil
	}

	payload, marshalErr := json.Marshal(gitlabMergeRequestUpdate{RemoveLabels: variants})
	if marshalErr != nil {
		return &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to encode merge request update body",
			Err:     marshalErr,
		}
	}

	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber)
	if _, sendErr := a.client.Send(ctx, http.MethodPut, path, bytes.NewReader(payload)); sendErr != nil {
		return a.labelNotFoundToNil(sendErr, prNumber)
	}
	return nil
}

// labelNotFoundToNil maps a [domain.ErrSCMNotFound] from a label read or
// write to nil, logging one WARN naming prNumber, and returns every
// other kind unchanged.
func (a *GitLabSCMAdapter) labelNotFoundToNil(err error, prNumber int) error {
	scm := asSCMError(err)
	if scm.Kind == domain.ErrSCMNotFound {
		a.log.Warn("gitlab merge request not found during label removal",
			slog.Int("pr_number", prNumber))
		return nil
	}
	return scm
}
