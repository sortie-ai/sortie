package sentence

import (
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

var outcomeNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func fixtureShape(reactions orchestrator.ShapeReactions) *orchestrator.WorkflowShape {
	return &orchestrator.WorkflowShape{
		Tracker: orchestrator.ShapeTracker{HandoffState: "Human Review"},
		Polling: orchestrator.ShapePolling{IntervalMS: 30000},
		Dispatch: orchestrator.ShapeDispatch{
			Rules: []orchestrator.ShapeRule{
				{Name: "specify", Next: "implement"},
				{Name: "implement", Stage: "stage-implement"},
			},
			HopCeiling: 10,
		},
		Reactions: reactions,
	}
}

func withSessionBudget(s *orchestrator.WorkflowShape, sessions int) *orchestrator.WorkflowShape {
	s.Agent.MaxSessions = &sessions
	return s
}

func workSession(rule string, hops int) *orchestrator.SnapshotRunningEntry {
	return &orchestrator.SnapshotRunningEntry{
		Identifier:    "MT-654",
		RuleName:      rule,
		StagePosition: &orchestrator.SnapshotStagePosition{HopCount: hops},
	}
}

func pendingRow(kind string, edit func(*orchestrator.SnapshotPendingReaction)) []orchestrator.SnapshotPendingReaction {
	row := orchestrator.SnapshotPendingReaction{
		Identifier:  "MT-1",
		Kind:        kind,
		ConfigKey:   orchestrator.ReactionConfigKey(kind),
		PullRequest: orchestrator.SnapshotPullRequest{Number: 88},
	}
	if edit != nil {
		edit(&row)
	}
	return []orchestrator.SnapshotPendingReaction{row}
}

func watchedFor(window time.Duration, used, limit int) func(*orchestrator.SnapshotPendingReaction) {
	return func(r *orchestrator.SnapshotPendingReaction) {
		r.WatchEndsAt = new(outcomeNow.Add(window))
		r.UsedAttempts = &used
		r.MaxAttempts = &limit
	}
}

func TestNextOutcomes(t *testing.T) {
	t.Parallel()

	noReactions := orchestrator.ShapeReactions{}
	watchReactions := orchestrator.ShapeReactions{
		CIFailure: &orchestrator.ShapeCIFailure{},
		AutoMerge: &orchestrator.ShapeAutoMerge{},
	}
	ciLabelEscalation := orchestrator.ShapeEscalation{Escalation: "label", EscalationLabel: "needs-human"}
	ci := orchestrator.ShapeReactions{CIFailure: &orchestrator.ShapeCIFailure{ShapeEscalation: ciLabelEscalation}}
	ciTriage := orchestrator.ShapeReactions{CIFailure: &orchestrator.ShapeCIFailure{Triage: true, ShapeEscalation: ciLabelEscalation}}
	review := orchestrator.ShapeReactions{ReviewComments: &orchestrator.ShapeReviewComments{DebounceMS: 60000, ShapeEscalation: ciLabelEscalation}}
	botReview := orchestrator.ShapeReactions{BotReview: &orchestrator.ShapeBotReview{ShapeEscalation: ciLabelEscalation}}
	mergeConflicts := orchestrator.ShapeReactions{MergeConflicts: &orchestrator.ShapeMergeConflicts{ShapeEscalation: ciLabelEscalation}}
	fixLabel := orchestrator.ShapeReactions{LabelCommands: &orchestrator.ShapeLabelCommands{FixLabel: "sortie:fix"}}

	const (
		succeeds      = "If the run succeeds"
		fails         = "If the run fails"
		retries       = "Sortie retries with exponential backoff."
		handoff       = "Sortie moves the issue to Human Review"
		watchHandoff  = handoff + ", and a pull request the run leaves gets the CI failure and auto-merge reactions."
		ciCondition   = "If a check in the next 25m finds CI failing on the pull request"
		fixCondition  = "If `sortie:fix` is added to the pull request"
		sessionsHeld  = "session budget (5 of 5 sessions used)"
		reviewCommand = "Sortie dispatches review fix 1 of 3"
	)

	tests := []struct {
		name      string
		situation Situation
		shape     *orchestrator.WorkflowShape
		want      []NextOutcome
	}{
		{
			name:      "a running stage lists the next stage and a retry on failure",
			situation: Situation{Running: workSession("specify", 0), Spend: &Spend{Sessions: 1}},
			shape:     fixtureShape(noReactions),
			want: []NextOutcome{
				{succeeds, "Sortie adds `stage-implement`, and a poll tick then starts stage implement in a new session."},
				{fails, retries},
			},
		},
		{
			name:      "the last stage hands off and names the reactions configured",
			situation: Situation{Running: workSession("implement", 0), Spend: &Spend{Sessions: 1}},
			shape:     fixtureShape(watchReactions),
			want:      []NextOutcome{{succeeds, watchHandoff}, {fails, retries}},
		},
		{
			name:      "the last stage hands off without a watch when no reaction is configured",
			situation: Situation{Running: workSession("implement", 0), Spend: &Spend{Sessions: 1}},
			shape:     fixtureShape(noReactions),
			want:      []NextOutcome{{succeeds, handoff + "."}, {fails, retries}},
		},
		{
			name:      "a stage at the hop ceiling hands off instead of starting the next stage",
			situation: Situation{Running: workSession("specify", 10), Spend: &Spend{Sessions: 1}},
			shape:     fixtureShape(watchReactions),
			want: []NextOutcome{
				{succeeds, "The hop count is at its ceiling (10 of 10), so stage implement does not start: " + watchHandoff},
				{fails, retries},
			},
		},
		{
			name:      "a CI failure with fixes used up escalates",
			situation: Situation{PendingReactions: pendingRow("ci", watchedFor(1500*time.Second, 2, 2))},
			shape:     fixtureShape(ci),
			want: []NextOutcome{{
				ciCondition,
				"CI fixes are used up (2 of 2), so Sortie applies `needs-human` to the issue and keeps watching the pull request. A new commit that no Sortie run made can restart the count.",
			}},
		},
		{
			name:      "review fixes used up escalate and keep watching the pull request",
			situation: Situation{PendingReactions: pendingRow("review", watchedFor(1590*time.Second, 3, 3))},
			shape:     fixtureShape(review),
			want: []NextOutcome{{
				"If a check in the next 25m finds new review comments on the pull request",
				"Review fixes are used up (3 of 3), so Sortie applies `needs-human` to the issue and keeps watching the pull request.",
			}},
		},
		{
			name:      "bot review fixes used up escalate and keep watching the pull request",
			situation: Situation{PendingReactions: pendingRow("bot-review", watchedFor(1500*time.Second, 3, 3))},
			shape:     fixtureShape(botReview),
			want: []NextOutcome{{
				"If a check in the next 25m finds a new bot review comment on the code of the pull request",
				"Bot review fixes are used up (3 of 3), so Sortie applies `needs-human` to the issue and keeps watching the pull request.",
			}},
		},
		{
			name:      "rebases used up escalate and keep watching the pull request",
			situation: Situation{PendingReactions: pendingRow("merge-conflict", watchedFor(1500*time.Second, 2, 2))},
			shape:     fixtureShape(mergeConflicts),
			want: []NextOutcome{{
				"If a check in the next 25m finds a new merge conflict on the pull request",
				"Rebases are used up (2 of 2), so Sortie applies `needs-human` to the issue and keeps watching the pull request. A new commit that no Sortie run made can restart the count.",
			}},
		},
		{
			name:      "a CI failure with fixes left dispatches the next fix",
			situation: Situation{PendingReactions: pendingRow("ci", watchedFor(1500*time.Second, 1, 3))},
			shape:     fixtureShape(ci),
			want:      []NextOutcome{{ciCondition, "Sortie dispatches CI fix 2 of 3."}},
		},
		{
			name:      "a pending reaction the config lacks lists nothing",
			situation: Situation{PendingReactions: pendingRow("ci", watchedFor(1500*time.Second, 0, 3))},
			shape:     fixtureShape(noReactions),
			want:      []NextOutcome{},
		},
		{
			name:      "an exhausted session budget holds both outcomes of a running stage",
			situation: Situation{Running: workSession("specify", 0), Spend: &Spend{Sessions: 4}},
			shape:     withSessionBudget(fixtureShape(noReactions), 5),
			want: []NextOutcome{
				{succeeds, "Sortie adds `stage-implement`, then holds the issue at its " + sessionsHeld + " before stage implement starts."},
				{fails, "Sortie holds the issue at its " + sessionsHeld + "."},
			},
		},
		{
			name:      "a CI failure with a triage command runs it even when fixes are used up",
			situation: Situation{PendingReactions: pendingRow("ci", watchedFor(1500*time.Second, 2, 2))},
			shape:     fixtureShape(ciTriage),
			want:      []NextOutcome{{ciCondition, "Sortie runs the triage command, and its answer decides what follows."}},
		},
		{
			name:      "review comments outside the debounce wait count the settle time against the window",
			situation: Situation{PendingReactions: pendingRow("review", watchedFor(1590*time.Second, 0, 3))},
			shape:     fixtureShape(review),
			want: []NextOutcome{{
				"If a check in the next 25m finds new review comments on the pull request",
				reviewCommand + " once no newer comment arrives for 1m.",
			}},
		},
		{
			name: "review comments inside the debounce wait count only the poll interval",
			situation: Situation{PendingReactions: pendingRow("review", func(r *orchestrator.SnapshotPendingReaction) {
				watchedFor(1590*time.Second, 0, 3)(r)
				r.Wait = "debounce"
			})},
			shape: fixtureShape(review),
			want: []NextOutcome{{
				"When the new review comments on the pull request settle in the next 26m",
				reviewCommand + ".",
			}},
		},
		{
			name: "a fix label beside a running CI fix waits for the current run",
			situation: Situation{
				Running:          &orchestrator.SnapshotRunningEntry{Identifier: "MT-1", ReactionKind: "ci"},
				PendingReactions: pendingRow("label-fix", nil),
			},
			shape: fixtureShape(fixLabel),
			want:  []NextOutcome{{fixCondition, "Sortie dispatches a fix of the pull request after the current run ends."}},
		},
		{
			name: "a fix label beside a queued CI fix retry waits for the CI fix run",
			situation: Situation{
				Retry:            &orchestrator.SnapshotRetryEntry{Identifier: "MT-1", Trigger: "ci_fix", ReactionKind: "ci"},
				PendingReactions: pendingRow("label-fix", nil),
			},
			shape: fixtureShape(fixLabel),
			want: []NextOutcome{
				{"When the CI fix run comes due", "Sortie dispatches a new session."},
				{fixCondition, "Sortie dispatches a fix of the pull request after the CI fix run ends."},
			},
		},
		{
			name: "a fix label beside a running fix session lists nothing",
			situation: Situation{
				Running:          &orchestrator.SnapshotRunningEntry{Identifier: "MT-1", ReactionKind: "label-fix"},
				PendingReactions: pendingRow("label-fix", nil),
			},
			shape: fixtureShape(fixLabel),
			want:  []NextOutcome{},
		},
		{
			name: "a fix label beside a queued fix retry lists only the retry",
			situation: Situation{
				Retry:            &orchestrator.SnapshotRetryEntry{Identifier: "MT-1", ReactionKind: "label-fix"},
				PendingReactions: pendingRow("label-fix", nil),
			},
			shape: fixtureShape(fixLabel),
			want:  []NextOutcome{{"When the label fix run comes due", "Sortie dispatches a new session."}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := NextOutcomes(tt.situation, Context{Now: outcomeNow, Shape: tt.shape})

			if got == nil {
				t.Fatalf("NextOutcomes(%s) = nil, want a non-nil list", tt.name)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("NextOutcomes(%s) = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}
