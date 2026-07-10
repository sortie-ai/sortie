package orchestrator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"

	_ "github.com/sortie-ai/sortie/internal/scm/github"
)

// skipUnlessGitHubE2EAutoMerge skips the test when the auto-merge E2E gate
// is absent or the required credentials are not set.
func skipUnlessGitHubE2EAutoMerge(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping auto-merge E2E test in short mode")
	}
	if os.Getenv("SORTIE_GITHUB_E2E") != "1" {
		t.Skip("skipping auto-merge E2E test: set SORTIE_GITHUB_E2E=1, SORTIE_GITHUB_TOKEN, SORTIE_GITHUB_PROJECT")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping auto-merge E2E test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping auto-merge E2E test: SORTIE_GITHUB_PROJECT not set")
	}
}

// autoMergeAPIClient is a minimal GitHub REST client used only for test
// setup and teardown in the auto-merge E2E test. It is intentionally
// separate from the githubAPIClient declared in github_integration_test.go
// so the two E2E tests can be run independently without coupling.
type autoMergeAPIClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func newAutoMergeAPIClient(t *testing.T) *autoMergeAPIClient {
	t.Helper()
	return &autoMergeAPIClient{
		token:      os.Getenv("SORTIE_GITHUB_TOKEN"),
		baseURL:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// autoMergePRResponse is the subset of the GitHub pull-request JSON used
// for asserting merge state after the reconcile loop runs.
type autoMergePRResponse struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// autoMergeIssueCommentResponse is the subset of a GitHub issue comment
// response used to verify the tracker comment posted by postAutoMergeSuccess.
type autoMergeIssueCommentResponse struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// doAMRequest executes a GitHub REST API request and returns the raw body.
// It calls t.Fatalf on network or non-2xx response errors.
func (c *autoMergeAPIClient) doAMRequest(t *testing.T, method, path string, body any) []byte {
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

// doAMRequestIgnoreStatus executes a GitHub REST API request and returns the
// HTTP status code. It does NOT call Fatalf on non-2xx; callers handle
// non-success status themselves. Used in cleanup helpers that must tolerate
// 404 (resource already deleted) and 422.
func (c *autoMergeAPIClient) doAMRequestIgnoreStatus(method, path string, body any) (int, error) {
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
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain body to reuse connection
	return resp.StatusCode, nil
}

// createAutoMergeTestIssue creates a GitHub issue in the target repository.
// Returns the numeric issue ID string (used as the tracker issue ID).
func (c *autoMergeAPIClient) createAutoMergeTestIssue(t *testing.T, owner, repo, title string) (issueNumber int, issueID string) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues", owner, repo)
	resp := c.doAMRequest(t, "POST", path, map[string]any{
		"title": title,
		"body":  "E2E auto-merge test issue — safe to close",
	})

	var parsed struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("unmarshal create issue response: %v", err)
	}
	issueNumber = parsed.Number
	issueID = fmt.Sprintf("%d", issueNumber)
	t.Logf("created test issue #%d in %s/%s", issueNumber, owner, repo)
	return issueNumber, issueID
}

// createAutoMergeBranchAndPR creates a throwaway branch with a single commit,
// then opens a PR from that branch. Returns the PR number.
func (c *autoMergeAPIClient) createAutoMergeBranchAndPR(t *testing.T, owner, repo, branch, prTitle string) int {
	t.Helper()

	// Resolve the default branch HEAD SHA.
	repoPath := fmt.Sprintf("/repos/%s/%s", owner, repo)
	repoResp := c.doAMRequest(t, "GET", repoPath, nil)
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repoResp, &repoInfo); err != nil {
		t.Fatalf("unmarshal repo info: %v", err)
	}
	if repoInfo.DefaultBranch == "" {
		repoInfo.DefaultBranch = "main"
	}

	// Fetch the SHA of the default branch tip.
	refPath := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, repoInfo.DefaultBranch)
	refResp := c.doAMRequest(t, "GET", refPath, nil)
	var refInfo struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(refResp, &refInfo); err != nil {
		t.Fatalf("unmarshal ref info: %v", err)
	}
	baseSHA := refInfo.Object.SHA
	if baseSHA == "" {
		t.Fatalf("could not resolve HEAD SHA for %s/%s", repoInfo.DefaultBranch, repo)
	}

	// Create the new branch.
	refsPath := fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo)
	c.doAMRequest(t, "POST", refsPath, map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": baseSHA,
	})
	t.Logf("created branch %q from %s SHA %s", branch, repoInfo.DefaultBranch, baseSHA[:8])

	// Retrieve or create the sentinel file blob.
	// Push one trivial commit: create a new file whose name equals the branch.
	fileName := fmt.Sprintf("e2e/auto-merge-marker-%s.txt", branch)
	fileContent := []byte(branch + "\n")
	fileContentB64 := encodeBase64(fileContent)

	commitPath := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, fileName)
	c.doAMRequest(t, "PUT", commitPath, map[string]any{
		"message": "test: add auto-merge marker for " + branch,
		"content": fileContentB64,
		"branch":  branch,
	})
	t.Logf("pushed sentinel commit to branch %q", branch)

	// Open a PR from branch → default branch.
	prsPath := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	prResp := c.doAMRequest(t, "POST", prsPath, map[string]any{
		"title": prTitle,
		"head":  branch,
		"base":  repoInfo.DefaultBranch,
		"body":  "Auto-merge E2E test PR — safe to merge or close",
	})

	var pr struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(prResp, &pr); err != nil {
		t.Fatalf("unmarshal create PR response: %v", err)
	}
	t.Logf("opened PR #%d (branch %q → %s)", pr.Number, branch, repoInfo.DefaultBranch)
	return pr.Number
}

// fetchPRState fetches the current state of the PR.
func (c *autoMergeAPIClient) fetchPRState(t *testing.T, owner, repo string, prNumber int) autoMergePRResponse {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	resp := c.doAMRequest(t, "GET", path, nil)

	var pr autoMergePRResponse
	if err := json.Unmarshal(resp, &pr); err != nil {
		t.Fatalf("unmarshal PR state: %v", err)
	}
	return pr
}

// fetchIssueComments fetches comments on the given issue.
func (c *autoMergeAPIClient) fetchIssueComments(t *testing.T, owner, repo string, issueNumber int) []autoMergeIssueCommentResponse {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	resp := c.doAMRequest(t, "GET", path, nil)

	var comments []autoMergeIssueCommentResponse
	if err := json.Unmarshal(resp, &comments); err != nil {
		t.Fatalf("unmarshal issue comments: %v", err)
	}
	return comments
}

// branchExists returns true if the branch still exists in the repository.
func (c *autoMergeAPIClient) branchExists(owner, repo, branch string) bool {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, branch)
	status, _ := c.doAMRequestIgnoreStatus("GET", path, nil)
	return status == http.StatusOK
}

// closeIssue closes the test issue, tolerating errors (best-effort cleanup).
func (c *autoMergeAPIClient) closeIssue(t *testing.T, owner, repo string, issueNumber int) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, issueNumber)
	status, err := c.doAMRequestIgnoreStatus("PATCH", path, map[string]any{
		"state":        "closed",
		"state_reason": "not_planned",
	})
	if err != nil || status >= 300 {
		t.Logf("cleanup: close issue #%d returned status=%d err=%v (tolerated)", issueNumber, status, err)
		return
	}
	t.Logf("cleanup: closed issue #%d", issueNumber)
}

// closePR closes the PR if it is still open (best-effort cleanup).
func (c *autoMergeAPIClient) closePR(t *testing.T, owner, repo string, prNumber int) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	status, err := c.doAMRequestIgnoreStatus("PATCH", path, map[string]any{
		"state": "closed",
	})
	if err != nil || (status >= 300 && status != http.StatusUnprocessableEntity) {
		t.Logf("cleanup: close PR #%d returned status=%d err=%v (tolerated)", prNumber, status, err)
		return
	}
	t.Logf("cleanup: closed PR #%d", prNumber)
}

// deleteBranch deletes the branch, tolerating 404 when already deleted.
func (c *autoMergeAPIClient) deleteBranch(t *testing.T, owner, repo, branch string) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch)
	status, err := c.doAMRequestIgnoreStatus("DELETE", path, nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
		return
	}
	t.Logf("cleanup: deleted branch %q (status=%d)", branch, status)
}

// encodeBase64 returns the standard base64 encoding of b.
func encodeBase64(b []byte) string {
	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, ((len(b)+2)/3)*4)
	for i := 0; i < len(b); i += 3 {
		var block [3]byte
		n := copy(block[:], b[i:])
		enc := [4]byte{
			base64Chars[block[0]>>2],
			base64Chars[((block[0]&3)<<4)|(block[1]>>4)],
			base64Chars[((block[1]&0xf)<<2)|(block[2]>>6)],
			base64Chars[block[2]&0x3f],
		}
		switch n {
		case 1:
			enc[2] = '='
			enc[3] = '='
		case 2:
			enc[3] = '='
		}
		out = append(out, enc[:]...)
	}
	return string(out)
}

// randomHex returns a 4-byte random hex string.
func randomHex() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// autoMergeMetricsCounter is a minimal domain.Metrics stub that only tracks
// IncAutoMergeReactions calls. All other methods are no-ops.
type autoMergeMetricsCounter struct {
	domain.NoopMetrics
	calls atomic.Int64
	// labels records each label argument passed to IncAutoMergeReactions.
	mu     chan struct{}
	labels []string
}

func newAutoMergeMetricsCounter() *autoMergeMetricsCounter {
	c := &autoMergeMetricsCounter{
		mu: make(chan struct{}, 1),
	}
	c.mu <- struct{}{}
	return c
}

func (m *autoMergeMetricsCounter) IncAutoMergeReactions(result string) {
	<-m.mu
	m.calls.Add(1)
	m.labels = append(m.labels, result)
	m.mu <- struct{}{}
}

func (m *autoMergeMetricsCounter) mergedCount() int {
	<-m.mu
	defer func() { m.mu <- struct{}{} }()
	count := 0
	for _, l := range m.labels {
		if l == "merged" {
			count++
		}
	}
	return count
}

// openInMemoryStore opens an in-memory SQLite store and runs migrations.
func openInMemoryStore(t *testing.T) *persistence.Store {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("persistence.Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return store
}

// TestReconcileAutoMerge_LiveAPI_E2E exercises the full auto-merge reconcile
// loop against the live GitHub API using the sortie-ai/sortie-test repository.
// The test creates a throwaway branch with a single commit, opens a PR, wires
// up the real SCM and tracker adapters, and drives reconcileAutoMerge until
// the PR is merged. It then verifies via the REST API that the PR is merged,
// the branch is gone, the linked issue carries a Sortie auto-merge comment,
// and the metrics stub recorded exactly one "merged" call.
//
// Required environment variables:
//
//	SORTIE_GITHUB_E2E=1
//	SORTIE_GITHUB_TOKEN=ghp_...   PAT with issues:write, pull_requests:write, contents:write
//	SORTIE_GITHUB_PROJECT=sortie-ai/sortie-test
//
// No t.Parallel — the test mutates shared state in a live repository.
func TestReconcileAutoMerge_LiveAPI_E2E(t *testing.T) {
	skipUnlessGitHubE2EAutoMerge(t)

	ctx := context.Background()

	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("SORTIE_GITHUB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo := parts[0], parts[1]

	ghClient := newAutoMergeAPIClient(t)

	// Create a fresh throwaway branch name guaranteed to be unique per run.
	branch := fmt.Sprintf("auto-merge-test-%d-%s", time.Now().UnixNano(), randomHex())
	prTitle := fmt.Sprintf("sortie-e2e-auto-merge %s", branch)
	issueTitle := fmt.Sprintf("sortie-e2e-auto-merge-issue %s", branch)

	// Create a test issue to receive the post-merge comment.
	issueNumber, issueID := ghClient.createAutoMergeTestIssue(t, owner, repo, issueTitle)

	// Open a PR from the throwaway branch.
	prNumber := ghClient.createAutoMergeBranchAndPR(t, owner, repo, branch, prTitle)

	// Register cleanup before any assertions that could call t.Fatal.
	t.Cleanup(func() {
		ghClient.closeIssue(t, owner, repo, issueNumber)
		ghClient.closePR(t, owner, repo, prNumber)
		ghClient.deleteBranch(t, owner, repo, branch)
	})

	// Wire the real SCM adapter from the registry (blank-imported above).
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

	// Wire the real tracker adapter (GitHub issues) from the registry.
	trackerFactory, err := registry.Trackers.Get("github")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(%q): %v", "github", err)
	}
	trackerAdapter, err := trackerFactory(map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
		"project": project,
	})
	if err != nil {
		t.Fatalf("NewGitHubAdapter: %v", err)
	}

	// In-memory SQLite store for fingerprint tracking.
	store := openInMemoryStore(t)

	// Metrics stub that counts "merged" calls.
	metrics := newAutoMergeMetricsCounter()

	// Build the reconcile state with one PendingReaction for the new PR.
	state := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.PendingReactions[rkey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID,
		Attempt:    1,
		Kind:       ReactionKindAutoMerge,
		CreatedAt:  time.Now().UTC(),
		KindData: &AutoMergeReactionData{
			PRNumber: prNumber,
			Owner:    owner,
			Repo:     repo,
			Branch:   branch,
		},
	}

	params := ReconcileParams{
		TrackerAdapter: trackerAdapter,
		SCMAdapter:     scmAdapter,
		AutoMergeConfig: AutoMergeReactionConfig{
			Strategy:       domain.StrategySquash,
			RequireCI:      false,
			DeleteBranch:   true,
			PollIntervalMS: 30000,
			MaxRetries:     2,
		},
		AutoMergeReactionConfigured: true,
		Store:                       store,
		OnRetryFire:                 noopRetryFire,
		NowFunc:                     nil,
		Ctx:                         ctx,
		Logger:                      discardLogger(),
	}

	// Drive reconcileAutoMerge up to five times with two-second pauses.
	// GitHub's mergeability computation is asynchronous; the PR may
	// initially return mergeable_state=unknown and needs a few ticks.
	const maxTicks = 5
	for range maxTicks {
		// Re-enqueue the pending reaction if it was deferred.
		if _, present := state.PendingReactions[rkey]; !present {
			break
		}

		reconcileAutoMerge(state, params, discardLogger(), ctx, metrics)

		// Drain any post-merge background goroutines (comment + branch delete).
		state.TrackerOpsWg.Wait()

		// Break early once merged.
		if metrics.mergedCount() > 0 {
			t.Logf("merge detected after tick; exiting early")
			break
		}

		// Re-add the pending entry if it was deferred with a retry-at in
		// the future, so the next tick re-evaluates mergeability.
		if pending, present := state.PendingReactions[rkey]; present {
			pending.PendingRetryAt = time.Time{}
			state.PendingReactions[rkey] = pending
		}

		time.Sleep(2 * time.Second)
	}

	// Verify: PR is merged.
	pr := ghClient.fetchPRState(t, owner, repo, prNumber)
	if !pr.Merged {
		t.Errorf("fetchPRState(PR #%d).Merged = false, want true", prNumber)
	}

	// Verify: source branch is deleted.
	if ghClient.branchExists(owner, repo, branch) {
		t.Errorf("branch %q still exists after merge with delete_branch=true", branch)
	}

	// Verify: issue carries a comment whose body contains the static prefix
	// that buildAutoMergeComment always produces.
	comments := ghClient.fetchIssueComments(t, owner, repo, issueNumber)
	const commentSubstring = "Sortie auto-merged PR #"
	found := false
	for _, c := range comments {
		if strings.Contains(c.Body, commentSubstring) {
			found = true
			t.Logf("found auto-merge comment on issue #%d: %.80q", issueNumber, c.Body)
			break
		}
	}
	if !found {
		t.Errorf("issue #%d has no comment containing %q", issueNumber, commentSubstring)
	}

	// Verify: metrics stub recorded IncAutoMergeReactions("merged") exactly once.
	if got := metrics.mergedCount(); got != 1 {
		t.Errorf("IncAutoMergeReactions(\"merged\") call count = %d, want 1", got)
	}

	// Verify: reaction fingerprint was cleared after successful merge.
	fp, _, fpErr := store.GetReactionFingerprint(ctx, issueID, ReactionKindAutoMerge)
	if fpErr != nil {
		t.Errorf("GetReactionFingerprint(%q, %q): %v", issueID, ReactionKindAutoMerge, fpErr)
	}
	if fp != "" {
		t.Errorf("GetReactionFingerprint(%q, %q) = %q, want empty (fingerprint should be cleared)", issueID, ReactionKindAutoMerge, fp)
	}
}
