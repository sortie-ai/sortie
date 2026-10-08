package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessGitHubIntegration skips the test unless SORTIE_GITHUB_TEST=1.
// Tests also require SORTIE_GITHUB_TOKEN and SORTIE_GITHUB_PROJECT to be set.
func skipUnlessGitHubIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_GITHUB_TEST") != "1" {
		t.Skip("skipping GitHub integration test: set SORTIE_GITHUB_TEST=1 to enable")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping GitHub integration test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping GitHub integration test: SORTIE_GITHUB_PROJECT not set")
	}
}

func integrationAdapter(t *testing.T) *GitHubAdapter {
	t.Helper()
	cfg := map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
		"project": os.Getenv("SORTIE_GITHUB_PROJECT"),
	}
	a, err := NewGitHubAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitHubAdapter: %v", err)
	}
	return a.(*GitHubAdapter)
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	a := integrationAdapter(t)
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	t.Logf("fetched %d candidate issues", len(issues))

	for _, iss := range issues {
		if iss.ID == "" {
			t.Errorf("issue has empty ID: %+v", iss)
		}
		if iss.Identifier == "" {
			t.Errorf("issue has empty Identifier: %+v", iss)
		}
		if iss.ID != iss.Identifier {
			t.Errorf("issue %q: ID != Identifier (%q != %q)", iss.ID, iss.ID, iss.Identifier)
		}
		if iss.Title == "" {
			t.Errorf("issue %s has empty Title", iss.ID)
		}
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil in candidate list", iss.ID)
		}
		if iss.BlockedBy == nil {
			t.Errorf("issue %s: BlockedBy should be non-nil", iss.ID)
		}
		if !iss.BlockersUnresolved && len(iss.BlockedBy) != 0 {
			t.Errorf("issue %s: BlockersUnresolved is false with a non-empty BlockedBy, want the dependency summary to have proved this specific issue dependency-free", iss.ID)
		}
		// This integration environment seeds no blocker relation on any
		// candidate, so a resolved candidate here always means the
		// dependency summary proved it has none, not that a real
		// blocker was read and found terminal.
		if iss.Priority != nil {
			t.Errorf("issue %s: Priority should always be nil (GitHub has no native priority)", iss.ID)
		}
		// All labels should be lowercased.
		for _, l := range iss.Labels {
			if l != strings.ToLower(l) {
				t.Errorf("issue %s: label %q is not lowercase", iss.ID, l)
			}
		}
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set; set to a valid issue number")
	}

	a := integrationAdapter(t)
	issue, err := a.FetchIssueByID(context.Background(), issueID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%q): %v", issueID, err)
	}

	t.Logf("fetched issue %s: %q state=%q", issue.ID, issue.Title, issue.State)

	if issue.ID != issueID {
		t.Errorf("ID = %q, want %q", issue.ID, issueID)
	}
	if issue.ID != issue.Identifier {
		t.Errorf("ID != Identifier: %q != %q", issue.ID, issue.Identifier)
	}
	if issue.Title == "" {
		t.Error("Title is empty")
	}
	if issue.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil (may be empty)")
	}
	if issue.BlockersUnresolved {
		t.Error("BlockersUnresolved = true after a successful FetchIssueByID, want false: the by-ID read always resolves blockers")
	}
	if issue.Priority != nil {
		t.Error("Priority should always be nil for GitHub issues")
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set")
	}

	a := integrationAdapter(t)
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	state, ok := result[issueID]
	if !ok {
		t.Fatalf("issue %q not in result map", issueID)
	}
	if state == "" {
		t.Errorf("issue %q has empty state", issueID)
	}
	t.Logf("issue %s state = %q", issueID, state)
}

func TestFetchPendingReviews_Integration(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	prEnv := os.Getenv("SORTIE_GITHUB_PR_NUMBER")
	if prEnv == "" {
		t.Skip("skipping: SORTIE_GITHUB_PR_NUMBER not set; set to a PR number with CHANGES_REQUESTED reviews")
	}

	var prNumber int
	if _, err := fmt.Sscanf(prEnv, "%d", &prNumber); err != nil {
		t.Fatalf("SORTIE_GITHUB_PR_NUMBER=%q is not a valid integer: %v", prEnv, err)
	}

	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("SORTIE_GITHUB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo := parts[0], parts[1]

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}

	comments, err := a.FetchPendingReviews(context.Background(), prNumber, owner, repo)
	if err != nil {
		t.Fatalf("FetchPendingReviews(PR #%d): %v", prNumber, err)
	}

	t.Logf("fetched %d review comments from PR #%d", len(comments), prNumber)

	// Bot comments must be excluded.
	for _, c := range comments {
		if strings.EqualFold(c.Reviewer, "bot") || strings.HasSuffix(c.Reviewer, "[bot]") {
			t.Errorf("bot comment not filtered: reviewer=%q", c.Reviewer)
		}
		if c.ID == "" {
			t.Error("comment has empty ID")
		}
	}
}

func TestFetchPendingReviews_Integration_NotFound(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("SORTIE_GITHUB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo := parts[0], parts[1]

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}

	// PR 999999 is very unlikely to exist.
	_, err = a.FetchPendingReviews(context.Background(), 999999, owner, repo)
	if err == nil {
		t.Skip("PR 999999 unexpectedly exists; skipping not-found assertion")
	}

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("expected *domain.SCMError, got %T: %v", err, err)
	}
	if se.Kind != domain.ErrSCMNotFound {
		t.Errorf("SCMError.Kind = %q, want %q", se.Kind, domain.ErrSCMNotFound)
	}
}

func TestIntegration_VerifyAutoMergeScopes(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	cfg := map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
	}
	a, err := NewGitHubSCMAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}
	scmAdapter := a.(*GitHubSCMAdapter)

	scopes, missing, verifyErr := scmAdapter.VerifyAutoMergeScopes(context.Background(), true)
	if verifyErr != nil {
		if se, ok := errors.AsType[*domain.SCMError](verifyErr); ok {
			t.Logf("VerifyAutoMergeScopes transport error (kind=%s): %v", se.Kind, verifyErr)
		}
		t.Skipf("VerifyAutoMergeScopes returned transport error; skipping scope assertion: %v", verifyErr)
	}

	t.Logf("granted scopes: %v", scopes)

	if len(missing) > 0 {
		t.Logf("missing scopes: %v (token lacks scope for auto-merge)", missing)
	} else {
		t.Logf("token has sufficient auto-merge scopes")
	}

	if len(scopes) == 0 {
		// Fine-grained PATs and GitHub App installation tokens do not
		// populate X-OAuth-Scopes; VerifyAutoMergeScopes signals
		// "unable to verify" by returning nil scopes with an empty
		// missing list. Callers fail open and rely on runtime auth
		// checks on the first merge attempt.
		t.Log("X-OAuth-Scopes header was empty (fine-grained PAT or GitHub App)")
		if len(missing) != 0 {
			t.Errorf("missing = %v; want nil when scopes are unverifiable", missing)
		}
	}
}

const integrationStatement = "/close\n@sortie-literal-probe please look\n[~jdoe] see https://example.com/probe?x=1\n" +
	"*bold* _it_ h1. Heading\n```` fenced ````\n{NoFormat} then {noformat}\ntoken=[redacted]"

func renderedGitHubComment(t *testing.T, ctx context.Context, issueID, since, marker string) string {
	t.Helper()

	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments?since=%s&per_page=100",
		os.Getenv("SORTIE_GITHUB_PROJECT"), issueID, since)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SORTIE_GITHUB_TOKEN"))
	req.Header.Set("Accept", "application/vnd.github.full+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	var comments []struct {
		Body     string `json:"body"`
		BodyHTML string `json:"body_html"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return comment.BodyHTML
		}
	}
	t.Fatalf("no comment containing %q among %d comments since %s", marker, len(comments), since)
	return ""
}

func TestIntegration_CommentIssueWithLiteral_RendersInert(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set; set to a valid issue number")
	}
	a := integrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	since := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	marker := "sortie literal round trip " + time.Now().UTC().Format(time.RFC3339Nano)
	if err := a.CommentIssueWithLiteral(ctx, issueID, marker, integrationStatement); err != nil {
		t.Fatalf("CommentIssueWithLiteral(%s): %v", issueID, err)
	}

	rendered := renderedGitHubComment(t, ctx, issueID, since, marker)

	if got := strings.Count(rendered, "<pre"); got != 1 {
		t.Errorf("body_html holds %d <pre> blocks, want 1:\n%s", got, rendered)
	}
	if strings.Contains(rendered, "<a ") {
		t.Errorf("body_html holds a link or mention, want the statement inert:\n%s", rendered)
	}
	if !strings.Contains(rendered, "@sortie-literal-probe") {
		t.Errorf("body_html does not show the statement:\n%s", rendered)
	}
}

func ensureProbeLabel(t *testing.T, ctx context.Context, a *GitHubAdapter, name string) {
	t.Helper()

	labelPath := "/repos/" + a.owner + "/" + a.repo + "/labels/" + url.PathEscape(name)
	_, _, err := a.client.Get(ctx, labelPath, nil)
	if err == nil {
		return
	}
	if !domain.IsNotFound(err) {
		t.Fatalf("GET %s: %v", labelPath, err)
	}

	payload, err := json.Marshal(map[string]string{"name": name, "color": "ededed"})
	if err != nil {
		t.Fatalf("marshal label payload: %v", err)
	}
	if _, err := a.client.Send(ctx, "POST", "/repos/"+a.owner+"/"+a.repo+"/labels", bytes.NewReader(payload)); err != nil {
		t.Fatalf("create probe label %q: %v", name, err)
	}
}

func TestIntegration_LabelRoundTrip(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set; set to a valid issue number")
	}

	a := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const probe = "sortie-label-probe"
	ensureProbeLabel(t, ctx, a, probe)

	if err := a.AddLabel(ctx, issueID, probe); err != nil {
		t.Fatalf("AddLabel(%s, %q): %v", issueID, probe, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := a.RemoveLabel(cleanupCtx, issueID, probe); err != nil {
			t.Errorf("cleanup RemoveLabel(%s, %q): %v", issueID, probe, err)
		}
	})

	fetched, err := a.FetchIssueByID(ctx, issueID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", issueID, err)
	}
	if !slices.Contains(fetched.Labels, probe) {
		t.Fatalf("FetchIssueByID(%s) labels = %v, want %q after AddLabel", issueID, fetched.Labels, probe)
	}

	if err := a.RemoveLabel(ctx, issueID, probe); err != nil {
		t.Fatalf("RemoveLabel(%s, %q): %v", issueID, probe, err)
	}
	fetched, err = a.FetchIssueByID(ctx, issueID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s) after removal: %v", issueID, err)
	}
	if slices.Contains(fetched.Labels, probe) {
		t.Errorf("FetchIssueByID(%s) labels = %v, want %q gone immediately after RemoveLabel", issueID, fetched.Labels, probe)
	}
}
