package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

type recordedFrame struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type recordingSession struct {
	session    domain.Session
	state      *sessionState
	recordPath string
}

func (r *recordingSession) recorded(t *testing.T, method string) []recordedFrame {
	t.Helper()

	data, err := os.ReadFile(r.recordPath)
	if err != nil {
		t.Fatalf("read recorded frames: %v", err)
	}
	var frames []recordedFrame
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var frame recordedFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("decode recorded frame %q: %v", line, err)
		}
		if frame.Method == method {
			frames = append(frames, frame)
		}
	}
	return frames
}

func startRecordingSessionE(t *testing.T, adapter *CodexAdapter, settings map[string]any, resumeID string, verification bool) (*recordingSession, error) {
	t.Helper()

	recordPath := filepath.Join(t.TempDir(), "frames")
	command := agenttest.FakeRuntime(t, t.TempDir(), "codex", scenarioRecordFrames, recordFramesParams{RecordPath: recordPath})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            domain.AgentConfig{Command: command},
		ResumeSessionID:        resumeID,
		CredentialVerification: verification,
		Settings:               settings,
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })
	return &recordingSession{session: session, state: session.Internal.(*sessionState), recordPath: recordPath}, nil
}

func startRecordingSession(t *testing.T, adapter *CodexAdapter, settings map[string]any, resumeID string) *recordingSession {
	t.Helper()

	session, err := startRecordingSessionE(t, adapter, settings, resumeID, false)
	if err != nil {
		t.Fatalf("StartSession(Settings=%v) error = %v", settings, err)
	}
	return session
}

func runRecordedTurns(t *testing.T, adapter *CodexAdapter, r *recordingSession, turns int) {
	t.Helper()

	for turn := 1; turn <= turns; turn++ {
		if _, err := adapter.RunTurn(context.Background(), r.session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}}); err != nil {
			t.Fatalf("RunTurn(%d) error = %v", turn, err)
		}
	}
}

func codexEffortProbe(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
	t.Helper()

	resumeID := ""
	if resumed {
		resumeID = "thread-prior"
	}
	adapter := &CodexAdapter{}
	session, err := startRecordingSessionE(t, adapter, settings, resumeID, verification)
	if err != nil {
		return nil, fmt.Errorf("StartSession: %w", err)
	}
	runRecordedTurns(t, adapter, session, turns)

	var efforts []string
	for _, frame := range session.recorded(t, "turn/start") {
		effort, _ := frame.Params[registry.EffortKey].(string)
		efforts = append(efforts, effort)
	}
	return efforts, nil
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, codexEffortProbe)
}
