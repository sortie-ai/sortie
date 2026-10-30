package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// rolloutLine is one line of a rollout file. The payload stays raw so a line
// of any other type is never decoded.
type rolloutLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type turnContextPayload struct {
	Model string `json:"model"`
}

type tokenUsageRecordPayload struct {
	ResponseID string `json:"response_id"`
	Usage      struct {
		InputTokens           *int64 `json:"input_tokens"`
		CachedInputTokens     *int64 `json:"cached_input_tokens"`
		CacheWriteInputTokens *int64 `json:"cache_write_input_tokens"`
		OutputTokens          *int64 `json:"output_tokens"`
	} `json:"usage"`
}

// rolloutPath locates the rollout file a thread operation reported under the
// runtime's home. Only the part below the home's sessions directory is kept,
// so the reported path cannot lead a read outside the home. It returns nil
// when the path does not end in sessions plus four elements.
func rolloutPath(reported string) *agentcore.RuntimePath {
	elements := strings.FieldsFunc(reported, func(r rune) bool { return r == '/' || r == '\\' })
	if len(elements) < 5 || elements[len(elements)-5] != "sessions" {
		return nil
	}
	return &agentcore.RuntimePath{
		HomeVar:     "CODEX_HOME",
		HomeDefault: []string{".codex"},
		Segments:    append([]string{"sessions"}, elements[len(elements)-4:]...),
	}
}

// readRollout reads the thread's rollout file on the session's host. Any
// failure, and a session with no usable rollout path, returns the zero
// [usage.RecordsRead]. A missing file is logged only when logMissing holds.
func (state *sessionState) readRollout(ctx context.Context, logger *slog.Logger, logMissing bool) usage.RecordsRead {
	if state.rollout == nil {
		return usage.RecordsRead{}
	}
	name := state.rollout.Segments[len(state.rollout.Segments)-1]
	var readings []usage.SpendReading
	var model string
	ordinal := 0
	err := state.target.ScanRuntimeFile(ctx, nil, *state.rollout, usage.MaxRecordsBytes, usage.MaxRecordLineBytes, func(line []byte) error {
		ordinal++
		if reading, ok := rolloutReading(line, name+":"+strconv.Itoa(ordinal), &model); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	if err != nil {
		if logMissing || !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("rollout read failed", slog.Any("error", err))
		}
		return usage.RecordsRead{}
	}
	return usage.RecordsRead{Readings: readings, OK: true}
}

// rolloutReading maps one rollout line to a reading and reports whether the
// line is one. A turn_context line updates *model for the records that follow
// it and is not itself a reading. A line that does not decode, or a record
// that cannot be identified, yields an unknown reading under lineID. The
// event_msg token_count lines mirror the live notification and are skipped,
// since reading both would count each request twice.
func rolloutReading(line []byte, lineID string, model *string) (usage.SpendReading, bool) {
	var parsed rolloutLine
	if err := json.Unmarshal(line, &parsed); err != nil {
		return usage.SpendReading{Identity: lineID}, true
	}
	switch parsed.Type {
	case "turn_context":
		var turn turnContextPayload
		if json.Unmarshal(parsed.Payload, &turn) == nil && strings.TrimSpace(turn.Model) != "" {
			*model = turn.Model
		}
		return usage.SpendReading{}, false
	case "token_usage_record":
		var record tokenUsageRecordPayload
		if json.Unmarshal(parsed.Payload, &record) != nil || record.ResponseID == "" {
			return usage.SpendReading{Identity: lineID}, true
		}
		return usage.SpendReading{
			Input:      record.Usage.InputTokens,
			Output:     record.Usage.OutputTokens,
			CacheRead:  record.Usage.CachedInputTokens,
			CacheWrite: record.Usage.CacheWriteInputTokens,
			Model:      *model,
			Identity:   record.ResponseID,
		}, true
	}
	return usage.SpendReading{}, false
}
