package gitlab

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// gitlabReviewer is one entry from the merge-request reviewers route.
// State is the top-level review state; the nested User.State (account
// state) is never decoded.
type gitlabReviewer struct {
	User  gitlabUser `json:"user"`
	State string     `json:"state"`
}

// gitlabApprovals is the merge-request approvals response, decoding
// only the Community Edition field the review-decision fold reads.
type gitlabApprovals struct {
	Approved bool `json:"approved"`
}

// gitlabUserDetail is the GET /users/:id response, decoding only the
// platform bot marker.
type gitlabUserDetail struct {
	Bot bool `json:"bot"`
}

// fetchReviewers returns every reviewer of the given merge request via
// GET /projects/{projectPath}/merge_requests/{prNumber}/reviewers.
//
// A decode failure returns a [*domain.SCMError] of kind
// [domain.ErrSCMPayload].
func (a *GitLabSCMAdapter) fetchReviewers(ctx context.Context, prNumber int, owner, repo string) ([]gitlabReviewer, error) {
	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber) + "/reviewers"

	return paginateSCM(ctx, a.client, a.log, path, nil, func(body []byte) ([]gitlabReviewer, error) {
		var batch []gitlabReviewer
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse reviewers response",
				Err:     err,
			}
		}
		return batch, nil
	})
}

// fetchNotes returns every non-system-filtered comment note of the given
// merge request via GET /projects/{projectPath}/merge_requests/{prNumber}/notes,
// requesting activity_filter=only_comments and sort=asc.
//
// A decode failure returns a [*domain.SCMError] of kind
// [domain.ErrSCMPayload].
func (a *GitLabSCMAdapter) fetchNotes(ctx context.Context, prNumber int, owner, repo string) ([]gitlabNote, error) {
	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber) + "/notes"
	params := url.Values{"activity_filter": {"only_comments"}, "sort": {"asc"}}

	return paginateSCM(ctx, a.client, a.log, path, params, func(body []byte) ([]gitlabNote, error) {
		var batch []gitlabNote
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse notes response",
				Err:     err,
			}
		}
		return batch, nil
	})
}

// resolveBotFlag reports whether userID belongs to a platform bot
// account, consulting and populating the adapter-lifetime cache shared
// by [GitLabSCMAdapter.FetchPendingReviews] and
// [GitLabSCMAdapter.FetchBotReviewComments].
//
// userID <= 0 resolves to false with no request. A GET /users/:id
// failure of kind [domain.ErrSCMNotFound] caches and returns false, nil;
// any other lookup failure is returned unconverted-further and is never
// cached, so a route that recovers reclassifies the user on the next
// call.
func (a *GitLabSCMAdapter) resolveBotFlag(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}

	a.mu.Lock()
	cached, hit := a.botCache[userID]
	a.mu.Unlock()
	if hit {
		return cached, nil
	}

	path := "/users/" + strconv.FormatInt(userID, 10)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		scmErr := scmcore.AsSCMError(err)
		if scmErr.Kind == domain.ErrSCMNotFound {
			a.mu.Lock()
			a.botCache[userID] = false
			a.mu.Unlock()
			return false, nil
		}
		return false, scmErr
	}

	var detail gitlabUserDetail
	if jsonErr := json.Unmarshal(body, &detail); jsonErr != nil {
		return false, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse user response",
			Err:     jsonErr,
		}
	}

	a.mu.Lock()
	a.botCache[userID] = detail.Bot
	a.mu.Unlock()
	return detail.Bot, nil
}

// normalizeNotes maps notes to [domain.ReviewComment] values.
//
// The current merge-request head SHA is read once, and only when at
// least one note carries a diff position, because only such a note can
// be outdated. Outdated is true only when the note's position carries a
// non-empty head_sha that differs from the current one; EndLine stays
// zero for every comment, since GitLab's position describes one line.
func (a *GitLabSCMAdapter) normalizeNotes(ctx context.Context, prNumber int, owner, repo string, notes []gitlabNote) ([]domain.ReviewComment, error) {
	headSHA := ""
	needsHeadSHA := false
	for _, n := range notes {
		if n.Position != nil {
			needsHeadSHA = true
			break
		}
	}
	if needsHeadSHA {
		mr, err := a.fetchMergeRequest(ctx, prNumber, owner, repo)
		if err != nil {
			return nil, err
		}
		headSHA = mr.SHA
	}

	out := make([]domain.ReviewComment, 0, len(notes))
	for _, n := range notes {
		comment := domain.ReviewComment{
			ID:          strconv.FormatInt(n.ID, 10),
			Reviewer:    n.Author.Username,
			Body:        n.Body,
			SubmittedAt: scmcore.ParseTimestampOrZero(n.CreatedAt),
		}

		if n.Position != nil {
			comment.FilePath = n.Position.NewPath
			if comment.FilePath == "" {
				comment.FilePath = n.Position.OldPath
			}
			switch {
			case n.Position.NewLine != nil:
				comment.StartLine = *n.Position.NewLine
			case n.Position.OldLine != nil:
				comment.StartLine = *n.Position.OldLine
			}
			comment.Outdated = headSHA != "" && n.Position.HeadSHA != "" && n.Position.HeadSHA != headSHA
		}

		out = append(out, comment)
	}
	return out, nil
}

// FetchPendingReviews returns the review comments of non-bot
// requested_changes reviewers on the given merge request.
//
// A reviewer whose platform bot flag cannot be resolved for any reason
// other than a deleted account is treated as not-a-bot, logged at WARN,
// and the read continues; the notes of every excluded reviewer are
// dropped along with the reviewer. Returns an empty non-nil slice with
// no notes request when no reviewer survives the exclusion.
func (a *GitLabSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	reviewers, err := a.fetchReviewers(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	blocking := make(map[string]struct{})
	for _, r := range reviewers {
		if r.State != "requested_changes" {
			continue
		}

		platformBot, botErr := a.resolveBotFlag(ctx, r.User.ID)
		if botErr != nil {
			a.log.Warn("reviewer bot flag unresolved, treating reviewer as not a bot",
				slog.Int64("user_id", r.User.ID),
				slog.Any("error", botErr))
			platformBot = false
		}
		if scmcore.IsBotAuthor(r.User.Username, platformBot, nil) {
			continue
		}
		blocking[r.User.Username] = struct{}{}
	}

	if len(blocking) == 0 {
		return []domain.ReviewComment{}, nil
	}

	notes, err := a.fetchNotes(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	selected := make([]gitlabNote, 0, len(notes))
	for _, n := range notes {
		if n.System {
			continue
		}
		if _, ok := blocking[n.Author.Username]; !ok {
			continue
		}
		selected = append(selected, n)
	}

	return a.normalizeNotes(ctx, prNumber, owner, repo, selected)
}

// FetchBotReviewComments returns the review comments authored by
// allowlisted or platform-flagged bot accounts on the given merge
// request, with no review-state filter.
//
// An allowlisted author is selected with no lookup. Any other
// non-allowlisted author is classified through
// [GitLabSCMAdapter.resolveBotFlag]; unlike
// [GitLabSCMAdapter.FetchPendingReviews], a lookup failure here aborts
// the method and returns the error unchanged, because the lookup widens
// a bot-comment set and a failed lookup silently dropping members is
// worse than aborting.
func (a *GitLabSCMAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	notes, err := a.fetchNotes(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	selected := make([]gitlabNote, 0, len(notes))
	for _, n := range notes {
		if n.System {
			continue
		}
		if scmcore.IsBotAuthor(n.Author.Username, false, botUsernames) {
			selected = append(selected, n)
			continue
		}

		platformBot, botErr := a.resolveBotFlag(ctx, n.Author.ID)
		if botErr != nil {
			return nil, botErr
		}
		if scmcore.IsBotAuthor(n.Author.Username, platformBot, botUsernames) {
			selected = append(selected, n)
		}
	}

	return a.normalizeNotes(ctx, prNumber, owner, repo, selected)
}

// GetReviewDecision returns the aggregated review decision for the
// given merge request, folding the reviewer states and the approvals
// payload.
//
// A requested_changes reviewer short-circuits to
// [domain.ReviewDecisionChangesRequested] before the approvals read,
// which is behavior-identical to the full evaluation because that arm
// returns unconditionally; a later approval never clears an outstanding
// block. No bot exclusion is applied at any arm: the merge gate asks
// whether an outstanding block exists, and a bot's block is a block.
func (a *GitLabSCMAdapter) GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error) {
	reviewers, err := a.fetchReviewers(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}
	for _, r := range reviewers {
		if r.State == "requested_changes" {
			return domain.ReviewDecisionChangesRequested, nil
		}
	}

	path := "/projects/" + projectPath(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber) + "/approvals"
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		return "", scmcore.AsSCMError(err)
	}

	var approvals gitlabApprovals
	if jsonErr := json.Unmarshal(body, &approvals); jsonErr != nil {
		return "", &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse approvals response",
			Err:     jsonErr,
		}
	}

	if approvals.Approved {
		return domain.ReviewDecisionApproved, nil
	}
	if len(reviewers) > 0 {
		return domain.ReviewDecisionReviewRequired, nil
	}
	return domain.ReviewDecisionNotRequired, nil
}
