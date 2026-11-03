package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestLabelTrackerPriorityDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		priorities map[string]int
		wantID     string
		wantKind   string
	}{
		{name: "mapped priority overrides creation order", priorities: map[string]int{"priority/high": 1, "priority/low": 4}, wantID: "2", wantKind: "kind-b"},
		{name: "unconfigured preserves creation order", wantID: "1", wantKind: "kind-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/owner/repo/labels", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `[{"name":"To Do"}]`) //nolint:errcheck // test server
			})
			mux.HandleFunc("GET /repos/owner/repo/issues", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `[
     {"number":1,"title":"Older low priority","state":"open","labels":[{"name":"To Do"},{"name":"priority/low"}],"created_at":"2026-01-01T00:00:00Z","issue_dependencies_summary":{"total_blocked_by":0}},
     {"number":2,"title":"Newer urgent","state":"open","labels":[{"name":"To Do"},{"name":"PRIORITY/High"}],"created_at":"2026-02-01T00:00:00Z","issue_dependencies_summary":{"total_blocked_by":0}}
    ]`) //nolint:errcheck // test server
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			constructor, err := registry.Trackers.Get("github")
			if err != nil {
				t.Fatalf("Trackers.Get(github) = %v", err)
			}
			tracker, err := constructor(map[string]any{
				"api_key": "test-token", "project": "owner/repo", "endpoint": srv.URL,
				"active_states": []string{"To Do"}, "terminal_states": []string{"Done"}, "priority_labels": tt.priorities,
			})
			if err != nil {
				t.Fatalf("tracker constructor() = %v", err)
			}
			candidates, err := tracker.FetchCandidateIssues(context.Background())
			if err != nil || len(candidates) != 2 {
				t.Fatalf("FetchCandidateIssues() = %v, %v, want two candidates", candidates, err)
			}

			cfg := runConfig(t, map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}})
			cfg.Tracker.Kind = "github"
			cfg.Agent.MaxConcurrentAgents = 1
			cfg.SetExtensionSection("kind-b", map[string]any{})
			cfg.Dispatch.Rules = []config.DispatchRule{{Name: "urgent", Match: config.DispatchMatch{Priority: &config.PriorityPredicate{Op: "eq", Value: 1}}, Selection: config.DispatchSelection{AgentKind: "kind-b"}}}
			h := newRunHarness(t, cfg, "kind-a", "kind-b")
			h.tracker.candidates = candidates
			for _, issue := range candidates {
				h.tracker.issues[issue.ID] = issue
			}
			for _, recorder := range h.recorders {
				recorder.adapter.runTurnFn = func(ctx context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					<-ctx.Done()
					return domain.TurnResult{}, ctx.Err()
				}
			}
			h.run()
			sessions := h.waitForSessions(1)
			h.stop()
			got := sessions[tt.wantKind]
			if len(got) != 1 || filepath.Base(got[0].WorkspacePath) != tt.wantID {
				t.Errorf("dispatch sessions = %v, want issue %s with %s", sessions, tt.wantID, tt.wantKind)
			}
		})
	}
}
