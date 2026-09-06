//go:build unix

package clientprotocol

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
	"github.com/sortie-ai/sortie/internal/qualification/e2e"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// geminiNativeProbeBound bounds every native surface launch.
const geminiNativeProbeBound = 5 * time.Minute

// geminiAdapterNotesRelPath is the tracked durable notes file the rerun
// compares against, read but never mutated.
const geminiAdapterNotesRelPath = "../../../docs/gemini-adapter-notes.md"

// geminiQualificationResult is what one qualification collection
// produced: the validated verdict, the bounded summary, the record
// count, and whether the durable notes were compared.
type geminiQualificationResult struct {
	Verdict         qualification.Verdict
	Summary         string
	EvidenceRecords int
	NotesCompared   bool
}

// geminiEvidenceDir is the ephemeral evidence directory the collector
// owns under the test's temporary root.
func geminiEvidenceDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "qualification")
}

// geminiJSONLWriter buffers JSON Lines and syncs every append, so a
// validated file is on disk before its validators read it.
type geminiJSONLWriter struct {
	file *os.File
}

func newGeminiJSONLWriter(file *os.File) *geminiJSONLWriter {
	return &geminiJSONLWriter{file: file}
}

// append marshals and writes one record line.
func (w *geminiJSONLWriter) append(t *testing.T, rec qualification.Record) error {
	t.Helper()
	line, err := qualification.MarshalRecord(rec)
	if err != nil {
		return err
	}
	if _, err := w.file.Write(append(line, '\n')); err != nil {
		return err
	}
	return w.file.Sync()
}

// close flushes and closes the file.
func (w *geminiJSONLWriter) close(t *testing.T) {
	t.Helper()
	if err := w.file.Close(); err != nil {
		t.Fatalf("close the evidence file: %v", err)
	}
}

// geminiWriteQualificationObservations serializes the non-final records
// in the canonical write order into the evidence file, flushes, and
// closes it. The canonical ordering, not goroutine completion order,
// determines the sequence numbers.
func geminiWriteQualificationObservations(t *testing.T, dir string, records []qualification.Record) string {
	t.Helper()

	ordered := slices.Clone(records)
	slices.SortStableFunc(ordered, geminiOrderCompare)
	for i := range ordered {
		ordered[i].Sequence = i + 1
		ordered[i].ObservedAt = qualification.FixtureTime
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create the ephemeral evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the path is built from the test's own temp directory
	if err != nil {
		t.Fatalf("open the evidence file: %v", err)
	}
	writer := newGeminiJSONLWriter(file)
	for _, rec := range ordered {
		if err := writer.append(t, rec); err != nil {
			t.Fatalf("write evidence record %d: %v", rec.Sequence, err)
		}
	}
	writer.close(t)
	return path
}

// geminiEvidenceLineCount counts the non-empty lines of an evidence
// file, so the aggregate's sequence continues the file's own count.
func geminiEvidenceLineCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the path is built from the test's own temp directory
	if err != nil {
		t.Fatalf("read the evidence file: %v", err)
	}
	count := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// geminiAppendQualificationAggregate reopens the evidence file in append
// mode, writes exactly one terminal aggregate record carrying the
// computed verdict, flushes, and closes it. The aggregate is the last
// line of the file.
func geminiAppendQualificationAggregate(t *testing.T, path string, verdict qualification.Verdict) {
	t.Helper()

	classification := qualification.AggregateGradeFor(verdict)
	aggregate := qualification.Record{
		SchemaVersion: 1,
		Sequence:      geminiEvidenceLineCount(t, path) + 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioQualification,
		Surface:       qualification.SurfaceAggregate,
		Capability:    qualification.CapabilityEligibility,
		Source:        qualification.SourceComparison,
		Grade:         classification,
		Outcome:       qualification.OutcomePass,
		InputID:       qualification.InputAggregate,
		EvidencePath:  new(qualification.EvidencePathQualificationVerdict),
		Detail:        "aggregate qualification verdict recomputed from the closed non-final evidence set",
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the path is built from the test's own temp directory
	if err != nil {
		t.Fatalf("reopen the evidence file for the aggregate: %v", err)
	}
	writer := newGeminiJSONLWriter(file)
	if err := writer.append(t, aggregate); err != nil {
		t.Fatalf("append the aggregate record: %v", err)
	}
	writer.close(t)
}

// geminiRunTwoPassEvidenceFlow runs the strict two-pass state machine
// against the declaration set the run was collected under: write the
// closed non-final set, validate it, append exactly one aggregate,
// validate the complete file, derive the bounded summary, and return
// the verdict with the summary and the final record count.
func geminiRunTwoPassEvidenceFlow(t *testing.T, dir string, records []qualification.Record, declarations qualification.DeclaredGapSet) (qualification.Verdict, string, int) {
	t.Helper()

	path := geminiWriteQualificationObservations(t, dir, records)
	verdict, err := qualification.ValidateObservationsWithDeclarations(path, declarations)
	if err != nil {
		t.Fatalf("first-pass validation failed: %v", err)
	}
	geminiAppendQualificationAggregate(t, path, verdict)
	finalVerdict, err := qualification.ValidateEvidenceWithDeclarations(path, declarations)
	if err != nil {
		t.Fatalf("final-pass validation failed: %v", err)
	}
	if finalVerdict != verdict {
		t.Fatalf("final-pass verdict %q differs from the first-pass verdict %q", finalVerdict, verdict)
	}

	complete, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("read the validated evidence file: %v", err)
	}
	conclusions, err := geminiSummaryConclusionsFromRecords(complete[:len(complete)-1], verdict)
	if err != nil {
		t.Fatalf("derive the bounded summary: %v", err)
	}
	return verdict, formatGeminiQualificationSummary(conclusions), len(complete)
}

// geminiNotesConsistencyError reads the tracked notes without mutating
// them and compares their bounded conclusions against the freshly
// validated summary. compared reports whether durable notes existed;
// err is the mismatch, when the notes exist but disagree.
func geminiNotesConsistencyError(notesPath string, conclusions geminiSummaryConclusions) (compared bool, err error) {
	raw, readErr := os.ReadFile(notesPath) //nolint:gosec // the notes path is the operator's tracked documentation file
	if readErr != nil {
		return false, nil
	}
	return true, validateGeminiAdapterNotes(string(raw), conclusions)
}

// geminiRunNotesConsistency performs the rerun's notes-consistency
// check: it reads the tracked notes, never mutates them, and requires
// their bounded conclusions and verdict to equal the freshly validated
// summary. A run without durable notes yet returns false and does not
// fail: the initial run may stop after the validated summary, and
// support publication requires the notes-consistent rerun.
func geminiRunNotesConsistency(t *testing.T, notesPath string, conclusions geminiSummaryConclusions, requireMatch bool) bool {
	t.Helper()

	compared, err := geminiNotesConsistencyError(notesPath, conclusions)
	if !compared {
		if requireMatch {
			t.Fatalf("the durable adapter notes are missing; the rerun requires the notes-consistency check to pass")
		}
		return false
	}
	if err != nil {
		if requireMatch {
			t.Fatalf("the durable notes do not match the freshly validated summary: %v", err)
		}
		return false
	}
	return true
}

// TestGeminiQualificationCollectorOrdering confirms the collector's
// writer: records handed over in any completion order land in the
// canonical scenario, surface, capability, and per-scenario tiebreak
// order, carry one-based contiguous sequence numbers, and pass the
// strict first-pass validator.
func TestGeminiQualificationCollectorOrdering(t *testing.T) {
	t.Parallel()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()

	shuffled := slices.Clone(fixture.Records)
	slices.Reverse(shuffled)
	for i := range shuffled {
		// A real collector never assigns Sequence itself; only the
		// writer does, after sorting. Zeroing it here proves the sort
		// key is the canonical scenario/surface/capability tiebreak,
		// not a leftover sequence number the reversal happened to
		// preserve, which would mask a comparator that only looks at
		// Sequence.
		shuffled[i].Sequence = 0
	}

	path := geminiWriteQualificationObservations(t, geminiEvidenceDir(t), shuffled)

	stored, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("qualification.ReadEvidenceFile() error = %v", err)
	}
	if len(stored) != len(fixture.Records) {
		t.Fatalf("stored record count = %d, want %d", len(stored), len(fixture.Records))
	}
	for i := range stored {
		if stored[i].Sequence != i+1 {
			t.Errorf("record %d carries sequence %d, want contiguous one-based numbering", i+1, stored[i].Sequence)
			break
		}
	}
	for i := 1; i < len(stored); i++ {
		if geminiOrderCompare(stored[i-1], stored[i]) > 0 {
			t.Errorf("stored records are out of canonical order at position %d", i+1)
			break
		}
	}
	qualification.RequireObservationVerdict(t, path, qualification.VerdictQualified)
}

// TestGeminiQualificationTwoPassWrite confirms the two-pass flow: the
// non-final set validates at exactly 66+T+N records, the aggregate is
// appended exactly once as the final line, and the final pass
// independently recomputes the same verdict from 67+T+N records.
func TestGeminiQualificationTwoPassWrite(t *testing.T) {
	t.Parallel()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()
	T := qualification.TokenRecordCount(fixture.Records)
	N := qualification.ProtocolSessionCount(fixture.Records)

	path := geminiWriteQualificationObservations(t, geminiEvidenceDir(t), fixture.Records)

	verdict, err := qualification.ValidateObservations(path)
	if err != nil {
		t.Fatalf("qualification.ValidateObservations() error = %v", err)
	}
	if verdict != qualification.VerdictQualified {
		t.Errorf("first-pass verdict = %q, want %q", verdict, qualification.VerdictQualified)
	}
	if got := geminiEvidenceLineCount(t, path); got != 66+T+N {
		t.Errorf("first-pass record count = %d, want exactly %d (T=%d, N=%d)", got, 66+T+N, T, N)
	}

	geminiAppendQualificationAggregate(t, path, verdict)
	finalVerdict, err := qualification.ValidateEvidence(path)
	if err != nil {
		t.Fatalf("qualification.ValidateEvidence() error = %v", err)
	}
	if finalVerdict != qualification.VerdictQualified {
		t.Errorf("final-pass verdict = %q, want %q", finalVerdict, qualification.VerdictQualified)
	}
	if got := geminiEvidenceLineCount(t, path); got != 67+T+N {
		t.Errorf("final record count = %d, want exactly %d", got, 67+T+N)
	}

	complete, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("qualification.ReadEvidenceFile() error = %v", err)
	}
	aggregate := complete[len(complete)-1]
	if aggregate.Scenario != qualification.ScenarioQualification || aggregate.Grade != qualification.GradeQualified {
		t.Errorf("final record = %s/%s, want the terminal qualified aggregate", aggregate.Scenario, aggregate.Grade)
	}
	qualificationCount := 0
	for _, rec := range complete {
		if rec.Scenario == qualification.ScenarioQualification {
			qualificationCount++
		}
	}
	if qualificationCount != 1 {
		t.Errorf("qualification record count = %d, want exactly 1", qualificationCount)
	}
}

// TestGeminiQualificationNotesRerunControl confirms the durable-notes
// control: a rerun reads but never mutates the tracked notes, matches
// them against the freshly validated summary, fails on a stale
// conclusion, and lets the initial run stop after the validated summary
// when the notes do not exist yet.
func TestGeminiQualificationNotesRerunControl(t *testing.T) {
	t.Parallel()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()
	conclusions, err := geminiSummaryConclusionsFromRecords(fixture.Records, qualification.VerdictQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}

	t.Run("the initial run may stop after the validated summary", func(t *testing.T) {
		t.Parallel()

		notesPath := filepath.Join(t.TempDir(), "gemini-adapter-notes.md")
		if geminiRunNotesConsistency(t, notesPath, conclusions, false) {
			t.Error("geminiRunNotesConsistency() = true with no notes file, want the initial run to stop after the summary")
		}
	})

	t.Run("a consistent rerun matches without editing the notes", func(t *testing.T) {
		t.Parallel()

		notesPath := writeGeminiNotesFile(t, geminiAdapterNotesFixture(conclusions))
		before, err := os.ReadFile(notesPath)
		if err != nil {
			t.Fatalf("read notes fixture: %v", err)
		}
		if !geminiRunNotesConsistency(t, notesPath, conclusions, true) {
			t.Error("geminiRunNotesConsistency() = false, want a matching rerun")
		}
		after, err := os.ReadFile(notesPath)
		if err != nil {
			t.Fatalf("read notes fixture after comparison: %v", err)
		}
		if string(before) != string(after) {
			t.Error("the notes-consistency check mutated the durable notes, want a read-only comparison")
		}
	})

	t.Run("a stale conclusion fails the rerun without editing the notes", func(t *testing.T) {
		t.Parallel()

		stale := strings.Replace(geminiAdapterNotesFixture(conclusions),
			"- protocol turn_disposition: Observed: usable with a bounded evidence shape",
			"- protocol turn_disposition: Not observed: not_observed with a bounded evidence shape", 1)
		notesPath := writeGeminiNotesFile(t, stale)
		before, err := os.ReadFile(notesPath)
		if err != nil {
			t.Fatalf("read stale notes fixture: %v", err)
		}

		compared, mismatch := geminiNotesConsistencyError(notesPath, conclusions)
		if !compared {
			t.Fatal("geminiNotesConsistencyError() compared = false, want the stale notes to be compared")
		}
		if mismatch == nil {
			t.Error("geminiNotesConsistencyError() mismatch = nil, want the stale conclusion rejected")
		}
		after, err := os.ReadFile(notesPath)
		if err != nil {
			t.Fatalf("read stale notes fixture after the check: %v", err)
		}
		if string(before) != string(after) {
			t.Error("the mismatch check mutated the durable notes, want a read-only comparison")
		}
	})
}

// errNativeProbeLaunchFailed marks a cmd.Start failure. A binary that
// never launched is a prerequisite the native path never reached, not
// evidence about what a run produced, so it is reported and recognized
// differently from a failure of the run itself: a non-zero exit or the
// bounded-wait timeout.
var errNativeProbeLaunchFailed = errors.New("native probe failed to launch")

// geminiRunNativeProbe launches one native surface's bounded probe with
// the qualification argv plus any documented per-input flags, the
// minimum environment allowlist, and the controlled workspace, captures
// its combined output, and drains its process group.
func geminiRunNativeProbe(t *testing.T, runtime geminiQualificationRuntime, surface qualification.Surface, prompt string, extraArgs ...string) (string, error) {
	t.Helper()

	argv := append(geminiQualificationLaunchArgv(runtime.Config, surface, prompt, runtime.PolicyPath), extraArgs...)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the operator-selected executable with the documented flags
	cmd.Dir = runtime.Workspace.Checkout
	cmd.Env = runtime.Env
	// The probe leads its own group, so the drain below reaches the
	// tree it forked. Without this the leader inherits this test's
	// group and the signal names a group the probe does not lead.
	procutil.SetProcessGroup(cmd)

	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return output.String(), fmt.Errorf("native launch: %w: %w", errNativeProbeLaunchFailed, err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
		return output.String(), waitErr
	case <-time.After(geminiNativeProbeBound):
		// The wait goroutine owns the only reap; the bounded wait
		// drains the group and then collects that one result rather
		// than racing a second wait on the same process.
		_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
		<-done
		return output.String(), errors.New("the native probe exceeded its bounded wait")
	}
}

// geminiNativeTerminal recognizes one native surface's terminal outcome
// from its output. A text surface carries no terminal member this
// harness recognizes and is characterized as unstructured. A launch or
// run failure is classified before either structured recognizer runs.
// Each structured surface is recognized only by the one terminal shape
// a live probe of the installed runtime was directly observed to
// produce on that surface: native_json by its response member,
// native_stream_json by its own type: "result" event; anything else
// reports no distinct outcome.
//
// A declaration's residual exposure has a second cause beyond an
// unreliable probe prompt: geminiNativeJSONTerminal or
// geminiNativeStreamTerminal failing to map the runtime's actual
// terminal shape. A refusal reported under a shape outside what either
// recognizer reads falls through unrecognized on every surface. The
// same bounds apply as for the prompt cause (the all-excluded rejection
// when every case of a surface-capability is excluded, and the
// notes-consistency rerun), and the remedy for this cause is widening
// one of the two recognizers, not the probe's prompt.
func geminiNativeTerminal(surface qualification.Surface, output string, launchErr error) (geminiProtocolTurnOutcome, bool) {
	if surface == qualification.SurfaceNativeText {
		// Unstructured residue is characterized, never graded usable.
		return geminiProtocolTurnOutcome{}, false
	}
	if launchErr != nil {
		if errors.Is(launchErr, errNativeProbeLaunchFailed) {
			// A binary that never launched carries no case-specific
			// terminal signal to recognize.
			return geminiProtocolTurnOutcome{}, false
		}
		// A bounded exit or timeout is the process ending without a
		// recognizable terminal member: transport loss for the retry
		// classification only when the surface said nothing else.
		return geminiProtocolTurnOutcome{ErrKind: domain.ErrPortExit}, true
	}
	switch surface {
	case qualification.SurfaceNativeJSON:
		return geminiNativeJSONTerminal(output)
	case qualification.SurfaceNativeStreamJSON:
		return geminiNativeStreamTerminal(output)
	}
	return geminiProtocolTurnOutcome{}, false
}

// geminiNativeJSONTerminal recognizes native_json's one observed
// terminal shape: a single document. Neither native surface has ever
// been observed to carry stopReason, stop_reason, or is_error, and a
// document that matches neither response-shaped nor error-shaped stays
// unstructured rather than guessed at.
func geminiNativeJSONTerminal(output string) (geminiProtocolTurnOutcome, bool) {
	values := geminiDecodeNativeJSONValues(output)
	if len(values) == 0 {
		return geminiProtocolTurnOutcome{}, false
	}
	object, ok := values[0].(map[string]any)
	if !ok {
		return geminiProtocolTurnOutcome{}, false
	}
	if _, hasError := object["error"]; hasError {
		// A defensive branch with no observed trigger on this surface,
		// kept for symmetry with the stream path's own error check.
		return geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError}, true
	}
	if _, hasResponse := object["response"]; hasResponse {
		return geminiProtocolTurnOutcome{StopReason: stopReasonEndTurn}, true
	}
	return geminiProtocolTurnOutcome{}, false
}

// geminiNativeStreamTerminal recognizes native_stream_json's own
// terminal event: the one decoded object whose type member equals
// "result". Every other event is skipped regardless of what members it
// happens to carry. The found event's status member is read directly,
// never behind an incidental unmarshal failure on a different member,
// and mapped to the same closed outcome enum the harness uses
// elsewhere.
func geminiNativeStreamTerminal(output string) (geminiProtocolTurnOutcome, bool) {
	var result map[string]any
	found := false
	for _, value := range geminiDecodeNativeJSONValues(output) {
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if object["type"] == "result" {
			result, found = object, true
			break
		}
	}
	if !found {
		return geminiProtocolTurnOutcome{}, false
	}
	status, ok := result["status"].(string)
	if !ok {
		return geminiProtocolTurnOutcome{}, false
	}
	var outcome geminiProtocolTurnOutcome
	switch status {
	case "end_turn", "success", "completed":
		outcome.StopReason = stopReasonEndTurn
	case "refusal", "declined":
		outcome.ErrKind = domain.ErrTurnRefused
	case "cancelled", "canceled":
		outcome.ErrKind = domain.ErrTurnCancelled
	case "max_tokens", "max_requests", "limit":
		outcome.ErrKind = domain.ErrTurnFailed
	default:
		if _, hasError := result["error"]; !hasError {
			return geminiProtocolTurnOutcome{}, false
		}
		outcome.ErrKind = domain.ErrResponseError
	}
	return outcome, true
}

// geminiTokenLedger accumulates the token-bearing paths each surface's
// bounded live runs exposed, for the per-surface inventories.
type geminiTokenLedger struct {
	mu      sync.Mutex
	sources map[qualification.Surface][]geminiTokenSource
}

func newGeminiTokenLedger() *geminiTokenLedger {
	return &geminiTokenLedger{sources: map[qualification.Surface][]geminiTokenSource{}}
}

// add records one surface's observed token sources, dropping duplicate
// paths so the closed (surface, evidence_path) uniqueness key never
// repeats.
func (l *geminiTokenLedger) add(surface qualification.Surface, sources ...geminiTokenSource) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	for _, source := range l.sources[surface] {
		seen[source.Path] = true
	}
	for _, source := range sources {
		if source.Path == "" || seen[source.Path] {
			continue
		}
		seen[source.Path] = true
		l.sources[surface] = append(l.sources[surface], source)
	}
}

// sorted returns one surface's collected sources in path order, so the
// records land in the canonical token order.
func (l *geminiTokenLedger) sorted(surface qualification.Surface) []geminiTokenSource {
	l.mu.Lock()
	defer l.mu.Unlock()
	sources := append([]geminiTokenSource(nil), l.sources[surface]...)
	slices.SortFunc(sources, func(a, b geminiTokenSource) int { return strings.Compare(a.Path, b.Path) })
	return sources
}

// geminiCollectContinuationRecords runs every surface's continuation
// seed and recall and returns their eight records.
func geminiCollectContinuationRecords(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, ledger *geminiTokenLedger, agentName string) []qualification.Record {
	t.Helper()

	records := []qualification.Record{}
	seed, recall := geminiRunProtocolContinuation(t, runtime, tracker, ledger, agentName)
	records = append(records, seed, recall)
	for _, surface := range []qualification.Surface{qualification.SurfaceNativeText, qualification.SurfaceNativeJSON, qualification.SurfaceNativeStreamJSON} {
		seed, recall := geminiRunNativeContinuation(t, runtime, surface)
		records = append(records, seed, recall)
	}
	return records
}

// geminiRunProtocolContinuation runs the protocol surface's
// continuation relation through the real adapter: a first process
// stores the generated nonce, that session ends, and a second process
// resumes through the adapter's own continuation route with its
// invented-method control and replay requirement. The behavioral oracle
// is the recall turn returning the nonce.
func geminiRunProtocolContinuation(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, ledger *geminiTokenLedger, agentName string) (seed, recall qualification.Record) {
	t.Helper()

	nonce := geminiNonce(t)
	catalog := geminiSemanticInputCatalog(runtime.Probes, nonce)
	adapter := &ClientProtocolAdapter{}
	command := strings.Join(geminiQualificationLaunchArgv(runtime.Config, qualification.SurfaceProtocol, "", runtime.PolicyPath), " ")
	startParams := func(resumeID string) domain.StartSessionParams {
		return domain.StartSessionParams{
			WorkspacePath: runtime.Workspace.Checkout,
			AgentConfig: domain.AgentConfig{
				Kind:           "agent-client-protocol",
				Command:        command,
				ReadTimeoutMS:  runtime.Timeouts.ReadTimeoutMS,
				TurnTimeoutMS:  runtime.Timeouts.TurnTimeoutMS,
				StallTimeoutMS: runtime.Timeouts.StallTimeoutMS,
			},
			ResumeSessionID: resumeID,
		}
	}

	relation := geminiContinuationRelation{Surface: qualification.SurfaceProtocol}
	seedSession, err := adapter.StartSession(context.Background(), startParams(""))
	if err != nil {
		return geminiLiveContinuationRecords(relation, agentName, runtime.Version)
	}
	tracker.register(geminiSessionGroupPID(seedSession))
	t.Cleanup(func() {
		_ = adapter.StopSession(context.Background(), seedSession)
	})
	var seedEvents []domain.AgentEvent
	seedResult, seedErr := adapter.RunTurn(context.Background(), seedSession, domain.RunTurnParams{
		Prompt:  catalog[qualification.InputContinuationSeed].Prompt,
		OnEvent: collectEvents(&seedEvents),
	})
	if seedErr == nil && seedResult.UsageMeasured {
		ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
			Path: "/turn/result/usage", Usage: &seedResult.Usage, SessionID: seedSession.ID,
		})
	}
	relation.SeedSessionID = seedSession.ID
	if seedErr != nil {
		return geminiLiveContinuationRecords(relation, agentName, runtime.Version)
	}
	if err := adapter.StopSession(context.Background(), seedSession); err != nil {
		t.Errorf("stop the protocol continuation seed session: %v", err)
	}

	recallSession, err := adapter.StartSession(context.Background(), startParams(seedSession.ID))
	if err != nil {
		return geminiLiveContinuationRecords(relation, agentName, runtime.Version)
	}
	tracker.register(geminiSessionGroupPID(recallSession))
	t.Cleanup(func() {
		_ = adapter.StopSession(context.Background(), recallSession)
	})
	var recallEvents []domain.AgentEvent
	recallResult, recallErr := adapter.RunTurn(context.Background(), recallSession, domain.RunTurnParams{
		Prompt:  catalog[qualification.InputContinuationRecall].Prompt,
		OnEvent: collectEvents(&recallEvents),
	})
	if recallErr == nil && recallResult.UsageMeasured {
		ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
			Path: "/turn/result/usage", Usage: &recallResult.Usage, SessionID: recallSession.ID,
		})
	}
	relation.RecallSessionID = recallSession.ID
	relation.ReplayConfirmed = recallErr == nil && geminiEventsCarry(recallEvents, nonce)
	if !relation.ReplayConfirmed && relation.RecallSessionID == relation.SeedSessionID {
		// Equal ids without the behavioral oracle stay honestly
		// unobserved rather than a fresh fallback with equal ids, which
		// the closed relation rejects.
		relation.RecallSessionID = ""
	}
	return geminiLiveContinuationRecords(relation, agentName, runtime.Version)
}

// geminiRunNativeContinuation runs one native surface's continuation
// relation: the seed process carries a test-generated --session-id and
// stores the nonce, that process ends, and a second process resumes
// with the documented --resume latest selector. The recall's actual
// identifier is the runtime-observed one, never a copy of the requested
// id, and the behavioral oracle is the nonce's return in the output.
func geminiRunNativeContinuation(t *testing.T, runtime geminiQualificationRuntime, surface qualification.Surface) (seed, recall qualification.Record) {
	t.Helper()

	nonce := geminiNonce(t)
	catalog := geminiSemanticInputCatalog(runtime.Probes, nonce)
	generated := geminiGeneratedNativeSessionID(t)
	nativeName := filepath.Base(runtime.Config.CommandPath)

	seedOutput, seedErr := geminiRunNativeProbe(t, runtime, surface, catalog[qualification.InputContinuationSeed].Prompt,
		"--session-id", generated)
	if seedErr != nil {
		return geminiLiveContinuationRecords(geminiContinuationRelation{Surface: surface}, nativeName, runtime.Version)
	}
	seedID := geminiNativeSessionID(surface, seedOutput)
	if seedID == "" {
		seedID = generated
	}

	recallOutput, recallErr := geminiRunNativeProbe(t, runtime, surface, catalog[qualification.InputContinuationRecall].Prompt,
		"--resume", "latest")
	relation := geminiContinuationRelation{Surface: surface, SeedSessionID: seedID}
	if recallErr != nil {
		return geminiLiveContinuationRecords(relation, nativeName, runtime.Version)
	}
	relation.RecallSessionID = geminiNativeSessionID(surface, recallOutput)
	relation.ReplayConfirmed = strings.Contains(recallOutput, nonce)
	if !relation.ReplayConfirmed && relation.RecallSessionID == relation.SeedSessionID {
		// Equal ids without the behavioral oracle stay honestly
		// unobserved rather than a fresh fallback with equal ids, which
		// the closed relation rejects.
		relation.RecallSessionID = ""
	}
	return geminiLiveContinuationRecords(relation, nativeName, runtime.Version)
}

// geminiGeneratedNativeSessionID generates one public test session
// identifier (an RFC 4122 version 4 UUID) for a native continuation
// seed launch.
func geminiGeneratedNativeSessionID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		t.Fatalf("generate the native seed session identifier: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// geminiIdentityRecordsForReferenced emits exactly one runtime-identity
// record for every distinct non-null actual protocol session id the
// non-final records reference, built from the session's own structured
// implementation log, and none for any other session.
func geminiIdentityRecordsForReferenced(t *testing.T, capture *geminiLogCapture, records []qualification.Record, agentName, agentVersion string) []qualification.Record {
	t.Helper()

	referenced := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == qualification.SurfaceProtocol && rec.SessionID != nil {
			referenced[*rec.SessionID] = true
		}
	}
	ids := slices.Collect(maps.Keys(referenced))
	slices.Sort(ids)
	out := []qualification.Record{}
	for _, id := range ids {
		name, version, ok := capture.sessionFacts(id)
		if !ok {
			// A missing agentInfo stays an explicit identity gap: the
			// record exists, its handshake facts stay null, and the
			// classification is not usable.
			rec := qualification.IdentityFixtureRecord(id)
			rec.AgentName = nil
			rec.AgentVersion = nil
			rec.Grade = qualification.GradeNotObserved
			rec.Outcome = qualification.OutcomeNotObserved
			rec.Detail = "the session's implementation log carried no handshake identity"
			out = append(out, rec)
			continue
		}
		if name == "" {
			name = agentName
		}
		if version == "" {
			version = agentVersion
		}
		out = append(out, geminiIdentityRecord(id, name, version, 1))
	}
	return out
}

// geminiRunAuthenticationCanary runs one bounded headless canary in the
// isolated configuration home and distinguishes usable authentication
// from a model call failure without printing any credential name or
// value. A canary that does not produce its marker is a prerequisite
// failure of the enabled gate.
func geminiRunAuthenticationCanary(t *testing.T, runtime geminiQualificationRuntime) {
	t.Helper()

	output, err := geminiRunNativeProbe(t, runtime, qualification.SurfaceNativeText,
		"Reply with exactly SORTIE_AUTH_OK and do not call any tool.")
	if err == nil && strings.Contains(output, "SORTIE_AUTH_OK") {
		return
	}
	t.Fatal("the authentication canary did not produce its marker: usable authentication could not be distinguished from a model call failure, and no credential name or value is reported")
}

// TestGeminiQualification is the Unix live qualification test. With the
// gate disabled it skips with the one explicit reason. With the gate
// enabled it fails rather than skips on any missing prerequisite,
// fixture, observation, evidence, or cleanup failure, and computes and
// records the eligibility verdict with no manual override.
func TestGeminiQualification(t *testing.T) {
	config := requireGeminiQualification(t)
	result := collectGeminiQualification(t, config)
	if !slices.Contains(qualification.Verdicts, result.Verdict) {
		t.Fatalf("collectGeminiQualification() verdict = %q, want a member of the closed verdict set", result.Verdict)
	}
}

// collectGeminiQualification runs the complete live qualification: it
// resolves the coordinates once, proves the prerequisites, collects the
// bounded observations across all four surfaces, writes and validates
// the two-pass JSONL evidence, prints the bounded summary, compares the
// durable notes when they exist, and applies the exact process-group
// absence oracle to every captured group. Any enabled-prerequisite,
// fixture, evidence, or cleanup failure fails the test; nothing here
// ever skips.
func collectGeminiQualification(t *testing.T, config geminiQualificationConfig) geminiQualificationResult {
	t.Helper()

	// Every adapter session's structured implementation log feeds the
	// runtime-identity records; the capture never mutates production
	// logging beyond restoring the previous default handler.
	capture := &geminiLogCapture{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	runtime := geminiResolveQualificationRuntime(t, config)
	tracker := &geminiProcessGroupTracker{}
	ledger := newGeminiTokenLedger()
	dir := geminiEvidenceDir(t)

	// The authentication canary is the live credential prerequisite.
	geminiRunAuthenticationCanary(t, runtime)

	// Workspace security is recorded first, from fixture facts only.
	workspaceRecord := geminiWorkspaceSecurityRecord(geminiWorkspaceSecurityFacts{
		PresentProjectConfig: geminiPresentProjectGeminiConfig(runtime.Workspace.Checkout),
		HomeClass:            "run_scoped_temp",
		CLIHomeClass:         "run_scoped_temp",
		EnvNames:             config.EnvAllowlist,
		SkipTrustUsed:        true,
		TrustPromptObserved:  false,
		MemberBasenames:      append([]string{filepath.Base(config.CommandPath)}, runtime.Probes.names()...),
	})

	// The protocol surface's control records: policy precondition,
	// permission, and MCP receipt.
	protocolRecords := geminiCollectProtocolRecords(t, runtime, tracker, ledger)
	agentName := geminiHandshakeAgentName(capture, filepath.Base(config.CommandPath))

	// The four surfaces' semantic probes and derived baselines.
	surfaceRecords := geminiCollectSurfaceRecords(t, runtime, tracker, ledger)

	// Every surface's continuation seed and recall.
	continuationRecords := geminiCollectContinuationRecords(t, runtime, tracker, ledger, agentName)

	// The per-surface token inventories, one record per observed path
	// or the single sentinel.
	tokenRecords := []qualification.Record{}
	for _, surface := range qualification.MeasuredSurfaces {
		tokenRecords = append(tokenRecords, geminiLiveTokenInventoryRecords(
			surface, ledger.sorted(surface), false, agentName, runtime.Version)...)
	}

	// The isolated end-to-end run with the real protocol adapter.
	e2eRecord := geminiCollectEndToEndRecord(t, runtime, agentName)

	// The bounded process-group cleanup oracle.
	cleanupRecord := geminiCollectCleanupRecord(t, tracker)

	observations := slices.Concat(
		[]qualification.Record{workspaceRecord},
		protocolRecords,
		surfaceRecords,
		tokenRecords,
		continuationRecords,
		[]qualification.Record{e2eRecord, cleanupRecord},
	)

	// The derived baselines follow their own complete observations.
	records := slices.Concat(observations, geminiBaselineRecords(t, observations))

	// One runtime-identity record per referenced actual protocol
	// session, from each session's own structured log.
	records = append(records, geminiIdentityRecordsForReferenced(t, capture, records, agentName, runtime.Version)...)

	verdict, summary, count := geminiRunTwoPassEvidenceFlow(t, dir, records, runtime.Config.DeclaredGaps)
	t.Logf("%s", summary)

	// On a rerun the durable notes must match the fresh summary; an
	// initial run without notes yet stops after the validated summary.
	notesPath, notesErr := filepath.Abs(geminiAdapterNotesRelPath)
	if notesErr == nil {
		geminiRunNotesConsistency(t, notesPath, geminiFreshConclusions(t, dir, verdict), false)
	}

	return geminiQualificationResult{
		Verdict:         verdict,
		Summary:         summary,
		EvidenceRecords: count,
	}
}

// geminiFreshConclusions derives the bounded conclusions from the
// validated evidence file.
func geminiFreshConclusions(t *testing.T, dir string, verdict qualification.Verdict) geminiSummaryConclusions {
	t.Helper()
	complete, err := qualification.ReadEvidenceFile(filepath.Join(dir, "evidence.jsonl"))
	if err != nil {
		t.Fatalf("read the validated evidence file: %v", err)
	}
	conclusions, err := geminiSummaryConclusionsFromRecords(complete[:len(complete)-1], verdict)
	if err != nil {
		t.Fatalf("derive the bounded conclusions: %v", err)
	}
	return conclusions
}

// geminiCollectProtocolRecords collects the protocol surface's policy
// precondition, permission, MCP, continuation, and runtime-identity
// records through the real generic adapter, from live bounded turns.
func geminiCollectProtocolRecords(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, ledger *geminiTokenLedger) []qualification.Record {
	t.Helper()

	catalog := geminiSemanticInputCatalog(runtime.Probes, geminiNonce(t))
	adapter := &ClientProtocolAdapter{}
	command := strings.Join(geminiQualificationLaunchArgv(runtime.Config, qualification.SurfaceProtocol, "", runtime.PolicyPath), " ")
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: runtime.Workspace.Checkout,
		AgentConfig: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        command,
			ReadTimeoutMS:  runtime.Timeouts.ReadTimeoutMS,
			TurnTimeoutMS:  runtime.Timeouts.TurnTimeoutMS,
			StallTimeoutMS: runtime.Timeouts.StallTimeoutMS,
		},
		MCPConfigPath: runtime.MCP.ConfigPath,
	})
	if err != nil {
		t.Fatalf("start the protocol control session: %v", err)
	}
	tracker.register(geminiSessionGroupPID(session))
	t.Cleanup(func() {
		if err := adapter.StopSession(context.Background(), session); err != nil {
			t.Errorf("stop the protocol control session: %v", err)
		}
	})

	var events []domain.AgentEvent
	runProbe := func(prompt string) (domain.TurnResult, error) {
		return adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  prompt,
			OnEvent: collectEvents(&events),
		})
	}

	// The policy precondition: one turn instructing run_shell_command
	// with the policy-load probe. The precondition passes only when the
	// captured tool result carries the deny marker and the probe's
	// side-effect file stays absent.
	policySessionID := session.ID
	policyResult, policyErr := runProbe(catalog[qualification.InputPolicyControl].Prompt)
	if policyErr == nil && policyResult.UsageMeasured {
		ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
			Path: "/turn/result/usage", Usage: &policyResult.Usage, SessionID: session.ID,
		})
	}
	policyObserved := policyErr == nil &&
		geminiEventsCarry(events, runtime.PolicyDenyMarker) &&
		geminiProbeMarkerAbsent(t, runtime.Probes.PolicyLoad)
	policyRecord := qualification.Record{
		SchemaVersion: 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioPolicyPrecondition,
		Surface:       qualification.SurfaceAggregate,
		Capability:    qualification.CapabilityPermissionHandling,
		Source:        qualification.SourceProcessObservation,
		InputID:       qualification.InputPolicyControl,
		EvidencePath:  new("policy.deny_marker"),
		SessionID:     new(policySessionID),
		AgentVersion:  new(runtime.Version),
	}
	switch {
	case policyResultExitFailed(policyErr):
		policyRecord.Outcome = qualification.OutcomePrerequisiteFailed
		policyRecord.Grade = qualification.GradeNotObserved
		policyRecord.Detail = "the policy control turn did not complete"
	case policyObserved:
		policyRecord.Outcome = qualification.OutcomePass
		policyRecord.Grade = qualification.GradeUsable
		policyRecord.Detail = "policy deny marker returned and the probe side effect stayed absent"
	default:
		policyRecord.Outcome = qualification.OutcomeFixtureInductionFailed
		policyRecord.Grade = qualification.GradeNotObserved
		policyRecord.Detail = "the policy control tool call was not induced"
	}

	// The permission probe: exactly one attempt, no retry until a
	// desired result appears.
	permissionResult, permissionErr := runProbe(catalog[qualification.InputPermissionProbe].Prompt)
	if permissionErr == nil && permissionResult.UsageMeasured {
		ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
			Path: "/turn/result/usage", Usage: &permissionResult.Usage, SessionID: session.ID,
		})
	}
	permissionRequested := geminiPermissionRequested(events)
	permissionAnswered := geminiPermissionAnswered(events)
	permissionRecord := qualification.Record{
		SchemaVersion:   1,
		ObservedAt:      qualification.FixtureTime,
		Scenario:        qualification.ScenarioPermissionRequest,
		Surface:         qualification.SurfaceProtocol,
		Capability:      qualification.CapabilityPermissionHandling,
		Source:          qualification.SourceProtocolStable,
		InputID:         qualification.InputPermissionProbe,
		EvidencePath:    new("session/request_permission"),
		SessionID:       new(session.ID),
		AgentName:       new(filepath.Base(runtime.Config.CommandPath)),
		AgentVersion:    new(runtime.Version),
		ProtocolVersion: new(1),
	}
	switch {
	case permissionErr != nil:
		permissionRecord.Outcome = qualification.OutcomePrerequisiteFailed
		permissionRecord.Grade = qualification.GradeNotObserved
		permissionRecord.Detail = "the permission control turn did not complete"
	case !permissionRequested:
		permissionRecord.Outcome = qualification.OutcomeFixtureInductionFailed
		permissionRecord.Grade = qualification.GradeNotObserved
		permissionRecord.Detail = "no permission request was emitted for the controlled operation"
	case !permissionAnswered:
		permissionRecord.Outcome = qualification.OutcomeAdapterUnanswered
		permissionRecord.Grade = qualification.GradeNotObserved
		permissionRecord.Detail = "the request was captured and no correlated client response went back for it"
	default:
		permissionRecord.Outcome = qualification.OutcomePass
		permissionRecord.Grade = qualification.GradeUsable
		permissionRecord.Detail = "request answered with a refusing option and no request left pending"
	}

	// The MCP probe: delivery is graded only on server receipt plus the
	// turn consuming the returned nonce.
	mcpResult, mcpErr := runProbe(catalog[qualification.InputMCPProbe].Prompt)
	if mcpErr == nil && mcpResult.UsageMeasured {
		ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
			Path: "/turn/result/usage", Usage: &mcpResult.Usage, SessionID: session.ID,
		})
	}
	received := geminiReadMCPReceipt(t, runtime.MCP)
	turnConsumed := mcpErr == nil && geminiEventsCarry(events, runtime.MCP.Nonce)
	mcpRecord := qualification.Record{
		SchemaVersion:   1,
		ObservedAt:      qualification.FixtureTime,
		Scenario:        qualification.ScenarioToolServer,
		Surface:         qualification.SurfaceProtocol,
		Capability:      qualification.CapabilityToolServerDelivery,
		Source:          qualification.SourceProcessObservation,
		Grade:           geminiGradeMCPDelivery(received, runtime.MCP.Nonce, turnConsumed),
		Outcome:         qualification.OutcomePass,
		InputID:         qualification.InputMCPProbe,
		EvidencePath:    new("mcp_server.receipt"),
		SessionID:       new(session.ID),
		AgentName:       new(filepath.Base(runtime.Config.CommandPath)),
		AgentVersion:    new(runtime.Version),
		ProtocolVersion: new(1),
		Detail:          "test server receipt and the turn's returned nonce",
	}
	if got := geminiGradeMCPDelivery(received, runtime.MCP.Nonce, turnConsumed); got != qualification.GradeUsable {
		mcpRecord.Outcome = qualification.OutcomeNotObserved
		mcpRecord.Detail = "no server receipt or unconsumed returned nonce"
	}

	return []qualification.Record{policyRecord, permissionRecord, mcpRecord}
}

// geminiSessionGroupPID returns the session's process-group leader PID.
// procutil.SetProcessGroup makes the launched leader's PID the PGID.
func geminiSessionGroupPID(session domain.Session) int {
	pid, err := strconv.Atoi(session.AgentPID)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// geminiEventsCarry reports whether any event message carries needle.
func geminiEventsCarry(events []domain.AgentEvent, needle string) bool {
	return slices.ContainsFunc(events, func(event domain.AgentEvent) bool {
		return strings.Contains(event.Message, needle)
	})
}

// TestGeminiPermissionNoticeStems pins the permission scenario's two
// observations to the notices the shared human-request decision
// actually produces. A renamed notice reddens here instead of quietly
// collapsing the requested and answered readings into one and making
// the unanswered verdict unreachable.
func TestGeminiPermissionNoticeStems(t *testing.T) {
	t.Parallel()

	answered := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
	unanswered := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerPending)

	if !answered.Transmit {
		t.Fatal("the answering posture Transmit = false, want a correlated reply going back")
	}
	if unanswered.Transmit {
		t.Fatal("the unanswered posture Transmit = true, want no correlated reply")
	}
	if !strings.Contains(answered.Notice, geminiPermissionNoticeStem) {
		t.Errorf("answered notice = %q, want it to carry %q", answered.Notice, geminiPermissionNoticeStem)
	}
	if !strings.Contains(unanswered.Notice, geminiPermissionUnansweredStem) {
		t.Errorf("unanswered notice = %q, want it to carry %q", unanswered.Notice, geminiPermissionUnansweredStem)
	}
	if strings.Contains(unanswered.Notice, geminiPermissionNoticeStem) {
		t.Errorf("unanswered notice = %q carries the answered stem, want the two observations distinct", unanswered.Notice)
	}

	captured := []domain.AgentEvent{{Type: domain.EventNotification, Message: unanswered.Notice}}
	if !geminiPermissionRequested(captured) {
		t.Error("geminiPermissionRequested() = false for a captured request, want true")
	}
	if geminiPermissionAnswered(captured) {
		t.Error("geminiPermissionAnswered() = true with no correlated reply, want false")
	}
}

// geminiProbeMarkerAbsent reports whether a probe's side-effect file is
// absent, the no-side-effect condition the policy precondition and the
// permission scenarios require.
func geminiProbeMarkerAbsent(t *testing.T, probePath string) bool {
	t.Helper()
	if _, err := os.Stat(geminiProbeMarkerPath(probePath)); errors.Is(err, os.ErrNotExist) {
		return true
	}
	return false
}

// geminiPermissionNoticeStem is the observable stem of the notice the
// adapter emits when it answers a permission request with a refusing
// option, which is the message-shape observation of a correlated reply
// going back to the runtime.
const geminiPermissionNoticeStem = "refused a permission request"

// geminiPermissionUnansweredStem is the observable stem of the notice
// the adapter emits instead when the request carried no option it
// could answer with. The request reached the client and ended the
// attempt, and no correlated reply went back, so this stem observes a
// request without an answer.
const geminiPermissionUnansweredStem = "needs a permission this unattended run cannot grant"

// geminiPermissionRequested reports whether a permission request
// reached the adapter during the turn, read through either notice the
// adapter emits for one. Both postures observe the request; only one
// of them observes an answer, which is why the two predicates read
// different stems.
func geminiPermissionRequested(events []domain.AgentEvent) bool {
	return geminiEventsCarry(events, geminiPermissionNoticeStem) ||
		geminiEventsCarry(events, geminiPermissionUnansweredStem)
}

// geminiPermissionAnswered reports whether a correlated reply went back
// for the request, which only the answering posture's notice observes.
func geminiPermissionAnswered(events []domain.AgentEvent) bool {
	return geminiEventsCarry(events, geminiPermissionNoticeStem)
}

// policyResultExitFailed reports whether the policy-control turn ended
// in an error the precondition cannot be read from.
func policyResultExitFailed(err error) bool {
	return err != nil
}

// geminiCollectSurfaceRecords collects the four surfaces' semantic
// probes, derived baselines, and token inventory inputs in canonical
// order.
func geminiCollectSurfaceRecords(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, ledger *geminiTokenLedger) []qualification.Record {
	t.Helper()

	catalog := geminiSemanticInputCatalog(runtime.Probes, geminiNonce(t))
	records := []qualification.Record{}
	refusalRuns := map[qualification.Surface]geminiSemanticObservation{}

	for _, surface := range qualification.MeasuredSurfaces {
		for _, capability := range []qualification.Capability{qualification.CapabilityTurnDisposition, qualification.CapabilityRetryClassification} {
			for _, caseID := range qualification.CapabilityCases[capability] {
				var obs geminiSemanticObservation
				if capability == qualification.CapabilityRetryClassification && caseID == qualification.CaseNonRetryableRefusal {
					// The non-retryable-refusal retry record references
					// the same physical run as its surface's refusal
					// disposition record; it is never a second launch.
					refusalObs := refusalRuns[surface]
					refusalObs.Tag = qualification.CaseNonRetryableRefusal
					obs = refusalObs
				} else {
					obs = geminiCollectSemanticCase(t, runtime, tracker, surface, caseID, catalog, ledger)
					if capability == qualification.CapabilityTurnDisposition && caseID == qualification.CaseRuntimeRefusal {
						refusalRuns[surface] = obs
					}
				}
				records = append(records, geminiSemanticRecordFor(t, surface, capability, caseID, obs, runtime.Config.DeclaredGaps))
			}
		}
	}
	return records
}

// geminiBaselineRecords derives all 16 per-surface comparison baselines
// from their own complete observations, so a written baseline can never
// differ from its derivation.
func geminiBaselineRecords(t *testing.T, prior []qualification.Record) []qualification.Record {
	t.Helper()

	records := []qualification.Record{}
	for _, surface := range qualification.MeasuredSurfaces {
		for _, capability := range qualification.ComparisonCapabilities {
			grade := geminiDeriveBaseline(t, surface, capability, prior)
			records = append(records, qualification.Record{
				SchemaVersion: 1,
				ObservedAt:    qualification.FixtureTime,
				Scenario:      qualification.ScenarioSurfaceBaseline,
				Surface:       surface,
				Capability:    capability,
				Source:        qualification.SourceComparison,
				Grade:         grade,
				Outcome:       qualification.BaselineVerdictFor(grade),
				InputID:       qualification.InputBaseline,
				EvidencePath:  new(fmt.Sprintf("/comparison/%s/%s", surface, capability)),
				Detail:        fmt.Sprintf("derived %s grade for %s", capability, surface),
			})
		}
	}
	return records
}

// geminiCollectSemanticCase launches one case's bounded probe on one
// surface and returns its observation. A case with no CaseInputs entry
// or no catalog entry fails the run: that is a harness defect, never a
// fact about the catalog, so it must never be marked NoInducer.
func geminiCollectSemanticCase(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, surface qualification.Surface, caseID qualification.Case, catalog map[qualification.InputID]geminiInputSpec, ledger *geminiTokenLedger) geminiSemanticObservation {
	t.Helper()

	inputID, known := qualification.CaseInputs[caseID]
	if !known {
		t.Fatalf("semantic case %s carries no CaseInputs entry", caseID)
	}
	spec, inCatalog := catalog[inputID]
	if !inCatalog {
		t.Fatalf("semantic case %s maps to input %s, which the catalog omits", caseID, inputID)
	}
	if !spec.Launch {
		// limit_reached and the refusal-retry reference: the catalog
		// declares no probe for this case at all.
		return geminiSemanticObservation{Tag: caseID, NoInducer: true}
	}

	switch surface {
	case qualification.SurfaceProtocol:
		return geminiInduceProtocolCase(t, runtime, tracker, caseID, catalog, ledger)
	default:
		return geminiInduceNativeCase(t, runtime, surface, caseID, catalog, ledger)
	}
}

// geminiInduceProtocolCase runs one protocol case's live probe through
// the adapter's own session lifecycle.
func geminiInduceProtocolCase(t *testing.T, runtime geminiQualificationRuntime, tracker *geminiProcessGroupTracker, caseID qualification.Case, catalog map[qualification.InputID]geminiInputSpec, ledger *geminiTokenLedger) geminiSemanticObservation {
	t.Helper()

	adapter := &ClientProtocolAdapter{}
	command := strings.Join(geminiQualificationLaunchArgv(runtime.Config, qualification.SurfaceProtocol, "", runtime.PolicyPath), " ")
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: runtime.Workspace.Checkout,
		AgentConfig: domain.AgentConfig{
			Command:        command,
			ReadTimeoutMS:  runtime.Timeouts.ReadTimeoutMS,
			TurnTimeoutMS:  runtime.Timeouts.TurnTimeoutMS,
			StallTimeoutMS: runtime.Timeouts.StallTimeoutMS,
		},
	})
	if err != nil {
		return geminiSemanticObservation{Tag: caseID, Failure: qualification.OutcomePrerequisiteFailed}
	}
	tracker.register(geminiSessionGroupPID(session))
	t.Cleanup(func() {
		if err := adapter.StopSession(context.Background(), session); err != nil {
			t.Errorf("stop the probe session: %v", err)
		}
	})

	observation := geminiSemanticObservation{Tag: caseID, SessionID: session.ID, EvidencePath: "/turn/stop_reason"}

	switch caseID {
	case qualification.CaseCancellation:
		// The marker proves the child is active; the single
		// cancellation is the adapter's own session/cancel through the
		// bounded context.
		probeCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		turnDone := make(chan turnOutcome, 1)
		go func() {
			var turnEvents []domain.AgentEvent
			result, err := adapter.RunTurn(probeCtx, session, domain.RunTurnParams{
				Prompt:  catalog[qualification.InputDispositionCancellation].Prompt,
				OnEvent: collectEvents(&turnEvents),
			})
			turnDone <- turnOutcome{result: result, err: err}
		}()
		waitForFile(t, geminiProbeMarkerPath(runtime.Probes.Cancellation), geminiSemanticProbeTimeout)
		cancel()
		outcome := <-turnDone
		if agentErr, ok := errors.AsType[*domain.AgentError](outcome.err); ok {
			observation.Induced = true
			observation.Distinct = agentErr.Kind == domain.ErrTurnCancelled
			observation.Structured = true
		} else if outcome.err == nil {
			observation.Induced = true
			observation.Distinct = false
			observation.Structured = true
		}
		return observation

	case qualification.CaseRetryableTransport:
		turnDone := make(chan turnOutcome, 1)
		go func() {
			var turnEvents []domain.AgentEvent
			result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
				Prompt:  catalog[qualification.InputRetryableTransport].Prompt,
				OnEvent: collectEvents(&turnEvents),
			})
			turnDone <- turnOutcome{result: result, err: err}
		}()
		waitForFile(t, geminiProbeMarkerPath(runtime.Probes.Transport), geminiSemanticProbeTimeout)
		_ = procutil.SignalProcessGroup(geminiSessionGroupPID(session), syscall.SIGKILL)
		outcome := <-turnDone
		if outcome.result.UsageMeasured {
			ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
				Path: "/turn/result/usage", Usage: &outcome.result.Usage, SessionID: session.ID,
			})
		}
		observation.Induced = true
		if agentErr, ok := outcome.err.(*domain.AgentError); ok && agentErr.Kind == domain.ErrPortExit {
			observation.Distinct = true
			observation.Structured = true
		}
		return observation

	default:
		var turnEvents []domain.AgentEvent
		turnResult, turnErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  catalog[qualification.CaseInputs[caseID]].Prompt,
			OnEvent: collectEvents(&turnEvents),
		})
		if turnErr == nil && turnResult.UsageMeasured {
			ledger.add(qualification.SurfaceProtocol, geminiTokenSource{
				Path: "/turn/result/usage", Usage: &turnResult.Usage, SessionID: session.ID,
			})
		}
		outcome := geminiProtocolTurnOutcome{Attributed: true}
		if agentErr, ok := errors.AsType[*domain.AgentError](turnErr); ok {
			outcome.ErrKind = agentErr.Kind
		}
		if caseID == qualification.CaseSuccess && turnErr == nil {
			outcome.StopReason = stopReasonEndTurn
		}
		if mapped, distinct := geminiProtocolCase(outcome); distinct && mapped == caseID {
			observation.Induced = true
			observation.Distinct = true
			observation.Structured = true
		}
		return observation
	}
}

// geminiInduceNativeCase launches one native surface's case probe and
// classifies the captured output, feeding any token-bearing structured
// member the output carries into the surface's inventory. Induced
// carries one meaning on the structured surfaces: the recognized
// terminal outcome maps to the case being probed, so it implies both
// Distinct and Structured there; on the text surface, which recognizes
// no terminal member by construction, it stays whether the probe ran
// at all.
func geminiInduceNativeCase(t *testing.T, runtime geminiQualificationRuntime, surface qualification.Surface, caseID qualification.Case, catalog map[qualification.InputID]geminiInputSpec, ledger *geminiTokenLedger) geminiSemanticObservation {
	t.Helper()

	inputID := qualification.CaseInputs[caseID]
	observation := geminiSemanticObservation{Tag: caseID, EvidencePath: "/output/terminal"}

	output, launchErr := geminiRunNativeProbe(t, runtime, surface, catalog[inputID].Prompt)
	nativeSession := geminiNativeSessionID(surface, output)
	ledger.add(surface, geminiScanNativeTokenOutput(surface, output, nativeSession)...)
	outcome, structured := geminiNativeTerminal(surface, output, launchErr)
	observation.Structured = structured

	mapped, distinct := geminiProtocolCase(outcome)
	caseOwnSignal := distinct && mapped == caseID && structured
	if caseOwnSignal {
		observation.Distinct = true
	}

	switch surface {
	case qualification.SurfaceNativeText:
		observation.Induced = launchErr == nil
	default:
		observation.Induced = caseOwnSignal
	}

	if launchErr != nil {
		switch {
		case errors.Is(launchErr, errNativeProbeLaunchFailed):
			// A missing or unlaunchable binary is a prerequisite the
			// run never reached, not evidence the run produced,
			// mirroring the protocol surface's own StartSession
			// failure.
			observation.Failure = qualification.OutcomePrerequisiteFailed
		case !caseOwnSignal:
			// A non-zero exit is the case's own terminal signal only
			// for retryable_transport; any other failed run is a
			// failed measurement.
			observation.Failure = qualification.OutcomeRuntimeFailed
		}
	}
	// The text surface is characterized as unstructured residue; every
	// induced text case grades gap through Structured=false.
	return observation
}

// geminiDeriveBaseline derives one capability's baseline grade for one
// surface from the records collected so far, so a written baseline can
// never differ from its own derivation.
func geminiDeriveBaseline(t *testing.T, surface qualification.Surface, capability qualification.Capability, records []qualification.Record) qualification.Grade {
	t.Helper()

	switch capability {
	case qualification.CapabilityTurnDisposition, qualification.CapabilityRetryClassification:
		var classes []qualification.Grade
		for _, caseID := range qualification.CapabilityCases[capability] {
			found := false
			for i := range records {
				rec := &records[i]
				if rec.Scenario == qualification.ScenarioSemanticProbe && rec.Surface == surface &&
					rec.Capability == capability && rec.SemanticCase != nil && *rec.SemanticCase == caseID {
					classes = append(classes, rec.Grade)
					found = true
					break
				}
			}
			if !found {
				return qualification.GradeNotObserved
			}
		}
		return qualification.DeriveBaselineGrade(classes)
	case qualification.CapabilityTokenCeiling:
		// The baseline follows the surface's complete inventory: a
		// consumable source grades usable, a completed inventory
		// without one grades gap, and a failed inventory grades
		// not_observed.
		completed := false
		for i := range records {
			rec := &records[i]
			if rec.Scenario != qualification.ScenarioTokenSource || rec.Surface != surface || rec.Capability != qualification.CapabilityTokenCeiling {
				continue
			}
			if rec.EvidencePath == nil {
				if rec.Grade == qualification.GradeNotObserved {
					return qualification.GradeNotObserved
				}
				completed = true
				continue
			}
			if rec.Grade == qualification.GradeUsable {
				return qualification.GradeUsable
			}
			completed = true
		}
		if completed {
			return qualification.GradeGap
		}
		return qualification.GradeNotObserved
	case qualification.CapabilitySessionContinuation:
		// The baseline follows the recall record's own classification.
		for i := range records {
			rec := &records[i]
			if rec.Scenario == qualification.ScenarioContinuation && rec.Surface == surface && rec.InputID == qualification.InputContinuationRecall {
				return rec.Grade
			}
		}
		return qualification.GradeNotObserved
	default:
		return qualification.GradeNotObserved
	}
}

// geminiCollectCleanupRecord applies the bounded absence oracle to
// every captured process group and builds the single cleanup record.
func geminiCollectCleanupRecord(t *testing.T, tracker *geminiProcessGroupTracker) qualification.Record {
	t.Helper()

	survivors := false
	for _, pgid := range tracker.groups {
		present, err := qualification.ProcessGroupPresent(pgid)
		if err != nil {
			return geminiProcessCleanupRecord(tracker.count(), true)
		}
		if present {
			_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
			if !geminiAwaitGroupDrain(t, pgid, qualification.ShutdownDeadline) {
				survivors = true
			}
		}
	}
	return geminiProcessCleanupRecord(tracker.count(), survivors)
}

// geminiCollectEndToEndRecord drives the isolated file-tracker E2E
// harness with the real protocol adapter and builds its record from the
// actual protocol session the harness's observer captured.
func geminiCollectEndToEndRecord(t *testing.T, runtime geminiQualificationRuntime, agentName string) qualification.Record {
	t.Helper()

	adapter := &ClientProtocolAdapter{}
	command := strings.Join(geminiQualificationLaunchArgv(runtime.Config, qualification.SurfaceProtocol, "", runtime.PolicyPath), " ")
	harness := e2e.NewHarnessWithAgent(t, adapter, command, "agent-client-protocol")
	cancel, runDone := e2e.StartWorkflow(t, harness)

	deadline := time.Now().Add(qualification.ShutdownDeadline)
	var condition e2e.TerminalCondition
	for {
		condition = e2e.ObserveTerminalCondition(t, harness)
		if condition.Reached() || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Stop the run and join its goroutine before the absence oracle
	// polls, so no still-dispatching orchestrator can keep a captured
	// group alive and drive groupClean false for a reason that is not
	// the agent's cleanup. A run that will not stop within the shared
	// bound is a cleanup failure, not an observation: the groups below
	// would be read while dispatch can still create more, so the
	// record built from them would grade the harness rather than the
	// runtime.
	cancel()
	select {
	case <-runDone:
	case <-time.After(qualification.ShutdownDeadline):
		t.Fatalf("qualification workflow still running %s after cancellation; cleanup cannot be observed", qualification.ShutdownDeadline)
	}

	groupClean := true
	for _, pgid := range harness.Agent().PGIDs() {
		if !geminiAwaitGroupDrain(t, pgid, qualification.ShutdownDeadline) {
			groupClean = false
		}
	}
	sessionID := ""
	if ids := harness.Agent().SessionIDs(); len(ids) > 0 {
		sessionID = ids[0]
	}
	return e2e.TerminalRecord(condition, groupClean, sessionID, agentName, runtime.Version)
}

// geminiHandshakeAgentName reports the runtime's handshake-reported
// name from the captured implementation log, falling back to the
// executable basename when no session logged its handshake.
func geminiHandshakeAgentName(capture *geminiLogCapture, fallback string) string {
	for _, entry := range capture.implementationRecords() {
		if name := entry.Attrs["name"]; name != "" {
			return name
		}
	}
	return fallback
}

// TestGeminiNativeProbeClassification exercises the harness helpers
// directly, with no qualification gate required: a cmd.Start failure
// never carries a case signal, a run failure keeps reaching its
// outcome (including the retryable_transport repair), the
// missing-CaseInputs-or-catalog-entry guard fails the run, and the
// DeclarableSurfaces recognizer binding holds.
func TestGeminiNativeProbeClassification(t *testing.T) {
	t.Parallel()

	t.Run("a cmd.Start failure never carries a case signal", func(t *testing.T) {
		t.Parallel()

		runtime := geminiQualificationRuntime{
			Config: geminiQualificationConfig{CommandPath: filepath.Join(t.TempDir(), "does-not-exist"), Model: "fixture-model"},
			Env:    []string{"PATH=" + os.Getenv("PATH")},
		}
		_, err := geminiRunNativeProbe(t, runtime, qualification.SurfaceNativeJSON, "prompt")
		if !errors.Is(err, errNativeProbeLaunchFailed) {
			t.Fatalf("geminiRunNativeProbe() error = %v, want it to satisfy errNativeProbeLaunchFailed", err)
		}

		catalog := map[qualification.InputID]geminiInputSpec{
			qualification.InputRetryableTransport: {Prompt: "probe", Launch: true},
		}
		observation := geminiInduceNativeCase(t, runtime, qualification.SurfaceNativeJSON, qualification.CaseRetryableTransport, catalog, newGeminiTokenLedger())
		if observation.Failure != qualification.OutcomePrerequisiteFailed {
			t.Errorf("observation.Failure = %s, want prerequisite_failed", observation.Failure)
		}
		if observation.Distinct {
			t.Error("observation.Distinct = true, want false: a missing binary carries no case-specific terminal signal, even though its recognized-outcome mapping would otherwise match")
		}
		if observation.Induced {
			t.Error("observation.Induced = true, want false for a launch failure")
		}
	})

	t.Run("a run failure keeps reaching its outcome, contrasted with a launch failure", func(t *testing.T) {
		t.Parallel()

		workspace := t.TempDir()
		script := agenttest.WriteScript(t, workspace, "run-failure-probe", "exit 7\n")
		runtime := geminiQualificationRuntime{
			Config:    geminiQualificationConfig{CommandPath: script, Model: "fixture-model"},
			Workspace: geminiQualificationWorkspace{Checkout: workspace},
			Env:       []string{"PATH=" + os.Getenv("PATH")},
		}

		_, err := geminiRunNativeProbe(t, runtime, qualification.SurfaceNativeJSON, "prompt")
		if err == nil {
			t.Fatal("geminiRunNativeProbe() = nil error, want a non-zero-exit run failure")
		}
		if errors.Is(err, errNativeProbeLaunchFailed) {
			t.Errorf("geminiRunNativeProbe() error = %v, want it NOT to satisfy errNativeProbeLaunchFailed for a run failure", err)
		}

		retryCatalog := map[qualification.InputID]geminiInputSpec{
			qualification.InputRetryableTransport: {Prompt: "probe", Launch: true},
		}
		repaired := geminiInduceNativeCase(t, runtime, qualification.SurfaceNativeJSON, qualification.CaseRetryableTransport, retryCatalog, newGeminiTokenLedger())
		if repaired.Failure != "" {
			t.Errorf("retryable_transport observation.Failure = %q, want empty: the repair maps a run failure onto the case's own terminal signal", repaired.Failure)
		}
		if !repaired.Induced || !repaired.Distinct || !repaired.Structured {
			t.Errorf("retryable_transport observation = %+v, want Induced, Distinct, and Structured all true", repaired)
		}
		rec := geminiSemanticRecordFor(t, qualification.SurfaceNativeJSON, qualification.CapabilityRetryClassification, qualification.CaseRetryableTransport, repaired, qualification.DeclaredGapSet{})
		if rec.Grade != qualification.GradeUsable || rec.Outcome != qualification.OutcomePass {
			t.Errorf("retryable_transport record = %s/%s, want usable/pass", rec.Grade, rec.Outcome)
		}

		successCatalog := map[qualification.InputID]geminiInputSpec{
			qualification.InputDispositionSuccess: {Prompt: "probe", Launch: true},
		}
		unmatched := geminiInduceNativeCase(t, runtime, qualification.SurfaceNativeJSON, qualification.CaseSuccess, successCatalog, newGeminiTokenLedger())
		if unmatched.Failure != qualification.OutcomeRuntimeFailed {
			t.Errorf("success observation.Failure = %s, want runtime_failed for a run failure whose outcome does not map to the case", unmatched.Failure)
		}
		unmatchedRec := geminiSemanticRecordFor(t, qualification.SurfaceNativeJSON, qualification.CapabilityTurnDisposition, qualification.CaseSuccess, unmatched, qualification.DeclaredGapSet{})
		if unmatchedRec.Grade != qualification.GradeNotObserved {
			t.Errorf("success record = %s, want not_observed", unmatchedRec.Grade)
		}
	})

	t.Run("the DeclarableSurfaces recognizer binding", func(t *testing.T) {
		t.Parallel()

		// geminiNativeTerminal returns the zero outcome for
		// SurfaceNativeText before it ever reads output, so varying the
		// output content proves nothing beyond this one call; the output
		// value here is an arbitrary structured-looking payload chosen to
		// show the surface check, not the content, decides the result.
		output := `{"session_id":"sess","response":{"text":"hi"},"stats":{}}`
		outcome, structured := geminiNativeTerminal(qualification.SurfaceNativeText, output, nil)
		if structured || outcome != (geminiProtocolTurnOutcome{}) {
			t.Errorf("geminiNativeTerminal(native_text, %q, nil) = %+v, %v, want the zero outcome and false", output, outcome, structured)
		}
		if slices.Contains(qualification.DeclarableSurfaces, qualification.SurfaceNativeText) {
			t.Error("DeclarableSurfaces contains SurfaceNativeText, want it excluded to match geminiNativeTerminal's non-recognition")
		}
	})
}

// geminiTestHelperProcessEnv gates the re-exec branch
// TestGeminiCollectSemanticCaseMissingEntryGuard spawns to observe a
// t.Fatalf call from outside the failing goroutine, since a failed
// subtest would otherwise mark this whole package's test run failed.
const geminiTestHelperProcessEnv = "CLIENTPROTOCOL_TEST_HELPER_PROCESS"

// TestGeminiCollectSemanticCaseMissingEntryGuard confirms
// geminiCollectSemanticCase fails the run, rather than returning an
// observation, for a case with no CaseInputs entry or whose mapped
// input_id has no catalog entry. The assertion runs in a re-exec'd
// subprocess: a t.Fatalf inside a subtest would otherwise mark this
// whole test binary's run failed, masking every other passing test.
func TestGeminiCollectSemanticCaseMissingEntryGuard(t *testing.T) {
	t.Parallel()

	if os.Getenv(geminiTestHelperProcessEnv) == "1" {
		geminiCollectSemanticCase(t, geminiQualificationRuntime{}, &geminiProcessGroupTracker{},
			qualification.SurfaceProtocol, qualification.Case("bogus-case"), map[qualification.InputID]geminiInputSpec{}, newGeminiTokenLedger())
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestGeminiCollectSemanticCaseMissingEntryGuard$", "-test.v")
	cmd.Env = append(os.Environ(), geminiTestHelperProcessEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("subprocess exited successfully, want a failure from the missing-CaseInputs guard; output:\n%s", output)
	}
	if !strings.Contains(string(output), "carries no CaseInputs entry") {
		t.Errorf("subprocess output = %s, want it to name the missing CaseInputs entry", output)
	}
}

// TestGeminiNativeJSONTerminal covers native_json's own observed
// terminal shape: a success document, an empty stdout (the shape every
// observed native_json failure path writes), and an error-bearing
// document.
func TestGeminiNativeJSONTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		output      string
		wantOutcome geminiProtocolTurnOutcome
		wantOK      bool
	}{
		{
			name:        "a success document recognizes end_turn",
			output:      `{"session_id":"sess-fixture","response":{"text":"hi"},"stats":{"input":1}}`,
			wantOutcome: geminiProtocolTurnOutcome{StopReason: stopReasonEndTurn},
			wantOK:      true,
		},
		{
			name:   "an empty stdout, the shape every observed failure path writes, recognizes nothing",
			output: "",
			wantOK: false,
		},
		{
			name:        "an error-bearing document recognizes ErrResponseError",
			output:      `{"error":{"message":"boom"}}`,
			wantOutcome: geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError},
			wantOK:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outcome, ok := geminiNativeJSONTerminal(tt.output)
			if ok != tt.wantOK || outcome != tt.wantOutcome {
				t.Errorf("geminiNativeJSONTerminal(%q) = %+v, %v, want %+v, %v", tt.output, outcome, ok, tt.wantOutcome, tt.wantOK)
			}
		})
	}
}

// TestGeminiNativeStreamTerminal covers native_stream_json's own
// type: "result" terminal event: the status mapping, the ordering
// fragility this file's own comment names (status read directly rather
// than gated behind an error unmarshal failure), and the imprecision
// this recognizer closes (only a type: "result" line is ever the
// terminal event, never an incidental status-named member elsewhere).
func TestGeminiNativeStreamTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		output      string
		wantOutcome geminiProtocolTurnOutcome
		wantOK      bool
	}{
		{
			name:        "a type result line with status success recognizes end_turn",
			output:      `{"type":"result","status":"success"}`,
			wantOutcome: geminiProtocolTurnOutcome{StopReason: stopReasonEndTurn},
			wantOK:      true,
		},
		{
			name:        "a type result line with status error and an object-valued error recognizes ErrResponseError",
			output:      `{"type":"result","status":"error","error":{"message":"boom"}}`,
			wantOutcome: geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError},
			wantOK:      true,
		},
		{
			name:        "status is read directly off the result event, even when error is a plain string rather than an object",
			output:      `{"type":"result","status":"error","error":"boom"}`,
			wantOutcome: geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError},
			wantOK:      true,
		},
		{
			name: "a non-terminal line carrying a status-named member is never the terminal event, and no type result line at all recognizes nothing",
			output: `{"type":"init","status":"success"}
{"type":"message","status":"success"}`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outcome, ok := geminiNativeStreamTerminal(tt.output)
			if ok != tt.wantOK || outcome != tt.wantOutcome {
				t.Errorf("geminiNativeStreamTerminal(%q) = %+v, %v, want %+v, %v", tt.output, outcome, ok, tt.wantOutcome, tt.wantOK)
			}
		})
	}
}

// TestGeminiNativeJSONCaseSuccessGradesUsableLive proves, against the
// live-gated Gemini CLI, that CaseSuccess on SurfaceNativeJSON now
// grades usable end to end through geminiInduceNativeCase, closing the
// open question about a structured native surface's
// terminal member in code rather than by probe transcript alone. With
// the gate disabled it skips cleanly.
func TestGeminiNativeJSONCaseSuccessGradesUsableLive(t *testing.T) {
	config := requireGeminiQualification(t)
	runtime := geminiResolveQualificationRuntime(t, config)

	catalog := geminiSemanticInputCatalog(runtime.Probes, geminiNonce(t))
	obs := geminiCollectSemanticCase(t, runtime, &geminiProcessGroupTracker{}, qualification.SurfaceNativeJSON, qualification.CaseSuccess, catalog, newGeminiTokenLedger())
	rec := geminiSemanticRecordFor(t, qualification.SurfaceNativeJSON, qualification.CapabilityTurnDisposition, qualification.CaseSuccess, obs, qualification.DeclaredGapSet{})
	if rec.Grade != qualification.GradeUsable {
		t.Errorf("native_json CaseSuccess record = %s, want usable", rec.Grade)
	}
}
