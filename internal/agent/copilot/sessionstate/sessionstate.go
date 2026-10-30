// Package sessionstate holds what the Copilot CLI session-state journal
// shares between its readers: where the journal lives, how much of it a
// reader may take, the records declaration of its per-request usage lines, how
// one of those lines maps to a spend reading, and how a usage checkpoint from
// the event stream maps to a running reading. The copilot-cli kind and the
// Copilot usage declaration on the agent-client-protocol kind import it.
//
// The package registers no kind and holds no adapter.
package sessionstate

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	// HomeVar names the environment variable that relocates the runtime home.
	HomeVar = "COPILOT_HOME"

	// DefaultHome is the runtime home under the user's home directory when
	// HomeVar is unset or empty.
	DefaultHome = ".copilot"

	// MaxFileBytes is the largest journal a reader takes, the limit every
	// records reader applies. Larger files are rejected without being read.
	MaxFileBytes = usage.MaxRecordsBytes
)

const (
	usageRecordType     = "session.usage_record"
	usageCheckpointType = "session.usage_checkpoint"
)

// Records declares the journal's per-request usage lines as a records source.
// The runtime does not write the journal as soon as a request completes, so a
// kill, or a cancel that ends the process, can land before the file or a
// request's line exists and leave the turn's amount unknown.
var Records = usage.Source{
	Category: usage.SourceJournal,
	Strategy: usage.StrategyRecords,
	Spend: usage.SpendDeclaration{
		Shape:             usage.SpendPerRequest,
		CacheInInput:      true,
		ReasoningInOutput: true,
		ServedModel:       true,
	},
	WrittenAtTurnEnd: true,
}

// JournalPath returns the path of the session's journal under the runtime
// home. It does not validate the id: the read refuses one that is not a safe
// path segment.
func JournalPath(sessionID string) agentcore.RuntimePath {
	return agentcore.RuntimePath{
		HomeVar:     HomeVar,
		HomeDefault: []string{DefaultHome},
		Segments:    []string{"session-state", sessionID, "events.jsonl"},
	}
}

type journalLine struct {
	Type string `json:"type"`
	Data struct {
		Usage struct {
			Model            string          `json:"model"`
			InputTokens      json.RawMessage `json:"inputTokens"`
			OutputTokens     json.RawMessage `json:"outputTokens"`
			CacheReadTokens  json.RawMessage `json:"cacheReadTokens"`
			CacheWriteTokens json.RawMessage `json:"cacheWriteTokens"`
			Accounting       struct {
				UsageID string `json:"usageId"`
			} `json:"accounting"`
		} `json:"usage"`
	} `json:"data"`
}

// UsageRecord maps one journal line to the spend reading of a single model
// request. It reports false for a line that is not valid JSON and for every
// type but the per-request usage record; checkpoint and shutdown lines carry
// session totals and never yield a per-request reading. A count that is absent or not a
// JSON integer is nil. Model is the record's own, verbatim.
func UsageRecord(line []byte) (usage.SpendReading, bool) {
	var decoded journalLine
	if err := json.Unmarshal(line, &decoded); err != nil || decoded.Type != usageRecordType {
		return usage.SpendReading{}, false
	}
	u := decoded.Data.Usage
	return usage.SpendReading{
		Input:      count(u.InputTokens),
		Output:     count(u.OutputTokens),
		CacheRead:  count(u.CacheReadTokens),
		CacheWrite: count(u.CacheWriteTokens),
		Model:      u.Model,
		Identity:   u.Accounting.UsageID,
	}, true
}

func count(raw json.RawMessage) *int64 {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// checkpointLine omits the cost, request and nano-AIU members on purpose: the
// runtime writes them as floats, and a typed integer field would reject the
// whole line.
type checkpointLine struct {
	Type string `json:"type"`
	Data struct {
		AccountingSnapshot struct {
			ModelMetrics map[string]struct {
				Usage struct {
					InputTokens      json.RawMessage `json:"inputTokens"`
					OutputTokens     json.RawMessage `json:"outputTokens"`
					CacheReadTokens  json.RawMessage `json:"cacheReadTokens"`
					CacheWriteTokens json.RawMessage `json:"cacheWriteTokens"`
				} `json:"usage"`
			} `json:"modelMetrics"`
		} `json:"accountingSnapshot"`
	} `json:"data"`
}

// UsageCheckpoint maps one event-stream line to the running spend reading of
// a usage checkpoint: one entry per served model, sorted by model name, each
// holding that model's total over the session. It reports false for a line
// that is not valid JSON and for every type but the checkpoint. A count that
// is absent or not a JSON integer is nil, and a checkpoint without models
// yields a reading without entries.
func UsageCheckpoint(line []byte) (usage.SpendReading, bool) {
	var decoded checkpointLine
	if err := json.Unmarshal(line, &decoded); err != nil || decoded.Type != usageCheckpointType {
		return usage.SpendReading{}, false
	}
	metrics := decoded.Data.AccountingSnapshot.ModelMetrics
	entries := make([]usage.SpendReading, 0, len(metrics))
	for _, model := range slices.Sorted(maps.Keys(metrics)) {
		u := metrics[model].Usage
		entries = append(entries, usage.SpendReading{
			Input:      count(u.InputTokens),
			Output:     count(u.OutputTokens),
			CacheRead:  count(u.CacheReadTokens),
			CacheWrite: count(u.CacheWriteTokens),
			Model:      model,
		})
	}
	return usage.SpendReading{ByModel: entries}, true
}

// ScanRecords returns the journal's per-request usage readings in file order.
// The caller supplies the scan that reaches the runtime's host, so a local and
// a remote launch share this read. It returns the scan's error unchanged, with
// the readings gathered before it; a caller ignores them on error.
func ScanRecords(ctx context.Context, sessionID string,
	scan func(ctx context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error,
) ([]usage.SpendReading, error) {
	var readings []usage.SpendReading
	err := scan(ctx, JournalPath(sessionID), MaxFileBytes, usage.MaxRecordLineBytes, func(line []byte) error {
		if reading, ok := UsageRecord(line); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	return readings, err
}
