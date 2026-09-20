package usagesource

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

const geminiAgentName = "gemini-cli"

// geminiConfirmedVersions are the builds this source was measured against; it
// supplies no figure for any other.
var geminiConfirmedVersions = []string{"0.59.0"}

const (
	geminiSourceTelemetry = "telemetry"
	geminiSourceJournal   = "journal"
)

// The runtime batches telemetry on a five second tick no configuration
// shortens; the deadline is that tick plus write slack, and the poll interval
// is short enough that the early exit, not the interval, ends a fast drain.
const (
	geminiDrainDeadline = 6 * time.Second
	geminiPollInterval  = 200 * time.Millisecond
)

const (
	geminiMaxTelemetryBytes = 64 * 1024 * 1024
	geminiMaxJournalBytes   = 64 * 1024 * 1024
	geminiMaxJournalLine    = 10 * 1024 * 1024
)

// geminiTelemetryEvent is the one record shape carrying every counter, among
// the spans, metric blocks and second log representation sharing the file.
const geminiTelemetryEvent = "gemini_cli.api_response"

type geminiRecord struct {
	Attributes geminiAttributes `json:"attributes"`
}

// geminiAttributes is the attribute block of one telemetry record. The counters
// are pointers because the runtime writes this record for every completed
// request whether or not the response carried a usage block, and only a field's
// absence tells an announcement from a measurement.
type geminiAttributes struct {
	EventName string `json:"event.name"`
	SessionID string `json:"session.id"`
	Model     string `json:"model"`
	Input     *int64 `json:"input_token_count"`
	Output    *int64 `json:"output_token_count"`
	Cached    *int64 `json:"cached_content_token_count"`
	Thoughts  *int64 `json:"thoughts_token_count"`
}

type geminiJournalLine struct {
	SessionID string               `json:"sessionId"`
	ID        string               `json:"id"`
	Type      string               `json:"type"`
	Model     string               `json:"model"`
	Tokens    *geminiJournalTokens `json:"tokens"`
}

type geminiJournalTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
}

// geminiFigure is one source's whole reading of this run. basis counts input
// plus output and no reasoning, matching the wire bound's basis; measured
// separates a real zero from the absence of a figure.
type geminiFigure struct {
	usage    domain.TokenUsage
	model    string
	basis    int64
	measured bool
}

// geminiReader reads the runtime's OpenTelemetry output as the primary source
// and its session journal as the durability backstop. The two are never summed
// for one request: telemetry is complete but arrives a batch tick late and is
// lost if the runtime is killed in that window, while the journal is written
// before the prompt result and survives the kill. A drain that ends with
// telemetry short of the turn's bound reports the journal's whole
// run-cumulative figure instead, replacing rather than adding to it.
type geminiReader struct {
	mu sync.Mutex

	dir     string
	outfile string

	sessionID string

	// offset is where the next telemetry decode starts. It begins at the
	// file's size when the session opened, so a resumed runtime's earlier
	// records contribute nothing.
	offset int64

	total     domain.TokenUsage
	reasoning int64
	model     string

	// blocks counts telemetry records that carried a counter block; counted
	// counts the ones that carried a figure. The figure alone tells neither
	// apart from no record.
	blocks  int
	counted int

	// reported is the highest run-cumulative figure, on the bound's basis, a
	// drain has answered with. A turn's contribution is measured from it so a
	// fallback to the journal is never charged twice.
	reported int64

	// outstanding is the part of earlier turns' bounds no figure has covered
	// yet. Neither source says which turn a record belongs to, so carrying the
	// debt stops the tail of a partial turn from proving the one after it.
	outstanding int64

	completeness Completeness

	// inherited are the journal messages present when the session opened, so a
	// resumed history is not charged to this run.
	inherited map[string]struct{}

	// now and home are substituted by tests; production reads time.Now and the
	// runtime's own home.
	now  func() time.Time
	home string
}

func newGeminiReader() *geminiReader {
	return &geminiReader{}
}

// Claim arms the source for a local launch, taking a session-private directory
// for the outfile and returning the assignments that make the runtime write
// there. A remote launch is refused: this source reads a local filesystem, and
// a worker reached over SSH writes its outfile on the far host.
func (r *geminiReader) Claim(target agentcore.LaunchTarget) ([]string, bool) {
	if target.RemoteCommand != "" {
		return nil, false
	}

	dir, err := os.MkdirTemp("", "sortie-usage-")
	if err != nil {
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.dir = dir
	r.outfile = filepath.Join(dir, "telemetry.json")
	return []string{
		"GEMINI_TELEMETRY_ENABLED=true",
		"GEMINI_TELEMETRY_TARGET=local",
		"GEMINI_TELEMETRY_OUTFILE=" + r.outfile,
	}, true
}

// Recognize accepts only the name and versions this source was measured
// against.
func (r *geminiReader) Recognize(name, version string) bool {
	if name != geminiAgentName {
		return false
	}
	return slices.Contains(geminiConfirmedVersions, version)
}

// Open records the session whose records count and the starting position in
// each source: the outfile's size, and the message identifiers a resumed
// runtime brought with it.
func (r *geminiReader) Open(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sessionID = sessionID
	r.total = domain.TokenUsage{}
	r.reasoning = 0
	r.model = ""
	r.blocks = 0
	r.counted = 0
	r.reported = 0
	r.outstanding = 0
	r.completeness = CompletenessUnknown
	r.offset = 0
	if info, err := os.Stat(r.outfile); err == nil {
		r.offset = info.Size()
	}
	r.inherited = r.journalMessageIDs()
}

// Close removes the session-private directory Claim took.
func (r *geminiReader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dir == "" {
		return
	}
	os.RemoveAll(r.dir) //nolint:errcheck,gosec // best-effort cleanup of a directory this source created
	r.dir = ""
	r.outfile = ""
}

// Completeness reports how far the figure the most recent Drain returned was
// proven to account for that turn, and is CompletenessUnknown before any Drain
// has answered. It describes that one call and is reset by the next, so it is
// read on the goroutine that called Drain, before another begins.
func (r *geminiReader) Completeness() Completeness {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completeness
}

// Drain polls the telemetry outfile until this turn's records account for the
// turn's bound, then reconciles what it holds against the journal.
func (r *geminiReader) Drain(ctx context.Context, lowerBound int64) (agentcore.RecoveredUsage, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completeness = CompletenessUnknown
	if r.sessionID == "" || r.outfile == "" {
		return agentcore.RecoveredUsage{}, "", false
	}

	blocksBefore, countedBefore := r.blocks, r.counted
	deadline := r.clock().Add(geminiDrainDeadline)

	for {
		r.readTelemetry()
		telemetry := r.telemetryFigure(lowerBound, blocksBefore, countedBefore)
		if telemetry.measured && r.accountsForTurn(telemetry.basis, lowerBound) {
			return r.answer(telemetry, geminiSourceTelemetry, CompletenessAccounted, lowerBound)
		}
		if !r.clock().Before(deadline) {
			break
		}
		if !r.waitForNextPoll(ctx) {
			return agentcore.RecoveredUsage{}, "", false
		}
	}

	// The journal is written before the prompt result, so whatever it holds is
	// at least as far along as batch telemetry: the two are one run's figure
	// read twice, and the fuller reading replaces the other rather than adding
	// to it.
	telemetry := r.telemetryFigure(lowerBound, blocksBefore, countedBefore)
	journal := r.readJournal(lowerBound)

	figure, source := telemetry, geminiSourceTelemetry
	if journal.measured && (!telemetry.measured || journal.basis > telemetry.basis) {
		figure, source = journal, geminiSourceJournal
	}
	if !figure.measured {
		r.settle(r.reported, lowerBound)
		return agentcore.RecoveredUsage{}, "", false
	}
	// Graded before it is answered: answering moves the position the grade is
	// measured from.
	completeness := r.classify(figure.basis, lowerBound)
	return r.answer(figure, source, completeness, lowerBound)
}

// waitForNextPoll releases the lock for one poll interval and retakes it before
// returning, so a Close on the session's own goroutine is not held behind a
// drain that is only waiting. Everything the loop has accumulated lives in the
// reader, so it survives the gap. It reports whether the interval elapsed,
// false meaning the context ended first.
func (r *geminiReader) waitForNextPoll(ctx context.Context) bool {
	r.mu.Unlock()
	defer r.mu.Lock()

	select {
	case <-ctx.Done():
		return false
	case <-time.After(geminiPollInterval):
		return true
	}
}

func (r *geminiReader) answer(figure geminiFigure, source string, completeness Completeness, lowerBound int64) (agentcore.RecoveredUsage, string, bool) {
	r.completeness = completeness
	r.settle(figure.basis, lowerBound)
	return agentcore.RecoveredUsage{Run: figure.usage, Model: figure.model}, source, true
}

// settle moves the position the next turn is measured from and carries the part
// of this turn's bound no figure covered into the next turn's debt. reported
// never moves backwards, so a later turn is measured from the fullest figure the
// session has already been told about.
func (r *geminiReader) settle(basis, lowerBound int64) {
	owed := r.outstanding + max(lowerBound, 0)
	covered := max(basis-r.reported, 0)
	r.outstanding = max(owed-covered, 0)
	r.reported = max(r.reported, basis)
}

// accountsForTurn reports whether the run-cumulative figure has advanced since
// the last drain by the whole of what the run owes: this turn's bound plus any
// earlier bound no figure has covered. The comparison is on the bound's own
// basis (input plus output, no reasoning), so one early request's thinking
// cannot satisfy a whole turn of requests.
func (r *geminiReader) accountsForTurn(basis, lowerBound int64) bool {
	return lowerBound > 0 && basis-r.reported >= lowerBound+r.outstanding
}

// classify grades a figure against the turn's bound. Without a bound there is
// nothing to prove, which is a different answer from proving it incomplete.
func (r *geminiReader) classify(basis, lowerBound int64) Completeness {
	if lowerBound <= 0 {
		return CompletenessUnknown
	}
	if r.accountsForTurn(basis, lowerBound) {
		return CompletenessAccounted
	}
	return CompletenessPartial
}

func (r *geminiReader) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// telemetryFigure returns what telemetry has read for this run so far. A record
// whose counters all default to zero proves a measurement only where the turn's
// own bound reported no spend to contradict it.
func (r *geminiReader) telemetryFigure(lowerBound int64, blocksBefore, countedBefore int) geminiFigure {
	return geminiFigure{
		usage:    r.total,
		model:    r.model,
		basis:    r.total.InputTokens + r.total.OutputTokens - r.reasoning,
		measured: r.counted > countedBefore || (r.blocks > blocksBefore && lowerBound <= 0),
	}
}

// readTelemetry folds this session's new outfile records into the run-cumulative
// figure. The file is a stream of pretty-printed JSON values, not one per line,
// so a trailing value still being written decodes short and is left for the next
// pass.
func (r *geminiReader) readTelemetry() {
	info, err := os.Stat(r.outfile)
	if err != nil || info.Size() <= r.offset {
		return
	}
	if info.Size() > geminiMaxTelemetryBytes {
		return
	}

	file, err := os.Open(r.outfile) //nolint:gosec // the path is this source's own temporary file
	if err != nil {
		return
	}
	defer file.Close() //nolint:errcheck // read-only handle

	if _, err := file.Seek(r.offset, io.SeekStart); err != nil {
		return
	}

	decoder := json.NewDecoder(file)
	consumed := int64(0)
	for {
		var record geminiRecord
		if err := decoder.Decode(&record); err != nil {
			break
		}
		consumed = decoder.InputOffset()
		r.fold(record)
	}
	r.offset += consumed
}

// fold adds one telemetry record's counters to the run-cumulative figure when
// it belongs to this session and carries a counter block. Input already
// includes the cached read, so cached is recorded as a subset and never added
// on top; output carries the separately priced thought tokens.
func (r *geminiReader) fold(record geminiRecord) {
	attrs := record.Attributes
	if attrs.EventName != geminiTelemetryEvent || attrs.SessionID != r.sessionID {
		return
	}
	if attrs.Input == nil || attrs.Output == nil {
		return
	}

	input, output := *attrs.Input, *attrs.Output
	cached, thoughts := counterOrZero(attrs.Cached), counterOrZero(attrs.Thoughts)

	r.total.InputTokens += input
	r.total.OutputTokens += output + thoughts
	r.total.CacheReadTokens += cached
	r.total.TotalTokens = r.total.InputTokens + r.total.OutputTokens
	r.reasoning += thoughts
	r.blocks++
	if input > 0 || output > 0 || cached > 0 || thoughts > 0 {
		r.counted++
	}
	if attrs.Model != "" {
		r.model = attrs.Model
	}
}

func counterOrZero(counter *int64) int64 {
	if counter == nil {
		return 0
	}
	return *counter
}

// readJournal sums the token-bearing messages this session appended after the
// run opened. It is the backstop for a kill that destroys the telemetry file
// inside the batch window, so it returns a whole run-cumulative figure rather
// than one turn's contribution. Messages are deduplicated by identifier with
// the last occurrence winning, since later journal lines revise earlier ones.
func (r *geminiReader) readJournal(lowerBound int64) geminiFigure {
	path, ok := r.findJournal()
	if !ok {
		return geminiFigure{}
	}

	file, err := os.Open(path) //nolint:gosec // the path came from this source's own scan of the runtime's home
	if err != nil {
		return geminiFigure{}
	}
	defer file.Close() //nolint:errcheck // read-only handle

	type entry struct {
		tokens geminiJournalTokens
		model  string
	}
	latest := map[string]entry{}
	order := []string{}

	scanner := newLineScanner(file, geminiMaxJournalLine)
	for scanner.Scan() {
		var line geminiJournalLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.ID == "" || line.Tokens == nil {
			continue
		}
		if _, inherited := r.inherited[line.ID]; inherited {
			continue
		}
		if _, seen := latest[line.ID]; !seen {
			order = append(order, line.ID)
		}
		latest[line.ID] = entry{tokens: *line.Tokens, model: line.Model}
	}

	figure := geminiFigure{}
	for _, id := range order {
		e := latest[id]
		figure.usage.InputTokens += e.tokens.Input
		figure.usage.OutputTokens += e.tokens.Output + e.tokens.Thoughts
		figure.usage.CacheReadTokens += e.tokens.Cached
		figure.basis += e.tokens.Input + e.tokens.Output
		if e.tokens.Input > 0 || e.tokens.Output > 0 || e.tokens.Cached > 0 || e.tokens.Thoughts > 0 {
			figure.measured = true
		}
		if e.model != "" {
			figure.model = e.model
		}
	}
	figure.usage.TotalTokens = figure.usage.InputTokens + figure.usage.OutputTokens

	// The journal substitutes zero for a response that reported none, as
	// telemetry does, so a block of zeros proves a measurement only where no
	// bound contradicts it.
	if len(order) > 0 && lowerBound <= 0 {
		figure.measured = true
	}
	return figure
}

// journalMessageIDs returns the identifiers of every message the journal
// already holds, the position a resumed history starts this run at.
func (r *geminiReader) journalMessageIDs() map[string]struct{} {
	path, ok := r.findJournal()
	if !ok {
		return nil
	}
	file, err := os.Open(path) //nolint:gosec // the path came from this source's own scan of the runtime's home
	if err != nil {
		return nil
	}
	defer file.Close() //nolint:errcheck // read-only handle

	ids := map[string]struct{}{}
	scanner := newLineScanner(file, geminiMaxJournalLine)
	for scanner.Scan() {
		var line geminiJournalLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.ID == "" {
			continue
		}
		ids[line.ID] = struct{}{}
	}
	return ids
}

// findJournal returns the path of the journal whose metadata header names this
// session. The filename carries only a truncated identifier, so it cannot be
// constructed; the header is matched instead.
func (r *geminiReader) findJournal() (string, bool) {
	home := cmp.Or(r.home, os.Getenv("GEMINI_CLI_HOME"))
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = dir
	}

	matches, err := filepath.Glob(filepath.Join(home, ".gemini", "tmp", "*", "chats", "*.jsonl"))
	if err != nil {
		return "", false
	}
	for _, path := range matches {
		if r.journalNames(path) {
			return path, true
		}
	}
	return "", false
}

func newLineScanner(r io.Reader, maxLine int) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	return scanner
}

func (r *geminiReader) journalNames(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // the path came from a glob of the runtime's own home, not from input
	if err != nil || info.Size() > geminiMaxJournalBytes {
		return false
	}
	file, err := os.Open(path) //nolint:gosec // the path came from a glob of the runtime's own home
	if err != nil {
		return false
	}
	defer file.Close() //nolint:errcheck // read-only handle

	scanner := newLineScanner(file, geminiMaxJournalLine)
	if !scanner.Scan() {
		return false
	}
	var header geminiJournalLine
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return false
	}
	return header.SessionID == r.sessionID
}
