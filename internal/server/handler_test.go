package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// --- Test helpers ---

func fixedSnapshot(snap orchestrator.RuntimeSnapshotResult) SnapshotFunc {
	return func() (orchestrator.RuntimeSnapshotResult, error) {
		return snap, nil
	}
}

func failingSnapshot(msg string) SnapshotFunc {
	return func() (orchestrator.RuntimeSnapshotResult, error) {
		return orchestrator.RuntimeSnapshotResult{}, fmt.Errorf("%s", msg)
	}
}

func acceptingRefresh() RefreshFunc {
	return func() bool { return true }
}

func coalescingRefresh() RefreshFunc {
	return func() bool { return false }
}

func testServer(t *testing.T, snapFn SnapshotFunc, refreshFn RefreshFunc) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn: snapFn,
		RefreshFn:  refreshFn,
		Logger:     slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	return v
}

// --- Wire-type constructor tests ---

func TestToRunningEntryResponse(t *testing.T) {
	t.Parallel()

	// Use a non-UTC time to verify UTC normalization.
	est := time.FixedZone("EST", -5*60*60)
	startedAt := time.Date(2026, 3, 24, 12, 0, 0, 0, est)
	lastEventAt := time.Date(2026, 3, 24, 12, 5, 0, 0, est)

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:            "issue-1",
		Identifier:         "MT-100",
		State:              "In Progress",
		SessionID:          "sess-abc",
		TurnCount:          5,
		LastAgentEvent:     domain.EventTurnCompleted,
		LastAgentTimestamp: lastEventAt,
		LastAgentMessage:   "Implementing feature",
		StartedAt:          startedAt,
		AgentInputTokens:   1000,
		AgentOutputTokens:  500,
		AgentTotalTokens:   1500,
		WorkspacePath:      "/tmp/ws/MT-100",
	}

	got := toRunningEntryResponse(entry)

	if got.IssueID != "issue-1" {
		t.Errorf("IssueID = %q, want %q", got.IssueID, "issue-1")
	}
	if got.IssueIdentifier != "MT-100" {
		t.Errorf("IssueIdentifier = %q, want %q", got.IssueIdentifier, "MT-100")
	}
	if got.State != "In Progress" {
		t.Errorf("State = %q, want %q", got.State, "In Progress")
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-abc")
	}
	if got.TurnCount != 5 {
		t.Errorf("TurnCount = %d, want %d", got.TurnCount, 5)
	}
	if got.LastEvent != string(domain.EventTurnCompleted) {
		t.Errorf("LastEvent = %q, want %q", got.LastEvent, domain.EventTurnCompleted)
	}
	if got.LastMessage != "Implementing feature" {
		t.Errorf("LastMessage = %q, want %q", got.LastMessage, "Implementing feature")
	}
	if got.WorkspacePath != "/tmp/ws/MT-100" {
		t.Errorf("WorkspacePath = %q, want %q", got.WorkspacePath, "/tmp/ws/MT-100")
	}

	// UTC normalization
	if got.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt location = %v, want UTC", got.StartedAt.Location())
	}
	if !got.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, startedAt)
	}
	if got.LastEventAt.Location() != time.UTC {
		t.Errorf("LastEventAt location = %v, want UTC", got.LastEventAt.Location())
	}

	// Token info
	if got.Tokens.InputTokens != 1000 {
		t.Errorf("Tokens.InputTokens = %d, want %d", got.Tokens.InputTokens, 1000)
	}
	if got.Tokens.OutputTokens != 500 {
		t.Errorf("Tokens.OutputTokens = %d, want %d", got.Tokens.OutputTokens, 500)
	}
	if got.Tokens.TotalTokens != 1500 {
		t.Errorf("Tokens.TotalTokens = %d, want %d", got.Tokens.TotalTokens, 1500)
	}
}

func TestToRetryEntryResponse(t *testing.T) {
	t.Parallel()

	dueAtMS := int64(1711276800000) // 2024-03-24T12:00:00.000Z
	entry := orchestrator.SnapshotRetryEntry{
		IssueID:    "retry-1",
		Identifier: "MT-301",
		Attempt:    3,
		DueAtMS:    dueAtMS,
		Error:      "agent timeout",
	}

	got := toRetryEntryResponse(entry)

	if got.IssueID != "retry-1" {
		t.Errorf("IssueID = %q, want %q", got.IssueID, "retry-1")
	}
	if got.IssueIdentifier != "MT-301" {
		t.Errorf("IssueIdentifier = %q, want %q", got.IssueIdentifier, "MT-301")
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d, want %d", got.Attempt, 3)
	}
	if got.Error != "agent timeout" {
		t.Errorf("Error = %q, want %q", got.Error, "agent timeout")
	}

	// DueAt should be UTC.
	if got.DueAt.Location() != time.UTC {
		t.Errorf("DueAt location = %v, want UTC", got.DueAt.Location())
	}
	wantDueAt := time.UnixMilli(dueAtMS).UTC()
	if !got.DueAt.Equal(wantDueAt) {
		t.Errorf("DueAt = %v, want %v", got.DueAt, wantDueAt)
	}
}

func TestToStateResponse(t *testing.T) {
	t.Parallel()

	t.Run("empty snapshot produces non-nil slices", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
			Running:     []orchestrator.SnapshotRunningEntry{},
			Retrying:    []orchestrator.SnapshotRetryEntry{},
			AgentTotals: orchestrator.SnapshotAgentTotals{},
		}

		got := toStateResponse(snap)

		if got.Running == nil {
			t.Fatal("Running = nil, want non-nil empty slice")
		}
		if len(got.Running) != 0 {
			t.Errorf("len(Running) = %d, want 0", len(got.Running))
		}
		if got.Retrying == nil {
			t.Fatal("Retrying = nil, want non-nil empty slice")
		}
		if len(got.Retrying) != 0 {
			t.Errorf("len(Retrying) = %d, want 0", len(got.Retrying))
		}
		if got.Counts.Running != 0 {
			t.Errorf("Counts.Running = %d, want 0", got.Counts.Running)
		}
		if got.Counts.Retrying != 0 {
			t.Errorf("Counts.Retrying = %d, want 0", got.Counts.Retrying)
		}
		if got.RateLimits == nil {
			t.Fatal("RateLimits = nil, want non-nil empty map")
		}
	})

	t.Run("nil RateLimits becomes empty map", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
			Running:     nil,
			Retrying:    nil,
			RateLimits:  nil,
		}

		got := toStateResponse(snap)

		if got.RateLimits == nil {
			t.Fatal("RateLimits = nil, want non-nil empty map")
		}
		if len(got.RateLimits) != 0 {
			t.Errorf("len(RateLimits) = %d, want 0", len(got.RateLimits))
		}
	})

	t.Run("counts match entries", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "a", Identifier: "MT-1"},
				{IssueID: "b", Identifier: "MT-2"},
			},
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "c", Identifier: "MT-3"},
			},
		}

		got := toStateResponse(snap)

		if got.Counts.Running != 2 {
			t.Errorf("Counts.Running = %d, want 2", got.Counts.Running)
		}
		if got.Counts.Retrying != 1 {
			t.Errorf("Counts.Retrying = %d, want 1", got.Counts.Retrying)
		}
	})
}

func TestBuildIssueDetail(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	snap := orchestrator.RuntimeSnapshotResult{
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:       "id-1",
				Identifier:    "MT-100",
				State:         "In Progress",
				StartedAt:     now,
				WorkspacePath: "/tmp/ws/mt-100",
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				IssueID:    "id-2",
				Identifier: "MT-200",
				Attempt:    2,
				DueAtMS:    now.UnixMilli(),
				Error:      "boom",
			},
		},
	}

	t.Run("running issue", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("MT-100", snap)
		if got == nil {
			t.Fatal("buildIssueDetail(MT-100) = nil, want non-nil")
		}
		if got.Status != "running" {
			t.Errorf("Status = %q, want %q", got.Status, "running")
		}
		if got.IssueID != "id-1" {
			t.Errorf("IssueID = %q, want %q", got.IssueID, "id-1")
		}
		if got.Running == nil {
			t.Fatal("Running = nil, want non-nil")
		}
		if got.Retry != nil {
			t.Errorf("Retry = %v, want nil for running issue", got.Retry)
		}
		if got.Workspace == nil {
			t.Fatal("Workspace = nil, want non-nil for running issue with path")
		}
		if got.Workspace.Path != "/tmp/ws/mt-100" {
			t.Errorf("Workspace.Path = %q, want %q", got.Workspace.Path, "/tmp/ws/mt-100")
		}
		if got.Attempts == nil {
			t.Fatal("Attempts = nil, want non-nil")
		}
		if got.RecentEvents == nil {
			t.Fatal("RecentEvents = nil, want non-nil empty slice")
		}
		if got.Tracked == nil {
			t.Fatal("Tracked = nil, want non-nil empty map")
		}
	})

	t.Run("retrying issue", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("MT-200", snap)
		if got == nil {
			t.Fatal("buildIssueDetail(MT-200) = nil, want non-nil")
		}
		if got.Status != "retrying" {
			t.Errorf("Status = %q, want %q", got.Status, "retrying")
		}
		if got.Running != nil {
			t.Errorf("Running = %v, want nil for retrying issue", got.Running)
		}
		if got.Retry == nil {
			t.Fatal("Retry = nil, want non-nil")
		}
		if got.Retry.Attempt != 2 {
			t.Errorf("Retry.Attempt = %d, want %d", got.Retry.Attempt, 2)
		}
		if got.LastError == nil {
			t.Fatal("LastError = nil, want non-nil for retrying issue with error")
		}
		if *got.LastError != "boom" {
			t.Errorf("LastError = %q, want %q", *got.LastError, "boom")
		}
		if got.Workspace != nil {
			t.Errorf("Workspace = %v, want nil for retrying issue", got.Workspace)
		}
		if got.Attempts.RestartCount != 1 {
			t.Errorf("Attempts.RestartCount = %d, want %d", got.Attempts.RestartCount, 1)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("NONEXISTENT", snap)
		if got != nil {
			t.Errorf("buildIssueDetail(NONEXISTENT) = %v, want nil", got)
		}
	})

	t.Run("running issue without workspace path", func(t *testing.T) {
		t.Parallel()
		noWSSnap := orchestrator.RuntimeSnapshotResult{
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "id-3", Identifier: "MT-300", State: "To Do"},
			},
		}
		got := buildIssueDetail("MT-300", noWSSnap)
		if got == nil {
			t.Fatal("buildIssueDetail(MT-300) = nil, want non-nil")
		}
		if got.Workspace != nil {
			t.Errorf("Workspace = %v, want nil when WorkspacePath is empty", got.Workspace)
		}
	})

	t.Run("retrying issue without error", func(t *testing.T) {
		t.Parallel()
		noErrSnap := orchestrator.RuntimeSnapshotResult{
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "id-4", Identifier: "MT-400", Attempt: 1, Error: ""},
			},
		}
		got := buildIssueDetail("MT-400", noErrSnap)
		if got == nil {
			t.Fatal("buildIssueDetail(MT-400) = nil, want non-nil")
		}
		if got.LastError != nil {
			t.Errorf("LastError = %v, want nil when Error is empty", got.LastError)
		}
	})
}

// --- HTTP endpoint tests ---

func TestHandleState(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("success with populated snapshot", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "id-1", Identifier: "MT-1", StartedAt: fixedTime},
			},
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "id-2", Identifier: "MT-2", DueAtMS: fixedTime.UnixMilli()},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{
				InputTokens:    100,
				OutputTokens:   50,
				TotalTokens:    150,
				SecondsRunning: 60.0,
			},
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
		}

		var body stateResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		_ = resp.Body.Close()

		if body.Counts.Running != 1 {
			t.Errorf("Counts.Running = %d, want 1", body.Counts.Running)
		}
		if body.Counts.Retrying != 1 {
			t.Errorf("Counts.Retrying = %d, want 1", body.Counts.Retrying)
		}
		if body.AgentTotals.TotalTokens != 150 {
			t.Errorf("AgentTotals.TotalTokens = %d, want 150", body.AgentTotals.TotalTokens)
		}
	})

	t.Run("snapshot error returns 503 with generic message", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, failingSnapshot("connection refused"), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
		}

		var body errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if body.Error.Code != "snapshot_unavailable" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "snapshot_unavailable")
		}

		wantMsg := "orchestrator state snapshot unavailable"
		if body.Error.Message != wantMsg {
			t.Errorf("error message = %q, want %q", body.Error.Message, wantMsg)
		}
	})

	t.Run("empty state returns non-null JSON arrays", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Running:     nil,
			Retrying:    nil,
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// Decode as raw JSON to check for null vs [].
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if string(rawMap["running"]) == "null" {
			t.Error("running = null in JSON, want []")
		}
		if string(rawMap["retrying"]) == "null" {
			t.Error("retrying = null in JSON, want []")
		}
		if string(rawMap["rate_limits"]) == "null" {
			t.Error("rate_limits = null in JSON, want {}")
		}
	})
}

func TestHandleIssueDetail(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:       "id-1",
				Identifier:    "MT-100",
				State:         "In Progress",
				StartedAt:     fixedTime,
				WorkspacePath: "/tmp/ws/mt-100",
			},
		},
	}

	t.Run("found running issue", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/MT-100")
		if err != nil {
			t.Fatalf("GET /api/v1/MT-100: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		body := decodeJSON[issueDetailResponse](t, resp)
		if body.IssueIdentifier != "MT-100" {
			t.Errorf("IssueIdentifier = %q, want %q", body.IssueIdentifier, "MT-100")
		}
		if body.Status != "running" {
			t.Errorf("Status = %q, want %q", body.Status, "running")
		}
	})

	t.Run("not found returns 404", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/NONEXISTENT")
		if err != nil {
			t.Fatalf("GET /api/v1/NONEXISTENT: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}

		var body errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if body.Error.Code != "issue_not_found" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "issue_not_found")
		}
	})

	t.Run("snapshot error returns 503 with generic message", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, failingSnapshot("connection refused"), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/MT-100")
		if err != nil {
			t.Fatalf("GET /api/v1/MT-100: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
		}

		var body errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode error response: %v", err)
		}

		wantMsg := "orchestrator state snapshot unavailable"
		if body.Error.Message != wantMsg {
			t.Errorf("error message = %q, want %q", body.Error.Message, wantMsg)
		}
	})
}

func TestHandleRefresh(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), acceptingRefresh())
		resp, err := http.Post(ts.URL+"/api/v1/refresh", "", nil)
		if err != nil {
			t.Fatalf("POST /api/v1/refresh: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
		}

		body := decodeJSON[refreshResponse](t, resp)
		if !body.Queued {
			t.Error("Queued = false, want true")
		}
		if body.Coalesced {
			t.Error("Coalesced = true, want false when accepted")
		}
		if body.Operations == nil {
			t.Fatal("Operations = nil, want non-nil")
		}
		if len(body.Operations) != 2 {
			t.Errorf("len(Operations) = %d, want 2", len(body.Operations))
		}
	})

	t.Run("coalesced", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), coalescingRefresh())
		resp, err := http.Post(ts.URL+"/api/v1/refresh", "", nil)
		if err != nil {
			t.Fatalf("POST /api/v1/refresh: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body := decodeJSON[refreshResponse](t, resp)
		if !body.Coalesced {
			t.Error("Coalesced = false, want true when coalesced")
		}
		if !body.Queued {
			t.Error("Queued = false, want true even when coalesced")
		}
	})
}

// --- Method enforcement tests ---

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), acceptingRefresh())

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "POST /api/v1/state", method: http.MethodPost, path: "/api/v1/state"},
		{name: "PUT /api/v1/state", method: http.MethodPut, path: "/api/v1/state"},
		{name: "DELETE /api/v1/state", method: http.MethodDelete, path: "/api/v1/state"},
		{name: "GET /api/v1/refresh", method: http.MethodGet, path: "/api/v1/refresh"},
		{name: "PUT /api/v1/refresh", method: http.MethodPut, path: "/api/v1/refresh"},
		{name: "POST /api/v1/MT-100", method: http.MethodPost, path: "/api/v1/MT-100"},
		{name: "DELETE /api/v1/MT-100", method: http.MethodDelete, path: "/api/v1/MT-100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(tt.method, ts.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tt.method, tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
			}

			var body errorResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Code != "method_not_allowed" {
				t.Errorf("error code = %q, want %q", body.Error.Code, "method_not_allowed")
			}
		})
	}
}

// --- JSON encoding tests ---

func TestStateResponseJSON(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	resp := stateResponse{
		GeneratedAt: fixedTime,
		Counts:      stateCounts{Running: 1, Retrying: 0},
		Running: []runningEntryResponse{
			{
				IssueID:         "id-1",
				IssueIdentifier: "MT-1",
				State:           "In Progress",
				StartedAt:       fixedTime,
				LastEventAt:     fixedTime,
				Tokens:          tokenInfo{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			},
		},
		Retrying:    []retryEntryResponse{},
		AgentTotals: orchestrator.SnapshotAgentTotals{},
		RateLimits:  map[string]any{},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify required top-level keys exist.
	for _, key := range []string{"generated_at", "counts", "running", "retrying", "agent_totals", "rate_limits"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}

// TestWriteJSONMarshalFailure verifies that writeJSON returns a complete
// error envelope (not a partial body) when JSON encoding fails.
func TestWriteJSONMarshalFailure(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	logger := slog.New(slog.DiscardHandler)

	// math.NaN is not representable in JSON — forces an encoding error.
	writeJSON(rec, logger, http.StatusOK, math.NaN())

	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck // test code

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var envelope errorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body: %s)", err, body)
	}
	if envelope.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, "internal_error")
	}
}
