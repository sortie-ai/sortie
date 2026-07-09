package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"

	_ "github.com/sortie-ai/sortie/internal/scm/github"
)

// skipUnlessGitHubE2ELabelReview skips the test when the GitHub E2E gate is
// absent or the required credentials are not set. Mirrors the auto-merge
// and merge-conflict E2E gates: a single SORTIE_GITHUB_E2E gate plus the
// token and project, never a per-operation env var. The test must skip
// cleanly — never fail — when the gate is unset.
func skipUnlessGitHubE2ELabelReview(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping label-review E2E test in short mode")
	}
	if os.Getenv("SORTIE_GITHUB_E2E") != "1" {
		t.Skip("skipping label-review E2E test: set SORTIE_GITHUB_E2E=1, SORTIE_GITHUB_TOKEN, SORTIE_GITHUB_PROJECT")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping label-review E2E test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping label-review E2E test: SORTIE_GITHUB_PROJECT not set")
	}
}

// labelReviewAPIClient is a minimal GitHub REST client used only for test
// setup and teardown in the label-review E2E test. It is intentionally
// separate from the auto-merge and merge-conflict E2E clients so the three
// tests can run independently without coupling.
type labelReviewAPIClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func newLabelReviewAPIClient(t *testing.T) *labelReviewAPIClient {
	t.Helper()
	return &labelReviewAPIClient{
		token:      os.Getenv("SORTIE_GITHUB_TOKEN"),
		baseURL:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *labelReviewAPIClient) doRequest(t *testing.T, method, path string, body any) []byte {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body for %s %s: %v", method, path, err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper; best-effort cleanup

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody
}

// doRequestIgnoreStatus executes a request and returns the status code
// without failing on non-2xx. Used by cleanup helpers that tolerate 404/422.
func (c *labelReviewAPIClient) doRequestIgnoreStatus(method, path string, body any) (int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()        //nolint:errcheck // cleanup helper
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain to reuse connection
	return resp.StatusCode, nil
}

func (c *labelReviewAPIClient) defaultBranchSHA(t *testing.T, owner, repo string) (defaultBranch, sha string) {
	t.Helper()
	repoResp := c.doRequest(t, "GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repoResp, &repoInfo); err != nil {
		t.Fatalf("unmarshal repo info: %v", err)
	}
	if repoInfo.DefaultBranch == "" {
		repoInfo.DefaultBranch = "main"
	}

	refResp := c.doRequest(t, "GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, repoInfo.DefaultBranch), nil)
	var refInfo struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(refResp, &refInfo); err != nil {
		t.Fatalf("unmarshal ref info: %v", err)
	}
	if refInfo.Object.SHA == "" {
		t.Fatalf("could not resolve HEAD SHA for %s/%s", repoInfo.DefaultBranch, repo)
	}
	return repoInfo.DefaultBranch, refInfo.Object.SHA
}

func (c *labelReviewAPIClient) putFile(t *testing.T, owner, repo, branch, path, content, message string) {
	t.Helper()
	c.doRequest(t, "PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path), map[string]any{
		"message": message,
		"content": encodeBase64([]byte(content)),
		"branch":  branch,
	})
}

// createLabelReviewPR opens a throwaway PR with a single distinguishing
// commit on a fresh branch. Returns the PR number.
func (c *labelReviewAPIClient) createLabelReviewPR(t *testing.T, owner, repo, branch, prTitle string) int {
	t.Helper()

	defaultBranch, baseSHA := c.defaultBranchSHA(t, owner, repo)

	c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": baseSHA,
	})

	c.putFile(t, owner, repo, branch, fmt.Sprintf("e2e/label-review-%s.txt", branch),
		"label-review E2E test content\n", "test: seed label-review E2E branch")

	prResp := c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), map[string]any{
		"title": prTitle,
		"head":  branch,
		"base":  defaultBranch,
		"body":  "Label-review E2E test PR — safe to close",
	})
	var pr struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(prResp, &pr); err != nil {
		t.Fatalf("unmarshal create PR response: %v", err)
	}
	t.Logf("opened label-review E2E PR #%d (branch %q -> %s)", pr.Number, branch, defaultBranch)
	return pr.Number
}

// addLabel applies label to the given PR via the issue-labels endpoint.
func (c *labelReviewAPIClient) addLabel(t *testing.T, owner, repo string, prNumber int, label string) {
	t.Helper()
	c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, prNumber), map[string]any{
		"labels": []string{label},
	})
}

// currentLabels returns the lowercased names of every label currently
// applied to the given PR.
func (c *labelReviewAPIClient) currentLabels(t *testing.T, owner, repo string, prNumber int) []string {
	t.Helper()
	resp := c.doRequest(t, "GET", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, prNumber), nil)
	var issue struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(resp, &issue); err != nil {
		t.Fatalf("unmarshal issue labels: %v", err)
	}
	names := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		names = append(names, strings.ToLower(l.Name))
	}
	return names
}

func (c *labelReviewAPIClient) closePR(t *testing.T, owner, repo string, prNumber int) {
	t.Helper()
	status, err := c.doRequestIgnoreStatus("PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber), map[string]any{
		"state": "closed",
	})
	if err != nil || (status >= 300 && status != http.StatusUnprocessableEntity) {
		t.Logf("cleanup: close PR #%d returned status=%d err=%v (tolerated)", prNumber, status, err)
	}
}

func (c *labelReviewAPIClient) deleteBranch(t *testing.T, owner, repo, branch string) {
	t.Helper()
	status, err := c.doRequestIgnoreStatus("DELETE", fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch), nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
	}
}

// TestReconcileLabelReview_LiveAPI_E2E exercises reconcileLabelReviewCommands
// against the live GitHub API. It creates a throwaway PR, applies the
// configured review label via the REST API, wires the real SCM adapter, and
// drives the reconcile pass until exactly one label-review dispatch is
// scheduled. It then asserts the label was removed from the live PR
// (best-effort acknowledgment) and that re-applying the label produces a
// second dispatch with no process restart.
//
// Required environment variables:
//
//	SORTIE_GITHUB_E2E=1
//	SORTIE_GITHUB_TOKEN=ghp_...   PAT with issues:write, pull_requests:write, contents:write
//	SORTIE_GITHUB_PROJECT=sortie-ai/sortie-test
//
// No t.Parallel — the test mutates shared state in a live repository.
func TestReconcileLabelReview_LiveAPI_E2E(t *testing.T) {
	skipUnlessGitHubE2ELabelReview(t)

	ctx := context.Background()

	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("SORTIE_GITHUB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo := parts[0], parts[1]

	ghClient := newLabelReviewAPIClient(t)

	branch := fmt.Sprintf("label-review-test-%d-%s", time.Now().UnixNano(), randomHex(4))
	prTitle := fmt.Sprintf("sortie-e2e-label-review %s", branch)
	issueID := fmt.Sprintf("%s/%s#e2e-label-review", owner, repo)

	prNumber := ghClient.createLabelReviewPR(t, owner, repo, branch, prTitle)

	t.Cleanup(func() {
		ghClient.closePR(t, owner, repo, prNumber)
		ghClient.deleteBranch(t, owner, repo, branch)
	})

	scmFactory, err := registry.SCMAdapters.Get("github")
	if err != nil {
		t.Fatalf("registry.SCMAdapters.Get(%q): %v", "github", err)
	}
	scmAdapter, err := scmFactory(map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}

	store := openInMemoryStore(t)

	state := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	state.PendingReactions[rkey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID,
		Attempt:    1,
		Kind:       ReactionKindLabelReview,
		CreatedAt:  time.Now().UTC(),
		KindData: &LabelReviewReactionData{
			PRNumber: prNumber,
			Owner:    owner,
			Repo:     repo,
		},
	}

	params := labelReviewParams(store, scmAdapter)
	params.Ctx = ctx

	reviewLabel := params.LabelReviewConfig.ReviewLabel
	ghClient.addLabel(t, owner, repo, prNumber, reviewLabel)

	const maxTicks = 8
	dispatched := false
	for range maxTicks {
		reconcileLabelReviewCommands(state, params, discardLogger(), ctx, &domain.NoopMetrics{})

		if _, retryQueued := state.RetryAttempts[issueID]; retryQueued {
			dispatched = true
			break
		}
		if pending, present := state.PendingReactions[rkey]; present {
			pending.PendingRetryAt = time.Time{}
			state.PendingReactions[rkey] = pending
		}
		time.Sleep(2 * time.Second)
	}
	if !dispatched {
		t.Fatal("label-review dispatch did not fire within the tick budget")
	}

	retry := state.RetryAttempts[issueID]
	if retry.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindLabelReview)
	}
	if retry.SessionID != "" {
		t.Errorf("RetryEntry.SessionID = %q, want empty (fresh session, not a resume)", retry.SessionID)
	}
	raw, ok := retry.ContinuationContext["label_review"]
	if !ok {
		t.Fatal(`ContinuationContext missing "label_review" key`)
	}
	lrContext, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("label_review context type = %T, want map[string]any", raw)
	}
	if lrContext["pr_number"] != prNumber {
		t.Errorf("label_review[pr_number] = %v, want %d", lrContext["pr_number"], prNumber)
	}
	if lrContext["owner"] != owner {
		t.Errorf("label_review[owner] = %v, want %q", lrContext["owner"], owner)
	}
	if lrContext["repo"] != repo {
		t.Errorf("label_review[repo] = %v, want %q", lrContext["repo"], repo)
	}
	actor, _ := lrContext["actor"].(string)
	if actor == "" {
		t.Error("label_review[actor] is empty, want the acting user's login recorded")
	}

	// Best-effort acknowledgment: the review label should be removed from
	// the live PR after the confirmed dispatch.
	labels := ghClient.currentLabels(t, owner, repo, prNumber)
	for _, l := range labels {
		if l == strings.ToLower(reviewLabel) {
			t.Errorf("review label %q still present on PR #%d after dispatch; want removed", reviewLabel, prNumber)
		}
	}

	// Simulate the dispatched session completing (the worker-exit path
	// clears the retry entry; this reconcile file does not clear it
	// itself). Re-apply the label and confirm a second dispatch fires
	// with no process restart, driven by the reconcile's own re-enqueue.
	CancelRetry(state, issueID)

	ghClient.addLabel(t, owner, repo, prNumber, reviewLabel)

	dispatchedAgain := false
	for range maxTicks {
		reconcileLabelReviewCommands(state, params, discardLogger(), ctx, &domain.NoopMetrics{})

		if _, retryQueued := state.RetryAttempts[issueID]; retryQueued {
			dispatchedAgain = true
			break
		}
		if pending, present := state.PendingReactions[rkey]; present {
			pending.PendingRetryAt = time.Time{}
			state.PendingReactions[rkey] = pending
		}
		time.Sleep(2 * time.Second)
	}
	if !dispatchedAgain {
		t.Fatal("second label-review dispatch did not fire within the tick budget after re-applying the label")
	}
}
