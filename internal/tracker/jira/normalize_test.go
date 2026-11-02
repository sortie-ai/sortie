package jira

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestNormalizeSearchIssue_AllFields(t *testing.T) {
	t.Parallel()

	pri := "2"
	ji := jiraIssue{
		ID:  "10001",
		Key: "PROJ-1",
		Fields: jiraFields{
			Summary:   "Test issue",
			Status:    &jiraStatus{Name: "To Do"},
			Priority:  &jiraPriority{ID: pri},
			Labels:    []string{"Feature", "Auth"},
			Assignee:  &jiraUser{DisplayName: "Alice"},
			IssueType: &jiraIssueType{Name: "Story"},
			Parent:    &jiraParent{ID: "10000", Key: "PROJ-0"},
			IssueLinks: []jiraIssueLink{
				{
					Type:        jiraLinkType{Name: "Blocks", Inward: "is blocked by"},
					InwardIssue: &jiraLinkedIssue{ID: "10010", Key: "PROJ-10", Fields: &jiraLinkedIssueFields{Status: &jiraStatus{Name: "In Progress"}}},
				},
			},
			Description: json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello"}]}]}`),
			Created:     "2025-01-15T10:30:00.000+0000",
			Updated:     "2025-01-16T14:00:00.000+0000",
		},
	}

	issue := normalizeSearchIssue("3", "https://test.atlassian.net", ji)

	if issue.ID != "10001" {
		t.Errorf("ID = %q, want %q", issue.ID, "10001")
	}
	if issue.Identifier != "PROJ-1" {
		t.Errorf("Identifier = %q, want %q", issue.Identifier, "PROJ-1")
	}
	if issue.Title != "Test issue" {
		t.Errorf("Title = %q, want %q", issue.Title, "Test issue")
	}
	if issue.Description != "Hello" {
		t.Errorf("Description = %q, want %q", issue.Description, "Hello")
	}
	if issue.State != "To Do" {
		t.Errorf("State = %q, want %q", issue.State, "To Do")
	}
	if issue.Priority == nil || *issue.Priority != 2 {
		t.Errorf("Priority = %v, want 2", issue.Priority)
	}
	if issue.URL != "https://test.atlassian.net/browse/PROJ-1" {
		t.Errorf("URL = %q, want %q", issue.URL, "https://test.atlassian.net/browse/PROJ-1")
	}
	if issue.Assignee != "Alice" {
		t.Errorf("Assignee = %q, want %q", issue.Assignee, "Alice")
	}
	if issue.IssueType != "Story" {
		t.Errorf("IssueType = %q, want %q", issue.IssueType, "Story")
	}
	if issue.Parent == nil || issue.Parent.ID != "10000" || issue.Parent.Identifier != "PROJ-0" {
		t.Errorf("Parent = %v, want {10000, PROJ-0}", issue.Parent)
	}
	if issue.CreatedAt != "2025-01-15T10:30:00.000+0000" {
		t.Errorf("CreatedAt = %q", issue.CreatedAt)
	}
	if issue.UpdatedAt != "2025-01-16T14:00:00.000+0000" {
		t.Errorf("UpdatedAt = %q", issue.UpdatedAt)
	}

	// Labels lowercased
	if len(issue.Labels) != 2 || issue.Labels[0] != "feature" || issue.Labels[1] != "auth" {
		t.Errorf("Labels = %v, want [feature auth]", issue.Labels)
	}

	// Blockers: only inward "Blocks" link
	if len(issue.BlockedBy) != 1 {
		t.Fatalf("BlockedBy len = %d, want 1", len(issue.BlockedBy))
	}
	if issue.BlockedBy[0].Identifier != "PROJ-10" {
		t.Errorf("BlockedBy[0].Identifier = %q, want PROJ-10", issue.BlockedBy[0].Identifier)
	}
	if issue.BlockedBy[0].State != "In Progress" {
		t.Errorf("BlockedBy[0].State = %q, want In Progress", issue.BlockedBy[0].State)
	}

	// searchFields carries issuelinks into every candidate search, so
	// Jira declares BlockersFromCandidates and this candidate already
	// carries its one real blocker with no further read owed.
	adaptertest.AssertCandidateBlockerSource(t, registry.BlockersFromCandidates, issue, 1)
	adaptertest.AssertBlockerRefsNormalized(t, issue.BlockedBy)
	adaptertest.AssertBlockerIdentifiersMatchIssue(t, issue, issue.BlockedBy)
}

func TestNormalizeSearchIssue_NilFields(t *testing.T) {
	t.Parallel()

	ji := jiraIssue{
		ID:  "10002",
		Key: "PROJ-2",
		Fields: jiraFields{
			Summary: "Minimal",
		},
	}

	issue := normalizeSearchIssue("3", "https://test.atlassian.net", ji)

	if issue.State != "" {
		t.Errorf("State = %q, want empty (nil status)", issue.State)
	}
	if issue.Priority != nil {
		t.Errorf("Priority = %v, want nil (nil priority)", issue.Priority)
	}
	if issue.Assignee != "" {
		t.Errorf("Assignee = %q, want empty (nil assignee)", issue.Assignee)
	}
	if issue.IssueType != "" {
		t.Errorf("IssueType = %q, want empty (nil issuetype)", issue.IssueType)
	}
	if issue.Parent != nil {
		t.Errorf("Parent = %v, want nil", issue.Parent)
	}
	if issue.Description != "" {
		t.Errorf("Description = %q, want empty (nil description)", issue.Description)
	}

	// Non-nil empty slice invariants
	if issue.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(issue.Labels) != 0 {
		t.Errorf("Labels = %v, want empty", issue.Labels)
	}
	if issue.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice")
	}
	if len(issue.BlockedBy) != 0 {
		t.Errorf("BlockedBy = %v, want empty", issue.BlockedBy)
	}
}

func TestNormalizeSearchIssue_NonIntegerPriority(t *testing.T) {
	t.Parallel()

	ji := jiraIssue{
		ID:  "1",
		Key: "X-1",
		Fields: jiraFields{
			Priority: &jiraPriority{ID: "high"},
		},
	}
	issue := normalizeSearchIssue("3", "https://x.atlassian.net", ji)
	if issue.Priority != nil {
		t.Errorf("Priority = %v, want nil for non-integer id", issue.Priority)
	}
}

func TestNormalizeSearchIssue_BlockerExtraction(t *testing.T) {
	t.Parallel()

	ji := jiraIssue{
		ID:  "1",
		Key: "X-1",
		Fields: jiraFields{
			IssueLinks: []jiraIssueLink{
				// Inward "Blocks"; should produce BlockerRef
				{
					Type:        jiraLinkType{Name: "Blocks"},
					InwardIssue: &jiraLinkedIssue{ID: "2", Key: "X-2", Fields: &jiraLinkedIssueFields{Status: &jiraStatus{Name: "Open"}}},
				},
				// Outward "Blocks"; should be ignored
				{
					Type:         jiraLinkType{Name: "Blocks"},
					OutwardIssue: &jiraLinkedIssue{ID: "3", Key: "X-3"},
				},
				// "Relates" link; should be ignored
				{
					Type:        jiraLinkType{Name: "Relates"},
					InwardIssue: &jiraLinkedIssue{ID: "4", Key: "X-4"},
				},
				// "Blocks" with nil inward issue; should be ignored
				{
					Type: jiraLinkType{Name: "Blocks"},
				},
				// Blocker with nil status fields -> state ""
				{
					Type:        jiraLinkType{Name: "Blocks"},
					InwardIssue: &jiraLinkedIssue{ID: "5", Key: "X-5"},
				},
			},
		},
	}

	issue := normalizeSearchIssue("3", "https://x.atlassian.net", ji)

	if len(issue.BlockedBy) != 2 {
		t.Fatalf("BlockedBy len = %d, want 2", len(issue.BlockedBy))
	}
	if issue.BlockedBy[0].Identifier != "X-2" || issue.BlockedBy[0].State != "Open" {
		t.Errorf("BlockedBy[0] = %+v, want X-2/Open", issue.BlockedBy[0])
	}
	if issue.BlockedBy[1].Identifier != "X-5" || issue.BlockedBy[1].State != "" {
		t.Errorf("BlockedBy[1] = %+v, want X-5/empty state", issue.BlockedBy[1])
	}
}

func TestNormalizeComments(t *testing.T) {
	t.Parallel()

	t.Run("normal comments", func(t *testing.T) {
		t.Parallel()

		comments := []jiraComment{
			{
				ID:      "100",
				Author:  &jiraUser{DisplayName: "Alice"},
				Body:    json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello"}]}]}`),
				Created: "2025-01-01T00:00:00.000+0000",
			},
		}
		result := normalizeComments("3", comments)
		if len(result) != 1 {
			t.Fatalf("len = %d, want 1", len(result))
		}
		if result[0].ID != "100" {
			t.Errorf("ID = %q, want 100", result[0].ID)
		}
		if result[0].Author != "Alice" {
			t.Errorf("Author = %q, want Alice", result[0].Author)
		}
		if result[0].Body != "Hello" {
			t.Errorf("Body = %q, want Hello", result[0].Body)
		}
		if result[0].CreatedAt != "2025-01-01T00:00:00.000+0000" {
			t.Errorf("CreatedAt = %q", result[0].CreatedAt)
		}
	})

	t.Run("author identity and kind", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			author   *jiraUser
			wantID   string
			wantKind domain.CommentAuthorKind
		}{
			{"no author", nil, "", domain.CommentAuthorUnknown},
			{"atlassian prefers account ID over name", &jiraUser{DisplayName: "Display Name", AccountID: "acc-1", Name: "alice", AccountType: "atlassian"}, "acc-1", domain.CommentAuthorPerson},
			{"mixed-case account type", &jiraUser{DisplayName: "Display Name", AccountID: "acc-1", AccountType: "Atlassian"}, "acc-1", domain.CommentAuthorPerson},
			{"app", &jiraUser{DisplayName: "Display Name", AccountID: "acc-2", AccountType: "app"}, "acc-2", domain.CommentAuthorBot},
			{"customer", &jiraUser{DisplayName: "Display Name", AccountID: "acc-3", AccountType: "customer"}, "acc-3", domain.CommentAuthorExternal},
			{"other account type", &jiraUser{DisplayName: "Display Name", AccountID: "acc-4", AccountType: "robot"}, "acc-4", domain.CommentAuthorUnknown},
			{"no account type with name", &jiraUser{DisplayName: "Display Name", Name: "alice"}, "alice", domain.CommentAuthorPerson},
			{"no account type and no name", &jiraUser{DisplayName: "Display Name"}, "", domain.CommentAuthorUnknown},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				wantAuthor := ""
				if tt.author != nil {
					wantAuthor = tt.author.DisplayName
				}

				got := normalizeComments("3", []jiraComment{{
					ID:      "100",
					Author:  tt.author,
					Body:    json.RawMessage(`{"type":"doc","content":[]}`),
					Created: "2025-01-01T00:00:00.000+0000",
					Updated: "2025-01-02T00:00:00.000+0000",
				}})[0]

				want := domain.Comment{
					ID: "100", Author: wantAuthor, AuthorID: tt.wantID, AuthorKind: tt.wantKind,
					CreatedAt: "2025-01-01T00:00:00.000+0000", UpdatedAt: "2025-01-02T00:00:00.000+0000",
				}
				if got != want {
					t.Errorf("normalizeComments(%+v) = %+v, want %+v", tt.author, got, want)
				}
			})
		}
	})

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()

		result := normalizeComments("3", []jiraComment{})
		if result == nil {
			t.Error("result is nil, want non-nil empty slice")
		}
		if len(result) != 0 {
			t.Errorf("len = %d, want 0", len(result))
		}
	})
}

func TestUnmarshalADF(t *testing.T) {
	t.Parallel()

	t.Run("nil input", func(t *testing.T) {
		t.Parallel()

		if v := unmarshalADF(nil); v != nil {
			t.Errorf("got %v, want nil", v)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		t.Parallel()

		if v := unmarshalADF(json.RawMessage{}); v != nil {
			t.Errorf("got %v, want nil", v)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		t.Parallel()

		if v := unmarshalADF(json.RawMessage(`{invalid`)); v != nil {
			t.Errorf("got %v, want nil", v)
		}
	})
	t.Run("valid json", func(t *testing.T) {
		t.Parallel()

		v := unmarshalADF(json.RawMessage(`{"type":"doc"}`))
		if v == nil {
			t.Error("got nil, want non-nil")
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("type = %T, want map[string]any", v)
		}
		if m["type"] != "doc" {
			t.Errorf("type = %v, want doc", m["type"])
		}
	})
}

func TestNormalizeSearchIssue_EmptyLabelsSlice(t *testing.T) {
	t.Parallel()

	// Empty labels array from Jira (not nil) -> non-nil empty slice
	ji := jiraIssue{
		ID:  "1",
		Key: "X-1",
		Fields: jiraFields{
			Labels: []string{},
		},
	}
	issue := normalizeSearchIssue("3", "https://x.atlassian.net", ji)
	if issue.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(issue.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(issue.Labels))
	}
}

func TestNormalizeSearchIssue_URLOmitsEndpointUserinfo(t *testing.T) {
	t.Parallel()

	issue := normalizeSearchIssue("3", "https://bot:s3cret@x.atlassian.net", jiraIssue{ID: "1", Key: "PROJ-1"})

	if want := "https://x.atlassian.net/browse/PROJ-1"; issue.URL != want {
		t.Errorf("normalizeSearchIssue(userinfo endpoint).URL = %q, want %q", issue.URL, want)
	}
}

func TestExtractBlockers_Empty(t *testing.T) {
	t.Parallel()

	result := extractBlockers(nil)
	if result == nil {
		t.Error("result is nil, want non-nil empty slice")
	}
	if len(result) != 0 {
		t.Errorf("len = %d, want 0", len(result))
	}
}
