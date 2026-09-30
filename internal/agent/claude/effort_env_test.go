package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const inheritedEffortLevel = "low"

type errorReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

type failureCounter struct{ failures int }

func (*failureCounter) Helper() {}

func (f *failureCounter) Errorf(string, ...any) { f.failures++ }

func awaitRecordedEnv(t *testing.T, path string) map[string]string {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var values map[string]string
			if json.Unmarshal(data, &values) == nil {
				return values
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no environment recording appeared at %s within 60s", path)
	return nil
}

func recordedEffortEnv(t *testing.T, passthrough map[string]any, verification bool, mutate func(*sessionState)) string {
	t.Helper()

	dir := t.TempDir()
	recording := filepath.Join(dir, "recorded.json")
	command := agenttest.FakeRuntime(t, dir, "fake-claude", agenttest.RecordedEnvScenario, agenttest.RecordedEnv{
		Path:  recording,
		Names: []string{effortEnvName},
	})
	adapter, err := NewClaudeCodeAdapter(passthrough)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter(%v) error = %v", passthrough, err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            domain.AgentConfig{Command: command},
		CredentialVerification: verification,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if mutate != nil {
		mutate(session.Internal.(*sessionState))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		_, _ = adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}})
	}()

	return awaitRecordedEnv(t, recording)[effortEnvName]
}

func assertEffortEnvWithheld(t errorReporter, got string) {
	t.Helper()

	if got != "" {
		t.Errorf("launch environment %s = %q, want it withheld", effortEnvName, got)
	}
}

func TestEffortEnv(t *testing.T) {
	// Not parallel: t.Setenv carries the inherited variable.
	t.Setenv(effortEnvName, inheritedEffortLevel)

	tests := []struct {
		name         string
		passthrough  map[string]any
		verification bool
		want         string
	}{
		{name: "set effort withholds the variable on a working launch", passthrough: map[string]any{registry.EffortKey: "high"}, want: ""},
		{name: "set effort withholds the variable on a credential verification launch", passthrough: map[string]any{registry.EffortKey: "high"}, verification: true, want: ""},
		{name: "unset effort leaves the inherited variable on a working launch", passthrough: map[string]any{}, want: inheritedEffortLevel},
		{name: "unset effort leaves the inherited variable on a credential verification launch", passthrough: map[string]any{}, verification: true, want: inheritedEffortLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recordedEffortEnv(t, tt.passthrough, tt.verification, nil)

			if tt.want == "" {
				assertEffortEnvWithheld(t, got)
				return
			}
			if got != tt.want {
				t.Errorf("recorded %s = %q, want %q", effortEnvName, got, tt.want)
			}
		})
	}
}

func TestEffortEnv_EmptyWithheldEnvFailsTheLocalArm(t *testing.T) {
	// Not parallel: t.Setenv carries the inherited variable.
	t.Setenv(effortEnvName, inheritedEffortLevel)

	got := recordedEffortEnv(t, map[string]any{registry.EffortKey: "high"}, false, func(state *sessionState) {
		state.target.WithheldEnv = nil
	})

	var reporter failureCounter
	assertEffortEnvWithheld(&reporter, got)
	if reporter.failures != 1 {
		t.Errorf("assertEffortEnvWithheld(%q) reported %d failures, want 1", got, reporter.failures)
	}
}
