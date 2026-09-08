package qualification

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestNotesBindingRequired confirms NotesBindingRequired is total over
// the closed verdict set and returns true for VerdictQualified only.
func TestNotesBindingRequired(t *testing.T) {
	t.Parallel()

	for _, verdict := range Verdicts {
		want := verdict == VerdictQualified
		if got := NotesBindingRequired(verdict); got != want {
			t.Errorf("NotesBindingRequired(%s) = %v, want %v", verdict, got, want)
		}
	}
}

// compliantNotesExpectation returns the literal NotesExpectation the
// notes_test.go fixture document below is built to satisfy exactly.
func compliantNotesExpectation() NotesExpectation {
	return NotesExpectation{
		Verdict: VerdictQualified,
		Grades: []NotesGrade{
			{
				Surface:    SurfaceProtocol,
				Capability: CapabilityTurnDisposition,
				Grade:      GradeUsable,
				Label:      StatusLabel(GradeUsable),
			},
			{
				Surface:    SurfaceNativeText,
				Capability: CapabilityTokenCeiling,
				Grade:      GradeNotObserved,
				Label:      StatusLabel(GradeNotObserved),
			},
		},
		Excluded:   []string{"retry_classification human_input: declared no_retry_path"},
		Unobserved: []string{"native_json tool_server_delivery: no server declared"},
	}
}

// compliantNotesDocument renders the one notes document
// compliantNotesExpectation() is built to satisfy exactly, so every
// doctor below can mutate it and violate exactly one comparison-table
// row.
func compliantNotesDocument(want NotesExpectation) string {
	return "# Fixture runtime adapter notes\n\n" +
		"Eligibility: " + string(want.Verdict) + "\n\n" +
		"## Entry points\n\nThe fixture runtime enters through one documented flag.\n\n" +
		"## Load-bearing capability observations\n\n" +
		"- protocol turn_disposition: Observed: usable\n" +
		"- native_text token_ceiling: Not observed: not_observed\n\n" +
		"## Protocol-specific observations\n\nThe fixture runtime reports one stop reason per turn.\n\n" +
		"## Native headless observations\n\nThe fixture runtime's headless mode prints unstructured text.\n\n" +
		"## Workspace trust and process boundary\n\nThe fixture runtime trusts the workspace it is launched against.\n\n" +
		"## Excluded capability cases\n\n" +
		"- retry_classification human_input: declared no_retry_path\n\n" +
		"## Unobserved surfaces\n\n" +
		"- native_json tool_server_delivery: no server declared\n" +
		NotesScopeStatement + ".\n"
}

// TestValidateNotes confirms ValidateNotes accepts a document that
// satisfies every row of the comparison table and rejects a document
// that violates exactly one row, one row at a time.
func TestValidateNotes(t *testing.T) {
	t.Parallel()

	want := compliantNotesExpectation()
	baseline := compliantNotesDocument(want)

	t.Run("a document satisfying every row is accepted", func(t *testing.T) {
		t.Parallel()

		if err := ValidateNotes(baseline, want); err != nil {
			t.Errorf("ValidateNotes() error = %v, want nil for a fully compliant document", err)
		}
	})

	tests := []struct {
		name   string
		doctor func(string) string
	}{
		{
			name: "section order: a required heading is missing",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"## Protocol-specific observations\n\nThe fixture runtime reports one stop reason per turn.\n\n",
					"", 1)
			},
		},
		{
			name: "section order: two headings appear out of order",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"## Entry points\n\nThe fixture runtime enters through one documented flag.\n\n## Load-bearing capability observations",
					"## Load-bearing capability observations\n\n## Entry points\n\nThe fixture runtime enters through one documented flag.", 1)
			},
		},
		{
			name: "eligibility line: zero lines",
			doctor: func(doc string) string {
				return strings.Replace(doc, "Eligibility: qualified\n\n", "", 1)
			},
		},
		{
			name: "eligibility line: more than one line",
			doctor: func(doc string) string {
				return doc + "\nEligibility: qualified\n"
			},
		},
		{
			name: "eligibility line: value differs from the verdict",
			doctor: func(doc string) string {
				return strings.Replace(doc, "Eligibility: qualified\n\n", "Eligibility: not_qualified\n\n", 1)
			},
		},
		{
			name: "grade rows: a row is missing",
			doctor: func(doc string) string {
				return strings.Replace(doc, "- native_text token_ceiling: Not observed: not_observed\n", "", 1)
			},
		},
		{
			name: "grade rows: a row is duplicated",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"- protocol turn_disposition: Observed: usable\n",
					"- protocol turn_disposition: Observed: usable\n- protocol turn_disposition: Observed: usable\n", 1)
			},
		},
		{
			name: "grade rows: a row is unknown to the expectation",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"## Load-bearing capability observations\n\n",
					"## Load-bearing capability observations\n\n- native_json runtime_identity: Observed: usable\n", 1)
			},
		},
		{
			name: "grade rows: a row carries the wrong label",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"- protocol turn_disposition: Observed: usable\n",
					"- protocol turn_disposition: Not observed: usable\n", 1)
			},
		},
		{
			name: "grade rows: a row carries the wrong grade",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"- protocol turn_disposition: Observed: usable\n",
					"- protocol turn_disposition: Observed: gap\n", 1)
			},
		},
		{
			name: "excluded cases: an entry is absent from its section",
			doctor: func(doc string) string {
				return strings.Replace(doc, "- retry_classification human_input: declared no_retry_path\n", "", 1)
			},
		},
		{
			name: "excluded cases: an entry also appears after Unobserved surfaces",
			doctor: func(doc string) string {
				return strings.Replace(doc,
					"## Unobserved surfaces\n\n",
					"## Unobserved surfaces\n\n- retry_classification human_input: declared no_retry_path\n", 1)
			},
		},
		{
			name: "unobserved cases: an entry is absent",
			doctor: func(doc string) string {
				return strings.Replace(doc, "- native_json tool_server_delivery: no server declared\n", "", 1)
			},
		},
		{
			name: "scope statement: absent",
			doctor: func(doc string) string {
				return strings.Replace(doc, NotesScopeStatement, "This runtime's Windows live status is undocumented", 1)
			},
		},
		{
			name: "cleanliness: a line carries a binary version value",
			doctor: func(doc string) string {
				return doc + "\nFixture runtime measured at 1.2.3.\n"
			},
		},
		{
			name: "cleanliness: a line carries a measurement date",
			doctor: func(doc string) string {
				return doc + "\nMeasured on 2026-01-01.\n"
			},
		},
		{
			name: "cleanliness: a line carries an environment variable value",
			doctor: func(doc string) string {
				return doc + "\nFIXTURE_RUNTIME_HOME=/tmp/example\n"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doctored := tt.doctor(baseline)
			if doctored == baseline {
				t.Fatalf("doctor for %q left the baseline document unchanged", tt.name)
			}
			if err := ValidateNotes(doctored, want); err == nil {
				t.Errorf("ValidateNotes() = nil error, want rejection when the notes violate %q", tt.name)
			}
		})
	}
}

// TestNotesExpectationJSONRoundTrip confirms NotesExpectation and
// NotesGrade round-trip through encoding/json with their declared
// field names, the shape the tracked expectation artifact depends on.
func TestNotesExpectationJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := compliantNotesExpectation()

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal(want) error = %v", err)
	}
	for _, field := range []string{`"Verdict"`, `"Grades"`, `"Excluded"`, `"Unobserved"`, `"Surface"`, `"Capability"`, `"Grade"`, `"Label"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("json.Marshal(want) = %s, want it to carry the declared field name %s", raw, field)
		}
	}

	var got NotesExpectation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped NotesExpectation = %+v, want %+v", got, want)
	}
}

// TestValidateNotesRejectsDriftTheShapeAlone covers the two ways a
// document can disagree with its run without omitting anything: a grade
// row outside the vocabulary, and an entry the run never recorded.
// Requiring only that expected content is present would accept both.
func TestValidateNotesRejectsDriftTheShapeAlone(t *testing.T) {
	t.Parallel()

	want := compliantNotesExpectation()

	t.Run("a grade row outside the vocabulary is rejected rather than skipped", func(t *testing.T) {
		t.Parallel()
		document := strings.Replace(compliantNotesDocument(want),
			"## Protocol-specific observations",
			"- protocol turn_disposition: Invented: usable\n\n## Protocol-specific observations", 1)
		err := ValidateNotes(document, want)
		if err == nil {
			t.Fatal("ValidateNotes() = nil, want an error naming the row outside the vocabulary")
		}
		if !strings.Contains(err.Error(), "outside the vocabulary") {
			t.Errorf("error = %q, want it to name the vocabulary", err)
		}
	})

	t.Run("prose about a capability is not mistaken for a grade row", func(t *testing.T) {
		t.Parallel()
		document := strings.Replace(compliantNotesDocument(want),
			"## Protocol-specific observations",
			"- protocol token_ceiling: no token-bearing path\n\n## Protocol-specific observations", 1)
		if err := ValidateNotes(document, want); err != nil {
			t.Errorf("ValidateNotes() error = %v, want nil for a single-colon prose line", err)
		}
	})

	t.Run("an unobserved case the run never recorded is rejected", func(t *testing.T) {
		t.Parallel()
		document := strings.Replace(compliantNotesDocument(want),
			"## Unobserved surfaces\n\n",
			"## Unobserved surfaces\n\n- protocol session_continuation: invented case\n", 1)
		err := ValidateNotes(document, want)
		if err == nil {
			t.Fatal("ValidateNotes() = nil, want an error naming the unrecorded case")
		}
		if !strings.Contains(err.Error(), "does not record") {
			t.Errorf("error = %q, want it to say the run does not record the case", err)
		}
	})

	t.Run("an excluded case the run never recorded is rejected", func(t *testing.T) {
		t.Parallel()
		document := strings.Replace(compliantNotesDocument(want),
			"## Excluded capability cases\n\n",
			"## Excluded capability cases\n\n- turn_disposition cancellation: invented exclusion\n", 1)
		err := ValidateNotes(document, want)
		if err == nil {
			t.Fatal("ValidateNotes() = nil, want an error naming the unrecorded exclusion")
		}
		if !strings.Contains(err.Error(), "does not record") {
			t.Errorf("error = %q, want it to say the run does not record the case", err)
		}
	})
}
