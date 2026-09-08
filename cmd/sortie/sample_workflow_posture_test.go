package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/workflow"
)

// publishedACPSampleAgentCommand loads the named sample workflow from
// examples/ and returns its agent.command string.
func publishedACPSampleAgentCommand(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "examples", name)
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("workflow.Load(%s): %v", name, err)
	}
	agentBlock, ok := wf.Config["agent"].(map[string]any)
	if !ok {
		t.Fatalf("workflow.Load(%s): agent block is not a map", name)
	}
	command, ok := agentBlock["command"].(string)
	if !ok {
		t.Fatalf("workflow.Load(%s): agent.command is not a string", name)
	}
	return command
}

// publishedPostureReporter is the subset of *testing.T
// checkPublishedPosture calls, factored out so the self-check subtest
// can drive it against fixture commands without failing its own run.
type publishedPostureReporter interface {
	Errorf(format string, args ...any)
}

// checkPublishedPosture reports every required launch-posture flag
// missing from command: the trust switch and the approval-mode
// posture switch that together make the launch non-interactive.
func checkPublishedPosture(r publishedPostureReporter, command string) {
	for _, want := range []string{"--skip-trust", "--approval-mode yolo"} {
		if !strings.Contains(command, want) {
			r.Errorf("agent.command = %q, want it to contain %q", command, want)
		}
	}
}

// publishedPostureRecorder records Errorf calls instead of failing the
// enclosing test, so the missing-flag subtests can drive
// checkPublishedPosture's rejecting arm without reddening their own
// run.
type publishedPostureRecorder struct {
	errors []string
}

func (r *publishedPostureRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// TestSampleWorkflowPublishedPosture confirms the published sample's
// agent.command carries both the trust switch and the posture switch
// that make the shipped launch non-interactive, and that the check
// itself is capable of catching a command missing either one.
func TestSampleWorkflowPublishedPosture(t *testing.T) {
	t.Parallel()

	command := publishedACPSampleAgentCommand(t, "WORKFLOW.agent-client-protocol.md")
	checkPublishedPosture(t, command)

	t.Run("the check fails a command missing either flag", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			command string
		}{
			{"missing trust switch", "gemini --acp --approval-mode yolo"},
			{"missing posture switch", "gemini --acp --skip-trust"},
			{"missing both", "gemini --acp"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				recorder := &publishedPostureRecorder{}
				checkPublishedPosture(recorder, tt.command)
				if len(recorder.errors) == 0 {
					t.Errorf("checkPublishedPosture(%q) recorded no failure, want at least one", tt.command)
				}
			})
		}
	})
}
