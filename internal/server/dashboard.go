package server

import (
	"bytes"
	"cmp"
	_ "embed" // required for //go:embed directives
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed favicon.ico
var faviconICO []byte

// dashboardData is the template context for the HTML dashboard. All
// duration and relative-time fields are pre-formatted in Go; the template
// performs no computation.
type dashboardData struct {
	Version     string
	Uptime      string
	GeneratedAt time.Time

	RunningCount   int
	RetryingCount  int
	AvailableSlots int
	TotalTokens    int64

	Running              []dashboardRunningEntry
	Retrying             []dashboardRetryEntry
	RunHistory           []dashboardRunHistoryEntry
	HasSSH               bool
	BudgetExhaustedCount int
	BudgetExhausted      []dashboardBudgetEntry

	RuntimeDisplay  string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64

	HasTokenRates      bool
	EstimatedCostUSD   *string
	EstimatedCostLabel string

	// RunningUnreportedNote, RunningNonReportingNote, EndedUnmeasuredNote,
	// and CostUnpricedNote name a count the footer totals exclude, or are
	// empty when that count is zero.
	RunningUnreportedNote   string
	RunningNonReportingNote string
	EndedUnmeasuredNote     string
	CostUnpricedNote        string
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
	DetailURL        string
	Host             string
	ToolTimePct      string
	APITimePct       string
	WorkflowFile     string
	EstimatedCostUSD string
	UsageMeasured    bool

	// UsageReportingRow, ModelRow, APIRequestsRow, TokensRow, and
	// EstCostRow are pre-formatted usage-disposition panel rows the
	// template prints verbatim.
	UsageReportingRow string
	ModelRow          string
	APIRequestsRow    string
	TokensRow         string
	EstCostRow        string
}

const dashPlaceholder = "—"

// usageReportingRow renders the Usage reporting row: the one place a
// session's reason for reporting nothing is stated. The "not declared" arm
// is unreachable in a shipped binary and exists only to keep the switch
// total.
func usageReportingRow(arrival registry.UsageArrival, attribution registry.UsageAttribution) string {
	switch arrival {
	case registry.UsageArrivalNone:
		return "this session reports no token usage"
	case registry.UsageArrivalIncremental:
		return "figures arrive during each turn" + usageAttributionClause(attribution)
	case registry.UsageArrivalTurnEnd:
		return "figures arrive when a turn ends" + usageAttributionClause(attribution)
	default:
		return "not declared"
	}
}

// usageAttributionClause names what a declared, non-none arrival's figures
// attribute to, appended to the Usage reporting row.
func usageAttributionClause(attribution registry.UsageAttribution) string {
	switch attribution {
	case registry.UsageAttributionPerModel:
		return ", per model"
	case registry.UsageAttributionSessionTotal:
		return ", as a session total"
	default:
		return ""
	}
}

// usageModelRow renders the Model row. A none or undeclared attribution
// reaches the dash rather than restating the Usage reporting row's reason.
func usageModelRow(attribution registry.UsageAttribution, modelName string) string {
	switch {
	case attribution == registry.UsageAttributionNone:
		return dashPlaceholder
	case attribution == registry.UsageAttributionSessionTotal:
		return "not attributed to a model"
	case attribution.NamesModel():
		if modelName == "" {
			return "not reported yet"
		}
		return modelName
	default:
		return dashPlaceholder
	}
}

// usageAPIRequestsRow renders the API Requests row. A count shows only when
// the measurement verdict is true; the none arm is evaluated first so it
// reaches the dash rather than restating the Usage reporting row's reason.
// An incremental session reads "not reported yet" because a figure not yet
// arrived and one that never will are inseparable from the event stream.
func usageAPIRequestsRow(
	arrival registry.UsageArrival,
	measured bool,
	apiRequestCount int,
	requestsByModel map[string]int,
) string {
	switch {
	case arrival == registry.UsageArrivalNone:
		return dashPlaceholder
	case measured && len(requestsByModel) >= 2:
		return FormatInt(int64(apiRequestCount)) + " (" + formatRequestsByModel(requestsByModel) + ")"
	case measured:
		return FormatInt(int64(apiRequestCount))
	case arrival == registry.UsageArrivalIncremental:
		return "not reported yet"
	default:
		return "not measured"
	}
}

// formatRequestsByModel renders the per-model split as "model: count" pairs
// joined by commas, in ascending model-name order for determinism.
func formatRequestsByModel(requestsByModel map[string]int) string {
	pairs := make([]string, 0, len(requestsByModel))
	for _, model := range slices.Sorted(maps.Keys(requestsByModel)) {
		pairs = append(pairs, model+": "+FormatInt(int64(requestsByModel[model])))
	}
	return strings.Join(pairs, ", ")
}

// usageTokensRow renders the Tokens row. tokensStr is the pre-formatted
// figure with any cached-tokens suffix already applied. An unmeasured
// session reads "not reported yet" only while tokensAwaited; once its figure
// can no longer arrive the row drops that word rather than promise one that
// is not coming.
func usageTokensRow(arrival registry.UsageArrival, usageMeasured, tokensAwaited, tokensPending bool, tokensStr string) string {
	switch {
	case arrival == registry.UsageArrivalNone:
		return dashPlaceholder
	case !usageMeasured && tokensAwaited:
		return "not reported yet"
	case !usageMeasured:
		return "not reported"
	case tokensPending:
		return tokensStr + ", excludes the turn in progress"
	case arrival == registry.UsageArrivalIncremental || arrival == registry.UsageArrivalTurnEnd:
		return tokensStr
	default:
		return tokensStr + ", usage reporting not declared"
	}
}

// usageEstCostRow renders the Est. Cost row. costStr is the pre-formatted
// cost, or empty when none was computed. The none arm precedes the
// rates-unconfigured arm so their shared dash output is unobservable rather
// than contradictory.
func usageEstCostRow(arrival registry.UsageArrival, hasRates, tokensPending bool, costStr string) string {
	display := costStr
	if display == "" {
		display = dashPlaceholder
	}
	switch {
	case arrival == registry.UsageArrivalNone:
		return dashPlaceholder
	case !hasRates:
		return dashPlaceholder
	case tokensPending:
		return display + ", excludes the turn in progress"
	default:
		return display
	}
}

type dashboardRetryEntry struct {
	Identifier string
	Attempt    int
	DueIn      string
	Error      string
}

// dashboardBudgetEntry is one pre-formatted row of the budget-blocked
// table.
type dashboardBudgetEntry struct {
	Identifier string
	Reason     string
	Used       string
	BlockedFor string
	DetailURL  string
}

// budgetReasonLabels maps a machine-readable budget reason to its display
// label. A reason absent from the map renders as its raw value, keeping the
// mapping open to a later reason.
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

// countedNote renders n as a singular or plural %d-format sentence, or
// empty when n <= 0.
func countedNote(n int64, singular, plural string) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return fmt.Sprintf(singular, n)
	default:
		return fmt.Sprintf(plural, n)
	}
}

// formatRelativeTime formats dueAtMS (epoch milliseconds) relative to now:
// "overdue" if passed, "now" if within one second, else "in <duration>".
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

// buildDashboardData maps a [orchestrator.RuntimeSnapshotResult] into the
// template-ready [dashboardData].
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

	sortedRunning := make([]orchestrator.SnapshotRunningEntry, len(snap.Running))
	copy(sortedRunning, snap.Running)
	slices.SortFunc(sortedRunning, func(a, b orchestrator.SnapshotRunningEntry) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	running := make([]dashboardRunningEntry, len(sortedRunning))
	hasSSH := false
	hasRates := len(tokenRates) > 0
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

		var entryCostStr string
		if hasRates && e.UsageMeasured {
			if rc, ok := tokenRates[e.AgentKind]; ok {
				if c := EstimateCost(e.AgentInputTokens, e.AgentOutputTokens, e.CacheReadTokens, &rc); c != nil {
					entryCostStr = FormatCost(*c)
				}
			}
		}

		tokensStr := FormatInt(e.AgentTotalTokens)
		if e.CacheReadTokens != 0 {
			tokensStr += " (" + FormatInt(e.CacheReadTokens) + " cached)"
		}

		running[i] = dashboardRunningEntry{
			Identifier:        displayID,
			State:             e.State,
			TurnCount:         e.TurnCount,
			Duration:          FormatDuration(dur),
			LastEvent:         string(e.LastAgentEvent),
			TotalTokens:       e.AgentTotalTokens,
			CacheReadTokens:   e.CacheReadTokens,
			ModelName:         e.ModelName,
			DetailURL:         "/api/v1/" + url.PathEscape(e.Identifier),
			Host:              e.SSHHost,
			ToolTimePct:       toolPct,
			APITimePct:        apiPct,
			WorkflowFile:      e.WorkflowFile,
			EstimatedCostUSD:  entryCostStr,
			UsageMeasured:     e.UsageMeasured,
			UsageReportingRow: usageReportingRow(e.UsageArrival, e.UsageAttribution),
			ModelRow:          usageModelRow(e.UsageAttribution, e.ModelName),
			APIRequestsRow: usageAPIRequestsRow(
				e.UsageArrival, e.APIRequestsMeasured, e.APIRequestCount, e.RequestsByModel),
			TokensRow:  usageTokensRow(e.UsageArrival, e.UsageMeasured, e.TokensAwaited, e.TokensPending, tokensStr),
			EstCostRow: usageEstCostRow(e.UsageArrival, hasRates, e.TokensPending, entryCostStr),
		}
	}
	data.Running = running
	data.HasSSH = hasSSH
	data.HasTokenRates = hasRates
	data.RunningUnreportedNote = countedNote(int64(snap.AgentTotals.RunningUnreported),
		"%d running session has not reported token usage yet; the totals above exclude it.",
		"%d running sessions have not reported token usage yet; the totals above exclude them.")
	data.RunningNonReportingNote = countedNote(int64(snap.AgentTotals.RunningNonReporting),
		"%d running session runs an agent that reports no token usage; the totals above exclude it.",
		"%d running sessions run agents that report no token usage; the totals above exclude them.")
	data.EndedUnmeasuredNote = countedNote(snap.AgentTotals.UnmeasuredSessions,
		"%d session that already ended never reported token usage; the totals above exclude it too.",
		"%d sessions that already ended never reported token usage; the totals above exclude them too.")

	aggregateCost, aggregateCostSet, unpricedCount := activeCostTotal(sortedRunning, tokenRates)
	if hasRates {
		data.CostUnpricedNote = countedNote(int64(unpricedCount),
			"%d running session is excluded from Est. Cost because token_rates has no price for its agent.",
			"%d running sessions are excluded from Est. Cost because token_rates has no price for their agents.")
		data.EstimatedCostLabel = "Active Est. Cost (USD)"
	}
	if aggregateCostSet {
		data.EstimatedCostUSD = new(FormatCost(aggregateCost))
	}

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

	// snap.BudgetExhausted is already sorted by Identifier, unlike Running
	// and Retrying above.
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

// mapRunHistoryEntries converts [RunHistoryEntry] values into
// [dashboardRunHistoryEntry] with pre-formatted fields. Duration is computed
// when StartedAt and CompletedAt are both valid RFC 3339 strings.
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
			wf = "\u2014"
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
