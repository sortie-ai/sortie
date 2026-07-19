package gitea

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// giteaMergeFixture is a minimal Gitea REST client used only to create and clean
// up the throwaway pull request the merge-flow test drives. It never invokes the
// SCM methods under test.
type giteaMergeFixture struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// newGiteaMergeFixture builds the fixture client from the integration env vars,
// authenticating with SORTIE_GITEA_TOKEN against SORTIE_GITEA_ENDPOINT + /api/v1.
func newGiteaMergeFixture(t *testing.T) *giteaMergeFixture {
	t.Helper()
	endpoint := strings.TrimRight(requireEnv(t, "SORTIE_GITEA_ENDPOINT"), "/")
	if !strings.HasSuffix(endpoint, "/api/v1") {
		endpoint += "/api/v1"
	}
	return &giteaMergeFixture{
		token:      requireEnv(t, "SORTIE_GITEA_TOKEN"),
		baseURL:    endpoint,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// do executes a request and fails the test on a transport error or a non-2xx
// status, returning the raw response body.
func (f *giteaMergeFixture) do(t *testing.T, method, path string, body any) []byte {
	t.Helper()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body for %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, f.baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper; best-effort cleanup

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response for %s %s: %v", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody
}

// doStatus executes a request and returns the status code, tolerating a non-2xx
// response so cleanup helpers can ignore an already-gone resource.
func (f *giteaMergeFixture) doStatus(method, path string, body any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, f.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // cleanup helper
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// pushFile commits a single file to the branch, advancing its head.
func (f *giteaMergeFixture) pushFile(t *testing.T, owner, repo, branch, path, content string) {
	t.Helper()
	f.do(t, http.MethodPost, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path), map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
		"message": "test: " + path,
	})
}

// createBranchAndPR branches from the repository default branch, commits a
// sentinel file, opens a pull request, and returns the PR index.
func (f *giteaMergeFixture) createBranchAndPR(t *testing.T, owner, repo, branch, title string) int {
	t.Helper()

	repoResp := f.do(t, http.MethodGet, fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repoResp, &repoInfo); err != nil {
		t.Fatalf("unmarshal repo info: %v", err)
	}
	if repoInfo.DefaultBranch == "" {
		repoInfo.DefaultBranch = "main"
	}

	f.do(t, http.MethodPost, fmt.Sprintf("/repos/%s/%s/branches", owner, repo), map[string]any{
		"new_branch_name": branch,
		"old_branch_name": repoInfo.DefaultBranch,
	})

	f.pushFile(t, owner, repo, branch, fmt.Sprintf("merge-flow-%s.txt", branch), branch+" first\n")

	prResp := f.do(t, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), map[string]any{
		"head":  branch,
		"base":  repoInfo.DefaultBranch,
		"title": title,
		"body":  "SCM adapter merge-flow integration test PR; safe to merge or close.",
	})
	var pr struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(prResp, &pr); err != nil {
		t.Fatalf("unmarshal create PR response: %v", err)
	}
	return pr.Number
}

// pushCommit advances the branch head with a second file, invalidating an
// earlier head-SHA precondition.
func (f *giteaMergeFixture) pushCommit(t *testing.T, owner, repo, branch string) {
	t.Helper()
	f.pushFile(t, owner, repo, branch,
		fmt.Sprintf("merge-flow-advance-%d.txt", time.Now().UnixNano()), "advance\n")
}

// closePR closes the pull request if still open, tolerating an already-closed or
// already-merged state.
func (f *giteaMergeFixture) closePR(t *testing.T, owner, repo string, prNumber int) {
	t.Helper()
	status, err := f.doStatus(http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber),
		map[string]any{"state": "closed"})
	if err != nil || (status >= 300 && status != http.StatusUnprocessableEntity) {
		t.Logf("cleanup: close PR #%d returned status=%d err=%v (tolerated)", prNumber, status, err)
	}
}

// deleteBranch deletes the branch, tolerating a 404 when it is already gone.
func (f *giteaMergeFixture) deleteBranch(t *testing.T, owner, repo, branch string) {
	t.Helper()
	status, err := f.doStatus(http.MethodDelete,
		fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, branch), nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
	}
}

// requireSCMErrorKind asserts err is a [*domain.SCMError] of the given kind and
// returns it so the caller can inspect the message.
func requireSCMErrorKind(t *testing.T, err error, want domain.SCMErrorKind) *domain.SCMError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *domain.SCMError of kind %q, got nil", want)
	}
	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind != want {
		t.Errorf("SCMError.Kind = %q, want %q", se.Kind, want)
	}
	return se
}

// mergeRandomHex returns an n-byte random hex string, keeping each run's branch
// name unique so re-runs never collide on the persistent instance.
func mergeRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func TestIntegration_SCMMergeFlow(t *testing.T) {
	skipUnlessIntegration(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)
	fixture := newGiteaMergeFixture(t)

	// The flow runs several network calls plus a bounded mergeability readiness
	// poll, so it uses a wider budget than the single-call read tests; each
	// underlying request stays bounded by the adapter's own 30 s client timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	branch := fmt.Sprintf("sortie-merge-flow-%d-%s", time.Now().UnixNano(), mergeRandomHex(4))
	prNumber := fixture.createBranchAndPR(t, owner, repo, branch, "sortie merge-flow "+branch)

	t.Cleanup(func() {
		fixture.closePR(t, owner, repo, prNumber)
		fixture.deleteBranch(t, owner, repo, branch)
	})

	var staleSHA string
	var happyHeadSHA string

	t.Run("GetMergeability", func(t *testing.T) {
		status, err := adapter.GetMergeability(ctx, prNumber, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(PR #%d): %v", prNumber, err)
		}
		if status.HeadSHA == "" {
			t.Errorf("GetMergeability(PR #%d).HeadSHA = %q, want non-empty", prNumber, status.HeadSHA)
		}
		if status.BranchName != branch {
			t.Errorf("GetMergeability(PR #%d).BranchName = %q, want %q", prNumber, status.BranchName, branch)
		}
		staleSHA = status.HeadSHA
	})
	if t.Failed() {
		t.Skip("GetMergeability subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_StalePrecondition", func(t *testing.T) {
		// Advance the head so staleSHA becomes valid but out of date, driving
		// Gitea's 409 "head out of date" path deterministically.
		fixture.pushCommit(t, owner, repo, branch)
		_, err := adapter.MergePR(ctx, prNumber, owner, repo, domain.StrategySquash, "", "", staleSHA)
		se := requireSCMErrorKind(t, err, domain.ErrSCMConflict)
		if strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("MergePR stale precondition Message = %q, want no 'already merged' marker", se.Message)
		}
	})
	if t.Failed() {
		t.Skip("MergePR_StalePrecondition subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_HappyPath", func(t *testing.T) {
		// The stale-precondition push makes Gitea recompute mergeability
		// asynchronously; poll until it leaves unknown before capturing the head.
		const maxAttempts = 5
		var headSHA string
		for attempt := range maxAttempts {
			status, err := adapter.GetMergeability(ctx, prNumber, owner, repo)
			if err != nil {
				t.Fatalf("GetMergeability(PR #%d): %v", prNumber, err)
			}
			headSHA = status.HeadSHA
			if status.Mergeability != domain.MergeabilityUnknown {
				break
			}
			if attempt < maxAttempts-1 {
				time.Sleep(2 * time.Second)
			}
		}
		if headSHA == "" {
			t.Fatalf("GetMergeability(PR #%d).HeadSHA is empty after the readiness poll", prNumber)
		}
		result, err := adapter.MergePR(ctx, prNumber, owner, repo, domain.StrategySquash, "", "", headSHA)
		if err != nil {
			t.Fatalf("MergePR(PR #%d, squash, headSHA=%q): %v", prNumber, headSHA, err)
		}
		if !result.Merged {
			t.Errorf("MergePR(PR #%d).Merged = false, want true", prNumber)
		}
		happyHeadSHA = headSHA
	})
	if t.Failed() {
		t.Skip("MergePR_HappyPath subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_AlreadyMerged", func(t *testing.T) {
		// Gitea returns HTTP 405 "already merged" on a second merge, unlike the
		// GitHub sibling's idempotent success.
		_, err := adapter.MergePR(ctx, prNumber, owner, repo, domain.StrategySquash, "", "", happyHeadSHA)
		se := requireSCMErrorKind(t, err, domain.ErrSCMConflict)
		if !strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("MergePR second call Message = %q, want the 'already merged' marker", se.Message)
		}
	})
	if t.Failed() {
		t.Skip("MergePR_AlreadyMerged subtest failed; skipping DeleteBranch subtests")
	}

	t.Run("DeleteBranch", func(t *testing.T) {
		if err := adapter.DeleteBranch(ctx, owner, repo, branch); err != nil {
			t.Fatalf("DeleteBranch(%q): %v", branch, err)
		}
	})
	if t.Failed() {
		t.Skip("DeleteBranch subtest failed; skipping DeleteBranch_Again")
	}

	t.Run("DeleteBranch_Again", func(t *testing.T) {
		err := adapter.DeleteBranch(ctx, owner, repo, branch)
		if err == nil {
			return
		}
		_ = requireSCMErrorKind(t, err, domain.ErrSCMNotFound)
	})
}
