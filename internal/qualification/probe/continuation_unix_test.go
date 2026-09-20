//go:build unix

package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

func continuationCoordinates(runtimePath string) Coordinates {
	return Coordinates{
		CommandPath: runtimePath,
		Profile: qualification.RuntimeProfile{
			ProbePrompts: map[string]string{
				promptKeyContinuationSeed:   "please seed",
				promptKeyContinuationRecall: "please recall",
			},
			EntryPoints: map[qualification.Surface]qualification.EntryPoint{
				semanticTestSurface: {
					Args:       []string{"--native"},
					SeedArgs:   []string{"--seed"},
					ResumeArgs: []string{"--resume"},
				},
			},
			Recognizers: map[qualification.Surface]qualification.Recognizer{semanticTestSurface: semanticRecognizer},
		},
	}
}

func TestInduceNativeContinuationTransportLoss(t *testing.T) {
	t.Parallel()

	script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{ExitCode: 1})
	seed, recall := induceNativeContinuation(t, continuationCoordinates(script), semanticFixture(t), semanticTestSurface)

	if seed.Grade != qualification.GradeNotObserved || seed.Outcome != qualification.OutcomeRuntimeFailed {
		t.Errorf("induceNativeContinuation(...) seed = %+v, want not_observed/runtime_failed: a launch the collector itself ended must never be graded usable, and never a pass", seed)
	}

	if recall.Grade != qualification.GradeNotObserved || recall.Outcome != qualification.OutcomeRuntimeFailed || recall.Detail != qualification.RecallUnobservedActual {
		t.Errorf("induceNativeContinuation(...) recall = %+v, want not_observed/runtime_failed/%q: the validator requires that closed detail for an unobserved actual session, never a gap-level pass with the fresh-fallback detail", recall, qualification.RecallUnobservedActual)
	}
}

type continuationArgvRecorderParams struct {
	ReceiptDir   string
	SeedOutput   string
	RecallOutput string
}

const continuationArgvRecorderScenario = "continuation-argv-recorder"

func runContinuationArgvRecorder(args []string, params continuationArgvRecorderParams) int {
	joined := strings.Join(args, "\x00")
	var name, output string
	switch {
	case strings.Contains(joined, "SEED_MARKER"):
		name, output = "seed", params.SeedOutput
	case strings.Contains(joined, "RECALL_MARKER"):
		name, output = "recall", params.RecallOutput
	default:
		return 1
	}
	if err := os.WriteFile(filepath.Join(params.ReceiptDir, name+".argv"), []byte(joined), 0o600); err != nil {
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 2
	}
	if err := os.WriteFile(filepath.Join(params.ReceiptDir, name+".cwd"), []byte(cwd), 0o600); err != nil {
		return 2
	}
	if _, err := fmt.Fprint(os.Stdout, output); err != nil {
		return 2
	}
	return 0
}

func init() {
	probeScenarios[continuationArgvRecorderScenario] = agenttest.Typed(runContinuationArgvRecorder)
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func readReceipt(t *testing.T, dir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read receipt %s: %v: the launch it names never ran", name, err)
	}
	return string(content)
}

func TestInduceNativeContinuationArgvAndWorkspace(t *testing.T) {
	t.Parallel()

	receiptDir := t.TempDir()
	script := agenttest.FakeRuntime(t, t.TempDir(), "native", continuationArgvRecorderScenario, continuationArgvRecorderParams{
		ReceiptDir:   receiptDir,
		SeedOutput:   `{"type":"result","status":"end_turn","session_id":"resolved-from-output"}`,
		RecallOutput: `{"type":"result","status":"end_turn"}`,
	})

	profile := qualification.RuntimeProfile{
		ProbePrompts: map[string]string{
			promptKeyContinuationSeed:   "SEED_MARKER nonce={nonce}",
			promptKeyContinuationRecall: "RECALL_MARKER",
		},
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			semanticTestSurface: {
				Args:       []string{"--prompt", "{prompt}"},
				SeedArgs:   []string{"--seed", "{session_id}"},
				ResumeArgs: []string{"--resume", "{session_id}"},
			},
		},
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			semanticTestSurface: {
				Locator:       qualification.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
				StatusMember:  "status",
				StatusEndTurn: []string{"end_turn"},
				SessionIDPath: []string{"session_id"},
			},
		},
	}

	fixture := semanticFixture(t)
	fixture.nonce = "TESTNONCE456"

	induceNativeContinuation(t, Coordinates{CommandPath: script, Profile: profile}, fixture, semanticTestSurface)

	seedArgv := readReceipt(t, receiptDir, "seed.argv")
	seedCwd := readReceipt(t, receiptDir, "seed.cwd")
	recallArgv := readReceipt(t, receiptDir, "recall.argv")
	recallCwd := readReceipt(t, receiptDir, "recall.cwd")

	t.Run("the seed launch receives a real identifier, never the placeholder text", func(t *testing.T) {
		t.Parallel()

		if strings.Contains(seedArgv, "{session_id}") {
			t.Errorf("seed argv = %q, want no unsubstituted {session_id} placeholder", seedArgv)
		}
		fields := strings.Split(seedArgv, "\x00")
		idx := slices.Index(fields, "--seed")
		if idx == -1 || idx+1 >= len(fields) || !uuidV4Pattern.MatchString(fields[idx+1]) {
			t.Errorf("seed argv = %v, want a v4 UUID immediately after --seed", fields)
		}
	})

	t.Run("the recall runs in the seed's own workspace and names its resolved identifier", func(t *testing.T) {
		t.Parallel()

		if recallCwd != seedCwd {
			t.Errorf("recall cwd = %q, want the seed's own workspace %q", recallCwd, seedCwd)
		}
		if strings.Contains(recallArgv, "{session_id}") {
			t.Errorf("recall argv = %q, want no unsubstituted {session_id} placeholder", recallArgv)
		}
		if !strings.Contains(recallArgv, "resolved-from-output") {
			t.Errorf("recall argv = %q, want it to name the seed's own resolved session id %q rather than a fixed literal", recallArgv, "resolved-from-output")
		}
	})

	t.Run("the seed prompt carries the run's own nonce", func(t *testing.T) {
		t.Parallel()

		if strings.Contains(seedArgv, "{nonce}") {
			t.Errorf("seed argv = %q, want no unsubstituted {nonce} placeholder", seedArgv)
		}
		if !strings.Contains(seedArgv, "TESTNONCE456") {
			t.Errorf("seed argv = %q, want it to carry the run's own nonce %q", seedArgv, "TESTNONCE456")
		}
	})
}
