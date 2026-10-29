package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// maxJournalDirUnits is the longest project directory name, in UTF-16 code
// units, that Claude Code writes without a hash suffix.
const maxJournalDirUnits = 200

// journalLine holds the members of a journal line the adapter reads.
type journalLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// journalEntry is one reading with the time its line was written, which
// orders the lines of several files.
type journalEntry struct {
	reading usage.SpendReading
	at      time.Time
}

// journalDir returns the name of the journal's project directory for the
// working directory cwd the runtime ran in: every UTF-16 code unit outside
// [A-Za-z0-9] becomes a dash, and a name past maxJournalDirUnits keeps its
// first units and gains the base-36 absolute value of the Java string hash
// of cwd, as the runtime computes it.
func journalDir(cwd string) string {
	units := utf16.Encode([]rune(cwd))
	name := make([]byte, len(units))
	for i, unit := range units {
		name[i] = '-'
		if unit < 0x80 && isAlphanumeric(byte(unit)) {
			name[i] = byte(unit)
		}
	}
	if len(name) <= maxJournalDirUnits {
		return string(name)
	}
	return string(name[:maxJournalDirUnits]) + "-" + strconv.FormatInt(javaHashAbs(units), 36)
}

func isAlphanumeric(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func javaHashAbs(units []uint16) int64 {
	var hash int32
	for _, unit := range units {
		hash = 31*hash + int32(unit)
	}
	if hash < 0 {
		return -int64(hash)
	}
	return int64(hash)
}

// journalPath names a file under the runtime's project directory for this
// session, on whichever host runs the session.
func (s *sessionState) journalPath(segments ...string) agentcore.RuntimePath {
	return agentcore.RuntimePath{
		HomeVar:     "CLAUDE_CONFIG_DIR",
		HomeDefault: []string{".claude"},
		Segments:    append([]string{"projects", s.journalDir}, segments...),
	}
}

// readJournal reads the session journal and the journals of the sub-agents
// the stream or the session journal names, in the order the runtime made
// their requests. A missing session journal fails the read, because the
// runtime resumes a session from that file and its absence never means an
// empty history; a missing sub-agent journal adds nothing. The sub-agent
// journals are never found by listing a directory.
func (s *sessionState) readJournal(ctx context.Context) usage.RecordsRead {
	sessionFile := s.claudeSessionID + ".jsonl"
	session, named, err := s.scanJournal(ctx, sessionFile, s.journalPath(sessionFile))
	if err != nil {
		return s.failedJournalRead(err)
	}
	sequences := [][]journalEntry{session}
	ids := slices.Clone(named)
	for _, id := range s.subagents {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		name := "agent-" + id + ".jsonl"
		sequence, _, err := s.scanJournal(ctx, name, s.journalPath(s.claudeSessionID, "subagents", name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return s.failedJournalRead(err)
		}
		sequences = append(sequences, sequence)
	}
	return usage.RecordsRead{Readings: mergeJournals(sequences), OK: true}
}

func (s *sessionState) failedJournalRead(err error) usage.RecordsRead {
	if !errors.Is(err, fs.ErrNotExist) {
		s.logger().Debug("session journal read failed", slog.Any("error", err))
	}
	return usage.RecordsRead{}
}

// scanJournal reads one journal file. It returns the file's entries in file
// order and the sub-agent ids its lines name.
func (s *sessionState) scanJournal(ctx context.Context, name string, path agentcore.RuntimePath) (entries []journalEntry, named []string, err error) {
	ordinal := 0
	err = s.target.ScanRuntimeFile(ctx, nil, path, usage.MaxRecordsBytes, usage.MaxRecordLineBytes, func(line []byte) error {
		ordinal++
		parsed, decoded := decodeJournalLine(line)
		if id := parsed.subagentID(); id != "" && !slices.Contains(named, id) {
			named = append(named, id)
		}
		var previous time.Time
		if len(entries) > 0 {
			previous = entries[len(entries)-1].at
		}
		if entry, ok := journalEntryOf(parsed, decoded, name+":"+strconv.Itoa(ordinal), previous); ok {
			entries = append(entries, entry)
		}
		return nil
	})
	return entries, named, err
}

// decodeJournalLine decodes a line that must be a JSON object. A member of
// the wrong type on a line that is not an assistant line is ignored, because
// only an assistant line carries a figure.
func decodeJournalLine(line []byte) (journalLine, bool) {
	var parsed journalLine
	if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
		return parsed, false
	}
	err := json.Unmarshal(line, &parsed)
	var typeErr *json.UnmarshalTypeError
	if err == nil || (errors.As(err, &typeErr) && parsed.Type != "assistant") {
		return parsed, true
	}
	return journalLine{}, false
}

// subagentID returns the id of the sub-agent a user line's tool result
// started, or "" when the line names none.
func (l journalLine) subagentID() string {
	if l.Type != "user" {
		return ""
	}
	var result struct {
		AgentID string `json:"agentId"`
	}
	if json.Unmarshal(l.ToolUseResult, &result) != nil {
		return ""
	}
	return result.AgentID
}

// journalEntryOf maps a decoded line to a reading. A line that does not
// decode, and a request line without a message id, yield a reading with no
// counts under lineID, so the ledger sees an unknown amount instead of a
// skipped line.
func journalEntryOf(parsed journalLine, decoded bool, lineID string, previous time.Time) (journalEntry, bool) {
	if !decoded {
		return journalEntry{reading: usage.SpendReading{Identity: lineID}, at: previous}, true
	}
	if parsed.Type != "assistant" || parsed.Message.Model == syntheticModel {
		return journalEntry{}, false
	}
	at := previous
	if stamped, err := time.Parse(time.RFC3339, parsed.Timestamp); err == nil {
		at = stamped
	}
	if parsed.Message.ID == "" {
		return journalEntry{reading: usage.SpendReading{Identity: lineID}, at: at}, true
	}
	reading := spendReadingOf(parsed.Message.Usage, requestMembers)
	reading.Model = parsed.Message.Model
	reading.Identity = parsed.Message.ID + "/" + parsed.RequestID
	return journalEntry{reading: reading, at: at}, true
}

// mergeJournals interleaves the sequences by line time. Each sequence keeps
// its own order, and a tie goes to the earlier sequence, so the session
// journal wins over its sub-agents' journals.
func mergeJournals(sequences [][]journalEntry) []usage.SpendReading {
	var merged []usage.SpendReading
	for {
		next := -1
		for i, sequence := range sequences {
			if len(sequence) > 0 && (next < 0 || sequence[0].at.Before(sequences[next][0].at)) {
				next = i
			}
		}
		if next < 0 {
			return merged
		}
		merged = append(merged, sequences[next][0].reading)
		sequences[next] = sequences[next][1:]
	}
}
