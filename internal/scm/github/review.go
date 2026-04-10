package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.SCMAdapters.Register("github", NewGitHubSCMAdapter)
}

var _ domain.SCMAdapter = (*GitHubSCMAdapter)(nil)

// maxReviewPages is the maximum number of pagination pages fetched for
// reviews and review comments to prevent unbounded API calls.
const maxReviewPages = 20

type githubReview struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Body  string `json:"body"`
	User  struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

type githubReviewComment struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	StartLine *int   `json:"start_line"`
	Line      *int   `json:"line"`
	Position  *int   `json:"position"`
	Body      string `json:"body"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// GitHubSCMAdapter implements [domain.SCMAdapter] for the GitHub Pull
// Request Reviews API. Safe for concurrent use.
type GitHubSCMAdapter struct {
	client *githubClient
}

// NewGitHubSCMAdapter creates a [GitHubSCMAdapter] from adapter-specific
// configuration. Required config key: "api_key". Optional: "endpoint"
// (defaults to https://api.github.com), "user_agent".
func NewGitHubSCMAdapter(adapterConfig map[string]any) (domain.SCMAdapter, error) {
	apiKey, _ := adapterConfig["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMAuth,
			Message: "missing required config key: api_key",
		}
	}

	endpoint, _ := adapterConfig["endpoint"].(string)
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	userAgent, _ := adapterConfig["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &GitHubSCMAdapter{
		client: newGitHubClient(endpoint, apiKey, userAgent),
	}, nil
}

// FetchPendingReviews returns review comments from non-bot
// CHANGES_REQUESTED reviews on the given PR. Outdated comments (where
// GitHub reports position as null) have Outdated=true. Returns an empty
// non-nil slice when no matching reviews exist.
func (a *GitHubSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	reviews, err := a.fetchAllReviews(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var result []domain.ReviewComment

	for _, review := range reviews {
		if !strings.EqualFold(review.State, "CHANGES_REQUESTED") {
			continue
		}
		if strings.EqualFold(review.User.Type, "Bot") {
			continue
		}

		// Include non-empty review body as a PR-level comment.
		body := strings.TrimSpace(review.Body)
		if body != "" {
			prCommentID := fmt.Sprintf("review-%d", review.ID)
			if _, dup := seen[prCommentID]; !dup {
				seen[prCommentID] = struct{}{}
				submittedAt, _ := time.Parse(time.RFC3339, review.SubmittedAt)
				result = append(result, domain.ReviewComment{
					ID:          prCommentID,
					Reviewer:    review.User.Login,
					Body:        body,
					SubmittedAt: submittedAt,
				})
			}
		}

		// Fetch inline comments for this review.
		comments, fetchErr := a.fetchReviewComments(ctx, prNumber, owner, repo, review.ID)
		if fetchErr != nil {
			return nil, fetchErr
		}

		for _, c := range comments {
			if strings.EqualFold(c.User.Type, "Bot") {
				continue
			}

			commentID := strconv.FormatInt(c.ID, 10)
			if _, dup := seen[commentID]; dup {
				continue
			}
			seen[commentID] = struct{}{}

			startLine := 0
			if c.StartLine != nil {
				startLine = *c.StartLine
			} else if c.Line != nil {
				startLine = *c.Line
			}

			endLine := 0
			if c.Line != nil && *c.Line != startLine {
				endLine = *c.Line
			}

			createdAt, _ := time.Parse(time.RFC3339, c.CreatedAt)

			result = append(result, domain.ReviewComment{
				ID:          commentID,
				FilePath:    c.Path,
				StartLine:   startLine,
				EndLine:     endLine,
				Reviewer:    c.User.Login,
				Body:        c.Body,
				SubmittedAt: createdAt,
				Outdated:    c.Position == nil,
			})
		}
	}

	if result == nil {
		result = []domain.ReviewComment{}
	}
	return result, nil
}

func (a *GitHubSCMAdapter) fetchAllReviews(ctx context.Context, prNumber int, owner, repo string) ([]githubReview, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	params := url.Values{"per_page": {"100"}}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		return nil, toSCMError(err)
	}

	var all []githubReview
	for page := 0; page < maxReviewPages; page++ {
		var batch []githubReview
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse reviews response",
				Err:     err,
			}
		}
		all = append(all, batch...)

		if nextURL == "" {
			break
		}

		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, toSCMError(err)
		}
	}

	if nextURL != "" {
		slog.WarnContext(ctx, "reviews response truncated at page limit",
			slog.Int("pages_fetched", len(all)/100+1),
			slog.Int("max_pages", maxReviewPages))
	}

	return all, nil
}

func (a *GitHubSCMAdapter) fetchReviewComments(ctx context.Context, prNumber int, owner, repo string, reviewID int64) ([]githubReviewComment, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments",
		url.PathEscape(owner), url.PathEscape(repo), prNumber, reviewID)
	params := url.Values{"per_page": {"100"}}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		return nil, toSCMError(err)
	}

	var all []githubReviewComment
	for page := 0; page < maxReviewPages; page++ {
		var batch []githubReviewComment
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse review comments response",
				Err:     err,
			}
		}
		all = append(all, batch...)

		if nextURL == "" {
			break
		}

		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, toSCMError(err)
		}
	}

	if nextURL != "" {
		slog.WarnContext(ctx, "review comments response truncated at page limit",
			slog.Int64("review_id", reviewID),
			slog.Int("max_pages", maxReviewPages))
	}

	return all, nil
}

// toSCMError converts a TrackerError (from the shared githubClient) to
// the SCMError type. Non-TrackerError errors are wrapped as transport
// errors.
func toSCMError(err error) *domain.SCMError {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return &domain.SCMError{
			Kind:    domain.ErrSCMTransport,
			Message: err.Error(),
			Err:     err,
		}
	}

	kindMap := map[domain.TrackerErrorKind]domain.SCMErrorKind{
		domain.ErrTrackerTransport: domain.ErrSCMTransport,
		domain.ErrTrackerAuth:      domain.ErrSCMAuth,
		domain.ErrTrackerAPI:       domain.ErrSCMAPI,
		domain.ErrTrackerNotFound:  domain.ErrSCMNotFound,
		domain.ErrTrackerPayload:   domain.ErrSCMPayload,
	}

	scmKind, ok := kindMap[te.Kind]
	if !ok {
		scmKind = domain.ErrSCMAPI
	}

	return &domain.SCMError{
		Kind:    scmKind,
		Message: te.Message,
		Err:     err,
	}
}
