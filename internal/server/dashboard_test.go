package server

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
)

func dashboardServer(t *testing.T, snapFn SnapshotFunc, version string, slotFunc SlotFunc) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn: snapFn,
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		Version:    version,
		StartedAt:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		SlotFunc:   slotFunc,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func dashboardSnapshot() orchestrator.RuntimeSnapshotResult {
	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	return orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:          "id-651",
				Identifier:       "MT-651",
				State:            "In Progress",
				TurnCount:        3,
				LastAgentEvent:   domain.EventTurnCompleted,
				StartedAt:        now.Add(-5 * time.Minute),
				AgentTotalTokens: 1200,
				UsageMeasured:    true,
			},
			{
				IssueID:          "id-649",
				Identifier:       "MT-649",
				State:            "In Progress",
				TurnCount:        7,
				LastAgentEvent:   domain.EventNotification,
				StartedAt:        now.Add(-12 * time.Minute),
				AgentTotalTokens: 2000,
				UsageMeasured:    true,
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				IssueID:    "id-650",
				Identifier: "MT-650",
				Attempt:    3,
				DueAtMS:    now.Add(45 * time.Second).UnixMilli(),
				Error:      "no available orchestrator slots",
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{
			InputTokens:    5000,
			OutputTokens:   2400,
			TotalTokens:    7400,
			SecondsRunning: 11565, // 3h 12m 45s
		},
	}
}

type dashboardResponse struct {
	Body       string
	StatusCode int
	Header     http.Header
}

func getDashboard(t *testing.T, ts *httptest.Server, path string) dashboardResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + path) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test code
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return dashboardResponse{Body: string(b), StatusCode: resp.StatusCode, Header: resp.Header}
}

func TestHandleDashboard_OK(t *testing.T) {
	t.Parallel()

	snap := dashboardSnapshot()
	ts := dashboardServer(t,
		fixedSnapshot(snap),
		"0.1.0",
		func() int { return 5 },
	)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	ct := dr.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}

	for _, want := range []string{
		"MT-649",
		"MT-651",
		"MT-650",
		"0.1.0",
		"7,400",
		"1,200",
		"2,000",
		"5,000",
		"2,400",
		"In Progress",
		"turn_completed",
		"no available orchestrator slots",
		"accordion-header",
		"row-detail",
		`aria-expanded="false"`,
		"expand-indicator",
	} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleDashboard_SnapshotError(t *testing.T) {
	t.Parallel()

	ts := dashboardServer(t,
		failingSnapshot("snapshot kaboom"),
		"",
		nil,
	)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusServiceUnavailable)
	}
	if !strings.Contains(dr.Body, "unavailable") {
		t.Errorf("body missing 'unavailable': %s", dr.Body)
	}
}

func TestHandleDashboard_EmptyState(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
		AgentTotals: orchestrator.SnapshotAgentTotals{},
	}
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	if !strings.Contains(dr.Body, "No running sessions") {
		t.Error("body missing 'No running sessions'")
	}
	if !strings.Contains(dr.Body, "No retries pending") {
		t.Error("body missing 'No retries pending'")
	}
}

func TestHandleDashboard_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Now().UTC(),
	}
	ts := dashboardServer(t, fixedSnapshot(snap), "", nil)

	resp, err := http.Post(ts.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test code

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST / status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestDashboard_ExactRootPathOnly(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Now().UTC(),
	}
	ts := dashboardServer(t, fixedSnapshot(snap), "", nil)

	dr := getDashboard(t, ts, "/nonexistent")

	if dr.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nonexistent status = %d, want %d", dr.StatusCode, http.StatusNotFound)
	}
	if strings.Contains(dr.Body, "Sortie Dashboard") {
		t.Error("GET /nonexistent returned dashboard HTML, want 404")
	}
}

func TestBuildDashboardData(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				Identifier:       "MT-651",
				State:            "In Progress",
				TurnCount:        3,
				LastAgentEvent:   domain.EventTurnCompleted,
				StartedAt:        now.Add(-5*time.Minute - 12*time.Second),
				AgentTotalTokens: 1200,
			},
			{
				Identifier:       "MT-649",
				State:            "In Progress",
				TurnCount:        7,
				LastAgentEvent:   domain.EventNotification,
				StartedAt:        now.Add(-12*time.Minute - 34*time.Second),
				AgentTotalTokens: 2000,
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				Identifier: "MT-653",
				Attempt:    1,
				DueAtMS:    now.Add(2 * time.Minute).UnixMilli(),
				Error:      "later",
			},
			{
				Identifier: "MT-650",
				Attempt:    3,
				DueAtMS:    now.Add(45 * time.Second).UnixMilli(),
				Error:      "no slots",
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{
			InputTokens:    5000,
			OutputTokens:   2400,
			TotalTokens:    7400,
			SecondsRunning: 3600,
		},
	}

	startedAt := now.Add(-2*time.Hour - 15*time.Minute - 30*time.Second)

	t.Run("full snapshot", func(t *testing.T) {
		t.Parallel()

		data := buildDashboardData(snap, "0.2.0", startedAt, func() int { return 5 }, now, nil)

		if data.Version != "0.2.0" {
			t.Errorf("Version = %q, want %q", data.Version, "0.2.0")
		}
		if data.Uptime != "2h 15m 30s" {
			t.Errorf("Uptime = %q, want %q", data.Uptime, "2h 15m 30s")
		}
		if data.RunningCount != 2 {
			t.Errorf("RunningCount = %d, want %d", data.RunningCount, 2)
		}
		if data.RetryingCount != 2 {
			t.Errorf("RetryingCount = %d, want %d", data.RetryingCount, 2)
		}
		if data.AvailableSlots != 3 {
			t.Errorf("AvailableSlots = %d, want %d", data.AvailableSlots, 3)
		}
		if data.TotalTokens != 7400 {
			t.Errorf("TotalTokens = %d, want %d", data.TotalTokens, 7400)
		}
		if data.InputTokens != 5000 {
			t.Errorf("InputTokens = %d, want %d", data.InputTokens, 5000)
		}
		if data.OutputTokens != 2400 {
			t.Errorf("OutputTokens = %d, want %d", data.OutputTokens, 2400)
		}

		// Running sorted by StartedAt ascending (MT-649 started earlier).
		if len(data.Running) != 2 {
			t.Fatalf("len(Running) = %d, want 2", len(data.Running))
		}
		if data.Running[0].Identifier != "MT-649" {
			t.Errorf("Running[0].Identifier = %q, want %q", data.Running[0].Identifier, "MT-649")
		}
		if data.Running[1].Identifier != "MT-651" {
			t.Errorf("Running[1].Identifier = %q, want %q", data.Running[1].Identifier, "MT-651")
		}
		if data.Running[0].Duration != "12m 34s" {
			t.Errorf("Running[0].Duration = %q, want %q", data.Running[0].Duration, "12m 34s")
		}
		if data.Running[0].DetailURL != "/api/v1/MT-649" {
			t.Errorf("Running[0].DetailURL = %q, want %q", data.Running[0].DetailURL, "/api/v1/MT-649")
		}

		// Retrying sorted by DueAtMS ascending (MT-650 due sooner).
		if len(data.Retrying) != 2 {
			t.Fatalf("len(Retrying) = %d, want 2", len(data.Retrying))
		}
		if data.Retrying[0].Identifier != "MT-650" {
			t.Errorf("Retrying[0].Identifier = %q, want %q", data.Retrying[0].Identifier, "MT-650")
		}
		if data.Retrying[1].Identifier != "MT-653" {
			t.Errorf("Retrying[1].Identifier = %q, want %q", data.Retrying[1].Identifier, "MT-653")
		}
		if data.Retrying[0].DueIn != "in 45s" {
			t.Errorf("Retrying[0].DueIn = %q, want %q", data.Retrying[0].DueIn, "in 45s")
		}
	})

	t.Run("empty version defaults to dev", func(t *testing.T) {
		t.Parallel()

		data := buildDashboardData(snap, "", startedAt, nil, now, nil)
		if data.Version != "dev" {
			t.Errorf("Version = %q, want %q", data.Version, "dev")
		}
	})

	t.Run("nil slotFunc yields zero available", func(t *testing.T) {
		t.Parallel()

		data := buildDashboardData(snap, "v1", startedAt, nil, now, nil)
		if data.AvailableSlots != 0 {
			t.Errorf("AvailableSlots = %d, want 0", data.AvailableSlots)
		}
	})

	t.Run("available slots clamped to zero", func(t *testing.T) {
		t.Parallel()

		// slotFunc returns 1 but 2 running -> clamped to 0.
		data := buildDashboardData(snap, "v1", startedAt, func() int { return 1 }, now, nil)
		if data.AvailableSlots != 0 {
			t.Errorf("AvailableSlots = %d, want 0", data.AvailableSlots)
		}
	})

	t.Run("timing percentages formatted as string", func(t *testing.T) {
		t.Parallel()

		timingSnap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier: "MT-T1",
					State:      "In Progress",
					StartedAt:  now.Add(-100 * time.Second), // 100s = 100000ms
					ToolTimeMs: 12300,                       // 12.3%
					APITimeMs:  45600,                       // 45.6%
				},
			},
		}

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now, nil)

		if len(data.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(data.Running))
		}
		if data.Running[0].ToolTimePct != "12.3%" {
			t.Errorf("ToolTimePct = %q, want %q", data.Running[0].ToolTimePct, "12.3%")
		}
		if data.Running[0].APITimePct != "45.6%" {
			t.Errorf("APITimePct = %q, want %q", data.Running[0].APITimePct, "45.6%")
		}
	})

	t.Run("timing N/A when zero values", func(t *testing.T) {
		t.Parallel()

		timingSnap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier: "MT-NAT",
					State:      "In Progress",
					StartedAt:  now.Add(-60 * time.Second),
					ToolTimeMs: 0,
					APITimeMs:  0,
				},
			},
		}

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now, nil)

		if data.Running[0].ToolTimePct != "N/A" {
			t.Errorf("ToolTimePct = %q, want %q", data.Running[0].ToolTimePct, "N/A")
		}
		if data.Running[0].APITimePct != "N/A" {
			t.Errorf("APITimePct = %q, want %q", data.Running[0].APITimePct, "N/A")
		}
	})

	t.Run("timing N/A when zero elapsed", func(t *testing.T) {
		t.Parallel()

		timingSnap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier: "MT-ZE",
					State:      "In Progress",
					StartedAt:  now, // zero elapsed
					ToolTimeMs: 500,
					APITimeMs:  1000,
				},
			},
		}

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now, nil)

		if data.Running[0].ToolTimePct != "N/A" {
			t.Errorf("ToolTimePct = %q, want %q (zero elapsed)", data.Running[0].ToolTimePct, "N/A")
		}
		if data.Running[0].APITimePct != "N/A" {
			t.Errorf("APITimePct = %q, want %q (zero elapsed)", data.Running[0].APITimePct, "N/A")
		}
	})

	t.Run("timing N/A when StartedAt is zero value", func(t *testing.T) {
		t.Parallel()

		timingSnap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier: "MT-ZERO-START",
					State:      "In Progress",
					StartedAt:  time.Time{}, // zero value
					ToolTimeMs: 5000,
					APITimeMs:  10000,
				},
			},
		}

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now, nil)

		if data.Running[0].ToolTimePct != "N/A" {
			t.Errorf("ToolTimePct = %q, want %q (zero StartedAt)", data.Running[0].ToolTimePct, "N/A")
		}
		if data.Running[0].APITimePct != "N/A" {
			t.Errorf("APITimePct = %q, want %q (zero StartedAt)", data.Running[0].APITimePct, "N/A")
		}
	})

	t.Run("mid-turn shows TurnCount 1", func(t *testing.T) {
		t.Parallel()

		midTurnSnap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:     "MT-MIDTURN",
					State:          "In Progress",
					TurnCount:      1,
					LastAgentEvent: domain.EventNotification,
					StartedAt:      now.Add(-30 * time.Second),
				},
			},
		}

		data := buildDashboardData(midTurnSnap, "v1", startedAt, nil, now, nil)

		if len(data.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(data.Running))
		}
		if data.Running[0].TurnCount != 1 {
			t.Errorf("TurnCount = %d, want 1 (mid-turn must show started turn)", data.Running[0].TurnCount)
		}
		if data.Running[0].LastEvent != string(domain.EventNotification) {
			t.Errorf("LastEvent = %q, want %q", data.Running[0].LastEvent, domain.EventNotification)
		}
	})
}

func TestBuildDashboardData_Budget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("session axis renders the qualified identifier, humanized reason, and session cell", func(t *testing.T) {
		t.Parallel()

		usedTokens := int64(0)
		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{
					IssueID:        "id-budget-1",
					Identifier:     "MT-649",
					DisplayID:      "org/repo#649",
					Reason:         "session_budget",
					UsedSessions:   3,
					BudgetSessions: 3,
					UsedTokens:     &usedTokens,
					ExhaustedAt:    now.Add(-90 * time.Second),
				},
			},
		}

		data := buildDashboardData(snap, "v1", now, nil, now, nil)

		if data.BudgetExhaustedCount != 1 {
			t.Errorf("BudgetExhaustedCount = %d, want 1", data.BudgetExhaustedCount)
		}
		if len(data.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(data.BudgetExhausted))
		}
		row := data.BudgetExhausted[0]
		if row.Identifier != "org/repo#649" {
			t.Errorf("Identifier = %q, want %q (DisplayID preferred)", row.Identifier, "org/repo#649")
		}
		if row.Reason != "Session budget" {
			t.Errorf("Reason = %q, want %q (humanized)", row.Reason, "Session budget")
		}
		if row.Used != "3 of 3" {
			t.Errorf("Used = %q, want %q (session pair)", row.Used, "3 of 3")
		}
		if row.DetailURL != "/api/v1/MT-649" {
			t.Errorf("DetailURL = %q, want %q", row.DetailURL, "/api/v1/MT-649")
		}
	})

	t.Run("token axis renders the token cell", func(t *testing.T) {
		t.Parallel()

		usedTokens := int64(1500)
		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{
					IssueID:      "id-budget-2",
					Identifier:   "MT-650",
					Reason:       "token_budget",
					UsedTokens:   &usedTokens,
					BudgetTokens: 1000,
					ExhaustedAt:  now,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now, nil, now, nil)

		if len(data.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(data.BudgetExhausted))
		}
		if got := data.BudgetExhausted[0].Used; got != "1,500 of 1,000" {
			t.Errorf("Used = %q, want %q (token pair)", got, "1,500 of 1,000")
		}
	})

	t.Run("falls back to the issue ID when the identifier is empty", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{IssueID: "id-noident", Reason: "session_budget", ExhaustedAt: now},
			},
		}

		data := buildDashboardData(snap, "v1", now, nil, now, nil)

		if len(data.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(data.BudgetExhausted))
		}
		if got := data.BudgetExhausted[0].Identifier; got != "id-noident" {
			t.Errorf("Identifier = %q, want %q (issue-ID fallback)", got, "id-noident")
		}
	})

	t.Run("an unmapped reason falls back to its raw value", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{IssueID: "id-future", Identifier: "MT-1", Reason: "future_budget", ExhaustedAt: now},
			},
		}

		data := buildDashboardData(snap, "v1", now, nil, now, nil)

		if len(data.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(data.BudgetExhausted))
		}
		if got := data.BudgetExhausted[0].Reason; got != "future_budget" {
			t.Errorf("Reason = %q, want %q (raw fallback for an unmapped reason)", got, "future_budget")
		}
	})

	t.Run("empty set produces a zero count and no rows", func(t *testing.T) {
		t.Parallel()

		data := buildDashboardData(orchestrator.RuntimeSnapshotResult{GeneratedAt: now}, "v1", now, nil, now, nil)

		if data.BudgetExhaustedCount != 0 {
			t.Errorf("BudgetExhaustedCount = %d, want 0", data.BudgetExhaustedCount)
		}
		if len(data.BudgetExhausted) != 0 {
			t.Errorf("len(BudgetExhausted) = %d, want 0", len(data.BudgetExhausted))
		}
	})
}

// TestHandleDashboard_Budget covers the HTML surface: the card and table
// render only when something is blocked, and no rendered string names a
// setting to change.
func TestHandleDashboard_Budget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("card and table render when the set is non-empty", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{
					IssueID:        "id-budget-3",
					Identifier:     "MT-651",
					Reason:         "session_budget",
					UsedSessions:   3,
					BudgetSessions: 3,
					ExhaustedAt:    now.Add(-90 * time.Second),
				},
			},
		}

		ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
		dr := getDashboard(t, ts, "/")

		if dr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
		}
		for _, want := range []string{"MT-651", "Session budget", "3 of 3"} {
			if !strings.Contains(dr.Body, want) {
				t.Errorf("body missing %q", want)
			}
		}
	})

	t.Run("no card or table rendered when the set is empty", func(t *testing.T) {
		t.Parallel()

		ts := dashboardServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{GeneratedAt: now}), "1.0.0", nil)
		dr := getDashboard(t, ts, "/")

		if dr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
		}
		if strings.Contains(dr.Body, "Budget Blocked") {
			t.Error("body contains a budget-blocked card/table with nothing blocked")
		}
	})

	t.Run("no rendered string names a setting to change", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{
					IssueID:        "id-budget-4",
					Identifier:     "MT-652",
					Reason:         "session_budget",
					UsedSessions:   3,
					BudgetSessions: 3,
					ExhaustedAt:    now,
				},
			},
		}

		ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
		dr := getDashboard(t, ts, "/")

		if dr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
		}
		for _, forbidden := range []string{"max_sessions", "max_tokens", "max_consecutive_absences"} {
			if strings.Contains(dr.Body, forbidden) {
				t.Errorf("body contains %q, want absent", forbidden)
			}
		}
	})
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"negative", -5 * time.Second, "0s"},
		{"45 seconds", 45 * time.Second, "45s"},
		{"12m 34s", 12*time.Minute + 34*time.Second, "12m 34s"},
		{"2h 15m 30s", 2*time.Hour + 15*time.Minute + 30*time.Second, "2h 15m 30s"},
		{"1d 3h 12m", 27*time.Hour + 12*time.Minute + 45*time.Second, "1d 3h 12m"},
		{"exact 1 hour", 1 * time.Hour, "1h 0m 0s"},
		{"exact 1 day", 24 * time.Hour, "1d 0h 0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FormatDuration(tt.d)
			if got != tt.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestFormatRelativeTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		dueAtMS int64
		want    string
	}{
		{"future 45s", now.Add(45 * time.Second).UnixMilli(), "in 45s"},
		{"future 2m 10s", now.Add(2*time.Minute + 10*time.Second).UnixMilli(), "in 2m 10s"},
		{"past 30s", now.Add(-30 * time.Second).UnixMilli(), "overdue"},
		{"past 5m", now.Add(-5 * time.Minute).UnixMilli(), "overdue"},
		{"near zero within 1s", now.Add(-500 * time.Millisecond).UnixMilli(), "now"},
		{"exact now", now.UnixMilli(), "now"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatRelativeTime(tt.dueAtMS, now)
			if got != tt.want {
				t.Errorf("formatRelativeTime(%d, now) = %q, want %q", tt.dueAtMS, got, tt.want)
			}
		})
	}
}

func TestFormatInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input int64
		want  string
	}{
		{"zero", 0, "0"},
		{"small", 999, "999"},
		{"one thousand", 1000, "1,000"},
		{"large", 1234567, "1,234,567"},
		{"negative", -1234, "-1,234"},
		{"negative large", -1234567, "-1,234,567"},
		{"exact boundary", 1000000, "1,000,000"},
		{"single digit", 7, "7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FormatInt(tt.input)
			if got != tt.want {
				t.Errorf("FormatInt(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDashboard_MetaRefresh(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Now().UTC(),
	}
	ts := dashboardServer(t, fixedSnapshot(snap), "", nil)

	dr := getDashboard(t, ts, "/")

	if !strings.Contains(dr.Body, `<meta http-equiv="refresh" content="5"`) {
		t.Error("body missing meta refresh tag")
	}
}

func TestDashboard_NoExternalResources(t *testing.T) {
	t.Parallel()

	snap := dashboardSnapshot()
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", func() int { return 5 })

	dr := getDashboard(t, ts, "/")

	for _, pattern := range []string{`src="http`, `href="http`} {
		if strings.Contains(dr.Body, pattern) {
			t.Errorf("body contains external resource reference: %s", pattern)
		}
	}
}

func TestDashboard_HTMLEscaping(t *testing.T) {
	t.Parallel()

	xss := `<script>alert('xss')</script>`
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
		Running: []orchestrator.SnapshotRunningEntry{
			{
				Identifier:     xss,
				State:          "In Progress",
				StartedAt:      time.Date(2026, 3, 24, 11, 50, 0, 0, time.UTC),
				LastAgentEvent: domain.EventTurnCompleted,
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "", nil)

	dr := getDashboard(t, ts, "/")

	if strings.Contains(dr.Body, "<script>alert") {
		t.Error("body contains unescaped XSS payload — XSS vulnerability")
	}
	if !strings.Contains(dr.Body, "&lt;script&gt;") {
		t.Error("body missing HTML-escaped script tag")
	}
}

func TestHandleDashboard_SSHHostColumn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:    "id-1",
				Identifier: "SSH-1",
				State:      "In Progress",
				StartedAt:  now.Add(-5 * time.Minute),
				SSHHost:    "worker-a",
			},
			{
				IssueID:    "id-2",
				Identifier: "SSH-2",
				State:      "In Progress",
				StartedAt:  now.Add(-3 * time.Minute),
				SSHHost:    "worker-b",
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "0.1.0", func() int { return 4 })
	dr := getDashboard(t, ts, "/")

	for _, want := range []string{"worker-a", "worker-b", "SSH-1", "SSH-2"} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleDashboard_NoSSHHostColumn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:    "id-1",
				Identifier: "LOCAL-1",
				State:      "In Progress",
				StartedAt:  now.Add(-5 * time.Minute),
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "0.1.0", func() int { return 4 })
	dr := getDashboard(t, ts, "/")

	if !strings.Contains(dr.Body, "LOCAL-1") {
		t.Error("body missing LOCAL-1")
	}
}

// TestBuildDashboardData_ExtendedFields verifies CacheReadTokens, ModelName,
// and API request count pass through to the template data. The request count
// reaches the template only through its pre-formatted row.
func TestBuildDashboardData_ExtendedFields(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				Identifier:       "MT-EXT",
				State:            "In Progress",
				TurnCount:        5,
				LastAgentEvent:   domain.EventTurnCompleted,
				StartedAt:        now.Add(-10 * time.Minute),
				AgentTotalTokens: 3000,
				CacheReadTokens:  8000,
				ModelName:        "claude-sonnet-4-20250514",
				APIRequestCount:  12,

				UsageArrival:        registry.UsageArrivalIncremental,
				APIRequestsMeasured: true,
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{
			InputTokens:     5000,
			OutputTokens:    2400,
			TotalTokens:     7400,
			CacheReadTokens: 15000,
			SecondsRunning:  3600,
		},
	}

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), func() int { return 5 }, now, nil)

	if len(data.Running) != 1 {
		t.Fatalf("len(Running) = %d, want 1", len(data.Running))
	}
	entry := data.Running[0]
	if entry.CacheReadTokens != 8000 {
		t.Errorf("Running[0].CacheReadTokens = %d, want 8000", entry.CacheReadTokens)
	}
	if entry.ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("Running[0].ModelName = %q, want %q", entry.ModelName, "claude-sonnet-4-20250514")
	}
	if entry.APIRequestsRow != "12" {
		t.Errorf("Running[0].APIRequestsRow = %q, want %q", entry.APIRequestsRow, "12")
	}
	if data.CacheReadTokens != 15000 {
		t.Errorf("CacheReadTokens = %d, want 15000", data.CacheReadTokens)
	}
}

func TestHandleDashboard_ExtendedFieldsRendered(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:          "id-ext",
				Identifier:       "MT-EXT-DASH",
				State:            "In Progress",
				StartedAt:        now.Add(-5 * time.Minute),
				AgentTotalTokens: 4500,
				CacheReadTokens:  12345,
				ModelName:        "claude-sonnet-4-20250514",
				APIRequestCount:  9,
				UsageMeasured:    true,
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{
			CacheReadTokens: 25000,
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", func() int { return 5 })
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	for _, want := range []string{"MT-EXT-DASH", "12,345", "25,000"} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestMapRunHistoryEntries(t *testing.T) {
	t.Parallel()

	errMsg := "agent crashed"
	tests := []struct {
		name         string
		input        RunHistoryEntry
		wantWF       string
		wantDuration string
		wantError    string
		wantTurns    int
	}{
		{
			name: "non-empty workflow file passed through",
			input: RunHistoryEntry{
				Identifier:   "MT-1",
				Attempt:      1,
				Status:       "succeeded",
				WorkflowFile: "WORKFLOW.md",
				StartedAt:    "2026-03-24T10:00:00Z",
				CompletedAt:  "2026-03-24T10:00:30Z",
			},
			wantWF:       "WORKFLOW.md",
			wantDuration: "30s",
			wantError:    "",
			wantTurns:    0,
		},
		{
			name: "empty workflow file becomes em dash",
			input: RunHistoryEntry{
				Identifier:   "MT-2",
				Attempt:      1,
				Status:       "succeeded",
				WorkflowFile: "",
				StartedAt:    "2026-03-24T10:00:00Z",
				CompletedAt:  "2026-03-24T10:01:00Z",
			},
			wantWF:       "\u2014",
			wantDuration: "1m 0s",
			wantError:    "",
			wantTurns:    0,
		},
		{
			name: "non-nil error extracted",
			input: RunHistoryEntry{
				Identifier:   "MT-3",
				Attempt:      2,
				Status:       "failed",
				WorkflowFile: "backend.WORKFLOW.md",
				StartedAt:    "2026-03-24T10:00:00Z",
				CompletedAt:  "2026-03-24T10:02:00Z",
				Error:        &errMsg,
			},
			wantWF:       "backend.WORKFLOW.md",
			wantDuration: "2m 0s",
			wantError:    "agent crashed",
			wantTurns:    0,
		},
		{
			name: "invalid RFC3339 dates produce empty duration",
			input: RunHistoryEntry{
				Identifier:   "MT-4",
				Attempt:      1,
				Status:       "failed",
				WorkflowFile: "WORKFLOW.md",
				StartedAt:    "not-a-date",
				CompletedAt:  "also-not-a-date",
			},
			wantWF:       "WORKFLOW.md",
			wantDuration: "",
			wantError:    "",
			wantTurns:    0,
		},
		{
			name: "turns completed mapped from TurnsCompleted",
			input: RunHistoryEntry{
				Identifier:     "MT-5",
				Attempt:        1,
				Status:         "succeeded",
				WorkflowFile:   "WORKFLOW.md",
				StartedAt:      "2026-03-24T10:00:00Z",
				CompletedAt:    "2026-03-24T10:00:10Z",
				TurnsCompleted: 8,
			},
			wantWF:       "WORKFLOW.md",
			wantDuration: "10s",
			wantError:    "",
			wantTurns:    8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mapRunHistoryEntries([]RunHistoryEntry{tt.input})

			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			e := got[0]
			if e.WorkflowFile != tt.wantWF {
				t.Errorf("WorkflowFile = %q, want %q", e.WorkflowFile, tt.wantWF)
			}
			if e.Duration != tt.wantDuration {
				t.Errorf("Duration = %q, want %q", e.Duration, tt.wantDuration)
			}
			if e.Error != tt.wantError {
				t.Errorf("Error = %q, want %q", e.Error, tt.wantError)
			}
			if e.Turns != tt.wantTurns {
				t.Errorf("Turns = %d, want %d", e.Turns, tt.wantTurns)
			}
		})
	}
}

func TestHandleDashboard_RunHistory(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
	}

	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StartedAt:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		RunHistoryFn: func(_ context.Context, _ int) ([]RunHistoryEntry, error) {
			return []RunHistoryEntry{
				{
					Identifier:   "MT-100",
					Attempt:      1,
					Status:       "succeeded",
					WorkflowFile: "backend.WORKFLOW.md",
					StartedAt:    "2026-03-24T09:00:00Z",
					CompletedAt:  "2026-03-24T09:05:00Z",
				},
			}, nil
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	for _, want := range []string{"MT-100", "backend.WORKFLOW.md", "Run History", "Turns", "accordion-header", "row-detail"} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleDashboard_NoRunHistoryFn(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	if strings.Contains(dr.Body, "Run History") {
		t.Error("body contains 'Run History', want omitted when RunHistoryFn is nil")
	}
}

func TestBuildDashboardData_DisplayID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:    "id-9",
				Identifier: "9",
				DisplayID:  "owner/repo#9",
				State:      "In Progress",
				StartedAt:  now.Add(-1 * time.Minute),
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				IssueID:    "id-7",
				Identifier: "7",
				DisplayID:  "owner/repo#7",
				Attempt:    1,
				DueAtMS:    now.Add(1 * time.Minute).UnixMilli(),
				Error:      "timeout",
			},
		},
	}

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), nil, now, nil)

	if len(data.Running) != 1 {
		t.Fatalf("len(Running) = %d, want 1", len(data.Running))
	}
	if data.Running[0].Identifier != "owner/repo#9" {
		t.Errorf("Running[0].Identifier = %q, want %q", data.Running[0].Identifier, "owner/repo#9")
	}
	if len(data.Retrying) != 1 {
		t.Fatalf("len(Retrying) = %d, want 1", len(data.Retrying))
	}
	if data.Retrying[0].Identifier != "owner/repo#7" {
		t.Errorf("Retrying[0].Identifier = %q, want %q", data.Retrying[0].Identifier, "owner/repo#7")
	}
}

func TestBuildDashboardData_FallsBackToIdentifier(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:    "id-PROJ-42",
				Identifier: "PROJ-42",
				DisplayID:  "",
				State:      "In Progress",
				StartedAt:  now.Add(-1 * time.Minute),
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				IssueID:    "id-PROJ-43",
				Identifier: "PROJ-43",
				DisplayID:  "",
				Attempt:    2,
				DueAtMS:    now.Add(30 * time.Second).UnixMilli(),
				Error:      "no slots",
			},
		},
	}

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), nil, now, nil)

	if len(data.Running) != 1 {
		t.Fatalf("len(Running) = %d, want 1", len(data.Running))
	}
	if data.Running[0].Identifier != "PROJ-42" {
		t.Errorf("Running[0].Identifier = %q, want %q", data.Running[0].Identifier, "PROJ-42")
	}
	if len(data.Retrying) != 1 {
		t.Fatalf("len(Retrying) = %d, want 1", len(data.Retrying))
	}
	if data.Retrying[0].Identifier != "PROJ-43" {
		t.Errorf("Retrying[0].Identifier = %q, want %q", data.Retrying[0].Identifier, "PROJ-43")
	}
}

// TestHandleDashboard_FooterCacheReadLabel verifies the footer uses the
// "Cache Read:" label (not the ambiguous "Cache:") with an explaining
// tooltip.
func TestHandleDashboard_FooterCacheReadLabel(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
		AgentTotals: orchestrator.SnapshotAgentTotals{
			InputTokens:     734,
			OutputTokens:    280,
			CacheReadTokens: 2077449,
		},
	}
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	if !strings.Contains(dr.Body, "Cache Read:") {
		t.Error(`footer body missing "Cache Read:" label`)
	}

	wantTitle := `title="Prompt cache read tokens`
	if !strings.Contains(dr.Body, wantTitle) {
		t.Errorf("footer body missing tooltip attribute %q", wantTitle)
	}

	if !strings.Contains(dr.Body, "2,077,449") {
		t.Error(`footer body missing formatted cache read token count "2,077,449"`)
	}
}

func TestHandleDashboard_SessionsCachedTokensTooltip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name            string
		cacheReadTokens int64
		wantTooltip     bool
		wantFormatted   string
	}{
		{
			name:            "non-zero cache read tokens renders tooltip",
			cacheReadTokens: 763850,
			wantTooltip:     true,
			wantFormatted:   "763,850",
		},
		{
			name:            "zero cache read tokens omits annotation",
			cacheReadTokens: 0,
			wantTooltip:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snap := orchestrator.RuntimeSnapshotResult{
				GeneratedAt: now,
				Running: []orchestrator.SnapshotRunningEntry{
					{
						IssueID:          "id-1",
						Identifier:       "MT-1",
						State:            "In Progress",
						StartedAt:        now.Add(-5 * time.Minute),
						AgentTotalTokens: 1000,
						CacheReadTokens:  tt.cacheReadTokens,
						UsageMeasured:    true,
						UsageArrival:     registry.UsageArrivalIncremental,
						UsageAttribution: registry.UsageAttributionPerModel,
					},
				},
			}
			ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)

			dr := getDashboard(t, ts, "/")

			if dr.StatusCode != http.StatusOK {
				t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
			}

			wantAnnotation := "(" + FormatInt(tt.cacheReadTokens) + " cached)"
			gotTooltip := strings.Contains(dr.Body, wantAnnotation)
			if gotTooltip != tt.wantTooltip {
				t.Errorf("body contains cached-count annotation %q = %v, want %v", wantAnnotation, gotTooltip, tt.wantTooltip)
			}

			if tt.wantFormatted != "" && !strings.Contains(dr.Body, tt.wantFormatted) {
				t.Errorf("body missing formatted cache read count %q", tt.wantFormatted)
			}
		})
	}
}

func TestMapRunHistoryEntries_DisplayID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		identifier string
		displayID  string
		wantID     string
	}{
		{
			name:       "DisplayID set — used as Identifier",
			identifier: "42",
			displayID:  "owner/repo#42",
			wantID:     "owner/repo#42",
		},
		{
			name:       "DisplayID empty — falls back to Identifier",
			identifier: "PROJ-99",
			displayID:  "",
			wantID:     "PROJ-99",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runs := []RunHistoryEntry{
				{
					Identifier:  tt.identifier,
					DisplayID:   tt.displayID,
					Attempt:     1,
					Status:      "succeeded",
					StartedAt:   "2026-03-24T10:00:00Z",
					CompletedAt: "2026-03-24T10:05:00Z",
				},
			}

			got := mapRunHistoryEntries(runs)

			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			if got[0].Identifier != tt.wantID {
				t.Errorf("Identifier = %q, want %q", got[0].Identifier, tt.wantID)
			}
		})
	}
}

func TestEvenTemplateFunc(t *testing.T) {
	t.Parallel()

	tmpl := template.Must(template.New("test").Funcs(template.FuncMap{
		"even": func(i int) bool { return i%2 == 0 },
	}).Parse(`{{if even .}}yes{{else}}no{{end}}`))

	tests := []struct {
		name  string
		input int
		want  string
	}{
		{"zero_is_even", 0, "yes"},
		{"one_is_odd", 1, "no"},
		{"two_is_even", 2, "yes"},
		{"three_is_odd", 3, "no"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tt.input); err != nil {
				t.Fatalf("Execute(%d): %v", tt.input, err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("even(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDashboard_RowsCollapsedByDefault(t *testing.T) {
	t.Parallel()

	snap := dashboardSnapshot()
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", func() int { return 5 })
	dr := getDashboard(t, ts, "/")

	// Check for expanded rows in the rendered markup. The CSS and JS also
	// contain the literal string aria-expanded="true" (in selectors and
	// querySelector calls), so match the attribute in element context only.
	if strings.Contains(dr.Body, `aria-expanded="true"
`) || strings.Contains(dr.Body, `aria-expanded="true">`) {
		t.Error(`accordion header has aria-expanded="true" — all rows must start collapsed`)
	}
	if strings.Contains(dr.Body, `row-detail open"`) || strings.Contains(dr.Body, `row-detail open `) {
		t.Error("body contains open detail row — all rows must start collapsed")
	}
}

func TestDashboard_DetailPanelFields(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("running session fields in detail panel", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					IssueID:          "id-dp",
					Identifier:       "MT-DP",
					State:            "In Progress",
					StartedAt:        now.Add(-10 * time.Minute),
					AgentTotalTokens: 5000,
					ModelName:        "claude-sonnet-4-20250514",
					WorkflowFile:     "backend.WORKFLOW.md",
					ToolTimeMs:       12000,
					APITimeMs:        45000,
					UsageArrival:     registry.UsageArrivalIncremental,
					UsageAttribution: registry.UsageAttributionPerModel,
				},
			},
		}
		ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", func() int { return 5 })
		dr := getDashboard(t, ts, "/")

		if dr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
		}
		for _, want := range []string{"claude-sonnet-4-20250514", "backend.WORKFLOW.md"} {
			if !strings.Contains(dr.Body, want) {
				t.Errorf("body missing detail panel field %q", want)
			}
		}
	})

	t.Run("run history error appears in detail panel", func(t *testing.T) {
		t.Parallel()

		errMsg := "agent timed out"
		srv := New(Params{
			SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{GeneratedAt: now}),
			RefreshFn:  acceptingRefresh(),
			Logger:     slog.New(slog.DiscardHandler),
			StartedAt:  now.Add(-1 * time.Hour),
			RunHistoryFn: func(_ context.Context, _ int) ([]RunHistoryEntry, error) {
				return []RunHistoryEntry{
					{
						Identifier:  "MT-ERR",
						Attempt:     2,
						Status:      "failed",
						StartedAt:   "2026-03-24T09:00:00Z",
						CompletedAt: "2026-03-24T09:01:00Z",
						Error:       &errMsg,
					},
				}, nil
			},
		})
		ts := httptest.NewServer(srv.Mux())
		t.Cleanup(ts.Close)

		dr := getDashboard(t, ts, "/")
		if !strings.Contains(dr.Body, "agent timed out") {
			t.Error("body missing run history error message in detail panel")
		}
	})
}

func TestDashboard_StripingAlternates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-1", Identifier: "MT-S1", State: "In Progress", StartedAt: now.Add(-3 * time.Minute)},
			{IssueID: "id-2", Identifier: "MT-S2", State: "In Progress", StartedAt: now.Add(-2 * time.Minute)},
			{IssueID: "id-3", Identifier: "MT-S3", State: "In Progress", StartedAt: now.Add(-1 * time.Minute)},
		},
	}

	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StartedAt:  now.Add(-1 * time.Hour),
		RunHistoryFn: func(_ context.Context, _ int) ([]RunHistoryEntry, error) {
			return []RunHistoryEntry{
				{Identifier: "MT-H1", Attempt: 1, Status: "succeeded", StartedAt: "2026-03-24T08:00:00Z", CompletedAt: "2026-03-24T08:01:00Z"},
				{Identifier: "MT-H2", Attempt: 1, Status: "succeeded", StartedAt: "2026-03-24T08:01:00Z", CompletedAt: "2026-03-24T08:02:00Z"},
				{Identifier: "MT-H3", Attempt: 1, Status: "succeeded", StartedAt: "2026-03-24T08:02:00Z", CompletedAt: "2026-03-24T08:03:00Z"},
			}, nil
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	dr := getDashboard(t, ts, "/")

	if !strings.Contains(dr.Body, "row-even") {
		t.Error("body missing row-even class — striping not applied")
	}
	if strings.Contains(dr.Body, "tr:nth-child") {
		t.Error("body contains tr:nth-child — old CSS rule must be removed")
	}
}

func TestDashboard_RunningSessionsIdentifierLinks(t *testing.T) {
	t.Parallel()

	snap := dashboardSnapshot()
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", func() int { return 5 })
	dr := getDashboard(t, ts, "/")

	if !strings.Contains(dr.Body, "<a href=") {
		t.Error("body missing anchor tag — running session identifier links not rendered")
	}
}

func TestDashboard_DetailRowColspan(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-1", Identifier: "MT-CS1", State: "In Progress", StartedAt: now.Add(-1 * time.Minute)},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{IssueID: "id-2", Identifier: "MT-CS2", Attempt: 1, DueAtMS: now.Add(1 * time.Minute).UnixMilli(), Error: "timeout"},
		},
	}

	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StartedAt:  now.Add(-1 * time.Hour),
		RunHistoryFn: func(_ context.Context, _ int) ([]RunHistoryEntry, error) {
			return []RunHistoryEntry{
				{Identifier: "MT-CS-H1", Attempt: 1, Status: "succeeded", StartedAt: "2026-03-24T08:00:00Z", CompletedAt: "2026-03-24T08:01:00Z"},
			}, nil
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	dr := getDashboard(t, ts, "/")

	for _, want := range []string{`colspan="5"`, `colspan="3"`, `colspan="4"`} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q — detail row colspan incorrect", want)
		}
	}
}

func TestHandleDashboard_AccordionToggleRefactor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			// StartedAt ascending -> MT-R0 is index 0, MT-R1 is index 1 after sort.
			{IssueID: "id-r0", Identifier: "MT-R0", State: "In Progress", StartedAt: now.Add(-5 * time.Minute)},
			{IssueID: "id-r1", Identifier: "MT-R1", State: "In Progress", StartedAt: now.Add(-3 * time.Minute)},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{IssueID: "id-q0", Identifier: "MT-Q0", Attempt: 1, DueAtMS: now.Add(30 * time.Second).UnixMilli(), Error: "no slots"},
		},
	}

	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StartedAt:  now.Add(-1 * time.Hour),
		RunHistoryFn: func(_ context.Context, _ int) ([]RunHistoryEntry, error) {
			return []RunHistoryEntry{
				{Identifier: "MT-H0", Attempt: 1, Status: "succeeded", StartedAt: "2026-03-24T08:00:00Z", CompletedAt: "2026-03-24T08:01:00Z"},
			}, nil
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	dr := getDashboard(t, ts, "/")
	if dr.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	body := dr.Body

	t.Run("no role=button on tr", func(t *testing.T) {
		t.Parallel()
		if strings.Contains(body, `role="button"`) {
			t.Error(`body contains role="button" — must not appear after accordion toggle refactor`)
		}
	})

	t.Run("accordion-toggle button present in every accordion-header row", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(body, `class="accordion-toggle"`) {
			t.Error(`body missing class="accordion-toggle"`)
		}
		// 2 running + 1 retrying + 1 history = 4 accordion-toggle buttons.
		const wantCount = 4
		if got := strings.Count(body, `class="accordion-toggle"`); got != wantCount {
			t.Errorf("accordion-toggle class count = %d, want %d", got, wantCount)
		}
	})

	t.Run("aria-expanded=false is on button not on tr", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(body, `aria-expanded="false"`) {
			t.Error(`body missing aria-expanded="false"`)
		}
		// The refactor moved aria-expanded onto the nested button, so an
		// accordion-header row carrying it is a regression.
		if strings.Contains(body, `class="accordion-header" aria-expanded="`) ||
			strings.Contains(body, `aria-expanded="false" class="accordion-header"`) ||
			strings.Contains(body, `aria-expanded="true" class="accordion-header"`) {
			t.Error(`body contains aria-expanded on accordion-header tr — it must only appear on accordion-toggle button`)
		}
	})

	t.Run("aria-controls on button matches id of detail row", func(t *testing.T) {
		t.Parallel()
		pairs := [][2]string{
			{`aria-controls="detail-running-0"`, `id="detail-running-0"`},
			{`aria-controls="detail-running-1"`, `id="detail-running-1"`},
			{`aria-controls="detail-retry-0"`, `id="detail-retry-0"`},
			{`aria-controls="detail-history-0"`, `id="detail-history-0"`},
		}
		for _, pair := range pairs {
			if !strings.Contains(body, pair[0]) {
				t.Errorf("body missing button attribute %q", pair[0])
			}
			if !strings.Contains(body, pair[1]) {
				t.Errorf("body missing detail row attribute %q", pair[1])
			}
		}
	})

	t.Run("no tabindex=0 on tr", func(t *testing.T) {
		t.Parallel()
		// tabindex="0" was only ever on accordion <tr> rows.
		if strings.Contains(body, `tabindex="0"`) {
			t.Error(`body contains tabindex="0" — must not appear after accordion toggle refactor`)
		}
	})
}

func TestBuildDashboardData_TokenRates(t *testing.T) {
	t.Parallel()

	fptr := func(v float64) *float64 { return &v }
	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	rates := TokenRates{
		"claude": TokenRateConfig{
			InputPerMtok:  fptr(3.0),
			OutputPerMtok: fptr(15.0),
		},
	}

	t.Run("with rates and matching AgentKind computes aggregate cost", func(t *testing.T) {
		t.Parallel()

		// 2M input @ $3/Mtok = $6, 1M output @ $15/Mtok = $15 -> $21
		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:        "MT-1",
					State:             "In Progress",
					StartedAt:         now.Add(-5 * time.Minute),
					AgentKind:         "claude",
					AgentInputTokens:  2_000_000,
					AgentOutputTokens: 1_000_000,
					UsageMeasured:     true,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now.Add(-1*time.Hour), nil, now, rates)

		if !data.HasTokenRates {
			t.Error("HasTokenRates = false, want true")
		}
		if data.EstimatedCostUSD == nil {
			t.Fatal("EstimatedCostUSD = nil, want non-nil")
		}
		if *data.EstimatedCostUSD != "$21.00" {
			t.Errorf("EstimatedCostUSD = %q, want %q", *data.EstimatedCostUSD, "$21.00")
		}
		if data.Running[0].EstimatedCostUSD != "$21.00" {
			t.Errorf("Running[0].EstimatedCostUSD = %q, want %q", data.Running[0].EstimatedCostUSD, "$21.00")
		}
		if data.EstimatedCostLabel == "" {
			t.Error("EstimatedCostLabel is empty, want non-empty")
		}
	})

	t.Run("without token rates HasTokenRates is false", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:        "MT-2",
					State:             "In Progress",
					StartedAt:         now.Add(-3 * time.Minute),
					AgentKind:         "claude",
					AgentInputTokens:  1_000_000,
					AgentOutputTokens: 500_000,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now.Add(-1*time.Hour), nil, now, nil)

		if data.HasTokenRates {
			t.Error("HasTokenRates = true, want false (nil rates)")
		}
		if data.EstimatedCostUSD != nil {
			t.Errorf("EstimatedCostUSD = %q, want nil", *data.EstimatedCostUSD)
		}
	})

	t.Run("rates configured but no matching AgentKind — cost nil", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:        "MT-3",
					State:             "In Progress",
					StartedAt:         now.Add(-3 * time.Minute),
					AgentKind:         "unknown-adapter",
					AgentInputTokens:  1_000_000,
					AgentOutputTokens: 500_000,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now.Add(-1*time.Hour), nil, now, rates)

		if !data.HasTokenRates {
			t.Error("HasTokenRates = false, want true")
		}
		if data.EstimatedCostUSD != nil {
			t.Errorf("EstimatedCostUSD = %q, want nil (no matching kind)", *data.EstimatedCostUSD)
		}
		if data.Running[0].EstimatedCostUSD != "" {
			t.Errorf("Running[0].EstimatedCostUSD = %q, want empty", data.Running[0].EstimatedCostUSD)
		}
	})

	t.Run("zero tokens with rates shows $0.00", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:        "MT-4",
					State:             "In Progress",
					StartedAt:         now.Add(-1 * time.Minute),
					AgentKind:         "claude",
					AgentInputTokens:  0,
					AgentOutputTokens: 0,
					UsageMeasured:     true,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now.Add(-1*time.Hour), nil, now, rates)

		if !data.HasTokenRates {
			t.Error("HasTokenRates = false, want true")
		}
		if data.EstimatedCostUSD == nil {
			t.Fatal("EstimatedCostUSD = nil, want \"$0.00\"")
		}
		if *data.EstimatedCostUSD != "$0.00" {
			t.Errorf("EstimatedCostUSD = %q, want %q", *data.EstimatedCostUSD, "$0.00")
		}
	})

	t.Run("unmeasured entry contributes nothing to the aggregate cost", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:        "MT-MEASURED",
					State:             "In Progress",
					StartedAt:         now.Add(-5 * time.Minute),
					AgentKind:         "claude",
					AgentInputTokens:  1_000_000,
					AgentOutputTokens: 0,
					UsageMeasured:     true,
					UsageArrival:      registry.UsageArrivalIncremental,
					UsageAttribution:  registry.UsageAttributionPerModel,
				},
				{
					Identifier:        "MT-UNMEASURED",
					State:             "In Progress",
					StartedAt:         now.Add(-3 * time.Minute),
					AgentKind:         "claude",
					AgentInputTokens:  1_000_000,
					AgentOutputTokens: 0,
					UsageMeasured:     false,
					UsageArrival:      registry.UsageArrivalIncremental,
					UsageAttribution:  registry.UsageAttributionPerModel,
				},
			},
		}

		data := buildDashboardData(snap, "v1", now.Add(-1*time.Hour), nil, now, rates)

		// 1M input @ $3/Mtok = $3, from the measured entry alone.
		if data.EstimatedCostUSD == nil || *data.EstimatedCostUSD != "$3.00" {
			t.Errorf("EstimatedCostUSD = %v, want $3.00 (only the measured entry)", data.EstimatedCostUSD)
		}

		var measured, unmeasured dashboardRunningEntry
		for _, e := range data.Running {
			switch e.Identifier {
			case "MT-MEASURED":
				measured = e
			case "MT-UNMEASURED":
				unmeasured = e
			}
		}
		if measured.EstimatedCostUSD != "$3.00" {
			t.Errorf("measured entry EstimatedCostUSD = %q, want %q", measured.EstimatedCostUSD, "$3.00")
		}
		if unmeasured.EstimatedCostUSD != "" {
			t.Errorf("unmeasured entry EstimatedCostUSD = %q, want empty", unmeasured.EstimatedCostUSD)
		}
		if !measured.UsageMeasured {
			t.Error("measured entry UsageMeasured = false, want true")
		}
		if unmeasured.UsageMeasured {
			t.Error("unmeasured entry UsageMeasured = true, want false")
		}
	})

}

func TestHandleDashboard_WithTokenRates(t *testing.T) {
	t.Parallel()

	fptr := func(v float64) *float64 { return &v }
	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:           "id-cost-1",
				Identifier:        "MT-COST",
				State:             "In Progress",
				StartedAt:         now.Add(-5 * time.Minute),
				AgentKind:         "claude",
				AgentInputTokens:  1_000_000,
				AgentOutputTokens: 1_000_000,
				UsageMeasured:     true,
			},
		},
	}

	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StartedAt:  now.Add(-1 * time.Hour),
		TokenRates: TokenRates{
			"claude": TokenRateConfig{
				InputPerMtok:  fptr(3.0),
				OutputPerMtok: fptr(15.0),
			},
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	// Cost card, per-entry cost label, footer cost line, and disclaimer must all appear.
	for _, want := range []string{
		"Est. Cost",
		"$",
		"Cost estimates are based on configured token rates",
	} {
		if !strings.Contains(dr.Body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleDashboard_WithoutTokenRates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:           "id-nocost",
				Identifier:        "MT-NOCOST",
				State:             "In Progress",
				StartedAt:         now.Add(-3 * time.Minute),
				AgentKind:         "claude",
				AgentInputTokens:  500_000,
				AgentOutputTokens: 250_000,
			},
		},
	}

	// No TokenRates set -> the Est. Cost row still renders, per the
	// usageEstCostRow rates-unconfigured arm, but with the dash.
	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)

	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	if !strings.Contains(dr.Body, "<dt>Est. Cost</dt>") {
		t.Error("body missing the Est. Cost row; it must render with a dash when no token rates are configured")
	}
	if strings.Contains(dr.Body, "Cost estimates are based on configured token rates") {
		t.Error("body contains cost disclaimer but no token rates configured")
	}
}

// TestHandleDashboard_UnmeasuredRunningEntry verifies an unmeasured running
// session renders "not reported" in the Tokens cell, keeps the missing-value
// marker for Est. Cost, and gets a footer note naming the count.
func TestHandleDashboard_UnmeasuredRunningEntry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:          "id-unm",
				Identifier:       "MT-UNM",
				State:            "In Progress",
				StartedAt:        now.Add(-3 * time.Minute),
				UsageMeasured:    false,
				UsageArrival:     registry.UsageArrivalIncremental,
				UsageAttribution: registry.UsageAttributionPerModel,
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{RunningUnreported: 1},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	if !strings.Contains(dr.Body, "not reported") {
		t.Error("body missing \"not reported\" for an unmeasured running entry's Tokens cell")
	}
	if !strings.Contains(dr.Body, "1 running session has not reported token usage yet; the totals above exclude it.") {
		t.Errorf("body = %q, want the footer note naming the unmeasured count", dr.Body)
	}
}

func TestCountedNote(t *testing.T) {
	t.Parallel()

	const singular = "%d thing has happened."
	const plural = "%d things have happened."

	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero returns empty", 0, ""},
		{"negative returns empty", -1, ""},
		{"one uses singular form", 1, "1 thing has happened."},
		{"two uses plural form", 2, "2 things have happened."},
		{"large count uses plural form", 42, "42 things have happened."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := countedNote(tt.n, singular, plural)

			if got != tt.want {
				t.Errorf("countedNote(%d, ...) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestBuildDashboardData_ExclusionNotes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	fptr := func(v float64) *float64 { return &v }
	rates := TokenRates{"claude": TokenRateConfig{InputPerMtok: fptr(3.0)}}

	t.Run("singular forms", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:   "MT-UNREPORTED",
					StartedAt:    now.Add(-time.Minute),
					UsageArrival: registry.UsageArrivalIncremental,
				},
				{
					Identifier:   "MT-NOARRIVAL",
					StartedAt:    now.Add(-time.Minute),
					UsageArrival: registry.UsageArrivalNone,
				},
				{
					Identifier:       "MT-UNPRICED",
					StartedAt:        now.Add(-time.Minute),
					AgentKind:        "unpriced-kind",
					UsageMeasured:    true,
					AgentInputTokens: 1000,
					UsageArrival:     registry.UsageArrivalIncremental,
				},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{UnmeasuredSessions: 1, RunningUnreported: 1, RunningNonReporting: 1},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

		if data.RunningUnreportedNote != "1 running session has not reported token usage yet; the totals above exclude it." {
			t.Errorf("RunningUnreportedNote = %q", data.RunningUnreportedNote)
		}
		if data.RunningNonReportingNote != "1 running session runs an agent that reports no token usage; the totals above exclude it." {
			t.Errorf("RunningNonReportingNote = %q", data.RunningNonReportingNote)
		}
		if data.EndedUnmeasuredNote != "1 session that already ended never reported token usage; the totals above exclude it too." {
			t.Errorf("EndedUnmeasuredNote = %q", data.EndedUnmeasuredNote)
		}
		if data.CostUnpricedNote != "1 running session is excluded from Est. Cost because token_rates has no price for its agent." {
			t.Errorf("CostUnpricedNote = %q", data.CostUnpricedNote)
		}

		for _, note := range []string{
			data.RunningUnreportedNote, data.RunningNonReportingNote,
			data.EndedUnmeasuredNote, data.CostUnpricedNote,
		} {
			for _, forbidden := range []string{"aggregate_metrics", "unmeasured_sessions", "run_history", "SELECT", "sqlite"} {
				if strings.Contains(note, forbidden) {
					t.Errorf("note %q contains internal identifier %q", note, forbidden)
				}
			}
		}
	})

	t.Run("plural forms", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{Identifier: "MT-U1", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalIncremental},
				{Identifier: "MT-U2", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalTurnEnd},
				{Identifier: "MT-N1", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalNone},
				{Identifier: "MT-N2", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalNone},
				{
					Identifier: "MT-P1", StartedAt: now.Add(-time.Minute), AgentKind: "unpriced-kind",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
				{
					Identifier: "MT-P2", StartedAt: now.Add(-time.Minute), AgentKind: "other-unpriced",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{UnmeasuredSessions: 2, RunningUnreported: 2, RunningNonReporting: 2},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

		if data.RunningUnreportedNote != "2 running sessions have not reported token usage yet; the totals above exclude them." {
			t.Errorf("RunningUnreportedNote = %q", data.RunningUnreportedNote)
		}
		if data.RunningNonReportingNote != "2 running sessions run agents that report no token usage; the totals above exclude them." {
			t.Errorf("RunningNonReportingNote = %q", data.RunningNonReportingNote)
		}
		if data.EndedUnmeasuredNote != "2 sessions that already ended never reported token usage; the totals above exclude them too." {
			t.Errorf("EndedUnmeasuredNote = %q", data.EndedUnmeasuredNote)
		}
		if data.CostUnpricedNote != "2 running sessions are excluded from Est. Cost because token_rates has no price for their agents." {
			t.Errorf("CostUnpricedNote = %q", data.CostUnpricedNote)
		}
	})

	t.Run("all counts zero leaves every note empty", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier:       "MT-ALL-GOOD",
					StartedAt:        now.Add(-time.Minute),
					AgentKind:        "claude",
					UsageMeasured:    true,
					AgentInputTokens: 1000,
					UsageArrival:     registry.UsageArrivalIncremental,
				},
			},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

		if data.RunningUnreportedNote != "" {
			t.Errorf("RunningUnreportedNote = %q, want empty", data.RunningUnreportedNote)
		}
		if data.RunningNonReportingNote != "" {
			t.Errorf("RunningNonReportingNote = %q, want empty", data.RunningNonReportingNote)
		}
		if data.EndedUnmeasuredNote != "" {
			t.Errorf("EndedUnmeasuredNote = %q, want empty", data.EndedUnmeasuredNote)
		}
		if data.CostUnpricedNote != "" {
			t.Errorf("CostUnpricedNote = %q, want empty", data.CostUnpricedNote)
		}
	})

	t.Run("no token rates leaves the cost note empty", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{
					Identifier: "MT-NO-RATES", StartedAt: now.Add(-time.Minute), AgentKind: "claude",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
			},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, nil)

		if data.CostUnpricedNote != "" {
			t.Errorf("CostUnpricedNote = %q, want empty: Est. Cost is not shown without token rates", data.CostUnpricedNote)
		}
	})

	t.Run("no-usage-arrival session never contributes to the unreported note", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{Identifier: "MT-NOARR", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalNone},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{RunningNonReporting: 1},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

		if data.RunningUnreportedNote != "" {
			t.Errorf("RunningUnreportedNote = %q, want empty: an arrival-none session must not read as \"not reported yet\"", data.RunningUnreportedNote)
		}
		if data.RunningNonReportingNote == "" {
			t.Error("RunningNonReportingNote is empty, want the no-usage-arrival note")
		}
	})

	// Each reason gets a distinct count so a note sourced from the wrong
	// population wouldn't pass by accident.
	t.Run("each note counts only its own population, not a neighboring one", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running: []orchestrator.SnapshotRunningEntry{
				{Identifier: "MT-U1", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalIncremental},
				{
					Identifier: "MT-P1", StartedAt: now.Add(-time.Minute), AgentKind: "unpriced-kind",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
				{
					Identifier: "MT-P2", StartedAt: now.Add(-time.Minute), AgentKind: "other-unpriced",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
				{
					Identifier: "MT-P3", StartedAt: now.Add(-time.Minute), AgentKind: "third-unpriced",
					UsageMeasured: true, AgentInputTokens: 1000, UsageArrival: registry.UsageArrivalIncremental,
				},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{UnmeasuredSessions: 7, RunningUnreported: 1},
		}

		data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

		if data.RunningUnreportedNote != "1 running session has not reported token usage yet; the totals above exclude it." {
			t.Errorf("RunningUnreportedNote = %q, want the count of running-unreported sessions (1)", data.RunningUnreportedNote)
		}
		if data.RunningNonReportingNote != "" {
			t.Errorf("RunningNonReportingNote = %q, want empty (no arrival-none sessions present)", data.RunningNonReportingNote)
		}
		if data.EndedUnmeasuredNote != "7 sessions that already ended never reported token usage; the totals above exclude them too." {
			t.Errorf("EndedUnmeasuredNote = %q, want the persisted count (7), not the running count", data.EndedUnmeasuredNote)
		}
		if data.CostUnpricedNote != "3 running sessions are excluded from Est. Cost because token_rates has no price for their agents." {
			t.Errorf("CostUnpricedNote = %q, want the count of unpriced measured sessions (3)", data.CostUnpricedNote)
		}
	})
}

// TestHandleDashboard_NoUsageArrivalAndEndedUnmeasuredNotesRendered verifies
// the footer notes render through the actual template, not just the struct.
func TestHandleDashboard_NoUsageArrivalAndEndedUnmeasuredNotesRendered(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-noarr", Identifier: "MT-NOARR", StartedAt: now.Add(-time.Minute), UsageArrival: registry.UsageArrivalNone},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{UnmeasuredSessions: 1, RunningNonReporting: 1},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	if !strings.Contains(dr.Body, "1 running session runs an agent that reports no token usage; the totals above exclude it.") {
		t.Errorf("body missing the no-usage-arrival footer note; body = %q", dr.Body)
	}
	if !strings.Contains(dr.Body, "1 session that already ended never reported token usage; the totals above exclude it too.") {
		t.Errorf("body missing the ended-unmeasured footer note; body = %q", dr.Body)
	}
	if strings.Contains(dr.Body, "has not reported token usage yet") {
		t.Error("body contains the running-unreported note for an arrival-none session, want it absent")
	}
}

func TestHandleDashboard_NoUnmeasuredEntries_NoFooterNote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:       "id-m",
				Identifier:    "MT-M",
				State:         "In Progress",
				StartedAt:     now.Add(-3 * time.Minute),
				UsageMeasured: true,
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}
	if strings.Contains(dr.Body, "have not reported token usage") {
		t.Errorf("body = %q, want no unmeasured-sessions footer note", dr.Body)
	}
}

// usageArrivalValues and usageAttributionValues enumerate every value,
// including the undeclared zero value, so a totality test covers the full
// combination space.
var (
	usageArrivalValues = []registry.UsageArrival{
		registry.UsageArrivalUndeclared,
		registry.UsageArrivalIncremental,
		registry.UsageArrivalTurnEnd,
		registry.UsageArrivalNone,
	}
	usageAttributionValues = []registry.UsageAttribution{
		registry.UsageAttributionUndeclared,
		registry.UsageAttributionPerModel,
		registry.UsageAttributionSessionTotal,
		registry.UsageAttributionNone,
	}
)

func TestUsageRowFunctions_Totality(t *testing.T) {
	t.Parallel()

	for _, arrival := range usageArrivalValues {
		for _, attribution := range usageAttributionValues {
			for _, measured := range []bool{false, true} {
				for _, pending := range []bool{false, true} {
					name := string(arrival) + "/" + string(attribution)
					if measured {
						name += "/measured"
					}
					if pending {
						name += "/pending"
					}
					if name == "" {
						name = "undeclared/undeclared"
					}

					if got := usageReportingRow(arrival, attribution); got == "" {
						t.Errorf("usageReportingRow(%q, %q) = empty, want non-empty (%s)", arrival, attribution, name)
					}
					if got := usageModelRow(attribution, ""); got == "" {
						t.Errorf("usageModelRow(%q, \"\") = empty, want non-empty (%s)", attribution, name)
					}
					if got := usageModelRow(attribution, "some-model"); got == "" {
						t.Errorf("usageModelRow(%q, \"some-model\") = empty, want non-empty (%s)", attribution, name)
					}
					if got := usageAPIRequestsRow(arrival, measured, 3, nil); got == "" {
						t.Errorf("usageAPIRequestsRow(%q, %v, 3, nil) = empty, want non-empty (%s)", arrival, measured, name)
					}
					if got := usageAPIRequestsRow(arrival, measured, 3, map[string]int{"a": 1, "b": 2}); got == "" {
						t.Errorf("usageAPIRequestsRow(%q, %v, 3, breakdown) = empty, want non-empty (%s)", arrival, measured, name)
					}
					if got := usageTokensRow(arrival, measured, true, pending, "1,234"); got == "" {
						t.Errorf("usageTokensRow(%q, %v, true, %v, \"1,234\") = empty, want non-empty (%s)", arrival, measured, pending, name)
					}
					if got := usageTokensRow(arrival, measured, false, pending, "1,234"); got == "" {
						t.Errorf("usageTokensRow(%q, %v, false, %v, \"1,234\") = empty, want non-empty (%s)", arrival, measured, pending, name)
					}
					if got := usageEstCostRow(arrival, true, pending, "$1.23"); got == "" {
						t.Errorf("usageEstCostRow(%q, true, %v, \"$1.23\") = empty, want non-empty (%s)", arrival, pending, name)
					}
					if got := usageEstCostRow(arrival, false, pending, ""); got == "" {
						t.Errorf("usageEstCostRow(%q, false, %v, \"\") = empty, want non-empty (%s)", arrival, pending, name)
					}
				}
			}
		}
	}
}

func TestUsageReportingRow_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		arrival     registry.UsageArrival
		attribution registry.UsageAttribution
		want        string
	}{
		{"none/none", registry.UsageArrivalNone, registry.UsageAttributionNone, "this session reports no token usage"},
		{"incremental/per_model", registry.UsageArrivalIncremental, registry.UsageAttributionPerModel, "figures arrive during each turn, per model"},
		{"incremental/session_total", registry.UsageArrivalIncremental, registry.UsageAttributionSessionTotal, "figures arrive during each turn, as a session total"},
		{"turn_end/per_model", registry.UsageArrivalTurnEnd, registry.UsageAttributionPerModel, "figures arrive when a turn ends, per model"},
		{"turn_end/session_total", registry.UsageArrivalTurnEnd, registry.UsageAttributionSessionTotal, "figures arrive when a turn ends, as a session total"},
		{"undeclared/undeclared", registry.UsageArrivalUndeclared, registry.UsageAttributionUndeclared, "not declared"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := usageReportingRow(tt.arrival, tt.attribution); got != tt.want {
				t.Errorf("usageReportingRow(%q, %q) = %q, want %q", tt.arrival, tt.attribution, got, tt.want)
			}
		})
	}
}

func TestUsageModelRow_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		attribution registry.UsageAttribution
		modelName   string
		want        string
	}{
		{"none attribution", registry.UsageAttributionNone, "", dashPlaceholder},
		{"session_total attribution", registry.UsageAttributionSessionTotal, "", "not attributed to a model"},
		{"per_model attribution, model not yet reported", registry.UsageAttributionPerModel, "", "not reported yet"},
		{"per_model attribution, model reported", registry.UsageAttributionPerModel, "claude-sonnet-5", "claude-sonnet-5"},
		{"undeclared attribution", registry.UsageAttributionUndeclared, "", dashPlaceholder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := usageModelRow(tt.attribution, tt.modelName); got != tt.want {
				t.Errorf("usageModelRow(%q, %q) = %q, want %q", tt.attribution, tt.modelName, got, tt.want)
			}
		})
	}
}

func TestUsageAPIRequestsRow_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		arrival  registry.UsageArrival
		measured bool
		want     string
	}{
		{"none arrival", registry.UsageArrivalNone, false, dashPlaceholder},
		{"incremental arrival, measured", registry.UsageArrivalIncremental, true, "7"},
		{"turn_end arrival", registry.UsageArrivalTurnEnd, false, "not measured"},
		{"undeclared arrival", registry.UsageArrivalUndeclared, false, "not measured"},
		{"arrival outside the declared set", registry.UsageArrival("bogus"), false, "not measured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := usageAPIRequestsRow(tt.arrival, tt.measured, 7, nil); got != tt.want {
				t.Errorf("usageAPIRequestsRow(%q, %v, 7, nil) = %q, want %q", tt.arrival, tt.measured, got, tt.want)
			}
		})
	}
}

// TestUsageTokensRow_Golden pins the rendered string per arrival. The two
// unmeasured arms differ by one word: only a session whose figure may still
// arrive is offered as one the operator can wait for.
func TestUsageTokensRow_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		arrival       registry.UsageArrival
		usageMeasured bool
		tokensAwaited bool
		tokensPending bool
		want          string
	}{
		{name: "none arrival", arrival: registry.UsageArrivalNone, want: dashPlaceholder},
		{name: "not measured, figure still to come", arrival: registry.UsageArrivalIncremental, tokensAwaited: true, want: "not reported yet"},
		{name: "not measured, figure was due and never came", arrival: registry.UsageArrivalTurnEnd, want: "not reported"},
		{name: "pending", arrival: registry.UsageArrivalTurnEnd, usageMeasured: true, tokensPending: true, want: "1,234, excludes the turn in progress"},
		{name: "incremental settled", arrival: registry.UsageArrivalIncremental, usageMeasured: true, want: "1,234"},
		{name: "turn_end settled", arrival: registry.UsageArrivalTurnEnd, usageMeasured: true, want: "1,234"},
		{name: "undeclared arrival", arrival: registry.UsageArrivalUndeclared, usageMeasured: true, want: "1,234, usage reporting not declared"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usageTokensRow(tt.arrival, tt.usageMeasured, tt.tokensAwaited, tt.tokensPending, "1,234")
			if got != tt.want {
				t.Errorf("usageTokensRow(%q, %v, %v, %v, \"1,234\") = %q, want %q",
					tt.arrival, tt.usageMeasured, tt.tokensAwaited, tt.tokensPending, got, tt.want)
			}
		})
	}
}

func TestUsageEstCostRow_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		arrival       registry.UsageArrival
		hasRates      bool
		tokensPending bool
		costStr       string
		want          string
	}{
		{"none arrival", registry.UsageArrivalNone, true, false, "$1.23", dashPlaceholder},
		{"rates unconfigured", registry.UsageArrivalIncremental, false, false, "", dashPlaceholder},
		{"pending", registry.UsageArrivalTurnEnd, true, true, "$1.23", "$1.23, excludes the turn in progress"},
		{"settled with cost", registry.UsageArrivalIncremental, true, false, "$1.23", "$1.23"},
		{"settled with no cost computed", registry.UsageArrivalIncremental, true, false, "", dashPlaceholder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := usageEstCostRow(tt.arrival, tt.hasRates, tt.tokensPending, tt.costStr); got != tt.want {
				t.Errorf("usageEstCostRow(%q, %v, %v, %q) = %q, want %q", tt.arrival, tt.hasRates, tt.tokensPending, tt.costStr, got, tt.want)
			}
		})
	}
}

// usageReportingKindStrings names every registered agent kind, so the
// absent-string test can confirm none leaks into operator-facing copy.
var usageReportingKindStrings = []string{
	"agent-client-protocol",
	"claude-code",
	"codex",
	"copilot-cli",
	"kiro",
	"mock",
	"opencode",
}

// TestHandleDashboard_UsageReportingPanel_NoAdapterOrLaunchModeStrings
// asserts no rendered output names an agent kind or launch mode: a session
// narrowed by its launch mode reads as one that reports nothing, and the
// preflight diagnostic, not the dashboard, states cause and remedy.
func TestHandleDashboard_UsageReportingPanel_NoAdapterOrLaunchModeStrings(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID: "id-none", Identifier: "MT-NONE", State: "In Progress",
				StartedAt: now.Add(-time.Minute), SSHHost: "build-host.example.com",
				UsageArrival: registry.UsageArrivalNone, UsageAttribution: registry.UsageAttributionNone,
			},
			{
				IssueID: "id-turnend", Identifier: "MT-TURNEND", State: "In Progress",
				StartedAt: now.Add(-2 * time.Minute), UsageMeasured: true,
				UsageArrival: registry.UsageArrivalTurnEnd, UsageAttribution: registry.UsageAttributionSessionTotal,
				ModelName: "irrelevant",
			},
			{
				IssueID: "id-incremental", Identifier: "MT-INCREMENTAL", State: "In Progress",
				StartedAt: now.Add(-3 * time.Minute), UsageMeasured: true,
				UsageArrival: registry.UsageArrivalIncremental, UsageAttribution: registry.UsageAttributionPerModel,
				ModelName: "claude-sonnet-5",
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	lower := strings.ToLower(dr.Body)
	for _, kind := range usageReportingKindStrings {
		if strings.Contains(lower, strings.ToLower(kind)) {
			t.Errorf("body contains agent kind string %q, want none", kind)
		}
	}
	for _, launchMode := range []string{"ssh", "remote"} {
		if strings.Contains(lower, launchMode) {
			t.Errorf("body contains launch-mode string %q, want none", launchMode)
		}
	}
}

// TestHandleDashboard_UsageReportingPanel_StatesOnceAndFirst asserts a
// rendered panel states a usage-reporting fact once, and the Usage reporting
// row precedes Model, API Requests, Tokens, and Est. Cost in document order.
func TestHandleDashboard_UsageReportingPanel_StatesOnceAndFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID: "id-none", Identifier: "MT-NONE", State: "In Progress",
				StartedAt:    now.Add(-time.Minute),
				UsageArrival: registry.UsageArrivalNone, UsageAttribution: registry.UsageAttributionNone,
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")

	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	const sentence = "this session reports no token usage"
	if count := strings.Count(dr.Body, sentence); count != 1 {
		t.Errorf("body contains %q %d times, want exactly 1", sentence, count)
	}

	usageIdx := strings.Index(dr.Body, "<dt>Usage reporting</dt>")
	if usageIdx < 0 {
		t.Fatal("body missing the Usage reporting row label")
	}
	for _, label := range []string{"<dt>Model</dt>", "<dt>API Requests</dt>", "<dt>Tokens</dt>", "<dt>Est. Cost</dt>"} {
		idx := strings.Index(dr.Body, label)
		if idx < 0 {
			t.Fatalf("body missing the %s row label", label)
		}
		if usageIdx >= idx {
			t.Errorf("Usage reporting row at byte %d does not precede %s at byte %d", usageIdx, label, idx)
		}
	}
}

// extractDashboardRow returns the text of the <dd> following <dt>label</dt>,
// so a test can read one row's own value rather than searching the whole
// page for a substring.
func extractDashboardRow(t *testing.T, body, label string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)<dt>` + regexp.QuoteMeta(label) + `</dt>\s*<dd>(.*?)</dd>`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("body missing a <dt>%s</dt> row", label)
	}
	return strings.TrimSpace(m[1])
}

// TestRequestCountStateMatrix_RenderedRowAndWire walks the full state matrix
// and checks both presenters. The wire half checks api_request_count is null
// exactly when the verdict is false; the rendered half matches the API
// Requests row's own value against the admissible strings, rather than
// searching the page for an absent numeral, which the page's other numbers
// would defeat.
func TestRequestCountStateMatrix_RenderedRowAndWire(t *testing.T) {
	t.Parallel()

	admissibleUnmeasured := map[string]bool{
		"not reported yet": true,
		"not measured":     true,
		dashPlaceholder:    true,
	}

	tests := []struct {
		name            string
		arrival         registry.UsageArrival
		measured        bool
		apiRequestCount int
	}{
		{"incremental, no turn, zero count: measured, presents zero", registry.UsageArrivalIncremental, true, 0},
		{"incremental, no turn, positive count: measured, presents the count", registry.UsageArrivalIncremental, true, 5},
		{"incremental, turns began, zero count: unmeasured", registry.UsageArrivalIncremental, false, 0},
		{"incremental, turns began, positive count: measured, presents the count", registry.UsageArrivalIncremental, true, 7},
		{"turn_end, unmeasured, contradicted raw count ignored on the wire", registry.UsageArrivalTurnEnd, false, 9},
		{"none, unmeasured, contradicted raw count ignored on the wire", registry.UsageArrivalNone, false, 9},
		{"undeclared arrival, unmeasured", registry.UsageArrivalUndeclared, false, 9},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry := orchestrator.SnapshotRunningEntry{
				IssueID:             "issue-matrix",
				Identifier:          "MT-MATRIX",
				State:               "In Progress",
				StartedAt:           time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
				UsageArrival:        tt.arrival,
				APIRequestCount:     tt.apiRequestCount,
				APIRequestsMeasured: tt.measured,
			}

			resp := toRunningEntryResponse(entry)
			if (resp.APIRequestCount != nil) != tt.measured {
				t.Fatalf("APIRequestCount != nil = %v, want %v (api_requests_measured)", resp.APIRequestCount != nil, tt.measured)
			}
			if tt.measured && *resp.APIRequestCount != tt.apiRequestCount {
				t.Errorf("APIRequestCount = %d, want %d", *resp.APIRequestCount, tt.apiRequestCount)
			}

			snap := orchestrator.RuntimeSnapshotResult{
				GeneratedAt: entry.StartedAt.Add(time.Minute),
				Running:     []orchestrator.SnapshotRunningEntry{entry},
			}
			ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
			dr := getDashboard(t, ts, "/")
			if dr.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
			}

			row := extractDashboardRow(t, dr.Body, "API Requests")
			if tt.measured {
				want := FormatInt(int64(tt.apiRequestCount))
				if row != want {
					t.Errorf("rendered API Requests row = %q, want %q", row, want)
				}
				return
			}
			if !admissibleUnmeasured[row] {
				t.Errorf("rendered API Requests row = %q, want one of %v", row, admissibleUnmeasured)
			}
			if strings.Contains(row, FormatInt(int64(tt.apiRequestCount))) && tt.apiRequestCount != 0 {
				t.Errorf("rendered API Requests row = %q leaks the raw unmeasured count %d", row, tt.apiRequestCount)
			}
		})
	}
}

// TestHandleDashboard_TurnEndMeasuredTokensUnmeasuredRequests is an
// end-to-end check: a session that delivers usage on a turn-final event but
// never a token_usage event renders "not measured" in API Requests and its
// real non-zero total in Tokens, with no zero standing in for either fact.
func TestHandleDashboard_TurnEndMeasuredTokensUnmeasuredRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:             "id-turnend-p10",
				Identifier:          "MT-P10",
				State:               "In Progress",
				StartedAt:           now.Add(-3 * time.Minute),
				LastAgentEvent:      domain.EventTurnCompleted,
				UsageArrival:        registry.UsageArrivalTurnEnd,
				UsageAttribution:    registry.UsageAttributionSessionTotal,
				UsageMeasured:       true,
				APIRequestsMeasured: false,
				APIRequestCount:     0,
				AgentTotalTokens:    1500,
			},
		},
	}

	ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)
	dr := getDashboard(t, ts, "/")
	if dr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", dr.StatusCode, http.StatusOK)
	}

	requestsRow := extractDashboardRow(t, dr.Body, "API Requests")
	if requestsRow != "not measured" {
		t.Errorf("API Requests row = %q, want %q", requestsRow, "not measured")
	}
	tokensRow := extractDashboardRow(t, dr.Body, "Tokens")
	if tokensRow != "1,500" {
		t.Errorf("Tokens row = %q, want %q", tokensRow, "1,500")
	}
	if requestsRow == "0" || tokensRow == "0" {
		t.Errorf("a row states a fabricated zero: API Requests=%q, Tokens=%q", requestsRow, tokensRow)
	}
}

// TestDashboard_SessionPastItsArrivalPointReportsNothing: a turn-end kind
// whose turn ended with no figure must not be offered as one still to come.
func TestDashboard_SessionPastItsArrivalPointReportsNothing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	state := orchestrator.NewState(5000, 4, 500_000, nil, orchestrator.AgentTotals{})
	state.Running["iss"] = &orchestrator.RunningEntry{
		Identifier:     "MT-INERT",
		Issue:          domain.Issue{ID: "iss", State: "In Progress"},
		StartedAt:      now.Add(-time.Minute),
		AgentKind:      "transport",
		TurnCount:      1,
		LastAgentEvent: domain.EventTurnCompleted,
		UsageArrival:   registry.UsageArrivalTurnEnd,
	}

	data := buildDashboardData(orchestrator.RuntimeSnapshot(state, now), "test", now.Add(-time.Hour), nil, now, nil)

	if len(data.Running) != 1 {
		t.Fatalf("dashboard rendered %d running rows, want 1", len(data.Running))
	}
	if got := data.Running[0].TokensRow; got != "not reported" {
		t.Errorf("Tokens row = %q, want %q", got, "not reported")
	}
	if got := data.RunningUnreportedNote; got != "" {
		t.Errorf("RunningUnreportedNote = %q, want empty: this session's figures are not still to come", got)
	}
	want := "1 running session runs an agent that reports no token usage; the totals above exclude it."
	if got := data.RunningNonReportingNote; got != want {
		t.Errorf("RunningNonReportingNote = %q, want %q", got, want)
	}
}

// TestDashboard_SessionInsideItsFirstTurnStillAwaitsFigures is the control:
// the same absent figure, but the settling turn has not ended, so the panel
// keeps saying the figure has not arrived yet.
func TestDashboard_SessionInsideItsFirstTurnStillAwaitsFigures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	state := orchestrator.NewState(5000, 4, 500_000, nil, orchestrator.AgentTotals{})
	state.Running["iss"] = &orchestrator.RunningEntry{
		Identifier:     "MT-PENDING",
		Issue:          domain.Issue{ID: "iss", State: "In Progress"},
		StartedAt:      now.Add(-time.Minute),
		AgentKind:      "transport",
		TurnCount:      1,
		LastAgentEvent: domain.EventOtherMessage,
		UsageArrival:   registry.UsageArrivalTurnEnd,
	}

	data := buildDashboardData(orchestrator.RuntimeSnapshot(state, now), "test", now.Add(-time.Hour), nil, now, nil)

	if got := data.Running[0].TokensRow; got != "not reported yet" {
		t.Errorf("Tokens row = %q, want %q", got, "not reported yet")
	}
	want := "1 running session has not reported token usage yet; the totals above exclude it."
	if got := data.RunningUnreportedNote; got != want {
		t.Errorf("RunningUnreportedNote = %q, want %q", got, want)
	}
	if got := data.RunningNonReportingNote; got != "" {
		t.Errorf("RunningNonReportingNote = %q, want empty", got)
	}
}
