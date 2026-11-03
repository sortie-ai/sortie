package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The constructor uses the default logger, so these logging assertions run
// serially while it is replaced.
func TestPriorityLabelsCatalog(t *testing.T) {
	for _, tt := range []struct {
		name           string
		failDiagnostic bool
		warnings       int
	}{
		{name: "missing label warns", warnings: 1},
		{name: "catalog failure is advisory", failDiagnostic: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			buf := swapDefaultLogger(t)
			s := newFakeServer(t)
			registerTokenAndProject(t, s)
			var reads atomic.Int32
			s.handle("/api/v4/projects/"+testEscapedProject+"/labels", func(w http.ResponseWriter, r *http.Request) {
				if reads.Add(1) > 1 && tt.failDiagnostic {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				fmt.Fprint(w, `[{"id":1,"name":"PRIORITY/High"},{"id":2,"name":"backlog"}]`) //nolint:errcheck // test server
			})
			s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `[{"iid":7,"title":"Issue","state":"opened","labels":["backlog","Priority/High"]}]`) //nolint:errcheck // test server
			})
			srv := httptest.NewServer(s)
			t.Cleanup(srv.Close)
			cfg := validConfig(testProject)
			cfg["endpoint"] = srv.URL
			cfg["priority_labels"] = map[string]any{"PRIORITY/High": 2, "missing-priority": 4}
			adapter, err := NewGitLabAdapter(cfg)
			if err != nil {
				t.Fatalf("NewGitLabAdapter(priority_labels) = %v", err)
			}
			if got := strings.Count(buf.String(), "priority_labels label does not resolve against repository labels"); got != tt.warnings {
				t.Errorf("NewGitLabAdapter(priority_labels) warning count = %d, want %d; log: %s", got, tt.warnings, buf.String())
			}
			if tt.warnings > 0 && (!strings.Contains(buf.String(), "label=missing-priority") || strings.Contains(buf.String(), "label=priority/high")) {
				t.Errorf("NewGitLabAdapter(priority_labels) log = %q, want missing-priority only", buf.String())
			}
			if tt.failDiagnostic && !strings.Contains(buf.String(), "gitlab priority_labels catalog unavailable") {
				t.Errorf("NewGitLabAdapter(priority_labels) log = %q, want catalog failure warning", buf.String())
			}
			issues, err := adapter.FetchCandidateIssues(context.Background())
			if err != nil || len(issues) != 1 || issues[0].Priority == nil || *issues[0].Priority != 2 {
				t.Errorf("FetchCandidateIssues(priority_labels) = %+v, %v, want one issue with priority 2", issues, err)
			}
		})
	}
}
