package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func effortTurnFixture(turns int) []byte {
	var b strings.Builder
	for turn := 1; turn <= turns; turn++ {
		id := fmt.Sprintf("turn-%03d", turn)
		fmt.Fprintf(&b, `{"id":%d,"result":{"turn":{"id":%q,"status":"starting"}}}`+"\n", turn, id)
		fmt.Fprintf(&b, `{"method":"turn/started","params":{"turnId":%q}}`+"\n", id)
		fmt.Fprintf(&b, `{"method":"turn/completed","params":{"turn":{"id":%q,"status":"completed"}}}`+"\n", id)
	}
	return []byte(b.String())
}

func turnStartEfforts(t *testing.T, recorder *capturingWriteCloser) []string {
	t.Helper()

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	var efforts []string
	for _, write := range recorder.writes {
		var message struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(write), &message); err != nil {
			t.Fatalf("decode write %q: %v", write, err)
		}
		if message.Method != "turn/start" {
			continue
		}
		effort, _ := message.Params[registry.EffortKey].(string)
		efforts = append(efforts, effort)
	}
	return efforts
}

func codexEffortProbe(t *testing.T, passthrough map[string]any, verification bool, turns int) ([]string, error) {
	t.Helper()

	adapter, err := NewCodexAdapter(passthrough)
	if err != nil {
		return nil, err
	}
	recorder := &capturingWriteCloser{}
	state := makeTestStateWithStdin(t, effortTurnFixture(turns), recorder)
	state.credentialVerification = verification
	session := fakeSession(state)

	for turn := 1; turn <= turns; turn++ {
		if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}}); err != nil {
			return nil, fmt.Errorf("RunTurn(%d): %w", turn, err)
		}
	}
	return turnStartEfforts(t, recorder), nil
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, codexEffortProbe)
}
