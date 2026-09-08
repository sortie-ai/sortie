package qualification

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// NotesGrade is one surface-capability grade a notes document reports,
// with the exact status label the document's row must carry for it.
type NotesGrade struct {
	Surface    Surface
	Capability Capability
	Grade      Grade
	Label      string
}

// NotesExpectation is what a validated run expects a runtime's own
// notes document to state: its eligibility verdict, every
// surface-capability grade, every excluded capability case, and every
// unobserved surface.
type NotesExpectation struct {
	Verdict    Verdict
	Grades     []NotesGrade
	Excluded   []string
	Unobserved []string
}

// NotesScopeStatement is the sentence a notes document MUST carry to
// state the profile's Unix-only live scope.
const NotesScopeStatement = "Windows live qualification is unobserved"

// NotesSections returns the required section headings in order,
// matched as an ordered subsequence of the document's headings. The
// document's H1 is not constrained: it names the runtime, and the
// runtime varies. Each call returns a fresh slice, so no caller can
// edit the rule set a later comparison runs against.
func NotesSections() []string {
	return []string{
		"## Entry points",
		"## Load-bearing capability observations",
		"## Protocol-specific observations",
		"## Native headless observations",
		"## Workspace trust and process boundary",
		"## Excluded capability cases",
		"## Unobserved surfaces",
	}
}

// StatusLabel maps a grade to the exact status label a notes grade row
// MUST use. It returns the empty string for a grade no row can carry.
func StatusLabel(g Grade) string {
	switch g {
	case GradeUsable, GradeGap:
		return "Observed:"
	case GradeNotObserved:
		return "Not observed:"
	case GradeNotApplicable:
		return "Not applicable:"
	case GradeDeclaredGap:
		return "Declared gap:"
	case GradeNotInducible:
		return "Not inducible:"
	}
	return ""
}

// NotesBindingRequired reports whether a run at this verdict MUST have
// a readable notes document to compare against. True for
// VerdictQualified only.
func NotesBindingRequired(v Verdict) bool {
	return v == VerdictQualified
}

// notesAlternation renders a closed value set as a regex alternation.
func notesAlternation[T ~string](values []T) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, regexp.QuoteMeta(string(value)))
	}
	return strings.Join(quoted, "|")
}

// notesStatusLabelAlternation renders the status labels StatusLabel
// maps grades to as a regex alternation, one alternative per grade,
// skipping a grade whose label is empty (the eligibility-only grades
// no non-final row can carry).
func notesStatusLabelAlternation(grades []Grade) string {
	var labels []string
	for _, grade := range grades {
		label := StatusLabel(grade)
		if label == "" {
			continue
		}
		labels = append(labels, regexp.QuoteMeta(strings.TrimSuffix(label, ":")))
	}
	return strings.Join(labels, "|")
}

var (
	notesGradeRowPattern = regexp.MustCompile(`^- (` + notesAlternation(Surfaces) + `) ([a-z_]+): (` + notesStatusLabelAlternation(RowGrades) + `): (` + notesAlternation(RowGrades) + `)\b`)
	// notesGradeRowCandidate matches the shape of a grade row - a known
	// surface and capability, then a status label, then a grade -
	// without constraining the label or the grade to the vocabulary. A
	// row of this shape is held to the closed pattern rather than
	// passing unexamined. A single-colon row is prose about a
	// capability, not a grade, and does not match.
	notesGradeRowCandidate = regexp.MustCompile(`^- (` + notesAlternation(Surfaces) + `) ([a-z_]+): [^:]+: \S`)
	notesVersionPattern    = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)
	notesDatePattern       = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	notesEnvValuePattern   = regexp.MustCompile(`^[A-Z][A-Z0-9_]*=\S`)
)

// notesListEntries returns the list entries of one section: every line
// opening with a list marker, with the marker stripped. Prose lines
// carry no marker and are not entries, so a section may explain itself
// without that explanation counting as a recorded case.
func notesListEntries(section []string) []string {
	entries := make([]string, 0, len(section))
	for _, line := range section {
		if after, found := strings.CutPrefix(line, "- "); found {
			entries = append(entries, after)
		}
	}
	return entries
}

// matchSectionEntries reports the first disagreement between a
// section's own list entries and the entries the validated run
// produced. The comparison runs both ways on purpose: requiring only
// that every expected entry is present would let a document add a case
// the run never recorded and still validate, which is the drift the
// binding exists to catch.
func matchSectionEntries(section string, got, want []string) error {
	present := make(map[string]bool, len(got))
	for _, entry := range got {
		present[entry] = true
	}
	expected := make(map[string]bool, len(want))
	for _, entry := range want {
		expected[entry] = true
		if !present[entry] {
			return fmt.Errorf("case %q is absent from the %s section", entry, section)
		}
	}
	for _, entry := range got {
		if !expected[entry] {
			return fmt.Errorf("the %s section carries case %q that the validated run does not record", section, entry)
		}
	}
	return nil
}

// ValidateNotes reports the first disagreement between a notes document
// and a validated run's expectation, or nil when they agree. It never
// writes.
func ValidateNotes(document string, want NotesExpectation) error {
	lines := strings.Split(document, "\n")
	trimmed := make([]string, len(lines))
	for i, line := range lines {
		trimmed[i] = strings.TrimSpace(line)
	}

	position := -1
	for _, heading := range NotesSections() {
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

	wantGrades := map[string]NotesGrade{}
	for _, grade := range want.Grades {
		wantGrades[string(grade.Surface)+" "+string(grade.Capability)] = grade
	}
	seenGrades := map[string]bool{}
	for i, line := range trimmed {
		match := notesGradeRowPattern.FindStringSubmatch(line)
		if match == nil {
			// A row shaped like a grade but failing the closed pattern
			// carries a label or grade outside the vocabulary. Skipping
			// it would let a document state a contradictory grade and
			// still validate, so the shape alone is enough to reject.
			if notesGradeRowCandidate.MatchString(line) {
				return fmt.Errorf("notes line %d is shaped like a grade row but carries a status label or grade outside the vocabulary: %q", i+1, line)
			}
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

	excludedHeading := slices.Index(trimmed, "## Excluded capability cases")
	if excludedHeading < 0 {
		return fmt.Errorf("notes heading %q is missing", "## Excluded capability cases")
	}
	unobservedHeading := slices.Index(trimmed, "## Unobserved surfaces")
	if unobservedHeading < 0 {
		return fmt.Errorf("notes heading %q is missing", "## Unobserved surfaces")
	}
	if err := matchSectionEntries("Excluded capability cases", notesListEntries(trimmed[excludedHeading:unobservedHeading]), want.Excluded); err != nil {
		return err
	}

	tail := trimmed[unobservedHeading:]
	tailBody := strings.Join(tail, "\n")
	for _, entry := range want.Excluded {
		if strings.Contains(tailBody, entry) {
			return fmt.Errorf("excluded capability case %q appears in the Unobserved surfaces section, want only the Excluded capability cases section", entry)
		}
	}
	if err := matchSectionEntries("Unobserved surfaces", notesListEntries(tail), want.Unobserved); err != nil {
		return err
	}

	if !strings.Contains(document, NotesScopeStatement) {
		return fmt.Errorf("notes do not state that %s", strings.ToLower(NotesScopeStatement))
	}

	for i, line := range trimmed {
		switch {
		case notesVersionPattern.MatchString(line):
			return fmt.Errorf("notes line %d carries a binary version value", i+1)
		case notesDatePattern.MatchString(line):
			return fmt.Errorf("notes line %d carries a measurement date", i+1)
		case notesEnvValuePattern.MatchString(line):
			return fmt.Errorf("notes line %d carries an environment variable value", i+1)
		}
	}
	return nil
}
