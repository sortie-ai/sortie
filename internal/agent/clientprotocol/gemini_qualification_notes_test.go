package clientprotocol

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// geminiSummaryGrade is one surface-capability grade with the exact
// status label the adapter notes must use for it.
type geminiSummaryGrade struct {
	Surface    qualification.Surface
	Capability qualification.Capability
	Grade      qualification.Grade
	Label      string
}

// geminiSummarySemantic is one of the 36 semantic-case verdicts.
type geminiSummarySemantic struct {
	Surface    qualification.Surface
	Capability qualification.Capability
	Case       qualification.Case
	Verdict    qualification.Outcome
}

// geminiSummaryToken is one token-bearing path with its classification.
type geminiSummaryToken struct {
	Surface        qualification.Surface
	EvidencePath   string
	Classification qualification.Grade
}

// geminiSummaryContinuation is one surface's continuation outcome.
type geminiSummaryContinuation struct {
	Surface qualification.Surface
	Outcome string
	Grade   qualification.Grade
}

// geminiSummaryConclusions is the bounded summary a validated evidence
// set produces. It carries no runtime version, timestamp, session
// identifier, filesystem path, prompt, or secret value.
type geminiSummaryConclusions struct {
	Verdict       qualification.Verdict
	Grades        []geminiSummaryGrade
	Semantics     []geminiSummarySemantic
	Tokens        []geminiSummaryToken
	Continuations []geminiSummaryContinuation
	Workspace     string
	Unobserved    []string
}

// geminiNotesHeadings are the required adapter-notes headings in R8
// order.
var geminiNotesHeadings = []string{
	"# Gemini CLI adapter notes",
	"## Entry points",
	"## Load-bearing capability observations",
	"## Protocol-specific observations",
	"## Native headless observations",
	"## Workspace trust and process boundary",
	"## Unobserved surfaces",
}

// geminiNotesUnixScope is the sentence the notes must carry to state the
// Unix-only live scope and the unobserved Windows behavior.
const geminiNotesUnixScope = "Windows live qualification is unobserved"

// geminiStatusLabel maps a grade to the exact notes status label.
func geminiStatusLabel(classification qualification.Grade) string {
	switch classification {
	case qualification.GradeUsable, qualification.GradeGap:
		return "Observed:"
	case qualification.GradeNotObserved:
		return "Not observed:"
	case qualification.GradeNotApplicable:
		return "Not applicable:"
	}
	return ""
}

// geminiSummaryConclusionsFromRecords derives the bounded conclusions
// from a validated non-final evidence set and its computed verdict.
func geminiSummaryConclusionsFromRecords(records []qualification.Record, verdict qualification.Verdict) (geminiSummaryConclusions, error) {
	conclusions := geminiSummaryConclusions{Verdict: verdict}

	for i := range records {
		rec := &records[i]
		class, err := qualification.ClassifyRecord(rec)
		if err != nil {
			return geminiSummaryConclusions{}, fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		switch class {
		case qualification.RowBaseline:
			conclusions.Grades = append(conclusions.Grades, geminiSummaryGrade{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Grade:      rec.Grade,
				Label:      geminiStatusLabel(rec.Grade),
			})
		case qualification.RowMCPDelivery, qualification.RowPermission:
			conclusions.Grades = append(conclusions.Grades, geminiSummaryGrade{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Grade:      rec.Grade,
				Label:      geminiStatusLabel(rec.Grade),
			})
		case qualification.RowSemantic:
			semantic := geminiSummarySemantic{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Verdict:    rec.Outcome,
			}
			if rec.SemanticCase != nil {
				semantic.Case = *rec.SemanticCase
			}
			conclusions.Semantics = append(conclusions.Semantics, semantic)
			if rec.Grade == qualification.GradeNotObserved {
				conclusions.Unobserved = append(conclusions.Unobserved,
					fmt.Sprintf("%s %s %s", rec.Surface, rec.Capability, semantic.Case))
			}
		case qualification.RowToken:
			path := "(no token-bearing path)"
			if rec.EvidencePath != nil {
				path = *rec.EvidencePath
			}
			conclusions.Tokens = append(conclusions.Tokens, geminiSummaryToken{
				Surface:        rec.Surface,
				EvidencePath:   path,
				Classification: rec.Grade,
			})
		case qualification.RowContinuationRecall:
			conclusions.Continuations = append(conclusions.Continuations, geminiSummaryContinuation{
				Surface: rec.Surface,
				Outcome: rec.Detail,
				Grade:   rec.Grade,
			})
		case qualification.RowWorkspaceSecurity:
			conclusions.Workspace = fmt.Sprintf("%s %s", geminiStatusLabel(rec.Grade), rec.Detail)
		}
	}

	if len(conclusions.Grades) != 18 {
		return geminiSummaryConclusions{}, fmt.Errorf("derived %d capability grades, want 16 baselines plus tool server and permission", len(conclusions.Grades))
	}
	if len(conclusions.Semantics) != 36 {
		return geminiSummaryConclusions{}, fmt.Errorf("derived %d semantic verdicts, want 36", len(conclusions.Semantics))
	}
	if len(conclusions.Continuations) != 4 {
		return geminiSummaryConclusions{}, fmt.Errorf("derived %d continuation outcomes, want 4", len(conclusions.Continuations))
	}
	if conclusions.Workspace == "" {
		return geminiSummaryConclusions{}, fmt.Errorf("derived no workspace security conclusion")
	}

	slices.SortStableFunc(conclusions.Grades, func(a, b geminiSummaryGrade) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		return slices.Index(qualification.Capabilities, a.Capability) - slices.Index(qualification.Capabilities, b.Capability)
	})
	slices.SortStableFunc(conclusions.Semantics, func(a, b geminiSummarySemantic) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		if c := slices.Index(qualification.Capabilities, a.Capability) - slices.Index(qualification.Capabilities, b.Capability); c != 0 {
			return c
		}
		return slices.Index(qualification.CapabilityCases[a.Capability], a.Case) - slices.Index(qualification.CapabilityCases[b.Capability], b.Case)
	})
	slices.SortStableFunc(conclusions.Tokens, func(a, b geminiSummaryToken) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		return strings.Compare(a.EvidencePath, b.EvidencePath)
	})
	slices.SortStableFunc(conclusions.Continuations, func(a, b geminiSummaryContinuation) int {
		return slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface)
	})
	slices.Sort(conclusions.Unobserved)

	return conclusions, nil
}

// formatGeminiQualificationSummary renders the bounded, actionable
// summary the harness prints after final validation.
func formatGeminiQualificationSummary(conclusions geminiSummaryConclusions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Eligibility: %s\n", conclusions.Verdict)
	fmt.Fprint(&b, "Capability grades:\n")
	for _, grade := range conclusions.Grades {
		fmt.Fprintf(&b, "%s %s: %s %s\n", grade.Surface, grade.Capability, grade.Label, grade.Grade)
	}
	fmt.Fprint(&b, "Semantic verdicts:\n")
	for _, semantic := range conclusions.Semantics {
		fmt.Fprintf(&b, "%s %s %s: %s\n", semantic.Surface, semantic.Capability, semantic.Case, semantic.Verdict)
	}
	fmt.Fprint(&b, "Token sources:\n")
	for _, token := range conclusions.Tokens {
		fmt.Fprintf(&b, "%s %s: %s\n", token.Surface, token.EvidencePath, token.Classification)
	}
	fmt.Fprint(&b, "Continuation:\n")
	for _, continuation := range conclusions.Continuations {
		fmt.Fprintf(&b, "%s: %s (%s %s)\n", continuation.Surface, continuation.Outcome, geminiStatusLabel(continuation.Grade), continuation.Grade)
	}
	fmt.Fprintf(&b, "Workspace security:\n%s\n", conclusions.Workspace)
	fmt.Fprint(&b, "Unobserved semantic cases:\n")
	if len(conclusions.Unobserved) == 0 {
		fmt.Fprint(&b, "none\n")
	}
	for _, entry := range conclusions.Unobserved {
		fmt.Fprintf(&b, "%s\n", entry)
	}
	return b.String()
}

// The notes-validation patterns: grade rows with their exact status
// labels, and the banned version, date, and environment-value shapes.
var (
	geminiNotesGradeRowPattern = regexp.MustCompile(`^- (` + geminiNotesAlternation(qualification.Surfaces) + `) ([a-z_]+): (Observed|Not observed|Not applicable): (usable|gap|not_observed|not_applicable)\b`)
	geminiNotesVersionPattern  = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)
	geminiNotesDatePattern     = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	geminiNotesEnvValuePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*=\S`)
)

// geminiNotesAlternation renders a closed value set as a regex
// alternation.
func geminiNotesAlternation[T ~string](values []T) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, regexp.QuoteMeta(string(value)))
	}
	return strings.Join(quoted, "|")
}

// validateGeminiAdapterNotes validates a durable adapter-notes document
// against the conclusions of a freshly validated summary: heading order,
// status labels, one matching eligibility line, every unobserved case in
// the final section, the Unix-only scope statement, and no version,
// date, or environment value.
func validateGeminiAdapterNotes(notes string, want geminiSummaryConclusions) error {
	lines := strings.Split(notes, "\n")
	trimmed := make([]string, len(lines))
	for i, line := range lines {
		trimmed[i] = strings.TrimSpace(line)
	}

	position := -1
	for _, heading := range geminiNotesHeadings {
		next := slices.Index(trimmed[position+1:], heading)
		if next < 0 {
			return fmt.Errorf("notes heading %q is missing or out of order", heading)
		}
		position += next + 1
	}

	eligibilityCount := 0
	for _, line := range trimmed {
		if !strings.HasPrefix(line, "Eligibility: ") {
			continue
		}
		eligibilityCount++
		if value := strings.TrimPrefix(line, "Eligibility: "); value != string(want.Verdict) {
			return fmt.Errorf("notes eligibility %q does not match the validated verdict %q", value, want.Verdict)
		}
	}
	if eligibilityCount != 1 {
		return fmt.Errorf("notes carry %d Eligibility lines, want exactly 1", eligibilityCount)
	}

	wantGrades := map[string]geminiSummaryGrade{}
	for _, grade := range want.Grades {
		wantGrades[string(grade.Surface)+" "+string(grade.Capability)] = grade
	}
	seenGrades := map[string]bool{}
	for i, line := range trimmed {
		match := geminiNotesGradeRowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key := match[1] + " " + match[2]
		wantGrade, known := wantGrades[key]
		if !known {
			return fmt.Errorf("notes line %d claims grade %q that the validated summary does not carry", i+1, key)
		}
		if seenGrades[key] {
			return fmt.Errorf("notes line %d duplicates the grade row %q", i+1, key)
		}
		seenGrades[key] = true
		if match[3]+":" != wantGrade.Label {
			return fmt.Errorf("notes line %d grades %q as %q, want %q", i+1, key, match[3]+":", wantGrade.Label)
		}
		if match[4] != string(wantGrade.Grade) {
			return fmt.Errorf("notes line %d grades %q as %s, want %s", i+1, key, match[4], wantGrade.Grade)
		}
	}
	for key := range wantGrades {
		if !seenGrades[key] {
			return fmt.Errorf("notes carry no status-label row for %q", key)
		}
	}

	unobservedHeading := slices.Index(trimmed, "## Unobserved surfaces")
	if unobservedHeading < 0 {
		return fmt.Errorf("notes heading %q is missing", "## Unobserved surfaces")
	}
	tail := strings.Join(trimmed[unobservedHeading:], "\n")
	for _, entry := range want.Unobserved {
		if !strings.Contains(tail, entry) {
			return fmt.Errorf("unobserved semantic case %q is absent from the Unobserved surfaces section", entry)
		}
	}

	if !strings.Contains(notes, geminiNotesUnixScope) {
		return fmt.Errorf("notes do not state that %s", strings.ToLower(geminiNotesUnixScope))
	}

	for i, line := range trimmed {
		switch {
		case geminiNotesVersionPattern.MatchString(line):
			return fmt.Errorf("notes line %d carries a binary version value", i+1)
		case geminiNotesDatePattern.MatchString(line):
			return fmt.Errorf("notes line %d carries a measurement date", i+1)
		case geminiNotesEnvValuePattern.MatchString(line):
			return fmt.Errorf("notes line %d carries an environment variable value", i+1)
		}
	}
	return nil
}

// geminiAdapterNotesFixture renders a compliant adapter-notes document
// from the summary conclusions, mirroring the manual transfer the
// qualification flow requires.
func geminiAdapterNotesFixture(want geminiSummaryConclusions) string {
	var b strings.Builder
	fmt.Fprint(&b, "# Gemini CLI adapter notes\n\n")
	fmt.Fprintf(&b, "Eligibility: %s\n\n", want.Verdict)

	fmt.Fprint(&b, "## Entry points\n\n")
	fmt.Fprint(&b, "Entry points use the generic agent-client-protocol kind with the runtime command the operator selects.\n\n")

	fmt.Fprint(&b, "## Load-bearing capability observations\n\n")
	for _, grade := range want.Grades {
		if grade.Surface == qualification.SurfaceNativeText {
			continue
		}
		fmt.Fprintf(&b, "- %s %s: %s %s with a bounded evidence shape\n", grade.Surface, grade.Capability, grade.Label, grade.Grade)
	}
	fmt.Fprint(&b, "\n")

	fmt.Fprint(&b, "## Protocol-specific observations\n\n")
	fmt.Fprint(&b, "Permission requests are answered with the shared refusal posture, and tool-server delivery is proven by server receipt.\n\n")

	fmt.Fprint(&b, "## Native headless observations\n\n")
	for _, grade := range want.Grades {
		if grade.Surface != qualification.SurfaceNativeText {
			continue
		}
		fmt.Fprintf(&b, "- %s %s: %s %s as unstructured residue or structured output\n", grade.Surface, grade.Capability, grade.Label, grade.Grade)
	}
	fmt.Fprint(&b, "\n")

	fmt.Fprintf(&b, "## Workspace trust and process boundary\n\n%s. %s and out of scope for this measurement.\n\n", want.Workspace, geminiNotesUnixScope)

	fmt.Fprint(&b, "## Unobserved surfaces\n\n")
	if len(want.Unobserved) == 0 {
		fmt.Fprint(&b, "All required observations completed.\n")
	}
	for _, entry := range want.Unobserved {
		fmt.Fprintf(&b, "- %s\n", entry)
	}
	return b.String()
}

// writeGeminiNotesFile writes a notes document to the test's temporary
// directory and returns its path. Tracked documentation is never touched.
func writeGeminiNotesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gemini-adapter-notes.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write notes fixture: %v", err)
	}
	return path
}

// TestGeminiQualificationBoundedSummary confirms the summary shape: the
// eligibility verdict, all surface-capability grades, all 36 semantic
// verdicts, token sources, continuation outcomes, and the workspace
// conclusion, with no session identifiers, versions, or timestamps.
func TestGeminiQualificationBoundedSummary(t *testing.T) {
	t.Parallel()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()
	conclusions, err := geminiSummaryConclusionsFromRecords(fixture.Records, qualification.VerdictQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}

	if len(conclusions.Grades) != 18 {
		t.Errorf("grade count = %d, want 18", len(conclusions.Grades))
	}
	if len(conclusions.Semantics) != 36 {
		t.Errorf("semantic verdict count = %d, want 36", len(conclusions.Semantics))
	}
	if got := len(conclusions.Tokens); got != qualification.TokenRecordCount(fixture.Records) {
		t.Errorf("token source count = %d, want %d", got, qualification.TokenRecordCount(fixture.Records))
	}
	if len(conclusions.Continuations) != 4 {
		t.Errorf("continuation count = %d, want 4", len(conclusions.Continuations))
	}
	if conclusions.Verdict != qualification.VerdictQualified {
		t.Errorf("verdict = %q, want %q", conclusions.Verdict, qualification.VerdictQualified)
	}
	if len(conclusions.Unobserved) != 0 {
		t.Errorf("unobserved cases = %v, want none for the qualified fixture", conclusions.Unobserved)
	}

	summary := formatGeminiQualificationSummary(conclusions)
	if got := strings.Count(summary, "Eligibility: qualified"); got != 1 {
		t.Errorf("summary carries %d eligibility lines, want 1", got)
	}
	for _, want := range []string{
		"protocol turn_disposition: Observed: usable",
		"native_text token_ceiling: Observed: gap",
		"protocol tool_server_delivery: Observed: usable",
		"protocol turn_disposition success: pass",
		"native_text (no token-bearing path): gap",
		"protocol: confirmed_same_session",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing the expected line %q", want)
		}
	}
	for _, banned := range []string{"sess-", qualification.FixtureAgentName, qualification.FixtureAgentVer, qualification.FixtureTime, "/turn/stop_reason"} {
		if strings.Contains(summary, banned) {
			t.Errorf("summary leaks the banned value %q", banned)
		}
	}
	if again := formatGeminiQualificationSummary(conclusions); again != summary {
		t.Error("formatGeminiQualificationSummary() is not deterministic across calls")
	}
}

// TestGeminiQualificationNotesContract confirms the notes validator
// accepts a compliant transfer of the summary and rejects structural,
// labeling, scope, and cleanliness violations.
func TestGeminiQualificationNotesContract(t *testing.T) {
	t.Parallel()

	qualifiedFixture := qualification.NewFixture(qualification.FixtureQualified)
	qualifiedFixture.Finalize()
	qualified, err := geminiSummaryConclusionsFromRecords(qualifiedFixture.Records, qualification.VerdictQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}

	notQualifiedFixture := qualification.NewFixture(qualification.FixtureNotQualified)
	notQualifiedFixture.Finalize()
	notQualified, err := geminiSummaryConclusionsFromRecords(notQualifiedFixture.Records, qualification.VerdictNotQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}
	if len(notQualified.Unobserved) == 0 {
		t.Fatal("not_qualified fixture carries no unobserved cases to check")
	}

	t.Run("compliant transfer validates from a file", func(t *testing.T) {
		t.Parallel()

		path := writeGeminiNotesFile(t, geminiAdapterNotesFixture(qualified))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read notes fixture: %v", err)
		}
		if err := validateGeminiAdapterNotes(string(content), qualified); err != nil {
			t.Errorf("validateGeminiAdapterNotes() error = %v, want nil for a compliant transfer", err)
		}
	})

	t.Run("compliant not_qualified transfer validates", func(t *testing.T) {
		t.Parallel()

		if err := validateGeminiAdapterNotes(geminiAdapterNotesFixture(notQualified), notQualified); err != nil {
			t.Errorf("validateGeminiAdapterNotes() error = %v, want nil for the not_qualified transfer", err)
		}
	})

	tests := []struct {
		name        string
		conclusions geminiSummaryConclusions
		doctor      func(notes string) string
	}{
		{
			name:        "heading out of order",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes,
					"## Entry points\n\nEntry points use the generic agent-client-protocol kind with the runtime command the operator selects.\n\n## Load-bearing capability observations",
					"## Load-bearing capability observations\n\n## Entry points\n\nEntry points use the generic agent-client-protocol kind with the runtime command the operator selects.", 1)
			},
		},
		{
			name:        "missing heading",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes, "## Protocol-specific observations\n\n", "", 1)
			},
		},
		{
			name:        "status label missing",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes,
					"- protocol turn_disposition: Observed: usable with a bounded evidence shape",
					"- protocol turn_disposition: usable with a bounded evidence shape", 1)
			},
		},
		{
			name:        "status label wrong",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes,
					"- protocol turn_disposition: Observed: usable with a bounded evidence shape",
					"- protocol turn_disposition: Not observed: not_observed with a bounded evidence shape", 1)
			},
		},
		{
			name:        "grade value wrong",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes,
					"- protocol turn_disposition: Observed: usable with a bounded evidence shape",
					"- protocol turn_disposition: Observed: gap with a bounded evidence shape", 1)
			},
		},
		{
			name:        "grade row the summary does not carry",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes,
					"## Protocol-specific observations",
					"- native_text tool_server_delivery: Not applicable: not_applicable with no native comparison\n\n## Protocol-specific observations", 1)
			},
		},
		{
			name:        "second eligibility line",
			conclusions: qualified,
			doctor: func(notes string) string {
				return notes + "\nEligibility: qualified\n"
			},
		},
		{
			name:        "eligibility mismatch",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes, "Eligibility: qualified", "Eligibility: not_qualified", 1)
			},
		},
		{
			name:        "unobserved case absent from the final section",
			conclusions: notQualified,
			doctor: func(notes string) string {
				entry := notQualified.Unobserved[0]
				return strings.Replace(notes, "- "+entry+"\n", "", 1)
			},
		},
		{
			name:        "unix-only scope statement missing",
			conclusions: qualified,
			doctor: func(notes string) string {
				return strings.Replace(notes, geminiNotesUnixScope, "Windows live qualification is recorded elsewhere", 1)
			},
		},
		{
			name:        "binary version value",
			conclusions: qualified,
			doctor: func(notes string) string {
				return notes + "\nMeasured against gemini 0.57.0.\n"
			},
		},
		{
			name:        "measurement date",
			conclusions: qualified,
			doctor: func(notes string) string {
				return notes + "\nMeasured on 2026-03-04.\n"
			},
		},
		{
			name:        "environment variable value",
			conclusions: qualified,
			doctor: func(notes string) string {
				return notes + "\nGEMINI_CLI_HOME=/tmp/run-scope\n"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := validateGeminiAdapterNotes(tt.doctor(geminiAdapterNotesFixture(tt.conclusions)), tt.conclusions); err == nil {
				t.Errorf("validateGeminiAdapterNotes() = nil error, want rejection when the notes %s", tt.name)
			}
		})
	}
}

// TestGeminiQualificationNotesMismatch confirms the consistency check
// compares the notes against a freshly validated summary with exact
// equality, rejecting stale verdicts and stale conclusions.
func TestGeminiQualificationNotesMismatch(t *testing.T) {
	t.Parallel()

	freshFixture := qualification.NewFixture(qualification.FixtureQualified)
	freshFixture.Finalize()
	fresh, err := geminiSummaryConclusionsFromRecords(freshFixture.Records, qualification.VerdictQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}

	otherFixture := qualification.NewFixture(qualification.FixtureNotQualified)
	otherFixture.Finalize()
	other, err := geminiSummaryConclusionsFromRecords(otherFixture.Records, qualification.VerdictNotQualified)
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}

	t.Run("identical conclusions from an independently built fixture match", func(t *testing.T) {
		t.Parallel()

		independent := qualification.NewFixture(qualification.FixtureQualified)
		independent.Finalize()
		independentConclusions, err := geminiSummaryConclusionsFromRecords(independent.Records, qualification.VerdictQualified)
		if err != nil {
			t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
		}
		if formatGeminiQualificationSummary(independentConclusions) != formatGeminiQualificationSummary(fresh) {
			t.Error("two independently built qualified fixtures produced different summaries")
		}
		if err := validateGeminiAdapterNotes(geminiAdapterNotesFixture(independentConclusions), fresh); err != nil {
			t.Errorf("validateGeminiAdapterNotes() error = %v, want nil for a fresh consistent rerun", err)
		}
	})

	t.Run("notes for the other verdict mismatch", func(t *testing.T) {
		t.Parallel()

		if err := validateGeminiAdapterNotes(geminiAdapterNotesFixture(other), fresh); err == nil {
			t.Error("validateGeminiAdapterNotes() = nil error, want mismatch when the eligibility verdict differs")
		}
	})

	t.Run("one stale status label mismatches", func(t *testing.T) {
		t.Parallel()

		stale := strings.Replace(geminiAdapterNotesFixture(fresh),
			"- protocol turn_disposition: Observed: usable with a bounded evidence shape",
			"- protocol turn_disposition: Not observed: not_observed with a bounded evidence shape", 1)
		if err := validateGeminiAdapterNotes(stale, fresh); err == nil {
			t.Error("validateGeminiAdapterNotes() = nil error, want mismatch for a stale status label")
		}
	})

	t.Run("an unobserved case that the fresh run observed mismatches", func(t *testing.T) {
		t.Parallel()

		if err := validateGeminiAdapterNotes(geminiAdapterNotesFixture(other), other); err != nil {
			t.Fatalf("validateGeminiAdapterNotes() error = %v, want nil for the baseline not_qualified transfer", err)
		}
		restored := qualification.NewFixture(qualification.FixtureQualified)
		restored.SetSemanticNotObserved(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseUnknownOutcome)
		restored.Finalize()
		freshWithGap, err := geminiSummaryConclusionsFromRecords(restored.Records, qualification.VerdictNotQualified)
		if err != nil {
			t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
		}
		if err := validateGeminiAdapterNotes(geminiAdapterNotesFixture(other), freshWithGap); err == nil {
			t.Error("validateGeminiAdapterNotes() = nil error, want mismatch when the fresh summary observes a case the notes call unobserved")
		}
	})
}
