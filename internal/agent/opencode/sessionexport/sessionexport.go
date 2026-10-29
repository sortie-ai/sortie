// Package sessionexport builds the OpenCode session export invocation and
// decodes the export document into per-request spend records. The opencode
// kind and the OpenCode declaration on the agent-client-protocol kind share
// it, so one parser and one size limit serve both routes. Start with [Args]
// and [Parse].
//
// The package registers no kind and holds no adapter.
package sessionexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// ErrOtherSession marks an export whose info.id is not the session asked for.
var ErrOtherSession = errors.New("session export of another session")

// Record is one finished assistant message of an export.
type Record struct {
	// Spend holds the counts as exported, with Model and Identity.
	Spend usage.SpendReading

	// CreatedMS is time.created in Unix milliseconds; 0 when absent or not an
	// integer.
	CreatedMS int64
}

// Args returns the argument vector of the sanitized export of one session.
// Sanitizing keeps tool output out of the document while it keeps counts,
// model, and timestamps.
func Args(sessionID string) []string {
	return []string{"session", "export", "--standalone", "--sanitize", sessionID}
}

// Count returns the integer a JSON member holds, nil for an absent, null,
// fractional, or non-numeric member.
func Count(raw json.RawMessage) *int64 {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// CountOmittedAtZero returns [Count] of a counter the runtime leaves out when
// it is zero, so an absent member reads zero.
func CountOmittedAtZero(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		var zero int64
		return &zero
	}
	return Count(raw)
}

// Parse returns the records of the finished assistant messages of an export,
// in document order. A message counts when it has a non-empty finish and a
// tokens object, an aborted one that kept its tokens included. The session
// aggregate in info is never read: it also holds requests no message
// records. Parse refuses data above [usage.MaxRecordsBytes], a document that
// does not decode, and a document of another session ([ErrOtherSession]).
func Parse(data []byte, sessionID string) ([]Record, error) {
	if len(data) > usage.MaxRecordsBytes {
		return nil, fmt.Errorf("session export exceeds %d bytes", usage.MaxRecordsBytes)
	}
	var doc struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
		Messages []struct {
			ID     string          `json:"id"`
			Type   string          `json:"type"`
			Finish string          `json:"finish"`
			Tokens json.RawMessage `json:"tokens"`
			Model  json.RawMessage `json:"model"`
			Time   json.RawMessage `json:"time"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode session export: %w", err)
	}
	if doc.Info.ID != sessionID {
		return nil, ErrOtherSession
	}

	var records []Record
	for _, m := range doc.Messages {
		if m.Type != "assistant" || m.Finish == "" {
			continue
		}
		tokens, ok := decodeObject[exportTokens](m.Tokens)
		if !ok {
			continue
		}
		cache, _ := decodeObject[exportCache](tokens.Cache)
		model, _ := decodeObject[exportModel](m.Model)
		created, _ := decodeObject[exportTime](m.Time)

		record := Record{Spend: usage.SpendReading{
			Input:      Count(tokens.Input),
			Output:     Count(tokens.Output),
			Reasoning:  Count(tokens.Reasoning),
			CacheRead:  Count(cache.Read),
			CacheWrite: Count(cache.Write),
			Identity:   m.ID,
		}}
		if model.ProviderID != "" && model.ID != "" {
			record.Spend.Model = model.ProviderID + "/" + model.ID
		}
		if ms := Count(created.Created); ms != nil {
			record.CreatedMS = *ms
		}
		records = append(records, record)
	}
	return records, nil
}

type exportTokens struct {
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Reasoning json.RawMessage `json:"reasoning"`
	Cache     json.RawMessage `json:"cache"`
}

type exportCache struct {
	Read  json.RawMessage `json:"read"`
	Write json.RawMessage `json:"write"`
}

type exportModel struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
}

type exportTime struct {
	Created json.RawMessage `json:"created"`
}

// decodeObject decodes raw when it is a JSON object; an absent, null, or
// non-object member and a member that does not decode yield false.
func decodeObject[T any](raw json.RawMessage) (T, bool) {
	var v T
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		var zero T
		return zero, false
	}
	return v, true
}
