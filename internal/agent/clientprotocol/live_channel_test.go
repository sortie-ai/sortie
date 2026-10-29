package clientprotocol

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

func optInCapture(t *testing.T) (info *implementation, updates []string) {
	t.Helper()
	data, _ := os.ReadFile("testdata/pulled_windows/optin_capture.jsonl")
	for line := range bytes.Lines(data) {
		var capture struct{ Msg json.RawMessage }
		var message struct {
			Method string
			Result struct{ AgentInfo *implementation }
			Params struct{ SessionID string }
		}
		if json.Unmarshal(line, &capture) != nil || json.Unmarshal(capture.Msg, &message) != nil {
			t.Fatalf("capture line %s is not a recorded message", line)
		}
		if message.Result.AgentInfo != nil {
			info = message.Result.AgentInfo
		}
		if message.Method == methodSessionUpdate {
			updates = append(updates, string(bytes.ReplaceAll(capture.Msg, []byte(message.Params.SessionID), []byte("sess-test"))))
		}
	}
	return info, updates
}

func TestLiveChannelOfPullOnlyCaptureCarriesNoWindow(t *testing.T) {
	t.Parallel()

	info, updates := optInCapture(t)
	if info == nil || len(updates) == 0 {
		t.Fatalf("capture holds agentInfo %v and %d session updates, want both", info, len(updates))
	}

	got := windowEvents(eventsOfTurnUnder(t, declarationFor(info, agentcore.LaunchTarget{}), updates...))

	if len(got) != 0 {
		t.Errorf("windows of %d captured session updates under agent %q = %v, want none", len(updates), info.Name, got)
	}
}
