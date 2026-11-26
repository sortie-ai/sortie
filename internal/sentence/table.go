package sentence

import (
	"fmt"
	"maps"
	"slices"
	"text/template"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// fallbackText stands in for a sentence whose template failed to render.
const fallbackText = "Status not available"

// parts holds the template sources of one table entry: short and long for a
// state sentence, text for an outcome, condition and text for a next outcome,
// reason, why, and clears for an attention item.
type parts struct {
	short     string
	long      string
	text      string
	condition string
	reason    string
	why       string
	clears    string
}

// Fragments shared by several templates.
const (
	ofLimit      = "{{if .Limit}} of {{count .Limit}}{{end}}"
	stagePlace   = "{{if .Stage}} stage {{.Stage}} ({{count .Pos}} of {{count .Len}}){{end}}"
	onStage      = "{{if .Stage}} on stage {{.Stage}}{{end}}"
	onStagePlace = "{{if .Stage}} on stage {{.Stage}} ({{count .Pos}} of {{count .Len}}){{end}}"
	iteration    = "{{if .Iteration}}, iteration {{count .Iteration}}{{end}}"
	nextCheck    = "{{.Check}} {{due .Next}}"
	usedCount    = "{{if .CountLabel}} {{.CountLabel}}: {{count .Used}}{{if .HasMax}} of {{count .Max}}{{end}}.{{end}}"
	ciProgress   = "{{if .HasCI}}: {{count .Pending}} pending, {{count .Passing}} passing{{end}}"
	failingList  = "{{if .FailingChecks}}{{list .FailingChecks}}{{else}}{{plural .FailingN \"failing check\" \"failing checks\"}}{{end}}"
	commentsOf   = "{{if .HasComments}}: {{plural .Actionable \"actionable comment\" \"actionable comments\"}}, {{count .New}} new{{end}}"
	newComments  = "{{plural .New \"new review comment\" \"new review comments\"}}"
	watched      = "{{if .Label}}the `{{.Label}}` label{{else}}a label command{{end}}"
	parkingLabel = "Parking label: `{{.Label}}`{{if .Unseen}} (not yet seen on the issue){{end}}."
	inNext       = "{{if .Window}} in the next {{duration .Window}}{{end}}"
	heldAt       = "{{if .MoneyHold}}money budget (`agent.max_cost_usd`){{else if .TokenHold}}token budget of {{tokens .Budget}}{{else}}session budget ({{count .Sessions}} of {{count .MaxSessions}} sessions used){{end}}"
	ofMax        = "{{if .Max}} of {{count .Max}}{{end}}"
	posture      = "{{if eq .Escalation \"label\"}}applies `{{.EscalationLabel}}` to the issue" +
		"{{else if eq .Escalation \"comment\"}}comments on the issue" +
		"{{else}}sends `escalation.{{.ConfigKey}}` only to the notification destinations that subscribe to it{{end}}"
	thePR      = "{{if .PR}}PR #{{.PR}}{{else}}the pull request{{end}}"
	afterAhead = "{{if eq .Ahead \"retry\"}} after {{retry .AheadTrigger .AheadRunInline .AheadAttempt}} ends" +
		"{{else if eq .Ahead \"run\"}} after the current run ends{{end}}"
)

// Fragments shared by the hold templates.
const (
	blockersTerminal = "{{if .Several}}every blocker reaches{{else}}the blocker reaches{{end}} a terminal state"
	resetsIn         = "{{if .HasReset}}, resets in {{duration .ResetsIn}}{{end}}"
)

// errorDetail ends a run-error sentence with the text the stored error
// carried, when it carried any.
const errorDetail = "{{if .Detail}}: {{.Detail}}{{end}}."

// eventMessage appends an event's message to its sentence when it has one.
const eventMessage = "{{if .Message}}: {{.Message}}{{end}}"

// Fragments shared by the attention templates.
const (
	parkedClears = "Clears when a poll tick finds `{{.Label}}` removed from the issue{{if .Unseen}} after Sortie has seen it there{{end}}" +
		"{{if .State}}, or the issue moved out of {{.State}}{{end}}."
	parkedSeen      = "{{if .Unseen}} (not yet seen on the issue){{end}}"
	budgetLeaves    = "or after the issue leaves the active tracker states."
	triesOfMax      = "{{count .Tries}}{{if .HasMax}} of {{count .Max}}{{end}}"
	orLeavesHandoff = "{{if .Handoff}} or leaves {{.Handoff}}{{end}}"
	terminalExit    = "the issue reaches a terminal tracker state" + orLeavesHandoff
	latestCheck     = "{{if eq .Latest \"failing\"}}; the latest check found " +
		"{{if .FailingChecks}}{{list .FailingChecks}} failing{{else}}{{plural .FailingN \"failing check\" \"failing checks\"}}{{end}}" +
		"{{else if eq .Latest \"pending\"}}; the latest check found CI pending{{end}}"
	triageAsked = " because the triage command asked for a person"
)

// ifTrigger opens a branch that renders when the escalation began for trigger.
func ifTrigger(trigger orchestrator.ReactionEscalationTrigger) string {
	return `{{if eq .Trigger "` + string(trigger) + `"}}`
}

// elseIfTrigger continues a branch chain opened by [ifTrigger].
func elseIfTrigger(trigger orchestrator.ReactionEscalationTrigger) string {
	return `{{else if eq .Trigger "` + string(trigger) + `"}}`
}

// onBudget renders text only for an escalation that began on an exhausted
// budget.
func onBudget(text string) string {
	return ifTrigger(orchestrator.EscalationTriggerBudget) + text + "{{end}}"
}

var table = map[string]parts{
	"run_work": {
		short: "Turn {{count .Turn}}" + onStage,
		long:  "Running" + stagePlace + ", turn {{count .Turn}}.",
	},
	"run_starting": {
		short: "Starting, no turn yet",
		long:  "Starting" + stagePlace + "; no turn has begun yet.",
	},
	"run_self_review": {
		short: "Self-review" + iteration + onStage,
		long:  "In self-review" + onStagePlace + iteration + ".",
	},
	"run_ci_fix": {
		short: "Fixing CI on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "Fixing CI on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_merge_conflict": {
		short: "Resolving the merge conflict on #{{.PR}}, rebase {{count .Attempt}}" + ofLimit,
		long:  "Resolving the merge conflict on PR #{{.PR}}, rebase {{count .Attempt}}" + ofLimit + ".",
	},
	"run_review_fix": {
		short: "Answering review comments on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "Answering review comments on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_bot_review_fix": {
		short: "Answering bot review comments on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "Answering bot review comments on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_label_review": {
		short: "Reviewing #{{.PR}}{{if .Label}} for `{{.Label}}`{{end}}",
		long:  "Reviewing PR #{{.PR}}{{if .Label}}, as the `{{.Label}}` label asked{{end}}.",
	},
	"run_label_fix": {
		short: "Fixing #{{.PR}}{{if .Label}} for `{{.Label}}`{{end}}",
		long:  "Fixing PR #{{.PR}}{{if .Label}}, as the `{{.Label}}` label asked{{end}}.",
	},

	"retry_paused": {
		short: "Retry {{count .Attempt}} paused: {{.Reason}}",
		long:  "Retry {{count .Attempt}} is paused: {{.Reason}}; next check {{due .Due}}.",
	},
	"retry_due_now": {
		short: "Retry {{count .Attempt}} due now",
		long:  "Retry {{count .Attempt}} is due now.",
	},
	"retry_error": {
		short: "Retry {{count .Attempt}} {{due .Due}} after an error",
		long:  "The last run ended with an error{{if .Error}}: {{.Error}}{{end}}. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_continuation": {
		short: "Next run {{due .Due}}: the issue is still active",
		long:  "The last run ended cleanly and the issue is still active; next run {{due .Due}}.",
	},
	"retry_stall": {
		short: "Retry {{count .Attempt}} {{due .Due}}: the agent stalled",
		long:  "The last run stalled: the agent sent no event within the stall timeout. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_timer": {
		short: "Retry {{count .Attempt}} {{due .Due}}: {{.Reason}}",
		long:  "The retry came due and was not dispatched: {{.Reason}}. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_ci_fix": {
		short: "CI fix run {{due .Due}}",
		long:  "CI failed on the pull request; fix run {{due .Due}}.",
	},
	"retry_reaction": {
		short: "{{.Run}} {{due .Due}}",
		long:  "{{.Run}} {{due .Due}}.",
	},
	"retry_generic": {
		short: "Retry {{count .Attempt}} {{due .Due}}",
		long:  "Retry {{count .Attempt}} {{due .Due}}.",
	},

	"held_budget_tokens": {
		short: "Held: token budget reached",
		long:  "Held at the token budget{{if .HasBudget}} of {{tokens .Budget}}{{end}}. Tokens used: {{if .AtLeast}}at least {{end}}{{tokens .Used}}.",
	},
	"held_budget_money": {
		short: "Held: money budget reached",
		long:  "Held at the money budget (`agent.max_cost_usd`).",
	},
	"held_budget_sessions": {
		short: "Held: session budget reached",
		long:  "Held at the session budget" + ofLimit + ". Sessions used: {{count .Sessions}}.",
	},

	"parked_agent_blocked": {
		short: "Parked: the agent needs a person",
		long:  "Parked: the agent stopped because it cannot go on without a person. " + parkingLabel,
	},
	"parked_handoff_absence": {
		short: "Parked: no evidence of work",
		long:  "Parked: the issue's runs ended without evidence of work as many times in a row as the limit allows. " + parkingLabel,
	},

	"held_escalated_ci": {
		short: "CI escalated on #{{.PR}}",
		long:  "Sortie escalated failing CI on PR #{{.PR}}; it keeps watching the pull request." + usedCount,
	},
	"held_escalated_reaction": {
		short: "{{.KindInitial}} escalated on #{{.PR}}",
		long:  "Sortie escalated {{.KindInline}} on PR #{{.PR}}." + usedCount,
	},
	"wait_ci": {
		short: "Waiting on CI for #{{.PR}}" + ciProgress,
		long:  "Waiting on CI for PR #{{.PR}}" + ciProgress + "; " + nextCheck + "." + usedCount,
	},
	"wait_ci_passing": {
		short: "CI passing on #{{.PR}}",
		long:  "CI passes on PR #{{.PR}} ({{count .Passing}} passing); " + nextCheck + "." + usedCount,
	},
	"wait_ci_failing": {
		short: "CI failing on #{{.PR}}",
		long:  "CI fails on PR #{{.PR}}: " + failingList + "; " + nextCheck + "." + usedCount,
	},
	"wait_review_debounce": {
		short: newComments + " on #{{.PR}}; next check {{due .Next}}",
		long:  "Waiting for " + newComments + " on PR #{{.PR}} ({{count .Actionable}} actionable) to settle; next check {{due .Next}}." + usedCount,
	},
	"wait_review": {
		short: "Waiting on review of #{{.PR}}",
		long:  "Waiting on review of PR #{{.PR}}" + commentsOf + "; " + nextCheck + "." + usedCount,
	},
	"wait_review_approval": {
		short: "Waiting on approval of #{{.PR}}",
		long:  "Waiting on approval of PR #{{.PR}}; " + nextCheck + "." + usedCount,
	},
	"wait_bot": {
		short: "Waiting on bot review of #{{.PR}}",
		long:  "Waiting on bot review of PR #{{.PR}}{{if .Bots}} by {{.Bots}}{{end}}" + commentsOf + "; " + nextCheck + "." + usedCount,
	},
	"wait_bot_handled": {
		short: "Bot review of #{{.PR}} handled",
		long:  "The triage command handled the bot review of PR #{{.PR}}{{if .Bots}} by {{.Bots}}{{end}}, so Sortie starts no session for it; " + nextCheck + "." + usedCount,
	},
	"wait_merge": {
		short: "Auto-merge waiting on #{{.PR}}",
		long:  "Auto-merge waits for PR #{{.PR}} to meet its merge conditions{{if .Strategy}} ({{.Strategy}} strategy){{end}}; " + nextCheck + "." + usedCount,
	},
	"merge_conflict_found": {
		short: "Merge conflict on #{{.PR}}",
		long:  "PR #{{.PR}} has a merge conflict; " + nextCheck + "." + usedCount,
	},
	"wait_merge_conflict": {
		short: "Watching #{{.PR}} for merge conflicts",
		long:  "Sortie watches PR #{{.PR}} for merge conflicts; {{.Mergeability}}; " + nextCheck + "." + usedCount,
	},
	"wait_merge_completion": {
		short: "Waiting for #{{.PR}} to merge",
		long:  "Waiting{{if .Handoff}} in {{.Handoff}}{{end}} for PR #{{.PR}} to merge{{if .Target}}, then Sortie moves the issue to {{.Target}}{{end}}; " + nextCheck + "." + usedCount,
	},
	"wait_label_command": {
		short: "Watching #{{.PR}} for " + watched,
		long:  "Sortie watches PR #{{.PR}} for " + watched + "; " + nextCheck + ".",
	},
	"wait_reaction": {
		short: "Waiting on #{{.PR}}",
		long:  "Waiting on a pull request reaction for PR #{{.PR}}; " + nextCheck + ".",
	},

	"attn_parked_agent_blocked": {
		reason: "Parked: the agent needs a person",
		why:    "The agent stopped because it cannot go on without a person, so Sortie parked the issue under the label `{{.Label}}`" + parkedSeen + ".",
		clears: parkedClears,
	},
	"attn_parked_handoff_absence": {
		reason: "Parked: no evidence of work",
		why: "The issue's runs ended without evidence of work as many times in a row as `agent.max_consecutive_absences` allows, " +
			"so Sortie parked it under the label `{{.Label}}`" + parkedSeen + ".",
		clears: parkedClears,
	},
	"attn_budget_tokens": {
		reason: "Token budget reached",
		why: "The issue has used {{if .AtLeast}}at least {{end}}{{tokens .Used}} tokens of its budget of {{tokens .Budget}}" +
			"{{if .Stopped}}, and the budget stopped {{plural .Stopped \"session\" \"sessions\"}} mid-run{{end}}.",
		clears: "Clears on the next poll tick after `agent.max_tokens` is raised above what the issue has used or is removed, " + budgetLeaves,
	},
	"attn_budget_money": {
		reason: "Money budget reached",
		why: "The issue has used {{if .AtLeast}}at least {{end}}{{usd .Used}} of its budget of {{usd .Budget}} in estimated cost" +
			"{{if .Stopped}}, and a budget ceiling stopped {{plural .Stopped \"session\" \"sessions\"}} mid-run{{end}}.",
		clears: "Clears on the next poll tick after `agent.max_cost_usd` is raised above what the issue has used or is removed, " + budgetLeaves,
	},
	"attn_budget_sessions": {
		reason: "Session budget reached",
		why:    "The issue has used {{plural .UsedSessions \"session\" \"sessions\"}} of its budget of {{count .BudgetSessions}}.",
		clears: "Clears on the next poll tick after `agent.max_sessions` is raised above {{count .UsedSessions}} or is removed, " + budgetLeaves,
	},
	"attn_reload": {
		reason: "{{.File}} failed to load",
		why:    "{{.File}} failed to load ({{.Error}}), so poll ticks dispatch nothing until it loads, and the configuration that last loaded stays in effect.",
		clears: "Clears when {{.File}} loads again, which Sortie tries each time the file is saved and on every poll tick.",
	},
	"attn_preflight": {
		reason: "Dispatch preflight failed",
		why:    "The dispatch preflight failed ({{.Error}}), so poll ticks dispatch nothing until it passes.",
		clears: "Clears on the first poll tick whose dispatch preflight passes.",
	},
	"attn_spend_daily": {
		reason: "Daily spend ceiling reached",
		why: "Sortie has spent {{if .AtLeast}}at least {{end}}{{usd .Used}} in estimated cost this UTC day against the daily ceiling of {{usd .Budget}}, " +
			"so it starts no agent session for any issue while sessions already running finish.",
		clears: "Clears once the UTC day ends, or after `agent.max_daily_cost_usd` is raised above the day's spend or removed.",
	},
	"attn_spend_weekly": {
		reason: "Weekly spend ceiling reached",
		why: "Sortie has spent {{if .AtLeast}}at least {{end}}{{usd .Used}} in estimated cost this UTC week against the weekly ceiling of {{usd .Budget}}, " +
			"so it starts no agent session for any issue while sessions already running finish.",
		clears: "Clears once the UTC week ends at the start of Monday, or after `agent.max_weekly_cost_usd` is raised above the week's spend or removed.",
	},
	"attn_spend_monthly": {
		reason: "Monthly spend ceiling reached",
		why: "Sortie has spent {{if .AtLeast}}at least {{end}}{{usd .Used}} in estimated cost this UTC month against the monthly ceiling of {{usd .Budget}}, " +
			"so it starts no agent session for any issue while sessions already running finish.",
		clears: "Clears once the UTC month ends, or after `agent.max_monthly_cost_usd` is raised above the month's spend or removed.",
	},

	"attn_escalated_ci": {
		reason: "CI escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerTriage) + "Sortie escalated failing CI on PR #{{.PR}}" + triageAsked + latestCheck + "." +
			"{{else}}Sortie escalated failing CI on PR #{{.PR}} after " + triesOfMax + " CI fixes" + latestCheck + ".{{end}}",
		clears: "Clears when a check of PR #{{.PR}} passes, a new commit lands on it, it merges or closes" +
			onBudget(", `reactions.ci_failure.max_retries` rises above {{count .Tries}}") + ", or " + terminalExit + ".",
	},
	"attn_escalated_review": {
		reason: "Review comments escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerTriage) + "Sortie escalated review comments on PR #{{.PR}}" + triageAsked + "." +
			"{{else}}Sortie used " + triesOfMax + " review fix runs on PR #{{.PR}}, and the pull request still holds review comments no Sortie run was given.{{end}}",
		clears: "Clears when " + ifTrigger(orchestrator.EscalationTriggerTriage) + "the review comments on PR #{{.PR}} change" +
			"{{else}}no review comment on PR #{{.PR}} is left that no Sortie run was given{{end}}, when the pull request merges or closes" +
			onBudget(", when Sortie restarts with `reactions.review_comments.max_continuation_turns` above {{count .Tries}}") +
			", or when " + terminalExit + ".",
	},
	"attn_escalated_bot_review": {
		reason: "Bot review escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerTriage) + "Sortie escalated bot review comments on PR #{{.PR}}" + triageAsked + "." +
			"{{else}}Sortie used " + triesOfMax + " bot review fix runs on PR #{{.PR}}, " +
			"and the pull request still holds bot review comments on its code that no Sortie run was given.{{end}}",
		clears: "Clears when " + ifTrigger(orchestrator.EscalationTriggerTriage) + "the bot review comments on PR #{{.PR}} change" +
			"{{else}}no bot review comment on the code of PR #{{.PR}} is left that no Sortie run was given{{end}}, when the pull request merges or closes" +
			onBudget(", when Sortie restarts with `reactions.bot_review.max_continuation_turns` above {{count .Tries}}") +
			", or when " + terminalExit + ".",
	},
	"attn_escalated_merge_conflict": {
		reason: "Merge conflict escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerTriage) + "Sortie escalated the merge conflict on PR #{{.PR}}" + triageAsked + "." +
			"{{else}}Sortie escalated the merge conflict on PR #{{.PR}} after " + triesOfMax + " rebase runs.{{end}}",
		clears: "Clears when a check finds no merge conflict on PR #{{.PR}}, a new commit lands on it, it merges or closes" +
			onBudget(", Sortie restarts with `reactions.merge_conflicts.max_retries` above {{count .Tries}}") + ", or " + terminalExit + ".",
	},
	"attn_escalated_merge": {
		reason: "Auto-merge escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerRefused) + "Sortie stopped auto-merging PR #{{.PR}} because the forge refused the merge call." +
			"{{else}}Sortie stopped auto-merging PR #{{.PR}} after " + triesOfMax + " merge calls did not merge it.{{end}}",
		clears: "Clears when PR #{{.PR}} merges or closes" +
			onBudget(", Sortie restarts with `reactions.auto_merge.max_retries` above {{count .Tries}} or at `0`") + ", or " + terminalExit + ".",
	},
	"attn_escalated_merge_completion": {
		reason: "Merge completion escalated on #{{.PR}}",
		why: ifTrigger(orchestrator.EscalationTriggerRefused) +
			"Sortie could not move the issue{{if .Target}} to {{.Target}}{{end}} after PR #{{.PR}} merged, because the tracker refused the move." +
			elseIfTrigger(orchestrator.EscalationTriggerMissingMergeCommit) +
			"PR #{{.PR}} merged, but the forge reports no merge commit for it, so Sortie has not moved the issue{{if .Target}} to {{.Target}}{{end}}." +
			"{{else}}Sortie could not move the issue{{if .Target}} to {{.Target}}{{end}} after PR #{{.PR}} merged: " +
			"{{plural .Tries \"tracker move\" \"tracker moves\"}} failed.{{end}}",
		clears: "Clears when " + terminalExit +
			ifTrigger(orchestrator.EscalationTriggerMissingMergeCommit) + ", or when the forge reports the merge commit of PR #{{.PR}}{{end}}.",
	},

	"stage_wait": {
		short: "Between stages: {{.Source}} to {{.Target}}",
		long:  "Moved from stage {{.Source}} to stage {{.Target}}, and Sortie added `{{.Label}}`; {{.Target}} starts when a poll tick reads that label on the issue.",
	},

	"hold_blocked_by": {
		short:  "Blocked by {{list .Blockers}}",
		long:   "Blocked by {{series .Blockers}}. Sortie dispatches the issue once " + blockersTerminal + ".",
		clears: "Released when " + blockersTerminal + ".",
	},
	"hold_blockers_unresolved": {
		short:  "Blockers not read: a tracker request failed",
		long:   "Sortie could not read the issue's blockers because a tracker request failed, and it dispatches no issue whose blockers it has not read. A later poll reads them again.",
		clears: "Released by the next successful read of its blockers.",
	},
	"hold_blockers_not_read": {
		short:  "Blockers not read on this poll",
		long:   "Each poll reads the blockers of a limited number of issues, taking them in turn, and this poll did not read the issue's. A later poll reads them.",
		clears: "Released when a later poll reads its blockers and finds none open.",
	},
	"hold_blockers_incomplete": {
		short:  "Blocker list incomplete",
		long:   "The tracker returned an incomplete list of the issue's blockers, and Sortie dispatches no issue whose blockers it cannot check.",
		clears: "Released when the tracker returns the whole blocker list and none is open.",
	},
	"hold_agent_slots_global": {
		short:  "No free agent slot: {{count .Used}} of {{count .Limit}} running",
		long:   "All agent slots are taken: {{count .Used}} of {{count .Limit}} under `agent.max_concurrent_agents`. Sortie dispatches the issue once a running session ends.",
		clears: "Released when a running session ends.",
	},
	"hold_agent_slots_state": {
		short:  "No free agent slot for {{.State}}: {{count .Used}} of {{count .Limit}} running",
		long:   "All agent slots for {{.State}} are taken: {{count .Used}} of {{count .Limit}} under `agent.max_concurrent_agents_by_state`. Sortie dispatches the issue once one of those sessions ends or its issue leaves {{.State}}.",
		clears: "Released when one of those sessions ends or its issue leaves {{.State}}.",
	},
	"hold_ssh_hosts": {
		short:  "No free SSH host",
		long:   "Every SSH host runs as many sessions as its per-host limit allows. Sortie dispatches the issue once a session on one of them ends.",
		clears: "Released when a session on an SSH host ends.",
	},
	"hold_spend_daily": {
		short:  "Daily spend ceiling reached" + resetsIn,
		long:   "The daily spend ceiling is reached, so Sortie starts no agent session for any issue until the UTC day ends.",
		clears: "Released once the UTC day ends, or after `agent.max_daily_cost_usd` is raised above the day's spend or removed.",
	},
	"hold_spend_weekly": {
		short:  "Weekly spend ceiling reached" + resetsIn,
		long:   "The weekly spend ceiling is reached, so Sortie starts no agent session for any issue until the UTC week ends at the start of Monday.",
		clears: "Released once the UTC week ends at the start of Monday, or after `agent.max_weekly_cost_usd` is raised above the week's spend or removed.",
	},
	"hold_spend_monthly": {
		short:  "Monthly spend ceiling reached" + resetsIn,
		long:   "The monthly spend ceiling is reached, so Sortie starts no agent session for any issue until the UTC month ends.",
		clears: "Released once the UTC month ends, or after `agent.max_monthly_cost_usd` is raised above the month's spend or removed.",
	},
	"hold_usage_limit": {
		short:  "Usage limit reached for {{if .ModelOnly}}{{or .Model \"the default model\"}} on {{end}}{{.AgentKind}}" + resetsIn,
		long:   "The agent provider refused a {{.AgentKind}} session because the account's usage limit{{if .ModelOnly}} for {{or .Model \"the default model\"}}{{end}} is reached. Sortie starts no {{.AgentKind}} session{{if .ModelOnly}} on that model{{end}} until the limit resets, and issues routed to other agents{{if .ModelOnly}} or models{{end}} keep dispatching.",
		clears: "Released once the usage limit resets.",
	},
	"hold_stage_hop": {
		short:  "Waiting to confirm the stage move",
		long:   "Sortie dispatches the next stage once a read of the issue confirms the stage move, and the latest read did not. A later poll reads the issue again.",
		clears: "Released when a later poll reads the issue in an active state.",
	},
	"hold_park_comments": {
		short:  "Waiting to read the comments since the park",
		long:   "Sortie gives the next run the comments posted since the issue was parked, and the latest read of them failed. A later poll reads them again.",
		clears: "Released when a later poll reads the issue's comments.",
	},

	"ended_handed": {
		short: "Handed off to {{.State}}",
		long:  "Handed off: Sortie moved the issue to {{.State}}.",
	},
	"ended_not_advanced": {
		short: "Handed off to {{.State}}; stage {{.Target}} not reached",
		long:  "Handed off to {{.State}} because the hop to stage {{.Target}} was not made: {{.Why}}.",
	},
	"ended_merged": {
		short: "Merged #{{.PR}}",
		long:  "Ended merged: {{.HowBefore}}{{.PR}}{{.HowAfter}}.",
	},
	"ended_done": {
		short: "Reached {{.State}}",
		long:  "Reached the tracker state {{.State}}.",
	},
	"ended_none": {
		short: "Not active; no ending recorded",
		long:  "Not active, and Sortie recorded no ending for the latest work.",
	},

	"next_if_run_succeeds": {condition: "If the run succeeds"},
	"next_if_run_fails":    {condition: "If the run fails"},
	"next_if_money_reach":  {condition: "If the money budget is reached"},
	"next_if_tokens_reach": {condition: "If tokens reach {{tokens .Budget}}"},
	"next_when_retry_due":  {condition: "When {{retry .Trigger .RunInline .Attempt}} comes due"},
	"next_when_label_read": {condition: "When a poll tick reads `{{.Label}}` on the issue"},
	"next_if_ci_fails":     {condition: "If a check" + inNext + " finds CI failing on " + thePR},
	"next_if_review_comments": {condition: "{{if .Settle}}When the new review comments on " + thePR + " settle" + inNext +
		"{{else}}If a check" + inNext + " finds new review comments on " + thePR + "{{end}}"},
	"next_if_bot_comments": {condition: "If a check" + inNext + " finds " +
		"{{if .Spent}}a new bot review comment on the code of " + thePR + "{{else}}new bot review comments on " + thePR + "{{end}}"},
	"next_if_merge_conflict": {condition: "If a check" + inNext + " finds a new merge conflict on " + thePR},
	"next_when_merge_ready": {condition: "When a check" + inNext + " finds that " + thePR + " " +
		"{{if .RequireCI}}passes CI and meets its other merge conditions{{else}}meets its merge conditions{{end}}"},
	"next_when_pr_merges": {condition: "When " + thePR + " merges"},
	"next_if_label_added": {condition: "If `{{.Label}}` is added to " + thePR},

	"next_stage":      {text: "Sortie adds `{{.Label}}`, and a poll tick then starts stage {{.Next}} in a new session."},
	"next_stage_held": {text: "Sortie adds `{{.Label}}`, then holds the issue at its " + heldAt + " before stage {{.Next}} starts."},
	"next_handoff": {text: "{{if .AtCeiling}}The hop count is at its ceiling ({{count .Hops}} of {{count .Ceiling}}), so stage {{.Next}} does not start: {{end}}" +
		"Sortie moves the issue to {{.Handoff}}" +
		"{{if .NoChange}}, or to {{.NoChange}} if the agent reports that nothing needed changing{{end}}" +
		"{{if .Watch}}, and a pull request the run leaves gets the {{series .Watch}} {{if gt (len .Watch) 1}}reactions{{else}}reaction{{end}}{{end}}."},
	"next_continue":   {text: "Sortie dispatches another session if the issue is still in an active state."},
	"next_backoff":    {text: "Sortie retries with exponential backoff."},
	"next_money_stop": {text: "Sortie stops the session and holds the issue at its money budget."},
	"next_token_stop": {text: "Sortie stops the session and holds the issue at its token budget."},
	"next_dispatch": {text: "Sortie dispatches a new session" + onStage +
		"{{if .ActiveState}} if the issue is still in an active state{{end}}."},
	"next_stage_start": {text: "Stage {{.Next}} starts in a new session."},
	"next_fix": {text: "Sortie dispatches {{.Run}} {{count .N}}" + ofMax +
		"{{if .Debounce}} once no newer comment arrives for {{duration .Debounce}}{{end}}."},
	"next_escalate": {text: "{{.Runs}} are used up ({{count .Used}} of {{count .Max}}), so Sortie " + posture + " and keeps watching the pull request." +
		"{{if .Restart}} A new commit that no Sortie run made can restart the count.{{end}}"},
	"next_triage":           {text: "Sortie runs the triage command, and its answer decides what follows."},
	"next_held":             {text: "Sortie holds the issue at its " + heldAt + "."},
	"next_auto_merge":       {text: "Auto-merge merges it with the {{.Strategy}} strategy{{if .DeleteBranch}} and deletes its branch{{end}}."},
	"next_merge_completion": {text: "Sortie moves the issue to {{.State}}."},
	"next_label_command":    {text: "Sortie dispatches {{.What}} of the pull request" + afterAhead + "."},

	"ctl_refresh":           {text: "Tracker poll and reconciliation queued."},
	"ctl_refresh_coalesced": {text: "A tracker poll and reconciliation was already queued; this request joined it."},
	"ctl_refused":           {text: "Nothing queued: Sortie is shutting down."},
	"ctl_retry_now":         {text: "Retry {{count .Attempt}} of {{.Key}} is due now."},
	"ctl_check_now":         {text: "{{.Key}}: a check of PR #{{.PR}} for {{.KindInline}} is queued for the next poll tick."},

	"ctl_acknowledge":         {text: "Acknowledged{{if .Key}} {{.Key}} ({{.Reason}}){{else}}: {{.Reason}}{{end}}. It comes back only if a new cause appears."},
	"ctl_acknowledge_already": {text: "Already acknowledged{{if .Key}} {{.Key}} ({{.Reason}}){{else}}: {{.Reason}}{{end}}. It comes back only if a new cause appears."},
	"ctl_acknowledge_refused": {text: "Nothing acknowledged: Sortie is shutting down."},

	"event_session_started":       {text: "Session started."},
	"event_startup_failed":        {text: "The session failed to start" + eventMessage + "."},
	"event_turn_completed":        {text: "Turn completed" + eventMessage + "."},
	"event_turn_failed":           {text: "Turn failed" + eventMessage + "."},
	"event_turn_cancelled":        {text: "Turn cancelled" + eventMessage + "."},
	"event_turn_ended_with_error": {text: "Turn ended with an error" + eventMessage + "."},
	"event_turn_input_required": {text: "Turn ended: {{if .Message}}{{.Message}}{{else}}" +
		"the agent asked for a decision only a person can make{{end}}."},
	"event_tool_result": {text: "Ran {{.Tool}}{{if .HasDuration}} in {{duration .Duration}}{{end}}" +
		"{{if .ToolError}}; it returned an error.{{else}}.{{end}}"},
	"event_notification":  {text: "{{.Raw}}"},
	"event_other_message": {text: "{{.Raw}}"},
	"event_malformed":     {text: "Sortie could not read a message from the agent" + eventMessage + "."},

	"run_error_absence":      {text: "The agent finished without changing anything, so Sortie did not hand the issue off."},
	"run_error_undetermined": {text: "Sortie could not tell whether the agent changed anything, so it did not hand the issue off."},
	"run_error_withheld":     {text: "Sortie did not hand the issue off" + errorDetail},

	"run_error_agent_not_found":       {text: "Sortie could not find the agent command" + errorDetail},
	"run_error_invalid_workspace_cwd": {text: "The agent could not use the workspace directory" + errorDetail},
	"run_error_response_timeout":      {text: "The agent did not respond in time" + errorDetail},
	"run_error_turn_timeout":          {text: "The agent did not finish {{.Turn}} within the turn time limit, so Sortie stopped it."},
	"run_error_port_exit":             {text: "The agent process ended unexpectedly" + errorDetail},
	"run_error_response_error":        {text: "The agent returned an error" + errorDetail},
	"run_error_turn_failed":           {text: "The agent reported that {{.Turn}} failed" + errorDetail},
	"run_error_turn_cancelled":        {text: "Sortie stopped the session before the agent finished {{if .Start}}starting{{else}}{{.Turn}}{{end}}."},
	"run_error_turn_input_required":   {text: "The agent stopped {{.Turn}} and asked for input" + errorDetail},
	"run_error_turn_incomplete":       {text: "The agent ended {{.Turn}} without finishing the task" + errorDetail},
	"run_error_turn_refused":          {text: "The agent declined to continue {{.Turn}}" + errorDetail},
	"run_error_turn_outcome_unknown":  {text: "The agent ended {{.Turn}} with an outcome Sortie could not read" + errorDetail},
	"run_error_turn_token_limit":      {text: "The agent ended {{.Turn}} because a token limit was reached" + errorDetail},
	"run_error_turn_request_limit":    {text: "The agent ended {{.Turn}} because it used up its request limit" + errorDetail},
	"run_error_usage_limit":           {text: "The agent provider refused the run because the account's usage limit is reached" + errorDetail},
	"run_error_credential_unverified": {text: "The agent's credential was refused or could not be confirmed" + errorDetail},
	"run_error_agent_other":           {text: "The agent reported an error" + errorDetail},
	"run_error_turn_ended":            {text: "The agent ended {{.Turn}} early{{if .Detail}} ({{.Detail}}){{end}}."},
	"run_error_stage_stopped":         {text: "Sortie stopped the run while {{.Doing}}."},
	"run_error_stage_timeout":         {text: "Sortie gave up on {{.Doing}} because it took too long."},
	"run_error_stage":                 {text: "Sortie hit an error while {{.Doing}}" + errorDetail},
	"run_error_panic":                 {text: "Sortie hit an internal error while running the agent" + errorDetail},
	"run_error_stopped":               {text: "Sortie stopped the run before it finished."},
	"run_error_ci_failed":             {text: "The CI checks failed on commit {{.Ref}}."},
	"run_error_money_ceiling":         {text: "Sortie stopped the run because the issue's estimated cost, {{.Used}}, reached its limit of {{.Budget}}."},
	"run_error_token_ceiling":         {text: "Sortie stopped the run because the issue used {{tokens .Used}} tokens, reaching its limit of {{tokens .Budget}}."},
	"run_error_stall":                 {text: "The agent sent no event within the stall timeout, so Sortie stopped the run."},
	"run_error_shutdown":              {text: "Sortie stopped the run because the service was shutting down."},
	"run_error_terminal":              {text: "Sortie stopped the run because the issue moved to {{.State}}, a final state."},
	"run_error_inactive":              {text: "Sortie stopped the run because the issue is no longer in an active state{{if .State}} (now {{.State}}){{end}}."},
	"run_error_out_of_time":           {text: "The run ran out of time."},

	"stall_marker": {
		short: "No events for {{elapsed .Silence}}; Sortie stops it after {{duration .Timeout}}",
		long: "No agent event for at least {{elapsed .Silence}}; Sortie stops the session as stalled after " +
			"{{duration .Timeout}} without one (`agent.stall_timeout_ms`).",
	},
	"stall_off": {text: "Stall detection is off; Sortie will not stop this session as stalled (`agent.stall_timeout_ms`)."},

	"health_poll_unrun": {text: "Sortie has not polled the tracker yet."},
	"health_poll_pass":  {text: "The latest poll read the tracker without an error."},
	"health_poll_fail": {text: "{{if gt .Failures 1}}{{count .Failures}} polls in a row failed, and the latest{{else}}The latest poll{{end}} " +
		"{{if .Preflight}}stopped at the dispatch preflight before reading the tracker{{else}}could not read the tracker{{end}}; " +
		"{{if .HasSuccess}}the last successful poll was {{duration .Ago}} ago{{else}}no poll has succeeded since Sortie started{{end}}."},
}

// templates is the parsed table. A source that does not parse panics at
// package initialization, so a broken table never reaches a request.
var templates = mustParse(table)

func mustParse(entries map[string]parts) *template.Template {
	root := template.New("sentence").Option("missingkey=error").Funcs(funcs)
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		for _, part := range []struct{ name, source string }{
			{"short", entry.short},
			{"long", entry.long},
			{"text", entry.text},
			{"condition", entry.condition},
			{"reason", entry.reason},
			{"why", entry.why},
			{"clears", entry.clears},
		} {
			if part.source == "" {
				continue
			}
			if _, err := root.New(name + "." + part.name).Parse(part.source); err != nil {
				panic(fmt.Sprintf("sentence template %s: %v", name, err))
			}
		}
	}
	return root
}

// pausedReasons words the Error text of a paused retry.
var pausedReasons = map[string]string{
	"reaction retry paused by an open blocking issue": "an open issue blocks it",
	"reaction retry paused by issue state":            "the issue's tracker state does not allow it",
}

// rescheduleReasons words the Error text of a retry that came due and was
// not dispatched.
var rescheduleReasons = map[string]string{
	"worker still running":            "its previous run was still stopping",
	"retry issue fetch failed":        "re-reading the issue from the tracker failed",
	"issue without a state label":     "the issue carried no state label",
	"no available orchestrator slots": "no agent slot was free",
	"no available SSH hosts":          "no SSH host had a free slot",
	"retry agent kind unavailable":    "its agent kind is not available",
	"retry agent settings refused":    "its agent settings were refused",
}

// runPhrase names the run a reaction retry starts, sentence-initial and
// inline.
type runPhrase struct {
	initial string
	inline  string
}

// runPhrases is keyed by reaction kind.
var runPhrases = map[string]runPhrase{
	orchestrator.ReactionKindCI:            {"CI fix run", "CI fix run"},
	orchestrator.ReactionKindReview:        {"Review fix run", "review fix run"},
	orchestrator.ReactionKindBotReview:     {"Bot review fix run", "bot review fix run"},
	orchestrator.ReactionKindMergeConflict: {"Rebase run", "rebase run"},
	orchestrator.ReactionKindLabelReview:   {"Label review run", "label review run"},
	orchestrator.ReactionKindLabelFix:      {"Label fix run", "label fix run"},
}

// kindWords names a reaction kind sentence-initial and inline, and holds the
// label of the count of tries it has used; count is empty for a kind that
// counts none.
type kindWords struct {
	initial string
	inline  string
	count   string
}

// reactionKindWords is keyed by reaction kind.
var reactionKindWords = map[string]kindWords{
	orchestrator.ReactionKindCI:              {"CI", "CI", "CI fixes used"},
	orchestrator.ReactionKindReview:          {"Review comments", "review comments", "Review fix runs used"},
	orchestrator.ReactionKindBotReview:       {"Bot review", "bot review", "Bot review fix runs used"},
	orchestrator.ReactionKindAutoMerge:       {"Auto-merge", "auto-merge", "Merge calls made"},
	orchestrator.ReactionKindMergeConflict:   {"Merge conflict", "the merge conflict", "Rebase runs used"},
	orchestrator.ReactionKindMergeCompletion: {"Merge completion", "merge completion", "Failed tracker moves"},
	orchestrator.ReactionKindLabelReview:     {"Review label command", "the review label command", ""},
	orchestrator.ReactionKindLabelFix:        {"Fix label command", "the fix label command", ""},
}

// otherReactionKind words a kind the table does not know.
var otherReactionKind = kindWords{"Reaction", "the reaction", ""}

// waitClauses leads the next-check clause of a pending reaction, keyed by the
// entry's Wait value; the empty key is an entry no pass has taken yet.
var waitClauses = map[string]string{
	"":           "first check",
	"backoff":    "the last check did not complete; next check",
	"retry_slot": "another run or retry holds the issue; next check",
	"triage":     "it waits on its triage command; next check",
}

// otherWaitClause leads the next-check clause of any other Wait value.
const otherWaitClause = "next check"

// mergeability words what a merge-conflict reaction knows of the pull
// request, keyed by the state of its last check.
type mergeability string

const (
	mergeabilityUnchecked mergeability = "unchecked"
	mergeabilityUnknown   mergeability = "unknown"
	mergeabilityClear     mergeability = "clear"
)

var mergeabilityWords = map[mergeability]string{
	mergeabilityUnchecked: "no check has completed yet",
	mergeabilityUnknown:   "the provider has not computed mergeability yet",
	mergeabilityClear:     "the last check found none",
}

// mergePhrase words how a pull request merged around its number.
type mergePhrase struct {
	before string
	after  string
}

// mergePaths is keyed by the merge path an outcome serves.
var mergePaths = map[string]mergePhrase{
	"auto_merge":       {"auto-merge merged PR #", ""},
	"merge_completion": {"merge completion saw PR #", " merge"},
}

// otherMergePath words a merge path the table does not know.
var otherMergePath = mergePhrase{"PR #", " merged"}

// hopResults words why a stage hop was not made, keyed by the hop result.
var hopResults = map[string]string{
	"ceiling": "the issue reached its limit of consecutive stage moves (`dispatch.max_consecutive_hops`)",
	"failed":  "the next stage's label could not be added to the issue",
}

// fixWords words what a reaction kind runs when its watch finds its event.
type fixWords struct {
	run     string
	runs    string
	restart bool // a head no Sortie run made resets the count
}

// fixVocabulary is keyed by the reaction kinds that count tries and run a fix.
var fixVocabulary = map[string]fixWords{
	orchestrator.ReactionKindCI:            {run: "CI fix", runs: "CI fixes", restart: true},
	orchestrator.ReactionKindReview:        {run: "review fix", runs: "Review fixes"},
	orchestrator.ReactionKindBotReview:     {run: "bot review fix", runs: "Bot review fixes"},
	orchestrator.ReactionKindMergeConflict: {run: "rebase", runs: "Rebases", restart: true},
}

// watchNames names, in the order a sentence lists them, what each reactions
// member watches for.
var watchNames = []struct {
	configKey string
	name      string
}{
	{"ci_failure", "CI failure"},
	{"review_comments", "review comments"},
	{"bot_review", "bot review"},
	{"merge_conflicts", "merge conflicts"},
	{"auto_merge", "auto-merge"},
	{"merge_completion", "merge completion"},
	{"label_commands", "label commands"},
}

// labelCommandWords words what a label command of each reaction kind
// dispatches.
var labelCommandWords = map[string]string{
	orchestrator.ReactionKindLabelReview: "a read-only review",
	orchestrator.ReactionKindLabelFix:    "a fix",
}
