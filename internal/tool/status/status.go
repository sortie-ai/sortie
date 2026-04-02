// Package status implements [domain.AgentTool] for the sortie_status tool,
// which returns live session runtime metadata to agents: current turn number,
// remaining turns, session duration, and cumulative token usage. It reads
// from the .sortie/state.json file written by the worker goroutine inside
// the session workspace.
package status

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.AgentTool = (*StatusTool)(nil)

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

type stateFile struct {
	TurnNumber      int    `json:"turn_number"`
	MaxTurns        int    `json:"max_turns"`
	Attempt         *int   `json:"attempt"`
	StartedAt       string `json:"started_at"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
}

type statusResponse struct {
	TurnNumber             int     `json:"turn_number"`
	MaxTurns               int     `json:"max_turns"`
	TurnsRemaining         int     `json:"turns_remaining"`
	Attempt                *int    `json:"attempt"`
	SessionDurationSeconds float64 `json:"session_duration_seconds"`
	Tokens                 tokens  `json:"tokens"`
}

type tokens struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

// StatusTool implements [domain.AgentTool] for the sortie_status tool.
// Construct via [New]; it is safe for concurrent use after construction.
type StatusTool struct {
	stateFilePath string
}

// New returns a [StatusTool] that reads session state from the
// .sortie/state.json file inside workspacePath.
//
// workspacePath must be an absolute path to the session workspace
// directory. New panics if workspacePath is empty (programming error).
func New(workspacePath string) *StatusTool {
	if workspacePath == "" {
		panic("status.New: workspacePath must not be empty")
	}
	return &StatusTool{
		stateFilePath: filepath.Join(workspacePath, ".sortie", "state.json"),
	}
}

// Name returns "sortie_status".
func (t *StatusTool) Name() string { return "sortie_status" }

// Description returns a human-readable summary of the tool.
func (t *StatusTool) Description() string {
	return "Returns live session runtime metadata: current turn number, " +
		"remaining turns, session duration, and cumulative token usage."
}

// InputSchema returns the JSON Schema for sortie_status input.
// The tool accepts no parameters; the schema is an empty object.
// The returned slice is a defensive copy.
func (t *StatusTool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

// Execute reads the worker state file and returns the current session
// metadata as a JSON object.
//
// Execute returns a tool-error response when the state file is absent,
// unreadable, or contains invalid JSON. The Go error return is non-nil
// only for internal marshal failures.
func (t *StatusTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	data, err := os.ReadFile(t.stateFilePath)
	if err != nil {
		return errorResponse("state file unavailable: " + err.Error())
	}

	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return errorResponse("state file malformed: " + err.Error())
	}

	startedAt, err := time.Parse(time.RFC3339Nano, sf.StartedAt)
	if err != nil {
		return errorResponse("state file has invalid started_at: " + err.Error())
	}

	turnsRemaining := sf.MaxTurns - sf.TurnNumber
	if turnsRemaining < 0 {
		turnsRemaining = 0
	}

	durationSeconds := math.Round(time.Since(startedAt).Seconds()*1000) / 1000

	resp := statusResponse{
		TurnNumber:             sf.TurnNumber,
		MaxTurns:               sf.MaxTurns,
		TurnsRemaining:         turnsRemaining,
		Attempt:                sf.Attempt,
		SessionDurationSeconds: durationSeconds,
		Tokens: tokens{
			InputTokens:     sf.InputTokens,
			OutputTokens:    sf.OutputTokens,
			TotalTokens:     sf.TotalTokens,
			CacheReadTokens: sf.CacheReadTokens,
		},
	}

	return json.Marshal(resp)
}

func errorResponse(msg string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"error": msg})
}
