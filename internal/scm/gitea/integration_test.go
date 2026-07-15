package gitea

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when SORTIE_GITEA_TEST is not
// "1", so disabled integration tests report as skipped rather than passing
// silently or hitting the network.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_GITEA_TEST") != "1" {
		t.Skip("skipping Gitea integration test: set SORTIE_GITEA_TEST=1 to enable")
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

// integrationConfig builds the adapter config from the integration env vars.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"api_key":  requireEnv(t, "SORTIE_GITEA_TOKEN"),
		"project":  requireEnv(t, "SORTIE_GITEA_PROJECT"),
		"endpoint": requireEnv(t, "SORTIE_GITEA_ENDPOINT"),
	}
}

// newIntegrationAdapter constructs a live adapter, running the real preflight.
func newIntegrationAdapter(t *testing.T) domain.TrackerAdapter {
	t.Helper()
	adapter, err := NewGiteaAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewGiteaAdapter: %v", err)
	}
	return adapter
}

// firstCandidate returns the first candidate issue or skips when the
// repository has none, so a write test has a real issue to act on.
func firstCandidate(t *testing.T, adapter domain.TrackerAdapter, ctx context.Context) domain.Issue {
	t.Helper()
	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in repository; cannot run write test")
	}
	return candidates[0]
}

// parseCreatedAt parses a comment's CreatedAt so ordering checks compare
// chronological order rather than lexicographic string order.
func parseCreatedAt(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("CreatedAt %q is not a valid RFC 3339 timestamp: %v", raw, err)
	}
	return parsed
}

func TestIntegration_TransitionIssue(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	if err := adapter.TransitionIssue(ctx, issue.ID, issue.State); err != nil {
		t.Fatalf("TransitionIssue(%s, %q): %v", issue.Identifier, issue.State, err)
	}
}

func TestIntegration_CommentIssue(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	body := "sortie write-path integration test comment at " + time.Now().UTC().Format(time.RFC3339)
	if err := adapter.CommentIssue(ctx, issue.ID, body); err != nil {
		t.Fatalf("CommentIssue(%s): %v", issue.Identifier, err)
	}
}

func TestIntegration_AddLabel(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	label := "needs-human"

	if err := adapter.AddLabel(ctx, issue.ID, label); err != nil {
		t.Fatalf("AddLabel(%s, %q): %v", issue.Identifier, label, err)
	}
}

func TestIntegration_FetchCandidateIssues_QueryFilter(t *testing.T) {
	skipUnlessIntegration(t)

	cfg := integrationConfig(t)
	// A wide-open lower bound so the filter narrows nothing, keeping the
	// assertion (construction and fetch succeed with an operator filter
	// configured) stable regardless of the live repository's issue history.
	cfg["query_filter"] = "since=2000-01-01T00:00:00Z"

	adapter, err := NewGiteaAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGiteaAdapter with query_filter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := adapter.FetchCandidateIssues(ctx); err != nil {
		t.Fatalf("FetchCandidateIssues with query_filter: %v", err)
	}
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	t.Logf("FetchCandidateIssues returned %d issues", len(issues))

	for _, iss := range issues {
		if iss.ID == "" {
			t.Errorf("issue %q: ID is empty", iss.Identifier)
		}
		if iss.Labels == nil {
			t.Errorf("issue %s: Labels is nil, want non-nil slice", iss.Identifier)
		}
		if iss.BlockedBy == nil {
			t.Errorf("issue %s: BlockedBy is nil, want non-nil slice", iss.Identifier)
		}
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil for candidate fetch", iss.Identifier)
		}
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	fetched, err := adapter.FetchIssueByID(ctx, issue.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", issue.ID, err)
	}
	if fetched.ID != issue.ID {
		t.Errorf("ID = %q, want %q", fetched.ID, issue.ID)
	}
	if fetched.Comments == nil {
		t.Error("Comments is nil, want non-nil slice for fully populated issue")
	}
	if fetched.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil slice for fully populated issue")
	}
}

func TestIntegration_FetchIssueByID_NotFound(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := adapter.FetchIssueByID(ctx, "999999")
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

func TestIntegration_FetchIssuesByStates_Empty(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchIssuesByStates(ctx, []string{})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(empty): %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("FetchIssuesByStates(empty) returned %d issues, want 0", len(issues))
	}
}

func TestIntegration_FetchIssuesByStates(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in repository; cannot test FetchIssuesByStates")
	}

	requested := make(map[string]struct{})
	states := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := requested[c.State]; ok {
			continue
		}
		requested[c.State] = struct{}{}
		states = append(states, c.State)
	}

	issues, err := adapter.FetchIssuesByStates(ctx, states)
	if err != nil {
		t.Fatalf("FetchIssuesByStates(%v): %v", states, err)
	}
	if issues == nil {
		t.Fatal("FetchIssuesByStates returned nil, want non-nil slice")
	}
	for _, iss := range issues {
		if _, ok := requested[iss.State]; !ok {
			t.Errorf("FetchIssuesByStates(%v): issue %s State = %q, want one of %v", states, iss.Identifier, iss.State, states)
		}
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	stateMap, err := adapter.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if stateMap[issue.ID] == "" {
		t.Errorf("state for %s is empty or missing", issue.ID)
	}
}

func TestIntegration_FetchIssueStatesByIdentifiers(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	stateMap, err := adapter.FetchIssueStatesByIdentifiers(ctx, []string{issue.Identifier})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if stateMap[issue.Identifier] == "" {
		t.Errorf("state for %s is empty or missing", issue.Identifier)
	}
}

func TestIntegration_FetchIssueComments(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	comments, err := adapter.FetchIssueComments(ctx, issue.ID)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", issue.ID, err)
	}
	if comments == nil {
		t.Fatal("comments is nil, want non-nil slice")
	}

	for i := 1; i < len(comments); i++ {
		prev := parseCreatedAt(t, comments[i-1].CreatedAt)
		curr := parseCreatedAt(t, comments[i].CreatedAt)
		if prev.After(curr) {
			t.Errorf("comments not in ascending createdAt order at index %d: %q before %q",
				i, comments[i-1].CreatedAt, comments[i].CreatedAt)
		}
	}
}
