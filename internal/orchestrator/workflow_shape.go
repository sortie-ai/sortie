package orchestrator

import (
	"maps"
	"slices"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// WorkflowShape reports the effective workflow shape served on the state API.
type WorkflowShape struct {
	Tracker   ShapeTracker   `json:"tracker"`
	Polling   ShapePolling   `json:"polling"`
	Agent     ShapeAgent     `json:"agent"`
	Worker    ShapeWorker    `json:"worker"`
	Dispatch  ShapeDispatch  `json:"dispatch"`
	Reactions ShapeReactions `json:"reactions"`
}

// ShapeTracker reports the tracker kind and project the poll loop's adapter was
// built with, and the states the workflow acts on.
type ShapeTracker struct {
	Kind            string   `json:"kind"`
	Project         string   `json:"project,omitempty"`
	ActiveStates    []string `json:"active_states"`
	InProgressState string   `json:"in_progress_state,omitempty"`
	HandoffState    string   `json:"handoff_state,omitempty"`
	NoChangeState   string   `json:"no_change_state,omitempty"`
	TerminalStates  []string `json:"terminal_states"`
}

// ShapePolling reports the tracker poll interval.
type ShapePolling struct {
	IntervalMS int `json:"interval_ms"`
}

// ShapeAgent reports the workflow-wide agent kind, its limits, and its budgets.
type ShapeAgent struct {
	Kind                       string         `json:"kind"`
	UsageArrival               string         `json:"usage_arrival"`
	MaxTurns                   int            `json:"max_turns"`
	StallTimeoutMS             *int           `json:"stall_timeout_ms"`
	MaxRetryBackoffMS          int            `json:"max_retry_backoff_ms"`
	MaxSessions                *int           `json:"max_sessions"`
	MaxTokens                  *int           `json:"max_tokens"`
	MaxCostUSD                 *float64       `json:"max_cost_usd"`
	MaxConcurrentAgents        int            `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int `json:"max_concurrent_agents_by_state"`
}

// ShapeWorker reports the SSH hosts agents launch on and their capacity.
type ShapeWorker struct {
	SSHHosts                   []string `json:"ssh_hosts"`
	MaxConcurrentAgentsPerHost *int     `json:"max_concurrent_agents_per_host"`
}

// ShapeDispatch reports the dispatch rules, the default selection, the stage chains, and the hop ceiling.
type ShapeDispatch struct {
	Rules              []ShapeRule     `json:"rules"`
	Default            *ShapeSelection `json:"default"`
	Chains             [][]string      `json:"chains"`
	MaxConsecutiveHops *int            `json:"max_consecutive_hops"`
	HopCeiling         int             `json:"hop_ceiling"`
}

// ShapeRule reports one dispatch rule and how an issue is selected for it.
type ShapeRule struct {
	Name         string      `json:"name,omitempty"`
	SelectedBy   string      `json:"selected_by"`
	Stage        string      `json:"stage,omitempty"`
	Match        *ShapeMatch `json:"match,omitempty"`
	Next         string      `json:"next,omitempty"`
	AgentKind    string      `json:"agent_kind"`
	UsageArrival string      `json:"usage_arrival"`
}

// ShapeMatch reports the predicates of a match rule.
type ShapeMatch struct {
	Labels     []string       `json:"labels,omitempty"`
	IssueType  []string       `json:"issue_type,omitempty"`
	Title      []string       `json:"title,omitempty"`
	Identifier []string       `json:"identifier,omitempty"`
	Assignee   []string       `json:"assignee,omitempty"`
	Priority   *ShapePriority `json:"priority,omitempty"`
}

// ShapePriority reports a priority predicate: one operand for a comparison, a list for in.
type ShapePriority struct {
	Op     string `json:"op"`
	Value  *int   `json:"value,omitempty"`
	Values []int  `json:"values,omitzero"`
}

// ShapeSelection reports the agent kind a selection runs and whether it reports token usage.
type ShapeSelection struct {
	AgentKind    string `json:"agent_kind"`
	UsageArrival string `json:"usage_arrival"`
}

// ShapeReactions reports the reaction kinds the process applies, each present only while applied.
type ShapeReactions struct {
	CIFailure       *ShapeCIFailure       `json:"ci_failure,omitempty"`
	ReviewComments  *ShapeReviewComments  `json:"review_comments,omitempty"`
	BotReview       *ShapeBotReview       `json:"bot_review,omitempty"`
	MergeConflicts  *ShapeMergeConflicts  `json:"merge_conflicts,omitempty"`
	AutoMerge       *ShapeAutoMerge       `json:"auto_merge,omitempty"`
	MergeCompletion *ShapeMergeCompletion `json:"merge_completion,omitempty"`
	LabelCommands   *ShapeLabelCommands   `json:"label_commands,omitempty"`
}

// ShapeEscalation reports what a reaction does when it gives up; it is embedded so its members sit beside the kind's own.
type ShapeEscalation struct {
	Escalation      string `json:"escalation"`
	EscalationLabel string `json:"escalation_label,omitempty"`
}

// ShapeCIFailure reports the CI failure reaction.
type ShapeCIFailure struct {
	MaxRetries int  `json:"max_retries"`
	Triage     bool `json:"triage"`
	ShapeEscalation
}

// ShapeReviewComments reports the review comments reaction.
type ShapeReviewComments struct {
	MaxContinuationTurns int  `json:"max_continuation_turns"`
	DebounceMS           int  `json:"debounce_ms"`
	PollIntervalMS       int  `json:"poll_interval_ms"`
	Triage               bool `json:"triage"`
	ShapeEscalation
}

// ShapeBotReview reports the bot review reaction.
type ShapeBotReview struct {
	MaxContinuationTurns int      `json:"max_continuation_turns"`
	PollIntervalMS       int      `json:"poll_interval_ms"`
	BotUsernames         []string `json:"bot_usernames"`
	Triage               bool     `json:"triage"`
	ShapeEscalation
}

// ShapeMergeConflicts reports the merge conflict reaction.
type ShapeMergeConflicts struct {
	MaxRetries     int  `json:"max_retries"`
	PollIntervalMS int  `json:"poll_interval_ms"`
	Triage         bool `json:"triage"`
	ShapeEscalation
}

// ShapeAutoMerge reports the auto-merge reaction.
type ShapeAutoMerge struct {
	Strategy       string `json:"strategy"`
	RequireCI      bool   `json:"require_ci"`
	DeleteBranch   bool   `json:"delete_branch"`
	PollIntervalMS int    `json:"poll_interval_ms"`
	MaxRetries     *int   `json:"max_retries"`
	ShapeEscalation
}

// ShapeMergeCompletion reports the merge completion reaction.
type ShapeMergeCompletion struct {
	TargetState    string `json:"target_state"`
	MaxRetries     int    `json:"max_retries"`
	PollIntervalMS int    `json:"poll_interval_ms"`
	ShapeEscalation
}

// ShapeLabelCommands reports which label commands are configured and how often they poll.
type ShapeLabelCommands struct {
	ReviewLabel    string `json:"review_label,omitempty"`
	FixLabel       string `json:"fix_label,omitempty"`
	PollIntervalMS int    `json:"poll_interval_ms"`
}

// WorkflowShape returns the effective workflow shape. Safe for concurrent use.
//
// Reaction entries other than the CI failure members come from fields set at
// construction, so a reload neither adds nor removes one. The tracker kind and
// project are fixed at construction too, because the adapter is not rebuilt on
// reload.
func (o *Orchestrator) WorkflowShape() WorkflowShape {
	cfg := o.workflowManager.Config()
	worker, remote := o.shapeWorker(cfg)

	return WorkflowShape{
		Tracker: ShapeTracker{
			Kind:            o.trackerKind,
			Project:         o.trackerProject,
			ActiveStates:    copyStrings(cfg.Tracker.ActiveStates),
			InProgressState: cfg.Tracker.InProgressState,
			HandoffState:    cfg.Tracker.HandoffState,
			NoChangeState:   cfg.Tracker.NoChangeState,
			TerminalStates:  copyStrings(cfg.Tracker.TerminalStates),
		},
		Polling: ShapePolling{IntervalMS: cfg.Polling.IntervalMS},
		Agent: ShapeAgent{
			Kind:                       cfg.Agent.Kind,
			UsageArrival:               o.usageArrival(cfg.Agent.Kind, remote),
			MaxTurns:                   cfg.Agent.TurnLimit(),
			StallTimeoutMS:             positiveOrNil(cfg.Agent.StallTimeoutMS),
			MaxRetryBackoffMS:          cfg.Agent.MaxRetryBackoffMS,
			MaxSessions:                positiveOrNil(cfg.Agent.MaxSessions),
			MaxTokens:                  positiveOrNil(cfg.Agent.MaxTokens),
			MaxCostUSD:                 positiveOrNil(cfg.Agent.MaxCostUSD),
			MaxConcurrentAgents:        cfg.Agent.MaxConcurrentAgents,
			MaxConcurrentAgentsByState: copyCaps(cfg.Agent.MaxConcurrentByState),
		},
		Worker:    worker,
		Dispatch:  o.shapeDispatch(cfg, remote),
		Reactions: o.shapeReactions(cfg),
	}
}

// shapeWorker reports the worker members and whether launches are remote.
func (o *Orchestrator) shapeWorker(cfg config.ServiceConfig) (ShapeWorker, bool) {
	parsed := ParseWorkerConfig(cfg.ExtensionSection("worker"), cfg.ExtensionEnvRefPaths("worker"))

	hosts := make([]string, 0, len(parsed.SSHHosts))
	for _, host := range parsed.SSHHosts {
		if !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}

	worker := ShapeWorker{SSHHosts: hosts}
	if len(hosts) > 0 {
		worker.MaxConcurrentAgentsPerHost = positiveOrNil(parsed.MaxPerHost)
	}
	return worker, len(hosts) > 0
}

// usageArrival resolves the arrival a session of kind starts with on the given
// launch, through the code an attempt uses. A session that can carry no token
// figure on that launch shows none, as preflight warns.
func (o *Orchestrator) usageArrival(kind string, remote bool) string {
	meta, registered := o.preflightParams.AgentRegistry.Meta(kind)
	arrival, _, carriesTokenFigure := usageDisposition(meta, registered, remote)
	if !carriesTokenFigure {
		return string(registry.UsageArrivalNone)
	}
	return string(arrival)
}

func (o *Orchestrator) shapeDispatch(cfg config.ServiceConfig, remote bool) ShapeDispatch {
	dispatch := cfg.Dispatch

	rules := make([]ShapeRule, 0, len(dispatch.Rules))
	for _, rule := range dispatch.Rules {
		kind := ruleResolution(rule, dispatch, cfg.Agent.Kind, "").AgentKind
		shaped := ShapeRule{
			Name:         rule.Name,
			Next:         rule.Next,
			AgentKind:    kind,
			UsageArrival: o.usageArrival(kind, remote),
		}
		switch {
		case rule.Stage != "":
			shaped.SelectedBy = "stage"
			shaped.Stage = rule.Stage
		case rule.IsCatchAll:
			shaped.SelectedBy = "catch_all"
		default:
			shaped.SelectedBy = "match"
			shaped.Match = shapeMatch(rule.Match)
		}
		rules = append(rules, shaped)
	}

	var fallback *ShapeSelection
	if resolution, ok := defaultSelection(dispatch, cfg.Agent.Kind, ""); ok {
		fallback = &ShapeSelection{
			AgentKind:    resolution.AgentKind,
			UsageArrival: o.usageArrival(resolution.AgentKind, remote),
		}
	}

	chains := dispatch.Chains()
	if chains == nil {
		chains = [][]string{}
	}

	shaped := ShapeDispatch{
		Rules:      rules,
		Default:    fallback,
		Chains:     chains,
		HopCeiling: dispatch.HopCeiling(),
	}
	if dispatch.MaxConsecutiveHopsWritten {
		hops := dispatch.MaxConsecutiveHops
		shaped.MaxConsecutiveHops = &hops
	}
	return shaped
}

func shapeMatch(m config.DispatchMatch) *ShapeMatch {
	shaped := &ShapeMatch{
		Labels:     slices.Clone(m.Labels),
		IssueType:  slices.Clone(m.IssueType),
		Title:      slices.Clone(m.Title),
		Identifier: slices.Clone(m.Identifier),
		Assignee:   slices.Clone(m.Assignee),
	}
	if m.Priority == nil {
		return shaped
	}
	shaped.Priority = &ShapePriority{Op: m.Priority.Op}
	if m.Priority.Op == "in" {
		shaped.Priority.Values = slices.Clone(m.Priority.Values)
		return shaped
	}
	value := m.Priority.Value
	shaped.Priority.Value = &value
	return shaped
}

func (o *Orchestrator) shapeReactions(cfg config.ServiceConfig) ShapeReactions {
	var reactions ShapeReactions

	if o.ciProvider != nil && o.scmAdapter != nil {
		reactions.CIFailure = &ShapeCIFailure{
			MaxRetries:      cfg.CIFeedback.MaxRetries,
			Triage:          o.ciTriage.Enabled(),
			ShapeEscalation: shapeEscalation(cfg.CIFeedback.Escalation, cfg.CIFeedback.EscalationLabel),
		}
	}
	if o.reviewReactionConfigured {
		review := o.reviewConfig
		reactions.ReviewComments = &ShapeReviewComments{
			MaxContinuationTurns: review.MaxContinuationTurns,
			DebounceMS:           review.DebounceMS,
			PollIntervalMS:       review.PollIntervalMS,
			Triage:               review.Triage.Enabled(),
			ShapeEscalation:      shapeEscalation(review.Escalation, review.EscalationLabel),
		}
	}
	if o.botReviewReactionConfigured {
		bot := o.botReviewConfig
		reactions.BotReview = &ShapeBotReview{
			MaxContinuationTurns: bot.MaxContinuationTurns,
			PollIntervalMS:       bot.PollIntervalMS,
			BotUsernames:         copyStrings(bot.BotUsernames),
			Triage:               bot.Triage.Enabled(),
			ShapeEscalation:      shapeEscalation(bot.Escalation, bot.EscalationLabel),
		}
	}
	if o.mergeConflictReactionConfigured {
		conflict := o.mergeConflictConfig
		reactions.MergeConflicts = &ShapeMergeConflicts{
			MaxRetries:      conflict.MaxRetries,
			PollIntervalMS:  conflict.PollIntervalMS,
			Triage:          conflict.Triage.Enabled(),
			ShapeEscalation: shapeEscalation(conflict.Escalation, conflict.EscalationLabel),
		}
	}
	if o.autoMergeReactionConfigured {
		merge := o.autoMergeConfig
		reactions.AutoMerge = &ShapeAutoMerge{
			Strategy:        string(merge.Strategy),
			RequireCI:       merge.RequireCI,
			DeleteBranch:    merge.DeleteBranch,
			PollIntervalMS:  merge.PollIntervalMS,
			MaxRetries:      positiveOrNil(merge.MaxRetries),
			ShapeEscalation: shapeEscalation(merge.Escalation, merge.EscalationLabel),
		}
	}
	if o.mergeCompletionReactionConfigured {
		completion := o.mergeCompletionConfig
		reactions.MergeCompletion = &ShapeMergeCompletion{
			TargetState:     completion.TargetState,
			MaxRetries:      completion.MaxRetries,
			PollIntervalMS:  completion.PollIntervalMS,
			ShapeEscalation: shapeEscalation(completion.Escalation, completion.EscalationLabel),
		}
	}
	reactions.LabelCommands = o.shapeLabelCommands()
	return reactions
}

// shapeLabelCommands returns nil when neither label command is configured.
func (o *Orchestrator) shapeLabelCommands() *ShapeLabelCommands {
	var commands ShapeLabelCommands
	switch {
	case o.labelReviewReactionConfigured:
		commands.PollIntervalMS = o.labelReviewConfig.PollIntervalMS
	case o.labelFixReactionConfigured:
		commands.PollIntervalMS = o.labelFixConfig.PollIntervalMS
	default:
		return nil
	}
	if o.labelReviewReactionConfigured {
		commands.ReviewLabel = o.labelReviewConfig.ReviewLabel
	}
	if o.labelFixReactionConfigured {
		commands.FixLabel = o.labelFixConfig.FixLabel
	}
	return &commands
}

// shapeEscalation serves an empty mode as none, since a reaction with no mode
// applies neither a label nor a comment. The label appears only in label mode.
func shapeEscalation(mode, label string) ShapeEscalation {
	if mode == "" {
		return ShapeEscalation{Escalation: "none"}
	}
	shaped := ShapeEscalation{Escalation: mode}
	if mode == "label" {
		shaped.EscalationLabel = coalesce(label, "needs-human")
	}
	return shaped
}

func copyStrings(values []string) []string {
	return append([]string{}, values...)
}

func copyCaps(caps map[string]int) map[string]int {
	copied := make(map[string]int, len(caps))
	maps.Copy(copied, caps)
	return copied
}

// positiveOrNil serves a non-positive setting as nil because such a value
// imposes no ceiling, and a client would read 0 as a ceiling of zero.
func positiveOrNil[T int | float64](n T) *T {
	if n <= 0 {
		return nil
	}
	return &n
}
