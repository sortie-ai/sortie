package gitea

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// giteaReview is one entry from the pull request reviews route. Dismissed marks
// a review an operator nullified, which the fold and the comment collection both
// skip; official and stale are decoded for parity with the wire shape but not
// gated on. The state enum uses Gitea's REQUEST_CHANGES spelling, not GitHub's
// CHANGES_REQUESTED.
type giteaReview struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	User        giteaUser `json:"user"`
	Dismissed   bool      `json:"dismissed"`
	Official    bool      `json:"official"`
	Stale       bool      `json:"stale"`
	SubmittedAt string    `json:"submitted_at"`
}

// giteaReviewComment is one inline comment from a review. Position is the
// new-side file line and OriginalPosition the old-side file line; the side a
// comment is not anchored to reads zero, and both read zero for a
// file-scoped comment. There is no end-line field, so comments are
// single-line.
type giteaReviewComment struct {
	ID               int64     `json:"id"`
	Path             string    `json:"path"`
	Body             string    `json:"body"`
	Position         int       `json:"position"`
	OriginalPosition int       `json:"original_position"`
	User             giteaUser `json:"user"`
	CreatedAt        string    `json:"created_at"`
}

// FetchPendingReviews returns the review comments of REQUEST_CHANGES reviews
// on the given PR.
//
// Each retained review contributes its trimmed body as a PR-level comment when
// non-empty, followed by its inline comments passing the same predicate.
// Dismissed reviews are skipped, and comments are deduplicated by id. The
// returned slice is non-nil even when empty; a failure returns a
// [*domain.SCMError]. An inline comment carries the file line of the diff
// side it is anchored to, and no comment is reported outdated.
//
// The predicate nominally excludes bot-authored reviews and comments, but has
// no effect on Gitea: there is no platform bot marker and this method passes
// no allowlist, so a bot's REQUEST_CHANGES review is not excluded here. The
// predicate is retained for structural parity with
// [GiteaSCMAdapter.FetchBotReviewComments].
func (a *GiteaSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	return a.collectReviewComments(ctx, prNumber, owner, repo,
		func(r giteaReview) bool {
			return r.State == "REQUEST_CHANGES" && !scmcore.IsBotAuthor(r.User.Login, false, nil)
		},
		func(c giteaReviewComment) bool {
			return !scmcore.IsBotAuthor(c.User.Login, false, nil)
		})
}

// FetchBotReviewComments returns the review comments authored by allowlisted bot
// accounts on the given PR, with no review-state filter.
//
// Gitea users carry no bot-type marker, so classification reduces to the
// botUsernames allowlist under a case-insensitive comparison; a nil or empty
// allowlist selects nothing. A review is retained when its author is
// allowlisted, and each inline comment is retained only when its own author is
// allowlisted, so a non-allowlisted reply inside a bot review is excluded.
// Dismissed reviews are skipped and comments are deduplicated by id. The
// returned slice is non-nil even when empty; a failure returns a
// [*domain.SCMError]. An inline comment carries the file line of the diff
// side it is anchored to, and no comment is reported outdated.
func (a *GiteaSCMAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	return a.collectReviewComments(ctx, prNumber, owner, repo,
		func(r giteaReview) bool {
			return scmcore.IsBotAuthor(r.User.Login, false, botUsernames)
		},
		func(c giteaReviewComment) bool {
			return scmcore.IsBotAuthor(c.User.Login, false, botUsernames)
		})
}

// GetReviewDecision returns the aggregated review decision for the given PR.
//
// Gitea exposes no aggregate decision, so the adapter folds the per-review list
// together with the PR object's requested-reviewers signal. Dismissed reviews do
// not contribute. A failure on either read returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error) {
	reviews, err := a.fetchAllReviews(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}

	pr, err := a.fetchPullRequest(ctx, prNumber, owner, repo)
	if err != nil {
		return "", err
	}

	decision, err := foldReviewDecision(reviews, len(pr.RequestedReviewers) > 0)
	if err != nil {
		return "", err
	}
	return decision, nil
}

// collectReviewComments fetches every review, retains those passing keepReview
// (dismissed reviews are always skipped first), and returns the retained
// reviews' bodies together with the inline comments passing keepComment as
// [domain.ReviewComment] values.
//
// A retained review's trimmed body becomes a PR-level comment with id
// "review-<id>" when non-empty; its inline comments follow, each admitted only
// when keepComment reports true, so a review-level match does not force in an
// inline comment authored by someone else. Comments are deduplicated by id
// across all reviews. The returned slice is non-nil even when empty; a fetch
// failure returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) collectReviewComments(ctx context.Context, prNumber int, owner, repo string, keepReview func(giteaReview) bool, keepComment func(giteaReviewComment) bool) ([]domain.ReviewComment, error) {
	reviews, err := a.fetchAllReviews(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	result := make([]domain.ReviewComment, 0)

	for _, r := range reviews {
		if r.Dismissed {
			continue
		}
		if !keepReview(r) {
			continue
		}

		body := strings.TrimSpace(r.Body)
		if body != "" {
			prCommentID := "review-" + strconv.FormatInt(r.ID, 10)
			if _, dup := seen[prCommentID]; !dup {
				seen[prCommentID] = struct{}{}
				result = append(result, domain.ReviewComment{
					ID:          prCommentID,
					Reviewer:    r.User.Login,
					Body:        body,
					SubmittedAt: scmcore.ParseTimestampOrZero(r.SubmittedAt),
				})
			}
		}

		comments, fetchErr := a.fetchReviewComments(ctx, prNumber, owner, repo, r.ID)
		if fetchErr != nil {
			return nil, fetchErr
		}

		for _, c := range comments {
			if !keepComment(c) {
				continue
			}
			commentID := strconv.FormatInt(c.ID, 10)
			if _, dup := seen[commentID]; dup {
				continue
			}
			seen[commentID] = struct{}{}
			result = append(result, normalizeReviewComment(c))
		}
	}

	return result, nil
}

// normalizeReviewComment maps a [giteaReviewComment] to a [domain.ReviewComment].
//
// StartLine takes Position when it is positive, else OriginalPosition when
// that is positive, else zero; when both are positive the new side wins, so
// the mapping stays total over every integer pair. EndLine stays zero
// because Gitea review comments are single-line. Outdated is never set: the
// route carries no invalidation signal, and its only candidate, the
// comment's stored commit identifier, is fixed when the comment is written
// and does not track a later push.
func normalizeReviewComment(c giteaReviewComment) domain.ReviewComment {
	startLine := 0
	switch {
	case c.Position > 0:
		startLine = c.Position
	case c.OriginalPosition > 0:
		startLine = c.OriginalPosition
	}

	return domain.ReviewComment{
		ID:          strconv.FormatInt(c.ID, 10),
		FilePath:    c.Path,
		StartLine:   startLine,
		Reviewer:    c.User.Login,
		Body:        c.Body,
		SubmittedAt: scmcore.ParseTimestampOrZero(c.CreatedAt),
	}
}

// giteaContributingReview pairs a decision-bearing [giteaReview] with its
// parsed submission time, so the ordering timestamp is parsed once, ahead
// of the sort that depends on it.
type giteaContributingReview struct {
	review giteaReview
	at     time.Time
}

// foldReviewDecision aggregates the per-review states with the PR's
// requested-reviewers signal into one [domain.ReviewDecision].
//
// Dismissed reviews, and any review whose state is not APPROVED or
// REQUEST_CHANGES, are filtered out before the parse; only a review that can
// change the verdict is parsed, so a malformed timestamp on a review that
// cannot change the verdict never fails the read. A retained review's
// submitted_at is ordered then id so the latest per reviewer supersedes
// their earlier ones. A retained review whose submitted_at does not parse
// as RFC 3339 fails with a [*domain.SCMError] of Kind
// [domain.ErrSCMPayload]. Any standing REQUEST_CHANGES yields
// changes-requested; otherwise any APPROVED yields approved; otherwise
// reviewRequested yields review-required; otherwise not-required.
func foldReviewDecision(reviews []giteaReview, reviewRequested bool) (domain.ReviewDecision, error) {
	contributing := make([]giteaContributingReview, 0, len(reviews))
	for _, r := range reviews {
		if r.Dismissed {
			continue
		}
		if r.State != "APPROVED" && r.State != "REQUEST_CHANGES" {
			continue
		}

		at, err := scmcore.ParseTimestamp("review submitted_at", r.SubmittedAt)
		if err != nil {
			return "", err
		}
		contributing = append(contributing, giteaContributingReview{review: r, at: at})
	}

	slices.SortFunc(contributing, func(a, b giteaContributingReview) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return cmp.Compare(a.review.ID, b.review.ID)
	})

	latestDecision := make(map[string]string)
	for _, pair := range contributing {
		latestDecision[pair.review.User.Login] = pair.review.State
	}

	anyChangesRequested := false
	anyApproved := false
	for _, decision := range latestDecision {
		switch decision {
		case "REQUEST_CHANGES":
			anyChangesRequested = true
		case "APPROVED":
			anyApproved = true
		}
	}

	if anyChangesRequested {
		return domain.ReviewDecisionChangesRequested, nil
	}
	if anyApproved {
		return domain.ReviewDecisionApproved, nil
	}
	if reviewRequested {
		return domain.ReviewDecisionReviewRequired, nil
	}
	return domain.ReviewDecisionNotRequired, nil
}

// fetchAllReviews returns every review on the given PR, following page-number
// pagination. A decode failure yields a [*domain.SCMError] of kind
// [domain.ErrSCMPayload].
func (a *GiteaSCMAdapter) fetchAllReviews(ctx context.Context, prNumber int, owner, repo string) ([]giteaReview, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)

	return paginateSCM(ctx, a.client, path, func(body []byte) ([]giteaReview, error) {
		var batch []giteaReview
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse reviews response",
				Err:     err,
			}
		}
		return batch, nil
	})
}

// fetchReviewComments returns every inline comment of the given review,
// following page-number pagination. A decode failure yields a [*domain.SCMError]
// of kind [domain.ErrSCMPayload].
func (a *GiteaSCMAdapter) fetchReviewComments(ctx context.Context, prNumber int, owner, repo string, reviewID int64) ([]giteaReviewComment, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments",
		url.PathEscape(owner), url.PathEscape(repo), prNumber, reviewID)

	return paginateSCM(ctx, a.client, path, func(body []byte) ([]giteaReviewComment, error) {
		var batch []giteaReviewComment
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse review comments response",
				Err:     err,
			}
		}
		return batch, nil
	})
}
