package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSCMAdapter_MergeFlow_Integration exercises all five auto-merge SCM
// adapter methods against the live GitHub API: GetMergeability,
// GetReviewDecision, GetCIStatus, MergePR, and DeleteBranch.
//
// The test creates a throwaway PR, runs the full merge lifecycle, and
// asserts that a second MergePR call on the already-merged PR returns a
// nil error with MergeResult.Merged == true (idempotent success).
//
// Required environment variables:
//
//	SORTIE_GITHUB_TEST=1
//	SORTIE_GITHUB_TOKEN=ghp_... (pull_requests:write + contents:write)
//	SORTIE_GITHUB_PROJECT=owner/repo
func TestSCMAdapter_MergeFlow_Integration(t *testing.T) {
	skipUnlessGitHubIntegration(t)

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

	ctx := context.Background()

	restClient := newMergeFlowAPIClient(t)

	// Create a unique throwaway branch name guaranteed to be unique per run.
	branch := fmt.Sprintf("auto-merge-adapter-test-%d-%s",
		time.Now().UnixNano(), mfRandomHex(4))

	prNumber := restClient.createMergeFlowBranchAndPR(t, owner, repo, branch,
		fmt.Sprintf("sortie-scm-adapter-test %s", branch))

	// Register cleanup before any assertion that could call t.Fatal.
	t.Cleanup(func() {
		restClient.closeMergeFlowPR(t, owner, repo, prNumber)
		restClient.deleteMergeFlowBranch(t, owner, repo, branch)
	})

	// headSHA is captured from GetMergeability and forwarded to MergePR.
	// mergeSHA is captured from the first MergePR call for idempotency checks.
	var headSHA string
	var mergeSHA string

	t.Run("GetMergeability", func(t *testing.T) {
		// Poll up to five times with two-second pauses; GitHub computes
		// mergeability asynchronously after PR creation.
		const maxAttempts = 5
		var status domain.PRMergeStatus
		for attempt := range maxAttempts {
			var mergeErr error
			status, mergeErr = a.GetMergeability(ctx, prNumber, owner, repo)
			if mergeErr != nil {
				t.Fatalf("GetMergeability(PR #%d): %v", prNumber, mergeErr)
			}
			t.Logf("GetMergeability attempt %d: mergeability=%q headSHA=%q branch=%q",
				attempt+1, status.Mergeability, status.HeadSHA, status.BranchName)
			if status.Mergeability != domain.MergeabilityUnknown {
				break
			}
			if attempt < maxAttempts-1 {
				time.Sleep(2 * time.Second)
			}
		}

		validStates := map[domain.MergeabilityState]bool{
			domain.MergeabilityClean:    true,
			domain.MergeabilityUnstable: true,
			domain.MergeabilityBlocked:  true,
			domain.MergeabilityDirty:    true,
			domain.MergeabilityUnknown:  true,
		}
		if !validStates[status.Mergeability] {
			t.Errorf("GetMergeability(PR #%d).Mergeability = %q, want a known domain value",
				prNumber, status.Mergeability)
		}
		if status.HeadSHA == "" {
			t.Errorf("GetMergeability(PR #%d).HeadSHA = %q, want non-empty", prNumber, status.HeadSHA)
		}
		if status.BranchName != branch {
			t.Errorf("GetMergeability(PR #%d).BranchName = %q, want %q",
				prNumber, status.BranchName, branch)
		}
		headSHA = status.HeadSHA
	})

	if t.Failed() {
		t.Skip("GetMergeability subtest failed; skipping dependent subtests")
	}

	t.Run("GetReviewDecision", func(t *testing.T) {
		decision, err := a.GetReviewDecision(ctx, prNumber, owner, repo)
		if err != nil {
			t.Fatalf("GetReviewDecision(PR #%d): %v", prNumber, err)
		}
		t.Logf("GetReviewDecision(PR #%d): %q", prNumber, decision)

		// The test repo has no required reviewers; expect NotRequired.
		if decision != domain.ReviewDecisionNotRequired {
			t.Errorf("GetReviewDecision(PR #%d) = %q, want %q",
				prNumber, decision, domain.ReviewDecisionNotRequired)
		}
	})

	if t.Failed() {
		t.Skip("GetReviewDecision subtest failed; skipping dependent subtests")
	}

	t.Run("GetCIStatus", func(t *testing.T) {
		status, err := a.GetCIStatus(ctx, prNumber, owner, repo)
		if err != nil {
			t.Fatalf("GetCIStatus(PR #%d): %v", prNumber, err)
		}
		t.Logf("GetCIStatus(PR #%d): %q", prNumber, status)

		// The test repo has no required checks; expect empty string.
		if status != "" {
			t.Errorf("GetCIStatus(PR #%d) = %q, want empty (no required checks)", prNumber, status)
		}
	})

	if t.Failed() {
		t.Skip("GetCIStatus subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_HappyPath", func(t *testing.T) {
		result, err := a.MergePR(ctx, prNumber, owner, repo,
			domain.StrategySquash, "", "", headSHA)
		if err != nil {
			t.Fatalf("MergePR(PR #%d, squash, headSHA=%q): %v", prNumber, headSHA, err)
		}
		t.Logf("MergePR(PR #%d): SHA=%q merged=%v message=%q",
			prNumber, result.SHA, result.Merged, result.Message)

		if !result.Merged {
			t.Errorf("MergePR(PR #%d).Merged = false, want true", prNumber)
		}
		if result.SHA == "" {
			t.Errorf("MergePR(PR #%d).SHA = %q, want non-empty", prNumber, result.SHA)
		}
		mergeSHA = result.SHA
	})

	if t.Failed() {
		t.Skip("MergePR_HappyPath subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_AlreadyMerged_IdempotentSuccess", func(t *testing.T) {
		// Call MergePR again on the same PR that was just merged. GitHub
		// returns HTTP 200 with the existing merge commit SHA — it does not
		// re-evaluate the head-SHA precondition once the PR is merged.
		result, err := a.MergePR(ctx, prNumber, owner, repo,
			domain.StrategySquash, "", "", headSHA)
		if err != nil {
			t.Fatalf("MergePR(PR #%d) second call = %v, want nil", prNumber, err)
		}
		if !result.Merged {
			t.Errorf("MergePR(PR #%d) second call Merged = false, want true", prNumber)
		}
		if result.SHA != mergeSHA {
			t.Errorf("MergePR(PR #%d) second call SHA = %q, want %q (same merge commit)",
				prNumber, result.SHA, mergeSHA)
		}
	})

	if t.Failed() {
		t.Skip("MergePR_AlreadyMerged_IdempotentSuccess subtest failed; skipping DeleteBranch")
	}

	t.Run("GetMergeability_AfterMerge", func(t *testing.T) {
		status, err := a.GetMergeability(ctx, prNumber, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(PR #%d) after merge: %v", prNumber, err)
		}
		if !status.Merged {
			t.Errorf("GetMergeability(PR #%d).Merged = false, want true after the happy-path merge", prNumber)
		}
		if status.MergeCommitSHA != mergeSHA {
			t.Errorf("GetMergeability(PR #%d).MergeCommitSHA = %q, want %q (the SHA MergePR_HappyPath captured)",
				prNumber, status.MergeCommitSHA, mergeSHA)
		}
	})

	if t.Failed() {
		t.Skip("GetMergeability_AfterMerge subtest failed; skipping DeleteBranch")
	}

	t.Run("DeleteBranch", func(t *testing.T) {
		err := a.DeleteBranch(ctx, owner, repo, branch)
		if err != nil {
			// GitHub may auto-delete branches on some repo configurations.
			// Treat ErrSCMNotFound as a successful no-op.
			var se *domain.SCMError
			if errors.As(err, &se) && se.Kind == domain.ErrSCMNotFound {
				t.Logf("DeleteBranch(%q): branch already gone (tolerated)", branch)
				return
			}
			t.Fatalf("DeleteBranch(%q): %v", branch, err)
		}
		t.Logf("DeleteBranch(%q): success", branch)
	})
}

// mergeFlowAPIClient is a minimal GitHub REST client used only for
// setup and cleanup in TestSCMAdapter_MergeFlow_Integration.
type mergeFlowAPIClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func newMergeFlowAPIClient(t *testing.T) *mergeFlowAPIClient {
	t.Helper()
	return &mergeFlowAPIClient{
		token:      os.Getenv("SORTIE_GITHUB_TOKEN"),
		baseURL:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// doMFRequest executes a GitHub REST API request and returns the raw
// response body. Calls t.Fatalf on network errors or non-2xx responses.
func (c *mergeFlowAPIClient) doMFRequest(t *testing.T, method, path string, body any) []byte {
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

// doMFRequestIgnoreStatus executes a GitHub REST API request and returns
// the HTTP status code without calling t.Fatalf on non-2xx responses.
// Used in cleanup helpers that must tolerate 404 and 422.
func (c *mergeFlowAPIClient) doMFRequestIgnoreStatus(method, path string, body any) (int, error) {
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

// createMergeFlowBranchAndPR creates a throwaway branch with a single
// commit and opens a PR from that branch to the default branch. Returns
// the PR number.
func (c *mergeFlowAPIClient) createMergeFlowBranchAndPR(t *testing.T, owner, repo, branch, prTitle string) int {
	t.Helper()

	// Resolve the default branch and its HEAD SHA.
	repoPath := fmt.Sprintf("/repos/%s/%s", owner, repo)
	repoResp := c.doMFRequest(t, "GET", repoPath, nil)
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repoResp, &repoInfo); err != nil {
		t.Fatalf("unmarshal repo info: %v", err)
	}
	if repoInfo.DefaultBranch == "" {
		repoInfo.DefaultBranch = "main"
	}

	refPath := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, repoInfo.DefaultBranch)
	refResp := c.doMFRequest(t, "GET", refPath, nil)
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

	// Create the new branch from the default branch tip.
	refsPath := fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo)
	c.doMFRequest(t, "POST", refsPath, map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": baseSHA,
	})
	t.Logf("created branch %q from %s SHA %s", branch, repoInfo.DefaultBranch, baseSHA[:8])

	// Push one trivial commit: create a sentinel file named after the branch.
	fileName := fmt.Sprintf("e2e/adapter-marker-%s.txt", branch)
	commitPath := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, fileName)
	c.doMFRequest(t, "PUT", commitPath, map[string]any{
		"message": "test: add adapter marker for " + branch,
		"content": mfBase64([]byte(branch + "\n")),
		"branch":  branch,
	})
	t.Logf("pushed sentinel commit to branch %q", branch)

	// Open a PR from branch → default branch.
	prsPath := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	prResp := c.doMFRequest(t, "POST", prsPath, map[string]any{
		"title": prTitle,
		"head":  branch,
		"base":  repoInfo.DefaultBranch,
		"body":  "SCM adapter integration test PR — safe to merge or close",
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

// closeMergeFlowPR closes the PR if it is still open. Tolerates errors.
func (c *mergeFlowAPIClient) closeMergeFlowPR(t *testing.T, owner, repo string, prNumber int) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	status, err := c.doMFRequestIgnoreStatus("PATCH", path, map[string]any{
		"state": "closed",
	})
	if err != nil || (status >= 300 && status != http.StatusUnprocessableEntity) {
		t.Logf("cleanup: close PR #%d returned status=%d err=%v (tolerated)", prNumber, status, err)
		return
	}
	t.Logf("cleanup: closed PR #%d (status=%d)", prNumber, status)
}

// deleteMergeFlowBranch deletes the branch, tolerating 404 when already
// deleted.
func (c *mergeFlowAPIClient) deleteMergeFlowBranch(t *testing.T, owner, repo, branch string) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch)
	status, err := c.doMFRequestIgnoreStatus("DELETE", path, nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
		return
	}
	t.Logf("cleanup: deleted branch %q (status=%d)", branch, status)
}

// mfBase64 returns the standard base64 encoding of b.
func mfBase64(b []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, ((len(b)+2)/3)*4)
	for i := 0; i < len(b); i += 3 {
		var block [3]byte
		n := copy(block[:], b[i:])
		enc := [4]byte{
			chars[block[0]>>2],
			chars[((block[0]&3)<<4)|(block[1]>>4)],
			chars[((block[1]&0xf)<<2)|(block[2]>>6)],
			chars[block[2]&0x3f],
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

// mfRandomHex returns a n-byte random hex string using crypto/rand.
func mfRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
