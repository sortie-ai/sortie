//go:build unix

package probe

import (
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

const semanticTestSurface = qualification.SurfaceNativeStreamJSON

var semanticRecognizer = qualification.Recognizer{
	Locator:       qualification.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
	ErrorMembers:  []string{"error"},
	StatusMember:  "status",
	StatusCases:   map[string]qualification.Case{"refusal": qualification.CaseRuntimeRefusal, "cancelled": qualification.CaseCancellation, "input_required": qualification.CaseHumanInput},
	StatusEndTurn: []string{"end_turn"},
}

func semanticCoordinates(runtimePath string) Coordinates {
	return Coordinates{
		CommandPath: runtimePath,
		Profile: qualification.RuntimeProfile{
			ProbePrompts: map[string]string{"success": "please succeed", "runtime_refusal": "please refuse"},
			EntryPoints: map[qualification.Surface]qualification.EntryPoint{
				semanticTestSurface: {
					Args:       []string{"--prompt", "{prompt}"},
					AskingArgs: []string{"--ask", "--prompt", "{prompt}"},
				},
			},
			Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer},
		},
	}
}

func semanticFixture(t *testing.T) *sharedFixture {
	t.Helper()
	return &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}, usage: &usageTracker{}}
}

func TestNativeSessionID(t *testing.T) {
	t.Parallel()

	output := `{"type":"result","status":"end_turn","session_id":"sess-native"}`
	withSessionIDPath := semanticRecognizer
	withSessionIDPath.SessionIDPath = []string{"session_id"}

	tests := []struct {
		name    string
		coords  Coordinates
		surface qualification.Surface
		output  string
		want    string
	}{
		{
			name:    "no recognizer for the surface reports empty",
			coords:  Coordinates{Profile: qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer}}},
			surface: qualification.SurfaceNativeJSON,
			output:  output,
			want:    "",
		},
		{
			name:    "a recognizer stating no session_id_path reports empty",
			coords:  Coordinates{Profile: qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer}}},
			surface: semanticTestSurface,
			output:  output,
			want:    "",
		},
		{
			name:    "a resolvable session_id_path reports its value",
			coords:  Coordinates{Profile: qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: withSessionIDPath}}},
			surface: semanticTestSurface,
			output:  output,
			want:    "sess-native",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := nativeSessionID(tt.coords, tt.surface, tt.output); got != tt.want {
				t.Errorf("nativeSessionID(..., %q, %q) = %q, want %q", tt.surface, tt.output, got, tt.want)
			}
		})
	}
}

func TestRecordUnrecognizedTerminal(t *testing.T) {
	t.Parallel()

	t.Run("a surface with no recognizer records nothing", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: qualification.RuntimeProfile{}}
		fixture := semanticFixture(t)

		recordUnrecognizedTerminal(coords, fixture, semanticTestSurface, `{"type":"result","status":"mystery"}`)

		if got := fixture.unrecognizedTerminals(); len(got) != 0 {
			t.Errorf("fixture.unrecognizedTerminals() = %+v, want none: the surface carries no recognizer", got)
		}
	})

	t.Run("a launch that produced no envelope at all records nothing", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer}}}
		fixture := semanticFixture(t)

		recordUnrecognizedTerminal(coords, fixture, semanticTestSurface, "plain unrecognized text")

		if got := fixture.unrecognizedTerminals(); len(got) != 0 {
			t.Errorf("fixture.unrecognizedTerminals() = %+v, want none: no envelope was located", got)
		}
	})

	t.Run("a located envelope the recognizer cannot resolve is recorded verbatim", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer}}}
		fixture := semanticFixture(t)

		recordUnrecognizedTerminal(coords, fixture, semanticTestSurface, `{"type":"result","status":"mystery"}`)

		got := fixture.unrecognizedTerminals()
		want := []map[string]any{{"type": "result", "status": "mystery"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("fixture.unrecognizedTerminals() = %+v, want %+v", got, want)
		}
	})
}

func TestInduceSuccessNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized end-of-turn terminal grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"end_turn"}` + "\n"})
		obs := induceSuccess(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceSuccess(...) = %+v, want usable/pass", obs)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
			t.Errorf("induceSuccess(...) evidence path = %q, want %q", obs.EvidencePath, want)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		obs := induceSuccess(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceSuccess(...) = %+v, want not_observed/fixture_induction_failed", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceSuccess(...) evidence path = %q, want empty: no outcome was observed", obs.EvidencePath)
		}
	})
}

func TestInduceRuntimeFailureNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized error terminal grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","error":"boom"}` + "\n"})
		obs := induceRuntimeFailure(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceRuntimeFailure(...) = %+v, want usable/pass", obs)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
			t.Errorf("induceRuntimeFailure(...) evidence path = %q, want %q", obs.EvidencePath, want)
		}
	})

	t.Run("a clean end of turn grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"end_turn"}` + "\n"})
		obs := induceRuntimeFailure(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceRuntimeFailure(...) = %+v, want not_observed/fixture_induction_failed", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceRuntimeFailure(...) evidence path = %q, want empty: no outcome was observed", obs.EvidencePath)
		}
	})
}

func TestInduceRefusalPairNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized refusal status grades usable/pass and derives its peer", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"refusal"}` + "\n"})
		disposition, retry := induceRefusalPair(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if disposition.Grade != qualification.GradeUsable || disposition.Outcome != qualification.OutcomePass {
			t.Errorf("induceRefusalPair(...) disposition = %+v, want usable/pass", disposition)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); disposition.EvidencePath != want {
			t.Errorf("induceRefusalPair(...) disposition evidence path = %q, want %q", disposition.EvidencePath, want)
		}
		if retry != disposition {
			t.Errorf("induceRefusalPair(...) retry = %+v, want it equal to disposition %+v: it is derived from the same run", retry, disposition)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		disposition, retry := induceRefusalPair(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if disposition.Grade != qualification.GradeNotObserved || disposition.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceRefusalPair(...) disposition = %+v, want not_observed/fixture_induction_failed", disposition)
		}
		if retry != disposition {
			t.Errorf("induceRefusalPair(...) retry = %+v, want it equal to disposition %+v", retry, disposition)
		}
	})
}

func TestInduceHumanInputNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized human-input terminal grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"input_required"}` + "\n"})
		obs := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceHumanInputNative(...) = %+v, want usable/pass", obs)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want %q", obs.EvidencePath, want)
		}
	})

	t.Run("a recognized terminal with no probe marker grades gap/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"end_turn"}` + "\n"})
		obs := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeGap || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceHumanInputNative(...) = %+v, want gap/pass", obs)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want %q", obs.EvidencePath, want)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		obs := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceHumanInputNative(...) = %+v, want not_observed/fixture_induction_failed", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want empty: no outcome was observed", obs.EvidencePath)
		}
	})

	t.Run("a launch that ends in transport loss with no probe marker grades not_observed/runtime_failed", func(t *testing.T) {
		t.Parallel()

		// An abnormal exit with no output exercises the same found=false,
		// transportLoss=true path a launch killed at the collector's bound would.
		// Before the fix a launch error read as found=true and, with no probe
		// marker, fell into the gap/pass arm meant for a recognized terminal.
		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{ExitCode: 1})
		obs := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("induceHumanInputNative(...) = %+v, want not_observed/runtime_failed, never the gap/pass a found=true launch error used to produce", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want empty: no outcome was observed", obs.EvidencePath)
		}
	})
}

type asyncCancellationParams struct {
	Status string
}

const asyncCancellationScenario = "async-cancellation"

func runAsyncCancellation(args []string, params asyncCancellationParams) int {
	if !spawnNamedProbe(args) {
		return 2
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	<-sigCh
	if _, err := fmt.Fprintf(os.Stdout, `{"type":"result","status":%q}`+"\n", params.Status); err != nil {
		return 2
	}
	return 0
}

const asyncHangWithMarkerScenario = "async-hang-with-marker"

func runAsyncHangWithMarker(args []string, _ struct{}) int {
	if !spawnNamedProbe(args) {
		return 2
	}
	agenttest.Hang()
	return 0
}

func init() {
	probeScenarios[asyncCancellationScenario] = agenttest.Typed(runAsyncCancellation)
	probeScenarios[asyncHangWithMarkerScenario] = agenttest.Typed(runAsyncHangWithMarker)
}

func TestInduceCancellationNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized cancellation status grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", asyncCancellationScenario, asyncCancellationParams{Status: "cancelled"})
		obs := induceCancellation(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceCancellation(...) = %+v, want usable/pass", obs)
		}
		if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
			t.Errorf("induceCancellation(...) evidence path = %q, want %q", obs.EvidencePath, want)
		}
	})

	t.Run("a recognized but differently-cased status grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", asyncCancellationScenario, asyncCancellationParams{Status: "refusal"})
		obs := induceCancellation(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceCancellation(...) = %+v, want not_observed/fixture_induction_failed", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceCancellation(...) evidence path = %q, want empty: no outcome was observed", obs.EvidencePath)
		}
	})
}

func TestInduceRetryableTransportNative(t *testing.T) {
	t.Parallel()

	script := agenttest.FakeRuntime(t, t.TempDir(), "native", asyncHangWithMarkerScenario, struct{}{})
	obs := induceRetryableTransport(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
	if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
		t.Errorf("induceRetryableTransport(...) = %+v, want usable/pass", obs)
	}
	if want := qualification.SemanticEvidencePath(semanticTestSurface); obs.EvidencePath != want {
		t.Errorf("induceRetryableTransport(...) evidence path = %q, want %q", obs.EvidencePath, want)
	}
}

func TestAwaitProbeStarted(t *testing.T) {
	t.Parallel()

	t.Run("the marker appears before the deadline", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		go func() {
			time.Sleep(30 * time.Millisecond)
			_ = os.WriteFile(dir+"/"+probeStartedMarker, nil, 0o600)
		}()
		if !awaitProbeStarted(dir, time.Now().Add(2*time.Second)) {
			t.Error("awaitProbeStarted(...) = false, want true once the marker appears")
		}
	})

	t.Run("the deadline passes with no marker", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if awaitProbeStarted(dir, time.Now().Add(20*time.Millisecond)) {
			t.Error("awaitProbeStarted(...) = true, want false: no marker was ever written")
		}
	})
}

func TestPromptNamingProbe(t *testing.T) {
	t.Parallel()

	fixture := &sharedFixture{failingProbe: "/tmp/x/failing-probe"}
	got := promptNamingProbe(fixture, "failing")
	if !strings.Contains(got, fixture.failingProbe) {
		t.Errorf("promptNamingProbe(...) = %q, want it to name %q", got, fixture.failingProbe)
	}
}
