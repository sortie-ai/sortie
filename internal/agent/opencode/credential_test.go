//go:build unix

package opencode

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestCredentialVerification(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH.
	verifiedBin := writeRunFixtureScript(t, t.TempDir(), "simple_turn.jsonl")
	unverifiedBin := writeRunFixtureScript(t, t.TempDir(), "logical_failure_exit0.jsonl")

	adapter, err := NewOpenCodeAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter() error = %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "opencode", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{}, verifiedBin, unverifiedBin, "opencode"))
}

func TestDeleteVerificationSession_CarriesManagedEnv(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, "env.txt")
	script := agenttest.WriteScript(t, tmpDir, "fake-opencode-delete",
		"printf '%s' \"$OPENCODE_DISABLE_AUTOUPDATE\" > '"+envPath+"'\n")
	state := &sessionState{
		target:     agentcore.LaunchTarget{Command: script, WorkspacePath: tmpDir},
		baseLogger: slog.Default(),
	}

	deleteVerificationSession(t.Context(), state, "ses_abc123")

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("ReadFile(env.txt): %v", err)
	}
	if string(got) != "true" {
		t.Errorf("session delete launch OPENCODE_DISABLE_AUTOUPDATE = %q, want %q", got, "true")
	}
}

func writeSSHExit255Script(t *testing.T, dir string, runOutput []byte) string {
	t.Helper()
	exportPath := filepath.Join(dir, "export.json")
	if err := os.WriteFile(exportPath, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(export.json): %v", err)
	}
	runPath := filepath.Join(dir, "run.jsonl")
	if err := os.WriteFile(runPath, runOutput, 0o644); err != nil {
		t.Fatalf("WriteFile(run.jsonl): %v", err)
	}
	body := `case "$1" in
  export) cat '` + exportPath + `'; exit 0;;
esac
cat '` + runPath + `'
exit 255
`
	return agenttest.WriteScript(t, dir, "fake-opencode-ssh255", body)
}

var nonTerminalStepFinishToolCalls = []byte(`{"type":"step_finish","timestamp":1777197446660,"sessionID":"ses_abc123","part":{"id":"prt_003","reason":"tool-calls","messageID":"msg_001","sessionID":"ses_abc123","type":"step-finish"}}` + "\n")

func TestRunTurn_SSHExit255WithoutForkPerTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		runOutput            []byte
		wantConnectionFailed bool
	}{
		{name: "no output wraps connection failed", wantConnectionFailed: true},
		{name: "output with no terminal step_finish still wraps connection failed", runOutput: nonTerminalStepFinishToolCalls, wantConnectionFailed: true},
		{name: "output with a terminal step_finish does not wrap connection failed", runOutput: loadFixture(t, "simple_turn.jsonl")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			script := writeSSHExit255Script(t, tmpDir, tt.runOutput)

			a, _ := NewOpenCodeAdapter(map[string]any{})
			session, err := a.StartSession(t.Context(), domain.StartSessionParams{
				WorkspacePath: tmpDir,
				AgentConfig:   domain.AgentConfig{Command: "opencode"},
				SSHHost:       "example.test",
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			state := session.Internal.(*sessionState)
			state.target.Command = script

			_, result, err := collectEvents(t, a, session, "work")

			if got := errors.Is(err, sshutil.ErrConnectionFailed); got != tt.wantConnectionFailed {
				t.Fatalf("RunTurn() error = %v, wraps sshutil.ErrConnectionFailed = %v, want %v", err, got, tt.wantConnectionFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
				t.Errorf("RunTurn() error kind = %v, want %q", err, domain.ErrPortExit)
			}
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
		})
	}
}
