package jira

import (
	"encoding/json"
	"testing"
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

	issue := normalizeSearchIssue("https://test.atlassian.net", ji)

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

	issue := normalizeSearchIssue("https://test.atlassian.net", ji)

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
	issue := normalizeSearchIssue("https://x.atlassian.net", ji)
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
				// Inward "Blocks" — should produce BlockerRef
				{
					Type:        jiraLinkType{Name: "Blocks"},
					InwardIssue: &jiraLinkedIssue{ID: "2", Key: "X-2", Fields: &jiraLinkedIssueFields{Status: &jiraStatus{Name: "Open"}}},
				},
				// Outward "Blocks" — should be ignored
				{
					Type:         jiraLinkType{Name: "Blocks"},
					OutwardIssue: &jiraLinkedIssue{ID: "3", Key: "X-3"},
				},
				// "Relates" link — should be ignored
				{
					Type:        jiraLinkType{Name: "Relates"},
					InwardIssue: &jiraLinkedIssue{ID: "4", Key: "X-4"},
				},
				// "Blocks" with nil inward issue — should be ignored
				{
					Type: jiraLinkType{Name: "Blocks"},
				},
				// Blocker with nil status fields → state ""
				{
					Type:        jiraLinkType{Name: "Blocks"},
					InwardIssue: &jiraLinkedIssue{ID: "5", Key: "X-5"},
				},
			},
		},
	}

	issue := normalizeSearchIssue("https://x.atlassian.net", ji)

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
		result := normalizeComments(comments)
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

	t.Run("nil author", func(t *testing.T) {
		t.Parallel()

		comments := []jiraComment{
			{
				ID:      "200",
				Author:  nil,
				Body:    json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"auto"}]}]}`),
				Created: "2025-01-01T00:00:00.000+0000",
			},
		}
		result := normalizeComments(comments)
		if result[0].Author != "" {
			t.Errorf("Author = %q, want empty for nil author", result[0].Author)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()

		result := normalizeComments([]jiraComment{})
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

	// Empty labels array from Jira (not nil) → non-nil empty slice
	ji := jiraIssue{
		ID:  "1",
		Key: "X-1",
		Fields: jiraFields{
			Labels: []string{},
		},
	}
	issue := normalizeSearchIssue("https://x.atlassian.net", ji)
	if issue.Labels == nil {
		t.Error("Labels is nil, want non-nil empty slice")
	}
	if len(issue.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(issue.Labels))
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
