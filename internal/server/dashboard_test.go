package server

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// --- Test helpers ---

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
			},
			{
				IssueID:          "id-649",
				Identifier:       "MT-649",
				State:            "In Progress",
				TurnCount:        7,
				LastAgentEvent:   domain.EventNotification,
				StartedAt:        now.Add(-12 * time.Minute),
				AgentTotalTokens: 2000,
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

// --- Tests ---

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

		data := buildDashboardData(snap, "0.2.0", startedAt, func() int { return 5 }, now)

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

		data := buildDashboardData(snap, "", startedAt, nil, now)
		if data.Version != "dev" {
			t.Errorf("Version = %q, want %q", data.Version, "dev")
		}
	})

	t.Run("nil slotFunc yields zero available", func(t *testing.T) {
		t.Parallel()

		data := buildDashboardData(snap, "v1", startedAt, nil, now)
		if data.AvailableSlots != 0 {
			t.Errorf("AvailableSlots = %d, want 0", data.AvailableSlots)
		}
	})

	t.Run("available slots clamped to zero", func(t *testing.T) {
		t.Parallel()

		// slotFunc returns 1 but 2 running → clamped to 0.
		data := buildDashboardData(snap, "v1", startedAt, func() int { return 1 }, now)
		if data.AvailableSlots != 0 {
			t.Errorf("AvailableSlots = %d, want 0", data.AvailableSlots)
		}
	})

	// --- Timing percentage formatting tests ---

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

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now)

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

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now)

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

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now)

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

		data := buildDashboardData(timingSnap, "v1", startedAt, nil, now)

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

		data := buildDashboardData(midTurnSnap, "v1", startedAt, nil, now)

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

			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
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

func TestFmtInt(t *testing.T) {
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

			got := fmtInt(tt.input)
			if got != tt.want {
				t.Errorf("fmtInt(%d) = %q, want %q", tt.input, got, tt.want)
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

	// html/template must escape the script tag.
	// Check for the XSS payload specifically; the legitimate accordion script block is also present.
	if strings.Contains(dr.Body, "<script>alert") {
		t.Error("body contains unescaped XSS payload — XSS vulnerability")
	}
	// The escaped version should be present.
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

// TestBuildDashboardData_ExtendedFields verifies that the new
// CacheReadTokens, ModelName, and APIRequestCount fields are passed
// through buildDashboardData to the template data structures.
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

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), func() int { return 5 }, now)

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
	if entry.APIRequestCount != 12 {
		t.Errorf("Running[0].APIRequestCount = %d, want 12", entry.APIRequestCount)
	}
	if data.CacheReadTokens != 15000 {
		t.Errorf("CacheReadTokens = %d, want 15000", data.CacheReadTokens)
	}
}

// TestHandleDashboard_ExtendedFieldsRendered verifies that the extended
// fields appear in the HTML output.
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

	// No RunHistoryFn → run history section must be omitted entirely.
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

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), nil, now)

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

	data := buildDashboardData(snap, "1.0.0", now.Add(-1*time.Hour), nil, now)

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

// TestHandleDashboard_FooterCacheReadLabel verifies that the dashboard footer uses
// the "Cache Read:" label (not the ambiguous "Cache:") and includes a tooltip
// explaining the metric for users unfamiliar with prompt caching.
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

	// The footer must use "Cache Read:" as the label.
	if !strings.Contains(dr.Body, "Cache Read:") {
		t.Error(`footer body missing "Cache Read:" label`)
	}

	// The span must carry the tooltip explaining the metric.
	wantTitle := `title="Prompt cache read tokens`
	if !strings.Contains(dr.Body, wantTitle) {
		t.Errorf("footer body missing tooltip attribute %q", wantTitle)
	}

	// The cache read token value must appear formatted.
	if !strings.Contains(dr.Body, "2,077,449") {
		t.Error(`footer body missing formatted cache read token count "2,077,449"`)
	}
}

// TestHandleDashboard_SessionsCachedTokensTooltip verifies that the Running
// Sessions table renders the cached token annotation with an explanatory tooltip
// when CacheReadTokens is non-zero.
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
					},
				},
			}
			ts := dashboardServer(t, fixedSnapshot(snap), "1.0.0", nil)

			dr := getDashboard(t, ts, "/")

			if dr.StatusCode != http.StatusOK {
				t.Fatalf("GET / status = %d, want %d", dr.StatusCode, http.StatusOK)
			}

			// The <small> tag in the sessions table is guarded by {{if .CacheReadTokens}},
			// so the tooltip only appears in the table rows when cached tokens are non-zero.
			wantTitle := `<small title="Prompt cache read tokens`
			gotTooltip := strings.Contains(dr.Body, wantTitle)
			if gotTooltip != tt.wantTooltip {
				t.Errorf("body contains sessions-table tooltip %q = %v, want %v", wantTitle, gotTooltip, tt.wantTooltip)
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

// TestHandleDashboard_AccordionToggleRefactor verifies that accordion rows use
// a <button> in the first cell rather than role="button" on the <tr>. It covers
// all three table sections (running, retrying, run history).
func TestHandleDashboard_AccordionToggleRefactor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			// StartedAt ascending → R0 is index 0, R1 is index 1 after sort.
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
		if !strings.Contains(body, `<button type="button" class="accordion-toggle"`) {
			t.Error(`body missing <button type="button" class="accordion-toggle">`)
		}
		// 2 running + 1 retrying + 1 history = 4 accordion-toggle buttons.
		const wantCount = 4
		if got := strings.Count(body, `<button type="button" class="accordion-toggle"`); got != wantCount {
			t.Errorf("accordion-toggle button count = %d, want %d", got, wantCount)
		}
	})

	t.Run("aria-expanded=false is on button not on tr", func(t *testing.T) {
		t.Parallel()
		// Positive: aria-expanded must appear as a button attribute.
		if !strings.Contains(body, `class="accordion-toggle" aria-expanded="false"`) {
			t.Error(`body missing aria-expanded="false" on accordion-toggle button`)
		}
		// Negative: <tr elements must not carry aria-expanded.
		// The old pattern placed aria-expanded directly on the <tr> alongside
		// role="button"; its absence confirms the refactor is complete.
		if strings.Contains(body, `tabindex="0"`) {
			t.Error(`body contains tabindex="0" — must not appear after accordion toggle refactor`)
		}
	})

	t.Run("aria-controls on button matches id of detail row", func(t *testing.T) {
		t.Parallel()
		// Each table section uses its own ID prefix with 0-based row counter.
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
		// tabindex="0" was only ever used on accordion <tr> rows; its absence
		// confirms the attribute was removed from table rows.
		if strings.Contains(body, `tabindex="0"`) {
			t.Error(`body contains tabindex="0" — must not appear after accordion toggle refactor`)
		}
	})
}
