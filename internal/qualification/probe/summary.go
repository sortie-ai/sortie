package probe

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// summaryGrade is one surface-capability grade with the exact status
// label the adapter notes must use for it.
type summaryGrade struct {
	Surface    qualification.Surface
	Capability qualification.Capability
	Grade      qualification.Grade
	Label      string
}

// summarySemantic is one of the semantic-case verdicts.
type summarySemantic struct {
	Surface    qualification.Surface
	Capability qualification.Capability
	Case       qualification.Case
	Verdict    qualification.Outcome
	Grade      qualification.Grade
	Detail     string
}

// summaryAbsentSurface is one declared-absent surface with its
// declared reason, printed next to the verdict so the operator reads
// it beside the eligibility line rather than inferring it from a
// shrunken grade section.
type summaryAbsentSurface struct {
	Surface qualification.Surface
	Reason  string
}

// summaryToken is one token-bearing path with its classification.
type summaryToken struct {
	Surface        qualification.Surface
	EvidencePath   string
	Classification qualification.Grade
}

// summaryContinuation is one surface's continuation outcome.
type summaryContinuation struct {
	Surface qualification.Surface
	Outcome string
	Grade   qualification.Grade
}

// Conclusions is the bounded summary a validated evidence set
// produces. It carries no runtime version, timestamp, session
// identifier, filesystem path, prompt, or secret value.
type Conclusions struct {
	Verdict        qualification.Verdict
	Grades         []summaryGrade
	Semantics      []summarySemantic
	Tokens         []summaryToken
	Continuations  []summaryContinuation
	Workspace      string
	Unobserved     []string
	Blocking       []string
	UnmeasuredRows []string
	Excluded       []string
	// AbsentSurfaces lists every surface the profile declares absent,
	// with its declared reason.
	AbsentSurfaces []summaryAbsentSurface
	// NativeReferenceAbsent reports that this run measured no
	// structured native surface, so every comparison row stands on the
	// protocol surface alone.
	NativeReferenceAbsent bool
}

// ConclusionsFromRecords derives the bounded conclusions from a
// validated non-final evidence set and its computed verdict, against
// the profile the run was collected under.
func ConclusionsFromRecords(records []qualification.Record, verdict qualification.Verdict, profile qualification.RuntimeProfile) (Conclusions, error) {
	conclusions := Conclusions{Verdict: verdict}

	for i := range records {
		rec := &records[i]
		class, err := qualification.ClassifyRecord(rec)
		if err != nil {
			return Conclusions{}, fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		switch class {
		case qualification.RowBaseline, qualification.RowMCPDelivery, qualification.RowPermission:
			conclusions.Grades = append(conclusions.Grades, summaryGrade{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Grade:      rec.Grade,
				Label:      qualification.StatusLabel(rec.Grade),
			})
		case qualification.RowSemantic:
			semantic := summarySemantic{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Verdict:    rec.Outcome,
				Grade:      rec.Grade,
				Detail:     rec.Detail,
			}
			if rec.SemanticCase != nil {
				semantic.Case = *rec.SemanticCase
			}
			conclusions.Semantics = append(conclusions.Semantics, semantic)
			switch rec.Grade {
			case qualification.GradeNotObserved:
				conclusions.Unobserved = append(conclusions.Unobserved,
					fmt.Sprintf("%s %s %s: %s", rec.Surface, rec.Capability, semantic.Case, rec.Outcome))
			case qualification.GradeDeclaredGap:
				conclusions.Excluded = append(conclusions.Excluded,
					fmt.Sprintf("%s %s: declared %s", rec.Capability, semantic.Case, rec.Detail))
			case qualification.GradeNotInducible:
				conclusions.Excluded = append(conclusions.Excluded,
					fmt.Sprintf("%s %s: no inducer", rec.Capability, semantic.Case))
			}
		case qualification.RowToken:
			path := "(no token-bearing path)"
			if rec.EvidencePath != nil {
				path = *rec.EvidencePath
			}
			conclusions.Tokens = append(conclusions.Tokens, summaryToken{
				Surface:        rec.Surface,
				EvidencePath:   path,
				Classification: rec.Grade,
			})
		case qualification.RowContinuationRecall:
			conclusions.Continuations = append(conclusions.Continuations, summaryContinuation{
				Surface: rec.Surface,
				Outcome: rec.Detail,
				Grade:   rec.Grade,
			})
		case qualification.RowWorkspaceSecurity:
			conclusions.Workspace = fmt.Sprintf("%s %s", qualification.StatusLabel(rec.Grade), rec.Detail)
		}
	}

	measured := profile.MeasuredSurfaces()
	wantGrades := len(measured)*len(qualification.ComparisonCapabilities) + 2
	wantSemantics := len(measured) * (len(qualification.CapabilityCases[qualification.CapabilityTurnDisposition]) + len(qualification.CapabilityCases[qualification.CapabilityRetryClassification]))
	wantContinuations := len(measured)
	if len(conclusions.Grades) != wantGrades {
		return Conclusions{}, fmt.Errorf("derived %d capability grades, want %d baselines plus tool server and permission", len(conclusions.Grades), wantGrades)
	}
	if len(conclusions.Semantics) != wantSemantics {
		return Conclusions{}, fmt.Errorf("derived %d semantic verdicts, want %d", len(conclusions.Semantics), wantSemantics)
	}
	if len(conclusions.Continuations) != wantContinuations {
		return Conclusions{}, fmt.Errorf("derived %d continuation outcomes, want %d", len(conclusions.Continuations), wantContinuations)
	}
	if conclusions.Workspace == "" {
		return Conclusions{}, fmt.Errorf("derived no workspace security conclusion")
	}

	slices.SortStableFunc(conclusions.Grades, func(a, b summaryGrade) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		return slices.Index(qualification.Capabilities, a.Capability) - slices.Index(qualification.Capabilities, b.Capability)
	})
	slices.SortStableFunc(conclusions.Semantics, func(a, b summarySemantic) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		if c := slices.Index(qualification.Capabilities, a.Capability) - slices.Index(qualification.Capabilities, b.Capability); c != 0 {
			return c
		}
		return slices.Index(qualification.CapabilityCases[a.Capability], a.Case) - slices.Index(qualification.CapabilityCases[b.Capability], b.Case)
	})
	slices.SortStableFunc(conclusions.Tokens, func(a, b summaryToken) int {
		if c := slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface); c != 0 {
			return c
		}
		return strings.Compare(a.EvidencePath, b.EvidencePath)
	})
	slices.SortStableFunc(conclusions.Continuations, func(a, b summaryContinuation) int {
		return slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface)
	})
	slices.Sort(conclusions.Unobserved)

	// A declared or catalog-owned exclusion carries its grade on every
	// surface its kind requires, so the walk above appends one
	// identical line per surface; collapse those down to the one line
	// per excluded case the summary prints.
	slices.Sort(conclusions.Excluded)
	conclusions.Excluded = slices.Compact(conclusions.Excluded)

	report := qualification.ExplainEligibility(records, profile)
	conclusions.NativeReferenceAbsent = report.NativeReferenceAbsent
	for _, entry := range profile.AbsentSurfaces {
		conclusions.AbsentSurfaces = append(conclusions.AbsentSurfaces, summaryAbsentSurface{
			Surface: entry.Surface,
			Reason:  entry.Reason,
		})
	}
	slices.SortFunc(conclusions.AbsentSurfaces, func(a, b summaryAbsentSurface) int {
		return slices.Index(qualification.Surfaces, a.Surface) - slices.Index(qualification.Surfaces, b.Surface)
	})
	for _, row := range report.Rows {
		switch row.Standing {
		case qualification.StandingBelow:
			conclusions.Blocking = append(conclusions.Blocking, fmt.Sprintf("%s: %s", row.Label, row.Cause))
		case qualification.StandingUnmeasured:
			conclusions.UnmeasuredRows = append(conclusions.UnmeasuredRows, fmt.Sprintf("%s: %s", row.Label, row.Cause))
		}
	}
	slices.Sort(conclusions.Blocking)
	slices.Sort(conclusions.UnmeasuredRows)

	return conclusions, nil
}

// FormatSummary renders the bounded, actionable summary the harness
// prints after final validation.
func FormatSummary(conclusions Conclusions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Eligibility: %s\n", conclusions.Verdict)
	fmt.Fprintf(&b, "%s\n", qualification.VerdictRationale(conclusions.Verdict))
	fmt.Fprint(&b, "Declared-absent surfaces:\n")
	if len(conclusions.AbsentSurfaces) == 0 {
		fmt.Fprint(&b, "none\n")
	}
	for _, entry := range conclusions.AbsentSurfaces {
		fmt.Fprintf(&b, "%s: declared absent (%s); corroborated\n", entry.Surface, entry.Reason)
	}
	if conclusions.NativeReferenceAbsent {
		fmt.Fprint(&b, "no structured native surface was measured: every comparison row stands on the protocol surface alone\n")
	}
	fmt.Fprint(&b, "Blocking rows:\n")
	if len(conclusions.Blocking) == 0 {
		fmt.Fprint(&b, "none\n")
	}
	for _, entry := range conclusions.Blocking {
		fmt.Fprintf(&b, "%s\n", entry)
	}
	fmt.Fprint(&b, "Unmeasured rows:\n")
	if len(conclusions.UnmeasuredRows) == 0 {
		fmt.Fprint(&b, "none\n")
	}
	for _, entry := range conclusions.UnmeasuredRows {
		fmt.Fprintf(&b, "%s\n", entry)
	}
	fmt.Fprint(&b, "Excluded cases:\n")
	if len(conclusions.Excluded) == 0 {
		fmt.Fprint(&b, "none\n")
	}
	for _, entry := range conclusions.Excluded {
		fmt.Fprintf(&b, "%s\n", entry)
	}
	fmt.Fprint(&b, "Capability grades:\n")
	for _, grade := range conclusions.Grades {
		fmt.Fprintf(&b, "%s %s: %s %s\n", grade.Surface, grade.Capability, grade.Label, grade.Grade)
	}
	fmt.Fprint(&b, "Semantic verdicts:\n")
	for _, semantic := range conclusions.Semantics {
		if semantic.Grade == qualification.GradeUsable {
			fmt.Fprintf(&b, "%s %s %s: %s (%s)\n", semantic.Surface, semantic.Capability, semantic.Case, semantic.Verdict, semantic.Grade)
			continue
		}
		fmt.Fprintf(&b, "%s %s %s: %s (%s): %s\n", semantic.Surface, semantic.Capability, semantic.Case, semantic.Verdict, semantic.Grade, semantic.Detail)
	}
	fmt.Fprint(&b, "Token sources:\n")
	for _, token := range conclusions.Tokens {
		fmt.Fprintf(&b, "%s %s: %s\n", token.Surface, token.EvidencePath, token.Classification)
	}
	fmt.Fprint(&b, "Continuation:\n")
	for _, continuation := range conclusions.Continuations {
		fmt.Fprintf(&b, "%s: %s (%s %s)\n", continuation.Surface, continuation.Outcome, qualification.StatusLabel(continuation.Grade), continuation.Grade)
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

// ExpectationFrom maps the bounded summary conclusions to the shared
// runtime-neutral expectation qualification.ValidateNotes compares a
// notes document against.
func ExpectationFrom(conclusions Conclusions) qualification.NotesExpectation {
	grades := make([]qualification.NotesGrade, 0, len(conclusions.Grades))
	for _, grade := range conclusions.Grades {
		grades = append(grades, qualification.NotesGrade{
			Surface:    grade.Surface,
			Capability: grade.Capability,
			Grade:      grade.Grade,
			Label:      grade.Label,
		})
	}
	return qualification.NotesExpectation{
		Verdict:    conclusions.Verdict,
		Grades:     grades,
		Excluded:   conclusions.Excluded,
		Unobserved: conclusions.Unobserved,
	}
}
