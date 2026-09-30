package claude

import (
	"context"
	"log/slog"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	unknownEffortLine = `Warning: Unknown --effort value "bogus-level", using the model default`
	reportMessage     = "reasoning level not applied by the agent"

	successStdout = `{"type":"system","subtype":"init","session_id":"report-session","cwd":"/tmp"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]},"session_id":"report-session"}
{"type":"result","subtype":"success","result":"All done.","is_error":false,"session_id":"report-session"}
`
	failureStdout = `{"type":"system","subtype":"init","session_id":"report-session","cwd":"/tmp"}
{"type":"result","subtype":"error","result":"model refused the request","is_error":true,"session_id":"report-session"}
`
)

func effortReports(t *testing.T, passthrough map[string]any, verification bool, turns int, out agenttest.Output) []agenttest.LogSpyEntry {
	t.Helper()

	dir := t.TempDir()
	command := agenttest.FakeRuntime(t, dir, "fake-claude", agenttest.OutputScenario, out)
	adapter, err := NewClaudeCodeAdapter(passthrough)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter(%v) error = %v", passthrough, err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          dir,
		AgentConfig:            domain.AgentConfig{Command: command},
		CredentialVerification: verification,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	spy := &agenttest.LogSpy{}
	session.Internal.(*sessionState).baseLogger = slog.New(spy)

	for range turns {
		_, _ = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}})
	}

	var reports []agenttest.LogSpyEntry
	for _, entry := range spy.Entries() {
		if entry.Msg == reportMessage {
			reports = append(reports, entry)
		}
	}
	return reports
}

func TestEffortReport(t *testing.T) {
	t.Parallel()

	withEffort := map[string]any{registry.EffortKey: "bogus-level"}
	stderrWithReport := "runtime chatter\n" + unknownEffortLine + "\n"

	tests := []struct {
		name         string
		passthrough  map[string]any
		verification bool
		turns        int
		out          agenttest.Output
		wantReports  int
	}{
		{
			name:        "two successful turns warn once",
			passthrough: withEffort,
			turns:       2,
			out:         agenttest.Output{Stdout: successStdout, Stderr: stderrWithReport},
			wantReports: 1,
		},
		{
			name:         "credential verification session warns once",
			passthrough:  withEffort,
			verification: true,
			turns:        1,
			out:          agenttest.Output{Stdout: successStdout, Stderr: stderrWithReport},
			wantReports:  1,
		},
		{
			name:        "no report line yields no warning",
			passthrough: withEffort,
			turns:       2,
			out:         agenttest.Output{Stdout: successStdout, Stderr: "runtime chatter\n"},
		},
		{
			name:        "failing turn yields no warning",
			passthrough: withEffort,
			turns:       1,
			out:         agenttest.Output{Stdout: failureStdout, Stderr: stderrWithReport},
		},
		{
			name:        "unset effort yields no warning",
			passthrough: map[string]any{},
			turns:       1,
			out:         agenttest.Output{Stdout: successStdout, Stderr: stderrWithReport},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reports := effortReports(t, tt.passthrough, tt.verification, tt.turns, tt.out)

			if len(reports) != tt.wantReports {
				t.Fatalf("reasoning-level warnings = %d, want %d", len(reports), tt.wantReports)
			}
			for _, report := range reports {
				if report.Level != slog.LevelWarn {
					t.Errorf("report level = %v, want %v", report.Level, slog.LevelWarn)
				}
				if got := report.Attrs["agent_kind"]; got != "claude-code" {
					t.Errorf("report agent_kind = %q, want %q", got, "claude-code")
				}
				if got := report.Attrs[registry.EffortKey]; got != "bogus-level" {
					t.Errorf("report %s = %q, want %q", registry.EffortKey, got, "bogus-level")
				}
				if report.Line != unknownEffortLine {
					t.Errorf("report line = %q, want %q", report.Line, unknownEffortLine)
				}
			}
		})
	}
}
