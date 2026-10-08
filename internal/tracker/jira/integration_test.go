package jira

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when the SORTIE_JIRA_TEST
// environment variable is not set to "1", so disabled integration tests are
// reported as skipped rather than silently passing.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_JIRA_TEST") != "1" {
		t.Skip("skipping Jira integration test: set SORTIE_JIRA_TEST=1 to enable")
	}
}

// requireEnv reads an environment variable and fails the test when empty.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// integrationConfig builds the adapter config map from environment variables.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	endpoint := requireEnv(t, "SORTIE_JIRA_ENDPOINT")
	apiKey := requireEnv(t, "SORTIE_JIRA_API_KEY")
	project := requireEnv(t, "SORTIE_JIRA_PROJECT")

	cfg := map[string]any{
		"endpoint": endpoint,
		"api_key":  apiKey,
		"project":  project,
	}

	if states := os.Getenv("SORTIE_JIRA_ACTIVE_STATES"); states != "" {
		parts := strings.Split(states, ",")
		trimmed := make([]any, 0, len(parts))
		for _, s := range parts {
			v := strings.TrimSpace(s)
			if v == "" {
				continue
			}
			trimmed = append(trimmed, v)
		}
		if len(trimmed) > 0 {
			cfg["active_states"] = trimmed
		}
	}
	if qf := os.Getenv("SORTIE_JIRA_QUERY_FILTER"); qf != "" {
		cfg["query_filter"] = qf
	}
	return cfg
}

// integrationEndpoint returns the configured Jira endpoint for URL validation.
func integrationEndpoint(t *testing.T) string {
	t.Helper()
	return strings.TrimRight(requireEnv(t, "SORTIE_JIRA_ENDPOINT"), "/")
}

// integrationActiveStates returns the active states from the environment
// variable or falls back to the adapter's defaults.
func integrationActiveStates(t *testing.T) []string {
	t.Helper()
	if states := os.Getenv("SORTIE_JIRA_ACTIVE_STATES"); states != "" {
		parts := strings.Split(states, ",")
		result := make([]string, 0, len(parts))
		for _, s := range parts {
			v := strings.TrimSpace(s)
			if v == "" {
				continue
			}
			result = append(result, v)
		}
		if len(result) > 0 {
			return result
		}
	}
	return defaultActiveStates
}

// jiraTimestampFormats lists the formats tried when validating Jira timestamps.
// RFC3339 is tried first, followed by the Jira-specific millisecond variant.
var jiraTimestampFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05.000+0000",
}

// assertParsesTimestamp asserts that ts is non-empty and parseable as a
// timestamp in one of the known Jira formats.
func assertParsesTimestamp(t *testing.T, field, ts string) {
	t.Helper()
	if ts == "" {
		t.Errorf("%s is empty", field)
		return
	}
	for _, layout := range jiraTimestampFormats {
		if _, err := time.Parse(layout, ts); err == nil {
			return
		}
	}
	t.Errorf("%s = %q is not parseable as a known timestamp format", field, ts)
}

// assertValidIssue validates that a normalized [domain.Issue] produced by the
// Jira adapter satisfies the expected invariants for IDs, labels, timestamps,
// relationships, and URLs.
func assertValidIssue(t *testing.T, iss domain.Issue, endpoint string) {
	t.Helper()

	if iss.ID == "" {
		t.Error("ID is empty")
	}
	if iss.Identifier == "" {
		t.Error("Identifier is empty")
	}
	if iss.Title == "" {
		t.Error("Title is empty")
	}
	if iss.State == "" {
		t.Error("State is empty")
	}
	if iss.Labels == nil {
		t.Error("Labels is nil, want non-nil slice")
	}
	for i, l := range iss.Labels {
		if l != strings.ToLower(l) {
			t.Errorf("Labels[%d] = %q is not lowercase", i, l)
		}
	}
	if iss.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil slice")
	}
	for i, b := range iss.BlockedBy {
		if b.Identifier == "" {
			t.Errorf("BlockedBy[%d].Identifier is empty", i)
		}
	}
	if iss.URL == "" {
		t.Error("URL is empty")
	} else if !strings.HasPrefix(iss.URL, endpoint) {
		t.Errorf("URL = %q does not start with endpoint %q", iss.URL, endpoint)
	}

	assertParsesTimestamp(t, "CreatedAt", iss.CreatedAt)
	assertParsesTimestamp(t, "UpdatedAt", iss.UpdatedAt)

	if iss.Parent != nil {
		if iss.Parent.ID == "" {
			t.Error("Parent.ID is empty")
		}
		if iss.Parent.Identifier == "" {
			t.Error("Parent.Identifier is empty")
		}
	}

	if iss.Comments != nil {
		for i, c := range iss.Comments {
			if c.ID == "" {
				t.Errorf("Comments[%d].ID is empty", i)
			}
		}
	}
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	t.Logf("FetchCandidateIssues returned %d issues", len(issues))

	endpoint := integrationEndpoint(t)
	for _, iss := range issues {
		assertValidIssue(t, iss, endpoint)
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil for candidate fetch", iss.Identifier)
		}
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot test FetchIssueByID")
	}

	identifier := candidates[0].Identifier

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	iss, err := adapter.FetchIssueByID(ctx2, identifier)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", identifier, err)
	}

	endpoint := integrationEndpoint(t)
	assertValidIssue(t, iss, endpoint)

	if iss.Identifier != identifier {
		t.Errorf("Identifier = %q, want %q", iss.Identifier, identifier)
	}
	if iss.Comments == nil {
		t.Error("Comments is nil, want non-nil slice for fully populated issue")
	}
	for i, c := range iss.Comments {
		if c.ID == "" {
			t.Errorf("Comments[%d].ID is empty", i)
		}
		if c.CreatedAt == "" {
			t.Errorf("Comments[%d].CreatedAt is empty", i)
		}
	}
}

func TestIntegration_FetchIssueByID_NotFound(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = adapter.FetchIssueByID(ctx, "NONEXISTENT-99999")
	if err == nil {
		t.Fatal("expected error for nonexistent issue, got nil")
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerNotFound)
	}
}

func TestIntegration_FetchIssuesByStates(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	activeStates := integrationActiveStates(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchIssuesByStates(ctx, activeStates)
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	t.Logf("FetchIssuesByStates returned %d issues", len(issues))

	stateSet := make(map[string]bool, len(activeStates))
	for _, s := range activeStates {
		stateSet[strings.ToLower(s)] = true
	}

	endpoint := integrationEndpoint(t)
	for _, iss := range issues {
		assertValidIssue(t, iss, endpoint)
		if !stateSet[strings.ToLower(iss.State)] {
			t.Errorf("issue %s: State %q not in requested states %v", iss.Identifier, iss.State, activeStates)
		}
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil for search", iss.Identifier)
		}
	}
}

func TestIntegration_FetchIssuesByStates_Empty(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchIssuesByStates(ctx, []string{})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(empty): %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("expected 0 issues for empty states, got %d", len(issues))
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project")
	}

	limit := min(len(candidates), 5)
	ids := make([]string, limit)
	candidateStates := make(map[string]string, limit)
	for i := range limit {
		ids[i] = candidates[i].ID
		candidateStates[candidates[i].ID] = candidates[i].State
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	stateMap, err := adapter.FetchIssueStatesByIDs(ctx2, ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	for _, id := range ids {
		state, ok := stateMap[id]
		if !ok {
			t.Errorf("ID %s missing from result map", id)
			continue
		}
		if state == "" {
			t.Errorf("ID %s: state is empty", id)
		}
		if prev, exists := candidateStates[id]; exists && state != prev {
			t.Logf("ID %s: state changed between calls (%q -> %q)", id, prev, state)
		}
	}
}

func TestIntegration_FetchIssueComments(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	comments, err := adapter.FetchIssueComments(ctx2, candidates[0].Identifier)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", candidates[0].Identifier, err)
	}

	if comments == nil {
		t.Fatal("comments is nil, want non-nil slice")
	}

	t.Logf("FetchIssueComments returned %d comments", len(comments))

	for i, c := range comments {
		if c.ID == "" {
			t.Errorf("comments[%d].ID is empty", i)
		}
		if c.CreatedAt == "" {
			t.Errorf("comments[%d].CreatedAt is empty", i)
		}
	}
}

const integrationStatement = "/close\n@sortie-literal-probe please look\n[~jdoe] see https://example.com/probe?x=1\n" +
	"*bold* _it_ h1. Heading\n```` fenced ````\n{NoFormat} then {noformat}\ntoken=[redacted]"

func TestIntegration_CommentIssueWithLiteral_StoresOneCodeBlock(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}
	jiraAdapter, ok := adapter.(*JiraAdapter)
	if !ok {
		t.Fatal("NewJiraAdapter did not return a *JiraAdapter")
	}
	if jiraAdapter.apiVersion != "3" {
		t.Skipf("skipping: structured comment view needs API version 3, adapter uses %q", jiraAdapter.apiVersion)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot run write test")
	}
	key := candidates[0].Identifier

	marker := "sortie literal round trip " + time.Now().UTC().Format(time.RFC3339Nano)
	if err := adapter.CommentIssueWithLiteral(ctx, key, marker, integrationStatement); err != nil {
		t.Fatalf("CommentIssueWithLiteral(%s): %v", key, err)
	}

	raw, _, err := jiraAdapter.client.Get(ctx, jiraAdapter.basePath+"/issue/"+key+"/comment", url.Values{"orderBy": {"-created"}, "maxResults": {"50"}})
	if err != nil {
		t.Fatalf("GET comments of %s: %v", key, err)
	}
	var listing struct {
		Comments []struct {
			Body json.RawMessage `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatalf("decode comments of %s: %v", key, err)
	}
	for _, comment := range listing.Comments {
		if !strings.Contains(string(comment.Body), marker) {
			continue
		}
		var document struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if err := json.Unmarshal(comment.Body, &document); err != nil {
			t.Fatalf("decode stored comment body: %v", err)
		}
		codeBlocks := 0
		for _, node := range document.Content {
			if node.Type == "codeBlock" {
				codeBlocks++
			}
		}
		if codeBlocks != 1 {
			t.Errorf("stored comment holds %d codeBlock nodes, want 1:\n%s", codeBlocks, comment.Body)
		}
		return
	}
	t.Fatalf("no stored comment contains %q among %d comments", marker, len(listing.Comments))
}

func TestIntegration_LabelRoundTrip(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewJiraAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in project; cannot run write test")
	}
	key := candidates[0].Identifier
	probe := "sortie-label-probe"

	if err := adapter.AddLabel(ctx, key, probe); err != nil {
		t.Fatalf("AddLabel(%s, %q): %v", key, probe, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := adapter.RemoveLabel(cleanupCtx, key, probe); err != nil {
			t.Errorf("cleanup RemoveLabel(%s, %q): %v", key, probe, err)
		}
	})

	issue, err := adapter.FetchIssueByID(ctx, key)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", key, err)
	}
	if !slices.Contains(issue.Labels, probe) {
		t.Fatalf("FetchIssueByID(%s) labels = %v, want %q after AddLabel", key, issue.Labels, probe)
	}

	if err := adapter.RemoveLabel(ctx, key, probe); err != nil {
		t.Fatalf("RemoveLabel(%s, %q): %v", key, probe, err)
	}
	issue, err = adapter.FetchIssueByID(ctx, key)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s) after removal: %v", key, err)
	}
	if slices.Contains(issue.Labels, probe) {
		t.Errorf("FetchIssueByID(%s) labels = %v, want %q gone immediately after RemoveLabel", key, issue.Labels, probe)
	}
}
