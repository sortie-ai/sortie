package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

const verificationSessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"

const deleteServerScenario = "copilot.delete-server"

type deleteServerParams struct {
	CapturePath string
}

type deleteServerCapture struct {
	Method    string `json:"method"`
	SessionID string `json:"sessionId"`
}

func init() {
	fakeScenarios[deleteServerScenario] = agenttest.Typed(runDeleteServerScenario)
}

func runDeleteServerScenario(_ []string, params deleteServerParams) int {
	br := bufio.NewReader(os.Stdin)
	length, err := readContentLengthHeader(br)
	if err != nil {
		return 1
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return 1
	}
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 1
	}

	captured, _ := json.Marshal(deleteServerCapture{Method: req.Method, SessionID: req.Params.SessionID})
	_ = os.WriteFile(params.CapturePath, captured, 0o600)

	resp, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"success": true}})
	_ = writeContentLengthFramed(os.Stdout, resp)
	return 0
}

func assertDeletedWithoutWarning(t *testing.T, target agentcore.LaunchTarget, capturePath string) {
	t.Helper()

	spy := &agenttest.LogSpy{}
	deleteVerificationSession(context.Background(), target, verificationSessionID, 5*time.Second, 0, slog.New(spy))

	for _, e := range spy.Entries() {
		if e.Level >= slog.LevelWarn {
			t.Errorf("unexpected %s record %q on a successful deletion", e.Level, e.Msg)
		}
	}
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the fake server observed no request: %v", err)
	}
	var captured deleteServerCapture
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if captured.Method != "session.delete" || captured.SessionID != verificationSessionID {
		t.Errorf("captured request = %+v, want session.delete for %q", captured, verificationSessionID)
	}
}

func TestDeleteVerificationSession_SendsFramedSessionDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	capturePath := dir + "/captured.json"
	bin := agenttest.FakeRuntime(t, dir, "copilot", deleteServerScenario, deleteServerParams{CapturePath: capturePath})

	assertDeletedWithoutWarning(t, agentcore.LaunchTarget{Command: bin, WorkspacePath: dir}, capturePath)
}
