package eval

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// evaluatorVersion is the evaluator build that derived a Result. It MUST
// increment in the same change as any edit that changes what re-deriving a
// tracked capture yields; Reemit is what fails when it does not.
const evaluatorVersion = 1

// summaryGrade is one surface-capability grade with the exact status label
// the adapter notes must use for it.
type summaryGrade struct {
	Surface    evidence.Surface
	Capability evidence.Capability
	Grade      evidence.Grade
	Label      string
}

type summarySemantic struct {
	Surface    evidence.Surface
	Capability evidence.Capability
	Case       evidence.Case
	Verdict    evidence.Outcome
	Grade      evidence.Grade
	Detail     string
}

// summaryAbsentSurface is one declared-absent surface with its reason,
// printed beside the eligibility line so the operator reads it there rather
// than inferring it from a shrunken grade section.
type summaryAbsentSurface struct {
	Surface evidence.Surface
	Reason  string
}

type summaryToken struct {
	Surface        evidence.Surface
	EvidencePath   string
	Classification evidence.Grade
}

type summaryContinuation struct {
	Surface evidence.Surface
	Outcome string
	Grade   evidence.Grade
}

// notInducibleAccount phrases a not-inducible case's reason so a real
// surface shortfall never reads as a limit of the measurer.
func notInducibleAccount(reason string) string {
	switch evidence.NotInducibleExclusion(reason) {
	case evidence.ExclusionNotInduced:
		return fmt.Sprintf("not induced (%s), so the case stays unmeasured on this surface", reason)
	case evidence.ExclusionSurfaceSilent:
		return fmt.Sprintf("induced, and the surface reported no outcome (%s), so the case keeps its obligation", reason)
	case evidence.ExclusionNone:
	}
	if reason == evidence.NotInducibleDetail {
		return "no deterministic inducer, so neither the condition nor the surface's account of it was established"
	}
	return fmt.Sprintf("not inducible (%s), a reason no rule covers, so the case keeps its obligation", reason)
}

// Conclusions is the bounded summary a validated evidence set produces. It
// carries no runtime version, timestamp, session identifier, filesystem path,
// prompt, or secret value.
type Conclusions struct {
	// Verdict is the transport-parity answer; Conformance is the product
	// one. Each has its own Blocking/Unmeasured row list below.
	Verdict               evidence.Verdict
	Conformance           evidence.Verdict
	Grades                []summaryGrade
	Semantics             []summarySemantic
	Tokens                []summaryToken
	Continuations         []summaryContinuation
	Workspace             string
	Unobserved            []string
	Blocking              []string
	UnmeasuredRows        []string
	ConformanceBlocking   []string
	ConformanceUnmeasured []string
	Excluded              []string
	AbsentSurfaces        []summaryAbsentSurface
	// NativeReferenceAbsent reports that this run measured no structured
	// native surface, so every comparison row stands on the protocol surface
	// alone.
	NativeReferenceAbsent bool
	// Truncated names each mismatched journal entry whose stream was
	// truncated, reported rather than failed since the discarded middle is
	// not what the recognizer read.
	Truncated []string
}

// conclusionsFromRecords derives the bounded conclusions from a validated
// non-final evidence set and its computed verdict, against the profile the
// run was collected under.
func conclusionsFromRecords(records []evidence.Record, verdict evidence.Verdict, p profile.RuntimeProfile) (Conclusions, error) {
	conclusions := Conclusions{Verdict: verdict}

	for i := range records {
		rec := &records[i]
		class, err := ClassifyRecord(rec)
		if err != nil {
			return Conclusions{}, fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		switch class {
		case evidence.RowBaseline, evidence.RowMCPDelivery, evidence.RowPermission:
			conclusions.Grades = append(conclusions.Grades, summaryGrade{
				Surface:    rec.Surface,
				Capability: rec.Capability,
				Grade:      rec.Grade,
				Label:      evidence.StatusLabel(rec.Grade),
			})
		case evidence.RowSemantic:
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
			case evidence.GradeNotObserved:
				conclusions.Unobserved = append(conclusions.Unobserved,
					fmt.Sprintf("%s %s %s: %s", rec.Surface, rec.Capability, semantic.Case, rec.Outcome))
			case evidence.GradeDeclaredGap:
				conclusions.Excluded = append(conclusions.Excluded,
					fmt.Sprintf("%s %s: declared %s", rec.Capability, semantic.Case, rec.Detail))
			case evidence.GradeNotInducible:
				conclusions.Excluded = append(conclusions.Excluded,
					fmt.Sprintf("%s %s: %s", rec.Capability, semantic.Case, notInducibleAccount(rec.Detail)))
			case evidence.GradeNotApplicable:
				conclusions.Excluded = append(conclusions.Excluded,
					fmt.Sprintf("%s %s: not applicable on %s: %s", rec.Capability, semantic.Case, rec.Surface, rec.Detail))
			}
		case evidence.RowToken:
			path := "(no token-bearing path)"
			if rec.EvidencePath != nil {
				path = *rec.EvidencePath
			}
			conclusions.Tokens = append(conclusions.Tokens, summaryToken{
				Surface:        rec.Surface,
				EvidencePath:   path,
				Classification: rec.Grade,
			})
		case evidence.RowContinuationRecall:
			conclusions.Continuations = append(conclusions.Continuations, summaryContinuation{
				Surface: rec.Surface,
				Outcome: rec.Detail,
				Grade:   rec.Grade,
			})
		case evidence.RowWorkspaceSecurity:
			conclusions.Workspace = fmt.Sprintf("%s %s", evidence.StatusLabel(rec.Grade), rec.Detail)
		}
	}

	measured := p.MeasuredSurfaces()
	wantGrades := len(measured)*len(evidence.ComparisonCapabilities()) + 2
	wantSemantics := len(measured) * (len(evidence.CapabilityCases[evidence.CapabilityTurnDisposition]) + len(evidence.CapabilityCases[evidence.CapabilityRetryClassification]))
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
		if c := slices.Index(evidence.Surfaces, a.Surface) - slices.Index(evidence.Surfaces, b.Surface); c != 0 {
			return c
		}
		return slices.Index(evidence.Capabilities, a.Capability) - slices.Index(evidence.Capabilities, b.Capability)
	})
	slices.SortStableFunc(conclusions.Semantics, func(a, b summarySemantic) int {
		if c := slices.Index(evidence.Surfaces, a.Surface) - slices.Index(evidence.Surfaces, b.Surface); c != 0 {
			return c
		}
		if c := slices.Index(evidence.Capabilities, a.Capability) - slices.Index(evidence.Capabilities, b.Capability); c != 0 {
			return c
		}
		return slices.Index(evidence.CapabilityCases[a.Capability], a.Case) - slices.Index(evidence.CapabilityCases[b.Capability], b.Case)
	})
	slices.SortStableFunc(conclusions.Tokens, func(a, b summaryToken) int {
		if c := slices.Index(evidence.Surfaces, a.Surface) - slices.Index(evidence.Surfaces, b.Surface); c != 0 {
			return c
		}
		return strings.Compare(a.EvidencePath, b.EvidencePath)
	})
	slices.SortStableFunc(conclusions.Continuations, func(a, b summaryContinuation) int {
		return slices.Index(evidence.Surfaces, a.Surface) - slices.Index(evidence.Surfaces, b.Surface)
	})
	slices.Sort(conclusions.Unobserved)

	// A declared or catalog-owned exclusion carries its grade on every
	// surface, so the walk above appends one identical line per surface;
	// collapse to the one line per excluded case the summary prints.
	slices.Sort(conclusions.Excluded)
	conclusions.Excluded = slices.Compact(conclusions.Excluded)

	report := ExplainEligibility(records, p)
	conclusions.Conformance = report.Conformance
	conclusions.NativeReferenceAbsent = report.NativeReferenceAbsent
	for _, entry := range p.AbsentSurfaces {
		conclusions.AbsentSurfaces = append(conclusions.AbsentSurfaces, summaryAbsentSurface{
			Surface: entry.Surface,
			Reason:  entry.Reason,
		})
	}
	slices.SortFunc(conclusions.AbsentSurfaces, func(a, b summaryAbsentSurface) int {
		return slices.Index(evidence.Surfaces, a.Surface) - slices.Index(evidence.Surfaces, b.Surface)
	})
	for _, row := range report.Rows {
		switch row.Standing {
		case StandingBelow:
			conclusions.Blocking = append(conclusions.Blocking, fmt.Sprintf("%s: %s", row.Label, row.Cause))
		case StandingUnmeasured:
			conclusions.UnmeasuredRows = append(conclusions.UnmeasuredRows, fmt.Sprintf("%s: %s", row.Label, row.Cause))
		}
		switch row.Conformance {
		case StandingBelow:
			conclusions.ConformanceBlocking = append(conclusions.ConformanceBlocking, fmt.Sprintf("%s: %s", row.Label, row.ConformanceCause))
		case StandingUnmeasured:
			conclusions.ConformanceUnmeasured = append(conclusions.ConformanceUnmeasured, fmt.Sprintf("%s: %s", row.Label, row.ConformanceCause))
		}
	}
	slices.Sort(conclusions.Blocking)
	slices.Sort(conclusions.UnmeasuredRows)
	slices.Sort(conclusions.ConformanceBlocking)
	slices.Sort(conclusions.ConformanceUnmeasured)

	return conclusions, nil
}

// formatSummary renders the bounded, actionable summary an evaluation
// prints once it finishes. noJournal states, at the top of the report, that
// no observation journal was available, so no grade was re-derived from it.
func formatSummary(conclusions Conclusions, noJournal bool) string {
	var b strings.Builder
	if noJournal {
		fmt.Fprint(&b, "no observation journal was available for this capture, so no grade was re-derived from it\n")
	}
	fmt.Fprintf(&b, "%s%s\n", evidence.NotesEligibilityPrefix, conclusions.Verdict)
	fmt.Fprintf(&b, "%s\n", QuestionRationale(evidence.QuestionTransportParity, conclusions.Verdict))
	fmt.Fprintf(&b, "%s%s\n", evidence.NotesConformancePrefix, conclusions.Conformance)
	fmt.Fprintf(&b, "%s\n", QuestionRationale(evidence.QuestionProductConformance, conclusions.Conformance))
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
	writeSummaryList(&b, "Blocking rows:", conclusions.Blocking)
	writeSummaryList(&b, "Unmeasured rows:", conclusions.UnmeasuredRows)
	writeSummaryList(&b, "Conformance-blocking rows:", conclusions.ConformanceBlocking)
	writeSummaryList(&b, "Conformance-unmeasured rows:", conclusions.ConformanceUnmeasured)
	writeSummaryList(&b, "Excluded cases:", conclusions.Excluded)
	fmt.Fprint(&b, "Capability grades:\n")
	for _, grade := range conclusions.Grades {
		fmt.Fprintf(&b, "%s %s: %s %s\n", grade.Surface, grade.Capability, grade.Label, grade.Grade)
	}
	fmt.Fprint(&b, "Semantic verdicts:\n")
	for _, semantic := range conclusions.Semantics {
		if semantic.Grade == evidence.GradeUsable {
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
		fmt.Fprintf(&b, "%s: %s (%s %s)\n", continuation.Surface, continuation.Outcome, evidence.StatusLabel(continuation.Grade), continuation.Grade)
	}
	fmt.Fprintf(&b, "Workspace security:\n%s\n", conclusions.Workspace)
	writeSummaryList(&b, "Unobserved semantic cases:", conclusions.Unobserved)
	writeSummaryList(&b, "Truncated entries disagreeing with evidence.jsonl:", conclusions.Truncated)
	return b.String()
}

// writeSummaryList renders one heading and its entries, printing "none" for
// an empty list so an empty section is not read as one the summary forgot.
func writeSummaryList(b *strings.Builder, heading string, entries []string) {
	fmt.Fprintf(b, "%s\n", heading)
	if len(entries) == 0 {
		fmt.Fprint(b, "none\n")
	}
	for _, entry := range entries {
		fmt.Fprintf(b, "%s\n", entry)
	}
}

// expectationFrom maps the bounded conclusions to the runtime-neutral
// expectation evidence.ValidateNotes compares a notes document against.
func expectationFrom(conclusions Conclusions) evidence.NotesExpectation {
	grades := make([]evidence.NotesGrade, 0, len(conclusions.Grades))
	for _, grade := range conclusions.Grades {
		grades = append(grades, evidence.NotesGrade{
			Surface:    grade.Surface,
			Capability: grade.Capability,
			Grade:      grade.Grade,
			Label:      grade.Label,
		})
	}
	expectation := evidence.NotesExpectation{
		Verdict:    conclusions.Verdict,
		Grades:     grades,
		Excluded:   conclusions.Excluded,
		Unobserved: conclusions.Unobserved,
	}
	if conclusions.Conformance != "" {
		expectation.Conformance = &conclusions.Conformance
	}
	return expectation
}

// Render produces the operator-facing summary text and the measurement
// document from one Result. measuredAt is the date the runtime was driven,
// never the clock.
func Render(r Result, p profile.RuntimeProfile, measuredAt, requestedModel, provenanceDigest string) (string, evidence.Measurement, error) {
	if provenanceDigest == "" {
		return "", evidence.Measurement{}, fmt.Errorf("the provenance artifact must be digested before the measurement that names it")
	}
	if r.Conformance == "" {
		return "", evidence.Measurement{}, fmt.Errorf("the result carries no product-conformance answer, and a measurement states both of the run's answers")
	}
	summary := formatSummary(r.Conclusions, !r.JournalAvailable)
	measurement := evidence.Measurement{
		SchemaVersion:    evidence.MeasurementSchemaVersion,
		ProfileDigest:    p.Digest(),
		MeasuredAt:       measuredAt,
		Expectation:      expectationFrom(r.Conclusions),
		RequestedModel:   requestedModel,
		ObservedModel:    nil,
		ProvenanceDigest: &provenanceDigest,
		EvaluatorVersion: r.EvaluatorVersion,
	}
	return summary, measurement, nil
}
