//go:build unix

package clientprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const scenarioReplayCapture = "clientprotocol.replay-capture"

func init() { fakeRuntimeScenarios[scenarioReplayCapture] = agenttest.Typed(runReplayCapture) }

type replayFrame struct {
	Agent bool
	Msg   json.RawMessage
}

type replayParams struct {
	Launch []string
	Frames []replayFrame
	Export string
}

type replayEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Type               string `json:"type"`
		ClientCapabilities struct {
			Meta map[string]struct{ Events []string } `json:"_meta"`
		} `json:"clientCapabilities"`
	} `json:"params"`
}

func decodeEnvelope(msg json.RawMessage) replayEnvelope {
	var e replayEnvelope
	_ = json.Unmarshal(msg, &e)
	return e
}

func runReplayCapture(args []string, p replayParams) int {
	if !slices.Equal(args, p.Launch) {
		if p.Export != "" && slices.Contains(args, "export") {
			fmt.Print(p.Export)
			return 0
		}
		return 3
	}

	var subscribed []string
	next := 0
	for in := json.NewDecoder(os.Stdin); ; {
		var raw json.RawMessage
		if in.Decode(&raw) != nil {
			return 0
		}
		req := decodeEnvelope(raw)
		for _, meta := range req.Params.ClientCapabilities.Meta {
			subscribed = append(subscribed, meta.Events...)
		}
		if len(req.ID) == 0 {
			continue
		}
		var out []json.RawMessage
		out, next = replayAnswer(p.Frames, next, req, subscribed)
		for _, line := range out {
			fmt.Printf("%s\n", line)
		}
	}
}

// replayAnswer returns the agent frames the capture sent after the request of
// req's method, with the response carrying req's id. A notification typed by
// params.type is withheld unless the client subscribed to that type, as a
// runtime does.
func replayAnswer(frames []replayFrame, from int, req replayEnvelope, subscribed []string) ([]json.RawMessage, int) {
	at := from
	for at < len(frames) && (frames[at].Agent || decodeEnvelope(frames[at].Msg).Method != req.Method) {
		at++
	}
	if at == len(frames) {
		return []json.RawMessage{fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)}, from
	}

	var out []json.RawMessage
	end := at + 1
	for ; end < len(frames) && frames[end].Agent; end++ {
		e := decodeEnvelope(frames[end].Msg)
		switch {
		case e.Method != "" && e.Params.Type != "" && !slices.Contains(subscribed, e.Params.Type):
		case e.Method != "":
			out = append(out, frames[end].Msg)
		default:
			var members map[string]json.RawMessage
			_ = json.Unmarshal(frames[end].Msg, &members)
			members["id"] = req.ID
			response, _ := json.Marshal(members)
			out = append(out, response)
		}
	}
	return out, end
}

type replayCase struct {
	Name, Capture, Export, Command, WantKind string
	Edits                                    []struct{ Frame, Old, New string }
	WantResetsAtMs                           int64
	WantModelOnly                            bool
}

func loadReplayCases(t *testing.T) (positive, negative []replayCase) {
	t.Helper()

	var manifest struct{ Positive, Negative []replayCase }
	if err := json.Unmarshal(waittest.File(t, "testdata/usage_limit/replay_cases.json"), &manifest); err != nil {
		t.Fatalf("replay cases: %v", err)
	}
	return manifest.Positive, manifest.Negative
}

func replayStart(t *testing.T, c replayCase) domain.StartSessionParams {
	t.Helper()

	words := strings.Fields(c.Command)
	params := replayParams{Launch: words[1:]}
	for line := range bytes.Lines(waittest.File(t, "testdata/usage_limit/"+c.Capture)) {
		var record struct {
			Dir string
			Msg json.RawMessage
			Raw string
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("capture %s: %v", c.Capture, err)
		}
		if record.Raw != "" {
			record.Msg = json.RawMessage(record.Raw)
		}
		params.Frames = append(params.Frames, replayFrame{Agent: slices.Contains([]string{"in", "agent->client", "a->c"}, record.Dir), Msg: record.Msg})
	}
	for _, edit := range c.Edits {
		edited := 0
		for i, f := range params.Frames {
			if f.Agent && bytes.Contains(f.Msg, []byte(edit.Frame)) && bytes.Contains(f.Msg, []byte(edit.Old)) {
				params.Frames[i].Msg = bytes.ReplaceAll(f.Msg, []byte(edit.Old), []byte(edit.New))
				edited++
			}
		}
		if edited == 0 {
			t.Fatalf("capture %s: edit %q matched no agent frame", c.Capture, edit.Old)
		}
	}
	if c.Export != "" {
		params.Export = string(waittest.File(t, "testdata/usage_limit/"+c.Export))
	}

	bin := agenttest.FakeRuntime(t, t.TempDir(), words[0], scenarioReplayCapture, params)
	return domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{
			Command:       strings.Join(append([]string{bin}, params.Launch...), " "),
			ReadTimeoutMS: 5000,
			TurnTimeoutMS: int(waittest.HangGuard.Milliseconds()),
		},
	}
}

func TestUsageLimitReplay(t *testing.T) {
	t.Parallel()

	adapter, err := NewClientProtocolAdapter()
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}
	positive, _ := loadReplayCases(t)
	cases := make([]agenttest.UsageLimitRefusalCase, len(positive))
	for i, c := range positive {
		cases[i] = agenttest.UsageLimitRefusalCase{Name: c.Name, Adapter: adapter, Start: replayStart(t, c), Turn: domain.RunTurnParams{Prompt: "go"}, WantModelOnly: c.WantModelOnly}
		if c.WantResetsAtMs != 0 {
			cases[i].WantResetsAt = time.UnixMilli(c.WantResetsAtMs).UTC()
		}
	}

	agenttest.AssertUsageLimitRefusal(t, "agent-client-protocol", cases)
}

func TestUsageLimitNotRecognized(t *testing.T) {
	t.Parallel()

	_, negative := loadReplayCases(t)
	for _, c := range negative {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			adapter, err := NewClientProtocolAdapter()
			if err != nil {
				t.Fatalf("NewClientProtocolAdapter() error = %v", err)
			}
			ctx := waittest.CleanupContext(t)
			session, err := adapter.StartSession(ctx, replayStart(t, c))
			if err != nil {
				t.Fatalf("StartSession(%s) error = %v", c.Capture, err)
			}
			t.Cleanup(func() { _ = adapter.StopSession(ctx, session) })

			_, err = adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

			var agentErr *domain.AgentError
			if c.WantKind == "" && err != nil || c.WantKind != "" && (!errors.As(err, &agentErr) || agentErr.Kind != domain.AgentErrorKind(c.WantKind)) {
				t.Errorf("RunTurn(%s) error = %v, want an error of kind %q", c.Name, err, c.WantKind)
			}
		})
	}
}
