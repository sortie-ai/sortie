//go:build unix

package probe

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

const continuationNativeScenario = "continuation-native"

type continuationNativeParams struct {
	SeedOutput   string
	RecallOutput string
}

func runContinuationNative(args []string, params continuationNativeParams) int {
	joined := strings.Join(args, "\x00")
	var output string
	switch {
	case strings.Contains(joined, "SEED_MARKER"):
		output = params.SeedOutput
	case strings.Contains(joined, "RECALL_MARKER"):
		output = params.RecallOutput
	default:
		return 1
	}
	if _, err := fmt.Fprint(os.Stdout, output); err != nil {
		return 2
	}
	return 0
}

func init() {
	probeScenarios[continuationNativeScenario] = agenttest.Typed(runContinuationNative)
}

const tokenTestSurface = qualification.SurfaceNativeJSON

func tokenRecognizerProfile(recognizer qualification.Recognizer) qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		ProbePrompts: map[string]string{
			"continuation_seed":   "SEED_MARKER",
			"continuation_recall": "RECALL_MARKER",
		},
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			tokenTestSurface: {Args: []string{"--prompt", "{prompt}"}},
		},
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			tokenTestSurface: recognizer,
		},
	}
}

func TestProtocolTokenInventory(t *testing.T) {
	t.Parallel()

	t.Run("no transport was captured at all", func(t *testing.T) {
		t.Parallel()

		sessionID, paths, inventory, _ := protocolTokenInventory(&sharedFixture{usage: &usageTracker{}, wireTraceDir: t.TempDir()})
		if sessionID != "" || paths != nil {
			t.Errorf("protocolTokenInventory(...) = (%q, %v), want empty sentinel", sessionID, paths)
		}
		if inventory.Grade != qualification.GradeNotObserved || inventory.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("protocolTokenInventory(...) inventory = %+v, want not_observed/fixture_induction_failed", inventory)
		}
	})

	t.Run("results were read and none carried a token-bearing extension", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeWireTrace(t, dir, capturedSessionNewResult, capturedCancelledResult)

		sessionID, paths, inventory, _ := protocolTokenInventory(&sharedFixture{usage: &usageTracker{}, wireTraceDir: dir})
		if sessionID != "" || paths != nil {
			t.Errorf("protocolTokenInventory(...) = (%q, %v), want the empty zero-source sentinel", sessionID, paths)
		}
		if inventory.Grade != qualification.GradeGap || inventory.Outcome != qualification.OutcomePass {
			t.Errorf("protocolTokenInventory(...) inventory = %+v, want gap/pass: this is a measured absence, not an unread one", inventory)
		}
	})

	t.Run("a result carried an extension a budget can be kept in", func(t *testing.T) {
		t.Parallel()

		sessionID, paths, inventory, _ := protocolTokenInventory(&sharedFixture{usage: &usageTracker{}, wireTraceDir: completeExtensionTrace(t)})
		if sessionID != "a-captured-session" {
			t.Errorf("protocolTokenInventory(...) sessionID = %q, want %q", sessionID, "a-captured-session")
		}
		if len(paths) == 0 {
			t.Fatalf("protocolTokenInventory(...) paths = %+v, want the counters the capture carried", paths)
		}
		for _, path := range paths {
			if path.Kind != "spend" {
				t.Errorf("protocolTokenInventory(...) path %+v, want kind spend", path)
			}
		}
		if inventory.Grade != qualification.GradeGap || inventory.Outcome != qualification.OutcomePass {
			t.Errorf("protocolTokenInventory(...) inventory = %+v, want gap/pass: (usable, pass) is outside SetTokenInventory's admitted pairs", inventory)
		}

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		if err := fixture.SetTokenInventory(qualification.SurfaceProtocol, sessionID, paths, inventory, nil); err != nil {
			t.Fatalf("SetTokenInventory(protocol, %q, %v, %+v) error = %v, want nil", sessionID, paths, inventory, err)
		}
		rec := fixture.FindFirst(func(r *qualification.Record) bool {
			return r.Scenario == qualification.ScenarioTokenSource && r.Surface == qualification.SurfaceProtocol && r.EvidencePath != nil
		})
		if rec == nil {
			t.Fatal("SetTokenInventory(protocol, ...) left no per-path token-source record for the reported spend path")
		}
		if rec.Grade != qualification.GradeUsable {
			t.Errorf("SetTokenInventory(protocol, ...) per-path record grade = %s, want %s: the usable grade must survive on the per-path record", rec.Grade, qualification.GradeUsable)
		}
		if rec.Source != qualification.SourceProtocolStable {
			t.Errorf("SetTokenInventory(protocol, ...) per-path record source = %s, want %s", rec.Source, qualification.SourceProtocolStable)
		}
	})
}

func TestNativeTokenInventory(t *testing.T) {
	t.Parallel()

	numericRecognizer := qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		SuccessMember: "ok",
		SessionIDPath: []string{"session_id"},
		TokenPaths:    []qualification.TokenPath{{Path: []string{"usage", "total"}, Kind: "spend"}},
	}

	t.Run("no recognizer for the surface sentinels fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: qualification.RuntimeProfile{}}
		sessionID, paths, inventory := nativeTokenInventory(t, coords, tokenTestSurface, []string{`{"ok":true,"usage":{"total":5}}`})
		if sessionID != "" || paths != nil {
			t.Errorf("nativeTokenInventory(...) = (%q, %v), want empty sentinel", sessionID, paths)
		}
		if inventory.Grade != qualification.GradeNotObserved || inventory.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("nativeTokenInventory(...) inventory = %+v, want not_observed/fixture_induction_failed", inventory)
		}
	})

	t.Run("no recognized outputs sentinels fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: tokenRecognizerProfile(numericRecognizer)}
		_, _, inventory := nativeTokenInventory(t, coords, tokenTestSurface, nil)
		if inventory.Grade != qualification.GradeNotObserved || inventory.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("nativeTokenInventory(...) inventory = %+v, want not_observed/fixture_induction_failed", inventory)
		}
	})

	t.Run("a recognizer with no token_paths is the zero-source gap", func(t *testing.T) {
		t.Parallel()

		bare := qualification.Recognizer{Locator: qualification.TerminalLocator{Mode: "first_value"}, SuccessMember: "ok"}
		coords := Coordinates{Profile: tokenRecognizerProfile(bare)}
		sessionID, paths, inventory := nativeTokenInventory(t, coords, tokenTestSurface, []string{`{"ok":true}`})
		if sessionID != "" || paths != nil {
			t.Errorf("nativeTokenInventory(...) = (%q, %v), want the empty zero-source sentinel", sessionID, paths)
		}
		if inventory.Grade != qualification.GradeGap || inventory.Outcome != qualification.OutcomePass {
			t.Errorf("nativeTokenInventory(...) inventory = %+v, want gap/pass: a runtime that ran fine but declares no token-bearing member is not an induction failure", inventory)
		}
	})

	t.Run("a recognized terminal resolving no token path is the zero-source gap", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: tokenRecognizerProfile(numericRecognizer)}
		sessionID, paths, inventory := nativeTokenInventory(t, coords, tokenTestSurface, []string{`{"ok":true}`})
		if sessionID != "" || paths != nil {
			t.Errorf("nativeTokenInventory(...) = (%q, %v), want the empty zero-source sentinel", sessionID, paths)
		}
		if inventory.Grade != qualification.GradeGap || inventory.Outcome != qualification.OutcomePass {
			t.Errorf("nativeTokenInventory(...) inventory = %+v, want gap/pass", inventory)
		}
	})

	t.Run("a recognized terminal with a numeric path contributes one record", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: tokenRecognizerProfile(numericRecognizer)}
		sessionID, paths, inventory := nativeTokenInventory(t, coords, tokenTestSurface, []string{`{"ok":true,"session_id":"sess-native-1","usage":{"total":5}}`})
		if len(paths) != 1 || paths[0].Kind != "spend" || paths[0].EvidencePath != "/usage/total" {
			t.Errorf("nativeTokenInventory(...) paths = %+v, want one /usage/total spend path", paths)
		}
		if sessionID != "sess-native-1" {
			t.Errorf("nativeTokenInventory(...) sessionID = %q, want the terminal's own %q", sessionID, "sess-native-1")
		}
		if inventory.Grade != qualification.GradeGap || inventory.Outcome != qualification.OutcomePass {
			t.Errorf("nativeTokenInventory(...) inventory = %+v, want gap/pass", inventory)
		}
	})

	t.Run("a path repeated across multiple recognized outputs is not duplicated", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: tokenRecognizerProfile(numericRecognizer)}
		outputs := []string{`{"ok":true,"session_id":"sess-native-1","usage":{"total":5}}`, `{"ok":true,"session_id":"sess-native-1","usage":{"total":9}}`}
		_, paths, _ := nativeTokenInventory(t, coords, tokenTestSurface, outputs)
		if len(paths) != 1 {
			t.Errorf("nativeTokenInventory(...) paths = %+v, want exactly one deduplicated record", paths)
		}
	})
}

func TestInduceNativeContinuation(t *testing.T) {
	t.Parallel()

	recognizer := qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		SuccessMember: "ok",
	}

	newFixture := func(t *testing.T, seedOutput, recallOutput string) (*sharedFixture, string) {
		t.Helper()
		script := agenttest.FakeRuntime(t, t.TempDir(), "continuation-native", continuationNativeScenario,
			continuationNativeParams{SeedOutput: seedOutput, RecallOutput: recallOutput})
		fixture := &sharedFixture{
			workspaceRoot: t.TempDir(),
			tracker:       &groupTracker{},
			nonce:         "TESTNONCE123",
		}
		return fixture, script
	}

	tests := []struct {
		name         string
		recallOutput string
		wantGrade    qualification.Grade
		wantOutcome  qualification.Outcome
		wantDetail   string
	}{
		{
			name:         "the recall echoes the seed's own nonce",
			recallOutput: `{"ok":true,"echo":"TESTNONCE123"}`,
			wantGrade:    qualification.GradeUsable,
			wantOutcome:  qualification.OutcomePass,
			wantDetail:   qualification.RecallConfirmedSameSession,
		},
		{
			name:         "the recall completes without the nonce on a surface naming no session",
			recallOutput: `{"ok":true}`,
			wantGrade:    qualification.GradeNotObserved,
			wantOutcome:  qualification.OutcomeRuntimeFailed,
			wantDetail:   qualification.RecallUnobservedActual,
		},
		{
			name:         "the recall produces no recognized terminal",
			recallOutput: "not json at all",
			wantGrade:    qualification.GradeNotObserved,
			wantOutcome:  qualification.OutcomeRuntimeFailed,
			wantDetail:   qualification.RecallUnobservedActual,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, script := newFixture(t, `{"ok":true}`, tt.recallOutput)
			profile := tokenRecognizerProfile(recognizer)
			entry := profile.EntryPoints[tokenTestSurface]
			entry.SeedArgs = []string{"--seed"}
			entry.ResumeArgs = []string{"--resume"}
			profile.EntryPoints[tokenTestSurface] = entry
			coords := Coordinates{CommandPath: script, Profile: profile}

			seed, recall := induceNativeContinuation(t, coords, fixture, tokenTestSurface)
			if seed.Grade != qualification.GradeUsable || seed.Outcome != qualification.OutcomePass {
				t.Errorf("induceNativeContinuation(...) seed = %+v, want usable/pass", seed)
			}
			if recall.Grade != tt.wantGrade || recall.Outcome != tt.wantOutcome || recall.Detail != tt.wantDetail {
				t.Errorf("induceNativeContinuation(...) recall = %+v, want grade %s outcome %s detail %s", recall, tt.wantGrade, tt.wantOutcome, tt.wantDetail)
			}
		})
	}

	t.Run("a surface stating no seed_args/resume_args skips the recall", func(t *testing.T) {
		t.Parallel()

		fixture, script := newFixture(t, `{"ok":true}`, `{"ok":true}`)
		profile := tokenRecognizerProfile(recognizer)
		coords := Coordinates{CommandPath: script, Profile: profile}

		seed, recall := induceNativeContinuation(t, coords, fixture, tokenTestSurface)
		if seed.Grade != qualification.GradeUsable {
			t.Errorf("induceNativeContinuation(...) seed.Grade = %s, want %s: the seed always launches regardless of the recall precondition", seed.Grade, qualification.GradeUsable)
		}
		if recall.Grade != qualification.GradeNotObserved || recall.Outcome != qualification.OutcomePrerequisiteFailed || recall.Detail != qualification.RecallPreconditionUnmet {
			t.Errorf("induceNativeContinuation(...) recall = %+v, want not_observed/prerequisite_failed/recall_precondition_unmet", recall)
		}
	})
}

// A collection whose turns all came back unmeasured has nothing to publish,
// however many of them ran.
func TestCompensationComesOnlyFromAReturnedFigure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		observe  func(*usageTracker)
		supplied bool
		session  string
	}{
		{
			name:    "no turn ran at all",
			observe: func(*usageTracker) {},
		},
		{
			name: "every turn came back unmeasured",
			observe: func(u *usageTracker) {
				u.observe("sess-one", false)
				u.observe("sess-two", false)
			},
		},
		{
			name: "one turn returned a figure",
			observe: func(u *usageTracker) {
				u.observe("sess-one", false)
				u.observe("sess-two", true)
				u.observe("sess-three", false)
			},
			supplied: true,
			session:  "sess-two",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := &sharedFixture{usage: &usageTracker{}}
			tc.observe(fixture.usage)

			got := protocolTokenCompensation(fixture)
			if got.supplied != tc.supplied {
				t.Errorf("supplied = %v, want %v", got.supplied, tc.supplied)
			}
			if got.sessionID != tc.session {
				t.Errorf("sessionID = %q, want %q", got.sessionID, tc.session)
			}
		})
	}
}
