package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

func normalizeIssueUnder(gi githubIssue, active, terminal []string) domain.Issue {
	model := issuekit.NewLabelStateModel(issuekit.NewStateSource(domain.TrackerStates{Active: active, Terminal: terminal}), nil)
	return normalizeIssue(gi, model, model.Snapshot())
}

func normalizeBlockersUnder(blockers []githubIssue, active, terminal []string) []domain.BlockerRef {
	model := issuekit.NewLabelStateModel(issuekit.NewStateSource(domain.TrackerStates{Active: active, Terminal: terminal}), nil)
	return normalizeBlockers(blockers, model, model.Snapshot(), "owner", "repo")
}

func typePtr(name string) *githubIssueType { return &githubIssueType{Name: name} }
func prMarker() *githubPR                  { return &githubPR{} }

func TestNormalizeIssue_AllFields(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		ID:        987654,
		Number:    42,
		Title:     "Add dark mode",
		Body:      new("Users want dark mode."),
		State:     "open",
		HTMLURL:   "https://github.com/owner/repo/issues/42",
		Labels:    []githubLabel{{Name: "In-Progress"}, {Name: "UI"}},
		Assignees: []githubUser{{Login: "alice"}},
		Type:      typePtr("Feature"),
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
	}

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	got := normalizeIssueUnder(gi, active, terminal)

	// ID and Identifier are both the issue number (not global int64 ID).
	if got.ID != "42" {
		t.Errorf("ID = %q, want %q", got.ID, "42")
	}
	if got.Identifier != "42" {
		t.Errorf("Identifier = %q, want %q", got.Identifier, "42")
	}
	if got.Title != "Add dark mode" {
		t.Errorf("Title = %q, want %q", got.Title, "Add dark mode")
	}
	if got.Description != "Users want dark mode." {
		t.Errorf("Description = %q, want %q", got.Description, "Users want dark mode.")
	}
	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil (GitHub has no native priority)", got.Priority)
	}
	if got.State != "in-progress" {
		t.Errorf("State = %q, want %q", got.State, "in-progress")
	}
	if got.URL != "https://github.com/owner/repo/issues/42" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want %q", got.Assignee, "alice")
	}
	if got.IssueType != "Feature" {
		t.Errorf("IssueType = %q, want %q", got.IssueType, "Feature")
	}
	if got.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q", got.CreatedAt)
	}
	if got.UpdatedAt != "2026-01-02T00:00:00Z" {
		t.Errorf("UpdatedAt = %q", got.UpdatedAt)
	}
	if got.BranchName != "" {
		t.Errorf("BranchName = %q, want empty (not available from API)", got.BranchName)
	}

	// List-path defaults.
	if got.Parent != nil {
		t.Errorf("Parent = %v, want nil in list normalization", got.Parent)
	}
	if got.Comments != nil {
		t.Errorf("Comments = %v, want nil in list normalization", got.Comments)
	}
}

func TestNormalizeIssue_LabelsLowercased(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		Number: 1,
		State:  "open",
		Labels: []githubLabel{{Name: "BACKLOG"}, {Name: "Priority-High"}},
	}
	got := normalizeIssueUnder(gi, []string{"backlog"}, nil)

	if len(got.Labels) != 2 {
		t.Fatalf("Labels len = %d, want 2", len(got.Labels))
	}
	if got.Labels[0] != "backlog" {
		t.Errorf("Labels[0] = %q, want %q", got.Labels[0], "backlog")
	}
	if got.Labels[1] != "priority-high" {
		t.Errorf("Labels[1] = %q, want %q", got.Labels[1], "priority-high")
	}
}

func TestNormalizeIssue_NonNilEmptyLabels(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open", Labels: nil}
	got := normalizeIssueUnder(gi, nil, nil)

	if got.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(got.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(got.Labels))
	}
}

func TestNormalizeIssue_MultipleAssignees(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		Number:    1,
		State:     "open",
		Assignees: []githubUser{{Login: "alice"}, {Login: "bob"}},
	}
	got := normalizeIssueUnder(gi, nil, nil)

	// Only the first assignee is used.
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want first assignee %q", got.Assignee, "alice")
	}
}

// TestNormalizeIssue_NonNilEmptyBlockedBy pins that a candidate with no
// dependency summary carries a non-nil empty BlockedBy AND is marked
// unresolved: the empty slice alone is not authoritative here, because
// nothing on the candidate route read the dependencies route. A green
// assertion on the shape alone, without the flag, is the defect this
// design replaces.
func TestNormalizeIssue_NonNilEmptyBlockedBy(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open"}
	got := normalizeIssueUnder(gi, nil, nil)

	if got.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice in list normalization")
	}
	if len(got.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0", len(got.BlockedBy))
	}
	if !got.BlockersUnresolved {
		t.Error("BlockersUnresolved = false, want true: no DependenciesSummary was present to prove the issue has no dependencies")
	}
}

func TestNormalizeIssue_DisplayIDEmpty(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 42, State: "open"}
	got := normalizeIssueUnder(gi, nil, nil)

	// normalizeIssue must leave DisplayID empty; callers are responsible for
	// calling qualifyDisplayID to set the qualified form.
	if got.DisplayID != "" {
		t.Errorf("DisplayID = %q, want empty string after normalizeIssue", got.DisplayID)
	}
}

func TestQualifyDisplayID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		owner  string
		repo   string
		num    int
		wantID string
	}{
		{
			name:   "standard owner/repo#N format",
			owner:  "my-org",
			repo:   "my-repo",
			num:    9,
			wantID: "my-org/my-repo#9",
		},
		{
			name:   "dotted repo name",
			owner:  "sortie-ai",
			repo:   "sortie.test",
			num:    100,
			wantID: "sortie-ai/sortie.test#100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &GitHubAdapter{owner: tt.owner, repo: tt.repo}
			issue := domain.Issue{
				ID:         strconv.Itoa(tt.num),
				Identifier: strconv.Itoa(tt.num),
			}

			a.qualifyDisplayID(&issue)

			if issue.DisplayID != tt.wantID {
				t.Errorf("DisplayID = %q, want %q", issue.DisplayID, tt.wantID)
			}
		})
	}
}

func TestNormalizeBlockers_NonEmpty(t *testing.T) {
	t.Parallel()

	blockers := []githubIssue{
		{ID: 500, Number: 5, State: "open", Labels: []githubLabel{{Name: "in-progress"}}},
		{ID: 600, Number: 6, State: "closed", Labels: []githubLabel{{Name: "done"}}},
	}
	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	got := normalizeBlockersUnder(blockers, active, terminal)

	if len(got) != 2 {
		t.Fatalf("normalizeBlockers len = %d, want 2", len(got))
	}
	if got[0].ID != "5" || got[0].Identifier != "5" {
		t.Errorf("got[0] ID/Identifier = %q/%q, want 5/5", got[0].ID, got[0].Identifier)
	}
	if got[0].State != "in-progress" {
		t.Errorf("got[0].State = %q, want %q", got[0].State, "in-progress")
	}
	if got[1].ID != "6" {
		t.Errorf("got[1].ID = %q, want %q", got[1].ID, "6")
	}
	if got[1].State != "done" {
		t.Errorf("got[1].State = %q, want %q", got[1].State, "done")
	}
	if got[0].DisplayID != "owner/repo#5" {
		t.Errorf("got[0].DisplayID = %q, want %q", got[0].DisplayID, "owner/repo#5")
	}

	adaptertest.AssertBlockerRefsNormalized(t, got)
	qualifiedIssue := domain.Issue{Identifier: "1", DisplayID: "owner/repo#1"}
	adaptertest.AssertBlockerIdentifiersMatchIssue(t, qualifiedIssue, got)
}

func TestNormalizeBlockers_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	got := normalizeBlockersUnder(nil, nil, nil)

	if got == nil {
		t.Error("normalizeBlockers(nil) returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestNormalizeComments(t *testing.T) {
	t.Parallel()

	const bot = `{"login":"ci[bot]","type":%q}`
	const person = `{"login":"alice","type":%q}`
	const app = `,"performed_via_github_app":{"id":15368,"slug":"ci"}`

	tests := []struct {
		name     string
		user     string
		app      string
		wantID   string
		wantKind domain.CommentAuthorKind
	}{
		{"bot with app", fmt.Sprintf(bot, "Bot"), app, "ci[bot]", domain.CommentAuthorBot},
		{"bot without app", fmt.Sprintf(bot, "Bot"), "", "ci[bot]", domain.CommentAuthorBot},
		{"bot in mixed case", fmt.Sprintf(bot, "bOt"), app, "ci[bot]", domain.CommentAuthorBot},
		{"user with app", fmt.Sprintf(person, "User"), app, "alice", domain.CommentAuthorApp},
		{"user in mixed case with app", fmt.Sprintf(person, "uSER"), app, "alice", domain.CommentAuthorApp},
		{"user with null app", fmt.Sprintf(person, "User"), `,"performed_via_github_app":null`, "alice", domain.CommentAuthorPerson},
		{"user without app field", fmt.Sprintf(person, "User"), "", "alice", domain.CommentAuthorPerson},
		{"null user", "null", "", "", domain.CommentAuthorUnknown},
		{"organization", fmt.Sprintf(person, "Organization"), "", "alice", domain.CommentAuthorUnknown},
		{"mannequin", fmt.Sprintf(person, "Mannequin"), app, "alice", domain.CommentAuthorUnknown},
		{"empty type", fmt.Sprintf(person, ""), "", "alice", domain.CommentAuthorUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := fmt.Sprintf(`{"id":9001,"user":%s,"body":"Looks good!","created_at":"2026-01-15T10:00:00Z","updated_at":"2026-01-15T10:05:00Z"%s}`, tt.user, tt.app)
			var raw githubComment
			if err := json.Unmarshal([]byte(payload), &raw); err != nil {
				t.Fatalf("decoding %s: %v", payload, err)
			}

			got := normalizeComments([]githubComment{raw})

			want := domain.Comment{
				ID: "9001", Author: tt.wantID, AuthorID: tt.wantID, AuthorKind: tt.wantKind,
				Body: "Looks good!", CreatedAt: "2026-01-15T10:00:00Z", UpdatedAt: "2026-01-15T10:05:00Z",
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("normalizeComments(%s) = %+v, want [%+v]", payload, got, want)
			}
		})
	}
}

func TestNormalizeComments_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	got := normalizeComments(nil)

	if got == nil {
		t.Error("normalizeComments(nil) returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestIsPullRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		gi   githubIssue
		want bool
	}{
		{
			name: "pull_request field set is PR",
			gi:   githubIssue{Number: 1, PullRequest: prMarker()},
			want: true,
		},
		{
			name: "pull_request field nil is issue",
			gi:   githubIssue{Number: 2, PullRequest: nil},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isPullRequest(tt.gi)
			if got != tt.want {
				t.Errorf("isPullRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeIssue_DependenciesSummaryPreFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		summary            *githubDependenciesSummary
		wantBlockersUnres  bool
		wantBlockedByEmpty bool
	}{
		{
			name:               "absent summary leaves the flag set",
			summary:            nil,
			wantBlockersUnres:  true,
			wantBlockedByEmpty: true,
		},
		{
			name:               "zero total_blocked_by clears the flag",
			summary:            &githubDependenciesSummary{BlockedBy: 0, TotalBlockedBy: 0},
			wantBlockersUnres:  false,
			wantBlockedByEmpty: true,
		},
		{
			name:               "positive total_blocked_by leaves the flag set",
			summary:            &githubDependenciesSummary{BlockedBy: 1, TotalBlockedBy: 1},
			wantBlockersUnres:  true,
			wantBlockedByEmpty: true,
		},
		{
			name: "the reporter's exact divergence: blocked_by 0, total_blocked_by 2, both closed but configured non-terminal",
			// GitHub's own blocked_by count answers "how many of my
			// dependencies does GitHub itself consider still open",
			// which is not the question the dispatch gate asks: a
			// blocker GitHub calls closed can still be a configured
			// non-terminal state. Reading BlockedBy here would wrongly
			// clear the flag on an issue that in fact has two blockers.
			summary:            &githubDependenciesSummary{BlockedBy: 0, TotalBlockedBy: 2},
			wantBlockersUnres:  true,
			wantBlockedByEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gi := githubIssue{Number: 1, State: "open", DependenciesSummary: tt.summary}
			got := normalizeIssueUnder(gi, nil, nil)

			if got.BlockersUnresolved != tt.wantBlockersUnres {
				t.Errorf("BlockersUnresolved = %t, want %t", got.BlockersUnresolved, tt.wantBlockersUnres)
			}
			if tt.wantBlockedByEmpty && len(got.BlockedBy) != 0 {
				t.Errorf("len(BlockedBy) = %d, want 0", len(got.BlockedBy))
			}
			if got.BlockedBy == nil {
				t.Error("BlockedBy is nil, want non-nil empty slice on every candidate path")
			}
		})
	}
}

// TestNormalizeIssue_DependenciesSummaryNullDecodesToNil pins that a
// JSON null for issue_dependencies_summary decodes to a nil pointer,
// read the same nil-safe way an absent field is.
func TestNormalizeIssue_DependenciesSummaryNullDecodesToNil(t *testing.T) {
	t.Parallel()

	var gi githubIssue
	raw := []byte(`{"number":1,"state":"open","issue_dependencies_summary":null}`)
	if err := json.Unmarshal(raw, &gi); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if gi.DependenciesSummary != nil {
		t.Fatalf("DependenciesSummary = %v, want nil after decoding a JSON null", gi.DependenciesSummary)
	}

	got := normalizeIssueUnder(gi, nil, nil)
	if !got.BlockersUnresolved {
		t.Error("BlockersUnresolved = false, want true: a JSON null summary proves nothing about dependencies")
	}
}

// TestNormalizeIssue_PreFilterDecisionEquality pins that for an
// issue whose summary proves zero dependencies, the pre-filter's
// cheap answer and an actual per-issue blocker read of the same
// zero-dependency issue produce the identical dispatch-relevant
// fields, so turning the pre-filter off would not change the
// decision, only its cost.
func TestNormalizeIssue_PreFilterDecisionEquality(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		Number:              1,
		State:               "open",
		DependenciesSummary: &githubDependenciesSummary{BlockedBy: 0, TotalBlockedBy: 0},
	}
	preFiltered := normalizeIssueUnder(gi, nil, nil)

	// The per-issue read path: a dependencies route responding with no
	// entries, normalized the same way fetchBlockers would.
	readBlockedBy := normalizeBlockersUnder(nil, nil, nil)

	if preFiltered.BlockersUnresolved {
		t.Fatal("pre-filtered issue has BlockersUnresolved = true, want false")
	}
	if len(preFiltered.BlockedBy) != len(readBlockedBy) {
		t.Errorf("pre-filtered BlockedBy len = %d, per-issue-read BlockedBy len = %d, want equal", len(preFiltered.BlockedBy), len(readBlockedBy))
	}
}

var _ domain.Issue = domain.Issue{}
