package gitea

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The constructor uses the default logger, so these logging assertions run
// serially while it is replaced.
func TestPriorityLabelsCatalog(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		warnings int
	}{
		{name: "missing label warns", status: http.StatusOK, warnings: 1},
		{name: "catalog failure is advisory", status: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prior := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prior) })
			mux := newPreflightMux(t)
			mux.HandleFunc("GET /api/v1/repos/acme/widgets/labels", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `[{"id":1,"name":"PRIORITY/High"}]`) //nolint:errcheck // test server
			})
			mux.HandleFunc("GET /api/v1/orgs/acme/labels", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
			mux.HandleFunc("GET /api/v1/repos/acme/widgets/issues", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `[{"number":7,"title":"Issue","state":"open","labels":[{"name":"backlog"},{"name":"Priority/High"}]}]`) //nolint:errcheck // test server
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			cfg := validConfig(srv.URL)
			cfg["priority_labels"] = map[string]any{"PRIORITY/High": 2, "missing-priority": 4}
			adapter, err := NewGiteaAdapter(cfg)
			if err != nil {
				t.Fatalf("NewGiteaAdapter(priority_labels) = %v", err)
			}
			if got := strings.Count(buf.String(), "priority_labels label does not resolve against repository labels"); got != tt.warnings {
				t.Errorf("NewGiteaAdapter(priority_labels) warning count = %d, want %d; log: %s", got, tt.warnings, buf.String())
			}
			if tt.warnings > 0 && (!strings.Contains(buf.String(), "label=missing-priority") || strings.Contains(buf.String(), "label=priority/high")) {
				t.Errorf("NewGiteaAdapter(priority_labels) log = %q, want missing-priority only", buf.String())
			}
			if tt.status != http.StatusOK && !strings.Contains(buf.String(), "failed to fetch labels for priority_labels diagnostic") {
				t.Errorf("NewGiteaAdapter(priority_labels) log = %q, want catalog failure warning", buf.String())
			}
			issues, err := adapter.FetchCandidateIssues(context.Background())
			if err != nil || len(issues) != 1 || issues[0].Priority == nil || *issues[0].Priority != 2 {
				t.Errorf("FetchCandidateIssues(priority_labels) = %+v, %v, want one issue with priority 2", issues, err)
			}
		})
	}
}
