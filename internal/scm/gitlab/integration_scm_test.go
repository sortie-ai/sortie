package gitlab

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// manyStatusCount is the total number of commit statuses the
// provisioning script seeds on the pagination-probe commit: 101
// succeeding, one failing, and one running. The count sits above 100,
// the page size the CI provider requests per page, so a multi-status
// read that reports exactly this many entries proves the walk crossed a
// page boundary rather than stopping at the provider's own page size.
// The committed provisioning script seeds the same count.
const manyStatusCount = 103

// seededFailingTargetURL is the target_url the provisioning script
// attaches to the failing status on the forge merge request head. The
// integration suite asserts it round-trips to the failing check run's
// DetailsURL, proving the per-status field name. The committed
// provisioning script hard-codes the same value.
const seededFailingTargetURL = "https://ci.example.com/build/12345"

// seededJournalLabel is the label the provisioning script adds and then
// removes on the forge merge request, producing the two-entry
// label-event journal the suite reads.
const seededJournalLabel = "forge-journal"

// probeMixedCaseLabel is the mixed-case label the RemoveLabel test
// attaches through the merge-flow fixture client and then removes with
// its lowercased spelling, pinning the case-insensitive match plus
// stored-casing write RemoveLabel performs.
const probeMixedCaseLabel = "Forge:Probe"

// newIntegrationSCMAdapter constructs a live GitLab SCM adapter from the
// integration env vars.
func newIntegrationSCMAdapter(t *testing.T) domain.SCMAdapter {
	t.Helper()
	adapter, err := NewGitLabSCMAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGitLabSCMAdapter: %v", err)
	}
	return adapter
}

// newIntegrationCIProvider constructs a live GitLab CI status provider
// from the integration env vars with the given log-line budget.
func newIntegrationCIProvider(t *testing.T, maxLogLines int) domain.CIStatusProvider {
	t.Helper()
	provider, err := NewGitLabCIProvider(maxLogLines, integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGitLabCIProvider: %v", err)
	}
	return provider
}

// integrationSCMAdapterConcrete returns the live adapter as
// *GitLabSCMAdapter, reaching VerifyAutoMergeScopes, which is declared on
// the concrete type rather than on domain.SCMAdapter, without importing
// the orchestrator package that declares the wider interface.
func integrationSCMAdapterConcrete(t *testing.T) *GitLabSCMAdapter {
	t.Helper()
	adapter := newIntegrationSCMAdapter(t)
	concrete, ok := adapter.(*GitLabSCMAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want *GitLabSCMAdapter", adapter)
	}
	return concrete
}

// integrationOwnerRepo splits SORTIE_GITLAB_PROJECT at its last "/" into
// the namespace path and the project path. The forge fixture project
// lives at namespace depth three, so a first-"/" split would cut the
// namespace segment short.
func integrationOwnerRepo(t *testing.T) (owner, repo string) {
	t.Helper()
	project := requireEnv(t, "SORTIE_GITLAB_PROJECT")
	idx := strings.LastIndex(project, "/")
	if idx < 0 {
		t.Fatalf("SORTIE_GITLAB_PROJECT=%q must be owner/repo", project)
	}
	owner, repo = project[:idx], project[idx+1:]
	if owner == "" || repo == "" {
		t.Fatalf("SORTIE_GITLAB_PROJECT=%q must be owner/repo", project)
	}
	return owner, repo
}

// integrationMRIID reads the named merge-request iid coordinate,
// skipping the test cleanly when it is unset.
func integrationMRIID(t *testing.T, key string) int {
	t.Helper()
	raw := optionalEnv(t, key)
	iid, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a valid integer: %v", key, raw, err)
	}
	return iid
}

func TestIntegration_FetchPendingReviews(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	draftIID := integrationMRIID(t, "SORTIE_GITLAB_DRAFT_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("forge_approved", func(t *testing.T) {
		got, err := adapter.FetchPendingReviews(ctx, forgeIID, owner, repo)
		if err != nil {
			t.Fatalf("FetchPendingReviews(!%d): %v", forgeIID, err)
		}
		adaptertest.AssertEmptyNonNil(t, got, "FetchPendingReviews")
	})

	t.Run("draft_unreviewed", func(t *testing.T) {
		got, err := adapter.FetchPendingReviews(ctx, draftIID, owner, repo)
		if err != nil {
			t.Fatalf("FetchPendingReviews(!%d): %v", draftIID, err)
		}
		adaptertest.AssertEmptyNonNil(t, got, "FetchPendingReviews")
	})
}

// forgeSentinelPath is the file the provisioning script commits on the
// forge merge request head and attaches the bot's inline discussion to.
const forgeSentinelPath = "forge-sentinel.txt"

func TestIntegration_FetchBotReviewComments(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	botUsername := optionalEnv(t, "SORTIE_GITLAB_BOT_USERNAME")
	humanUsername := optionalEnv(t, "SORTIE_GITLAB_HUMAN_USERNAME")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assertBotComments := func(t *testing.T, comments []domain.ReviewComment) {
		t.Helper()
		var hasPlain, hasInline bool
		for _, c := range comments {
			if strings.EqualFold(c.Reviewer, humanUsername) {
				t.Errorf("comment %s Reviewer = %q, want no human-authored comment", c.ID, c.Reviewer)
			}
			switch c.FilePath {
			case "":
				hasPlain = true
			case forgeSentinelPath:
				if c.StartLine != 1 {
					t.Errorf("inline comment %s StartLine = %d, want 1", c.ID, c.StartLine)
				}
				if c.Outdated {
					t.Errorf("inline comment %s Outdated = true, want false", c.ID)
				}
				hasInline = true
			}
		}
		if !hasPlain {
			t.Error("no plain (non-inline) bot comment found")
		}
		if !hasInline {
			t.Errorf("no inline bot comment on %q found", forgeSentinelPath)
		}
	}

	t.Run("allowlisted", func(t *testing.T) {
		comments, err := adapter.FetchBotReviewComments(ctx, forgeIID, owner, repo, []string{botUsername})
		if err != nil {
			t.Fatalf("FetchBotReviewComments(!%d, [%s]): %v", forgeIID, botUsername, err)
		}
		if comments == nil {
			t.Fatal("FetchBotReviewComments returned nil, want non-nil slice")
		}
		assertBotComments(t, comments)
	})

	t.Run("empty_allowlist", func(t *testing.T) {
		comments, err := adapter.FetchBotReviewComments(ctx, forgeIID, owner, repo, nil)
		if err != nil {
			t.Fatalf("FetchBotReviewComments(!%d, nil allowlist): %v", forgeIID, err)
		}
		if comments == nil {
			t.Fatal("FetchBotReviewComments returned nil, want non-nil slice")
		}
		// GitLab qualifies an author by the platform bot flag alone, so
		// an empty allowlist still selects the bot's comments, unlike
		// the Gitea sibling suite.
		assertBotComments(t, comments)
	})
}

func TestIntegration_GetReviewDecision(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	draftIID := integrationMRIID(t, "SORTIE_GITLAB_DRAFT_MR_IID")
	conflictIID := integrationMRIID(t, "SORTIE_GITLAB_CONFLICT_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tests := []struct {
		name string
		iid  int
		want domain.ReviewDecision
	}{
		{"forge_approved", forgeIID, domain.ReviewDecisionApproved},
		{"draft_review_required", draftIID, domain.ReviewDecisionReviewRequired},
		{"conflict_not_required", conflictIID, domain.ReviewDecisionNotRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.GetReviewDecision(ctx, tt.iid, owner, repo)
			if err != nil {
				t.Fatalf("GetReviewDecision(!%d): %v", tt.iid, err)
			}
			if got != tt.want {
				t.Errorf("GetReviewDecision(!%d) = %q, want %q", tt.iid, got, tt.want)
			}
		})
	}
}

func TestIntegration_GetMergeability(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	draftIID := integrationMRIID(t, "SORTIE_GITLAB_DRAFT_MR_IID")
	conflictIID := integrationMRIID(t, "SORTIE_GITLAB_CONFLICT_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("forge_clean", func(t *testing.T) {
		status, err := adapter.GetMergeability(ctx, forgeIID, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(!%d): %v", forgeIID, err)
		}
		if status.Mergeability != domain.MergeabilityClean {
			t.Errorf("GetMergeability(!%d).Mergeability = %q, want %q", forgeIID, status.Mergeability, domain.MergeabilityClean)
		}
		if status.HeadSHA == "" {
			t.Errorf("GetMergeability(!%d).HeadSHA is empty, want non-empty", forgeIID)
		}
		if status.BranchName != "forge-fixture" {
			t.Errorf("GetMergeability(!%d).BranchName = %q, want %q", forgeIID, status.BranchName, "forge-fixture")
		}
		if status.BaseBranch == "" {
			t.Errorf("GetMergeability(!%d).BaseBranch is empty, want non-empty", forgeIID)
		}
		if status.Draft {
			t.Errorf("GetMergeability(!%d).Draft = true, want false", forgeIID)
		}
		if status.Merged {
			t.Errorf("GetMergeability(!%d).Merged = true, want false", forgeIID)
		}
		if status.MergeCommitSHA != "" {
			t.Errorf("GetMergeability(!%d).MergeCommitSHA = %q, want empty", forgeIID, status.MergeCommitSHA)
		}
	})

	t.Run("draft_blocked", func(t *testing.T) {
		concrete, ok := adapter.(*GitLabSCMAdapter)
		if !ok {
			t.Fatalf("adapter type = %T, want *GitLabSCMAdapter", adapter)
		}
		prevLog := concrete.log
		log, buf := newCapturingLogger()
		concrete.log = log
		defer func() { concrete.log = prevLog }()

		status, err := adapter.GetMergeability(ctx, draftIID, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(!%d): %v", draftIID, err)
		}
		if status.Mergeability != domain.MergeabilityBlocked {
			t.Errorf("GetMergeability(!%d).Mergeability = %q, want %q", draftIID, status.Mergeability, domain.MergeabilityBlocked)
		}
		if !status.Draft {
			t.Errorf("GetMergeability(!%d).Draft = false, want true", draftIID)
		}

		// A settled draft on this deployment reports draft_status on
		// every read, so the Debug record's attribute pins the observed
		// wire value positively rather than resting on the WARN's
		// absence alone, which would pass identically for an unsettled
		// computing value.
		logOutput := buf.String()
		if want := "detailed_merge_status=draft_status"; !strings.Contains(logOutput, want) {
			t.Errorf("GetMergeability(!%d) log output = %q, want it to carry %q", draftIID, logOutput, want)
		}
		const debugMsg = "gitlab reported a non-mergeable detailed_merge_status"
		if n := strings.Count(logOutput, debugMsg); n != 1 {
			t.Errorf("GetMergeability(!%d): occurrences of %q in log output = %d, want 1 (log output: %q)", draftIID, debugMsg, n, logOutput)
		}
		if strings.Contains(logOutput, "unrecognized gitlab detailed_merge_status value") {
			t.Errorf("GetMergeability(!%d) log output = %q, want no unrecognized-value WARN", draftIID, logOutput)
		}
	})

	t.Run("conflict_dirty", func(t *testing.T) {
		status, err := adapter.GetMergeability(ctx, conflictIID, owner, repo)
		if err != nil {
			t.Fatalf("GetMergeability(!%d): %v", conflictIID, err)
		}
		if status.Mergeability != domain.MergeabilityDirty {
			t.Errorf("GetMergeability(!%d).Mergeability = %q, want %q", conflictIID, status.Mergeability, domain.MergeabilityDirty)
		}
	})
}

func TestIntegration_GetCIStatus(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	draftIID := integrationMRIID(t, "SORTIE_GITLAB_DRAFT_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tests := []struct {
		name string
		iid  int
		want string
	}{
		{"forge_failing", forgeIID, "failing"},
		{"draft_no_checks", draftIID, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.GetCIStatus(ctx, tt.iid, owner, repo)
			if err != nil {
				t.Fatalf("GetCIStatus(!%d): %v", tt.iid, err)
			}
			if got != tt.want {
				t.Errorf("GetCIStatus(!%d) = %q, want %q", tt.iid, got, tt.want)
			}
		})
	}
}

func TestIntegration_FetchCIStatus(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)
	provider := newIntegrationCIProvider(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	merge, err := adapter.GetMergeability(ctx, forgeIID, owner, repo)
	if err != nil {
		t.Fatalf("GetMergeability(!%d): %v", forgeIID, err)
	}
	if merge.HeadSHA == "" {
		t.Fatalf("GetMergeability(!%d).HeadSHA is empty; cannot resolve commit A", forgeIID)
	}

	result, err := provider.FetchCIStatus(ctx, merge.HeadSHA)
	if err != nil {
		t.Fatalf("FetchCIStatus(%q): %v", merge.HeadSHA, err)
	}
	if result.Status != domain.CIStatusFailing {
		t.Errorf("FetchCIStatus(%q).Status = %q, want %q", merge.HeadSHA, result.Status, domain.CIStatusFailing)
	}
	if result.FailingCount != 1 {
		t.Errorf("FetchCIStatus(%q).FailingCount = %d, want 1", merge.HeadSHA, result.FailingCount)
	}
	if result.CheckRuns == nil {
		t.Fatal("FetchCIStatus CheckRuns is nil, want non-nil slice")
	}
	if len(result.CheckRuns) != 2 {
		t.Errorf("len(FetchCIStatus(%q).CheckRuns) = %d, want 2", merge.HeadSHA, len(result.CheckRuns))
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
	if result.Ref != merge.HeadSHA {
		t.Errorf("FetchCIStatus(%q).Ref = %q, want %q", merge.HeadSHA, result.Ref, merge.HeadSHA)
	}

	adaptertest.AssertCIAggregateMatchesCore(t, result)
}

func TestIntegration_FetchCIStatus_ManyStatuses(t *testing.T) {
	skipUnlessIntegration(t)

	sha := optionalEnv(t, "SORTIE_GITLAB_MANY_STATUS_SHA")
	provider := newIntegrationCIProvider(t, 50)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := provider.FetchCIStatus(ctx, sha)
	if err != nil {
		t.Fatalf("FetchCIStatus(%q): %v", sha, err)
	}
	if len(result.CheckRuns) != manyStatusCount {
		t.Errorf("len(FetchCIStatus(%q).CheckRuns) = %d, want %d", sha, len(result.CheckRuns), manyStatusCount)
	}

	var hasSuccess, hasFailure, hasInProgress bool
	for _, run := range result.CheckRuns {
		switch {
		case run.Status == domain.CheckRunStatusCompleted && run.Conclusion == domain.CheckConclusionSuccess:
			hasSuccess = true
		case run.Status == domain.CheckRunStatusCompleted && run.Conclusion == domain.CheckConclusionFailure:
			hasFailure = true
		case run.Status == domain.CheckRunStatusInProgress:
			hasInProgress = true
		}
	}
	if !hasSuccess {
		t.Error("FetchCIStatus many-statuses result has no completed-success run")
	}
	if !hasFailure {
		t.Error("FetchCIStatus many-statuses result has no completed-failure run")
	}
	if !hasInProgress {
		t.Error("FetchCIStatus many-statuses result has no in-progress run")
	}

	adaptertest.AssertCIAggregateMatchesCore(t, result)

	if result.LogExcerpt != "" {
		t.Errorf("FetchCIStatus(%q).LogExcerpt = %q, want empty; no failing entry is a pipeline job", sha, result.LogExcerpt)
	}
}

func TestIntegration_ListLabelEvents(t *testing.T) {
	skipUnlessIntegration(t)

	forgeIID := integrationMRIID(t, "SORTIE_GITLAB_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := adapter.ListLabelEvents(ctx, forgeIID, owner, repo)
	if err != nil {
		t.Fatalf("ListLabelEvents(!%d): %v", forgeIID, err)
	}
	if events == nil {
		t.Fatal("ListLabelEvents returned nil, want non-nil slice")
	}
	if len(events) != 2 {
		t.Fatalf("ListLabelEvents(!%d) returned %d events, want 2", forgeIID, len(events))
	}
	if !events[0].Added {
		t.Errorf("ListLabelEvents(!%d)[0].Added = false, want true", forgeIID)
	}
	if events[1].Added {
		t.Errorf("ListLabelEvents(!%d)[1].Added = true, want false", forgeIID)
	}
	for i, e := range events {
		if e.Label != seededJournalLabel {
			t.Errorf("event[%d].Label = %q, want %q", i, e.Label, seededJournalLabel)
		}
		if e.Label != strings.ToLower(e.Label) {
			t.Errorf("event[%d].Label = %q, want lowercased", i, e.Label)
		}
		if e.ID == "" {
			t.Errorf("event[%d].ID is empty, want non-empty", i)
		}
		if e.Actor == "" {
			t.Errorf("event[%d].Actor is empty, want non-empty", i)
		}
	}

	adaptertest.AssertLabelEventsOrdered(t, events)
}

func TestIntegration_RemoveLabel(t *testing.T) {
	skipUnlessIntegration(t)

	labelIID := integrationMRIID(t, "SORTIE_GITLAB_LABEL_MR_IID")
	owner, repo := integrationOwnerRepo(t)
	adapter := integrationSCMAdapterConcrete(t)
	fixture := newGitLabMergeFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fixture.addLabel(t, projectPath(owner, repo), labelIID, probeMixedCaseLabel)

	lowered := strings.ToLower(probeMixedCaseLabel)
	if err := adapter.RemoveLabel(ctx, labelIID, owner, repo, lowered); err != nil {
		t.Fatalf("RemoveLabel(!%d, %q): %v", labelIID, lowered, err)
	}

	mr, err := adapter.fetchMergeRequest(ctx, labelIID, owner, repo)
	if err != nil {
		t.Fatalf("fetchMergeRequest(!%d): %v", labelIID, err)
	}
	if slices.ContainsFunc(mr.Labels, func(l string) bool { return strings.EqualFold(l, probeMixedCaseLabel) }) {
		t.Errorf("merge request !%d labels = %v, want %q removed", labelIID, mr.Labels, probeMixedCaseLabel)
	}

	err = adapter.RemoveLabel(ctx, labelIID, owner, repo, lowered)
	adaptertest.AssertLabelAbsentDisposition(t, err)
}

func TestIntegration_VerifyAutoMergeScopes(t *testing.T) {
	skipUnlessIntegration(t)
	// SORTIE_GITLAB_MR_IID is read only as the provisioned-instance
	// marker: this test consumes no seeded object of its own.
	_ = integrationMRIID(t, "SORTIE_GITLAB_MR_IID")

	adapter := integrationSCMAdapterConcrete(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopes, missing, err := adapter.VerifyAutoMergeScopes(ctx, true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if !slices.Contains(scopes, "api") {
		t.Errorf("VerifyAutoMergeScopes scopes = %v, want to contain %q", scopes, "api")
	}
	if len(missing) != 0 {
		t.Errorf("VerifyAutoMergeScopes missing = %v, want empty", missing)
	}
}

func TestIntegration_SCMErrorKinds(t *testing.T) {
	skipUnlessIntegration(t)
	// SORTIE_GITLAB_MR_IID is read only as the provisioned-instance
	// marker: this test consumes no seeded object of its own.
	_ = integrationMRIID(t, "SORTIE_GITLAB_MR_IID")

	owner, repo := integrationOwnerRepo(t)
	adapter := newIntegrationSCMAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("not_found", func(t *testing.T) {
		const nonexistentIID = 999999
		_, err := adapter.GetMergeability(ctx, nonexistentIID, owner, repo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMNotFound)
	})

	t.Run("auth", func(t *testing.T) {
		cfg := integrationConfig(t)
		cfg["api_key"] = "glpat-integration-suite-unknown-token"
		badAdapter, err := NewGitLabSCMAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabSCMAdapter with unknown token: %v", err)
		}
		_, err = badAdapter.GetMergeability(ctx, 1, owner, repo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
	})

	t.Run("payload_empty_expected_head_sha", func(t *testing.T) {
		_, err := adapter.MergePR(ctx, 1, owner, repo, domain.StrategySquash, "", "", "")
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}
