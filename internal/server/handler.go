package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/sentence"
)

// stateResponse is the JSON wire format for GET /api/v1/state.
type stateResponse struct {
	GeneratedAt            time.Time                            `json:"generated_at"`
	Version                string                               `json:"version"`
	StartedAt              *time.Time                           `json:"started_at"`
	Counts                 stateCounts                          `json:"counts"`
	Running                []runningEntryResponse               `json:"running"`
	Retrying               []retryEntryResponse                 `json:"retrying"`
	BudgetExhausted        []budgetExhaustedResponse            `json:"budget_exhausted"`
	Parked                 []parkedEntryResponse                `json:"parked"`
	PendingReactions       []pendingReactionResponse            `json:"pending_reactions"`
	BetweenStages          []stageHopResponse                   `json:"between_stages"`
	Active                 []activeIssueResponse                `json:"active"`
	Held                   []heldIssueResponse                  `json:"held"`
	Attention              []attentionItemResponse              `json:"attention"`
	AgentTotals            orchestrator.SnapshotAgentTotals     `json:"agent_totals"`
	RateLimits             map[string]agentUsageWindowsResponse `json:"rate_limits"`
	ActiveEstimatedCostUSD *float64                             `json:"active_estimated_cost_usd,omitempty"`

	// CostUnpricedRunning counts running, measured sessions
	// ActiveEstimatedCostUSD excludes for lacking a configured rate.
	// Present, zero included, when any agent kind has a rate
	// configured; omitted otherwise.
	CostUnpricedRunning *int `json:"cost_unpriced_running,omitempty"`

	// SpendCeiling carries the instance spend windows. Omitted while no
	// instance spend window is set.
	SpendCeiling *spendCeilingResponse `json:"spend_ceiling,omitempty"`

	Config *configResponse `json:"config,omitempty"`

	Health *stateHealthResponse `json:"health,omitempty"`
}

type stateCounts struct {
	Running          int `json:"running"`
	Retrying         int `json:"retrying"`
	BudgetExhausted  int `json:"budget_exhausted"`
	Parked           int `json:"parked"`
	PendingReactions int `json:"pending_reactions"`
	BetweenStages    int `json:"between_stages"`
	Held             int `json:"held"`
	Attention        int `json:"attention"`
}

type runningEntryResponse struct {
	IssueID             string                 `json:"issue_id"`
	IssueIdentifier     string                 `json:"issue_identifier"`
	DisplayIdentifier   string                 `json:"display_identifier,omitempty"`
	IssueTitle          string                 `json:"issue_title,omitempty"`
	IssueURL            string                 `json:"issue_url,omitempty"`
	IssueLabels         []string               `json:"issue_labels"`
	IssueType           string                 `json:"issue_type,omitempty"`
	State               string                 `json:"state"`
	SessionID           string                 `json:"session_id"`
	TurnCount           int                    `json:"turn_count"`
	SelfReviewActive    bool                   `json:"self_review_active"`
	SelfReviewIteration int                    `json:"self_review_iteration"`
	LastEvent           string                 `json:"last_event"`
	LastMessage         string                 `json:"last_message"`
	StartedAt           time.Time              `json:"started_at"`
	DispatchedAt        *time.Time             `json:"dispatched_at"`
	LastEventAt         *time.Time             `json:"last_event_at"`
	WorkspacePath       string                 `json:"workspace_path"`
	SSHHost             string                 `json:"ssh_host,omitempty"`
	Tokens              tokenInfo              `json:"tokens"`
	UncachedTokens      *int64                 `json:"uncached_tokens"`
	ModelName           string                 `json:"model_name,omitempty"`
	RuleName            string                 `json:"rule_name,omitempty"`
	StageLineage        *stageLineageResponse  `json:"stage_lineage"`
	StagePosition       *stagePositionResponse `json:"stage_position"`
	ConfiguredModel     string                 `json:"configured_model,omitempty"`
	ConfiguredEffort    string                 `json:"configured_effort,omitempty"`
	WorkflowFile        string                 `json:"workflow_file,omitempty"`
	AgentKind           string                 `json:"agent_kind,omitempty"`
	ReactionKind        string                 `json:"reaction_kind,omitempty"`
	ReactionConfigKey   string                 `json:"reaction_config_key,omitempty"`
	ReactionAttempt     int                    `json:"reaction_attempt,omitempty"`
	PullRequest         *pullRequestResponse   `json:"pull_request,omitempty"`
	APIRequestCount     *int                   `json:"api_request_count"`
	RequestsByModel     map[string]int         `json:"requests_by_model,omitempty"`
	Skills              []domain.SkillCall     `json:"skills,omitempty"`
	ToolTimePercent     *float64               `json:"tool_time_percent"`
	APITimePercent      *float64               `json:"api_time_percent"`
	TokensMeasured      bool                   `json:"tokens_measured"`

	// UsageArrival and UsageAttribution mirror
	// [orchestrator.SnapshotRunningEntry]'s frozen pair verbatim, as
	// their string values. TokensPending derives from that frozen
	// pair; APIRequestsMeasured is the session-resolved verdict the
	// snapshot carries, and it is true exactly when APIRequestCount
	// is non-nil.
	UsageArrival        string `json:"usage_arrival"`
	UsageAttribution    string `json:"usage_attribution"`
	TokensPending       bool   `json:"tokens_pending"`
	TokensAwaited       bool   `json:"tokens_awaited"`
	APIRequestsMeasured bool   `json:"api_requests_measured"`

	// The issue-level figures below are nil when unknown. The budget keys
	// are nil when no limit is configured and are never omitted.
	// IssueTokensCompleted and UsedTokens count uncached tokens, the unit of
	// the token budget.
	UsedSessions          *int     `json:"used_sessions"`
	BudgetSessions        *int     `json:"budget_sessions"`
	IssueTokensCompleted  *int64   `json:"issue_tokens_completed"`
	UnmeasuredSessions    *int     `json:"unmeasured_sessions"`
	UsedTokens            *int64   `json:"used_tokens"`
	UsedTokensComplete    bool     `json:"used_tokens_complete"`
	BudgetTokens          *int64   `json:"budget_tokens"`
	BudgetUSD             *float64 `json:"budget_usd"`
	EstimatedCostUSD      *float64 `json:"estimated_cost_usd"`
	CostStatus            string   `json:"cost_status"`
	IssueEstimatedCostUSD *float64 `json:"issue_estimated_cost_usd"`
	IssueCostStatus       string   `json:"issue_cost_status"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`

	// DispatchID keys the run's retained events.
	DispatchID string `json:"dispatch_id"`

	// QuietAt and StallAt are half and all of agent.stall_timeout_ms after
	// the last event, else after the start; both are null with stall
	// detection off. StallText and StallTextFull are null until QuietAt
	// has passed and with stall detection off, and StallOffText is null
	// while it is on.
	QuietAt       *time.Time `json:"quiet_at"`
	StallAt       *time.Time `json:"stall_at"`
	StallText     *string    `json:"stall_text"`
	StallTextFull *string    `json:"stall_text_full"`
	StallOffText  *string    `json:"stall_off_text"`
}

// tokenInfo carries a running row's five token figures. The members
// are nil together, exactly when the row's TokensMeasured is false,
// and a nil member serializes as JSON null rather than being omitted,
// so a consumer can tell a measured zero from an unmeasured figure.
type tokenInfo struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type retryEntryResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	DispatchedAt      *time.Time `json:"dispatched_at"`
	Attempt           int        `json:"attempt"`
	DueAt             time.Time  `json:"due_at"`
	Error             string     `json:"error"`

	Trigger           string                 `json:"trigger,omitempty"`
	ReactionKind      string                 `json:"reaction_kind,omitempty"`
	ReactionConfigKey string                 `json:"reaction_config_key,omitempty"`
	RuleName          string                 `json:"rule_name,omitempty"`
	AgentKind         string                 `json:"agent_kind,omitempty"`
	ConfiguredModel   string                 `json:"configured_model,omitempty"`
	StageLineage      *stageLineageResponse  `json:"stage_lineage"`
	StagePosition     *stagePositionResponse `json:"stage_position"`
	Paused            bool                   `json:"paused"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// stageLineageResponse is the stage hop that reached the run a row
// describes. It is nil in the wire format unless a hop reached that run.
type stageLineageResponse struct {
	ChainID         string `json:"chain_id,omitempty"`
	Previous        string `json:"previous,omitempty"`
	PreviousOutcome string `json:"previous_outcome,omitempty"`
}

// stagePositionResponse is where a row's rule stands on its stage chain. It
// is nil in the wire format when the rule is on no chain.
type stagePositionResponse struct {
	Chain      []string `json:"chain"`
	Current    string   `json:"current"`
	HopCount   int      `json:"hop_count"`
	HopCeiling int      `json:"hop_ceiling"`
}

// stageHopResponse is the JSON wire format for one issue between stages.
// Used by GET /api/v1/state and GET /api/v1/{identifier}.
type stageHopResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	SourceRule        string     `json:"source_rule"`
	TargetRule        string     `json:"target_rule"`
	TargetLabel       string     `json:"target_label"`
	HoppedAt          *time.Time `json:"hopped_at"`
	DispatchedAt      *time.Time `json:"dispatched_at"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	Hold *holdResponse `json:"hold"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// holdBlockerResponse is the JSON wire format for one blocker that holds a
// candidate.
type holdBlockerResponse struct {
	IssueID           string `json:"issue_id"`
	IssueIdentifier   string `json:"issue_identifier"`
	DisplayIdentifier string `json:"display_identifier,omitempty"`
	State             string `json:"state,omitempty"`
}

// holdSlotResponse is the JSON wire format for the agent slot limit that held
// a candidate.
type holdSlotResponse struct {
	Scope string `json:"scope"`
	State string `json:"state,omitempty"`
	Used  int    `json:"used"`
	Limit int    `json:"limit"`
}

// holdResponse is the JSON wire format for why a candidate is held back.
// Reason is one of the values of [orchestrator.HoldReasons]. Blockers is
// non-null exactly for a blocked_by hold, Slot exactly for an agent_slots_full
// hold, and ResetsAt exactly for a spend ceiling hold or a usage_limit hold.
// AgentKind, ModelOnly, and Model are served for a usage_limit hold only. A
// client MUST tolerate a Reason it does not know and show the served texts.
// Used by GET /api/v1/state and GET /api/v1/{identifier}.
type holdResponse struct {
	Reason      string                `json:"reason"`
	HeldSince   time.Time             `json:"held_since"`
	Blockers    []holdBlockerResponse `json:"blockers"`
	Slot        *holdSlotResponse     `json:"slot"`
	ResetsAt    *time.Time            `json:"resets_at"`
	AgentKind   string                `json:"agent_kind,omitempty"`
	ModelOnly   bool                  `json:"model_only,omitempty"`
	Model       string                `json:"model,omitempty"`
	ReasonText  string                `json:"reason_text"`
	ReleaseText string                `json:"release_text"`
}

// agentUsageWindowsResponse is the JSON wire format for one agent kind's
// usage windows.
type agentUsageWindowsResponse struct {
	ReportedAt time.Time             `json:"reported_at"`
	Windows    []usageWindowResponse `json:"windows"`
}

// usageWindowResponse is the JSON wire format for one provider usage window.
// A figure the agent did not report serves as null, never as zero.
type usageWindowResponse struct {
	Label       string     `json:"label"`
	UsedPercent *float64   `json:"used_percent"`
	Status      *string    `json:"status"`
	ResetsAt    *time.Time `json:"resets_at"`
	ReportedAt  time.Time  `json:"reported_at"`

	Amount *usageWindowAmountResponse `json:"amount"`
}

// usageWindowAmountResponse is the JSON wire format for a window's use in the
// vendor's own unit, never converted.
type usageWindowAmountResponse struct {
	Used  float64 `json:"used"`
	Limit float64 `json:"limit"`
	Unit  string  `json:"unit"`
}

// heldIssueResponse is the JSON wire format for an eligible candidate the
// latest dispatch pass held back. Used by GET /api/v1/state.
type heldIssueResponse struct {
	IssueID           string       `json:"issue_id"`
	IssueIdentifier   string       `json:"issue_identifier"`
	DisplayIdentifier string       `json:"display_identifier,omitempty"`
	IssueTitle        string       `json:"issue_title,omitempty"`
	IssueURL          string       `json:"issue_url,omitempty"`
	IssueType         string       `json:"issue_type,omitempty"`
	IssueLabels       []string     `json:"issue_labels,omitzero"`
	State             string       `json:"state,omitempty"`
	Hold              holdResponse `json:"hold"`
	StatusText        string       `json:"status_text"`
	StatusTextFull    string       `json:"status_text_full"`
}

// parkedEntryResponse is the JSON wire format for one issue held out of
// primary dispatch until a person acts on it. Used by GET /api/v1/state and
// GET /api/v1/{identifier}.
type parkedEntryResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	DispatchedAt      *time.Time `json:"dispatched_at"`
	Reason            string     `json:"reason"`
	ParkedState       string     `json:"parked_state,omitempty"`
	Label             string     `json:"label"`
	LabelApplied      bool       `json:"label_applied"`
	ParkedAt          time.Time  `json:"parked_at"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// pendingReactionResponse is the JSON wire format for one pending pull
// request reaction. Used by GET /api/v1/state and GET /api/v1/{identifier}.
type pendingReactionResponse struct {
	IssueID           string                 `json:"issue_id"`
	IssueIdentifier   string                 `json:"issue_identifier"`
	DisplayIdentifier string                 `json:"display_identifier,omitempty"`
	IssueTitle        string                 `json:"issue_title,omitempty"`
	IssueURL          string                 `json:"issue_url,omitempty"`
	IssueType         string                 `json:"issue_type,omitempty"`
	IssueLabels       []string               `json:"issue_labels,omitzero"`
	State             string                 `json:"state,omitempty"`
	Kind              string                 `json:"kind"`
	ConfigKey         string                 `json:"config_key"`
	PullRequest       pullRequestResponse    `json:"pull_request"`
	CreatedAt         time.Time              `json:"created_at"`
	DispatchedAt      *time.Time             `json:"dispatched_at"`
	NextPollAt        time.Time              `json:"next_poll_at"`
	Wait              string                 `json:"wait,omitempty"`
	UsedAttempts      *int                   `json:"used_attempts"`
	MaxAttempts       *int                   `json:"max_attempts"`
	EscalatedAt       *time.Time             `json:"escalated_at"`
	EscalationTrigger string                 `json:"escalation_trigger,omitempty"`
	WatchEndsAt       *time.Time             `json:"watch_ends_at"`
	LastCheck         *reactionCheckResponse `json:"last_check"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// attentionItemResponse is the JSON wire format for one thing that needs a
// person. Used by GET /api/v1/state.
type attentionItemResponse struct {
	ID                string                    `json:"id"`
	Kind              string                    `json:"kind"`
	Severity          string                    `json:"severity"`
	Since             *time.Time                `json:"since"`
	AcknowledgedAt    *time.Time                `json:"acknowledged_at"`
	IssueID           string                    `json:"issue_id,omitempty"`
	IssueIdentifier   string                    `json:"issue_identifier,omitempty"`
	DisplayIdentifier string                    `json:"display_identifier,omitempty"`
	IssueTitle        string                    `json:"issue_title,omitempty"`
	IssueURL          string                    `json:"issue_url,omitempty"`
	ReasonText        string                    `json:"reason_text"`
	WhyText           string                    `json:"why_text"`
	ClearsText        string                    `json:"clears_text"`
	Action            attentionActionResponse   `json:"action"`
	Evidence          attentionEvidenceResponse `json:"evidence"`
}

type attentionActionResponse struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
}

// spendCeilingResponse is the JSON wire format of the instance spend windows;
// a window whose setting is 0 is null.
type spendCeilingResponse struct {
	Daily   *spendWindowResponse `json:"daily"`
	Weekly  *spendWindowResponse `json:"weekly"`
	Monthly *spendWindowResponse `json:"monthly"`
}

// spendWindowResponse is the latest evaluation of one spend window.
type spendWindowResponse struct {
	BudgetUSD       float64   `json:"budget_usd"`
	UsedUSD         float64   `json:"used_usd"`
	UsedUSDComplete bool      `json:"used_usd_complete"`
	HoldsDispatch   bool      `json:"holds_dispatch"`
	ResetsAt        time.Time `json:"resets_at"`
}

// attentionEvidenceResponse carries each member only on the item kinds that
// have it.
type attentionEvidenceResponse struct {
	Reason             string                 `json:"reason,omitempty"`
	Label              string                 `json:"label,omitempty"`
	UsedSessions       *int                   `json:"used_sessions,omitempty"`
	BudgetSessions     *int                   `json:"budget_sessions,omitempty"`
	UsedTokens         *int64                 `json:"used_tokens,omitempty"`
	UsedTokensComplete *bool                  `json:"used_tokens_complete,omitempty"`
	BudgetTokens       *int64                 `json:"budget_tokens,omitempty"`
	UsedUSD            *float64               `json:"used_usd,omitempty"`
	UsedUSDComplete    *bool                  `json:"used_usd_complete,omitempty"`
	BudgetUSD          *float64               `json:"budget_usd,omitempty"`
	ResetsAt           *time.Time             `json:"resets_at,omitempty"`
	ReactionKind       string                 `json:"reaction_kind,omitempty"`
	ConfigKey          string                 `json:"config_key,omitempty"`
	PullRequest        *pullRequestResponse   `json:"pull_request,omitempty"`
	EscalationTrigger  string                 `json:"escalation_trigger,omitempty"`
	UsedAttempts       *int                   `json:"used_attempts,omitempty"`
	MaxAttempts        *int                   `json:"max_attempts,omitempty"`
	LastCheck          *reactionCheckResponse `json:"last_check,omitempty"`
	File               string                 `json:"file,omitempty"`
	Error              string                 `json:"error,omitempty"`
}

// pullRequestResponse is the JSON wire format for the pull request a pending
// reaction watches.
type pullRequestResponse struct {
	Number     int    `json:"number"`
	Repository string `json:"repository"`
	URL        string `json:"url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
}

// reactionCheckResponse is the JSON wire format for what a pending
// reaction's latest check observed. It serializes the members of the kind's
// result as one flat object, which struct tags cannot express: a count of
// zero and a mergeable of null must appear for the kinds that record them
// and not at all for the others.
type reactionCheckResponse orchestrator.ReactionCheck

type reactionCheckHeader struct {
	ObservedAt time.Time `json:"observed_at"`
	HeadSHA    string    `json:"head_sha,omitempty"`
}

// MarshalJSON writes observed_at, then head_sha when the check read a head,
// then the members of the one result the check holds.
func (c reactionCheckResponse) MarshalJSON() ([]byte, error) {
	header := reactionCheckHeader{ObservedAt: c.ObservedAt.UTC(), HeadSHA: c.HeadSHA}
	switch {
	case c.CI != nil:
		failingChecks := c.CI.FailingChecks
		if failingChecks == nil {
			failingChecks = []string{}
		}
		return json.Marshal(struct {
			reactionCheckHeader
			Status        string   `json:"status"`
			FailingChecks []string `json:"failing_checks"`
			Failing       int      `json:"failing"`
			Pending       int      `json:"pending"`
			Passing       int      `json:"passing"`
		}{header, c.CI.Status, failingChecks, c.CI.Failing, c.CI.Pending, c.CI.Passing})
	case c.Comments != nil:
		return json.Marshal(struct {
			reactionCheckHeader
			ActionableComments int  `json:"actionable_comments"`
			NewComments        int  `json:"new_comments"`
			Handled            bool `json:"handled,omitempty"`
			AwaitingApproval   bool `json:"awaiting_approval,omitempty"`
		}{header, c.Comments.Actionable, c.Comments.New, c.Comments.Handled, c.Comments.AwaitingApproval})
	case c.Mergeability != nil:
		return json.Marshal(struct {
			reactionCheckHeader
			Mergeable *bool `json:"mergeable"`
		}{header, c.Mergeability.Mergeable})
	default:
		return json.Marshal(header)
	}
}

// budgetExhaustedResponse is the JSON wire format for one issue held out
// of dispatch by a budget ceiling. Used by GET /api/v1/state and
// GET /api/v1/{identifier}.
type budgetExhaustedResponse struct {
	IssueID            string                 `json:"issue_id"`
	IssueIdentifier    string                 `json:"issue_identifier"`
	DisplayIdentifier  string                 `json:"display_identifier,omitempty"`
	IssueTitle         string                 `json:"issue_title,omitempty"`
	IssueURL           string                 `json:"issue_url,omitempty"`
	IssueType          string                 `json:"issue_type,omitempty"`
	IssueLabels        []string               `json:"issue_labels,omitzero"`
	State              string                 `json:"state,omitempty"`
	DispatchedAt       *time.Time             `json:"dispatched_at"`
	Reason             string                 `json:"reason"`
	UsedSessions       int                    `json:"used_sessions"`
	BudgetSessions     *int                   `json:"budget_sessions"`
	UsedTokens         *int64                 `json:"used_tokens"`
	BudgetTokens       *int64                 `json:"budget_tokens"`
	UsedUSD            *float64               `json:"used_usd"`
	UsedUSDComplete    *bool                  `json:"used_usd_complete"`
	BudgetUSD          *float64               `json:"budget_usd"`
	UnmeasuredSessions *int                   `json:"unmeasured_sessions"`
	StoppedInFlight    *int                   `json:"stopped_in_flight"`
	ExhaustedAt        time.Time              `json:"exhausted_at"`
	RuleName           string                 `json:"rule_name,omitempty"`
	StageLineage       *stageLineageResponse  `json:"stage_lineage"`
	StagePosition      *stagePositionResponse `json:"stage_position"`
	AgentKind          string                 `json:"agent_kind,omitempty"`
	ConfiguredModel    string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// issueResponse is the tracker's view of one issue, under the names the
// prompt template's issue variable uses. A member is omitted when its source
// holds none; Labels is nil only when the source holds no read of the labels.
type issueResponse struct {
	Title     string   `json:"title,omitempty"`
	URL       string   `json:"url,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
	Labels    []string `json:"labels,omitzero"`
	State     string   `json:"state,omitempty"`
}

type issueDetailResponse struct {
	IssueIdentifier  string                    `json:"issue_identifier"`
	IssueID          string                    `json:"issue_id"`
	Status           string                    `json:"status"`
	Issue            issueResponse             `json:"issue"`
	Workspace        *workspaceInfo            `json:"workspace"`
	Attempts         *attemptsInfo             `json:"attempts"`
	Running          *runningEntryResponse     `json:"running"`
	Retry            *retryEntryResponse       `json:"retry"`
	BudgetExhausted  *budgetExhaustedResponse  `json:"budget_exhausted"`
	Parked           *parkedEntryResponse      `json:"parked"`
	PendingReactions []pendingReactionResponse `json:"pending_reactions"`
	BetweenStages    *stageHopResponse         `json:"between_stages"`
	RecentEvents     []runEventResponse        `json:"recent_events"`
	LastError        *string                   `json:"last_error"`
	Tracked          map[string]any            `json:"tracked"`
	History          *issueHistoryResponse     `json:"history"`

	// PRLoop is what each configured pull request reaction recorded in the
	// pass that decides a live issue's status, or in the newest run's pass
	// of an issue that only run history knows; null without run history or a
	// workflow shape.
	PRLoop map[string]prLoopStationResponse `json:"pr_loop"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`

	NextOutcomes []nextOutcomeResponse `json:"next_outcomes"`
	situation    sentence.Situation    // recorded by buildIssueDetail; never serialized
}

// nextOutcomeResponse is the JSON wire format for one next outcome: a
// condition and what Sortie then does.
type nextOutcomeResponse struct {
	Condition string `json:"condition"`
	Text      string `json:"text"`
}

type workspaceInfo struct {
	Path string `json:"path"`
}

type attemptsInfo struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type refreshResponse struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
	OutcomeText string    `json:"outcome_text"`
}

type retryNowResponse struct {
	Queued            bool      `json:"queued"`
	RequestedAt       time.Time `json:"requested_at"`
	IssueID           string    `json:"issue_id,omitempty"`
	IssueIdentifier   string    `json:"issue_identifier,omitempty"`
	DisplayIdentifier string    `json:"display_identifier,omitempty"`
	Attempt           int       `json:"attempt,omitempty"`
	OutcomeText       string    `json:"outcome_text"`
}

// checkNowResponse is the body of a 202 and a 409 from
// POST /api/v1/reactions/{identifier}/{kind}/check.
type checkNowResponse struct {
	Queued            bool                 `json:"queued"`
	RequestedAt       time.Time            `json:"requested_at"`
	IssueID           string               `json:"issue_id,omitempty"`
	IssueIdentifier   string               `json:"issue_identifier,omitempty"`
	DisplayIdentifier string               `json:"display_identifier,omitempty"`
	Kind              string               `json:"kind,omitempty"`
	PullRequest       *pullRequestResponse `json:"pull_request,omitempty"`
	OutcomeText       string               `json:"outcome_text"`
}

// acknowledgeResponse is the body of a 200 and a 409 from
// POST /api/v1/attention/{id}/acknowledge.
type acknowledgeResponse struct {
	Acknowledged        bool       `json:"acknowledged"`
	AlreadyAcknowledged bool       `json:"already_acknowledged"`
	RequestedAt         time.Time  `json:"requested_at"`
	ID                  string     `json:"id,omitempty"`
	Kind                string     `json:"kind,omitempty"`
	IssueID             string     `json:"issue_id,omitempty"`
	IssueIdentifier     string     `json:"issue_identifier,omitempty"`
	DisplayIdentifier   string     `json:"display_identifier,omitempty"`
	AcknowledgedAt      *time.Time `json:"acknowledged_at,omitempty"`
	OutcomeText         string     `json:"outcome_text"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toRunningEntryResponse(e orchestrator.SnapshotRunningEntry, nowArgs ...time.Time) runningEntryResponse {
	resp := runningEntryResponse{
		IssueID:             e.IssueID,
		IssueIdentifier:     e.Identifier,
		DisplayIdentifier:   e.DisplayID,
		IssueTitle:          e.IssueTitle,
		IssueURL:            e.IssueURL,
		IssueLabels:         e.IssueLabels,
		IssueType:           e.IssueType,
		State:               e.State,
		SessionID:           e.SessionID,
		TurnCount:           e.TurnCount,
		SelfReviewActive:    e.SelfReviewActive,
		SelfReviewIteration: e.SelfReviewIteration,
		LastEvent:           string(e.LastAgentEvent),
		LastMessage:         e.LastAgentMessage,
		StartedAt:           e.StartedAt.UTC(),
		DispatchedAt:        utcOrNil(e.StageLineage.DispatchedAt),
		LastEventAt:         utcOrNil(e.LastAgentTimestamp),
		DispatchID:          e.DispatchID,
		QuietAt:             utcOrNil(e.QuietAt),
		StallAt:             utcOrNil(e.StallAt),
		WorkspacePath:       e.WorkspacePath,
		SSHHost:             e.SSHHost,
		ModelName:           e.ModelName,
		RuleName:            e.RuleName,
		StageLineage:        toStageLineageResponse(e.StageLineage),
		StagePosition:       toStagePositionResponse(e.StagePosition),
		ConfiguredModel:     e.ConfiguredModel,
		ConfiguredEffort:    e.ConfiguredEffort,
		WorkflowFile:        e.WorkflowFile,
		AgentKind:           e.AgentKind,
		ReactionKind:        e.ReactionKind,
		ReactionConfigKey:   e.ReactionConfigKey,
		RequestsByModel:     e.RequestsByModel,
		Skills:              e.Skills,
		TokensMeasured:      e.UsageMeasured,
		UsageArrival:        string(e.UsageArrival),
		UsageAttribution:    string(e.UsageAttribution),
		TokensPending:       e.TokensPending,
		TokensAwaited:       e.TokensAwaited,
		APIRequestsMeasured: e.APIRequestsMeasured,
	}

	// A figure no measurement produced is null in the place it already
	// occupies, beside the flag that explains it.
	if e.UsageMeasured {
		resp.Tokens = tokenInfo{
			InputTokens:      &e.AgentInputTokens,
			OutputTokens:     &e.AgentOutputTokens,
			TotalTokens:      &e.AgentTotalTokens,
			CacheReadTokens:  &e.CacheReadTokens,
			CacheWriteTokens: &e.CacheWriteTokens,
		}
		resp.UncachedTokens = new(e.UncachedTokens())
	}
	if e.APIRequestsMeasured {
		resp.APIRequestCount = &e.APIRequestCount
	}
	if e.Continuation != nil {
		resp.ReactionAttempt = e.Continuation.Attempt
		resp.PullRequest = &pullRequestResponse{
			Number:     e.Continuation.Number,
			Repository: e.Continuation.Repository,
		}
	}

	if len(nowArgs) > 0 && !e.StartedAt.IsZero() {
		now := nowArgs[0]
		elapsedMs := now.Sub(e.StartedAt).Milliseconds()
		if elapsedMs > 0 && e.ToolTimeMs > 0 {
			resp.ToolTimePercent = new(float64(e.ToolTimeMs) / float64(elapsedMs) * 100.0)
		}
		if elapsedMs > 0 && e.APITimeMs > 0 {
			resp.APITimePercent = new(float64(e.APITimeMs) / float64(elapsedMs) * 100.0)
		}
	}

	return resp
}

// toServedRunningEntry converts a running row for both endpoints, adding
// the issue-level figures that depend on the snapshot's limits and on the
// cost recorded for the issue's completed sessions.
func toServedRunningEntry(e orchestrator.SnapshotRunningEntry, snap orchestrator.RuntimeSnapshotResult) runningEntryResponse {
	resp := toRunningEntryResponse(e, snap.GeneratedAt)

	var completed int64
	var completedIncomplete bool
	if usage := e.IssueUsage; usage != nil {
		completed = usage.UncachedTokens
		completedIncomplete = orchestrator.SpendIncomplete(usage.UnmeasuredSessions, usage.UnaccountedTurns)
		resp.UsedSessions = new(usage.Sessions)
		resp.UnmeasuredSessions = new(usage.UnmeasuredSessions)
	}

	known := completed
	if e.UsageMeasured {
		known += e.UncachedTokens()
	}
	resp.UsedTokensComplete = e.IssueUsage != nil && !completedIncomplete && e.UsageMeasured && e.UnaccountedTurns == 0

	// A zero that leaves spend out would stand in for spend Sortie does not
	// know, so it is served as null.
	if e.IssueUsage != nil && (completed != 0 || !completedIncomplete) {
		resp.IssueTokensCompleted = new(completed)
	}
	if known != 0 || resp.UsedTokensComplete {
		resp.UsedTokens = new(known)
	}

	if snap.MaxTokens > 0 {
		resp.BudgetTokens = new(int64(snap.MaxTokens))
	}
	if snap.MaxCostUSD > 0 {
		resp.BudgetUSD = new(snap.MaxCostUSD)
	}
	if snap.MaxSessions > 0 {
		resp.BudgetSessions = new(snap.MaxSessions)
	}

	resp.EstimatedCostUSD, resp.CostStatus = liveCostOf(e)
	resp.IssueEstimatedCostUSD, resp.IssueCostStatus = issueCost(e.IssueUsage, resp.UsedTokens, resp.UsedTokensComplete, sessionPartOf(e))
	return resp
}

// utcOrNil returns the UTC value of at, or nil for the zero time, so a time
// no record could date serializes as null.
func utcOrNil(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return new(at.UTC())
}

// toStageLineageResponse returns nil unless a stage hop reached the run, so
// a chain identifier alone never makes a row claim a lineage.
func toStageLineageResponse(l orchestrator.StageLineage) *stageLineageResponse {
	if l.PreviousRule == "" {
		return nil
	}
	return &stageLineageResponse{
		ChainID:         l.ChainID,
		Previous:        l.PreviousRule,
		PreviousOutcome: l.PreviousOutcome,
	}
}

func toStagePositionResponse(p *orchestrator.SnapshotStagePosition) *stagePositionResponse {
	if p == nil {
		return nil
	}
	return &stagePositionResponse{
		Chain:      p.Chain,
		Current:    p.Current,
		HopCount:   p.HopCount,
		HopCeiling: p.HopCeiling,
	}
}

func toStageHopResponse(e orchestrator.SnapshotStageHop, words sentence.Context) stageHopResponse {
	return stageHopResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		SourceRule:        e.SourceRule,
		TargetRule:        e.TargetRule,
		TargetLabel:       e.TargetLabel,
		HoppedAt:          utcOrNil(e.HoppedAt),
		DispatchedAt:      utcOrNil(e.DispatchedAt),

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,

		Hold: toHoldResponse(e.Hold, words),
	}
}

// toHoldResponse returns the served view of a hold with its operator texts,
// or nil for no hold.
func toHoldResponse(h *orchestrator.SnapshotHold, words sentence.Context) *holdResponse {
	if h == nil {
		return nil
	}
	resp := &holdResponse{
		Reason:    string(h.Reason),
		HeldSince: h.Since.UTC(),
		ResetsAt:  utcOrNil(h.ResetsAt),
		AgentKind: h.AgentKind,
		ModelOnly: h.ModelOnly,
		Model:     h.Model,
	}
	if h.Blockers != nil {
		resp.Blockers = make([]holdBlockerResponse, 0, len(h.Blockers))
		for _, blocker := range h.Blockers {
			resp.Blockers = append(resp.Blockers, holdBlockerResponse{
				IssueID:           blocker.IssueID,
				IssueIdentifier:   blocker.Identifier,
				DisplayIdentifier: blocker.DisplayID,
				State:             blocker.State,
			})
		}
	}
	if h.Slot != nil {
		resp.Slot = &holdSlotResponse{
			Scope: string(h.Slot.Scope),
			State: h.Slot.State,
			Used:  h.Slot.Used,
			Limit: h.Slot.Limit,
		}
	}
	text := sentence.Hold(h, words)
	resp.ReasonText, resp.ReleaseText = text.Reason, text.Release
	return resp
}

func toHeldIssueResponse(e orchestrator.SnapshotHeldEntry, words sentence.Context) heldIssueResponse {
	row := heldIssueResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		Hold:              *toHoldResponse(&e.Hold, words),
	}
	row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Held: &e}, words)
	return row
}

func toRetryEntryResponse(e orchestrator.SnapshotRetryEntry) retryEntryResponse {
	return retryEntryResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		DispatchedAt:      utcOrNil(e.StageLineage.DispatchedAt),
		Attempt:           e.Attempt,
		DueAt:             time.UnixMilli(e.DueAtMS).UTC(),
		Error:             sentence.RunError(e.Error),

		Trigger:           e.Trigger,
		ReactionKind:      e.ReactionKind,
		ReactionConfigKey: e.ReactionConfigKey,
		RuleName:          e.RuleName,
		AgentKind:         e.AgentKind,
		ConfiguredModel:   e.ConfiguredModel,
		StageLineage:      toStageLineageResponse(e.StageLineage),
		StagePosition:     toStagePositionResponse(e.StagePosition),
		Paused:            e.Paused,
	}
}

func toParkedEntryResponse(e orchestrator.SnapshotParkedEntry) parkedEntryResponse {
	return parkedEntryResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.ParkedState,
		DispatchedAt:      utcOrNil(e.DispatchedAt),
		Reason:            e.Reason,
		ParkedState:       e.ParkedState,
		Label:             e.Label,
		LabelApplied:      e.LabelApplied,
		ParkedAt:          e.ParkedAt.UTC(),

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,
	}
}

func toPendingReactionResponse(e orchestrator.SnapshotPendingReaction) pendingReactionResponse {
	resp := pendingReactionResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		Kind:              e.Kind,
		ConfigKey:         e.ConfigKey,
		PullRequest:       toPullRequestResponse(e.PullRequest),
		CreatedAt:         e.CreatedAt.UTC(),
		DispatchedAt:      utcOrNil(e.DispatchedAt),
		NextPollAt:        e.NextPollAt.UTC(),
		Wait:              e.Wait,
		UsedAttempts:      e.UsedAttempts,
		MaxAttempts:       e.MaxAttempts,
		LastCheck:         (*reactionCheckResponse)(e.LastCheck),
		EscalationTrigger: e.EscalationTrigger,

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,
	}
	if e.EscalatedAt != nil {
		resp.EscalatedAt = new(e.EscalatedAt.UTC())
	}
	if e.WatchEndsAt != nil {
		resp.WatchEndsAt = new(e.WatchEndsAt.UTC())
	}
	return resp
}

func toPullRequestResponse(pr orchestrator.SnapshotPullRequest) pullRequestResponse {
	return pullRequestResponse{
		Number:     pr.Number,
		Repository: pr.Repository,
		URL:        pr.URL,
		Branch:     pr.Branch,
		HeadSHA:    pr.HeadSHA,
	}
}

func countUnacknowledged(items []attentionItemResponse) int {
	count := 0
	for _, item := range items {
		if item.AcknowledgedAt == nil {
			count++
		}
	}
	return count
}

// toAttentionResponses converts the derived list to its wire format, never nil.
func toAttentionResponses(items []sentence.AttentionItem) []attentionItemResponse {
	responses := make([]attentionItemResponse, 0, len(items))
	for _, item := range items {
		ev := item.Evidence
		resp := attentionItemResponse{
			ID:                item.ID,
			Kind:              item.Kind,
			Severity:          item.Severity,
			IssueID:           item.IssueID,
			IssueIdentifier:   item.IssueIdentifier,
			DisplayIdentifier: item.DisplayIdentifier,
			IssueTitle:        item.IssueTitle,
			IssueURL:          item.IssueURL,
			ReasonText:        item.ReasonText,
			WhyText:           item.WhyText,
			ClearsText:        item.ClearsText,
			Action:            attentionActionResponse{Kind: item.Action.Kind, URL: item.Action.URL},
			Evidence: attentionEvidenceResponse{
				Reason:             ev.Reason,
				Label:              ev.Label,
				UsedSessions:       ev.UsedSessions,
				BudgetSessions:     ev.BudgetSessions,
				UsedTokens:         ev.UsedTokens,
				UsedTokensComplete: ev.UsedTokensComplete,
				BudgetTokens:       ev.BudgetTokens,
				UsedUSD:            ev.UsedUSD,
				UsedUSDComplete:    ev.UsedUSDComplete,
				BudgetUSD:          ev.BudgetUSD,
				ReactionKind:       ev.ReactionKind,
				ConfigKey:          ev.ConfigKey,
				EscalationTrigger:  ev.EscalationTrigger,
				UsedAttempts:       ev.UsedAttempts,
				MaxAttempts:        ev.MaxAttempts,
				LastCheck:          (*reactionCheckResponse)(ev.LastCheck),
				File:               ev.File,
				Error:              ev.Error,
			},
		}
		if item.Since != nil {
			resp.Since = new(item.Since.UTC())
		}
		if item.AcknowledgedAt != nil {
			resp.AcknowledgedAt = new(item.AcknowledgedAt.UTC())
		}
		if ev.ResetsAt != nil {
			resp.Evidence.ResetsAt = new(ev.ResetsAt.UTC())
		}
		if ev.PullRequest != nil {
			resp.Evidence.PullRequest = new(toPullRequestResponse(*ev.PullRequest))
		}
		responses = append(responses, resp)
	}
	return responses
}

func toPendingReactionResponses(rows []orchestrator.SnapshotPendingReaction, words sentence.Context) []pendingReactionResponse {
	responses := make([]pendingReactionResponse, 0, len(rows))
	for _, e := range rows {
		row := toPendingReactionResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{PendingReaction: &e}, words)
		responses = append(responses, row)
	}
	return responses
}

// setRunningSentences sets the status pair, the stall pair, and the stall-off
// sentence of a running row.
func setRunningSentences(row *runningEntryResponse, e orchestrator.SnapshotRunningEntry, words sentence.Context) {
	row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Running: &e}, words)
	if stall, quiet := sentence.Stall(&e, words); quiet {
		row.StallText, row.StallTextFull = &stall.Short, &stall.Long
	}
	if text, off := sentence.StallOff(&e); off {
		row.StallOffText = &text
	}
}

// sentencesOf returns the short and long status sentence of one entry.
func sentencesOf(entry sentence.Entry, words sentence.Context) (short, long string) {
	status := sentence.For(entry, words)
	return status.Short, status.Long
}

func toBudgetExhaustedResponse(e orchestrator.SnapshotBudgetEntry) budgetExhaustedResponse {
	var budgetSessions *int
	if e.BudgetSessions > 0 {
		budgetSessions = new(e.BudgetSessions)
	}
	var budgetTokens *int64
	if e.BudgetTokens > 0 {
		budgetTokens = new(e.BudgetTokens)
	}
	var budgetUSD *float64
	if e.BudgetUSD > 0 {
		budgetUSD = new(e.BudgetUSD)
	}
	return budgetExhaustedResponse{
		IssueID:            e.IssueID,
		IssueIdentifier:    e.Identifier,
		DisplayIdentifier:  e.DisplayID,
		IssueTitle:         e.IssueTitle,
		IssueURL:           e.IssueURL,
		IssueType:          e.IssueType,
		IssueLabels:        e.IssueLabels,
		State:              e.State,
		DispatchedAt:       utcOrNil(e.StageLineage.DispatchedAt),
		Reason:             e.Reason,
		UsedSessions:       e.UsedSessions,
		BudgetSessions:     budgetSessions,
		UsedTokens:         heldUsedTokens(e),
		BudgetTokens:       budgetTokens,
		UsedUSD:            e.UsedUSD,
		UsedUSDComplete:    e.UsedUSDComplete,
		BudgetUSD:          budgetUSD,
		UnmeasuredSessions: e.UnmeasuredSessions,
		StoppedInFlight:    e.StoppedInFlight,
		ExhaustedAt:        e.ExhaustedAt,
		RuleName:           e.RuleName,
		StageLineage:       toStageLineageResponse(e.StageLineage),
		StagePosition:      toStagePositionResponse(e.StagePosition),
		AgentKind:          e.AgentKind,
		ConfiguredModel:    e.ConfiguredModel,
	}
}

// heldUsedTokens returns the held issue's token sum, or nil when the sum is
// unread or is a zero that leaves spend out: that zero would stand in for
// spend Sortie does not know.
func heldUsedTokens(e orchestrator.SnapshotBudgetEntry) *int64 {
	if e.UsedTokens == nil {
		return nil
	}
	var unmeasured int
	if e.UnmeasuredSessions != nil {
		unmeasured = *e.UnmeasuredSessions
	}
	if *e.UsedTokens == 0 && orchestrator.SpendIncomplete(unmeasured, e.UnaccountedTurns) {
		return nil
	}
	return e.UsedTokens
}

// toAgentUsageWindowsResponse maps one kind's snapshot windows to wire form.
func toAgentUsageWindowsResponse(member orchestrator.SnapshotUsageWindows) agentUsageWindowsResponse {
	windows := make([]usageWindowResponse, 0, len(member.Windows))
	for _, w := range member.Windows {
		resp := usageWindowResponse{
			Label:       w.Label,
			UsedPercent: w.UsedPercent,
			ReportedAt:  w.ReportedAt.UTC(),
		}
		if w.Status != "" {
			status := string(w.Status)
			resp.Status = &status
		}
		if !w.ResetsAt.IsZero() {
			resetsAt := w.ResetsAt.UTC()
			resp.ResetsAt = &resetsAt
		}
		if w.Amount != nil {
			resp.Amount = &usageWindowAmountResponse{
				Used:  w.Amount.Used,
				Limit: w.Amount.Limit,
				Unit:  w.Amount.Unit,
			}
		}
		windows = append(windows, resp)
	}
	return agentUsageWindowsResponse{ReportedAt: member.ReportedAt.UTC(), Windows: windows}
}

func toStateResponse(snap orchestrator.RuntimeSnapshotResult, words sentence.Context, health *orchestrator.SnapshotHealth) stateResponse {
	running := make([]runningEntryResponse, 0, len(snap.Running))
	for _, e := range snap.Running {
		row := toServedRunningEntry(e, snap)
		setRunningSentences(&row, e, words)
		running = append(running, row)
	}

	retrying := make([]retryEntryResponse, 0, len(snap.Retrying))
	for _, e := range snap.Retrying {
		row := toRetryEntryResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Retry: &e}, words)
		retrying = append(retrying, row)
	}

	budgetExhausted := make([]budgetExhaustedResponse, 0, len(snap.BudgetExhausted))
	for _, e := range snap.BudgetExhausted {
		row := toBudgetExhaustedResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{BudgetHold: &e}, words)
		budgetExhausted = append(budgetExhausted, row)
	}

	parked := make([]parkedEntryResponse, 0, len(snap.Parked))
	for _, e := range snap.Parked {
		row := toParkedEntryResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Parked: &e}, words)
		parked = append(parked, row)
	}

	pendingReactions := toPendingReactionResponses(snap.PendingReactions, words)

	betweenStages := make([]stageHopResponse, 0, len(snap.BetweenStages))
	for _, e := range snap.BetweenStages {
		row := toStageHopResponse(e, words)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{BetweenStages: &e}, words)
		betweenStages = append(betweenStages, row)
	}

	held := make([]heldIssueResponse, 0, len(snap.Held))
	for _, e := range snap.Held {
		held = append(held, toHeldIssueResponse(e, words))
	}

	attention := toAttentionResponses(sentence.Attention(snap, health, words))

	rateLimits := make(map[string]agentUsageWindowsResponse, len(snap.UsageWindows))
	for kind, member := range snap.UsageWindows {
		rateLimits[kind] = toAgentUsageWindowsResponse(member)
	}

	resp := stateResponse{
		GeneratedAt: snap.GeneratedAt.UTC(),
		Counts: stateCounts{
			Running:          len(running),
			Retrying:         len(retrying),
			BudgetExhausted:  len(budgetExhausted),
			Parked:           len(parked),
			PendingReactions: len(pendingReactions),
			BetweenStages:    len(betweenStages),
			Held:             len(held),
			Attention:        countUnacknowledged(attention),
		},
		Running:          running,
		Retrying:         retrying,
		BudgetExhausted:  budgetExhausted,
		Parked:           parked,
		PendingReactions: pendingReactions,
		BetweenStages:    betweenStages,
		Held:             held,
		Attention:        attention,
		AgentTotals:      snap.AgentTotals,
		RateLimits:       rateLimits,
	}
	resp.Active = activeIssuesOf(resp, snap.ActiveOrder)

	if snap.Rates.SourceAvailable() {
		total, anySet, unpriced := activeCostTotal(snap.Running)
		if anySet {
			resp.ActiveEstimatedCostUSD = &total
		}
		resp.CostUnpricedRunning = &unpriced
	}
	resp.SpendCeiling = spendCeilingOf(snap.SpendCeilings)

	return resp
}

func spendCeilingOf(statuses []orchestrator.SpendCeilingStatus) *spendCeilingResponse {
	if len(statuses) == 0 {
		return nil
	}
	var ceiling spendCeilingResponse
	for _, status := range statuses {
		window := &spendWindowResponse{
			BudgetUSD:       status.BudgetUSD,
			UsedUSD:         status.UsedUSD,
			UsedUSDComplete: status.UsedComplete,
			HoldsDispatch:   status.Holding,
			ResetsAt:        status.ResetsAt.UTC(),
		}
		switch status.Window {
		case orchestrator.SpendWindowDaily:
			ceiling.Daily = window
		case orchestrator.SpendWindowWeekly:
			ceiling.Weekly = window
		case orchestrator.SpendWindowMonthly:
			ceiling.Monthly = window
		default:
			panic("server: no spend ceiling member for window " + string(status.Window))
		}
	}
	return &ceiling
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		logger.Error("failed to marshal JSON response", slog.Any("error", err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		logger.Warn("failed to write JSON response", slog.Any("error", err))
	}
}

func writeErrorJSON(w http.ResponseWriter, logger *slog.Logger, status int, code string, message string) {
	writeJSON(w, logger, status, errorResponse{
		Error: errorDetail{Code: code, Message: message},
	})
}

func (s *Server) routeState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleState(w, r)
}

func (s *Server) routeRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleRefresh(w, r)
}

func (s *Server) routeRetryNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleRetryNow(w, r)
}

func (s *Server) routeAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleAcknowledge(w, r)
}

func (s *Server) routeCheckNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleCheckNow(w, r)
}

func (s *Server) routeIssueDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleIssueDetail(w, r)
}

// sentenceContext reads the workflow shape once for a request, so the
// sentences and the config the state route serves never disagree.
func (s *Server) sentenceContext(snap orchestrator.RuntimeSnapshotResult) sentence.Context {
	words := sentence.Context{Now: snap.GeneratedAt}
	if s.workflowShapeFn != nil {
		shape := s.workflowShapeFn()
		words.Shape = &shape
	}
	return words
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	snap := s.snapshotFn()

	var health *orchestrator.SnapshotHealth
	if s.healthFn != nil {
		health = new(s.healthFn())
	}

	words := s.sentenceContext(snap)
	resp := toStateResponse(snap, words, health)
	resp.Version = s.version
	resp.StartedAt = utcOrNil(s.startedAt)
	if words.Shape != nil {
		resp.Config = toConfigResponse(*words.Shape, snap.Rates)
	}
	if health != nil {
		resp.Health = toStateHealthResponse(*health, words)
	}
	s.attachActivePRLoops(r, &resp, snap, words.Shape)
	writeJSON(w, s.logger, http.StatusOK, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	identifier := r.PathValue("identifier")
	if identifier == "" {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			"issue identifier is empty")
		return
	}

	snap := s.snapshotFn()

	words := s.sentenceContext(snap)
	detail := buildIssueDetail(identifier, "", snap, words)
	notFoundIn := "current state"
	if s.runHistory != nil {
		notFoundIn = "current state or run history"
		var ok bool
		if detail, ok = s.withIssueHistory(w, r, identifier, snap, detail, words); !ok {
			return
		}
	}
	if detail == nil {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			fmt.Sprintf("issue identifier %q not found in %s", identifier, notFoundIn))
		return
	}
	recentEvents, err := s.recentEvents(r.Context(), detail)
	if err != nil {
		s.historyReadFailed(w, r, err)
		return
	}
	detail.RecentEvents = recentEvents

	for _, outcome := range sentence.NextOutcomes(detail.situation, words) {
		detail.NextOutcomes = append(detail.NextOutcomes, nextOutcomeResponse(outcome))
	}
	writeJSON(w, s.logger, http.StatusOK, detail)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

// withIssueHistory resolves identifier through run history and attaches the
// issue's history to detail, the live detail or nil. It returns a nil detail
// for an identifier history does not know either, and false after it has
// answered a failed read itself. The snapshot is read before run history, so
// a run that ends between the reads can appear in both but never in neither.
func (s *Server) withIssueHistory(w http.ResponseWriter, r *http.Request, identifier string, snap orchestrator.RuntimeSnapshotResult, detail *issueDetailResponse, words sentence.Context) (*issueDetailResponse, bool) {
	ctx := r.Context()

	var issueID string
	if detail != nil {
		issueID = detail.IssueID
	} else {
		newest, err := s.runHistory.QueryRuns(ctx, RunHistoryQuery{Identifier: identifier, Limit: 1})
		if err != nil {
			s.historyReadFailed(w, r, err)
			return nil, false
		}
		if len(newest) == 0 {
			return nil, true
		}
		issueID = newest[0].IssueID
		detail = buildIssueDetail(identifier, issueID, snap, words)
	}

	page, err := s.runHistory.QueryRuns(ctx, RunHistoryQuery{IssueID: issueID, Limit: issueHistoryPageSize + 1})
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	if len(page) == 0 {
		if detail != nil {
			detail.situation.Spend = &sentence.Spend{CostUSD: issueCostUSD(nil, detail.situation.Running)}
			if err := s.attachIssuePRLoop(ctx, detail, snap, words.Shape, nil); err != nil {
				s.historyReadFailed(w, r, err)
				return nil, false
			}
		}
		return detail, true
	}

	totals, err := s.runHistory.IssueTotals(ctx, issueID)
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	outcomes, err := s.runHistory.IssueOutcomes(ctx, []string{issueID})
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	newest := page[0]
	outcome, hasOutcome := outcomes[issueID]
	if detail == nil {
		detail = historyOnlyDetail(issueID, newest)
		detail.StatusText, detail.StatusTextFull = sentencesOf(sentence.Entry{Finished: finishedOf(newest, outcome, hasOutcome)}, words)
	}
	detail.situation.Spend = &sentence.Spend{
		Sessions: totals.Usage.Sessions,
		Tokens:   totals.Usage.UncachedTokens,
		CostUSD:  issueCostUSD(&totals.Usage, detail.situation.Running),
	}
	var chains [][]string
	if words.Shape != nil {
		chains = words.Shape.Dispatch.Chains
	}
	detail.History = issueHistoryOf(page, totals, chains)
	attachIssueOutcomes(detail.History.Runs, outcomes)
	if hasOutcome {
		detail.History.IssueOutcome = toIssueOutcomeResponse(outcome, newest.Identifier, newest.DisplayID)
	}
	if err := s.attachIssuePRLoop(ctx, detail, snap, words.Shape, &newest); err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	return detail, true
}

// historyOnlyDetail is the response for an issue that only run history
// knows: no live member, and the title and link of its newest run.
func historyOnlyDetail(issueID string, newest RunHistoryEntry) *issueDetailResponse {
	return &issueDetailResponse{
		IssueIdentifier:  newest.Identifier,
		IssueID:          issueID,
		Status:           "history",
		Issue:            issueResponse{Title: redact.Mask(newest.IssueTitle), URL: redact.Mask(newest.IssueURL)},
		Attempts:         &attemptsInfo{},
		PendingReactions: []pendingReactionResponse{},
		RecentEvents:     []runEventResponse{},
		Tracked:          map[string]any{},
		NextOutcomes:     []nextOutcomeResponse{},
	}
}

// issueHistoryOf builds the history member from an issue's newest runs, read
// one past issueHistoryPageSize so a longer history names its continuation.
func issueHistoryOf(page []RunHistoryEntry, totals IssueRunTotals, chains [][]string) *issueHistoryResponse {
	rows, next := cutPage(page, issueHistoryPageSize)
	return &issueHistoryResponse{
		Runs:       toRunResponses(rows, chains),
		NextCursor: next,
		Totals:     historyTotalsOf(totals),
		Hops:       hopRowsOf(rows),
		LastStage:  lastStageOf(rows, chains),
	}
}

// lastStageOf names the chain the newest deciding run stood on. A reaction
// run follows a pull request rather than a stage, and a CI result row
// carries no rule, so neither decides. Only the served page is read: a page
// holding no deciding run yields nil instead of costing a further read.
func lastStageOf(runs []RunHistoryEntry, chains [][]string) *lastStageResponse {
	for _, run := range runs {
		if run.ReactionKind != "" || run.RuleName == "" {
			continue
		}
		chain := orchestrator.StageChainOf(chains, run.RuleName, run.StagePrevious)
		if chain == nil {
			return nil
		}
		return &lastStageResponse{Chain: chain, Rule: run.RuleName}
	}
	return nil
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.drainingFlag.Load() {
		resp := refreshResponse{
			Queued:      false,
			Coalesced:   false,
			RequestedAt: time.Now().UTC(),
			Operations:  []string{},
		}
		resp.OutcomeText = sentence.RefreshOutcome(resp.Queued, resp.Coalesced)
		writeJSON(w, s.logger, http.StatusConflict, resp)

		s.logger.Debug("request served",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", http.StatusConflict),
			slog.Duration("duration", time.Since(start)),
		)
		return
	}

	accepted := s.refreshFn()

	resp := refreshResponse{
		Queued:      true,
		Coalesced:   !accepted,
		RequestedAt: time.Now().UTC(),
		Operations:  []string{"poll", "reconcile"},
	}
	resp.OutcomeText = sentence.RefreshOutcome(resp.Queued, resp.Coalesced)

	writeJSON(w, s.logger, http.StatusAccepted, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusAccepted),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) handleRetryNow(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	status, body := s.retryNowAnswer(w, r, start.UTC())
	writeJSON(w, s.logger, status, body)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
	)
}

// retryNowAnswer runs the request and returns the status and body to write.
func (s *Server) retryNowAnswer(w http.ResponseWriter, r *http.Request, requestedAt time.Time) (int, any) {
	refused := retryNowResponse{
		RequestedAt: requestedAt,
		OutcomeText: sentence.RetryNowOutcome(false, "", "", 0),
	}
	unavailable := errorResponse{Error: errorDetail{
		Code:    "retry_now_unavailable",
		Message: "Sortie did not take the request; nothing changed",
	}}

	if s.drainingFlag.Load() {
		return http.StatusConflict, refused
	}
	if s.retryNowFn == nil {
		return http.StatusServiceUnavailable, unavailable
	}

	identifier := r.PathValue("identifier")
	result := s.retryNowFn(r.Context(), identifier)

	// The wait for the event loop is unbounded, so the server's own deadline
	// may have lapsed by now.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.httpServer.WriteTimeout))

	switch result.Status {
	case orchestrator.RetryNowDue:
		return http.StatusAccepted, retryNowResponse{
			Queued:            true,
			RequestedAt:       requestedAt,
			IssueID:           result.IssueID,
			IssueIdentifier:   result.Identifier,
			DisplayIdentifier: result.DisplayID,
			Attempt:           result.Attempt,
			OutcomeText:       sentence.RetryNowOutcome(true, result.DisplayID, result.Identifier, result.Attempt),
		}
	case orchestrator.RetryNowNotFound:
		return http.StatusNotFound, errorResponse{Error: errorDetail{
			Code:    "retry_not_found",
			Message: fmt.Sprintf("no retry is scheduled for issue %q", identifier),
		}}
	case orchestrator.RetryNowDraining:
		return http.StatusConflict, refused
	default:
		return http.StatusServiceUnavailable, unavailable
	}
}

func (s *Server) handleCheckNow(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	status, body := s.checkNowAnswer(w, r, start.UTC())
	writeJSON(w, s.logger, status, body)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
	)
}

// checkNowAnswer runs the request and returns the status and body to write.
func (s *Server) checkNowAnswer(w http.ResponseWriter, r *http.Request, requestedAt time.Time) (int, any) {
	refused := checkNowResponse{
		RequestedAt: requestedAt,
		OutcomeText: sentence.CheckNowOutcome(false, orchestrator.CheckNowResult{}),
	}
	unavailable := errorResponse{Error: errorDetail{
		Code:    "check_now_unavailable",
		Message: "Sortie did not take the request; nothing changed",
	}}

	if s.drainingFlag.Load() {
		return http.StatusConflict, refused
	}
	if s.checkNowFn == nil {
		return http.StatusServiceUnavailable, unavailable
	}

	identifier := r.PathValue("identifier")
	kind := r.PathValue("kind")
	result := s.checkNowFn(r.Context(), identifier, kind)

	// The wait for the event loop is unbounded, so the server's own deadline
	// may have lapsed by now.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.httpServer.WriteTimeout))

	switch result.Status {
	case orchestrator.CheckNowDue:
		return http.StatusAccepted, checkNowResponse{
			Queued:            true,
			RequestedAt:       requestedAt,
			IssueID:           result.IssueID,
			IssueIdentifier:   result.Identifier,
			DisplayIdentifier: result.DisplayID,
			Kind:              result.Kind,
			PullRequest:       &pullRequestResponse{Number: result.PRNumber, Repository: result.PRRepository},
			OutcomeText:       sentence.CheckNowOutcome(true, result),
		}
	case orchestrator.CheckNowNotFound:
		return http.StatusNotFound, errorResponse{Error: errorDetail{
			Code:    "reaction_not_found",
			Message: fmt.Sprintf("no pending %q reaction for issue %q", kind, identifier),
		}}
	case orchestrator.CheckNowDraining:
		return http.StatusConflict, refused
	default:
		return http.StatusServiceUnavailable, unavailable
	}
}

func (s *Server) handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	status, body := s.acknowledgeAnswer(w, r, start.UTC())
	writeJSON(w, s.logger, status, body)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
	)
}

// acknowledgeAnswer runs the request and returns the status and body to write.
func (s *Server) acknowledgeAnswer(w http.ResponseWriter, r *http.Request, requestedAt time.Time) (int, any) {
	refused := acknowledgeResponse{
		RequestedAt: requestedAt,
		OutcomeText: sentence.AcknowledgeOutcome(false, false, orchestrator.AttentionCause{}),
	}
	unavailable := errorResponse{Error: errorDetail{
		Code:    "acknowledge_unavailable",
		Message: "Sortie did not take the request; nothing changed",
	}}

	if s.drainingFlag.Load() {
		return http.StatusConflict, refused
	}
	if s.acknowledgeFn == nil {
		return http.StatusServiceUnavailable, unavailable
	}

	itemID := r.PathValue("id")
	result := s.acknowledgeFn(r.Context(), itemID)

	// The wait for the event loop is unbounded, so the server's own deadline
	// may have lapsed by now.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.httpServer.WriteTimeout))

	switch result.Status {
	case orchestrator.AcknowledgeRecorded, orchestrator.AcknowledgeAlready:
		already := result.Status == orchestrator.AcknowledgeAlready
		issueID, identifier, displayID := causeIssue(result.Cause)
		return http.StatusOK, acknowledgeResponse{
			Acknowledged:        true,
			AlreadyAcknowledged: already,
			RequestedAt:         requestedAt,
			ID:                  result.Cause.ID,
			Kind:                result.Cause.Kind,
			IssueID:             issueID,
			IssueIdentifier:     identifier,
			DisplayIdentifier:   displayID,
			AcknowledgedAt:      new(result.AcknowledgedAt.UTC()),
			OutcomeText:         sentence.AcknowledgeOutcome(true, already, result.Cause),
		}
	case orchestrator.AcknowledgeNotFound:
		return http.StatusNotFound, errorResponse{Error: errorDetail{
			Code:    "attention_item_not_found",
			Message: fmt.Sprintf("no current attention item has ID %q", itemID),
		}}
	case orchestrator.AcknowledgeFailed:
		return http.StatusInternalServerError, errorResponse{Error: errorDetail{
			Code:    "acknowledgement_not_saved",
			Message: "Sortie could not save the acknowledgement; nothing changed",
		}}
	case orchestrator.AcknowledgeDraining:
		return http.StatusConflict, refused
	default:
		return http.StatusServiceUnavailable, unavailable
	}
}

// causeIssue returns the issue keys of the row a cause names, empty for a
// process-wide cause.
func causeIssue(cause orchestrator.AttentionCause) (issueID, identifier, displayID string) {
	switch {
	case cause.Parked != nil:
		return cause.Parked.IssueID, cause.Parked.Identifier, cause.Parked.DisplayID
	case cause.Budget != nil:
		return cause.Budget.IssueID, cause.Budget.Identifier, cause.Budget.DisplayID
	case cause.Reaction != nil:
		return cause.Reaction.IssueID, cause.Reaction.Identifier, cause.Reaction.DisplayID
	}
	return "", "", ""
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	s.logger.Warn("method not allowed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
	}
	writeErrorJSON(w, s.logger, http.StatusMethodNotAllowed,
		"method_not_allowed",
		fmt.Sprintf("method %s is not allowed on this endpoint", r.Method))
}

// crossOriginForbidden answers a state-changing request that the cross-origin
// check refused, before any route runs.
func (s *Server) crossOriginForbidden(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("cross-origin request rejected",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("origin", r.Header.Get("Origin")),
		slog.String("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")),
		slog.String("host", r.Host),
	)
	writeErrorJSON(w, s.logger, http.StatusForbidden,
		"cross_origin_request_rejected",
		"Sortie refused this request because it could not confirm that it came from a page on Sortie's own origin, and nothing changed; a reverse proxy in front of Sortie must pass on the Host header the browser sent unchanged, including its port.")
}

// buildIssueDetail assembles the live detail of one issue, or nil when no live
// entry holds it. An entry belongs to the issue when its identifier equals
// identifier, or when issueID is non-empty and the entry carries it, which
// finds an issue the tracker has since renamed.
func buildIssueDetail(identifier, issueID string, snap orchestrator.RuntimeSnapshotResult, words sentence.Context) *issueDetailResponse {
	matches := func(gotIdentifier, gotIssueID string) bool {
		return gotIdentifier == identifier || (issueID != "" && gotIssueID == issueID)
	}

	var runEntry *runningEntryResponse
	var retryEntry *retryEntryResponse
	var budgetEntry *budgetExhaustedResponse
	var parkedEntry *parkedEntryResponse
	var hopEntry *stageHopResponse
	var situation sentence.Situation

	for _, e := range snap.Running {
		if matches(e.Identifier, e.IssueID) {
			situation.Running = &e
			runEntry = new(toServedRunningEntry(e, snap))
			setRunningSentences(runEntry, e, words)
			break
		}
	}

	for _, e := range snap.Retrying {
		if matches(e.Identifier, e.IssueID) {
			situation.Retry = &e
			retryEntry = new(toRetryEntryResponse(e))
			retryEntry.StatusText, retryEntry.StatusTextFull = sentencesOf(sentence.Entry{Retry: &e}, words)
			break
		}
	}

	for _, e := range snap.BudgetExhausted {
		if matches(e.Identifier, e.IssueID) {
			budgetEntry = new(toBudgetExhaustedResponse(e))
			budgetEntry.StatusText, budgetEntry.StatusTextFull = sentencesOf(sentence.Entry{BudgetHold: &e}, words)
			break
		}
	}

	for _, e := range snap.Parked {
		if matches(e.Identifier, e.IssueID) {
			parkedEntry = new(toParkedEntryResponse(e))
			parkedEntry.StatusText, parkedEntry.StatusTextFull = sentencesOf(sentence.Entry{Parked: &e}, words)
			break
		}
	}

	for _, e := range snap.BetweenStages {
		if matches(e.Identifier, e.IssueID) {
			situation.BetweenStages = &e
			hopEntry = new(toStageHopResponse(e, words))
			hopEntry.StatusText, hopEntry.StatusTextFull = sentencesOf(sentence.Entry{BetweenStages: &e}, words)
			break
		}
	}

	var pendingRows []orchestrator.SnapshotPendingReaction
	for _, e := range snap.PendingReactions {
		if matches(e.Identifier, e.IssueID) {
			pendingRows = append(pendingRows, e)
		}
	}

	pending := toPendingReactionResponses(pendingRows, words)
	rows := issueRows{
		running: runEntry,
		retry:   retryEntry,
		budget:  budgetEntry,
		parked:  parkedEntry,
		pending: pending,
		between: hopEntry,
	}
	if rows.status() == "" {
		return nil
	}

	situation.PendingReactions = pendingRows

	var status string
	var statusText, statusTextFull string
	var entryIdentifier, entryIssueID string
	var issue issueResponse

	switch rows.status() {
	case statusRunning:
		status = statusRunning
		statusText, statusTextFull = runEntry.StatusText, runEntry.StatusTextFull
		entryIdentifier, entryIssueID = runEntry.IssueIdentifier, runEntry.IssueID
		issue = issueResponse{
			Title:     runEntry.IssueTitle,
			URL:       runEntry.IssueURL,
			IssueType: runEntry.IssueType,
			Labels:    runEntry.IssueLabels,
			State:     runEntry.State,
		}
	case statusRetrying:
		status = statusRetrying
		statusText, statusTextFull = retryEntry.StatusText, retryEntry.StatusTextFull
		entryIdentifier, entryIssueID = retryEntry.IssueIdentifier, retryEntry.IssueID
		issue = issueResponse{
			Title:     retryEntry.IssueTitle,
			URL:       retryEntry.IssueURL,
			IssueType: retryEntry.IssueType,
			Labels:    retryEntry.IssueLabels,
			State:     retryEntry.State,
		}
	case statusBudgetExhausted:
		status = statusBudgetExhausted
		statusText, statusTextFull = budgetEntry.StatusText, budgetEntry.StatusTextFull
		entryIdentifier, entryIssueID = budgetEntry.IssueIdentifier, budgetEntry.IssueID
		issue = issueResponse{
			Title:     budgetEntry.IssueTitle,
			URL:       budgetEntry.IssueURL,
			IssueType: budgetEntry.IssueType,
			Labels:    budgetEntry.IssueLabels,
			State:     budgetEntry.State,
		}
	case statusParked:
		status = statusParked
		statusText, statusTextFull = parkedEntry.StatusText, parkedEntry.StatusTextFull
		entryIdentifier, entryIssueID = parkedEntry.IssueIdentifier, parkedEntry.IssueID
		issue = issueResponse{
			Title:     parkedEntry.IssueTitle,
			URL:       parkedEntry.IssueURL,
			IssueType: parkedEntry.IssueType,
			Labels:    parkedEntry.IssueLabels,
			State:     parkedEntry.ParkedState,
		}
	case statusPendingReactions:
		status = statusPendingReactions
		deciding := decidingPendingReaction(pending)
		statusText, statusTextFull = deciding.StatusText, deciding.StatusTextFull
		entryIdentifier, entryIssueID = pendingRows[0].Identifier, pendingRows[0].IssueID
		issue = issueResponse{
			Title:     pendingRows[0].IssueTitle,
			URL:       pendingRows[0].IssueURL,
			IssueType: pendingRows[0].IssueType,
			Labels:    pendingRows[0].IssueLabels,
			State:     pendingRows[0].State,
		}
	case statusBetweenStages:
		status = statusBetweenStages
		statusText, statusTextFull = hopEntry.StatusText, hopEntry.StatusTextFull
		entryIdentifier, entryIssueID = hopEntry.IssueIdentifier, hopEntry.IssueID
		issue = issueResponse{
			Title:     hopEntry.IssueTitle,
			URL:       hopEntry.IssueURL,
			IssueType: hopEntry.IssueType,
			Labels:    hopEntry.IssueLabels,
			State:     hopEntry.State,
		}
	default:
		status = "unknown"
		entryIdentifier = identifier
	}

	var ws *workspaceInfo
	if runEntry != nil && runEntry.WorkspacePath != "" {
		ws = &workspaceInfo{Path: runEntry.WorkspacePath}
	}

	var attempts *attemptsInfo
	if retryEntry != nil {
		restartCount := retryEntry.Attempt
		if restartCount > 0 {
			restartCount--
		}
		attempts = &attemptsInfo{
			RestartCount:        restartCount,
			CurrentRetryAttempt: retryEntry.Attempt,
		}
	} else {
		attempts = &attemptsInfo{}
	}

	var lastError *string
	if retryEntry != nil && retryEntry.Error != "" {
		lastError = &retryEntry.Error
	}

	return &issueDetailResponse{
		IssueIdentifier:  entryIdentifier,
		IssueID:          entryIssueID,
		Status:           status,
		Issue:            issue,
		Workspace:        ws,
		Attempts:         attempts,
		Running:          runEntry,
		Retry:            retryEntry,
		BudgetExhausted:  budgetEntry,
		Parked:           parkedEntry,
		PendingReactions: pending,
		BetweenStages:    hopEntry,
		RecentEvents:     []runEventResponse{},
		LastError:        lastError,
		Tracked:          map[string]any{},
		StatusText:       statusText,
		StatusTextFull:   statusTextFull,
		NextOutcomes:     []nextOutcomeResponse{},
		situation:        situation,
	}
}

// decidingPendingReaction returns the first escalated row of a non-empty
// list; else the review row awaiting approval when no other row reports an
// outstanding condition; else its first row: the row that decides what the
// issue is waiting on.
func decidingPendingReaction(rows []pendingReactionResponse) pendingReactionResponse {
	for _, row := range rows {
		if row.EscalatedAt != nil {
			return row
		}
	}
	awaiting := slices.IndexFunc(rows, awaitsApproval)
	if awaiting < 0 {
		return rows[0]
	}
	for i, row := range rows {
		if i != awaiting && !reportsNothingOutstanding(row) {
			return rows[0]
		}
	}
	return rows[awaiting]
}

func awaitsApproval(row pendingReactionResponse) bool {
	return row.Kind == orchestrator.ReactionKindReview && row.LastCheck != nil &&
		row.LastCheck.Comments != nil && row.LastCheck.Comments.AwaitingApproval
}

// reportsNothingOutstanding reports whether row waits on no merge condition.
// The kinds that record no check wait on the merge or a label, so they never
// hold the approval reading back; a kind this does not know is assumed busy.
func reportsNothingOutstanding(row pendingReactionResponse) bool {
	check := row.LastCheck
	switch row.Kind {
	case orchestrator.ReactionKindCI:
		return check != nil && check.CI != nil && check.CI.Status == "passing"
	case orchestrator.ReactionKindBotReview:
		return check != nil && check.Comments != nil && (check.Comments.Actionable == 0 || check.Comments.Handled)
	case orchestrator.ReactionKindMergeConflict:
		return check != nil && check.Mergeability != nil && check.Mergeability.Mergeable != nil && *check.Mergeability.Mergeable
	case orchestrator.ReactionKindAutoMerge, orchestrator.ReactionKindMergeCompletion,
		orchestrator.ReactionKindLabelReview, orchestrator.ReactionKindLabelFix:
		return true
	default:
		return false
	}
}

// healthResponse is the JSON wire format for GET /livez.
type healthResponse struct {
	Status string `json:"status"`
}

// readyResponse is the JSON wire format for GET /readyz.
type readyResponse struct {
	Status        string            `json:"status"`
	Version       string            `json:"version"`
	UptimeSeconds float64           `json:"uptime_seconds"`
	Checks        map[string]string `json:"checks"`
}

func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	if s.drainingFlag.Load() {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, healthResponse{Status: "fail"})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, healthResponse{Status: "pass"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	var uptime float64
	if !s.startedAt.IsZero() {
		d := max(time.Since(s.startedAt), 0)
		uptime = d.Seconds()
	}

	if s.drainingFlag.Load() {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, readyResponse{
			Status:        "fail",
			Version:       s.version,
			UptimeSeconds: uptime,
			Checks: map[string]string{
				"database":  "fail",
				"preflight": "fail",
				"workflow":  "fail",
			},
		})
		return
	}

	status := "pass"
	checks := map[string]string{}

	if s.dbPingFn != nil {
		if err := s.dbPingFn(r.Context()); err != nil {
			checks["database"] = "fail"
			status = "fail"
		} else {
			checks["database"] = "pass"
		}
	} else {
		checks["database"] = "pass"
	}

	if s.preflightFn != nil && !s.preflightFn() {
		checks["preflight"] = "fail"
		status = "fail"
	} else {
		checks["preflight"] = "pass"
	}

	if s.workflowLoadedFn != nil && !s.workflowLoadedFn() {
		checks["workflow"] = "fail"
		status = "fail"
	} else {
		checks["workflow"] = "pass"
	}

	httpStatus := http.StatusOK
	if status != "pass" {
		httpStatus = http.StatusServiceUnavailable
	}

	writeJSON(w, s.logger, httpStatus, readyResponse{
		Status:        status,
		Version:       s.version,
		UptimeSeconds: uptime,
		Checks:        checks,
	})
}
