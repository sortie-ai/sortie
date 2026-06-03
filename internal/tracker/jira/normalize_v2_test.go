package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractBody_V2 verifies the v2 branch returns the unquoted JSON
// string verbatim and yields "" on empty or invalid input.
func TestExtractBody_V2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "wiki markup verbatim", raw: json.RawMessage(`"h2. Title\n\n*bold* and {code}x{code}"`), want: "h2. Title\n\n*bold* and {code}x{code}"},
		{name: "plain string verbatim", raw: json.RawMessage(`"just text"`), want: "just text"},
		{name: "empty json string", raw: json.RawMessage(`""`), want: ""},
		{name: "nil raw", raw: nil, want: ""},
		{name: "empty raw", raw: json.RawMessage{}, want: ""},
		{name: "non-string json object is invalid", raw: json.RawMessage(`{"type":"doc"}`), want: ""},
		{name: "malformed json", raw: json.RawMessage(`"unterminated`), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := extractBody("2", tt.raw); got != tt.want {
				t.Errorf("extractBody(2, %s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNormalizeSearchIssue_V2WikiMarkupRoundTrip is the binding AC2
// assertion: a v2 description carrying wiki-markup tokens normalizes
// into domain.Issue.Description byte-for-byte, not stripped to clean
// prose.
func TestNormalizeSearchIssue_V2WikiMarkupRoundTrip(t *testing.T) {
	t.Parallel()

	const wiki = "h2. Goal\n\nMigrate the *auth* layer.\n\n{code}config.set(\"ldap\")\n{code}"
	rawBody, err := json.Marshal(wiki)
	if err != nil {
		t.Fatalf("marshal wiki body: %v", err)
	}

	ji := jiraIssue{
		ID:  "20001",
		Key: "SRV-1",
		Fields: jiraFields{
			Summary:     "Migrate to LDAP auth",
			Description: rawBody,
		},
	}

	issue := normalizeSearchIssue("2", "https://jira.internal.example.com", ji)

	if issue.Description != wiki {
		t.Errorf("Description = %q, want verbatim wiki markup %q", issue.Description, wiki)
	}
	// Markup tokens must survive: the v2 path does not run ADF flattening.
	for _, token := range []string{"h2.", "*auth*", "{code}"} {
		if !strings.Contains(issue.Description, token) {
			t.Errorf("Description = %q, want to retain wiki token %q", issue.Description, token)
		}
	}
}

// TestNormalizeComments_V2WikiMarkupRoundTrip mirrors the AC2 assertion
// for comment bodies.
func TestNormalizeComments_V2WikiMarkupRoundTrip(t *testing.T) {
	t.Parallel()

	const wiki = "h3. Review\n\nLooks good. See {code}main.go{code} and *ship it*."
	rawBody, err := json.Marshal(wiki)
	if err != nil {
		t.Fatalf("marshal wiki body: %v", err)
	}

	comments := []jiraComment{
		{
			ID:      "40001",
			Author:  &jiraUser{DisplayName: "Bob Server"},
			Body:    rawBody,
			Created: "2025-02-05T10:00:00.000+0000",
		},
	}

	result := normalizeComments("2", comments)
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	if result[0].Body != wiki {
		t.Errorf("Body = %q, want verbatim wiki markup %q", result[0].Body, wiki)
	}
	if result[0].Author != "Bob Server" {
		t.Errorf("Author = %q, want Bob Server", result[0].Author)
	}
}

// TestNormalizeSearchIssue_VersionIndependentFields asserts that
// non-body normalization (labels, priority, blockers, timestamps,
// parent, assignee, browse URL) is identical for v2 and v3 inputs.
func TestNormalizeSearchIssue_VersionIndependentFields(t *testing.T) {
	t.Parallel()

	// v3 description is an ADF doc; v2 description is a raw string. Every
	// other field is identical so the comparison isolates body handling.
	makeIssue := func() jiraIssue {
		return jiraIssue{
			ID:  "10001",
			Key: "PROJ-1",
			Fields: jiraFields{
				Summary:   "Test issue",
				Status:    &jiraStatus{Name: "To Do"},
				Priority:  &jiraPriority{ID: "2"},
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
				Created: "2025-01-15T10:30:00.000+0000",
				Updated: "2025-01-16T14:00:00.000+0000",
			},
		}
	}

	const endpoint = "https://test.example.com"

	v3Issue := makeIssue()
	v3Issue.Fields.Description = json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"ignored"}]}]}`)
	v3 := normalizeSearchIssue("3", endpoint, v3Issue)

	v2Issue := makeIssue()
	v2Issue.Fields.Description = json.RawMessage(`"ignored"`)
	v2 := normalizeSearchIssue("2", endpoint, v2Issue)

	if v2.State != v3.State {
		t.Errorf("State: v2 = %q, v3 = %q", v2.State, v3.State)
	}
	if (v2.Priority == nil) != (v3.Priority == nil) || (v2.Priority != nil && *v2.Priority != *v3.Priority) {
		t.Errorf("Priority: v2 = %v, v3 = %v", v2.Priority, v3.Priority)
	}
	if v2.IssueType != v3.IssueType {
		t.Errorf("IssueType: v2 = %q, v3 = %q", v2.IssueType, v3.IssueType)
	}
	if v2.Assignee != v3.Assignee {
		t.Errorf("Assignee: v2 = %q, v3 = %q", v2.Assignee, v3.Assignee)
	}
	if v2.URL != v3.URL {
		t.Errorf("URL: v2 = %q, v3 = %q", v2.URL, v3.URL)
	}
	if v2.CreatedAt != v3.CreatedAt || v2.UpdatedAt != v3.UpdatedAt {
		t.Errorf("timestamps: v2 = (%q,%q), v3 = (%q,%q)", v2.CreatedAt, v2.UpdatedAt, v3.CreatedAt, v3.UpdatedAt)
	}

	if len(v2.Labels) != len(v3.Labels) {
		t.Fatalf("Labels len: v2 = %d, v3 = %d", len(v2.Labels), len(v3.Labels))
	}
	for i := range v3.Labels {
		if v2.Labels[i] != v3.Labels[i] {
			t.Errorf("Labels[%d]: v2 = %q, v3 = %q", i, v2.Labels[i], v3.Labels[i])
		}
	}
	if len(v2.BlockedBy) != len(v3.BlockedBy) {
		t.Fatalf("BlockedBy len: v2 = %d, v3 = %d", len(v2.BlockedBy), len(v3.BlockedBy))
	}
	for i := range v3.BlockedBy {
		if v2.BlockedBy[i] != v3.BlockedBy[i] {
			t.Errorf("BlockedBy[%d]: v2 = %+v, v3 = %+v", i, v2.BlockedBy[i], v3.BlockedBy[i])
		}
	}
	if (v2.Parent == nil) != (v3.Parent == nil) {
		t.Fatalf("Parent presence: v2 = %v, v3 = %v", v2.Parent, v3.Parent)
	}
	if v2.Parent != nil && *v2.Parent != *v3.Parent {
		t.Errorf("Parent: v2 = %+v, v3 = %+v", *v2.Parent, *v3.Parent)
	}
}

// TestSearchResponseV2_Decode verifies the v2 search envelope decodes
// its offset-pagination fields and inner issues.
func TestSearchResponseV2_Decode(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"startAt": 50,
		"maxResults": 50,
		"total": 137,
		"issues": [
			{"id": "1", "key": "SRV-1", "fields": {"summary": "one"}},
			{"id": "2", "key": "SRV-2", "fields": {"summary": "two"}}
		]
	}`)

	var sr searchResponseV2
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("unmarshal searchResponseV2: %v", err)
	}
	if sr.StartAt != 50 {
		t.Errorf("StartAt = %d, want 50", sr.StartAt)
	}
	if sr.MaxResults != 50 {
		t.Errorf("MaxResults = %d, want 50", sr.MaxResults)
	}
	if sr.Total != 137 {
		t.Errorf("Total = %d, want 137", sr.Total)
	}
	if len(sr.Issues) != 2 {
		t.Fatalf("Issues len = %d, want 2", len(sr.Issues))
	}
	if sr.Issues[0].Key != "SRV-1" || sr.Issues[1].Key != "SRV-2" {
		t.Errorf("Issues keys = [%q %q], want [SRV-1 SRV-2]", sr.Issues[0].Key, sr.Issues[1].Key)
	}
}
