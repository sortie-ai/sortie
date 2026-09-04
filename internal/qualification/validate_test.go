package qualification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeLines writes raw JSONL bodies to a temporary file and returns
// its path, for controls that need malformed lines.
func writeLines(T *testing.T, lines ...string) string {
	T.Helper()
	dir := filepath.Join(T.TempDir(), "qualification")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		T.Fatalf("create evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		T.Fatalf("write evidence file: %v", err)
	}
	return path
}

// TestValidateObservations confirms the first pass
// accepts both complete variants with their computed verdicts and the
// closed cardinality, and rejects zero-line, invalid-enum, wrong-schema,
// and non-contiguous input.
func TestValidateObservations(T *testing.T) {
	T.Parallel()

	T.Run("qualified variant computes qualified", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		tokenCount := tokenRecordCount(fixture.Records)
		sessionCount := protocolSessionCount(fixture.Records)
		if got := len(fixture.Records); got != 66+tokenCount+sessionCount {
			T.Errorf("qualified fixture Record count = %d, want 66+tokenCount+sessionCount = %d (T=%d, N=%d)", got, 66+tokenCount+sessionCount, tokenCount, sessionCount)
		}
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictQualified)
	})

	T.Run("not_qualified variant computes not_qualified", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureNotQualified)
		fixture.Finalize()
		tokenCount := tokenRecordCount(fixture.Records)
		sessionCount := protocolSessionCount(fixture.Records)
		if got := len(fixture.Records); got != 66+tokenCount+sessionCount {
			T.Errorf("not_qualified fixture Record count = %d, want 66+tokenCount+sessionCount = %d (T=%d, N=%d)", got, 66+tokenCount+sessionCount, tokenCount, sessionCount)
		}
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictNotQualified)
	})

	T.Run("zero-line file", func(T *testing.T) {
		T.Parallel()

		path := writeLines(T)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of an empty file")
		} else if !strings.Contains(err.Error(), "no records") {
			T.Errorf("ValidateObservations() error = %v, want the empty-file cause", err)
		}
	})

	T.Run("invalid enum value", func(T *testing.T) {
		T.Parallel()

		fields := marshalRecordFields(T, ValidRecord())
		fields["scenario"] = json.RawMessage(`"mystery_scenario"`)
		line, err := json.Marshal(fields)
		if err != nil {
			T.Fatalf("marshal doctored fields: %v", err)
		}
		path := writeLines(T, string(line))
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of an invalid enum")
		} else if !strings.Contains(err.Error(), "outside the closed value set") {
			T.Errorf("ValidateObservations() error = %v, want the closed-set cause", err)
		}
	})

	T.Run("wrong schema version", func(T *testing.T) {
		T.Parallel()

		fields := marshalRecordFields(T, ValidRecord())
		fields["schema_version"] = json.RawMessage(`2`)
		line, err := json.Marshal(fields)
		if err != nil {
			T.Fatalf("marshal doctored fields: %v", err)
		}
		path := writeLines(T, string(line))
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of schema_version 2")
		}
	})

	T.Run("non-contiguous Sequence", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		fixture.Records[3].Sequence = 99
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a non-contiguous Sequence")
		}
	})

	T.Run("blank lines are tolerated", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		var lines []string
		for _, rec := range fixture.Records {
			line, err := MarshalRecord(rec)
			if err != nil {
				T.Fatalf("marshal evidence Record: %v", err)
			}
			lines = append(lines, string(line))
		}
		path := writeLines(T, append([]string{""}, lines...)...)
		RequireObservationVerdict(T, path, VerdictQualified)
	})
}

// TestValidateEvidence confirms the final pass accepts
// both complete variants, and rejects aggregate records with a wrong
// evidence path, a non-pass Verdict, or session fields that must be null.
func TestValidateEvidence(T *testing.T) {
	T.Parallel()

	T.Run("qualified variant", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		path := WriteFinalEvidenceFile(T, fixture.Records, GradeQualified)
		RequireFinalVerdict(T, path, VerdictQualified)
	})

	T.Run("not_qualified variant", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureNotQualified)
		fixture.Finalize()
		path := WriteFinalEvidenceFile(T, fixture.Records, GradeNotQualified)
		RequireFinalVerdict(T, path, VerdictNotQualified)
	})

	T.Run("aggregate field violations", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name   string
			doctor func(rec *Record)
		}{
			{
				name: "wrong evidence path",
				doctor: func(rec *Record) {
					rec.EvidencePath = new("qualification.summary")
				},
			},
			{
				name: "null evidence path",
				doctor: func(rec *Record) {
					rec.EvidencePath = nil
				},
			},
			{
				name: "Verdict not pass",
				doctor: func(rec *Record) {
					rec.Outcome = OutcomeNotObserved
				},
			},
			{
				name: "session id set",
				doctor: func(rec *Record) {
					rec.SessionID = new(FixtureSession(SurfaceProtocol, "e2e"))
				},
			},
			{
				name: "wrong input id",
				doctor: func(rec *Record) {
					rec.InputID = InputBaseline
				},
			},
			{
				name: "protocol version set",
				doctor: func(rec *Record) {
					rec.ProtocolVersion = new(1)
				},
			},
		}

		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				fixture := NewFixture(FixtureQualified)
				fixture.Finalize()
				aggregate := aggregateFixtureRecord(GradeQualified)
				tt.doctor(&aggregate)
				complete := append(slices.Clone(fixture.Records), aggregate)
				for i := range complete {
					complete[i].Sequence = i + 1
				}
				path := WriteEvidenceFile(T, complete)
				if _, err := ValidateEvidence(path); err == nil {
					T.Errorf("ValidateEvidence() = nil error, want rejection when the aggregate %s", tt.name)
				}
			})
		}
	})
}
