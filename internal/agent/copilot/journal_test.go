package copilot

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func jsonlLines(t *testing.T, name string) [][]byte {
	t.Helper()

	return bytes.Split(bytes.TrimRight([]byte(loadTestFixture(t, "live/"+name)), "\n"), []byte("\n"))
}

func tokens(in, out, cacheRead, cacheWrite int64) domain.TokenUsage {
	return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite}
}

func recordSums(t *testing.T, lines [][]byte) (readings []usage.SpendReading, total domain.TokenUsage, byModel map[string]domain.TokenUsage) {
	t.Helper()

	byModel = map[string]domain.TokenUsage{}
	for _, line := range lines {
		r, ok := sessionstate.UsageRecord(line)
		if !ok {
			continue
		}
		counts, _ := usage.Normalize(r, usageSources.Records.Spend)
		total = usage.Add(total, counts)
		byModel[r.Model] = usage.Add(byModel[r.Model], counts)
		readings = append(readings, r)
	}
	return readings, total, byModel
}

func TestUsageCheckpointSettlesLiveCaptures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, fixture            string
		historyLines, settleLine int
		want                     domain.TokenUsage
		wantByModel              map[string]domain.TokenUsage
	}{
		{name: "fresh one turn", fixture: "fresh_one_turn", settleLine: 49, want: tokens(51251, 32, 24320, 0)},
		{name: "tool call three requests", fixture: "tool_call_three_requests", settleLine: 100, want: tokens(77152, 161, 52224, 0)},
		{name: "resume same model", fixture: "resume_same_model", historyLines: 22, settleLine: 43, want: tokens(51893, 28, 51456, 0)},
		{name: "resume model switch", fixture: "resume_model_switch", historyLines: 30, settleLine: 57, want: tokens(90350, 179, 45073, 45269)},
		{name: "resume after sigkill", fixture: "resume_after_kill_sigkill", historyLines: 14, settleLine: 164, want: tokens(104184, 203, 103296, 0)},
		{
			name: "sub-agent on another model", fixture: "subagent_explore_task", settleLine: 207, want: tokens(85644, 406, 79665, 4344),
			wantByModel: map[string]domain.TokenUsage{"mai-code-1.1-flash": tokens(77277, 194, 75648, 0), "gpt-5.6-luna": tokens(8367, 212, 4017, 4344)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			journal, stdout := jsonlLines(t, tt.fixture+".events.jsonl"), jsonlLines(t, tt.fixture+".stdout.jsonl")
			ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
			if err != nil {
				t.Fatalf("NewLedger(usageSources) = %v, want nil", err)
			}
			if history, _, _ := recordSums(t, journal[:tt.historyLines]); tt.historyLines == 0 {
				ledger.SessionCreated()
			} else {
				ledger.Boundary(usage.RecordsRead{Readings: history, OK: true})
			}
			reading, ok := sessionstate.UsageCheckpoint(stdout[tt.settleLine-1])
			if !ok {
				t.Fatalf("UsageCheckpoint(%s line %d) = false, want a checkpoint", tt.fixture, tt.settleLine)
			}

			got := ledger.Settle(usage.TurnEnd{Replied: true}, &reading, usage.RecordsRead{})

			_, appended, appendedByModel := recordSums(t, journal[tt.historyLines:])
			live := domain.UsageProvenance{Source: usage.SourceOutput, Strategy: usage.StrategyLive}
			if got.Run != tt.want || (tt.wantByModel != nil && !maps.Equal(got.ByModel, tt.wantByModel)) {
				t.Errorf("Settle(%s line %d) Run, ByModel = %+v, %+v, want %+v, %+v", tt.fixture, tt.settleLine, got.Run, got.ByModel, tt.want, tt.wantByModel)
			}
			if got.Run != appended || !maps.Equal(got.ByModel, appendedByModel) || !got.Complete || !slices.Equal(got.Provenance, []domain.UsageProvenance{live}) {
				t.Errorf("Settle(%s line %d) Run, ByModel, Complete, Provenance = %+v, %+v, %t, %+v, want the appended records' sum %+v, %+v, true, %+v",
					tt.fixture, tt.settleLine, got.Run, got.ByModel, got.Complete, got.Provenance, appended, appendedByModel, live)
			}
		})
	}
}

func TestRunTurnKilledMidTurnSettlesFromJournalOrOutput(t *testing.T) {
	sigterm, sigkill := tokens(25557, 69, 0, 0), tokens(25554, 69, 24320, 0)
	fromJournal := domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}
	fromOutput := domain.UsageProvenance{Source: usage.SourceOutput, Strategy: usage.StrategyLive}
	const sigtermSession, sigkillSession = "62decdde-b18b-4ef6-a0d0-6ab45d5a02e4", "a1ca984e-2392-4618-bf00-2a6eac6156fb"
	tests := []struct {
		name, fixture, sessionID string
		journal                  bool
		want                     domain.TokenUsage
		wantSource               []domain.UsageProvenance
	}{
		{name: "sigterm with journal", fixture: "kill_sigterm_midturn", sessionID: sigtermSession, journal: true, want: sigterm, wantSource: []domain.UsageProvenance{fromJournal}},
		{name: "sigterm without journal", fixture: "kill_sigterm_midturn", sessionID: sigtermSession, want: sigterm, wantSource: []domain.UsageProvenance{fromOutput}},
		{name: "sigkill with journal", fixture: "kill_sigkill_midturn", sessionID: sigkillSession, journal: true, want: sigkill, wantSource: []domain.UsageProvenance{fromJournal}},
		{name: "sigkill without journal", fixture: "kill_sigkill_midturn", sessionID: sigkillSession},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("COPILOT_HOME", home)
			t.Setenv("GH_TOKEN", "test-token-for-unit-test")
			if path := filepath.Join(home, "session-state", tt.sessionID, "events.jsonl"); tt.journal {
				if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0o755), os.WriteFile(path, []byte(loadTestFixture(t, "live/"+tt.fixture+".events.jsonl")), 0o600)); err != nil {
					t.Fatalf("writing journal %q: %v", path, err)
				}
			}
			command := agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{
				Stdout:  loadTestFixture(t, "live/"+tt.fixture+".stdout.jsonl"),
				Version: "1.0.105",
			})
			adapter := &CopilotAdapter{}
			session, err := adapter.StartSession(t.Context(), domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: domain.AgentConfig{Command: command}})
			if err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			session.Internal.(*sessionState).copilotSessionID = tt.sessionID

			got, _ := adapter.RunTurn(t.Context(), session, domain.RunTurnParams{Prompt: "kill", OnEvent: func(domain.AgentEvent) {}})

			if got.Usage != tt.want || got.UsageMeasured != (tt.want != domain.TokenUsage{}) || !slices.Equal(got.UsageProvenance, tt.wantSource) || !got.SpendUnaccounted {
				t.Errorf("RunTurn(%s) Usage, UsageMeasured, UsageProvenance, SpendUnaccounted = %+v, %t, %+v, %t, want %+v, measured only for a non-zero figure, %+v, true",
					tt.fixture, got.Usage, got.UsageMeasured, got.UsageProvenance, got.SpendUnaccounted, tt.want, tt.wantSource)
			}
		})
	}
}
