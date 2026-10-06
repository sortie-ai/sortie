package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
)

var recordGolden = flag.Bool("record-golden", false, "rewrite the golden notification fixtures instead of comparing against them")

const (
	goldenIssueID = "GOLD-1"
	goldenDir     = "testdata/golden_notify"
)

var goldenNow = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

type goldenOp struct {
	Op      string `json:"op"`
	IssueID string `json:"issue_id"`
	Text    string `json:"text,omitempty"`
}

type goldenStepRecord struct {
	Step string     `json:"step"`
	Ops  []goldenOp `json:"ops"`
}

type goldenRecorder struct {
	*mockTrackerAdapter
	*reviewReconcileStore

	mu      sync.Mutex
	ops     []goldenOp
	failing bool
}

var (
	_ domain.TrackerAdapter = (*goldenRecorder)(nil)
	_ ReconcileStore        = (*goldenRecorder)(nil)
	_ budgetHoldNoticeStore = (*goldenRecorder)(nil)
)

func newGoldenRecorder() *goldenRecorder {
	return &goldenRecorder{mockTrackerAdapter: &mockTrackerAdapter{}, reviewReconcileStore: &reviewReconcileStore{}}
}

func (r *goldenRecorder) record(op, issueID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, goldenOp{Op: op, IssueID: issueID, Text: text})
	if r.failing && (op == "comment" || op == "label") {
		return errors.New("golden tracker write failure")
	}
	return nil
}

func (r *goldenRecorder) drain() []goldenOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	ops := append([]goldenOp{}, r.ops...)
	r.ops = nil
	return ops
}

func (r *goldenRecorder) setFailing(failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing = failing
}

func (r *goldenRecorder) CommentIssue(_ context.Context, issueID, text string) error {
	return r.record("comment", issueID, text)
}

func (r *goldenRecorder) AddLabel(_ context.Context, issueID, label string) error {
	return r.record("label", issueID, label)
}

func (r *goldenRecorder) MarkReactionObservationDispatched(_ context.Context, issueID, kind, _ string) error {
	return r.record("observation_dispatched", issueID, kind)
}

func (r *goldenRecorder) UpsertBudgetHoldNotice(_ context.Context, notice persistence.BudgetHoldNotice) error {
	return r.record("budget_notice_row", notice.IssueID, notice.Reason)
}

type goldenWired struct {
	cfg       config.ServiceConfig
	rec       *goldenRecorder
	router    *route.Router
	exit      HandleWorkerExitParams
	reconcile ReconcileParams
	budget    budgetHoldNoticeParams
}

func goldenCommentEscalations(cfg config.ServiceConfig, reconcile ReconcileParams) []domain.EventType {
	var events []domain.EventType
	for _, producer := range []struct {
		event domain.EventType
		mode  string
	}{
		{domain.EventEscalationCIFailure, cfg.CIFeedback.Escalation},
		{domain.EventEscalationReviewComments, reconcile.ReviewConfig.Escalation},
		{domain.EventEscalationBotReview, reconcile.BotReviewConfig.Escalation},
		{domain.EventEscalationMergeConflicts, reconcile.MergeConflictConfig.Escalation},
		{domain.EventEscalationAutoMerge, reconcile.AutoMergeConfig.Escalation},
		{domain.EventEscalationMergeCompletion, reconcile.MergeCompletionConfig.Escalation},
	} {
		if producer.mode == "comment" {
			events = append(events, producer.event)
		}
	}
	return events
}

// goldenWiring is the only place a producer receives its notification
// wiring, so a change of that wiring edits this function and nothing else.
func goldenWiring(t *testing.T, cfg config.ServiceConfig, rec *goldenRecorder) goldenWired {
	t.Helper()

	cfg.Workspace.Root = t.TempDir()
	reconcile := ReconcileParams{
		TrackerAdapter: rec,
		Store:          rec,
		SCMAdapter:     &mockSCMAdapter{},
		CIFeedback:     cfg.CIFeedback,
		Logger:         discardLogger(),
	}
	var err error
	if rc, ok := cfg.Reactions["review_comments"]; ok {
		reconcile.ReviewConfig, err = BuildReviewReactionConfig(rc)
	}
	if rc, ok := cfg.Reactions["bot_review"]; ok && err == nil {
		reconcile.BotReviewConfig, err = BuildBotReviewReactionConfig(rc)
	}
	if rc, ok := cfg.Reactions["merge_conflicts"]; ok && err == nil {
		reconcile.MergeConflictConfig, err = BuildMergeConflictReactionConfig(rc)
	}
	if rc, ok := cfg.Reactions["auto_merge"]; ok && err == nil {
		reconcile.AutoMergeConfig, err = BuildAutoMergeReactionConfig(rc)
	}
	if rc, ok := cfg.Reactions["merge_completion"]; ok && err == nil {
		reconcile.MergeCompletionConfig, err = BuildMergeCompletionReactionConfig(rc, cfg.Tracker, registry.TrackerMeta{})
	}
	if err != nil {
		t.Fatalf("building reaction configuration: %v", err)
	}

	router := route.NewRouter(rec, nil)
	err = router.Update(route.Inputs{
		Comments:           cfg.Tracker.Comments,
		CommentEscalations: goldenCommentEscalations(cfg, reconcile),
	})
	if err != nil {
		t.Fatalf("Router.Update: %v", err)
	}
	reconcile.Router = router

	return goldenWired{
		cfg:    cfg,
		rec:    rec,
		router: router,
		exit: HandleWorkerExitParams{
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       func(string) {},
			NowFunc:           func() time.Time { return goldenNow },
			Logger:            discardLogger(),
			TrackerAdapter:    rec,
			HandoffState:      cfg.Tracker.HandoffState,
			NoChangeState:     cfg.Tracker.NoChangeState,
			ActiveStates:      cfg.Tracker.ActiveStates,
			TerminalStates:    cfg.Tracker.TerminalStates,
			Router:            router,
		},
		reconcile: reconcile,
		budget: budgetHoldNoticeParams{
			Store:   rec,
			Metrics: &domain.NoopMetrics{},
			Logger:  discardLogger(),
		},
	}
}

type goldenStep struct {
	name string
	run  func(t *testing.T, w goldenWired)
}

type goldenScenario struct {
	name       string
	comments   map[string]bool
	env        map[string]string
	reaction   string
	escalation string
	steps      []goldenStep
}

func (sc goldenScenario) rawConfig() map[string]any {
	tracker := map[string]any{
		"kind":              "file",
		"project":           "GOLD",
		"active_states":     []any{"To Do", "In Progress"},
		"terminal_states":   []any{"Done"},
		"handoff_state":     "Human Review",
		"in_progress_state": "In Progress",
	}
	if sc.comments != nil {
		comments := map[string]any{}
		for key, on := range sc.comments {
			comments[key] = on
		}
		tracker["comments"] = comments
	}

	raw := map[string]any{
		"tracker": tracker,
		"agent":   map[string]any{"kind": "mock", "max_turns": 1},
	}
	if sc.reaction != "" {
		reaction := map[string]any{"provider": "github"}
		if sc.escalation != "" {
			reaction["escalation"] = sc.escalation
		}
		if sc.reaction == "merge_completion" {
			reaction["target_state"] = "Done"
		}
		raw["reactions"] = map[string]any{sc.reaction: reaction}
	}
	return raw
}

func dispatchStep(name string, posture DispatchPosture) goldenStep {
	return goldenStep{name: name, run: func(t *testing.T, w goldenWired) {
		t.Helper()

		ec := newExitCapture()
		cfg := w.cfg
		var trackerOps sync.WaitGroup
		RunWorkerAttempt(context.Background(), domain.Issue{ID: goldenIssueID, Identifier: "GOLD-1", Title: "Golden", State: "To Do"}, nil, WorkerDeps{
			TrackerAdapter:         w.rec,
			Router:                 w.router,
			TrackerOpsWg:           &trackerOps,
			AgentAdapter:           &mockAgentAdapter{},
			Posture:                posture,
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(string, domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                &domain.NoopMetrics{},
		})
		ec.waitResult(t)
		trackerOps.Wait()
	}}
}

func exitStep(name string, result WorkerResult, adjust func(t *testing.T, p *HandleWorkerExitParams, r *WorkerResult)) goldenStep {
	return goldenStep{name: name, run: func(t *testing.T, w goldenWired) {
		t.Helper()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		state.Running[goldenIssueID] = &RunningEntry{
			Identifier: goldenIssueID,
			StartedAt:  goldenNow.Add(-90 * time.Second),
			Issue:      domain.Issue{ID: goldenIssueID, State: "In Progress"},
		}
		state.Claimed[goldenIssueID] = struct{}{}

		params := w.exit
		params.Store = &mockExitStore{}
		result.IssueID = goldenIssueID
		result.Identifier = goldenIssueID
		result.AgentAdapter = "mock"
		if adjust != nil {
			adjust(t, &params, &result)
		}

		HandleWorkerExit(state, result, params)
		state.TrackerOpsWg.Wait()
		CancelRetry(state, goldenIssueID)
	}}
}

func withheldHandoff(t *testing.T, _ *HandleWorkerExitParams, r *WorkerResult) {
	t.Helper()

	dir, baseline := handoffEvidenceGitWorkspace(t)
	r.WorkspacePath = dir
	r.HandoffEvidencePolicy = config.HandoffEvidenceObserved
	r.HandoffEvidenceBaseline = baseline
}

func commentScript() []goldenStep {
	return []goldenStep{
		dispatchStep("dispatch", PostureNormal),
		dispatchStep("dispatch_review_posture", PostureReview),
		exitStep("exit_completed", WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 3}, nil),
		exitStep("exit_completed_requeued", WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 3},
			func(_ *testing.T, p *HandleWorkerExitParams, _ *WorkerResult) { p.HandoffState = "" }),
		exitStep("exit_soft_stop_no_change", WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 2, SoftStop: true, SoftStopReason: "no-change-needed"}, nil),
		exitStep("exit_soft_stop_blocked", WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 2, SoftStop: true, SoftStopReason: "blocked"}, nil),
		exitStep("exit_handoff_withheld", WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 1}, withheldHandoff),
		exitStep("exit_failed", WorkerResult{ExitKind: WorkerExitError, Error: &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "turn timed out"}}, nil),
		exitStep("exit_cancelled", WorkerResult{ExitKind: WorkerExitCancelled}, nil),
	}
}

func escalationStep(name string, run func(t *testing.T, w goldenWired, state *State, pending *PendingReaction)) goldenStep {
	return goldenStep{name: name, run: func(t *testing.T, w goldenWired) {
		t.Helper()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		pending := &PendingReaction{IssueID: goldenIssueID, Identifier: goldenIssueID, CreatedAt: goldenNow}
		state.Claimed[goldenIssueID] = struct{}{}

		run(t, w, state, pending)
		state.TrackerOpsWg.Wait()
		CancelRetry(state, goldenIssueID)
	}}
}

func escalationScript(reaction string) []goldenStep {
	ctx := context.Background()
	log := discardLogger()
	metrics := &domain.NoopMetrics{}
	triggers := []ReactionEscalationTrigger{EscalationTriggerBudget, EscalationTriggerTriage}

	var steps []goldenStep
	switch reaction {
	case "ci_failure":
		result := domain.CIResult{
			Status:       domain.CIStatusFailing,
			FailingCount: 2,
			CheckRuns: []domain.CheckRun{
				{Name: "test", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure, DetailsURL: "https://ci.example.test/runs/1"},
				{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
				{Name: "build", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionSuccess},
			},
		}
		for _, trigger := range triggers {
			steps = append(steps, escalationStep("ci_"+string(trigger), func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindCI
				escalateCIFailure(state, w.reconcile, pending, result, "abc1234", 3, trigger, goldenNow, time.Second, log, ctx, metrics)
			}))
		}
	case "review_comments":
		data := &ReviewReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets", Branch: "feature/gold"}
		for _, trigger := range triggers {
			steps = append(steps, escalationStep("review_"+string(trigger), func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindReview
				escalateReviewFailure(state, w.reconcile, pending, 3, trigger, data, log, ctx, metrics)
			}))
		}
	case "bot_review":
		data := &BotReviewReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets", Branch: "feature/gold"}
		for _, trigger := range triggers {
			steps = append(steps, escalationStep("bot_review_"+string(trigger), func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindBotReview
				escalateBotReviewFailure(state, w.reconcile, pending, 3, trigger, data, log, ctx, metrics)
			}))
		}
	case "merge_conflicts":
		data := &MergeConflictReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets", Branch: "feature/gold"}
		for _, trigger := range triggers {
			steps = append(steps, escalationStep("merge_conflict_"+string(trigger), func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindMergeConflict
				escalateMergeConflictFailure(state, w.reconcile, pending, 2, trigger, data, log, ctx, metrics)
			}))
		}
	case "auto_merge":
		data := &AutoMergeReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets", Branch: "feature/gold"}
		steps = append(steps,
			escalationStep("auto_merge_escalated", func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindAutoMerge
				escalateAutoMergeFailure(state, w.reconcile, pending, 3, data, log, ctx, metrics)
			}),
			escalationStep("auto_merge_succeeded", func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindAutoMerge
				postAutoMergeSuccess(state, w.reconcile, pending, data, domain.MergeResult{SHA: "deadbeefcafe", Merged: true}, log, ctx)
			}),
		)
	case "merge_completion":
		data := &MergeCompletionReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets"}
		steps = append(steps,
			escalationStep("merge_completion_escalated", func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindMergeCompletion
				escalateMergeCompletion(state, w.reconcile, pending, data, 3, log, ctx)
			}),
			escalationStep("missing_sha_delivered", func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindMergeCompletion
				escalateMergeCompletionMissingSHA(state, w.reconcile, pending, data, 10*time.Minute, log, ctx)
			}),
			escalationStep("missing_sha_tracker_write_fails", func(_ *testing.T, w goldenWired, state *State, pending *PendingReaction) {
				pending.Kind = ReactionKindMergeCompletion
				w.rec.setFailing(true)
				defer w.rec.setFailing(false)
				escalateMergeCompletionMissingSHA(state, w.reconcile, pending, data, 10*time.Minute, log, ctx)
				state.TrackerOpsWg.Wait()
			}),
		)
	}
	return steps
}

func budgetHoldScript() []goldenStep {
	used := int64(1_200_000)
	unmeasured := 1
	stopped := 2
	entries := map[string]*BudgetExhaustedEntry{
		"budget_hold_session": {
			Reason: budgetReasonSession, UsedSessions: 4, BudgetSessions: 3, ExhaustedAt: goldenNow,
		},
		"budget_hold_token": {
			Reason: budgetReasonToken, UsedSessions: 2, BudgetSessions: 5, UsedTokens: &used, BudgetTokens: 1_000_000,
			UnmeasuredSessions: &unmeasured, StoppedInFlight: &stopped, ExhaustedAt: goldenNow,
		},
	}

	var steps []goldenStep
	for _, name := range []string{"budget_hold_session", "budget_hold_token"} {
		entry := entries[name]
		steps = append(steps, escalationStep(name, func(_ *testing.T, w goldenWired, state *State, _ *PendingReaction) {
			params := w.budget
			params.IssueID = goldenIssueID
			params.Entry = entry
			params.Delivery = w.router.Route(budgetHeldNotification(goldenIssueID, entry))
			params.Ctx = context.Background()
			postBudgetHoldNotice(state, params)
		}))
	}
	return steps
}

func goldenScenarios() []goldenScenario {
	var scenarios []goldenScenario

	for mask := range 8 {
		flags := map[string]bool{
			"on_dispatch":   mask&1 != 0,
			"on_completion": mask&2 != 0,
			"on_failure":    mask&4 != 0,
		}
		name := fmt.Sprintf("comments_dispatch%d_completion%d_failure%d", mask&1, mask>>1&1, mask>>2&1)
		scenarios = append(scenarios,
			goldenScenario{name: name + "_yaml", comments: flags, steps: commentScript()},
			goldenScenario{name: name + "_env", steps: commentScript(), env: map[string]string{
				"SORTIE_TRACKER_COMMENTS_ON_DISPATCH":   strconv.FormatBool(flags["on_dispatch"]),
				"SORTIE_TRACKER_COMMENTS_ON_COMPLETION": strconv.FormatBool(flags["on_completion"]),
				"SORTIE_TRACKER_COMMENTS_ON_FAILURE":    strconv.FormatBool(flags["on_failure"]),
			}},
		)
	}

	for _, reaction := range []string{"ci_failure", "review_comments", "bot_review", "merge_conflicts", "auto_merge", "merge_completion"} {
		modes := []string{"label", "comment"}
		if reaction == "auto_merge" {
			modes = append(modes, "")
		}
		for _, mode := range modes {
			label := mode
			if label == "" {
				label = "omitted"
			}
			scenarios = append(scenarios, goldenScenario{
				name:       fmt.Sprintf("escalation_%s_%s", reaction, label),
				reaction:   reaction,
				escalation: mode,
				steps:      escalationScript(reaction),
			})
		}
	}

	return append(scenarios, goldenScenario{name: "budget_hold", steps: budgetHoldScript()})
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join(goldenDir, name+".json")
	if *recordGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", goldenDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden fixture %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden %s differs\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestGoldenNotify(t *testing.T) {
	for _, name := range []string{
		"SORTIE_TRACKER_COMMENTS_ON_DISPATCH",
		"SORTIE_TRACKER_COMMENTS_ON_COMPLETION",
		"SORTIE_TRACKER_COMMENTS_ON_FAILURE",
		"SORTIE_ENV_FILE",
	} {
		t.Setenv(name, "")
	}

	for _, sc := range goldenScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			if len(sc.env) == 0 {
				t.Parallel()
			}
			for name, value := range sc.env {
				t.Setenv(name, value)
			}

			cfg, err := config.NewServiceConfig(sc.rawConfig())
			if err != nil {
				t.Fatalf("NewServiceConfig(%s): %v", sc.name, err)
			}
			rec := newGoldenRecorder()
			wired := goldenWiring(t, cfg, rec)

			records := make([]goldenStepRecord, 0, len(sc.steps))
			for _, step := range sc.steps {
				step.run(t, wired)
				records = append(records, goldenStepRecord{Step: step.name, Ops: rec.drain()})
			}

			got, err := json.MarshalIndent(records, "", "  ")
			if err != nil {
				t.Fatalf("MarshalIndent(%s): %v", sc.name, err)
			}
			checkGolden(t, sc.name, append(got, '\n'))
		})
	}
}

func (r *goldenRecorder) AddReactionHandedOffComments(_ context.Context, _, _ string, _ []string) error {
	return nil
}
