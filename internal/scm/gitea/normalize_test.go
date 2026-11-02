package gitea

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

func stateModel(active, terminal []string) (*issuekit.LabelStateModel, issuekit.LabelStates) {
	m := issuekit.NewLabelStateModel(issuekit.NewStateSource(domain.TrackerStates{Active: active, Terminal: terminal}), nil)
	return m, m.Snapshot()
}

func normalizeIssueUnder(gi giteaIssue, active, terminal []string) domain.Issue {
	m, s := stateModel(active, terminal)
	return normalizeIssue(gi, m, s)
}

func normalizeBlockersUnder(blockers []giteaIssue, active, terminal []string) []domain.BlockerRef {
	m, s := stateModel(active, terminal)
	return normalizeBlockers(blockers, m, s, "acme", "widgets")
}

func TestNormalizeIssue_AllFields(t *testing.T) {
	t.Parallel()

	raw := loadFixture(t, "issue_full.json")
	var gi giteaIssue
	if err := json.Unmarshal(raw, &gi); err != nil {
		t.Fatalf("json.Unmarshal(issue_full.json): %v", err)
	}

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	got := normalizeIssueUnder(gi, active, terminal)

	// issue_full.json carries number=42 and a distinct global id=9042; the
	// global id must never surface in ID, Identifier, or DisplayID.
	if got.ID != "42" {
		t.Errorf("ID = %q, want %q (from number, never the global id)", got.ID, "42")
	}
	if got.Identifier != "42" {
		t.Errorf("Identifier = %q, want %q", got.Identifier, "42")
	}
	if got.Title != "Add dark mode support" {
		t.Errorf("Title = %q, want %q", got.Title, "Add dark mode support")
	}
	if got.Description != "Users want a **dark mode** option." {
		t.Errorf("Description = %q, want %q", got.Description, "Users want a **dark mode** option.")
	}
	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil (gitea has no priority field)", got.Priority)
	}
	if got.State != "in-progress" {
		t.Errorf("State = %q, want %q", got.State, "in-progress")
	}
	if got.BranchName != "feature/dark-mode" {
		t.Errorf("BranchName = %q, want %q", got.BranchName, "feature/dark-mode")
	}
	if got.URL != "https://git.example.com/acme/widgets/issues/42" {
		t.Errorf("URL = %q, want the html_url value", got.URL)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "in-progress" || got.Labels[1] != "ui" {
		t.Errorf("Labels = %v, want [in-progress ui]", got.Labels)
	}
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want %q (from assignees[0], never the deprecated singular field)", got.Assignee, "alice")
	}
	if got.IssueType != "" {
		t.Errorf("IssueType = %q, want empty (gitea has no issue-type concept)", got.IssueType)
	}
	if got.Parent != nil {
		t.Errorf("Parent = %v, want nil", got.Parent)
	}
	if got.Comments != nil {
		t.Errorf("Comments = %v, want nil (normalizeIssue never fetches comments)", got.Comments)
	}
	if got.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice")
	}
	if len(got.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0", len(got.BlockedBy))
	}
	if !got.BlockersUnresolved {
		t.Error("BlockersUnresolved = false, want true: giteaIssue carries no dependency field to prove the issue has none")
	}
	if got.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want %q", got.CreatedAt, "2026-01-01T00:00:00Z")
	}
	if got.UpdatedAt != "2026-01-02T00:00:00Z" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-01-02T00:00:00Z")
	}
	if got.DisplayID != "" {
		t.Errorf("DisplayID = %q, want empty (callers apply setDisplayID)", got.DisplayID)
	}
}

func TestGiteaIssue_NullBodyDecodesToEmptyString(t *testing.T) {
	t.Parallel()

	var gi giteaIssue
	if err := json.Unmarshal([]byte(`{"number":1,"state":"open","body":null}`), &gi); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if gi.Body != "" {
		t.Errorf("Body = %q, want empty string for a JSON null body", gi.Body)
	}
}

func TestNormalizeIssue_LabelsLowercased(t *testing.T) {
	t.Parallel()

	gi := giteaIssue{
		Number: 1,
		State:  "open",
		Labels: []giteaLabel{{Name: "BACKLOG"}, {Name: "Priority-High"}},
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

	gi := giteaIssue{Number: 1, State: "open", Labels: nil}
	got := normalizeIssueUnder(gi, nil, nil)

	if got.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(got.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(got.Labels))
	}
}

func TestNormalizeIssue_EmptyAssignees(t *testing.T) {
	t.Parallel()

	gi := giteaIssue{Number: 1, State: "open", Assignees: nil}
	got := normalizeIssueUnder(gi, nil, nil)

	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty string for no assignees", got.Assignee)
	}
}

func TestNormalizeIssue_MultipleAssignees(t *testing.T) {
	t.Parallel()

	gi := giteaIssue{
		Number:    1,
		State:     "open",
		Assignees: []giteaUser{{Login: "alice"}, {Login: "bob"}},
	}
	got := normalizeIssueUnder(gi, nil, nil)

	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want first assignee %q", got.Assignee, "alice")
	}
}

func TestSetDisplayID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		owner  string
		repo   string
		before string
		want   string
	}{
		{"standard owner/repo#N format", "acme", "widgets", "", "acme/widgets#9"},
		{"dotted repo name", "sortie-ai", "sortie.test", "", "sortie-ai/sortie.test#9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := domain.Issue{ID: "9", Identifier: "9", DisplayID: tt.before}
			setDisplayID(&issue, tt.owner, tt.repo)

			if issue.DisplayID != tt.want {
				t.Errorf("setDisplayID(owner=%q, repo=%q) DisplayID = %q, want %q", tt.owner, tt.repo, issue.DisplayID, tt.want)
			}
		})
	}

	t.Run("idempotent: does not overwrite an already-qualified DisplayID", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "9", Identifier: "9", DisplayID: "already/qualified#9"}
		setDisplayID(&issue, "acme", "widgets")

		if issue.DisplayID != "already/qualified#9" {
			t.Errorf("setDisplayID overwrote an existing DisplayID: got %q, want %q", issue.DisplayID, "already/qualified#9")
		}
	})
}

func TestNormalizeComments_NonEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		userType string
		want     domain.CommentAuthorKind
	}{
		{"bot", "Bot", domain.CommentAuthorBot},
		{"user", "User", domain.CommentAuthorPerson},
		{"absent type", "", domain.CommentAuthorUnknown},
		{"organization", "Organization", domain.CommentAuthorUnknown},
		{"mixed case bot", "bOT", domain.CommentAuthorBot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := giteaComment{
				ID:        9001,
				User:      giteaUser{Login: "alice", Type: tt.userType},
				Body:      "Looks good!",
				CreatedAt: "2026-01-15T10:00:00Z",
				UpdatedAt: "2026-01-16T09:00:00Z",
			}
			want := domain.Comment{
				ID:         "9001",
				Author:     "alice",
				AuthorID:   "alice",
				AuthorKind: tt.want,
				Body:       "Looks good!",
				CreatedAt:  "2026-01-15T10:00:00Z",
				UpdatedAt:  "2026-01-16T09:00:00Z",
			}

			got := normalizeComments([]giteaComment{raw})

			if len(got) != 1 || got[0] != want {
				t.Errorf("normalizeComments(type %q) = %+v, want [%+v]", tt.userType, got, want)
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

func TestNormalizeBlockers_NonEmpty(t *testing.T) {
	t.Parallel()

	blockers := []giteaIssue{
		{Number: 5, State: "open", Labels: []giteaLabel{{Name: "in-progress"}}},
		{Number: 6, State: "closed", Labels: []giteaLabel{{Name: "done"}}},
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
	if got[0].DisplayID != "acme/widgets#5" {
		t.Errorf("got[0].DisplayID = %q, want %q", got[0].DisplayID, "acme/widgets#5")
	}

	adaptertest.AssertBlockerRefsNormalized(t, got)
	qualifiedIssue := domain.Issue{Identifier: "1", DisplayID: "acme/widgets#1"}
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

func TestIsPullRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		gi   giteaIssue
		want bool
	}{
		{"pull_request field set is a PR", giteaIssue{Number: 1, PullRequest: &giteaPR{}}, true},
		{"pull_request field nil is an issue", giteaIssue{Number: 2, PullRequest: nil}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isPullRequest(tt.gi)
			if got != tt.want {
				t.Errorf("isPullRequest(%v) = %v, want %v", tt.gi, got, tt.want)
			}
		})
	}
}
