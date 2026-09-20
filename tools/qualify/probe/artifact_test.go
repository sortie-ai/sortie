//go:build unix

package probe

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

var (
	_ notesConsistencyReporter = (*fakeNotesReporter)(nil)
	_ testingT                 = (*fakeTestingT)(nil)
)

func TestWriteEvidenceRecords(t *testing.T) {
	t.Parallel()

	t.Run("round-trips through evidence.ReadEvidenceFile", func(t *testing.T) {
		t.Parallel()

		records := []evidence.Record{evidence.ValidRecord()}
		path := filepath.Join(t.TempDir(), "evidence.jsonl")

		if err := writeEvidenceRecords(path, records); err != nil {
			t.Fatalf("writeEvidenceRecords(%q, ...) = %v, want nil", path, err)
		}

		got, err := evidence.ReadEvidenceFile(path)
		if err != nil {
			t.Fatalf("evidence.ReadEvidenceFile(%q) = _, %v, want nil", path, err)
		}
		if len(got) != len(records) {
			t.Fatalf("ReadEvidenceFile(%q) = %d records, want %d", path, len(got), len(records))
		}
		for i := range records {
			if !evidence.RecordsEqual(got[i], records[i]) {
				t.Errorf("record %d = %+v, want %+v", i, got[i], records[i])
			}
		}
	})

	t.Run("a path under a directory that does not exist reports an error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "no-such-dir", "evidence.jsonl")
		if err := writeEvidenceRecords(path, []evidence.Record{evidence.ValidRecord()}); err == nil {
			t.Fatalf("writeEvidenceRecords(%q, ...) = nil, want an error", path)
		}
	})
}

func TestWriteMeasurement(t *testing.T) {
	t.Parallel()

	t.Run("round-trips through evidence.ReadMeasurementFile", func(t *testing.T) {
		t.Parallel()

		measurement := evidence.Measurement{
			SchemaVersion:    5,
			ProfileDigest:    "digest-fixture",
			MeasuredAt:       "2026-01-01",
			RequestedModel:   "a-model",
			EvaluatorVersion: 1,
			Expectation: evidence.NotesExpectation{
				Verdict: evidence.VerdictQualified,
				Grades: []evidence.NotesGrade{
					{
						Surface:    evidence.SurfaceProtocol,
						Capability: evidence.CapabilityTurnDisposition,
						Grade:      evidence.GradeUsable,
						Label:      "Observed:",
					},
				},
				Excluded:   []string{"retry_classification human_input: declared no_retry_path"},
				Unobserved: []string{"native_json tool_server_delivery: no server declared"},
			},
		}
		path := filepath.Join(t.TempDir(), "measurement.json")

		if err := writeMeasurement(path, measurement); err != nil {
			t.Fatalf("writeMeasurement(%q, ...) = %v, want nil", path, err)
		}

		got, err := evidence.ReadMeasurementFile(path)
		if err != nil {
			t.Fatalf("evidence.ReadMeasurementFile(%q) = _, %v, want nil", path, err)
		}
		if !reflect.DeepEqual(got, measurement) {
			t.Errorf("ReadMeasurementFile(%q) = %+v, want %+v", path, got, measurement)
		}
	})

	t.Run("the encoded document is indented JSON ending in a trailing newline", func(t *testing.T) {
		t.Parallel()

		measurement := evidence.Measurement{
			SchemaVersion:    5,
			ProfileDigest:    "digest-fixture",
			MeasuredAt:       "2026-01-01",
			RequestedModel:   "a-model",
			EvaluatorVersion: 1,
			Expectation:      evidence.NotesExpectation{Verdict: evidence.VerdictUnmeasured},
		}
		path := filepath.Join(t.TempDir(), "measurement.json")
		if err := writeMeasurement(path, measurement); err != nil {
			t.Fatalf("writeMeasurement(%q, ...) = %v, want nil", path, err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile(%q): %v", path, err)
		}
		want, err := json.MarshalIndent(measurement, "", "  ")
		if err != nil {
			t.Fatalf("json.MarshalIndent: %v", err)
		}
		want = append(want, '\n')
		if string(raw) != string(want) {
			t.Errorf("written document = %q, want %q", raw, want)
		}
	})

	t.Run("a path under a directory that does not exist reports an error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "no-such-dir", "measurement.json")
		if err := writeMeasurement(path, evidence.Measurement{}); err == nil {
			t.Fatalf("writeMeasurement(%q, ...) = nil, want an error", path)
		}
	})
}

func TestWriteUnrecognizedTerminals(t *testing.T) {
	t.Parallel()

	t.Run("an empty set writes no file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "unrecognized.jsonl")
		if err := writeUnrecognizedTerminals(path, nil); err != nil {
			t.Fatalf("writeUnrecognizedTerminals(%q, nil) = %v, want nil", path, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%q) error = %v, want a not-exist error: an empty set must leave no file behind", path, err)
		}
	})

	t.Run("a non-empty set writes one JSON object per line, preserving the envelope verbatim", func(t *testing.T) {
		t.Parallel()

		terminals := []map[string]any{
			{"type": "result", "status": "mystery"},
			{"type": "result", "status": "another_mystery", "detail": map[string]any{"nested": true}},
		}
		path := filepath.Join(t.TempDir(), "unrecognized.jsonl")
		if err := writeUnrecognizedTerminals(path, terminals); err != nil {
			t.Fatalf("writeUnrecognizedTerminals(%q, ...) = %v, want nil", path, err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile(%q): %v", path, err)
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) != len(terminals) {
			t.Fatalf("written file has %d lines, want %d", len(lines), len(terminals))
		}
		for i, line := range lines {
			var got map[string]any
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatalf("line %d does not decode as JSON: %v", i, err)
			}
			if !reflect.DeepEqual(got, terminals[i]) {
				t.Errorf("line %d = %+v, want %+v", i, got, terminals[i])
			}
		}
	})

	t.Run("a path under a directory that does not exist reports an error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "no-such-dir", "unrecognized.jsonl")
		if err := writeUnrecognizedTerminals(path, []map[string]any{{"type": "result"}}); err == nil {
			t.Fatalf("writeUnrecognizedTerminals(%q, ...) = nil, want an error", path)
		}
	})
}

type fakeNotesReporter struct {
	calls []string
}

func (f *fakeNotesReporter) Fatalf(format string, args ...any) {
	f.calls = append(f.calls, format)
}

func notesConsistencyExpectation() evidence.NotesExpectation {
	return evidence.NotesExpectation{
		Verdict: evidence.VerdictQualified,
		Grades: []evidence.NotesGrade{
			{
				Surface:    evidence.SurfaceProtocol,
				Capability: evidence.CapabilityTurnDisposition,
				Grade:      evidence.GradeUsable,
				Label:      "Observed:",
			},
		},
	}
}

func notesConsistencyDocument(verdict evidence.Verdict) string {
	return "# Fixture runtime adapter notes\n\n" +
		"Eligibility: " + string(verdict) + "\n\n" +
		"## Entry points\n\nThe fixture runtime enters through one documented flag.\n\n" +
		"## Load-bearing capability observations\n\n" +
		"- protocol turn_disposition: Observed: usable\n\n" +
		"## Protocol-specific observations\n\nThe fixture runtime reports one stop reason per turn.\n\n" +
		"## Native headless observations\n\nThe fixture runtime's headless mode prints unstructured text.\n\n" +
		"## Workspace trust and process boundary\n\nThe fixture runtime trusts the workspace it is launched against.\n\n" +
		"## Excluded capability cases\n\n" +
		"## Unobserved surfaces\n\n" +
		evidence.NotesScopeStatement + ".\n"
}

func TestCheckNotesConsistency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		readNotes func(string) ([]byte, error)
		want      evidence.NotesExpectation
		wantFatal bool
	}{
		{
			name:      "absent notes with a verdict requiring binding fails",
			readNotes: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
			want:      evidence.NotesExpectation{Verdict: evidence.VerdictQualified},
			wantFatal: true,
		},
		{
			name:      "absent notes with a verdict not requiring binding is tolerated",
			readNotes: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
			want:      evidence.NotesExpectation{Verdict: evidence.VerdictUnmeasured},
			wantFatal: false,
		},
		{
			name:      "a read error other than not-exist always fails",
			readNotes: func(string) ([]byte, error) { return nil, fs.ErrPermission },
			want:      evidence.NotesExpectation{Verdict: evidence.VerdictUnmeasured},
			wantFatal: true,
		},
		{
			name: "a readable document agreeing with the expectation passes",
			readNotes: func(string) ([]byte, error) {
				return []byte(notesConsistencyDocument(evidence.VerdictQualified)), nil
			},
			want:      notesConsistencyExpectation(),
			wantFatal: false,
		},
		{
			name: "a readable document disagreeing with the expectation fails",
			readNotes: func(string) ([]byte, error) {
				return []byte(notesConsistencyDocument(evidence.VerdictNotQualified)), nil
			},
			want:      notesConsistencyExpectation(),
			wantFatal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeNotesReporter{}
			checkNotesConsistency(reporter, tt.readNotes, "notes.md", tt.want, "summary")

			if got := len(reporter.calls) > 0; got != tt.wantFatal {
				t.Errorf("checkNotesConsistency(...) called Fatalf = %v (%v), want %v", got, reporter.calls, tt.wantFatal)
			}
		})
	}
}

type fakeTestingT struct {
	fakeNotesReporter
}

func (f *fakeTestingT) Helper() {}
