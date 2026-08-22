package gitlab

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestNormalizeIssue_AllFields(t *testing.T) {
	t.Parallel()

	raw := loadFixture(t, "issue_full.json")
	var gi gitlabIssue
	if err := json.Unmarshal(raw, &gi); err != nil {
		t.Fatalf("json.Unmarshal(issue_full.json): %v", err)
	}

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	got := normalizeIssue(gi, "acme/widgets", active, terminal, "", nil)

	// issue_full.json carries iid=42 and a distinct global id=91042; the
	// global id must never surface in ID, Identifier, or DisplayID.
	if got.ID != "42" {
		t.Errorf("ID = %q, want %q (from iid, never the global id)", got.ID, "42")
	}
	if got.Identifier != "42" {
		t.Errorf("Identifier = %q, want %q", got.Identifier, "42")
	}
	if got.DisplayID != "acme/widgets#42" {
		t.Errorf("DisplayID = %q, want the server-provided references.full value %q", got.DisplayID, "acme/widgets#42")
	}
	if got.Title != "Add dark mode support" {
		t.Errorf("Title = %q, want %q", got.Title, "Add dark mode support")
	}
	if got.Description != "Users want a **dark mode** option." {
		t.Errorf("Description = %q, want %q", got.Description, "Users want a **dark mode** option.")
	}
	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil (gitlab issues carry no priority field)", got.Priority)
	}
	if got.State != "in-progress" {
		t.Errorf("State = %q, want %q", got.State, "in-progress")
	}
	if got.BranchName != "" {
		t.Errorf("BranchName = %q, want empty (gitlab issues carry no branch field)", got.BranchName)
	}
	if got.URL != "https://gitlab.example.com/acme/widgets/-/issues/42" {
		t.Errorf("URL = %q, want the web_url value", got.URL)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "in-progress" || got.Labels[1] != "ui" {
		t.Errorf("Labels = %v, want [in-progress ui] (lowercased)", got.Labels)
	}
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want %q (from assignees[0], never the deprecated singular assignee field %q)", got.Assignee, "alice", "carol")
	}
	if got.IssueType != "issue" {
		t.Errorf("IssueType = %q, want %q", got.IssueType, "issue")
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
	if got.CreatedAt != "2026-01-01T00:00:00.000+02:00" {
		t.Errorf("CreatedAt = %q, want %q", got.CreatedAt, "2026-01-01T00:00:00.000+02:00")
	}
	if got.UpdatedAt != "2026-01-02T00:00:00.000+02:00" {
		t.Errorf("UpdatedAt = %q, want %q", got.UpdatedAt, "2026-01-02T00:00:00.000+02:00")
	}
}

func TestNormalizeIssue_DisplayIDFallback(t *testing.T) {
	t.Parallel()

	gi := gitlabIssue{IID: 7, State: "opened"}
	got := normalizeIssue(gi, "acme/subgroup/widgets", nil, nil, "", nil)

	if want := "acme/subgroup/widgets#7"; got.DisplayID != want {
		t.Errorf("DisplayID = %q, want %q when references.full is empty", got.DisplayID, want)
	}
}

func TestNormalizeIssue_NonNilEmptyLabels(t *testing.T) {
	t.Parallel()

	gi := gitlabIssue{IID: 1, State: "opened", Labels: nil}
	got := normalizeIssue(gi, "acme/widgets", nil, nil, "", nil)

	if got.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(got.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(got.Labels))
	}
}

func TestNormalizeIssue_EmptyAssignees(t *testing.T) {
	t.Parallel()

	gi := gitlabIssue{IID: 1, State: "opened", Assignees: nil}
	got := normalizeIssue(gi, "acme/widgets", nil, nil, "", nil)

	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty string for no assignees", got.Assignee)
	}
}

func TestNormalizeIssue_MultipleAssignees(t *testing.T) {
	t.Parallel()

	gi := gitlabIssue{
		IID:       1,
		State:     "opened",
		Assignees: []gitlabUser{{Username: "alice"}, {Username: "bob"}},
	}
	got := normalizeIssue(gi, "acme/widgets", nil, nil, "", nil)

	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want the first assignee %q", got.Assignee, "alice")
	}
}

// TestNormalizeIssue_BlockedByAlwaysNonNilEmpty pins that the always-
// empty blocker list is a declared capability limit, not an unread
// value: GitLab Community Edition's issue-links route carries no
// "blocks" relation to invert, so [registry.BlockersUnsupported] reads
// the empty list as complete rather than as something a resolver
// still owes a read.
func TestNormalizeIssue_BlockedByAlwaysNonNilEmpty(t *testing.T) {
	t.Parallel()

	gi := gitlabIssue{IID: 1, State: "opened"}
	got := normalizeIssue(gi, "acme/widgets", nil, nil, "", nil)

	if got.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice")
	}
	if len(got.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0 (gitlab community edition has no blocks relation type)", len(got.BlockedBy))
	}

	adaptertest.AssertCandidateBlockerSource(t, registry.BlockersUnsupported, got, 0)
	adaptertest.AssertBlockerIdentifiersMatchIssue(t, got, got.BlockedBy)
}

func TestNormalizeIssue_NullDescriptionDecodesToEmptyString(t *testing.T) {
	t.Parallel()

	var gi gitlabIssue
	if err := json.Unmarshal([]byte(`{"iid":1,"state":"opened","description":null}`), &gi); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if gi.Description != "" {
		t.Errorf("Description = %q, want empty string for a JSON null description", gi.Description)
	}
}

func TestNormalizeComments_SystemDroppedInternalRetained(t *testing.T) {
	t.Parallel()

	raw := []gitlabNote{
		{ID: 501, Author: gitlabUser{Username: "alice"}, Body: "Started investigating.", CreatedAt: "2026-01-10T09:00:00Z", System: false},
		{ID: 502, Author: gitlabUser{Username: "sortie-bot"}, Body: "changed the description", CreatedAt: "2026-01-10T09:05:00Z", System: true},
		{ID: 503, Author: gitlabUser{Username: "bob"}, Body: "Confidential rollback details.", CreatedAt: "2026-01-10T10:00:00Z", System: false},
	}

	got := normalizeComments(raw)

	if len(got) != 2 {
		t.Fatalf("normalizeComments len = %d, want 2 (the system note dropped)", len(got))
	}
	if got[0].ID != "501" || got[0].Author != "alice" {
		t.Errorf("got[0] = %+v, want ID=501 Author=alice", got[0])
	}
	if got[1].ID != "503" || got[1].Author != "bob" {
		t.Errorf("got[1] = %+v, want ID=503 Author=bob (system note at index 1 removed, order preserved)", got[1])
	}
}

func TestNormalizeComments_InternalFlagDoesNotFilter(t *testing.T) {
	t.Parallel()

	var raw []gitlabNote
	if err := json.Unmarshal([]byte(`[{"id":900,"body":"internal note","author":{"username":"bob"},"created_at":"2026-01-01T00:00:00Z","system":false,"internal":true}]`), &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	got := normalizeComments(raw)
	if len(got) != 1 {
		t.Fatalf("normalizeComments len = %d, want 1 (internal:true must not be dropped)", len(got))
	}
	if got[0].Body != "internal note" {
		t.Errorf("got[0].Body = %q, want %q", got[0].Body, "internal note")
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
