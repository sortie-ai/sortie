package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed favicon.ico
var faviconICO []byte

// dashboardData is the template context for the HTML dashboard.
// All duration and relative-time fields are pre-formatted in Go;
// the template performs no computation.
type dashboardData struct {
	// Header
	Version     string
	Uptime      string
	GeneratedAt time.Time

	// Summary cards
	RunningCount   int
	RetryingCount  int
	AvailableSlots int
	TotalTokens    int64

	// Tables
	Running  []dashboardRunningEntry
	Retrying []dashboardRetryEntry
	HasSSH   bool

	// Footer
	RuntimeDisplay  string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

type dashboardRunningEntry struct {
	Identifier      string
	State           string
	TurnCount       int
	Duration        string
	LastEvent       string
	TotalTokens     int64
	CacheReadTokens int64
	ModelName       string
	APIRequestCount int
	DetailURL       string
	Host            string
	ToolTimePct     string
	APITimePct      string
}

type dashboardRetryEntry struct {
	Identifier string
	Attempt    int
	DueIn      string
	Error      string
}

// fmtInt formats an int64 with comma thousand separators.
func fmtInt(v int64) string {
	// Format first to avoid negation overflow on math.MinInt64.
	s := strconv.FormatInt(v, 10)

	digits := s
	negative := false
	if s[0] == '-' {
		negative = true
		digits = s[1:]
	}

	n := len(digits)
	if n <= 3 {
		return s
	}

	// Insert commas every 3 digits from the right.
	commas := (n - 1) / 3
	buf := make([]byte, n+commas)
	j := len(buf) - 1
	for i := n - 1; i >= 0; i-- {
		buf[j] = digits[i]
		j--
		if (n-i)%3 == 0 && i > 0 {
			buf[j] = ','
			j--
		}
	}

	if negative {
		return "-" + string(buf)
	}
	return string(buf)
}

// formatDuration formats a duration as a human-readable string with at
// most three components. When days are present, seconds are dropped.
// Negative durations return "0s".
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}

	totalSec := int(d.Seconds())
	days := totalSec / 86400
	hours := (totalSec % 86400) / 3600
	minutes := (totalSec % 3600) / 60
	seconds := totalSec % 60

	if days > 0 {
		// Show at most 3 components, drop seconds when days present.
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// formatRelativeTime formats a due-at timestamp (milliseconds since
// epoch) relative to now. Returns "overdue" if the time has passed,
// "now" if within one second, or "in <duration>" for future times.
func formatRelativeTime(dueAtMS int64, now time.Time) string {
	dueAt := time.UnixMilli(dueAtMS)
	diff := dueAt.Sub(now)

	if diff <= 0 {
		if diff > -time.Second {
			return "now"
		}
		return "overdue"
	}
	return "in " + formatDuration(diff)
}

// buildDashboardData maps a [orchestrator.RuntimeSnapshotResult] into
// the template-ready [dashboardData] struct.
func buildDashboardData(
	snap orchestrator.RuntimeSnapshotResult,
	version string,
	startedAt time.Time,
	slotFunc func() int,
	now time.Time,
) dashboardData {
	if version == "" {
		version = "dev"
	}

	runningCount := len(snap.Running)

	available := 0
	if slotFunc != nil {
		available = slotFunc() - runningCount
		if available < 0 {
			available = 0
		}
	}

	uptimeDur := time.Duration(0)
	if !startedAt.IsZero() {
		uptimeDur = now.Sub(startedAt)
		if uptimeDur < 0 {
			uptimeDur = 0
		}
	}

	data := dashboardData{
		Version:         version,
		Uptime:          formatDuration(uptimeDur),
		GeneratedAt:     snap.GeneratedAt,
		RunningCount:    runningCount,
		RetryingCount:   len(snap.Retrying),
		AvailableSlots:  available,
		TotalTokens:     snap.AgentTotals.TotalTokens,
		RuntimeDisplay:  formatDuration(time.Duration(snap.AgentTotals.SecondsRunning * float64(time.Second))),
		InputTokens:     snap.AgentTotals.InputTokens,
		OutputTokens:    snap.AgentTotals.OutputTokens,
		CacheReadTokens: snap.AgentTotals.CacheReadTokens,
	}

	// Copy and sort running entries by StartedAt ascending before mapping.
	sortedRunning := make([]orchestrator.SnapshotRunningEntry, len(snap.Running))
	copy(sortedRunning, snap.Running)
	sort.Slice(sortedRunning, func(i, j int) bool {
		return sortedRunning[i].StartedAt.Before(sortedRunning[j].StartedAt)
	})
	running := make([]dashboardRunningEntry, len(sortedRunning))
	hasSSH := false
	for i, e := range sortedRunning {
		dur := snap.GeneratedAt.Sub(e.StartedAt)
		if dur < 0 {
			dur = 0
		}
		if e.SSHHost != "" {
			hasSSH = true
		}

		elapsedMs := snap.GeneratedAt.Sub(e.StartedAt).Milliseconds()
		toolPct := "N/A"
		if elapsedMs > 0 && e.ToolTimeMs > 0 {
			toolPct = fmt.Sprintf("%.1f%%", float64(e.ToolTimeMs)/float64(elapsedMs)*100.0)
		}
		apiPct := "N/A"
		if elapsedMs > 0 && e.APITimeMs > 0 {
			apiPct = fmt.Sprintf("%.1f%%", float64(e.APITimeMs)/float64(elapsedMs)*100.0)
		}

		running[i] = dashboardRunningEntry{
			Identifier:      e.Identifier,
			State:           e.State,
			TurnCount:       e.TurnCount,
			Duration:        formatDuration(dur),
			LastEvent:       string(e.LastAgentEvent),
			TotalTokens:     e.AgentTotalTokens,
			CacheReadTokens: e.CacheReadTokens,
			ModelName:       e.ModelName,
			APIRequestCount: e.APIRequestCount,
			DetailURL:       "/api/v1/" + url.PathEscape(e.Identifier),
			Host:            e.SSHHost,
			ToolTimePct:     toolPct,
			APITimePct:      apiPct,
		}
	}
	data.Running = running
	data.HasSSH = hasSSH

	// Copy and sort retry entries by DueAtMS ascending before mapping.
	sortedRetrying := make([]orchestrator.SnapshotRetryEntry, len(snap.Retrying))
	copy(sortedRetrying, snap.Retrying)
	sort.Slice(sortedRetrying, func(i, j int) bool {
		return sortedRetrying[i].DueAtMS < sortedRetrying[j].DueAtMS
	})
	retrying := make([]dashboardRetryEntry, len(sortedRetrying))
	for i, e := range sortedRetrying {
		retrying[i] = dashboardRetryEntry{
			Identifier: e.Identifier,
			Attempt:    e.Attempt,
			DueIn:      formatRelativeTime(e.DueAtMS, snap.GeneratedAt),
			Error:      e.Error,
		}
	}
	data.Retrying = retrying

	return data
}

// handleFavicon serves the embedded favicon.ico with aggressive caching.
func handleFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconICO)
}

// handleDashboard serves the HTML dashboard at GET /.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshotFn()
	if err != nil {
		s.logger.Error("dashboard snapshot failed", slog.Any("error", err))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(
			`<!DOCTYPE html><html><head><meta http-equiv="refresh" content="5"></head>` +
				`<body><p>Dashboard temporarily unavailable. Orchestrator snapshot error: ` +
				`</p><p>Retry in 5s.</p></body></html>`))
		return
	}

	data := buildDashboardData(snap, s.version, s.startedAt, s.slotFunc, time.Now())

	var buf bytes.Buffer
	if err := s.dashboardTmpl.Execute(&buf, data); err != nil {
		s.logger.Error("dashboard template execution failed", slog.Any("error", err))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(
			`<!DOCTYPE html><html><head><meta http-equiv="refresh" content="5"></head>` +
				`<body><p>Internal dashboard error.</p></body></html>`))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
