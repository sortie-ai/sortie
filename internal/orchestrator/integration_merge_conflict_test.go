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

// skipUnlessGitHubE2EMergeConflict skips the test when the GitHub E2E gate is
// absent or the required credentials are not set. Mirrors the auto-merge E2E
// gate: a single SORTIE_GITHUB_E2E gate plus the token and project, never a
// per-operation env var. The test must skip cleanly — never fail — when the
// gate is unset.
func skipUnlessGitHubE2EMergeConflict(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping merge-conflict E2E test in short mode")
	}
	if os.Getenv("SORTIE_GITHUB_E2E") != "1" {
		t.Skip("skipping merge-conflict E2E test: set SORTIE_GITHUB_E2E=1, SORTIE_GITHUB_TOKEN, SORTIE_GITHUB_PROJECT")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping merge-conflict E2E test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping merge-conflict E2E test: SORTIE_GITHUB_PROJECT not set")
	}
}

// mergeConflictAPIClient is a minimal GitHub REST client used only for test
// setup and teardown in the merge-conflict E2E test. It is intentionally
// separate from the auto-merge and tracker E2E clients so the tests can run
// independently without coupling.
type mergeConflictAPIClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func newMergeConflictAPIClient(t *testing.T) *mergeConflictAPIClient {
	t.Helper()
	return &mergeConflictAPIClient{
		token:      os.Getenv("SORTIE_GITHUB_TOKEN"),
		baseURL:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// doRequest executes a GitHub REST API request and returns the raw body. It
// calls t.Fatalf on network or non-2xx response errors.
func (c *mergeConflictAPIClient) doRequest(t *testing.T, method, path string, body any) []byte {
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

// doRequestIgnoreStatus executes a request and returns the status code without
// failing on non-2xx. Used by cleanup helpers that tolerate 404/422.
func (c *mergeConflictAPIClient) doRequestIgnoreStatus(method, path string, body any) (int, error) {
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

// defaultBranchSHA returns the repository's default branch name and the SHA at
// its tip.
func (c *mergeConflictAPIClient) defaultBranchSHA(t *testing.T, owner, repo string) (defaultBranch, sha string) {
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

// putFile creates or updates a file on the given branch via the Contents API.
func (c *mergeConflictAPIClient) putFile(t *testing.T, owner, repo, branch, path, content, message string) {
	t.Helper()
	c.doRequest(t, "PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path), map[string]any{
		"message": message,
		"content": encodeBase64([]byte(content)),
		"branch":  branch,
	})
}

// createConflictingPR opens a PR whose head branch edits the same file as a
// subsequent commit on the base branch, guaranteeing a merge conflict. Returns
// the PR number and the base branch name.
func (c *mergeConflictAPIClient) createConflictingPR(t *testing.T, owner, repo, branch, prTitle string) (prNumber int, baseBranch string) {
	t.Helper()

	defaultBranch, baseSHA := c.defaultBranchSHA(t, owner, repo)

	// Create the head branch from the default branch tip.
	c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": baseSHA,
	})

	// Seed a shared file on the head branch.
	conflictFile := fmt.Sprintf("e2e/merge-conflict-%s.txt", branch)
	c.putFile(t, owner, repo, branch, conflictFile,
		"head branch content\n", "test: seed conflict file on head")

	// Edit the SAME file on the base (default) branch so the head branch can no
	// longer merge cleanly.
	c.putFile(t, owner, repo, defaultBranch, conflictFile,
		"base branch content\n", "test: seed conflicting change on base")

	// Open the PR from the head branch into the default branch.
	prResp := c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), map[string]any{
		"title": prTitle,
		"head":  branch,
		"base":  defaultBranch,
		"body":  "Merge-conflict E2E test PR — safe to close",
	})
	var pr struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(prResp, &pr); err != nil {
		t.Fatalf("unmarshal create PR response: %v", err)
	}
	t.Logf("opened conflicting PR #%d (branch %q → %s)", pr.Number, branch, defaultBranch)
	return pr.Number, defaultBranch
}

// createMergeConflictIssue creates a GitHub issue and returns its number and ID.
func (c *mergeConflictAPIClient) createMergeConflictIssue(t *testing.T, owner, repo, title string) (issueNumber int, issueID string) {
	t.Helper()
	resp := c.doRequest(t, "POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo), map[string]any{
		"title": title,
		"body":  "E2E merge-conflict test issue — safe to close",
	})
	var parsed struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("unmarshal create issue response: %v", err)
	}
	return parsed.Number, fmt.Sprintf("%d", parsed.Number)
}

func (c *mergeConflictAPIClient) closeIssue(t *testing.T, owner, repo string, issueNumber int) {
	t.Helper()
	status, err := c.doRequestIgnoreStatus("PATCH", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, issueNumber), map[string]any{
		"state":        "closed",
		"state_reason": "not_planned",
	})
	if err != nil || status >= 300 {
		t.Logf("cleanup: close issue #%d returned status=%d err=%v (tolerated)", issueNumber, status, err)
	}
}

func (c *mergeConflictAPIClient) closePR(t *testing.T, owner, repo string, prNumber int) {
	t.Helper()
	status, err := c.doRequestIgnoreStatus("PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber), map[string]any{
		"state": "closed",
	})
	if err != nil || (status >= 300 && status != http.StatusUnprocessableEntity) {
		t.Logf("cleanup: close PR #%d returned status=%d err=%v (tolerated)", prNumber, status, err)
	}
}

func (c *mergeConflictAPIClient) deleteBranch(t *testing.T, owner, repo, branch string) {
	t.Helper()
	status, err := c.doRequestIgnoreStatus("DELETE", fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch), nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
	}
}

// TestReconcileMergeConflict_LiveAPI_E2E exercises reconcileMergeConflicts
// against the live GitHub API. It creates an issue and a PR that genuinely
// conflicts with its base branch, wires the real SCM adapter, and drives the
// reconcile pass until one rebase continuation is dispatched. It asserts the
// dispatched merge_conflict continuation carries the PR's real base branch
// (not a hardcoded default).
//
// Required environment variables:
//
//	SORTIE_GITHUB_E2E=1
//	SORTIE_GITHUB_TOKEN=ghp_...   PAT with issues:write, pull_requests:write, contents:write
//	SORTIE_GITHUB_PROJECT=sortie-ai/sortie-test
//
// No t.Parallel — the test mutates shared state in a live repository.
func TestReconcileMergeConflict_LiveAPI_E2E(t *testing.T) {
	skipUnlessGitHubE2EMergeConflict(t)

	ctx := context.Background()

	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("SORTIE_GITHUB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo := parts[0], parts[1]

	ghClient := newMergeConflictAPIClient(t)

	branch := fmt.Sprintf("merge-conflict-test-%d-%s", time.Now().UnixNano(), randomHex())
	prTitle := fmt.Sprintf("sortie-e2e-merge-conflict %s", branch)
	issueTitle := fmt.Sprintf("sortie-e2e-merge-conflict-issue %s", branch)

	issueNumber, issueID := ghClient.createMergeConflictIssue(t, owner, repo, issueTitle)
	prNumber, baseBranch := ghClient.createConflictingPR(t, owner, repo, branch, prTitle)

	t.Cleanup(func() {
		ghClient.closeIssue(t, owner, repo, issueNumber)
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
	metrics := newMergeConflictMetricsSpy()

	state := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[rkey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID,
		Attempt:    1,
		Kind:       ReactionKindMergeConflict,
		CreatedAt:  time.Now().UTC(),
		KindData: &MergeConflictReactionData{
			PRNumber: prNumber,
			Owner:    owner,
			Repo:     repo,
			Branch:   branch,
		},
	}

	params := ReconcileParams{
		SCMAdapter:                      scmAdapter,
		MergeConflictConfig:             defaultMergeConflictConfig(),
		MergeConflictReactionConfigured: true,
		MergeConflictPendingTTL:         mergeConflictPendingDefaultTTL,
		Store:                           store,
		OnRetryFire:                     noopRetryFire,
		Ctx:                             ctx,
		Logger:                          discardLogger(),
	}

	// GitHub computes mergeability asynchronously; the PR may initially return
	// mergeable_state=unknown and needs a few ticks before it reports dirty.
	const maxTicks = 8
	for range maxTicks {
		if _, present := state.PendingReactions[rkey]; !present {
			break
		}

		reconcileMergeConflicts(state, params, discardLogger(), ctx, metrics)

		if metrics.checks["dispatched"] > 0 {
			t.Logf("merge-conflict continuation dispatched; exiting tick loop")
			break
		}

		// Clear the deferral so the next tick re-evaluates mergeability.
		if pending, present := state.PendingReactions[rkey]; present {
			pending.PendingRetryAt = time.Time{}
			state.PendingReactions[rkey] = pending
		}

		time.Sleep(2 * time.Second)
	}

	// Exactly one continuation dispatched for the conflicted PR.
	if metrics.checks["dispatched"] != 1 {
		t.Fatalf(`IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}
	if metrics.escalations["label"] != 0 || metrics.escalations["comment"] != 0 {
		t.Errorf("escalations = %v, want none on first dirty observation", metrics.escalations)
	}

	// The dispatched continuation carries the PR's real base branch.
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled after dirty dispatch; want scheduled")
	}
	raw, ok := retry.ContinuationContext["merge_conflict"]
	if !ok {
		t.Fatal(`ContinuationContext missing "merge_conflict" key`)
	}
	mergeContext, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("merge_conflict context type = %T, want map[string]any", raw)
	}
	if mergeContext["base"] != baseBranch {
		t.Errorf("ContinuationContext[merge_conflict][base] = %v, want %q (the PR's real base, not a hardcoded default)",
			mergeContext["base"], baseBranch)
	}

	// Confirm the adapter independently reports the same base branch.
	status, err := scmAdapter.GetMergeability(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if status.Mergeability != domain.MergeabilityDirty {
		t.Errorf("GetMergeability().Mergeability = %q, want %q", status.Mergeability, domain.MergeabilityDirty)
	}
	if status.BaseBranch != baseBranch {
		t.Errorf("GetMergeability().BaseBranch = %q, want %q", status.BaseBranch, baseBranch)
	}

	// The fingerprint row was written and marked dispatched for this head.
	fp, dispatched, fpErr := store.GetReactionFingerprint(ctx, issueID, ReactionKindMergeConflict)
	if fpErr != nil {
		t.Fatalf("GetReactionFingerprint: %v", fpErr)
	}
	if fp == "" {
		t.Error("GetReactionFingerprint returned empty fingerprint after dispatch; want set")
	}
	if !dispatched {
		t.Error("GetReactionFingerprint dispatched = false after dispatch; want true")
	}
}
