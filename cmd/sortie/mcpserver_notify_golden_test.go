package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

var recordGolden = flag.Bool("record-golden", false, "rewrite the golden notification fixtures instead of comparing against them")

const goldenNotifyDir = "testdata/golden_notify"

type goldenRequest struct {
	Server  string      `json:"server"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Headers http.Header `json:"headers"`
	Body    string      `json:"body"`
}

type goldenCall struct {
	Input    string          `json:"input"`
	Result   string          `json:"result"`
	Requests []goldenRequest `json:"requests"`
}

type goldenOperatorRecord struct {
	Cap   int          `json:"cap"`
	Calls []goldenCall `json:"calls"`
}

type goldenRequestLog struct {
	mu       sync.Mutex
	requests []goldenRequest
}

func (l *goldenRequestLog) add(request goldenRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, request)
}

func (l *goldenRequestLog) drain() []goldenRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	requests := append([]goldenRequest{}, l.requests...)
	l.requests = nil
	return requests
}

func newGoldenServer(t *testing.T, label string, log *goldenRequestLog) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		headers := r.Header.Clone()
		// Content-Length counts the webhook source field, which is the host name.
		headers.Del("Content-Length")
		log.add(goldenRequest{Server: label, Method: r.Method, Path: r.URL.Path, Headers: headers, Body: string(body)})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/hooks/" + label
}

var goldenEndpointKey = map[string]string{"slack": "webhook_url", "webhook": "url"}

type goldenOperatorScenario struct {
	name    string
	entries []map[string]any
	calls   []string
}

var goldenOperatorInputs = []string{
	`{"severity":"info","title":"Build finished","body":"All checks passed."}`,
	`{"severity":"critical","title":"Decision needed","body":"Pick a migration path.","category":"decision_needed"}`,
}

func goldenOperatorScenarios() []goldenOperatorScenario {
	return []goldenOperatorScenario{
		{name: "slack_without_events", entries: []map[string]any{{"kind": "slack"}}, calls: goldenOperatorInputs},
		{name: "webhook_without_events", entries: []map[string]any{{"kind": "webhook"}}, calls: goldenOperatorInputs},
		{name: "slack_and_webhook_without_events", entries: []map[string]any{{"kind": "slack"}, {"kind": "webhook"}}, calls: goldenOperatorInputs},
		{name: "webhook_cap_one", entries: []map[string]any{{"kind": "webhook", "max_per_session": 1}}, calls: goldenOperatorInputs},
	}
}

func goldenOperatorConfig(t *testing.T, sc goldenOperatorScenario, log *goldenRequestLog) config.ServiceConfig {
	t.Helper()

	notifications := make([]any, 0, len(sc.entries))
	for _, entry := range sc.entries {
		kind, _ := entry["kind"].(string)
		wired := maps.Clone(entry)
		wired[goldenEndpointKey[kind]] = newGoldenServer(t, kind, log)
		notifications = append(notifications, wired)
	}

	cfg, err := config.NewServiceConfig(map[string]any{"notifications": notifications})
	if err != nil {
		t.Fatalf("NewServiceConfig(%s): %v", sc.name, err)
	}
	return cfg
}

// goldenOperatorTool is the only place a notify_operator call reaches its
// backends, so a change to the sidecar wiring edits this function alone.
func goldenOperatorTool(t *testing.T, cfg config.ServiceConfig) domain.AgentTool {
	t.Helper()

	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".sortie"), 0o755); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}

	attempt := 2
	reg, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), SessionToolParams{
		WorkspacePath: workspace,
		IssueID:       "GOLD-1",
		Identifier:    "GOLD-1",
		DispatchID:    "golden-dispatch",
		Attempt:       &attempt,
		AgentKind:     "claude-code",
		Notifications: cfg.Notifications.Backends,
	})
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry: %v", err)
	}
	tool, ok := reg.Registry.Get("notify_operator")
	if !ok {
		t.Fatal("notify_operator not registered")
	}
	return tool
}

var goldenVolatileFields = regexp.MustCompile(`"(notification_id|timestamp)":"[^"]*"`)

func maskGoldenVolatile(t *testing.T, text string) string {
	t.Helper()

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("Hostname: %v", err)
	}
	encodedHost, err := json.Marshal(host)
	if err != nil {
		t.Fatalf("Marshal(%q): %v", host, err)
	}
	text = strings.ReplaceAll(text, `"source":`+string(encodedHost), `"source":"masked"`)
	return goldenVolatileFields.ReplaceAllString(text, `"$1":"masked"`)
}

func checkGoldenOperator(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join(goldenNotifyDir, name+".json")
	if *recordGolden {
		if err := os.MkdirAll(goldenNotifyDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", goldenNotifyDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden fixture %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden %s differs\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestGoldenNotifyOperator(t *testing.T) {
	t.Parallel()

	for _, sc := range goldenOperatorScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			log := &goldenRequestLog{}
			cfg := goldenOperatorConfig(t, sc, log)
			tool := goldenOperatorTool(t, cfg)

			record := goldenOperatorRecord{Cap: resolveNotificationCap(cfg.Notifications.Backends)}
			for _, input := range sc.calls {
				result, err := tool.Execute(context.Background(), json.RawMessage(input))
				if err != nil {
					t.Fatalf("notify_operator Execute(%s): %v", input, err)
				}
				if strings.Contains(string(result), "state_unavailable") {
					t.Fatalf("notify_operator Execute(%s) = %s, want the slot reservation to work", input, result)
				}
				requests := log.drain()
				for i := range requests {
					requests[i].Body = maskGoldenVolatile(t, requests[i].Body)
				}
				record.Calls = append(record.Calls, goldenCall{
					Input:    input,
					Result:   maskGoldenVolatile(t, string(result)),
					Requests: requests,
				})
			}

			got, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				t.Fatalf("MarshalIndent(%s): %v", sc.name, err)
			}
			checkGoldenOperator(t, sc.name, append(got, '\n'))
		})
	}
}
