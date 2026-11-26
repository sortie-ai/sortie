package sentence

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

var attentionAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type attentionInput struct {
	snap   orchestrator.RuntimeSnapshotResult
	health *orchestrator.SnapshotHealth
}

func parkedInput(edit func(*orchestrator.SnapshotParkedEntry)) attentionInput {
	row := orchestrator.SnapshotParkedEntry{
		IssueID: "id-652", Identifier: "MT-652", Reason: "agent_blocked", Label: "needs-human",
		LabelApplied: true, ParkedAt: attentionAt,
	}
	if edit != nil {
		edit(&row)
	}
	return attentionInput{snap: orchestrator.RuntimeSnapshotResult{Parked: []orchestrator.SnapshotParkedEntry{row}}}
}

func budgetInput(edit func(*orchestrator.SnapshotBudgetEntry)) attentionInput {
	row := orchestrator.SnapshotBudgetEntry{
		IssueID: "id-651", Identifier: "MT-651", Reason: "session_budget",
		UsedSessions: 3, BudgetSessions: 3, ExhaustedAt: attentionAt,
	}
	if edit != nil {
		edit(&row)
	}
	return attentionInput{snap: orchestrator.RuntimeSnapshotResult{BudgetExhausted: []orchestrator.SnapshotBudgetEntry{row}}}
}

func escalatedInput(kind, trigger string, edit func(*orchestrator.SnapshotPendingReaction)) attentionInput {
	row := orchestrator.SnapshotPendingReaction{
		IssueID: "id-1", Identifier: "MT-1", IssueURL: "https://tracker.example.com/MT-1",
		Kind: kind, ConfigKey: orchestrator.ReactionConfigKey(kind), EscalationTrigger: trigger,
		PullRequest:  orchestrator.SnapshotPullRequest{Number: 931, URL: "https://github.com/acme/web/pull/931"},
		UsedAttempts: new(2), MaxAttempts: new(2), EscalatedAt: new(attentionAt),
	}
	if edit != nil {
		edit(&row)
	}
	return attentionInput{snap: orchestrator.RuntimeSnapshotResult{PendingReactions: []orchestrator.SnapshotPendingReaction{row}}}
}

func failingHealth(workflowSince, preflightSince time.Time, workflowLoadFailed bool) *orchestrator.SnapshotHealth {
	health := &orchestrator.SnapshotHealth{}
	if !workflowSince.IsZero() {
		health.Workflow = orchestrator.SnapshotWorkflowHealth{
			File: "WORKFLOW.md",
			SnapshotHealthCheck: orchestrator.SnapshotHealthCheck{
				ConsecutiveFailures: 2, FailingSince: workflowSince, Error: "config: agent.turn_timeout_ms: must be greater than 0",
			},
		}
	}
	if !preflightSince.IsZero() {
		health.Preflight = orchestrator.SnapshotPreflightHealth{
			WorkflowLoadFailed: workflowLoadFailed,
			SnapshotHealthCheck: orchestrator.SnapshotHealthCheck{
				ConsecutiveFailures: 1, FailingSince: preflightSince, Error: "dispatch preflight failed: tracker.kind is required",
			},
		}
	}
	return health
}

func healthInput(health *orchestrator.SnapshotHealth) attentionInput {
	return attentionInput{health: health}
}

func TestAttentionDerivesItemsByCause(t *testing.T) {
	t.Parallel()

	withURL := func(r *orchestrator.SnapshotParkedEntry) { r.IssueURL = "https://tracker.example.com/MT-652" }
	noPRURL := func(r *orchestrator.SnapshotPendingReaction) { r.PullRequest.URL = "" }
	noURLs := func(r *orchestrator.SnapshotPendingReaction) { r.PullRequest.URL, r.IssueURL = "", "" }
	notEscalated := func(r *orchestrator.SnapshotPendingReaction) { r.EscalatedAt = nil }
	const issueURL, prURL = "https://tracker.example.com/MT-1", "https://github.com/acme/web/pull/931"

	tests := []struct {
		name  string
		input attentionInput
		want  []string
	}{
		{name: "clean snapshot and nil health", input: attentionInput{}, want: []string{}},
		{name: "clean snapshot and passing health", input: attentionInput{health: &orchestrator.SnapshotHealth{}}, want: []string{}},
		{name: "park with an issue URL opens the issue", input: parkedInput(withURL), want: []string{"parked/warning/open_issue/https://tracker.example.com/MT-652"}},
		{
			name:  "handoff absence park without a URL asks for acknowledgement",
			input: parkedInput(func(r *orchestrator.SnapshotParkedEntry) { r.Reason = "handoff_absence" }),
			want:  []string{"parked/warning/acknowledge/"},
		},
		{
			name:  "token hold",
			input: budgetInput(func(r *orchestrator.SnapshotBudgetEntry) { r.Reason, r.BudgetTokens = "token_budget", 2_000_000 }),
			want:  []string{"budget_held/warning/acknowledge/"},
		},
		{name: "session hold", input: budgetInput(nil), want: []string{"budget_held/warning/acknowledge/"}},
		{name: "escalation clears with escalated_at", input: escalatedInput("ci", "triage", notEscalated), want: []string{}},
		{name: "escalation opens the pull request", input: escalatedInput("review", "triage", nil), want: []string{"reaction_escalated/warning/open_pull_request/" + prURL}},
		{name: "escalation without a pull request URL opens the issue", input: escalatedInput("review", "triage", noPRURL), want: []string{"reaction_escalated/warning/open_issue/" + issueURL}},
		{name: "escalation without any URL asks for acknowledgement", input: escalatedInput("review", "triage", noURLs), want: []string{"reaction_escalated/warning/acknowledge/"}},
		{
			name:  "merge completion opens the issue though its pull request has a URL",
			input: escalatedInput("merge-completion", "refused", nil),
			want:  []string{"reaction_escalated/warning/open_issue/" + issueURL},
		},
		{
			name:  "workflow reload failure is critical",
			input: healthInput(failingHealth(attentionAt, time.Time{}, false)),
			want:  []string{"workflow_reload_failed/critical/acknowledge/"},
		},
		{
			name:  "preflight failure other than the workflow load",
			input: healthInput(failingHealth(time.Time{}, attentionAt, false)),
			want:  []string{"preflight_failed/critical/acknowledge/"},
		},
		{
			name:  "preflight failing on the workflow load yields only the reload item",
			input: healthInput(failingHealth(attentionAt, attentionAt, true)),
			want:  []string{"workflow_reload_failed/critical/acknowledge/"},
		},
		{
			name:  "preflight keeping the load error after the workflow loads yields nothing",
			input: healthInput(failingHealth(time.Time{}, attentionAt, true)),
			want:  []string{},
		},
	}
	for _, kind := range orchestrator.EscalatingReactionKinds() {
		want := "reaction_escalated/warning/open_pull_request/" + prURL
		if kind == orchestrator.ReactionKindMergeCompletion {
			want = "reaction_escalated/warning/open_issue/" + issueURL
		}
		tests = append(tests, struct {
			name  string
			input attentionInput
			want  []string
		}{name: "escalated " + kind, input: escalatedInput(kind, "budget_exhausted", nil), want: []string{want}})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Attention(tt.input.snap, tt.input.health, Context{})

			if got == nil {
				t.Fatal("Attention() = nil, want a non-nil list")
			}
			var summary []string
			for _, item := range got {
				summary = append(summary, item.Kind+"/"+item.Severity+"/"+item.Action.Kind+"/"+item.Action.URL)
				for name, text := range map[string]string{"ReasonText": item.ReasonText, "WhyText": item.WhyText, "ClearsText": item.ClearsText} {
					if text == "" || text == fallbackText {
						t.Errorf("Attention() %s of %s = %q, want its rendered text", name, item.Kind, text)
					}
				}
			}
			if !slices.Equal(summary, tt.want) {
				t.Errorf("Attention() kind/severity/action = %v, want %v", summary, tt.want)
			}
		})
	}
}

func TestAttentionID(t *testing.T) {
	t.Parallel()

	later := attentionAt.Add(time.Hour)
	ciFailing := func(r *orchestrator.SnapshotPendingReaction) {
		r.LastCheck = &orchestrator.ReactionCheck{CI: &orchestrator.CICheck{Status: "failing", FailingChecks: []string{"Test"}, Failing: 1}}
	}
	ciPendingLater := func(r *orchestrator.SnapshotPendingReaction) {
		r.NextPollAt = later
		r.LastCheck = &orchestrator.ReactionCheck{CI: &orchestrator.CICheck{Status: "pending", Pending: 1}}
	}
	atNanos := func(r *orchestrator.SnapshotParkedEntry) { r.ParkedAt = attentionAt.Add(123456789 * time.Nanosecond) }
	parkedLater := func(r *orchestrator.SnapshotParkedEntry) { r.ParkedAt = later }
	ceiling := func(n int) func(*orchestrator.SnapshotBudgetEntry) {
		return func(r *orchestrator.SnapshotBudgetEntry) { r.BudgetSessions = n }
	}

	tests := []struct {
		name     string
		a, b     attentionInput
		wantSame bool
	}{
		{"park restored at whole seconds", parkedInput(atNanos), parkedInput(nil), true},
		{"escalation after a later poll and a check moving to pending", escalatedInput("ci", "budget_exhausted", ciFailing), escalatedInput("ci", "budget_exhausted", ciPendingLater), true},
		{"new park time", parkedInput(nil), parkedInput(parkedLater), false},
		{"new ceiling", budgetInput(nil), budgetInput(ceiling(4)), false},
		{
			"new escalation time",
			escalatedInput("ci", "budget_exhausted", nil),
			escalatedInput("ci", "budget_exhausted", func(r *orchestrator.SnapshotPendingReaction) { r.EscalatedAt = new(later) }),
			false,
		},
		{
			"new workflow failing_since",
			healthInput(failingHealth(attentionAt, time.Time{}, false)),
			healthInput(failingHealth(later, time.Time{}, false)),
			false,
		},
		{
			"new preflight failing_since",
			healthInput(failingHealth(time.Time{}, attentionAt, false)),
			healthInput(failingHealth(time.Time{}, later, false)),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := Attention(tt.a.snap, tt.a.health, Context{})
			b := Attention(tt.b.snap, tt.b.health, Context{})

			if len(a) != 1 || len(b) != 1 {
				t.Fatalf("Attention() = %d and %d items, want 1 each", len(a), len(b))
			}
			if got := a[0].ID == b[0].ID; got != tt.wantSame {
				t.Errorf("Attention(%s) ids equal = %t, want %t", tt.name, got, tt.wantSame)
			}
			if tt.wantSame && !a[0].Since.Equal(*b[0].Since) {
				t.Errorf("Attention(%s) since = %v and %v, want equal", tt.name, a[0].Since, b[0].Since)
			}
		})
	}
}

func TestAttentionOrder(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		Parked: []orchestrator.SnapshotParkedEntry{
			{IssueID: "p2", Identifier: "MT-2", Reason: "agent_blocked", Label: "l", ParkedAt: attentionAt},
			{IssueID: "p1", Identifier: "MT-1", Reason: "agent_blocked", Label: "l", ParkedAt: attentionAt},
			{IssueID: "p0", Identifier: "MT-0", Reason: "agent_blocked", Label: "l"},
		},
		BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
			{IssueID: "b1", Identifier: "MT-3", Reason: "session_budget", UsedSessions: 1, BudgetSessions: 1, ExhaustedAt: attentionAt.Add(time.Minute)},
		},
		PendingReactions: escalatedInput("review", "triage", func(r *orchestrator.SnapshotPendingReaction) {
			r.IssueID, r.EscalatedAt = "e1", new(attentionAt.Add(2*time.Minute))
		}).snap.PendingReactions,
	}
	health := failingHealth(attentionAt.Add(time.Hour), attentionAt, false)

	got := Attention(snap, health, Context{})

	var order []string
	for _, item := range got {
		order = append(order, item.Kind+":"+item.IssueID)
	}
	wantHead := []string{"preflight_failed:", "workflow_reload_failed:", "parked:p0"}
	wantTail := []string{"budget_held:b1", "reaction_escalated:e1"}
	if len(order) != 7 || !slices.Equal(order[:3], wantHead) || !slices.Equal(order[5:], wantTail) {
		t.Fatalf("Attention() order = %v, want %v, then the park tie, then %v", order, wantHead, wantTail)
	}
	if tie := order[3:5]; !slices.Contains(tie, "parked:p1") || !slices.Contains(tie, "parked:p2") || got[3].ID >= got[4].ID {
		t.Errorf("Attention() tie = %v with ids %q, %q, want parks p1 and p2 by ascending id", tie, got[3].ID, got[4].ID)
	}
}

func TestAttentionTexts(t *testing.T) {
	t.Parallel()

	const handoffTail = "or the issue reaches a terminal tracker state or leaves In Review."
	shape := &orchestrator.WorkflowShape{
		Tracker:   orchestrator.ShapeTracker{HandoffState: "In Review"},
		Reactions: orchestrator.ShapeReactions{MergeCompletion: &orchestrator.ShapeMergeCompletion{TargetState: "Done"}},
	}
	const (
		budget   = "budget_exhausted"
		triage   = "triage"
		refused  = "refused"
		noCommit = "missing_merge_commit"
	)
	ciCheck := func(ci orchestrator.CICheck) func(*orchestrator.SnapshotPendingReaction) {
		return func(r *orchestrator.SnapshotPendingReaction) { r.LastCheck = &orchestrator.ReactionCheck{CI: &ci} }
	}
	noMax := func(r *orchestrator.SnapshotPendingReaction) { r.MaxAttempts = nil }

	type texts struct{ reason, why, clears string }
	tests := []struct {
		name  string
		input attentionInput
		shape *orchestrator.WorkflowShape
		want  texts
	}{
		{"agent_blocked park with the label seen, parked in a state", parkedInput(func(r *orchestrator.SnapshotParkedEntry) { r.ParkedState = "In Progress" }), nil, texts{
			"Parked: the agent needs a person",
			"The agent stopped because it cannot go on without a person, so Sortie parked the issue under the label `needs-human`.",
			"Clears when a poll tick finds `needs-human` removed from the issue, or the issue moved out of In Progress.",
		}},
		{"handoff_absence park with the label unseen", parkedInput(func(r *orchestrator.SnapshotParkedEntry) { r.Reason, r.LabelApplied = "handoff_absence", false }), nil, texts{
			"Parked: no evidence of work",
			"The issue's runs ended without evidence of work as many times in a row as `agent.max_consecutive_absences` allows, so Sortie parked it under the label `needs-human` (not yet seen on the issue).",
			"Clears when a poll tick finds `needs-human` removed from the issue after Sortie has seen it there.",
		}},
		{"token hold with a lower-bound spend", budgetInput(func(r *orchestrator.SnapshotBudgetEntry) {
			r.IssueID, r.Reason, r.UsedTokens, r.BudgetTokens = "id-651", "token_budget", new(int64(2_020_000)), 2_000_000
			r.UnmeasuredSessions, r.StoppedInFlight = new(1), new(1)
		}), nil, texts{
			"Token budget reached",
			"The issue has used at least 2.02M tokens of its budget of 2M, and the budget stopped 1 session mid-run.",
			"Clears on the next poll tick after `agent.max_tokens` is raised above what the issue has used or is removed, or after the issue leaves the active tracker states.",
		}},
		{"token hold with a complete spend and no stopped session", budgetInput(func(r *orchestrator.SnapshotBudgetEntry) {
			r.Reason, r.UsedTokens, r.BudgetTokens = "token_budget", new(int64(2_020_000)), 2_000_000
		}), nil, texts{why: "The issue has used 2.02M tokens of its budget of 2M."}},
		{"session hold", budgetInput(nil), nil, texts{
			"Session budget reached",
			"The issue has used 3 sessions of its budget of 3.",
			"Clears on the next poll tick after `agent.max_sessions` is raised above 3 or is removed, or after the issue leaves the active tracker states.",
		}},
		{"workflow reload failure", healthInput(failingHealth(attentionAt, time.Time{}, false)), nil, texts{
			"WORKFLOW.md failed to load",
			"WORKFLOW.md failed to load (config: agent.turn_timeout_ms: must be greater than 0), so poll ticks dispatch nothing until it loads, and the configuration that last loaded stays in effect.",
			"Clears when WORKFLOW.md loads again, which Sortie tries each time the file is saved and on every poll tick.",
		}},
		{"preflight failure drops the lead", healthInput(failingHealth(time.Time{}, attentionAt, false)), nil, texts{
			"Dispatch preflight failed",
			"The dispatch preflight failed (tracker.kind is required), so poll ticks dispatch nothing until it passes.",
			"Clears on the first poll tick whose dispatch preflight passes.",
		}},
		{"ci budget escalation naming the failing check", escalatedInput("ci", budget, ciCheck(orchestrator.CICheck{Status: "failing", FailingChecks: []string{"Test"}, Failing: 1})), shape, texts{
			"CI escalated on #931",
			"Sortie escalated failing CI on PR #931 after 2 of 2 CI fixes; the latest check found Test failing.",
			"Clears when a check of PR #931 passes, a new commit lands on it, it merges or closes, `reactions.ci_failure.max_retries` rises above 2, " + handoffTail,
		}},
		{"ci budget escalation counting unnamed failing checks", escalatedInput("ci", budget, ciCheck(orchestrator.CICheck{Status: "failing", Failing: 3})), shape, texts{
			why: "Sortie escalated failing CI on PR #931 after 2 of 2 CI fixes; the latest check found 3 failing checks.",
		}},
		{"ci triage escalation with a pending latest check", escalatedInput("ci", triage, ciCheck(orchestrator.CICheck{Status: "pending", Pending: 1})), shape, texts{
			why:    "Sortie escalated failing CI on PR #931 because the triage command asked for a person; the latest check found CI pending.",
			clears: "Clears when a check of PR #931 passes, a new commit lands on it, it merges or closes, " + handoffTail,
		}},
		{"ci budget escalation with no max, no check, and no shape", escalatedInput("ci", budget, noMax), nil, texts{
			why:    "Sortie escalated failing CI on PR #931 after 2 CI fixes.",
			clears: "Clears when a check of PR #931 passes, a new commit lands on it, it merges or closes, `reactions.ci_failure.max_retries` rises above 2, or the issue reaches a terminal tracker state.",
		}},
		{"review budget escalation", escalatedInput("review", budget, nil), shape, texts{
			"Review comments escalated on #931",
			"Sortie used 2 of 2 review fix runs on PR #931, and the pull request still holds review comments no Sortie run was given.",
			"Clears when no review comment on PR #931 is left that no Sortie run was given, when the pull request merges or closes, when Sortie restarts with `reactions.review_comments.max_continuation_turns` above 2, or when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"review triage escalation", escalatedInput("review", triage, func(r *orchestrator.SnapshotPendingReaction) { r.Identifier, r.PullRequest.Number = "MT-654", 940 }), shape, texts{
			why:    "Sortie escalated review comments on PR #940 because the triage command asked for a person.",
			clears: "Clears when the review comments on PR #940 change, when the pull request merges or closes, or when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"bot review budget escalation", escalatedInput("bot-review", budget, nil), shape, texts{
			"Bot review escalated on #931",
			"Sortie used 2 of 2 bot review fix runs on PR #931, and the pull request still holds bot review comments on its code that no Sortie run was given.",
			"Clears when no bot review comment on the code of PR #931 is left that no Sortie run was given, when the pull request merges or closes, when Sortie restarts with `reactions.bot_review.max_continuation_turns` above 2, or when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"bot review triage escalation", escalatedInput("bot-review", triage, nil), shape, texts{
			why:    "Sortie escalated bot review comments on PR #931 because the triage command asked for a person.",
			clears: "Clears when the bot review comments on PR #931 change, when the pull request merges or closes, or when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"merge conflict budget escalation", escalatedInput("merge-conflict", budget, nil), shape, texts{
			"Merge conflict escalated on #931",
			"Sortie escalated the merge conflict on PR #931 after 2 of 2 rebase runs.",
			"Clears when a check finds no merge conflict on PR #931, a new commit lands on it, it merges or closes, Sortie restarts with `reactions.merge_conflicts.max_retries` above 2, " + handoffTail,
		}},
		{"merge conflict triage escalation", escalatedInput("merge-conflict", triage, nil), shape, texts{
			why:    "Sortie escalated the merge conflict on PR #931 because the triage command asked for a person.",
			clears: "Clears when a check finds no merge conflict on PR #931, a new commit lands on it, it merges or closes, " + handoffTail,
		}},
		{"auto-merge budget escalation", escalatedInput("merge", budget, nil), shape, texts{
			"Auto-merge escalated on #931",
			"Sortie stopped auto-merging PR #931 after 2 of 2 merge calls did not merge it.",
			"Clears when PR #931 merges or closes, Sortie restarts with `reactions.auto_merge.max_retries` above 2 or at `0`, " + handoffTail,
		}},
		{"auto-merge refused escalation", escalatedInput("merge", refused, nil), shape, texts{
			why:    "Sortie stopped auto-merging PR #931 because the forge refused the merge call.",
			clears: "Clears when PR #931 merges or closes, " + handoffTail,
		}},
		{"merge completion budget escalation", escalatedInput("merge-completion", budget, nil), shape, texts{
			"Merge completion escalated on #931",
			"Sortie could not move the issue to Done after PR #931 merged: 2 tracker moves failed.",
			"Clears when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"merge completion refused escalation", escalatedInput("merge-completion", refused, func(r *orchestrator.SnapshotPendingReaction) { r.Identifier, r.PullRequest.Number = "MT-655", 950 }), shape, texts{
			why:    "Sortie could not move the issue to Done after PR #950 merged, because the tracker refused the move.",
			clears: "Clears when the issue reaches a terminal tracker state or leaves In Review.",
		}},
		{"merge completion escalation on a missing merge commit", escalatedInput("merge-completion", noCommit, nil), shape, texts{
			why:    "PR #931 merged, but the forge reports no merge commit for it, so Sortie has not moved the issue to Done.",
			clears: "Clears when the issue reaches a terminal tracker state or leaves In Review, or when the forge reports the merge commit of PR #931.",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Attention(tt.input.snap, tt.input.health, Context{Shape: tt.shape})

			if len(got) != 1 {
				t.Fatalf("Attention(%s) = %d items, want 1", tt.name, len(got))
			}
			for _, part := range []struct{ name, got, want string }{
				{"ReasonText", got[0].ReasonText, tt.want.reason},
				{"WhyText", got[0].WhyText, tt.want.why},
				{"ClearsText", got[0].ClearsText, tt.want.clears},
			} {
				if part.want != "" && part.got != part.want {
					t.Errorf("Attention(%s) %s = %q, want %q", tt.name, part.name, part.got, part.want)
				}
			}
		})
	}
}

func TestAttentionSpendWordingPerWindow(t *testing.T) {
	t.Parallel()

	for _, window := range orchestrator.SpendWindows() {
		t.Run(string(window), func(t *testing.T) {
			t.Parallel()

			snap := orchestrator.RuntimeSnapshotResult{SpendCeilings: []orchestrator.SpendCeilingStatus{
				{Window: window, BudgetUSD: 7, UsedUSD: 7, Holding: true, ResetsAt: attentionAt},
			}}

			got := Attention(snap, nil, Context{})

			if len(got) != 1 {
				t.Fatalf("Attention(%s hold) = %d items, want 1", window, len(got))
			}
			wantPrefix := strings.ToUpper(string(window)[:1]) + string(window)[1:] + " "
			if !strings.HasPrefix(got[0].ReasonText, wantPrefix) {
				t.Errorf("Attention(%s hold) ReasonText = %q, want prefix %q", window, got[0].ReasonText, wantPrefix)
			}
			for _, text := range []string{got[0].WhyText, got[0].ClearsText} {
				if text == fallbackText {
					t.Errorf("Attention(%s hold) text = %q, want the window's wording", window, text)
				}
			}
			if !strings.Contains(got[0].ClearsText, window.Setting()) {
				t.Errorf("Attention(%s hold) ClearsText = %q, want it to name %q", window, got[0].ClearsText, window.Setting())
			}
		})
	}
}
