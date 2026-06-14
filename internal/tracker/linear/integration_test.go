package linear

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when SORTIE_LINEAR_TEST is not
// "1", so disabled integration tests report as skipped rather than passing
// silently or hitting the network.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_LINEAR_TEST") != "1" {
		t.Skip("skipping Linear integration test: set SORTIE_LINEAR_TEST=1 to enable")
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
	cfg := map[string]any{
		"api_key": requireEnv(t, "SORTIE_LINEAR_API_KEY"),
		"project": requireEnv(t, "SORTIE_LINEAR_TEAM_KEY"),
	}
	if endpoint := os.Getenv("SORTIE_LINEAR_ENDPOINT"); endpoint != "" {
		cfg["endpoint"] = endpoint
	}
	if states := os.Getenv("SORTIE_LINEAR_ACTIVE_STATES"); states != "" {
		cfg["active_states"] = splitStates(states)
	}
	return cfg
}

// splitStates parses a comma-separated state list into a slice of any for the
// config map.
func splitStates(raw string) []any {
	parts := strings.Split(raw, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// newIntegrationAdapter constructs a live adapter, running the real preflight.
func newIntegrationAdapter(t *testing.T) domain.TrackerAdapter {
	t.Helper()
	adapter, err := NewLinearAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}
	return adapter
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

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team; cannot test FetchIssueByID")
	}

	identifier := candidates[0].Identifier
	iss, err := adapter.FetchIssueByID(ctx, identifier)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", identifier, err)
	}
	if iss.Identifier != identifier {
		t.Errorf("Identifier = %q, want %q", iss.Identifier, identifier)
	}
	if iss.Comments == nil {
		t.Error("Comments is nil, want non-nil slice for fully populated issue")
	}
}

func TestIntegration_FetchIssueByID_NotFound(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := adapter.FetchIssueByID(ctx, "00000000-0000-0000-0000-000000000000")
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

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team")
	}

	ids := []string{candidates[0].ID}
	stateMap, err := adapter.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if stateMap[candidates[0].ID] == "" {
		t.Errorf("state for %s is empty or missing", candidates[0].ID)
	}
}

func TestIntegration_FetchIssueStatesByIdentifiers(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team")
	}

	identifier := candidates[0].Identifier
	stateMap, err := adapter.FetchIssueStatesByIdentifiers(ctx, []string{identifier})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if stateMap[identifier] == "" {
		t.Errorf("state for %s is empty or missing", identifier)
	}
}

func TestIntegration_FetchIssueComments(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team")
	}

	comments, err := adapter.FetchIssueComments(ctx, candidates[0].Identifier)
	if err != nil {
		t.Fatalf("FetchIssueComments(%s): %v", candidates[0].Identifier, err)
	}
	if comments == nil {
		t.Fatal("comments is nil, want non-nil slice")
	}

	for i := 1; i < len(comments); i++ {
		if comments[i-1].CreatedAt > comments[i].CreatedAt {
			t.Errorf("comments not in ascending createdAt order at index %d: %q before %q",
				i, comments[i-1].CreatedAt, comments[i].CreatedAt)
		}
	}
}

// firstCandidate returns the first candidate issue or skips when the team has
// none, so a write test has a real issue to act on without mutating ordering.
func firstCandidate(t *testing.T, adapter domain.TrackerAdapter, ctx context.Context) domain.Issue {
	t.Helper()
	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team; cannot run write test")
	}
	return candidates[0]
}

func TestIntegration_TransitionIssue(t *testing.T) {
	skipUnlessIntegration(t)

	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue := firstCandidate(t, adapter, ctx)

	if err := adapter.TransitionIssue(ctx, issue.Identifier, issue.State); err != nil {
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
	if err := adapter.CommentIssue(ctx, issue.Identifier, body); err != nil {
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

	if err := adapter.AddLabel(ctx, issue.Identifier, label); err != nil {
		t.Fatalf("AddLabel(%s, %q): %v", issue.Identifier, label, err)
	}
}
