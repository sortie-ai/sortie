package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// provenanceAgent is one session's handshake reading, carried in
// provenanceDocument.SessionAgents.
type provenanceAgent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// provenanceDocument mirrors the provenance.json shape tools/qualify/probe
// writes.
type provenanceDocument struct {
	SchemaVersion   int                        `json:"schema_version"`
	ObservedAt      string                     `json:"observed_at"`
	ProfileDigest   string                     `json:"profile_digest"`
	CollectorDigest string                     `json:"collector_digest"`
	EvidenceFile    string                     `json:"evidence_file"`
	EvidenceDigest  string                     `json:"evidence_digest"`
	RequestedModel  string                     `json:"requested_model"`
	ProtocolVersion int                        `json:"protocol_version"`
	SessionAgents   map[string]provenanceAgent `json:"session_agents"`
	CredentialMode  map[string]string          `json:"credential_mode"`
}

// existingMeasurementFields carries only the members Reemit reads from the
// measurement document it replaces, decoded leniently since that document
// may still be schema 4, carrying no evaluator_version.
type existingMeasurementFields struct {
	MeasuredAt       string                    `json:"measured_at"`
	RequestedModel   string                    `json:"requested_model"`
	ObservedModel    *string                   `json:"observed_model"`
	EvaluatorVersion int                       `json:"evaluator_version"`
	Expectation      evidence.NotesExpectation `json:"expectation"`
}

// Reemit rewrites one capture directory's derived artifacts. It never
// rewrites evidence.jsonl, recomputes provenance.json's profile_digest and
// evidence_digest and carries every other member over byte for byte, and
// refuses a rewrite whose re-derived expectation has no raw material
// behind it.
func Reemit(dir string, p profile.RuntimeProfile) error {
	evidencePath := filepath.Join(dir, evidenceFileName)
	provenancePath := filepath.Join(dir, provenanceFileName)
	measurementPath := filepath.Join(dir, measurementFileName)

	measurementData, err := os.ReadFile(measurementPath) //nolint:gosec // the caller resolves path from its own capture directory
	if err != nil {
		return fmt.Errorf("read %s: %w", measurementPath, err)
	}
	var existingMeasurement existingMeasurementFields
	if err := json.Unmarshal(measurementData, &existingMeasurement); err != nil {
		return fmt.Errorf("decode %s: %w", measurementPath, err)
	}

	provenanceData, err := os.ReadFile(provenancePath) //nolint:gosec // the caller resolves path from its own capture directory
	if err != nil {
		return fmt.Errorf("read %s: %w", provenancePath, err)
	}
	var provenance provenanceDocument
	if err := json.Unmarshal(provenanceData, &provenance); err != nil {
		return fmt.Errorf("decode %s: %w", provenancePath, err)
	}

	journalPath := filepath.Join(dir, journalFileName)
	entries, journalErr := readJournal(journalPath)
	journalPresent := journalErr == nil
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}

	// evidence.jsonl is carried over byte for byte: reconstructing whole
	// records from a journal is the live-composition responsibility
	// tools/qualify/probe owns, which eval cannot import.
	records, err := evidence.ReadEvidenceFile(evidencePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", evidencePath, err)
	}
	parity, err := ValidateObservations(evidencePath, p)
	if err != nil {
		return fmt.Errorf("validate %s: %w", evidencePath, err)
	}
	conclusions, err := conclusionsFromRecords(records, parity, p)
	if err != nil {
		return err
	}
	newExpectation := expectationFrom(conclusions)

	if journalPresent && existingMeasurement.EvaluatorVersion == evaluatorVersion {
		if _, err := compareJournalToRecords(p, records, entries); err != nil {
			return fmt.Errorf("expectation: refusing to reemit under an unchanged evaluator version: %w", err)
		}
	}

	if moved := movedExpectationMember(existingMeasurement.Expectation, newExpectation); moved != "" {
		switch {
		case existingMeasurement.EvaluatorVersion == evaluatorVersion:
			return fmt.Errorf("expectation: refusing to reemit: %s would change under an unchanged evaluator version", moved)
		case !journalPresent:
			return fmt.Errorf("expectation: refusing to reemit: %s would change with no %s present to justify it", moved, journalFileName)
		}
	}

	profileDigest := p.Digest()
	evidenceDigest, err := fileSHA256(evidencePath)
	if err != nil {
		return err
	}
	provenance.ProfileDigest = profileDigest
	provenance.EvidenceDigest = evidenceDigest
	provenanceEncoded, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(provenancePath, append(provenanceEncoded, '\n'), 0o600); err != nil { //nolint:gosec // the caller resolves path from its own capture directory
		return fmt.Errorf("write %s: %w", provenancePath, err)
	}
	provenanceDigest, err := fileSHA256(provenancePath)
	if err != nil {
		return err
	}

	measurement := evidence.Measurement{
		SchemaVersion:    evidence.MeasurementSchemaVersion,
		ProfileDigest:    profileDigest,
		MeasuredAt:       existingMeasurement.MeasuredAt,
		Expectation:      newExpectation,
		RequestedModel:   existingMeasurement.RequestedModel,
		ObservedModel:    existingMeasurement.ObservedModel,
		ProvenanceDigest: &provenanceDigest,
		EvaluatorVersion: evaluatorVersion,
	}
	measurementEncoded, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(measurementPath, append(measurementEncoded, '\n'), 0o600); err != nil { //nolint:gosec // the caller resolves path from its own capture directory
		return fmt.Errorf("write %s: %w", measurementPath, err)
	}
	return nil
}

// movedExpectationMember names the first member of new that differs from
// old, or "" when the two carry the same derived content.
func movedExpectationMember(old, next evidence.NotesExpectation) string {
	switch {
	case old.Verdict != next.Verdict:
		return "expectation.verdict"
	case !equalNullableVerdict(old.Conformance, next.Conformance):
		return "expectation.conformance"
	case !slices.Equal(old.Grades, next.Grades):
		return "expectation.grades"
	case !slices.Equal(old.Excluded, next.Excluded):
		return "expectation.excluded"
	case !slices.Equal(old.Unobserved, next.Unobserved):
		return "expectation.unobserved"
	}
	return ""
}

// equalNullableVerdict reports whether two nullable verdicts carry the same
// state: both null, or both non-null with the same value.
func equalNullableVerdict(a, b *evidence.Verdict) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // the caller resolves path from its own capture directory
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}
