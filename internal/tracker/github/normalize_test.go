package github

import (
	"strconv"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func strPtr(s string) *string              { return &s }
func typePtr(name string) *githubIssueType { return &githubIssueType{Name: name} }
func prMarker() *githubPR                  { return &githubPR{} }

// --- normalizeIssue ---

func TestNormalizeIssue_AllFields(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		ID:        987654,
		Number:    42,
		Title:     "Add dark mode",
		Body:      strPtr("Users want dark mode."),
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

	got := normalizeIssue(gi, active, terminal)

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
	got := normalizeIssue(gi, []string{"backlog"}, nil)

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
	got := normalizeIssue(gi, nil, nil)

	if got.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(got.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(got.Labels))
	}
}

func TestNormalizeIssue_NilBody(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open", Body: nil}
	got := normalizeIssue(gi, nil, nil)

	if got.Description != "" {
		t.Errorf("Description = %q, want empty string for nil Body", got.Description)
	}
}

func TestNormalizeIssue_NilType(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open", Type: nil}
	got := normalizeIssue(gi, nil, nil)

	if got.IssueType != "" {
		t.Errorf("IssueType = %q, want empty string for nil Type", got.IssueType)
	}
}

func TestNormalizeIssue_EmptyAssignees(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open", Assignees: nil}
	got := normalizeIssue(gi, nil, nil)

	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty string for no assignees", got.Assignee)
	}
}

func TestNormalizeIssue_MultipleAssignees(t *testing.T) {
	t.Parallel()

	gi := githubIssue{
		Number:    1,
		State:     "open",
		Assignees: []githubUser{{Login: "alice"}, {Login: "bob"}},
	}
	got := normalizeIssue(gi, nil, nil)

	// Only the first assignee is used.
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want first assignee %q", got.Assignee, "alice")
	}
}

func TestNormalizeIssue_NonNilEmptyBlockedBy(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open"}
	got := normalizeIssue(gi, nil, nil)

	if got.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice in list normalization")
	}
	if len(got.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0", len(got.BlockedBy))
	}
}

func TestNormalizeIssue_NilPriority(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 1, State: "open"}
	got := normalizeIssue(gi, nil, nil)

	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil (GitHub has no native priority)", got.Priority)
	}
}

func TestNormalizeIssue_IDEqualsIdentifier(t *testing.T) {
	t.Parallel()

	gi := githubIssue{ID: 99999999, Number: 123, State: "open"}
	got := normalizeIssue(gi, nil, nil)

	// Both ID and Identifier must be the issue number, not the global int64 ID.
	if got.ID != "123" {
		t.Errorf("ID = %q, want issue number %q (not global ID)", got.ID, "123")
	}
	if got.Identifier != "123" {
		t.Errorf("Identifier = %q, want issue number %q", got.Identifier, "123")
	}
	if got.ID != got.Identifier {
		t.Errorf("ID %q != Identifier %q", got.ID, got.Identifier)
	}
}

func TestNormalizeIssue_DisplayIDEmpty(t *testing.T) {
	t.Parallel()

	gi := githubIssue{Number: 42, State: "open"}
	got := normalizeIssue(gi, nil, nil)

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

// --- normalizeBlockers ---

func TestNormalizeBlockers_NonEmpty(t *testing.T) {
	t.Parallel()

	blockers := []githubIssue{
		{ID: 500, Number: 5, State: "open", Labels: []githubLabel{{Name: "in-progress"}}},
		{ID: 600, Number: 6, State: "closed", Labels: []githubLabel{{Name: "done"}}},
	}
	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	got := normalizeBlockers(blockers, active, terminal)

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
}

func TestNormalizeBlockers_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	got := normalizeBlockers(nil, nil, nil)

	if got == nil {
		t.Error("normalizeBlockers(nil) returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// --- normalizeComments ---

func TestNormalizeComments_NonEmpty(t *testing.T) {
	t.Parallel()

	comments := []githubComment{
		{ID: 9001, User: githubUser{Login: "alice"}, Body: "Looks good!", CreatedAt: "2026-01-15T10:00:00Z"},
		{ID: 9002, User: githubUser{Login: "bob"}, Body: "Fix indentation.", CreatedAt: "2026-01-16T11:30:00Z"},
	}

	got := normalizeComments(comments)

	if len(got) != 2 {
		t.Fatalf("normalizeComments len = %d, want 2", len(got))
	}
	// ID is strconv.FormatInt of the int64 id.
	if got[0].ID != "9001" {
		t.Errorf("got[0].ID = %q, want %q", got[0].ID, "9001")
	}
	if got[0].Author != "alice" {
		t.Errorf("got[0].Author = %q, want %q", got[0].Author, "alice")
	}
	if got[0].Body != "Looks good!" {
		t.Errorf("got[0].Body = %q", got[0].Body)
	}
	if got[0].CreatedAt != "2026-01-15T10:00:00Z" {
		t.Errorf("got[0].CreatedAt = %q", got[0].CreatedAt)
	}
	if got[1].ID != "9002" {
		t.Errorf("got[1].ID = %q, want %q", got[1].ID, "9002")
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

// --- isPullRequest ---

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

// --- domain type sanity check (compile-time) ---

var _ domain.Issue = domain.Issue{}
