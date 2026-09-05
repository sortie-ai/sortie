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

	"github.com/sortie-ai/sortie/internal/qualification"
)

// TestHarnessContract confirms the isolated end-to-end harness's
// contract: tracker.kind file with a temporary issue file, a
// controlled git workspace under the same temporary root, no hooks,
// notifications, server, or network tracker, max_turns 1, an active
// and a non-active handoff state, and only the permitted effective
// sample fields.
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

// TestTerminalOracle drives the isolated file-tracker harness with the
// fake protocol agent and temporary file tracker only: it reaches the
// terminal condition, cancels and drains within the shared shutdown
// bound, and then applies the exact PGID postcondition with the
// signal-zero oracle.
func TestTerminalOracle(t *testing.T) {
	harness := NewHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		harness.orchestrator.Run(ctx)
		close(runDone)
	}()

	// A bounded wait for the terminal condition: one succeeded history
	// row, the file-tracker issue in its handoff state, no running or
	// retry snapshot entry, and a completed StopSession.
	deadline := time.Now().Add(qualification.ShutdownDeadline)
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

	// Reaching the condition triggers cleanup: cancel the orchestrator
	// and allow at most the shared 30-second shutdown bound for the
	// drain.
	cancel()
	select {
	case <-runDone:
	case <-time.After(qualification.ShutdownDeadline):
		t.Fatal("the orchestrator did not drain within the shared shutdown bound")
	}

	// The exact PGID postcondition: every captured group is gone.
	if len(harness.Agent().PGIDs()) != 1 {
		t.Fatalf("captured group count = %d, want 1 for the single fixture session", len(harness.Agent().PGIDs()))
	}
	for _, pgid := range harness.Agent().PGIDs() {
		qualification.AwaitProcessGroupAbsence(t, pgid)
	}

	// The terminal record from the observed condition is pass and
	// decodes cleanly.
	rec := TerminalRecord(condition, true, qualification.FixtureSession(qualification.SurfaceProtocol, "e2e"), qualification.FixtureAgentName, qualification.FixtureAgentVer)
	if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeUsable {
		t.Errorf("end-to-end record = %s/%s, want pass/usable at the terminal condition", rec.Outcome, rec.Grade)
	}
	line, err := qualification.MarshalRecord(rec)
	if err != nil {
		t.Fatalf("qualification.MarshalRecord() error = %v", err)
	}
	if _, err := qualification.DecodeRecord(line); err != nil {
		t.Errorf("qualification.DecodeRecord() error = %v, want the end-to-end record to decode cleanly", err)
	}

	// The run-history assertion: exactly one row, succeeded, for this
	// issue's single attempt.
	rows, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "succeeded" || rows[0].Attempt != 1 {
		t.Errorf("run history rows = %+v, want exactly one succeeded first attempt", rows)
	}
}
