package gitea

import (
	"context"
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
