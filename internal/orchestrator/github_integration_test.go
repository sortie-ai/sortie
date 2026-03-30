package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"

	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/tracker/github"
)

// --- GitHub E2E test helpers ---

// skipUnlessGitHubE2E skips the test unless SORTIE_GITHUB_E2E=1 and the
// required credentials are present. Required environment variables:
//
//	SORTIE_GITHUB_E2E=1              opt-in gate
//	SORTIE_GITHUB_TOKEN=ghp_...      PAT with Issues read/write
//	SORTIE_GITHUB_PROJECT=owner/repo target repository
//
// Example invocation:
//
//	SORTIE_GITHUB_E2E=1 \
//	SORTIE_GITHUB_TOKEN=ghp_xxxx \
//	SORTIE_GITHUB_PROJECT=sortie-ai/sortie-test \
//	go test ./internal/orchestrator/... -run TestGitHubIntegration -v -count=1
func skipUnlessGitHubE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping GitHub E2E test in short mode")
	}
	if os.Getenv("SORTIE_GITHUB_E2E") != "1" {
		t.Skip("skipping GitHub E2E test: set SORTIE_GITHUB_E2E=1")
	}
	if os.Getenv("SORTIE_GITHUB_TOKEN") == "" {
		t.Skip("skipping GitHub E2E test: SORTIE_GITHUB_TOKEN not set")
	}
	if os.Getenv("SORTIE_GITHUB_PROJECT") == "" {
		t.Skip("skipping GitHub E2E test: SORTIE_GITHUB_PROJECT not set")
	}
}

// githubAPIClient is a minimal HTTP client for test setup/teardown
// against the GitHub REST API. It is not used during the test itself;
// the orchestrator uses the real adapter.
type githubAPIClient struct {
	token      string
	owner      string
	repo       string
	baseURL    string
	httpClient *http.Client
}

func newGitHubAPIClient(t *testing.T) *githubAPIClient {
	t.Helper()
	project := os.Getenv("SORTIE_GITHUB_PROJECT")
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("SORTIE_GITHUB_PROJECT must be owner/repo, got %q", project)
	}
	return &githubAPIClient{
		token:      os.Getenv("SORTIE_GITHUB_TOKEN"),
		owner:      parts[0],
		repo:       parts[1],
		baseURL:    "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// githubIssueResponse is the subset of the GitHub issue JSON we need.
type githubIssueResponse struct {
	Number int               `json:"number"`
	State  string            `json:"state"`
	Title  string            `json:"title"`
	Labels []githubLabelResp `json:"labels"`
}

type githubLabelResp struct {
	Name string `json:"name"`
}

func (c *githubAPIClient) doRequest(t *testing.T, method, path string, body any) []byte {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper; best-effort cleanup

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody
}

// createTestIssue creates a GitHub issue with the given title and labels.
// Returns the issue number as a string.
func (c *githubAPIClient) createTestIssue(t *testing.T, title string, labels []string) string {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo)
	body := map[string]any{
		"title":  title,
		"labels": labels,
	}
	resp := c.doRequest(t, "POST", path, body)

	var issue githubIssueResponse
	if err := json.Unmarshal(resp, &issue); err != nil {
		t.Fatalf("unmarshal created issue: %v", err)
	}
	t.Logf("created test issue #%d: %q", issue.Number, issue.Title)
	return fmt.Sprintf("%d", issue.Number)
}

// fetchIssue fetches the current state of a GitHub issue.
func (c *githubAPIClient) fetchIssue(t *testing.T, number string) githubIssueResponse {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues/%s", c.owner, c.repo, number)
	resp := c.doRequest(t, "GET", path, nil)

	var issue githubIssueResponse
	if err := json.Unmarshal(resp, &issue); err != nil {
		t.Fatalf("unmarshal issue: %v", err)
	}
	return issue
}

// restoreIssueState removes state labels added by the orchestrator and closes
// the issue as not_planned, leaving the test repository clean.
func (c *githubAPIClient) restoreIssueState(t *testing.T, number string) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues/%s", c.owner, c.repo, number)

	// Remove any state labels the orchestrator may have added.
	for _, label := range []string{"done", "wontfix", "in-progress", "review"} {
		labelPath := fmt.Sprintf("/repos/%s/%s/issues/%s/labels/%s", c.owner, c.repo, number, label)
		// Ignore errors — label may not be present.
		req, err := http.NewRequestWithContext(context.Background(), "DELETE", c.baseURL+labelPath, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		resp, err := c.httpClient.Do(req)
		if err == nil {
			resp.Body.Close() //nolint:errcheck // cleanup helper
		}
	}

	// Close the issue to leave the test repo clean.
	c.doRequest(t, "PATCH", path, map[string]any{
		"state":        "closed",
		"state_reason": "not_planned",
	})
	t.Logf("cleanup: restored and closed issue #%s", number)
}

// --- Integration test ---

// TestGitHubIntegration_FullDispatchCycle verifies the full orchestrator
// dispatch cycle with the real GitHub adapter: poll → dispatch mock agent →
// handoff transition (label swap + close).
func TestGitHubIntegration_FullDispatchCycle(t *testing.T) {
	skipUnlessGitHubE2E(t)

	ctx := context.Background()

	// --- Setup: GitHub API client for test issue management ---
	ghClient := newGitHubAPIClient(t)

	// Create a test issue with the "backlog" label.
	issueTitle := fmt.Sprintf("sortie-e2e-%d", time.Now().UnixNano())
	issueNumber := ghClient.createTestIssue(t, issueTitle, []string{"backlog"})
	t.Cleanup(func() { ghClient.restoreIssueState(t, issueNumber) })

	// --- Setup: real GitHub tracker adapter via registry ---
	trackerFactory, err := registry.Trackers.Get("github")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(%q): %v", "github", err)
	}
	trackerAdapter, err := trackerFactory(map[string]any{
		"api_key":         os.Getenv("SORTIE_GITHUB_TOKEN"),
		"project":         os.Getenv("SORTIE_GITHUB_PROJECT"),
		"active_states":   []string{"backlog", "in-progress", "review"},
		"terminal_states": []string{"done", "wontfix"},
		// Scope fetches to this test's issue only so concurrent test runs
		// cannot dispatch one another's issues.
		"query_filter": issueTitle + " in:title",
	})
	if err != nil {
		t.Fatalf("NewGitHubAdapter: %v", err)
	}

	// --- Setup: mock agent adapter via registry ---
	agentFactory, err := registry.Agents.Get("mock")
	if err != nil {
		t.Fatalf("registry.Agents.Get(%q): %v", "mock", err)
	}
	agentAdapter, err := agentFactory(map[string]any{
		"max_turns": 1,
	})
	if err != nil {
		t.Fatalf("NewMockAdapter: %v", err)
	}

	// --- Setup: real SQLite store ---
	dbPath := t.TempDir() + "/test.db"
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	// --- Setup: config and workflow manager ---
	workspaceRoot := t.TempDir()
	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "github",
			APIKey:         os.Getenv("SORTIE_GITHUB_TOKEN"),
			Project:        os.Getenv("SORTIE_GITHUB_PROJECT"),
			ActiveStates:   []string{"backlog", "in-progress", "review"},
			TerminalStates: []string{"done", "wontfix"},
			HandoffState:   "done",
		},
		Polling:   config.PollingConfig{IntervalMS: 5000},
		Workspace: config.WorkspaceConfig{Root: workspaceRoot},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			MaxConcurrentAgents: 1,
			MaxTurns:            1,
			ReadTimeoutMS:       1000,
		},
		Extensions: map[string]any{},
	}

	tmpl, err := prompt.Parse("work on {{ .issue.identifier }}", "test", 0)
	if err != nil {
		t.Fatalf("prompt.Parse: %v", err)
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}
	regs := passingPreflightRegistries()

	// --- Setup: orchestrator state and construction ---
	state := NewState(
		cfg.Polling.IntervalMS,
		cfg.Agent.MaxConcurrentAgents,
		nil,
		AgentTotals{},
	)

	orch := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  trackerAdapter,
		AgentAdapter:    agentAdapter,
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	// --- Execution: run orchestrator with a timeout ---
	testCtx, testCancel := context.WithTimeout(ctx, 90*time.Second)
	defer testCancel()

	orchCtx, orchCancel := context.WithCancel(testCtx)
	done := make(chan struct{})
	go func() {
		orch.Run(orchCtx)
		close(done)
	}()

	// Poll the GitHub issue for the handoff transition to complete.
	// HandleWorkerExit persists run history before calling TransitionIssue,
	// so we cannot rely on run history alone — we must wait for the actual
	// label swap and state change on GitHub.
	t.Logf("waiting for orchestrator to dispatch and complete issue #%s...", issueNumber)
	var issue githubIssueResponse
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			orchCancel()
			<-done
			t.Fatalf("timed out waiting for issue #%s to transition to 'done'", issueNumber)
		default:
		}

		issue = ghClient.fetchIssue(t, issueNumber)
		hasDone := false
		for _, l := range issue.Labels {
			if strings.EqualFold(l.Name, "done") {
				hasDone = true
				break
			}
		}
		if hasDone && issue.State == "closed" {
			t.Logf("issue #%s transitioned: state=%q, labels include 'done'", issueNumber, issue.State)
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Stop the orchestrator.
	orchCancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("orchestrator did not shut down within 15 seconds")
	}

	// handoff_state="done" is terminal → issue should be closed.
	if issue.State != "closed" {
		t.Errorf("issue state = %q, want %q", issue.State, "closed")
	}

	// Issue should have the "done" label.
	hasLabel := func(name string) bool {
		for _, l := range issue.Labels {
			if strings.EqualFold(l.Name, name) {
				return true
			}
		}
		return false
	}
	if !hasLabel("done") {
		labels := make([]string, len(issue.Labels))
		for i, l := range issue.Labels {
			labels[i] = l.Name
		}
		t.Errorf("issue labels = %v, want 'done' present", labels)
	}
	if hasLabel("backlog") {
		t.Error("issue still has 'backlog' label after handoff to 'done'")
	}

	// --- Verification: run history in SQLite ---
	entries, err := store.QueryRunHistoryByIssue(ctx, issueNumber)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no run history entries for the test issue")
	}
	t.Logf("run history: %d entries, latest status=%q", len(entries), entries[0].Status)

	// --- Verification: workspace directory was created ---
	// The workspace manager creates a directory under workspaceRoot keyed
	// by the issue identifier. The GitHub adapter uses the issue number
	// as both ID and Identifier.
	wsEntries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", workspaceRoot, err)
	}
	if len(wsEntries) == 0 {
		t.Error("workspace root is empty; expected a workspace directory")
	} else {
		t.Logf("workspace directories: %d entries", len(wsEntries))
	}
}
