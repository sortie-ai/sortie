package gitea

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// seededFailingTargetURL is the target_url the provisioning script attaches to
// the failing status on the review PR head. The D3 reconciliation asserts it
// round-trips to the failing check run's DetailsURL, proving the per-status
// field name. The committed provisioning script hard-codes the same value.
const seededFailingTargetURL = "https://ci.example.com/build/12345"

// manyStatusCount is the number of distinct-context statuses the provisioning
// script seeds on the pagination-probe commit. It exceeds DEFAULT_PAGING_NUM
// (30) and MAX_RESPONSE_ITEMS (50), so the D4 reconciliation proves whether the
// single-GET combined-status read truncates. The committed provisioning script
// seeds the same count.
const manyStatusCount = 51

// seededOldSideCommentBody is the body of the human review's old-side inline
// comment. The provisioning script fixes the same value so the test can
// locate the comment by text rather than by a platform-assigned id.
const seededOldSideCommentBody = "Reviewer inline comment on the sentinel file's old side."

// seededPreImageLine and seededPostImageLine are the sentinel file's line
// numbers before and after the provisioning script's edit: it modifies
// seededPreImageLine and inserts one line above it, so the two numbers
// differ by one. The old-side comment anchors at the pre-image line, the
// new-side comments (human and bot) anchor at the post-image line. The
// committed provisioning script hard-codes the same two numbers.
const (
	seededPreImageLine  = 4
	seededPostImageLine = 5
)

// newIntegrationSCMAdapter constructs a live Gitea SCM adapter from the
// integration env vars, carrying the project preflight hint.
func newIntegrationSCMAdapter(t *testing.T) domain.SCMAdapter {
	t.Helper()
	adapter, err := NewGiteaSCMAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGiteaSCMAdapter: %v", err)
	}
	return adapter
}

// newIntegrationCIProvider constructs a live Gitea CI status provider from the
// integration env vars with the given log-line budget.
func newIntegrationCIProvider(t *testing.T, maxLogLines int) domain.CIStatusProvider {
	t.Helper()
	provider, err := NewGiteaCIProvider(maxLogLines, integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGiteaCIProvider: %v", err)
	}
	return provider
}

// integrationOwnerRepo splits SORTIE_GITEA_PROJECT into its owner and repo.
func integrationOwnerRepo(t *testing.T) (string, string) {
	t.Helper()
	project := requireEnv(t, "SORTIE_GITEA_PROJECT")
	owner, repo, ok := strings.Cut(project, "/")
	if !ok || owner == "" || repo == "" {
		t.Fatalf("SORTIE_GITEA_PROJECT=%q must be owner/repo", project)
	}
	return owner, repo
}

// integrationPRNumber parses SORTIE_GITEA_PR_NUMBER and skips the test cleanly
// when it is unset, so the suite stays portable to a lab with no seeded PR.
func integrationPRNumber(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("SORTIE_GITEA_PR_NUMBER")
	if raw == "" {
		t.Skip("skipping: SORTIE_GITEA_PR_NUMBER not set; seed a review PR to enable")
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("SORTIE_GITEA_PR_NUMBER=%q is not a valid integer: %v", raw, err)
	}
	return n
}

// integrationBotUsername reads SORTIE_GITEA_BOT_USERNAME and skips the test
// cleanly when it is unset.
func integrationBotUsername(t *testing.T) string {
	t.Helper()
	v := os.Getenv("SORTIE_GITEA_BOT_USERNAME")
	if v == "" {
		t.Skip("skipping: SORTIE_GITEA_BOT_USERNAME not set; seed a bot reviewer to enable")
	}
	return v
}

// integrationManyStatusSHA reads SORTIE_GITEA_MANY_STATUS_SHA and skips the test
// cleanly when it is unset.
func integrationManyStatusSHA(t *testing.T) string {
	t.Helper()
	v := os.Getenv("SORTIE_GITEA_MANY_STATUS_SHA")
	if v == "" {
		t.Skip("skipping: SORTIE_GITEA_MANY_STATUS_SHA not set; seed a many-status commit to enable")
	}
	return v
}

func TestIntegration_FetchPendingReviews(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	comments, err := adapter.FetchPendingReviews(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("FetchPendingReviews(PR #%d): %v", prNumber, err)
	}
	if comments == nil {
		t.Fatal("FetchPendingReviews returned nil, want non-nil slice")
	}

	bot := os.Getenv("SORTIE_GITEA_BOT_USERNAME")
	var hasReviewBody, hasInline, hasOldSide bool
	for _, c := range comments {
		if strings.HasPrefix(c.ID, "review-") {
			if c.FilePath != "" {
				t.Errorf("review-body comment %s FilePath = %q, want empty", c.ID, c.FilePath)
			}
			hasReviewBody = true
			continue
		}
		if c.FilePath == "" {
			t.Errorf("inline comment %s FilePath = %q, want non-empty", c.ID, c.FilePath)
			continue
		}
		if c.StartLine <= 0 {
			t.Errorf("inline comment %s StartLine = %d, want > 0", c.ID, c.StartLine)
		}
		if c.Outdated {
			t.Errorf("inline comment %s Outdated = true, want false for a current-diff line", c.ID)
		}
		if c.Reviewer == "" {
			t.Errorf("inline comment %s Reviewer is empty, want the human reviewer login", c.ID)
		}
		if bot != "" && strings.EqualFold(c.Reviewer, bot) {
			t.Errorf("inline comment %s Reviewer = %q, want the human reviewer, not the bot", c.ID, c.Reviewer)
		}
		hasInline = true

		if c.Body == seededOldSideCommentBody {
			hasOldSide = true
			if c.StartLine != seededPreImageLine {
				t.Errorf("old-side comment %s StartLine = %d, want %d (pre-image line)", c.ID, c.StartLine, seededPreImageLine)
			}
			if c.Outdated {
				t.Errorf("old-side comment %s Outdated = true, want false", c.ID)
			}
		}
	}
	if !hasReviewBody {
		t.Error("FetchPendingReviews returned no review-<id> body entry for the seeded REQUEST_CHANGES review")
	}
	if !hasInline {
		t.Error("FetchPendingReviews returned no inline review comment with a file path")
	}
	if !hasOldSide {
		t.Errorf("FetchPendingReviews returned no comment with body %q, want the seeded old-side comment", seededOldSideCommentBody)
	}
}

func TestIntegration_FetchBotReviewComments(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	botUsername := integrationBotUsername(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	comments, err := adapter.FetchBotReviewComments(ctx, prNumber, owner, repo, []string{botUsername})
	if err != nil {
		t.Fatalf("FetchBotReviewComments(PR #%d, [%s]): %v", prNumber, botUsername, err)
	}
	if comments == nil {
		t.Fatal("FetchBotReviewComments returned nil, want non-nil slice")
	}
	var found, foundInline bool
	for _, c := range comments {
		if !strings.EqualFold(c.Reviewer, botUsername) {
			continue
		}
		found = true
		if c.FilePath == "" {
			continue
		}
		foundInline = true
		if c.StartLine != seededPostImageLine {
			t.Errorf("bot inline comment %s StartLine = %d, want %d (post-image line)", c.ID, c.StartLine, seededPostImageLine)
		}
	}
	if !found {
		t.Errorf("FetchBotReviewComments(PR #%d, [%s]) returned no comment authored by the bot", prNumber, botUsername)
	}
	if !foundInline {
		t.Errorf("FetchBotReviewComments(PR #%d, [%s]) returned no inline comment authored by the bot", prNumber, botUsername)
	}

	empty, err := adapter.FetchBotReviewComments(ctx, prNumber, owner, repo, []string{})
	if err != nil {
		t.Fatalf("FetchBotReviewComments(PR #%d, empty allowlist): %v", prNumber, err)
	}
	if len(empty) != 0 {
		t.Errorf("FetchBotReviewComments(PR #%d, empty allowlist) returned %d comments, want 0", prNumber, len(empty))
	}
}

func TestIntegration_GetReviewDecision(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decision, err := adapter.GetReviewDecision(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("GetReviewDecision(PR #%d): %v", prNumber, err)
	}
	if decision != domain.ReviewDecisionChangesRequested {
		t.Errorf("GetReviewDecision(PR #%d) = %q, want %q", prNumber, decision, domain.ReviewDecisionChangesRequested)
	}
}

func TestIntegration_GetMergeability(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := adapter.GetMergeability(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("GetMergeability(PR #%d): %v", prNumber, err)
	}
	if status.HeadSHA == "" {
		t.Errorf("GetMergeability(PR #%d).HeadSHA = %q, want non-empty", prNumber, status.HeadSHA)
	}
	if status.BranchName == "" {
		t.Errorf("GetMergeability(PR #%d).BranchName = %q, want non-empty", prNumber, status.BranchName)
	}
	if status.BaseBranch == "" {
		t.Errorf("GetMergeability(PR #%d).BaseBranch = %q, want non-empty", prNumber, status.BaseBranch)
	}
	switch status.Mergeability {
	case domain.MergeabilityClean, domain.MergeabilityBlocked, domain.MergeabilityUnknown:
	default:
		t.Errorf("GetMergeability(PR #%d).Mergeability = %q, want one of clean/blocked/unknown", prNumber, status.Mergeability)
	}
}

func TestIntegration_GetCIStatus(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := adapter.GetCIStatus(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("GetCIStatus(PR #%d): %v", prNumber, err)
	}
	if status != "failing" {
		t.Errorf("GetCIStatus(PR #%d) = %q, want %q", prNumber, status, "failing")
	}
}

func TestIntegration_FetchCIStatus(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)
	provider := newIntegrationCIProvider(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Commit A (the PR head) carries the small determinate status set, so the
	// failing entry is always on page one, decoupled from the D4 probe commit.
	merge, err := adapter.GetMergeability(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("GetMergeability(PR #%d): %v", prNumber, err)
	}
	if merge.HeadSHA == "" {
		t.Fatalf("GetMergeability(PR #%d).HeadSHA is empty; cannot resolve commit A", prNumber)
	}

	result, err := provider.FetchCIStatus(ctx, merge.HeadSHA)
	if err != nil {
		t.Fatalf("FetchCIStatus(%q): %v", merge.HeadSHA, err)
	}
	if result.Status != domain.CIStatusFailing {
		t.Errorf("FetchCIStatus(%q).Status = %q, want %q", merge.HeadSHA, result.Status, domain.CIStatusFailing)
	}
	if result.FailingCount < 1 {
		t.Errorf("FetchCIStatus(%q).FailingCount = %d, want >= 1", merge.HeadSHA, result.FailingCount)
	}
	if result.CheckRuns == nil {
		t.Fatal("FetchCIStatus CheckRuns is nil, want non-nil slice")
	}

	var failingURL string
	for _, run := range result.CheckRuns {
		if run.Conclusion == domain.CheckConclusionFailure {
			failingURL = run.DetailsURL
		}
	}
	if failingURL != seededFailingTargetURL {
		t.Errorf("failing CheckRun.DetailsURL = %q, want %q", failingURL, seededFailingTargetURL)
	}
}

func TestIntegration_FetchCIStatus_ManyStatuses(t *testing.T) {
	skipUnlessIntegration(t)
	sha := integrationManyStatusSHA(t)
	provider := newIntegrationCIProvider(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := provider.FetchCIStatus(ctx, sha)
	if err != nil {
		t.Fatalf("FetchCIStatus(%q): %v", sha, err)
	}
	if len(result.CheckRuns) != manyStatusCount {
		t.Errorf("len(FetchCIStatus(%q).CheckRuns) = %d, want %d", sha, len(result.CheckRuns), manyStatusCount)
	}
}

func TestIntegration_ListLabelEvents(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := adapter.ListLabelEvents(ctx, prNumber, owner, repo)
	if err != nil {
		t.Fatalf("ListLabelEvents(PR #%d): %v", prNumber, err)
	}
	if events == nil {
		t.Fatal("ListLabelEvents returned nil, want non-nil slice")
	}

	var hasAdd, hasRemove bool
	for _, e := range events {
		if e.Label == "" {
			t.Errorf("label event %s Label is empty, want non-empty", e.ID)
		}
		if e.Label != strings.ToLower(e.Label) {
			t.Errorf("label event %s Label = %q, want lowercased", e.ID, e.Label)
		}
		if e.ID == "" {
			t.Error("label event has empty ID")
		}
		if e.Added {
			hasAdd = true
		} else {
			hasRemove = true
		}
	}
	if !hasAdd {
		t.Error("ListLabelEvents returned no Added==true event for the seeded label")
	}
	if !hasRemove {
		t.Error("ListLabelEvents returned no Added==false event for the seeded label")
	}

	for i := 1; i < len(events); i++ {
		if events[i].At.Before(events[i-1].At) {
			t.Errorf("label events not oldest-first at index %d: %s before %s",
				i, events[i].At.Format(time.RFC3339), events[i-1].At.Format(time.RFC3339))
		}
	}
}

func TestIntegration_VerifyAutoMergeScopes(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationSCMAdapter(t)

	scmAdapter, ok := adapter.(*GiteaSCMAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want *GiteaSCMAdapter", adapter)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopes, missing, err := scmAdapter.VerifyAutoMergeScopes(ctx, true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("VerifyAutoMergeScopes scopes = %v, want nil; Gitea exposes no scope surface", scopes)
	}
	if len(missing) != 0 {
		t.Errorf("VerifyAutoMergeScopes missing = %v, want empty; the token user has permissions.push", missing)
	}
}

func TestIntegration_RemoveLabel(t *testing.T) {
	skipUnlessIntegration(t)
	prNumber := integrationPRNumber(t)
	owner, repo := integrationOwnerRepo(t)
	scmAdapter := newIntegrationSCMAdapter(t)
	trackerAdapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// PRs share the issue index space, so a probe label attached through the
	// tracker adapter can be removed through the SCM adapter. The test re-attaches
	// on every run, so it is self-contained and re-runnable.
	const probeLabel = "scope-probe"

	if err := trackerAdapter.AddLabel(ctx, strconv.Itoa(prNumber), probeLabel); err != nil {
		t.Fatalf("AddLabel(PR #%d, %q): %v", prNumber, probeLabel, err)
	}

	if err := scmAdapter.RemoveLabel(ctx, prNumber, owner, repo, probeLabel); err != nil {
		t.Fatalf("RemoveLabel(PR #%d, %q): %v", prNumber, probeLabel, err)
	}
	if err := scmAdapter.RemoveLabel(ctx, prNumber, owner, repo, probeLabel); err != nil {
		t.Fatalf("RemoveLabel(PR #%d, %q) second call = %v, want nil (already-absent no-op)", prNumber, probeLabel, err)
	}
}
