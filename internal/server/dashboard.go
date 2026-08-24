package server

import (
	"bytes"
	"cmp"
	_ "embed" // required for //go:embed directives
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	Running              []dashboardRunningEntry
	Retrying             []dashboardRetryEntry
	RunHistory           []dashboardRunHistoryEntry
	HasSSH               bool
	BudgetExhaustedCount int
	BudgetExhausted      []dashboardBudgetEntry

	// Footer
	RuntimeDisplay  string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64

	// Cost estimation (conditional on configured token rates).
	HasTokenRates      bool
	EstimatedCostUSD   *string
	EstimatedCostLabel string

	// UnmeasuredRunningCount is the number of running sessions that have
	// reported no token measurement so far. Excluded from TotalTokens,
	// InputTokens, OutputTokens, CacheReadTokens, and EstimatedCostUSD.
	UnmeasuredRunningCount int
}

type dashboardRunningEntry struct {
	Identifier       string
	State            string
	TurnCount        int
	Duration         string
	LastEvent        string
	TotalTokens      int64
	CacheReadTokens  int64
	ModelName        string
	APIRequestCount  int
	DetailURL        string
	Host             string
	ToolTimePct      string
	APITimePct       string
	WorkflowFile     string
	EstimatedCostUSD string
	UsageMeasured    bool
}

type dashboardRetryEntry struct {
	Identifier string
	Attempt    int
	DueIn      string
	Error      string
}

// dashboardBudgetEntry is one pre-formatted row of the budget-blocked
// table. All formatting happens in Go; the template computes nothing.
type dashboardBudgetEntry struct {
	Identifier string
	Reason     string
	Used       string
	BlockedFor string
	DetailURL  string
}

// budgetReasonLabels maps a machine-readable budget reason to the
// human-readable label the dashboard renders. A reason absent from this
// map renders as its raw value, keeping the mapping open to a later
// reason this table has not yet been taught to humanize.
var budgetReasonLabels = map[string]string{
	"session_budget": "Session budget",
	"token_budget":   "Token budget",
}

type dashboardRunHistoryEntry struct {
	Identifier   string
	Attempt      int
	Turns        int
	Status       string
	WorkflowFile string
	StartedAt    string
	Duration     string
	Error        string
}

// FormatInt formats an int64 with comma thousand separators.
func FormatInt(v int64) string {
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

// FormatDuration formats a duration as a human-readable string with at
// most three components. When days are present, seconds are dropped.
// Negative durations return "0s".
func FormatDuration(d time.Duration) string {
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
	return "in " + FormatDuration(diff)
}

// buildDashboardData maps a [orchestrator.RuntimeSnapshotResult] into
// the template-ready [dashboardData] struct.
func buildDashboardData(
	snap orchestrator.RuntimeSnapshotResult,
	version string,
	startedAt time.Time,
	slotFunc func() int,
	now time.Time,
	tokenRates TokenRates,
) dashboardData {
	if version == "" {
		version = "dev"
	}

	runningCount := len(snap.Running)

	available := 0
	if slotFunc != nil {
		available = max(slotFunc()-runningCount, 0)
	}

	uptimeDur := time.Duration(0)
	if !startedAt.IsZero() {
		uptimeDur = max(now.Sub(startedAt), 0)
	}

	data := dashboardData{
		Version:         version,
		Uptime:          FormatDuration(uptimeDur),
		GeneratedAt:     snap.GeneratedAt,
		RunningCount:    runningCount,
		RetryingCount:   len(snap.Retrying),
		AvailableSlots:  available,
		TotalTokens:     snap.AgentTotals.TotalTokens,
		RuntimeDisplay:  FormatDuration(time.Duration(snap.AgentTotals.SecondsRunning * float64(time.Second))),
		InputTokens:     snap.AgentTotals.InputTokens,
		OutputTokens:    snap.AgentTotals.OutputTokens,
		CacheReadTokens: snap.AgentTotals.CacheReadTokens,
	}

	// Copy and sort running entries by StartedAt ascending before mapping.
	sortedRunning := make([]orchestrator.SnapshotRunningEntry, len(snap.Running))
	copy(sortedRunning, snap.Running)
	slices.SortFunc(sortedRunning, func(a, b orchestrator.SnapshotRunningEntry) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	running := make([]dashboardRunningEntry, len(sortedRunning))
	hasSSH := false
	hasRates := len(tokenRates) > 0
	var aggregateCost float64
	aggregateCostSet := false
	unmeasuredCount := 0
	for i, e := range sortedRunning {
		dur := max(snap.GeneratedAt.Sub(e.StartedAt), 0)
		if e.SSHHost != "" {
			hasSSH = true
		}

		toolPct := "N/A"
		apiPct := "N/A"
		if !e.StartedAt.IsZero() {
			elapsedMs := snap.GeneratedAt.Sub(e.StartedAt).Milliseconds()
			if elapsedMs > 0 && e.ToolTimeMs > 0 {
				toolPct = fmt.Sprintf("%.1f%%", float64(e.ToolTimeMs)/float64(elapsedMs)*100.0)
			}
			if elapsedMs > 0 && e.APITimeMs > 0 {
				apiPct = fmt.Sprintf("%.1f%%", float64(e.APITimeMs)/float64(elapsedMs)*100.0)
			}
		}

		displayID := e.Identifier
		if e.DisplayID != "" {
			displayID = e.DisplayID
		}

		if !e.UsageMeasured {
			unmeasuredCount++
		}

		var entryCostStr string
		if hasRates && e.AgentKind != "" && e.UsageMeasured {
			if rc, ok := tokenRates[e.AgentKind]; ok {
				if c := EstimateCost(e.AgentInputTokens, e.AgentOutputTokens, e.CacheReadTokens, &rc); c != nil {
					entryCostStr = FormatCost(*c)
					aggregateCost += *c
					aggregateCostSet = true
				}
			}
		}

		running[i] = dashboardRunningEntry{
			Identifier:       displayID,
			State:            e.State,
			TurnCount:        e.TurnCount,
			Duration:         FormatDuration(dur),
			LastEvent:        string(e.LastAgentEvent),
			TotalTokens:      e.AgentTotalTokens,
			CacheReadTokens:  e.CacheReadTokens,
			ModelName:        e.ModelName,
			APIRequestCount:  e.APIRequestCount,
			DetailURL:        "/api/v1/" + url.PathEscape(e.Identifier),
			Host:             e.SSHHost,
			ToolTimePct:      toolPct,
			APITimePct:       apiPct,
			WorkflowFile:     e.WorkflowFile,
			EstimatedCostUSD: entryCostStr,
			UsageMeasured:    e.UsageMeasured,
		}
	}
	data.Running = running
	data.HasSSH = hasSSH
	data.HasTokenRates = hasRates
	data.UnmeasuredRunningCount = unmeasuredCount
	if hasRates {
		data.EstimatedCostLabel = "Active Est. Cost (USD)"
	}
	if aggregateCostSet {
		data.EstimatedCostUSD = new(FormatCost(aggregateCost))
	}

	// Copy and sort retry entries by DueAtMS ascending before mapping.
	sortedRetrying := make([]orchestrator.SnapshotRetryEntry, len(snap.Retrying))
	copy(sortedRetrying, snap.Retrying)
	slices.SortFunc(sortedRetrying, func(a, b orchestrator.SnapshotRetryEntry) int {
		return cmp.Compare(a.DueAtMS, b.DueAtMS)
	})
	retrying := make([]dashboardRetryEntry, len(sortedRetrying))
	for i, e := range sortedRetrying {
		retryDisplayID := e.Identifier
		if e.DisplayID != "" {
			retryDisplayID = e.DisplayID
		}

		retrying[i] = dashboardRetryEntry{
			Identifier: retryDisplayID,
			Attempt:    e.Attempt,
			DueIn:      formatRelativeTime(e.DueAtMS, snap.GeneratedAt),
			Error:      e.Error,
		}
	}
	data.Retrying = retrying

	// snap.BudgetExhausted is already sorted by Identifier; no re-sort
	// needed here, unlike Running and Retrying above.
	budgetExhausted := make([]dashboardBudgetEntry, len(snap.BudgetExhausted))
	for i, e := range snap.BudgetExhausted {
		budgetDisplayID := e.Identifier
		if e.DisplayID != "" {
			budgetDisplayID = e.DisplayID
		}
		if budgetDisplayID == "" {
			budgetDisplayID = e.IssueID
		}

		reasonLabel, known := budgetReasonLabels[e.Reason]
		if !known {
			reasonLabel = e.Reason
		}

		var used string
		switch e.Reason {
		case "token_budget":
			var usedTokens int64
			if e.UsedTokens != nil {
				usedTokens = *e.UsedTokens
			}
			used = FormatInt(usedTokens) + " of " + FormatInt(e.BudgetTokens)
		default:
			used = FormatInt(int64(e.UsedSessions)) + " of " + FormatInt(int64(e.BudgetSessions))
		}

		blockedFor := max(snap.GeneratedAt.Sub(e.ExhaustedAt), 0)

		budgetExhausted[i] = dashboardBudgetEntry{
			Identifier: budgetDisplayID,
			Reason:     reasonLabel,
			Used:       used,
			BlockedFor: FormatDuration(blockedFor),
			DetailURL:  "/api/v1/" + url.PathEscape(e.Identifier),
		}
	}
	data.BudgetExhaustedCount = len(budgetExhausted)
	data.BudgetExhausted = budgetExhausted

	return data
}

// mapRunHistoryEntries converts [RunHistoryEntry] values into the
// dashboard-specific [dashboardRunHistoryEntry] with pre-formatted
// fields. Duration is computed from StartedAt and CompletedAt when
// both are valid RFC 3339 strings.
func mapRunHistoryEntries(runs []RunHistoryEntry) []dashboardRunHistoryEntry {
	out := make([]dashboardRunHistoryEntry, len(runs))
	for i, r := range runs {
		dur := ""
		startT, errS := time.Parse(time.RFC3339, r.StartedAt)
		endT, errE := time.Parse(time.RFC3339, r.CompletedAt)
		if errS == nil && errE == nil {
			dur = FormatDuration(endT.Sub(startT))
		}

		errMsg := ""
		if r.Error != nil {
			errMsg = *r.Error
		}

		wf := r.WorkflowFile
		if wf == "" {
			wf = "\u2014" // em dash for missing
		}

		histDisplayID := r.Identifier
		if r.DisplayID != "" {
			histDisplayID = r.DisplayID
		}

		out[i] = dashboardRunHistoryEntry{
			Identifier:   histDisplayID,
			Attempt:      r.Attempt,
			Turns:        r.TurnsCompleted,
			Status:       r.Status,
			WorkflowFile: wf,
			StartedAt:    r.StartedAt,
			Duration:     dur,
			Error:        errMsg,
		}
	}
	return out
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

	data := buildDashboardData(snap, s.version, s.startedAt, s.slotFunc, time.Now(), s.tokenRates)

	if s.runHistoryFn != nil {
		runs, err := s.runHistoryFn(r.Context(), 25)
		if err != nil {
			s.logger.Warn("dashboard run history query failed", slog.Any("error", err))
		} else {
			data.RunHistory = mapRunHistoryEntries(runs)
		}
	}

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
