package usagesource

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
	// CopilotHomeVar names the environment variable that relocates the runtime home.
	CopilotHomeVar = "COPILOT_HOME"

	// CopilotDefaultHome is the runtime home under the user's home directory when
	// CopilotHomeVar is unset or empty.
	CopilotDefaultHome = ".copilot"

	// CopilotMaxFileBytes is the largest journal a reader takes, the limit every
	// records reader applies. Larger files are rejected without being read.
	CopilotMaxFileBytes = usage.MaxRecordsBytes
)

const (
	// CopilotUnitPremiumRequests is the unit of the premium requests a request or a
	// session spent.
	CopilotUnitPremiumRequests = "premium requests"

	// CopilotUnitAICredits is the unit of the AI credits a request or a session
	// spent. The runtime counts them in billionths; readers record whole
	// credits.
	CopilotUnitAICredits = "AI credits"
)

const (
	usageRecordType     = "session.usage_record"
	usageCheckpointType = "session.usage_checkpoint"

	nanoPerCredit = 1e9

	initiatorUser     = "user"
	initiatorAgent    = "agent"
	initiatorSubAgent = "sub-agent"
)

// CopilotRecords declares the journal's per-request usage lines as a records source.
// The runtime does not write the journal as soon as a request completes, so a
// kill, or a cancel that ends the process, can land before the file or a
// request's line exists and leave the turn's amount unknown.
var CopilotRecords = usage.Source{
	Category: usage.SourceJournal,
	Strategy: usage.StrategyRecords,
	Spend: usage.SpendDeclaration{
		Shape:             usage.SpendPerRequest,
		CacheInInput:      true,
		ReasoningInOutput: true,
		ServedModel:       true,
		Amounts: []usage.AmountDeclaration{
			{Unit: CopilotUnitPremiumRequests, Shape: usage.AmountPerRequest},
			{Unit: CopilotUnitAICredits, Shape: usage.AmountPerRequest},
		},
	},
	WrittenAtTurnEnd: true,
}

// CopilotJournalPath returns the path of the session's journal under the runtime
// home. It does not validate the id: the read refuses one that is not a safe
// path segment.
func CopilotJournalPath(sessionID string) agentcore.RuntimePath {
	return agentcore.RuntimePath{
		HomeVar:     CopilotHomeVar,
		HomeDefault: []string{CopilotDefaultHome},
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
			Initiator        string          `json:"initiator"`
			Cost             json.RawMessage `json:"cost"`
			Accounting       struct {
				UsageID string `json:"usageId"`
			} `json:"accounting"`
			CopilotUsage struct {
				TotalNanoAiu json.RawMessage `json:"totalNanoAiu"`
			} `json:"copilotUsage"`
		} `json:"usage"`
	} `json:"data"`
}

// CopilotUsageRecord maps one journal line to the spend reading of a single model
// request. It reports false for a line that is not valid JSON and for every
// type but the per-request usage record; checkpoint and shutdown lines carry
// session totals and never yield a per-request reading. A count that is absent or not a
// JSON integer is nil. Model is the record's own, verbatim.
//
// The reading carries the request's premium requests and AI credits. The cost
// member is the multiplier the runtime applies to every call, so it counts
// only for a request the user initiated; an agent or sub-agent request
// charges none, and an absent or other initiator leaves the premium requests
// unknown.
func CopilotUsageRecord(line []byte) (usage.SpendReading, bool) {
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
		Amounts: []usage.AmountReading{
			{Unit: CopilotUnitPremiumRequests, Amount: requestPremiumRequests(u.Initiator, u.Cost)},
			{Unit: CopilotUnitAICredits, Amount: credits(u.CopilotUsage.TotalNanoAiu)},
		},
	}, true
}

func requestPremiumRequests(initiator string, cost json.RawMessage) *float64 {
	switch initiator {
	case initiatorUser:
		return number(cost)
	case initiatorAgent, initiatorSubAgent:
		none := 0.0
		return &none
	default:
		return nil
	}
}

// number returns the non-negative JSON number a member holds, nil for an
// absent, null, non-numeric, out-of-range, or negative member.
func number(raw json.RawMessage) *float64 {
	var n float64
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 {
		return nil
	}
	return &n
}

func credits(nano json.RawMessage) *float64 {
	n := number(nano)
	if n == nil {
		return nil
	}
	whole := *n / nanoPerCredit
	return &whole
}

func count(raw json.RawMessage) *int64 {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

type checkpointLine struct {
	Type string `json:"type"`
	Data struct {
		AccountingSnapshot struct {
			ModelMetrics map[string]struct {
				Requests struct {
					Cost json.RawMessage `json:"cost"`
				} `json:"requests"`
				TotalNanoAiu json.RawMessage `json:"totalNanoAiu"`
				Usage        struct {
					InputTokens      json.RawMessage `json:"inputTokens"`
					OutputTokens     json.RawMessage `json:"outputTokens"`
					CacheReadTokens  json.RawMessage `json:"cacheReadTokens"`
					CacheWriteTokens json.RawMessage `json:"cacheWriteTokens"`
				} `json:"usage"`
			} `json:"modelMetrics"`
		} `json:"accountingSnapshot"`
	} `json:"data"`
}

// CopilotUsageCheckpoint maps one event-stream line to the running spend reading of
// a usage checkpoint: one entry per served model, sorted by model name, each
// holding that model's total over the session. It reports false for a line
// that is not valid JSON and for every type but the checkpoint. A count that
// is absent or not a JSON integer is nil, and a checkpoint without models
// yields a reading without entries.
func CopilotUsageCheckpoint(line []byte) (usage.SpendReading, bool) {
	var decoded checkpointLine
	if err := json.Unmarshal(line, &decoded); err != nil || decoded.Type != usageCheckpointType {
		return usage.SpendReading{}, false
	}
	metrics := decoded.Data.AccountingSnapshot.ModelMetrics
	entries := make([]usage.SpendReading, 0, len(metrics))
	for _, model := range slices.Sorted(maps.Keys(metrics)) {
		metric := metrics[model]
		u := metric.Usage
		entries = append(entries, usage.SpendReading{
			Input:      count(u.InputTokens),
			Output:     count(u.OutputTokens),
			CacheRead:  count(u.CacheReadTokens),
			CacheWrite: count(u.CacheWriteTokens),
			Model:      model,
			Amounts: []usage.AmountReading{
				{Unit: CopilotUnitPremiumRequests, Amount: number(metric.Requests.Cost)},
				{Unit: CopilotUnitAICredits, Amount: credits(metric.TotalNanoAiu)},
			},
		})
	}
	return usage.SpendReading{ByModel: entries}, true
}

// CopilotScanRecords returns the journal's per-request usage readings in file order.
// The caller supplies the scan that reaches the runtime's host, so a local and
// a remote launch share this read. It returns the scan's error unchanged, with
// the readings gathered before it; a caller ignores them on error.
func CopilotScanRecords(ctx context.Context, sessionID string,
	scan func(ctx context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error,
) ([]usage.SpendReading, error) {
	var readings []usage.SpendReading
	err := scan(ctx, CopilotJournalPath(sessionID), CopilotMaxFileBytes, usage.MaxRecordLineBytes, func(line []byte) error {
		if reading, ok := CopilotUsageRecord(line); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	return readings, err
}
