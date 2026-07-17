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

	"github.com/sortie-ai/sortie/internal/domain"
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

// giteaReviewComment is one inline comment from a review. Position is the line
// on the current diff; a zero Position marks a comment whose anchor a later push
// removed, in which case OriginalPosition holds the line it was written against.
// There is no end-line field, so comments are single-line.
type giteaReviewComment struct {
	ID               int64     `json:"id"`
	Path             string    `json:"path"`
	Body             string    `json:"body"`
	Position         int       `json:"position"`
	OriginalPosition int       `json:"original_position"`
	User             giteaUser `json:"user"`
	CreatedAt        string    `json:"created_at"`
}

// FetchPendingReviews returns the review comments of non-bot REQUEST_CHANGES
// reviews on the given PR.
//
// Each retained review contributes its trimmed body as a PR-level comment when
// non-empty, followed by its inline comments. Dismissed reviews are skipped, and
// comments are deduplicated by id. The returned slice is non-nil even when
// empty; a failure returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	return a.collectReviewComments(ctx, prNumber, owner, repo, func(r giteaReview) bool {
		return r.State == "REQUEST_CHANGES" && !isBotAuthor(r.User.Login, nil)
	})
}

// FetchBotReviewComments returns the review comments authored by allowlisted bot
// accounts on the given PR, with no review-state filter.
//
// Gitea users carry no bot-type marker, so classification reduces to the
// botUsernames allowlist under a case-insensitive comparison; a nil or empty
// allowlist selects nothing. Dismissed reviews are skipped and comments are
// deduplicated by id. The returned slice is non-nil even when empty; a failure
// returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	return a.collectReviewComments(ctx, prNumber, owner, repo, func(r giteaReview) bool {
		return isBotAuthor(r.User.Login, botUsernames)
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

	return foldReviewDecision(reviews, len(pr.RequestedReviewers) > 0), nil
}

// collectReviewComments fetches every review, retains those passing keepReview
// (dismissed reviews are always skipped first), and returns their body and
// inline comments as [domain.ReviewComment] values.
//
// A retained review's trimmed body becomes a PR-level comment with id
// "review-<id>" when non-empty; its inline comments follow. Comments are
// deduplicated by id across all reviews. The returned slice is non-nil even when
// empty; a fetch failure returns a [*domain.SCMError].
func (a *GiteaSCMAdapter) collectReviewComments(ctx context.Context, prNumber int, owner, repo string, keepReview func(giteaReview) bool) ([]domain.ReviewComment, error) {
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
					SubmittedAt: parseUTC(r.SubmittedAt),
				})
			}
		}

		comments, fetchErr := a.fetchReviewComments(ctx, prNumber, owner, repo, r.ID)
		if fetchErr != nil {
			return nil, fetchErr
		}

		for _, c := range comments {
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
// A zero Position marks an outdated comment whose anchor a later push removed;
// its line falls back to OriginalPosition. EndLine stays zero because Gitea
// review comments are single-line.
func normalizeReviewComment(c giteaReviewComment) domain.ReviewComment {
	outdated := c.Position == 0
	startLine := c.Position
	if outdated {
		startLine = c.OriginalPosition
	}

	return domain.ReviewComment{
		ID:          strconv.FormatInt(c.ID, 10),
		FilePath:    c.Path,
		StartLine:   startLine,
		Reviewer:    c.User.Login,
		Body:        c.Body,
		SubmittedAt: parseUTC(c.CreatedAt),
		Outdated:    outdated,
	}
}

// isBotAuthor reports whether login matches an entry in botUsernames under a
// case-insensitive comparison. Gitea carries no platform bot marker, so a nil or
// empty allowlist selects nothing.
func isBotAuthor(login string, botUsernames []string) bool {
	return slices.ContainsFunc(botUsernames, func(name string) bool {
		return strings.EqualFold(login, name)
	})
}

// foldReviewDecision aggregates the per-review states with the PR's
// requested-reviewers signal into one [domain.ReviewDecision].
//
// Dismissed reviews are skipped. Reviews are ordered by submission time then id
// so the latest APPROVED or REQUEST_CHANGES per reviewer supersedes their earlier
// ones; COMMENT, PENDING, and REQUEST_REVIEW states are not decisions. Any
// standing REQUEST_CHANGES yields changes-requested; otherwise any APPROVED
// yields approved; otherwise reviewRequested yields review-required; otherwise
// not-required.
func foldReviewDecision(reviews []giteaReview, reviewRequested bool) domain.ReviewDecision {
	slices.SortFunc(reviews, func(a, b giteaReview) int {
		if c := parseUTC(a.SubmittedAt).Compare(parseUTC(b.SubmittedAt)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	latestDecision := make(map[string]string)
	for _, r := range reviews {
		if r.Dismissed {
			continue
		}
		switch r.State {
		case "APPROVED":
			latestDecision[r.User.Login] = "APPROVED"
		case "REQUEST_CHANGES":
			latestDecision[r.User.Login] = "REQUEST_CHANGES"
		}
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
		return domain.ReviewDecisionChangesRequested
	}
	if anyApproved {
		return domain.ReviewDecisionApproved
	}
	if reviewRequested {
		return domain.ReviewDecisionReviewRequired
	}
	return domain.ReviewDecisionNotRequired
}

// fetchAllReviews returns every review on the given PR, following page-number
// pagination. A decode failure yields a [*domain.SCMError] of kind
// [domain.ErrSCMPayload].
func (a *GiteaSCMAdapter) fetchAllReviews(ctx context.Context, prNumber int, owner, repo string) ([]giteaReview, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)

	return paginatePages(ctx, a.client, path, func(body []byte) ([]giteaReview, error) {
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

	return paginatePages(ctx, a.client, path, func(body []byte) ([]giteaReviewComment, error) {
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
