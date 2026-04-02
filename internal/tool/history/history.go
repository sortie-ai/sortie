// Package history implements [domain.AgentTool] for the workspace_history
// tool. It returns the most recent completed run attempts for the current
// issue so that agents can learn from prior session outcomes.
package history

import (
	"context"
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.AgentTool = (*HistoryTool)(nil)

const maxEntries = 10

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// QueryFunc returns the most recent run history entries for a given issue,
// up to limit entries, ordered newest-first. Implementations must return a
// non-nil empty slice when no entries exist. A limit of 0 means no limit.
type QueryFunc func(ctx context.Context, issueID string, limit int) ([]Entry, error)

// Entry represents a single completed run attempt as returned by [QueryFunc].
type Entry struct {
	Attempt      int     // Attempt number at time of run (1-based).
	AgentAdapter string  // Agent adapter kind used (e.g. "claude-code").
	StartedAt    string  // ISO-8601 timestamp of run start.
	CompletedAt  string  // ISO-8601 timestamp of run completion.
	Status       string  // Terminal status: succeeded, failed, timed_out, stalled, cancelled.
	Error        *string // Error message if failed; nil on success.
}

// HistoryTool implements [domain.AgentTool] for the workspace_history tool.
// Construct via [New]; safe for concurrent use after construction.
type HistoryTool struct {
	query   QueryFunc
	issueID string
}

type historyEntry struct {
	Attempt      int     `json:"attempt"`
	AgentAdapter string  `json:"agent_adapter"`
	StartedAt    string  `json:"started_at"`
	CompletedAt  string  `json:"completed_at"`
	Status       string  `json:"status"`
	Error        *string `json:"error"`
}

type historyResponse struct {
	IssueID string         `json:"issue_id"`
	Entries []historyEntry `json:"entries"`
}

// New returns a [HistoryTool] that queries run history for the given issue.
//
// New panics if query is nil or issueID is empty (programming errors).
func New(query QueryFunc, issueID string) *HistoryTool {
	if query == nil {
		panic("history.New: query must not be nil")
	}
	if issueID == "" {
		panic("history.New: issueID must not be empty")
	}
	return &HistoryTool{query: query, issueID: issueID}
}

// Name returns "workspace_history".
func (t *HistoryTool) Name() string { return "workspace_history" }

// Description returns a human-readable summary of the tool.
func (t *HistoryTool) Description() string {
	return "Returns the most recent completed run attempts for the current issue, " +
		"including status, error messages, and timing. Use this to understand what " +
		"happened in prior sessions before deciding on a strategy."
}

// InputSchema returns the JSON Schema for workspace_history input.
// The tool accepts no parameters; the schema is an empty object.
// The returned slice is a defensive copy.
func (t *HistoryTool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

// Execute queries run history for the current issue and returns the results
// as a JSON object. Query failures are returned as a JSON error response with
// a nil Go error. Only internal marshal failures produce a non-nil Go error.
func (t *HistoryTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	results, err := t.query(ctx, t.issueID, maxEntries)
	if err != nil {
		resp, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return resp, nil
	}

	entries := make([]historyEntry, len(results))
	for i, r := range results {
		entries[i] = historyEntry(r)
	}

	// Guarantee non-nil slice so JSON serializes as [] not null.
	if len(entries) == 0 {
		entries = []historyEntry{}
	}

	return json.Marshal(historyResponse{
		IssueID: t.issueID,
		Entries: entries,
	})
}
