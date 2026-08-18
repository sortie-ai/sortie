package gitlab

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// gitlabMergeFixture is a minimal GitLab REST client used only to build
// and clean up the throwaway merge requests the write tests drive. It
// never calls a method under test (domain.SCMAdapter or
// domain.CIStatusProvider).
type gitlabMergeFixture struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// newGitLabMergeFixture builds the fixture client from the integration
// env vars, authenticating with SORTIE_GITLAB_TOKEN in the PRIVATE-TOKEN
// header against SORTIE_GITLAB_ENDPOINT suffixed with /api/v4.
func newGitLabMergeFixture(t *testing.T) *gitlabMergeFixture {
	t.Helper()
	endpoint := strings.TrimRight(requireEnv(t, "SORTIE_GITLAB_ENDPOINT"), "/")
	if !strings.HasSuffix(endpoint, "/api/v4") {
		endpoint += "/api/v4"
	}
	return &gitlabMergeFixture{
		token:      requireEnv(t, "SORTIE_GITLAB_TOKEN"),
		baseURL:    endpoint,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// do executes a request and fails the test on a transport error or a
// non-2xx status, returning the raw response body.
func (f *gitlabMergeFixture) do(t *testing.T, method, path string, body any) []byte {
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
	req.Header.Set("PRIVATE-TOKEN", f.token)
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

// doStatus executes a request and returns the status code, tolerating a
// non-2xx response so cleanup helpers can ignore an already-gone
// resource.
func (f *gitlabMergeFixture) doStatus(method, path string, body any) (int, error) {
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
	req.Header.Set("PRIVATE-TOKEN", f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()        //nolint:errcheck // cleanup helper
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain body to reuse connection
	return resp.StatusCode, nil
}

// gitlabFixtureMergeRequest decodes the fields the fixture client reads
// off a merge-request response: the current head SHA and the settled
// mergeability status.
type gitlabFixtureMergeRequest struct {
	SHA                 string `json:"sha"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
}

// createBranch creates branch from ref in project, failing the test on
// a transport or HTTP error. project is the percent-encoded path the
// adapter itself addresses.
func (f *gitlabMergeFixture) createBranch(t *testing.T, project, branch, ref string) {
	t.Helper()
	f.do(t, http.MethodPost, "/projects/"+project+"/repository/branches", map[string]any{
		"branch": branch,
		"ref":    ref,
	})
}

// commitFile commits a single file to branch and returns the resulting
// commit SHA, read from the commits response's id field; the files route
// returns no commit identifier.
func (f *gitlabMergeFixture) commitFile(t *testing.T, project, branch, path, content string) string {
	t.Helper()
	resp := f.do(t, http.MethodPost, "/projects/"+project+"/repository/commits", map[string]any{
		"branch":         branch,
		"commit_message": "test: " + path,
		"actions": []map[string]any{
			{"action": "create", "file_path": path, "content": content},
		},
	})
	var commit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &commit); err != nil {
		t.Fatalf("unmarshal commit response: %v", err)
	}
	if commit.ID == "" {
		t.Fatal("commit response carried no id")
	}
	return commit.ID
}

// openMR opens a merge request from source to target and returns its
// iid. It opens the merge request with source-branch removal disabled:
// the project setting governing removal defaults to removing the
// source branch, the removal is asynchronous, and a test that asserts
// on the branch after the merge would otherwise be racing that reap.
// Dropping this would silently reintroduce that race.
//
// It also decodes and checks the create response's own copy of the
// setting, so a forge that ignores the request fails this setup step
// rather than a later subtest that would misread the symptom as a
// delete defect.
func (f *gitlabMergeFixture) openMR(t *testing.T, project, source, target, title string) int {
	t.Helper()
	resp := f.do(t, http.MethodPost, "/projects/"+project+"/merge_requests", map[string]any{
		"source_branch":        source,
		"target_branch":        target,
		"title":                title,
		"remove_source_branch": false,
	})
	var mr struct {
		IID                     int  `json:"iid"`
		ForceRemoveSourceBranch bool `json:"force_remove_source_branch"`
	}
	if err := json.Unmarshal(resp, &mr); err != nil {
		t.Fatalf("unmarshal create merge request response: %v", err)
	}
	if mr.ForceRemoveSourceBranch {
		t.Fatalf("merge request !%d force_remove_source_branch = %v, want %v", mr.IID, true, false)
	}
	return mr.IID
}

// headSHA re-reads the merge request and returns its current head SHA.
func (f *gitlabMergeFixture) headSHA(t *testing.T, project string, iid int) string {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/projects/"+project+"/merge_requests/"+strconv.Itoa(iid), nil)
	var mr gitlabFixtureMergeRequest
	if err := json.Unmarshal(resp, &mr); err != nil {
		t.Fatalf("unmarshal merge request response: %v", err)
	}
	return mr.SHA
}

// addLabel attaches label to the merge request via add_labels, letting
// GitLab store whatever casing label carries.
func (f *gitlabMergeFixture) addLabel(t *testing.T, project string, iid int, label string) {
	t.Helper()
	f.do(t, http.MethodPut, "/projects/"+project+"/merge_requests/"+strconv.Itoa(iid), map[string]any{
		"add_labels": label,
	})
}

// closeMR closes the merge request if still open, tolerating an
// already-closed or already-merged state.
func (f *gitlabMergeFixture) closeMR(t *testing.T, project string, iid int) {
	t.Helper()
	status, err := f.doStatus(http.MethodPut, "/projects/"+project+"/merge_requests/"+strconv.Itoa(iid), map[string]any{
		"state_event": "close",
	})
	if err != nil || status >= 300 {
		t.Logf("cleanup: close merge request !%d returned status=%d err=%v (tolerated)", iid, status, err)
	}
}

// deleteBranch deletes branch, tolerating a 404 when it is already gone.
func (f *gitlabMergeFixture) deleteBranch(t *testing.T, project, branch string) {
	t.Helper()
	status, err := f.doStatus(http.MethodDelete,
		"/projects/"+project+"/repository/branches/"+branch, nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound) {
		t.Logf("cleanup: delete branch %q returned status=%d err=%v (tolerated)", branch, status, err)
	}
}

// awaitMergeability polls the merge request's detailed_merge_status with
// a bounded number of tries until it settles, and returns whatever status
// is observed last, settled or not; the caller decides what a settled
// value other than the one it expects means.
//
// A status counts as settled only when it lies outside the computing set
// (unchecked, checking, preparing, approvals_syncing) and the merge
// request's own head sha has caught up with wantSHA. GitLab reports the
// identifier of the conflict check while that check is still running, so
// a read taken before the merge-request diff is regenerated carries
// "conflict" for a merge request that has none; such a read is
// recognizable because its sha still lags the branch. An empty wantSHA
// disables the sha gate. See gitlab-org/gitlab#608247.
func (f *gitlabMergeFixture) awaitMergeability(t *testing.T, project string, iid int, wantSHA string) string {
	t.Helper()

	const maxAttempts = 10
	const pollInterval = 2 * time.Second

	computing := map[string]bool{
		"unchecked":         true,
		"checking":          true,
		"preparing":         true,
		"approvals_syncing": true,
	}

	var status string
	for attempt := range maxAttempts {
		resp := f.do(t, http.MethodGet, "/projects/"+project+"/merge_requests/"+strconv.Itoa(iid), nil)
		var mr gitlabFixtureMergeRequest
		if err := json.Unmarshal(resp, &mr); err != nil {
			t.Fatalf("unmarshal merge request response: %v", err)
		}
		status = mr.DetailedMergeStatus
		if !computing[status] && (wantSHA == "" || mr.SHA == wantSHA) {
			return status
		}
		if attempt < maxAttempts-1 {
			time.Sleep(pollInterval)
		}
	}
	return status
}

// mergeSuffix returns a per-run random suffix so branch and merge-request
// names never collide across repeated runs against one provisioned
// instance.
func mergeSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random bytes for merge fixture suffix: %v", err)
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

// fixtureDefaultBranch reads project's default branch, so the write tests
// cut their own throwaway branches from the real default rather than
// assuming a name.
func fixtureDefaultBranch(t *testing.T, f *gitlabMergeFixture, project string) string {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/projects/"+project, nil)
	var proj struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(resp, &proj); err != nil {
		t.Fatalf("unmarshal project response: %v", err)
	}
	if proj.DefaultBranch == "" {
		t.Fatal("project response carried no default_branch")
	}
	return proj.DefaultBranch
}

func TestIntegration_MergePR_ProtectedTarget(t *testing.T) {
	skipUnlessIntegration(t)

	protectedBranch := optionalEnv(t, "SORTIE_GITLAB_PROTECTED_BRANCH")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)
	fixture := newGitLabMergeFixture(t)
	project := projectPath(owner, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := mergeSuffix(t)
	head := "sortie-protected-head-" + suffix

	fixture.createBranch(t, project, head, fixtureDefaultBranch(t, fixture, project))
	sentinelSHA := fixture.commitFile(t, project, head, "sortie-protected-sentinel.txt", "protected target probe\n")
	iid := fixture.openMR(t, project, head, protectedBranch, "sortie protected target "+suffix)

	t.Cleanup(func() {
		fixture.closeMR(t, project, iid)
		fixture.deleteBranch(t, project, head)
	})

	status := fixture.awaitMergeability(t, project, iid, sentinelSHA)
	if status != "mergeable" {
		t.Fatalf("merge request !%d detailed_merge_status = %q after polling, want %q", iid, status, "mergeable")
	}
	headSHA := fixture.headSHA(t, project, iid)

	_, err := adapter.MergePR(ctx, iid, owner, repo, domain.StrategySquash, "", "", headSHA)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("MergePR(!%d, protected target) error type = %T, want *domain.SCMError", iid, err)
	}
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
	if se.Kind == domain.ErrSCMConflict {
		t.Errorf("MergePR(!%d, protected target) Kind = %q, want NOT %q", iid, se.Kind, domain.ErrSCMConflict)
	}
	lower := strings.ToLower(se.Message)
	if !strings.Contains(lower, "credential") {
		t.Errorf("MergePR(!%d, protected target) Message = %q, want it to name the invalid-credential cause", iid, se.Message)
	}
	if !strings.Contains(lower, "may not merge") {
		t.Errorf("MergePR(!%d, protected target) Message = %q, want it to name the may-not-merge cause", iid, se.Message)
	}
}

func TestIntegration_SCMMergeFlow(t *testing.T) {
	skipUnlessIntegration(t)

	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)
	fixture := newGitLabMergeFixture(t)
	project := projectPath(owner, repo)

	// The flow runs several network calls plus a bounded mergeability
	// readiness poll, so it uses a wider budget than the single-call
	// read tests; each underlying request stays bounded by the
	// adapter's own client timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	suffix := mergeSuffix(t)
	base := "sortie-merge-base-" + suffix
	head := "sortie-merge-head-" + suffix

	defaultBranch := fixtureDefaultBranch(t, fixture, project)
	fixture.createBranch(t, project, base, defaultBranch)
	fixture.createBranch(t, project, head, base)
	fixture.commitFile(t, project, head, "sortie-merge-flow.txt", "first commit\n")
	iid := fixture.openMR(t, project, head, base, "sortie merge-flow "+suffix)

	t.Cleanup(func() {
		fixture.closeMR(t, project, iid)
		fixture.deleteBranch(t, project, head)
		fixture.deleteBranch(t, project, base)
	})

	var staleSHA, advancedSHA, currentSHA string

	t.Run("GetMergeability", func(t *testing.T) {
		status, err := adapter.GetMergeability(ctx, iid, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(!%d): %v", iid, err)
		}
		if status.HeadSHA == "" {
			t.Fatalf("GetMergeability(!%d).HeadSHA is empty", iid)
		}
		staleSHA = status.HeadSHA
	})
	if t.Failed() {
		t.Skip("GetMergeability subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_StalePrecondition", func(t *testing.T) {
		// Advances the head, so staleSHA becomes valid but out of
		// date, driving GitLab's stale-precondition rejection
		// deterministically.
		advancedSHA = fixture.commitFile(t, project, head, "sortie-merge-flow-advance.txt", "advance\n")
		if advancedSHA == staleSHA {
			t.Fatalf("commitFile did not advance the head past staleSHA %q", staleSHA)
		}

		// GitLab evaluates mergeability before the sha precondition, so a
		// merge attempted while the push is still being processed is
		// refused as unmergeable and never reaches the sha check at all.
		// Letting the verdict settle against the pushed head is what makes
		// the stale sha the reason this merge is rejected.
		if status := fixture.awaitMergeability(t, project, iid, advancedSHA); status != "mergeable" {
			t.Fatalf("merge request !%d detailed_merge_status = %q after polling, want %q",
				iid, status, "mergeable")
		}

		_, err := adapter.MergePR(ctx, iid, owner, repo, domain.StrategySquash, "", "", staleSHA)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMConflict)

		// ErrSCMConflict is promoted from both 405 and 409, so the kind
		// alone cannot distinguish the stale-sha rejection this subtest
		// exercises from a merge request GitLab refuses to merge for an
		// unrelated reason. Only the 409 proves the precondition fired.
		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("MergePR stale precondition error = %v, want a wrapped *domain.TrackerError", err)
		}
		if te.Status != http.StatusConflict {
			t.Errorf("MergePR stale precondition HTTP status = %d, want %d for a stale-sha rejection",
				te.Status, http.StatusConflict)
		}

		var se *domain.SCMError
		if errors.As(err, &se) && strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("MergePR stale precondition Message = %q, want no already-merged marker", se.Message)
		}
	})
	if t.Failed() {
		t.Skip("MergePR_StalePrecondition subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_HappyPath", func(t *testing.T) {
		// The stale-precondition push makes GitLab recompute
		// detailed_merge_status asynchronously; poll until the verdict
		// settles against that pushed head before capturing the head SHA
		// to merge with.
		status := fixture.awaitMergeability(t, project, iid, advancedSHA)
		if status != "mergeable" {
			t.Fatalf("merge request !%d detailed_merge_status = %q after polling, want %q", iid, status, "mergeable")
		}
		currentSHA = fixture.headSHA(t, project, iid)

		result, err := adapter.MergePR(ctx, iid, owner, repo, domain.StrategySquash, "", "", currentSHA)
		if err != nil {
			t.Fatalf("MergePR(!%d, squash, sha=%q): %v", iid, currentSHA, err)
		}
		if !result.Merged {
			t.Errorf("MergePR(!%d).Merged = false, want true", iid)
		}
		if result.SHA == "" {
			t.Errorf("MergePR(!%d).SHA is empty, want non-empty merge commit sha", iid)
		}
	})
	if t.Failed() {
		t.Skip("MergePR_HappyPath subtest failed; skipping dependent subtests")
	}

	t.Run("GetMergeability_AfterMerge", func(t *testing.T) {
		status, err := adapter.GetMergeability(ctx, iid, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(!%d) after merge: %v", iid, err)
		}
		if !status.Merged {
			t.Errorf("GetMergeability(!%d).Merged = false, want true after the happy-path merge", iid)
		}
		if status.MergeCommitSHA == "" {
			t.Errorf("GetMergeability(!%d).MergeCommitSHA is empty, want non-empty after the happy-path merge", iid)
		}
	})
	if t.Failed() {
		t.Skip("GetMergeability_AfterMerge subtest failed; skipping dependent subtests")
	}

	t.Run("MergePR_AlreadyMerged", func(t *testing.T) {
		_, err := adapter.MergePR(ctx, iid, owner, repo, domain.StrategySquash, "", "", currentSHA)
		adaptertest.AssertAlreadyMergedMarker(t, err)
	})
	if t.Failed() {
		t.Skip("MergePR_AlreadyMerged subtest failed; skipping DeleteBranch subtests")
	}

	// head is still present here because openMR opened the merge request
	// with source-branch removal disabled, so the forge did not reap it
	// on merge. This subtest exercises the adapter's own delete, not the
	// forge's asynchronous one.
	t.Run("DeleteBranch", func(t *testing.T) {
		if err := adapter.DeleteBranch(ctx, owner, repo, head); err != nil {
			t.Fatalf("DeleteBranch(%q): %v", head, err)
		}
	})
	if t.Failed() {
		t.Skip("DeleteBranch subtest failed; skipping DeleteBranch_Again")
	}

	t.Run("DeleteBranch_Again", func(t *testing.T) {
		err := adapter.DeleteBranch(ctx, owner, repo, head)
		adaptertest.AssertBranchAbsentDisposition(t, err)
	})
}
