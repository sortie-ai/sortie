// Package sessionstate holds what the Copilot CLI session-state journal
// shares between its readers: where the journal lives, which session ids may
// name a path inside it, how much of it a reader may take, and how one of its
// per-request usage lines maps to a spend reading. The copilot-cli kind and
// the Copilot usage declaration on the agent-client-protocol kind import it.
//
// The package registers no kind and holds no adapter.
package sessionstate

import (
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	// HomeVar names the environment variable that relocates the runtime home.
	HomeVar = "COPILOT_HOME"

	// DefaultHome is the runtime home under the user's home directory when
	// HomeVar is unset or empty.
	DefaultHome = ".copilot"

	// MaxFileBytes is the largest journal a reader takes. Larger files are
	// rejected without being read.
	MaxFileBytes = 64 * 1024 * 1024

	// MaxLineBytes is the largest single journal line a reader scans,
	// matching the stdout scanner cap the fork-per-turn skeleton uses.
	MaxLineBytes = 10 * 1024 * 1024
)

const usageRecordType = "session.usage_record"

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidSessionID reports whether id is safe to join into a journal path as one
// segment.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// EventsRel returns the journal's path relative to the runtime home, with
// forward slashes. The caller checks [ValidSessionID] first.
func EventsRel(sessionID string) string {
	return "session-state/" + sessionID + "/events.jsonl"
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
// session totals and never yield a reading. A count that is absent or not a
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
