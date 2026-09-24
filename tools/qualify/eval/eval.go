package eval

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// Input names one capture directory and the profile to grade against.
type Input struct {
	CaptureDir string
	Profile    profile.RuntimeProfile
}

// Result carries both verdicts, the derived conclusions, the records they
// were derived from, the evaluator version the derivation ran under, and
// whether an observation journal was available.
type Result struct {
	Parity           evidence.Verdict
	Conformance      evidence.Verdict
	Conclusions      Conclusions
	Records          []evidence.Record
	EvaluatorVersion int
	JournalAvailable bool
}

const (
	evidenceFileName    = "evidence.jsonl"
	provenanceFileName  = "provenance.json"
	journalFileName     = "observations.jsonl"
	unrecognizedFile    = "unrecognized.jsonl"
	measurementFileName = "measurement.json"
	summaryFileName     = "summary.txt"
)

// Run re-derives Result from the saved capture. It opens no network
// connection, launches no subprocess, reads no credential, and writes no
// file.
func Run(in Input) (Result, error) {
	evidencePath := filepath.Join(in.CaptureDir, evidenceFileName)
	if _, err := os.Stat(evidencePath); err != nil {
		return Result{}, fmt.Errorf("capture directory %s: missing %s: %w", in.CaptureDir, evidenceFileName, err)
	}
	provenancePath := filepath.Join(in.CaptureDir, provenanceFileName)
	if _, err := os.Stat(provenancePath); err != nil {
		return Result{}, fmt.Errorf("capture directory %s: missing %s: %w", in.CaptureDir, provenanceFileName, err)
	}

	parity, err := ValidateObservations(evidencePath, in.Profile)
	if err != nil {
		return Result{}, fmt.Errorf("validate %s: %w", evidencePath, err)
	}
	records, err := evidence.ReadEvidenceFile(evidencePath)
	if err != nil {
		return Result{}, err
	}
	conclusions, err := conclusionsFromRecords(records, parity, in.Profile)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Parity:           parity,
		Conformance:      conclusions.Conformance,
		Conclusions:      conclusions,
		Records:          records,
		EvaluatorVersion: evaluatorVersion,
	}

	journalPath := filepath.Join(in.CaptureDir, journalFileName)
	entries, err := readJournal(journalPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return result, nil
	case err != nil:
		return Result{}, err
	}
	result.JournalAvailable = true

	truncated, err := compareJournalToRecords(in.Profile, records, entries)
	if err != nil {
		return Result{}, err
	}
	result.Conclusions.Truncated = truncated
	return result, nil
}

// readJournal reads and strictly decodes every line of the observation
// journal at path. A missing file reports os.ErrNotExist so Run can tell it
// apart from a journal that exists but fails to decode.
func readJournal(path string) ([]evidence.JournalEntry, error) {
	file, err := os.Open(path) //nolint:gosec // the caller resolves path from its own capture directory
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read-only handle

	var entries []evidence.JournalEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Bytes()
		if len(text) == 0 {
			continue
		}
		entry, decodeErr := evidence.DecodeJournalEntry(text)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, line, decodeErr)
		}
		entries = append(entries, entry)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, fmt.Errorf("%s: %w", path, scanErr)
	}
	return entries, nil
}

// recordKey is the (surface, input) coordinate one published evidence.jsonl
// row answers a journal entry at.
type recordKey struct {
	surface evidence.Surface
	input   evidence.InputID
}

// indexRecordsByKey groups records by recordKey, preserving every record a
// key carries: a token inventory key may cover several per-path rows.
func indexRecordsByKey(records []evidence.Record) map[recordKey][]evidence.Record {
	index := make(map[recordKey][]evidence.Record, len(records))
	for _, rec := range records {
		key := recordKey{surface: rec.Surface, input: rec.InputID}
		index[key] = append(index[key], rec)
	}
	return index
}

// recordForInput returns the one published record at (surface, input),
// failing unless exactly one exists.
func recordForInput(index map[recordKey][]evidence.Record, surface evidence.Surface, input evidence.InputID) (evidence.Record, error) {
	matches := index[recordKey{surface: surface, input: input}]
	if len(matches) != 1 {
		return evidence.Record{}, fmt.Errorf("published evidence.jsonl carries %d record(s) for surface %s input %s, want 1", len(matches), surface, input)
	}
	return matches[0], nil
}

// recordForCase returns the one published record for a semantic case,
// resolving the input contract that case's record must carry.
func recordForCase(index map[recordKey][]evidence.Record, surface evidence.Surface, caseID evidence.Case) (evidence.Record, error) {
	input, ok := evidence.CaseInputs[caseID]
	if !ok {
		return evidence.Record{}, fmt.Errorf("case %q carries no input contract to match a published record against", caseID)
	}
	return recordForInput(index, surface, input)
}

// compareJournalToRecords re-derives every recognizer and inventory
// journal entry against its published record, reporting rather than
// failing a mismatch whose stream was truncated.
func compareJournalToRecords(p profile.RuntimeProfile, records []evidence.Record, entries []evidence.JournalEntry) ([]string, error) {
	index := indexRecordsByKey(records)
	streamsBySurface := map[evidence.Surface][]evidence.StreamCapture{}
	for _, entry := range entries {
		if entry.Launch == nil || entry.Streams == nil {
			continue
		}
		streamsBySurface[evidence.Surface(entry.Surface)] = append(streamsBySurface[evidence.Surface(entry.Surface)], *entry.Streams)
	}

	var truncated []string
	for i, entry := range entries {
		var (
			isTruncated bool
			diff        error
		)
		switch entry.Derivation {
		case evidence.DerivationRecognizer:
			isTruncated, diff = compareRecognizerEntry(index, p, entry)
		case evidence.DerivationInventory:
			isTruncated, diff = compareInventoryEntry(index, p, entry, streamsBySurface[evidence.Surface(entry.Surface)])
		default:
			continue
		}
		if diff == nil {
			continue
		}
		row := fmt.Sprintf("journal entry %d (surface %s, case %s): %v", i+1, entry.Surface, entry.Case, diff)
		if isTruncated {
			truncated = append(truncated, row)
			continue
		}
		return nil, errors.New(row)
	}
	return truncated, nil
}

// continuationCaseLabels are the journal "case" labels a continuation
// seed/recall entry carries, matching probe's own journal.append call sites.
const (
	continuationSeedLabel   = "continuation_seed"
	continuationRecallLabel = "continuation_recall"
)

// compareRecognizerEntry re-derives one recognizer journal entry, comparing
// it against its named record and reporting whether the stream was
// truncated.
func compareRecognizerEntry(index map[recordKey][]evidence.Record, p profile.RuntimeProfile, entry evidence.JournalEntry) (bool, error) {
	if entry.Launch == nil || entry.Streams == nil {
		return false, errors.New("a recognizer entry must carry launch and streams")
	}
	surface := evidence.Surface(entry.Surface)
	truncated := entry.Streams.Retention != evidence.StreamRetentionFull

	var (
		got evidence.Observation
		rec evidence.Record
		err error
	)
	switch entry.Case {
	case continuationSeedLabel:
		got = RecognizeContinuationSeed(p, surface, *entry.Launch, *entry.Streams)
		rec, err = recordForInput(index, surface, evidence.InputContinuationSeed)
	case continuationRecallLabel:
		got = RecognizeContinuationRecall(p, surface, *entry.Launch, *entry.Streams)
		rec, err = recordForInput(index, surface, evidence.InputContinuationRecall)
	default:
		caseID := evidence.Case(entry.Case)
		if !slices.Contains(evidence.Cases, caseID) {
			return false, fmt.Errorf("case %q is outside the closed value set the recognizer can key on", entry.Case)
		}
		got, _ = Recognize(p, surface, caseID, *entry.Launch, *entry.Streams)
		rec, err = recordForCase(index, surface, caseID)
	}
	if err != nil {
		return false, err
	}
	if rec.Grade == evidence.GradeDeclaredGap && rec.Outcome == evidence.OutcomeNotProducible && evidence.ObservationUnproduced(got) {
		return truncated, nil
	}
	return truncated, compareObservation(rec, got)
}

// compareInventoryEntry re-derives one inventory journal entry and compares
// it against its published record(s), grading each resolved path the way
// tools/qualify/probe does when it composes one.
func compareInventoryEntry(index map[recordKey][]evidence.Record, p profile.RuntimeProfile, entry evidence.JournalEntry, streams []evidence.StreamCapture) (bool, error) {
	surface := evidence.Surface(entry.Surface)
	truncated := false
	for _, stream := range streams {
		if stream.Retention != evidence.StreamRetentionFull {
			truncated = true
		}
	}
	_, paths, got := RecognizeInventory(p, surface, streams)

	if len(paths) == 0 {
		rec, err := recordForInput(index, surface, evidence.InputTokenInventory)
		if err != nil {
			return truncated, err
		}
		return truncated, compareObservation(rec, got)
	}

	matches := index[recordKey{surface: surface, input: evidence.InputTokenInventory}]
	if len(matches) != len(paths) {
		return truncated, fmt.Errorf("re-derivation resolves %d token-bearing path(s), published evidence.jsonl carries %d record(s) for surface %s", len(paths), len(matches), surface)
	}
	kindByPath := make(map[string]string, len(paths))
	for _, resolved := range paths {
		kindByPath[resolved.EvidencePath] = resolved.Kind
	}
	wantDetail := evidence.BoundDetail(got.Detail)
	for _, rec := range matches {
		path := "(no evidence_path)"
		if rec.EvidencePath != nil {
			path = *rec.EvidencePath
		}
		kind, resolved := kindByPath[path]
		if !resolved {
			return truncated, fmt.Errorf("token record at %s names an evidence_path the re-derivation did not resolve", path)
		}
		wantGrade := evidence.TokenPathGrade(kind)
		if rec.Grade != wantGrade || rec.Outcome != got.Outcome || rec.Detail != wantDetail {
			return truncated, fmt.Errorf("token record at %s carries grade=%s outcome=%s detail=%q, re-derivation yields grade=%s outcome=%s detail=%q",
				path, rec.Grade, rec.Outcome, rec.Detail, wantGrade, got.Outcome, wantDetail)
		}
	}
	return truncated, nil
}

// compareObservation reports the difference, if any, between a re-derived
// observation and the record evidence.jsonl published for its coordinate.
func compareObservation(rec evidence.Record, got evidence.Observation) error {
	if got.Grade != rec.Grade || got.Outcome != rec.Outcome || got.Detail != rec.Detail {
		return fmt.Errorf("re-derivation yields grade=%s outcome=%s detail=%q, published evidence.jsonl carries grade=%s outcome=%s detail=%q",
			got.Grade, got.Outcome, got.Detail, rec.Grade, rec.Outcome, rec.Detail)
	}
	return nil
}
