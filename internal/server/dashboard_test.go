package server

import (
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
	if strings.Contains(dr.Body, "<script>") {
		t.Error("body contains unescaped <script> tag — XSS vulnerability")
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
