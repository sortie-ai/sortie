//go:build unix

package probe

import (
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

const semanticTestSurface = evidence.SurfaceNativeStreamJSON

var semanticRecognizer = profile.Recognizer{
	Locator:       profile.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
	ErrorMembers:  []string{"error"},
	StatusMember:  "status",
	StatusCases:   map[string]evidence.Case{"refusal": evidence.CaseRuntimeRefusal, "cancelled": evidence.CaseCancellation, "input_required": evidence.CaseHumanInput},
	StatusEndTurn: []string{"end_turn"},
}

func semanticCoordinates(runtimePath string) Coordinates {
	return Coordinates{
		CommandPath: runtimePath,
		Profile: profile.RuntimeProfile{
			ProbePrompts: map[string]string{"success": "please succeed", "runtime_refusal": "please refuse"},
			EntryPoints: map[evidence.Surface]profile.EntryPoint{
				semanticTestSurface: {
					Args:       []string{"--prompt", "{prompt}"},
					AskingArgs: []string{"--ask", "--prompt", "{prompt}"},
				},
			},
			Recognizers: map[evidence.Surface]profile.Recognizer{semanticTestSurface: semanticRecognizer},
		},
	}
}

func semanticFixture(t *testing.T) *sharedFixture {
	t.Helper()
	return &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}, usage: &usageTracker{}}
}

func TestRecordUnrecognizedTerminal(t *testing.T) {
	t.Parallel()

	t.Run("a surface with no recognizer records nothing", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: profile.RuntimeProfile{}}
		fixture := semanticFixture(t)

		recordUnrecognizedTerminal(coords, fixture, semanticTestSurface, `{"type":"result","status":"mystery"}`)

		if got := fixture.unrecognizedTerminals(); len(got) != 0 {
			t.Errorf("fixture.unrecognizedTerminals() = %+v, want none: the surface carries no recognizer", got)
		}
	})

	t.Run("a launch that produced no envelope at all records nothing", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{semanticTestSurface: semanticRecognizer}}}
		fixture := semanticFixture(t)

		recordUnrecognizedTerminal(coords, fixture, semanticTestSurface, "plain unrecognized text")

		if got := fixture.unrecognizedTerminals(); len(got) != 0 {
			t.Errorf("fixture.unrecognizedTerminals() = %+v, want none: no envelope was located", got)
		}
	})

	t.Run("a located envelope the recognizer cannot resolve is recorded verbatim", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{semanticTestSurface: semanticRecognizer}}}
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
		g := induceSuccess(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeUsable || g.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceSuccess(...) = %+v, want usable/pass", g.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
			t.Errorf("induceSuccess(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		g := induceSuccess(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeNotObserved || g.obs.Outcome != evidence.OutcomeFixtureInductionFailed {
			t.Errorf("induceSuccess(...) = %+v, want not_observed/fixture_induction_failed", g.obs)
		}
		if g.obs.EvidencePath != "" {
			t.Errorf("induceSuccess(...) evidence path = %q, want empty: no outcome was observed", g.obs.EvidencePath)
		}
	})
}

func TestInduceRuntimeFailureNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized error terminal grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","error":"boom"}` + "\n"})
		g := induceRuntimeFailure(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeUsable || g.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceRuntimeFailure(...) = %+v, want usable/pass", g.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
			t.Errorf("induceRuntimeFailure(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
		}
	})

	t.Run("a clean end of turn grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"end_turn"}` + "\n"})
		g := induceRuntimeFailure(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeNotObserved || g.obs.Outcome != evidence.OutcomeFixtureInductionFailed {
			t.Errorf("induceRuntimeFailure(...) = %+v, want not_observed/fixture_induction_failed", g.obs)
		}
		if g.obs.EvidencePath != "" {
			t.Errorf("induceRuntimeFailure(...) evidence path = %q, want empty: no outcome was observed", g.obs.EvidencePath)
		}
	})
}

func TestInduceRefusalPairNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized refusal status grades usable/pass and derives its peer", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"refusal"}` + "\n"})
		disposition, retry := induceRefusalPair(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if disposition.obs.Grade != evidence.GradeUsable || disposition.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceRefusalPair(...) disposition = %+v, want usable/pass", disposition.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); disposition.obs.EvidencePath != want {
			t.Errorf("induceRefusalPair(...) disposition evidence path = %q, want %q", disposition.obs.EvidencePath, want)
		}
		if retry.obs != disposition.obs {
			t.Errorf("induceRefusalPair(...) retry = %+v, want it equal to disposition %+v: it is derived from the same run", retry.obs, disposition.obs)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		disposition, retry := induceRefusalPair(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if disposition.obs.Grade != evidence.GradeNotObserved || disposition.obs.Outcome != evidence.OutcomeFixtureInductionFailed {
			t.Errorf("induceRefusalPair(...) disposition = %+v, want not_observed/fixture_induction_failed", disposition.obs)
		}
		if retry.obs != disposition.obs {
			t.Errorf("induceRefusalPair(...) retry = %+v, want it equal to disposition %+v", retry.obs, disposition.obs)
		}
	})
}

func TestInduceHumanInputNative(t *testing.T) {
	t.Parallel()

	t.Run("a recognized human-input terminal grades usable/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"input_required"}` + "\n"})
		g := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeUsable || g.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceHumanInputNative(...) = %+v, want usable/pass", g.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
		}
	})

	t.Run("a recognized terminal with no probe marker grades gap/pass", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"type":"result","status":"end_turn"}` + "\n"})
		g := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeGap || g.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceHumanInputNative(...) = %+v, want gap/pass", g.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
		}
	})

	t.Run("unrecognized output grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized text\n"})
		g := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeNotObserved || g.obs.Outcome != evidence.OutcomeFixtureInductionFailed {
			t.Errorf("induceHumanInputNative(...) = %+v, want not_observed/fixture_induction_failed", g.obs)
		}
		if g.obs.EvidencePath != "" {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want empty: no outcome was observed", g.obs.EvidencePath)
		}
	})

	t.Run("a launch that ends in transport loss with no probe marker grades not_observed/runtime_failed", func(t *testing.T) {
		t.Parallel()

		// An abnormal exit with no output exercises the same found=false,
		// transportLoss=true path a launch killed at the collector's bound would.
		script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{ExitCode: 1})
		g := induceHumanInputNative(t, semanticCoordinates(script), semanticFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeNotObserved || g.obs.Outcome != evidence.OutcomeRuntimeFailed {
			t.Errorf("induceHumanInputNative(...) = %+v, want not_observed/runtime_failed, never the gap/pass a found=true launch error used to produce", g.obs)
		}
		if g.obs.EvidencePath != "" {
			t.Errorf("induceHumanInputNative(...) evidence path = %q, want empty: no outcome was observed", g.obs.EvidencePath)
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
		g := induceCancellation(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeUsable || g.obs.Outcome != evidence.OutcomePass {
			t.Errorf("induceCancellation(...) = %+v, want usable/pass", g.obs)
		}
		if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
			t.Errorf("induceCancellation(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
		}
	})

	t.Run("a recognized but differently-cased status grades not_observed/fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", asyncCancellationScenario, asyncCancellationParams{Status: "refusal"})
		g := induceCancellation(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
		if g.obs.Grade != evidence.GradeNotObserved || g.obs.Outcome != evidence.OutcomeFixtureInductionFailed {
			t.Errorf("induceCancellation(...) = %+v, want not_observed/fixture_induction_failed", g.obs)
		}
		if g.obs.EvidencePath != "" {
			t.Errorf("induceCancellation(...) evidence path = %q, want empty: no outcome was observed", g.obs.EvidencePath)
		}
	})
}

func TestInduceRetryableTransportNative(t *testing.T) {
	t.Parallel()

	script := agenttest.FakeRuntime(t, t.TempDir(), "native", asyncHangWithMarkerScenario, struct{}{})
	g := induceRetryableTransport(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)
	if g.obs.Grade != evidence.GradeUsable || g.obs.Outcome != evidence.OutcomePass {
		t.Errorf("induceRetryableTransport(...) = %+v, want usable/pass", g.obs)
	}
	if want := evidence.SemanticEvidencePath(semanticTestSurface); g.obs.EvidencePath != want {
		t.Errorf("induceRetryableTransport(...) evidence path = %q, want %q", g.obs.EvidencePath, want)
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
