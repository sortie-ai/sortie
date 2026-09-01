package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/typeutil"
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
	ID                  int64  `json:"id"`
	PullRequestReviewID *int64 `json:"pull_request_review_id"`
	Path                string `json:"path"`
	StartLine           *int   `json:"start_line"`
	OriginalStartLine   *int   `json:"original_start_line"`
	Line                *int   `json:"line"`
	OriginalLine        *int   `json:"original_line"`
	SubjectType         string `json:"subject_type"`
	Body                string `json:"body"`
	User                struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// GitHubSCMAdapter implements [domain.SCMAdapter] for the GitHub Pull
// Request Reviews API. Safe for concurrent use.
type GitHubSCMAdapter struct {
	client        *httpkit.Client
	graphqlClient *httpkit.Client
	baseURL       string
}

// NewGitHubSCMAdapter creates a [GitHubSCMAdapter] from adapter-specific
// configuration. Required config key: "api_key". Optional: "endpoint"
// (defaults to https://api.github.com; a value that does not parse as an
// absolute http or https URL with a host returns a [*domain.SCMError] of
// kind [domain.ErrSCMPayload]), "user_agent".
func NewGitHubSCMAdapter(adapterConfig map[string]any) (domain.SCMAdapter, error) {
	apiKey, fault := typeutil.StringField(adapterConfig, "api_key")
	if fault != nil {
		return nil, &domain.SCMError{Kind: domain.ErrSCMPayload, Message: fault.Error()}
	}
	if apiKey == "" {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMAuth,
			Message: "missing required config key: api_key",
		}
	}

	endpointRaw, fault := typeutil.StringField(adapterConfig, "endpoint")
	if fault != nil {
		return nil, &domain.SCMError{Kind: domain.ErrSCMPayload, Message: fault.Error()}
	}
	endpoint, redactedEndpoint, endpointOK := resolveEndpoint(endpointRaw)
	if !endpointOK {
		return nil, &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: fmt.Sprintf("github: endpoint %q is not a valid absolute http(s) url", redactedEndpoint),
		}
	}

	userAgent, fault := typeutil.StringField(adapterConfig, "user_agent")
	if fault != nil {
		return nil, &domain.SCMError{Kind: domain.ErrSCMPayload, Message: fault.Error()}
	}
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	adapter := &GitHubSCMAdapter{
		client:  newGitHubClient(endpoint, apiKey, userAgent),
		baseURL: endpoint,
	}
	adapter.graphqlClient = newGitHubClient(adapter.graphqlBasePath(), apiKey, userAgent)
	return adapter, nil
}

// FetchPendingReviews returns review comments from non-bot
// CHANGES_REQUESTED reviews on the given PR. Outdated line comments
// (where GitHub no longer reports a current line) have Outdated=true.
// Returns an empty non-nil slice when no matching reviews exist.
//
// SubmittedAt is parsed via [scmcore.ParseTimestampOrZero]: a review or
// comment timestamp that is not RFC 3339 normalizes to the zero time
// rather than failing the read, since SubmittedAt drives debounce only.
func (a *GitHubSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	reviews, err := a.fetchAllReviews(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	selected := make([]githubReview, 0, len(reviews))
	for _, review := range reviews {
		if !strings.EqualFold(review.State, "CHANGES_REQUESTED") {
			continue
		}
		if strings.EqualFold(review.User.Type, "Bot") {
			continue
		}
		selected = append(selected, review)
	}

	return a.collectReviewComments(ctx, prNumber, owner, repo, selected, func(comment githubReviewComment) bool {
		return !strings.EqualFold(comment.User.Type, "Bot")
	})
}

// FetchBotReviewComments returns review comments authored by automated
// review bots on the given PR. A review or comment is bot-authored when
// its user.type is "Bot" or its author login matches a botUsernames entry
// case-insensitively. Outdated line comments (where GitHub no longer
// reports a current line) have Outdated=true. Returns an empty non-nil
// slice when no bot comments exist.
//
// Unlike [GitHubSCMAdapter.FetchPendingReviews], no review-state filter is
// applied: review bots commonly post COMMENTED reviews, so requiring
// CHANGES_REQUESTED would drop most bot findings.
//
// SubmittedAt is parsed via [scmcore.ParseTimestampOrZero]: a review or
// comment timestamp that is not RFC 3339 normalizes to the zero time
// rather than failing the read, since SubmittedAt drives debounce only.
func (a *GitHubSCMAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	reviews, err := a.fetchAllReviews(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	selected := make([]githubReview, 0, len(reviews))
	for _, review := range reviews {
		if !scmcore.IsBotAuthor(review.User.Login, strings.EqualFold(review.User.Type, "Bot"), botUsernames) {
			continue
		}
		selected = append(selected, review)
	}

	return a.collectReviewComments(ctx, prNumber, owner, repo, selected, func(comment githubReviewComment) bool {
		return scmcore.IsBotAuthor(comment.User.Login, strings.EqualFold(comment.User.Type, "Bot"), botUsernames)
	})
}

func (a *GitHubSCMAdapter) collectReviewComments(
	ctx context.Context,
	prNumber int,
	owner, repo string,
	reviews []githubReview,
	includeComment func(githubReviewComment) bool,
) ([]domain.ReviewComment, error) {
	result := make([]domain.ReviewComment, 0)
	if len(reviews) == 0 {
		return result, nil
	}

	comments, err := a.fetchAllReviewComments(ctx, prNumber, owner, repo)
	if err != nil {
		return nil, err
	}

	commentsByReview := make(map[int64][]githubReviewComment)
	for _, comment := range comments {
		if comment.PullRequestReviewID == nil {
			continue
		}
		reviewID := *comment.PullRequestReviewID
		commentsByReview[reviewID] = append(commentsByReview[reviewID], comment)
	}

	seen := make(map[string]struct{})
	for _, review := range reviews {
		body := strings.TrimSpace(review.Body)
		if body != "" {
			prCommentID := fmt.Sprintf("review-%d", review.ID)
			if _, dup := seen[prCommentID]; !dup {
				seen[prCommentID] = struct{}{}
				result = append(result, domain.ReviewComment{
					ID:          prCommentID,
					Reviewer:    review.User.Login,
					Body:        body,
					SubmittedAt: scmcore.ParseTimestampOrZero(review.SubmittedAt),
				})
			}
		}

		for _, comment := range commentsByReview[review.ID] {
			if !includeComment(comment) {
				continue
			}

			commentID := strconv.FormatInt(comment.ID, 10)
			if _, dup := seen[commentID]; dup {
				continue
			}
			seen[commentID] = struct{}{}
			result = append(result, normalizeReviewComment(comment))
		}
	}

	return result, nil
}

func normalizeReviewComment(comment githubReviewComment) domain.ReviewComment {
	startLine, endLine := reviewCommentLines(comment)
	return domain.ReviewComment{
		ID:          strconv.FormatInt(comment.ID, 10),
		FilePath:    comment.Path,
		StartLine:   startLine,
		EndLine:     endLine,
		Reviewer:    comment.User.Login,
		Body:        comment.Body,
		SubmittedAt: scmcore.ParseTimestampOrZero(comment.CreatedAt),
		Outdated:    isOutdatedReviewComment(comment),
	}
}

func reviewCommentLines(comment githubReviewComment) (int, int) {
	if strings.EqualFold(comment.SubjectType, "file") {
		return 0, 0
	}

	var startLine, endLine int
	if comment.Line != nil {
		endLine = *comment.Line
		startLine = endLine
		if comment.StartLine != nil {
			startLine = *comment.StartLine
		}
	} else if comment.OriginalLine != nil {
		endLine = *comment.OriginalLine
		startLine = endLine
		if comment.OriginalStartLine != nil {
			startLine = *comment.OriginalStartLine
		}
	}

	if startLine == endLine {
		endLine = 0
	}
	return startLine, endLine
}

func isOutdatedReviewComment(comment githubReviewComment) bool {
	if strings.EqualFold(comment.SubjectType, "file") {
		return false
	}
	return comment.Line == nil && (comment.OriginalLine != nil || strings.EqualFold(comment.SubjectType, "line"))
}

func (a *GitHubSCMAdapter) fetchAllReviews(ctx context.Context, prNumber int, owner, repo string) ([]githubReview, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	params := url.Values{"per_page": {"100"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]githubReview, error) {
		var batch []githubReview
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse reviews response",
				Err:     err,
			}
		}
		return batch, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxReviewPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "reviews response truncated at page limit",
				slog.String("owner", owner),
				slog.String("repo", repo),
				slog.Int("pr_number", prNumber),
				slog.Int("max_pages", limit))
		},
	})

	all, err := paginator.All(ctx)
	if err != nil {
		return nil, scmcore.AsSCMError(err)
	}
	return all, nil
}

func (a *GitHubSCMAdapter) fetchAllReviewComments(ctx context.Context, prNumber int, owner, repo string) ([]githubReviewComment, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments",
		url.PathEscape(owner), url.PathEscape(repo), prNumber)
	params := url.Values{"per_page": {"100"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]githubReviewComment, error) {
		var batch []githubReviewComment
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &domain.SCMError{
				Kind:    domain.ErrSCMPayload,
				Message: "failed to parse review comments response",
				Err:     err,
			}
		}
		return batch, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxReviewPages,
		OnLimitReached: func(limit int) {
			slog.WarnContext(ctx, "review comments response truncated at page limit",
				slog.String("owner", owner),
				slog.String("repo", repo),
				slog.Int("pr_number", prNumber),
				slog.Int("max_pages", limit))
		},
	})

	all, err := paginator.All(ctx)
	if err != nil {
		return nil, scmcore.AsSCMError(err)
	}
	return all, nil
}
