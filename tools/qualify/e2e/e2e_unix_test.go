//go:build unix

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func TestHarnessContract(t *testing.T) {
	t.Parallel()

	harness := NewHarness(t)

	t.Run("file tracker over a temporary issue file", func(t *testing.T) {
		t.Parallel()

		if harness.sample == (effectiveSample{}) {
			t.Fatal("harness sample is empty")
		}
		raw, err := os.ReadFile(harness.issueFile)
		if err != nil {
			t.Fatalf("read issue file: %v", err)
		}
		if !strings.Contains(string(raw), issueIdentifier) {
			t.Errorf("issue file %s does not name the fixture issue", filepath.Base(harness.issueFile))
		}
		if !strings.Contains(string(raw), activeState) {
			t.Errorf("issue file does not start the issue in the active state %q", activeState)
		}
	})

	t.Run("controlled git workspace under the same temporary root", func(t *testing.T) {
		t.Parallel()

		if filepath.Dir(harness.workspaceRoot) != harness.tempRoot {
			t.Errorf("workspace root %q is not under the harness's temporary root", harness.workspaceRoot)
		}
	})

	t.Run("no hooks, notifications, server, or network tracker", func(t *testing.T) {
		t.Parallel()

		hooks := harness.manager.Config().Hooks
		if hooks.AfterCreate != "" || hooks.BeforeRun != "" || hooks.AfterRun != "" || hooks.BeforeRemove != "" {
			t.Errorf("harness carries a hooks block: %+v", hooks)
		}
		if notifications := harness.manager.Config().Notifications; len(notifications.Backends) != 0 {
			t.Errorf("harness carries a notification backend: %+v", notifications)
		}
		if harness.manager.Config().Tracker.Kind != "file" {
			t.Errorf("tracker kind = %q, want file", harness.manager.Config().Tracker.Kind)
		}
	})

	t.Run("active and non-active handoff states", func(t *testing.T) {
		t.Parallel()

		cfg := harness.manager.Config().Tracker
		if !slices.Contains(cfg.ActiveStates, activeState) {
			t.Errorf("active states = %v, want %q among them", cfg.ActiveStates, activeState)
		}
		if slices.Contains(cfg.ActiveStates, handoffState) {
			t.Errorf("handoff state %q must be non-active", handoffState)
		}
		if cfg.HandoffState != handoffState {
			t.Errorf("handoff state = %q, want %q", cfg.HandoffState, handoffState)
		}
	})

	t.Run("only the permitted effective sample fields are set", func(t *testing.T) {
		t.Parallel()

		cfg := harness.manager.Config()
		if cfg.Agent.Kind != harness.sample.AgentKind || cfg.Agent.Command != harness.sample.AgentCommand {
			t.Errorf("agent fields = %q/%q, want the sample's kind and command", cfg.Agent.Kind, cfg.Agent.Command)
		}
		if cfg.Agent.TurnTimeoutMS != harness.sample.TurnTimeoutMS ||
			cfg.Agent.ReadTimeoutMS != harness.sample.ReadTimeoutMS ||
			cfg.Agent.StallTimeoutMS != harness.sample.StallTimeoutMS {
			t.Errorf("agent bounds do not match the extracted sample fields")
		}
		if cfg.Agent.MaxTurns != 1 {
			t.Errorf("max_turns = %d, want 1", cfg.Agent.MaxTurns)
		}
		if cfg.Agent.MaxSessions != harness.sample.MaxSessions || cfg.Agent.MaxTokens != harness.sample.MaxTokens {
			t.Errorf("session and token bounds do not match the extracted sample fields")
		}
		if cfg.Agent.MaxRetryBackoffMS != 0 || cfg.Agent.MaxConsecutiveAbsences != 0 {
			t.Errorf("agent carries an unpermitted bound: retry backoff %d, absences %d",
				cfg.Agent.MaxRetryBackoffMS, cfg.Agent.MaxConsecutiveAbsences)
		}
		if len(cfg.Reactions) != 0 {
			t.Errorf("reactions = %v, want none", cfg.Reactions)
		}
		if cfg.CIFeedback.Kind != "" {
			t.Errorf("CI feedback configured: %q", cfg.CIFeedback.Kind)
		}
		if len(cfg.Notifications.Backends) != 0 {
			t.Errorf("notifications configured: %+v", cfg.Notifications)
		}
	})
}

func requireReleased(t *testing.T, g *procutil.Group) {
	t.Helper()

	deadline := time.Now().Add(ShutdownDeadline)
	for {
		released, cleanupErr := g.Released()
		if released {
			if cleanupErr != nil {
				t.Fatalf("Released() cleanup error = %v, want nil", cleanupErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Released() = false after %s, want the launch's group released", ShutdownDeadline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminalOracle(t *testing.T) {
	harness := NewHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		harness.orchestrator.Run(ctx)
		close(runDone)
	}()

	deadline := time.Now().Add(ShutdownDeadline)
	var condition TerminalCondition
	for {
		condition = ObserveTerminalCondition(t, harness)
		if condition.Reached() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-runDone
			t.Fatalf("the isolated end-to-end harness never reached its terminal condition; last observation = %+v", condition)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(ShutdownDeadline):
		t.Fatal("the orchestrator did not drain within the shared shutdown bound")
	}

	groups := harness.Agent().Groups()
	if len(groups) != 2 {
		t.Fatalf("captured group count = %d, want 2 (the credential verification launch and the working session)", len(groups))
	}
	for _, g := range groups {
		requireReleased(t, g)
	}

	rec := TerminalRecord(condition, true, evidencetest.FixtureSession(evidence.SurfaceProtocol, "e2e"), evidencetest.FixtureAgentName, evidencetest.FixtureAgentVer)
	if rec.Outcome != evidence.OutcomePass || rec.Grade != evidence.GradeUsable {
		t.Errorf("end-to-end record = %s/%s, want pass/usable at the terminal condition", rec.Outcome, rec.Grade)
	}
	line, err := evidence.MarshalRecord(rec)
	if err != nil {
		t.Fatalf("evidence.MarshalRecord() error = %v", err)
	}
	if _, err := evidence.DecodeRecord(line); err != nil {
		t.Errorf("evidence.DecodeRecord() error = %v, want the end-to-end record to decode cleanly", err)
	}

	rows, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "succeeded" || rows[0].Attempt != 1 {
		t.Errorf("run history rows = %+v, want exactly one succeeded first attempt", rows)
	}
}

func terminalConditionReached() TerminalCondition {
	return TerminalCondition{
		SucceededRow:    true,
		HandoffReached:  true,
		NoRunningEntry:  true,
		NoRetryEntry:    true,
		StopSessionDone: true,
	}
}

func TestTerminalConditionReached(t *testing.T) {
	t.Parallel()

	t.Run("every part satisfied reports reached", func(t *testing.T) {
		t.Parallel()

		if !terminalConditionReached().Reached() {
			t.Error("Reached() = false for a fully satisfied condition, want true")
		}
	})

	withheld := []struct {
		name     string
		withdraw func(*TerminalCondition)
	}{
		{name: "no succeeded run-history row", withdraw: func(c *TerminalCondition) { c.SucceededRow = false }},
		{name: "the issue never reached its handoff state", withdraw: func(c *TerminalCondition) { c.HandoffReached = false }},
		{name: "a running snapshot entry remains", withdraw: func(c *TerminalCondition) { c.NoRunningEntry = false }},
		{name: "a retry snapshot entry remains", withdraw: func(c *TerminalCondition) { c.NoRetryEntry = false }},
		{name: "the session never completed StopSession", withdraw: func(c *TerminalCondition) { c.StopSessionDone = false }},
	}

	for _, tt := range withheld {
		t.Run(tt.name+" reports not reached", func(t *testing.T) {
			t.Parallel()

			condition := terminalConditionReached()
			tt.withdraw(&condition)
			if condition.Reached() {
				t.Errorf("Reached() = true with %s, want false", tt.name)
			}
		})
	}
}

func TestTerminalRecordGrades(t *testing.T) {
	t.Parallel()

	notReached := terminalConditionReached()
	notReached.StopSessionDone = false

	tests := []struct {
		name        string
		condition   TerminalCondition
		groupClean  bool
		wantGrade   evidence.Grade
		wantOutcome evidence.Outcome
		wantDetail  string
	}{
		{
			name:        "reached with a clean process group is usable",
			condition:   terminalConditionReached(),
			groupClean:  true,
			wantGrade:   evidence.GradeUsable,
			wantOutcome: evidence.OutcomePass,
			wantDetail:  "one succeeded history row and the issue reached its handoff state",
		},
		{
			name:        "not reached is a runtime failure",
			condition:   notReached,
			groupClean:  true,
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeRuntimeFailed,
			wantDetail:  "StopSession never completed",
		},
		{
			name:        "reached with a surviving group is a runtime failure",
			condition:   terminalConditionReached(),
			groupClean:  false,
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeRuntimeFailed,
			wantDetail:  "the issue reached its handoff state and a captured process group outlived the run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := TerminalRecord(tt.condition, tt.groupClean, "sess-1", "fixture-agent", "1.0.0-fixture")
			if rec.Grade != tt.wantGrade {
				t.Errorf("TerminalRecord() grade = %q, want %q", rec.Grade, tt.wantGrade)
			}
			if rec.Outcome != tt.wantOutcome {
				t.Errorf("TerminalRecord() outcome = %q, want %q", rec.Outcome, tt.wantOutcome)
			}
			if rec.Detail != tt.wantDetail {
				t.Errorf("TerminalRecord() detail = %q, want %q", rec.Detail, tt.wantDetail)
			}
			if rec.SessionID == nil || *rec.SessionID != "sess-1" {
				t.Errorf("TerminalRecord() session id = %v, want a pointer to \"sess-1\"", rec.SessionID)
			}
			if rec.Scenario != evidence.ScenarioEndToEnd {
				t.Errorf("TerminalRecord() scenario = %q, want %q", rec.Scenario, evidence.ScenarioEndToEnd)
			}
		})
	}
}

func TestHarnessWorkflowPathIsAbsolute(t *testing.T) {
	t.Parallel()

	harness := NewHarness(t)
	got := harness.manager.WorkflowAbsPath()
	if !filepath.IsAbs(got) {
		t.Fatalf("WorkflowAbsPath() = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "WORKFLOW.md" {
		t.Errorf("WorkflowAbsPath() base = %q, want \"WORKFLOW.md\"", filepath.Base(got))
	}
}

func TestStartWorkflowDrivesTheRunToItsTerminalCondition(t *testing.T) {
	harness := NewHarness(t)

	cancel, runDone := StartWorkflow(t, harness)

	deadline := time.Now().Add(ShutdownDeadline)
	var condition TerminalCondition
	for {
		condition = ObserveTerminalCondition(t, harness)
		if condition.Reached() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-runDone
			t.Fatalf("StartWorkflow never reached the terminal condition; last observation = %+v", condition)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(ShutdownDeadline):
		t.Fatal("the run goroutine did not return after cancellation")
	}

	sessions := harness.Agent().SessionIDs()
	if len(sessions) == 0 {
		t.Fatal("SessionIDs() is empty after a completed run, want the session the harness observed")
	}
	if sessions[0] == "" {
		t.Error("SessionIDs()[0] is empty, want the observed protocol session identifier")
	}
}

func TestPromptTemplateByIDRefusesAnUndeclaredID(t *testing.T) {
	t.Parallel()

	harness := NewHarness(t)

	if got := harness.manager.PromptTemplateByID(""); got == nil {
		t.Error("PromptTemplateByID(\"\") = nil, want the fixture's default template")
	}
	if got := harness.manager.PromptTemplateByID("a-template-the-fixture-never-declared"); got != nil {
		t.Error("PromptTemplateByID() returned a template for an id the fixture never declared, want nil")
	}
}

func TestBudgetsWithDefaults(t *testing.T) {
	t.Parallel()

	t.Run("the zero value is the deterministic fake agent's contract", func(t *testing.T) {
		t.Parallel()
		got := Budgets{}.withDefaults()
		want := Budgets{
			ReadTimeoutMS:  5000,
			TurnTimeoutMS:  10000,
			StallTimeoutMS: 10000,
			Observation:    ShutdownDeadline,
			MaxSessions:    1,
			MaxTurns:       1,
		}
		if got != want {
			t.Errorf("Budgets{}.withDefaults() = %+v, want %+v", got, want)
		}
	})

	t.Run("a live caller's bounds survive untouched", func(t *testing.T) {
		t.Parallel()
		live := Budgets{
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  300000,
			StallTimeoutMS: 60000,
			Observation:    10 * time.Minute,
			MaxSessions:    4,
			MaxTurns:       3,
		}
		if got := live.withDefaults(); got != live {
			t.Errorf("withDefaults() = %+v, want the caller's own bounds %+v", got, live)
		}
	})

	t.Run("only the unset bounds are filled", func(t *testing.T) {
		t.Parallel()
		got := Budgets{TurnTimeoutMS: 300000}.withDefaults()
		if got.TurnTimeoutMS != 300000 {
			t.Errorf("TurnTimeoutMS = %d, want the caller's 300000", got.TurnTimeoutMS)
		}
		if got.ReadTimeoutMS != 5000 || got.StallTimeoutMS != 10000 {
			t.Errorf("unset bounds = read %d stall %d, want the fake agent's 5000 and 10000", got.ReadTimeoutMS, got.StallTimeoutMS)
		}
		if got.Observation != ShutdownDeadline {
			t.Errorf("Observation = %v, want %v", got.Observation, ShutdownDeadline)
		}
	})

	t.Run("the deterministic harness reports the fake agent's observation bound", func(t *testing.T) {
		t.Parallel()
		if got := NewHarness(t).Observation(); got != ShutdownDeadline {
			t.Errorf("NewHarness().Observation() = %v, want %v", got, ShutdownDeadline)
		}
	})
}

func TestCeilingStopObservationGradesEachConditionRow(t *testing.T) {
	t.Parallel()

	const sessionID = "sess-ceiling-observed"

	tests := []struct {
		name        string
		condition   CeilingCondition
		wantGrade   evidence.Grade
		wantOutcome evidence.Outcome
		wantDetail  string
	}{
		{
			name:        "no run ended within the observation bound",
			condition:   CeilingCondition{RunEnded: false},
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeRuntimeFailed,
			wantDetail:  "no run under the one-token ceiling ended within the observation bound",
		},
		{
			name:        "the ceiling stopped the run and a dispatch followed",
			condition:   CeilingCondition{RunEnded: true, Status: "budget_stopped", Runs: 2},
			wantGrade:   evidence.GradeGap,
			wantOutcome: evidence.OutcomePass,
		},
		{
			name:        "the ceiling stopped the run and no poll tick held the issue",
			condition:   CeilingCondition{RunEnded: true, Status: "budget_stopped", HoldObserved: false, Runs: 1},
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeRuntimeFailed,
		},
		{
			name:        "the ceiling stopped the run and no dispatch followed",
			condition:   CeilingCondition{RunEnded: true, Status: "budget_stopped", HoldObserved: true, Runs: 1, TotalTokens: 5},
			wantGrade:   evidence.GradeUsable,
			wantOutcome: evidence.OutcomePass,
		},
		{
			name:        "the run spent tokens and recorded no figure",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: false, UnaccountedTurns: 1},
			wantGrade:   evidence.GradeGap,
			wantOutcome: evidence.OutcomePass,
		},
		{
			name:        "the run recorded no token figure and no turn reported unaccounted spend",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: false, UnaccountedTurns: 0},
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomePrerequisiteFailed,
		},
		{
			name:        "the measured total is below the ceiling",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: true, TotalTokens: 0},
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeFixtureInductionFailed,
		},
		{
			name:        "no figure reached the orchestrator while the run was live",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: true, TotalTokens: 1, FigureTurn: 0},
			wantGrade:   evidence.GradeGap,
			wantOutcome: evidence.OutcomePass,
		},
		{
			name:        "a turn began after the run crossed the ceiling",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: true, TotalTokens: 1, FigureTurn: 1, TurnsStarted: 2},
			wantGrade:   evidence.GradeGap,
			wantOutcome: evidence.OutcomePass,
		},
		{
			name:        "the run crossed the ceiling on its last turn and ended on its own",
			condition:   CeilingCondition{RunEnded: true, Status: "succeeded", TokensMeasured: true, TotalTokens: 1, FigureTurn: 2, TurnsStarted: 2},
			wantGrade:   evidence.GradeNotObserved,
			wantOutcome: evidence.OutcomeFixtureInductionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obs := CeilingStopObservation(tt.condition, sessionID)
			if obs.Grade != tt.wantGrade {
				t.Errorf("CeilingStopObservation(%+v) grade = %s, want %s", tt.condition, obs.Grade, tt.wantGrade)
			}
			if obs.Outcome != tt.wantOutcome {
				t.Errorf("CeilingStopObservation(%+v) outcome = %s, want %s", tt.condition, obs.Outcome, tt.wantOutcome)
			}
			if tt.wantDetail != "" && obs.Detail != tt.wantDetail {
				t.Errorf("CeilingStopObservation(%+v) detail = %q, want %q", tt.condition, obs.Detail, tt.wantDetail)
			}
			if obs.Detail == "" {
				t.Errorf("CeilingStopObservation(%+v) detail is empty, want a bounded explanation", tt.condition)
			}
			if obs.SessionID != sessionID {
				t.Errorf("CeilingStopObservation(%+v) session = %q, want %q", tt.condition, obs.SessionID, sessionID)
			}
			if err := evidence.CheckObservationAdmitted(obs); err != nil {
				t.Errorf("CheckObservationAdmitted(CeilingStopObservation(%+v)) error = %v, want nil", tt.condition, err)
			}
		})
	}
}

func TestDispatchWatchAdd(t *testing.T) {
	t.Parallel()

	t.Run("ends after the ceilingQuietPolls-th further sample following the hold, with no dispatch", func(t *testing.T) {
		t.Parallel()

		w := &dispatchWatch{}
		if w.add(ceilingSample{runs: 1, held: true}) {
			t.Fatal("add(hold sample) = true, want false: the observation must keep waiting through the quiet window")
		}
		for i := range ceilingQuietPolls - 1 {
			if w.add(ceilingSample{runs: 1}) {
				t.Fatalf("add(quiet sample %d) = true, want false before the %d-th further sample", i+1, ceilingQuietPolls)
			}
		}
		if !w.add(ceilingSample{runs: 1}) {
			t.Fatalf("add(the %d-th further sample) = false, want true", ceilingQuietPolls)
		}
		if !w.held || w.runs != 1 || w.running {
			t.Errorf("watch state = %+v, want held with one run and nothing running", w)
		}
	})

	t.Run("ends on the first sample showing more than one row, before the hold", func(t *testing.T) {
		t.Parallel()

		w := &dispatchWatch{}
		if !w.add(ceilingSample{runs: 2}) {
			t.Fatal("add(runs=2) = false, want true: more than one recorded run is a dispatch")
		}
	})

	t.Run("ends on the first sample showing a running session, inside the quiet window", func(t *testing.T) {
		t.Parallel()

		w := &dispatchWatch{}
		if w.add(ceilingSample{runs: 1, held: true}) {
			t.Fatal("add(hold sample) = true, want false")
		}
		if w.add(ceilingSample{runs: 1}) {
			t.Fatal("add(quiet sample) = true, want false")
		}
		if !w.add(ceilingSample{runs: 1, running: true}) {
			t.Fatal("add(running=true) = false, want true: a session in flight is a dispatch")
		}
	})

	t.Run("ends on the window's last sample when it shows a dispatch", func(t *testing.T) {
		t.Parallel()

		w := &dispatchWatch{}
		if w.add(ceilingSample{runs: 1, held: true}) {
			t.Fatal("add(hold sample) = true, want false")
		}
		for i := range ceilingQuietPolls - 1 {
			if w.add(ceilingSample{runs: 1}) {
				t.Fatalf("add(quiet sample %d) = true, want false", i+1)
			}
		}
		if !w.add(ceilingSample{runs: 2}) {
			t.Fatal("add(the window's last sample, runs=2) = false, want true: a dispatch on the final tick must still register")
		}
		// The dispatch evidence must end the observation before the quiet
		// window's own bookkeeping runs, so the final sample's dispatch does
		// not also get counted as a quiet tick.
		if w.quietPolls != ceilingQuietPolls-1 {
			t.Errorf("quietPolls after the dispatching final sample = %d, want %d: dispatch evidence must be checked before the quiet window's bookkeeping", w.quietPolls, ceilingQuietPolls-1)
		}
	})

	t.Run("never ends while no sample shows the hold or a dispatch", func(t *testing.T) {
		t.Parallel()

		w := &dispatchWatch{}
		for i := range ceilingQuietPolls * 3 {
			if w.add(ceilingSample{runs: 1}) {
				t.Fatalf("add(sample %d) = true, want false: nothing here shows the hold or a dispatch", i)
			}
		}
	})
}
