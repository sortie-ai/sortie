package github

import (
	"context"
	"os"
	"strings"
	"testing"
)

// skipUnlessGitHubIntegration skips the test unless SORTIE_GITHUB_TEST=1.
// Tests also require SORTIE_GITHUB_TOKEN and SORTIE_GITHUB_PROJECT to be set.
func skipUnlessGitHubIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_GITHUB_TEST") != "1" {
		t.Skip("skipping GitHub integration test: set SORTIE_GITHUB_TEST=1 to enable")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping GitHub integration test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping GitHub integration test: SORTIE_GITHUB_PROJECT not set")
	}
}

func integrationAdapter(t *testing.T) *GitHubAdapter {
	t.Helper()
	cfg := map[string]any{
		"api_key": os.Getenv("SORTIE_GITHUB_TOKEN"),
		"project": os.Getenv("SORTIE_GITHUB_PROJECT"),
	}
	a, err := NewGitHubAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitHubAdapter: %v", err)
	}
	return a.(*GitHubAdapter)
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	a := integrationAdapter(t)
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	t.Logf("fetched %d candidate issues", len(issues))

	for _, iss := range issues {
		if iss.ID == "" {
			t.Errorf("issue has empty ID: %+v", iss)
		}
		if iss.Identifier == "" {
			t.Errorf("issue has empty Identifier: %+v", iss)
		}
		if iss.ID != iss.Identifier {
			t.Errorf("issue %q: ID != Identifier (%q != %q)", iss.ID, iss.ID, iss.Identifier)
		}
		if iss.Title == "" {
			t.Errorf("issue %s has empty Title", iss.ID)
		}
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil in candidate list", iss.ID)
		}
		if iss.BlockedBy == nil {
			t.Errorf("issue %s: BlockedBy should be non-nil", iss.ID)
		}
		if iss.Priority != nil {
			t.Errorf("issue %s: Priority should always be nil (GitHub has no native priority)", iss.ID)
		}
		// All labels should be lowercased.
		for _, l := range iss.Labels {
			if l != strings.ToLower(l) {
				t.Errorf("issue %s: label %q is not lowercase", iss.ID, l)
			}
		}
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set; set to a valid issue number")
	}

	a := integrationAdapter(t)
	issue, err := a.FetchIssueByID(context.Background(), issueID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%q): %v", issueID, err)
	}

	t.Logf("fetched issue %s: %q state=%q", issue.ID, issue.Title, issue.State)

	if issue.ID != issueID {
		t.Errorf("ID = %q, want %q", issue.ID, issueID)
	}
	if issue.ID != issue.Identifier {
		t.Errorf("ID != Identifier: %q != %q", issue.ID, issue.Identifier)
	}
	if issue.Title == "" {
		t.Error("Title is empty")
	}
	if issue.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil (may be empty)")
	}
	if issue.Priority != nil {
		t.Error("Priority should always be nil for GitHub issues")
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessGitHubIntegration(t)

	issueID := os.Getenv("SORTIE_GITHUB_ISSUE_ID")
	if issueID == "" {
		t.Skip("skipping: SORTIE_GITHUB_ISSUE_ID not set")
	}

	a := integrationAdapter(t)
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	state, ok := result[issueID]
	if !ok {
		t.Fatalf("issue %q not in result map", issueID)
	}
	if state == "" {
		t.Errorf("issue %q has empty state", issueID)
	}
	t.Logf("issue %s state = %q", issueID, state)
}
