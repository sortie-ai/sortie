package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// geminiRecords is the chat journal: one line per completed model request,
// appended when the request completes. It keeps thoughts apart from output.
var geminiRecords = usage.Source{
	Category: usage.SourceJournal,
	Strategy: usage.StrategyRecords,
	Spend: usage.SpendDeclaration{
		Shape:        usage.SpendPerRequest,
		CacheInInput: true,
		ServedModel:  true,
	},
}

// geminiJournalIDPrefix is how many leading bytes of the session id the
// runtime puts in a journal file's name.
const geminiJournalIDPrefix = 8

// errOtherSession marks a journal file whose header names another session.
var errOtherSession = errors.New("journal belongs to another session")

// geminiUnrecordedRequests reports whether the session sends a model request
// that neither the prompt result nor the journal records. A session whose
// current model is an auto selector sends one to choose the model, and a result
// that names no model cannot rule that out.
func geminiUnrecordedRequests(session json.RawMessage) bool {
	model, ok := jsonString(jsonMember(jsonMember(session, "models"), "currentModelId"))
	return !ok || model == "" || model == "auto" || strings.HasPrefix(model, "auto-")
}

// geminiJournalReading reads one journal line. A line that is not a JSON
// object, such as one a kill tore, is an unknown reading. A line that is not a
// completed request's record reads as none, because the journal writes a
// request's tokens only when the request completes. Control lines such as
// $rewindTo and $patch take nothing back: a rewound or compressed request was
// still billed.
func geminiJournalReading(line []byte) (usage.SpendReading, bool) {
	members, ok := jsonObject(line)
	if !ok {
		return usage.SpendReading{}, true
	}
	if kind, _ := jsonString(members["type"]); kind != "gemini" {
		return usage.SpendReading{}, false
	}
	rawTokens := members["tokens"]
	if len(rawTokens) == 0 || string(rawTokens) == "null" {
		return usage.SpendReading{}, false
	}

	var reading usage.SpendReading
	reading.Identity, _ = jsonString(members["id"])
	reading.Model, _ = jsonString(members["model"])
	tokens, ok := jsonObject(rawTokens)
	if !ok {
		return reading, true
	}

	if input, tool := jsonCount(tokens["input"]), jsonCount(tokens["tool"]); input != nil && tool != nil {
		total := *input + *tool
		reading.Input = &total
	}
	reading.Output = jsonCount(tokens["output"])
	reading.Reasoning = jsonCount(tokens["thoughts"])
	reading.CacheRead = jsonCount(tokens["cached"])
	var noCacheWrite int64
	reading.CacheWrite = &noCacheWrite
	return reading, true
}

// geminiReadRecords reads the session's journal in file order. The runtime
// names the file by its start minute and its directory by a slug from its own
// project registry, so the file is found by a name pattern instead of a path.
// A file counts only when its header names this session: the file each process
// start writes, and a session sharing the id prefix, hold none of its requests.
func geminiReadRecords(ctx context.Context, rt PullRuntime) (Records, error) {
	id := rt.SessionID()
	if !geminiJournalNameable(id) {
		return Records{}, errors.New("session id cannot name a journal file")
	}
	pattern := agentcore.RuntimePath{
		HomeVar:  geminiHomeVar,
		Segments: []string{".gemini", "tmp", "*", "chats", "session-*-" + id[:geminiJournalIDPrefix] + ".jsonl"},
	}
	paths, err := rt.MatchHomeFiles(ctx, pattern)
	if err != nil {
		return Records{}, fmt.Errorf("match journal files: %w", err)
	}

	var spend []usage.SpendReading
	for _, path := range paths {
		own, err := scanGeminiJournal(ctx, rt, path, id)
		switch {
		case errors.Is(err, errOtherSession), errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return Records{}, err
		}
		spend = append(spend, own...)
	}
	return Records{Spend: spend}, nil
}

// geminiJournalNameable reports whether the id's leading bytes can sit in a
// file name pattern as literals: anything else could widen the match or, on a
// remote host, reach a shell.
func geminiJournalNameable(id string) bool {
	if len(id) < geminiJournalIDPrefix {
		return false
	}
	for i := range geminiJournalIDPrefix {
		c := id[i]
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// scanGeminiJournal returns the readings of one journal file, or
// errOtherSession when its header, the first line, names another session.
func scanGeminiJournal(ctx context.Context, rt PullRuntime, path agentcore.RuntimePath, id string) ([]usage.SpendReading, error) {
	var readings []usage.SpendReading
	first := true
	err := rt.ScanHomeFile(ctx, path, usage.MaxRecordsBytes, usage.MaxRecordLineBytes, func(line []byte) error {
		if first {
			first = false
			if header, _ := jsonString(jsonMember(line, "sessionId")); header != id {
				return errOtherSession
			}
			return nil
		}
		if reading, ok := geminiJournalReading(line); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return readings, nil
}
