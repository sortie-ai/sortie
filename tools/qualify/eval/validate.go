// Package eval re-derives qualification verdicts offline from a saved
// capture. It opens no network connection, launches no subprocess, and
// reads no credential; live capture belongs to tools/qualify/probe.
package eval

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// ClassifyRecord matches one record against the closed tuple table and
// returns its row class, rejecting any tuple the table does not declare.
func ClassifyRecord(rec *evidence.Record) (evidence.RowClass, error) {
	inMeasuredSurfaces := slices.Contains(evidence.MeasurableSurfaces(), rec.Surface)

	switch {
	case rec.Scenario == evidence.ScenarioWorkspaceSecurity &&
		rec.Surface == evidence.SurfaceAggregate &&
		rec.Capability == evidence.CapabilityWorkspaceSecurity &&
		rec.InputID == evidence.InputSecurity && rec.SemanticCase == nil:
		return evidence.RowWorkspaceSecurity, nil

	case rec.Scenario == evidence.ScenarioPolicyPrecondition &&
		rec.Surface == evidence.SurfaceAggregate &&
		rec.Capability == evidence.CapabilityPermissionHandling &&
		rec.InputID == evidence.InputPolicyControl && rec.SemanticCase == nil:
		return evidence.RowPolicyPrecondition, nil

	case rec.Scenario == evidence.ScenarioSemanticProbe &&
		inMeasuredSurfaces && rec.SemanticCase != nil:
		cases, known := evidence.CapabilityCases[rec.Capability]
		if !known || !slices.Contains(cases, *rec.SemanticCase) {
			return evidence.RowNone, fmt.Errorf("semantic case %v is not valid for capability %s", rec.SemanticCase, rec.Capability)
		}
		if want := evidence.CaseInputs[*rec.SemanticCase]; rec.InputID != want {
			return evidence.RowNone, fmt.Errorf("semantic case %v requires input_id %s, got %s", *rec.SemanticCase, want, rec.InputID)
		}
		return evidence.RowSemantic, nil

	case rec.Scenario == evidence.ScenarioSurfaceBaseline &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.InputID == evidence.InputBaseline &&
		slices.Contains(evidence.ComparisonCapabilities(), rec.Capability):
		return evidence.RowBaseline, nil

	case rec.Scenario == evidence.ScenarioTokenSource &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.Capability == evidence.CapabilityTokenCeiling &&
		rec.InputID == evidence.InputTokenInventory:
		return evidence.RowToken, nil

	case rec.Scenario == evidence.ScenarioPermissionRequest &&
		rec.Surface == evidence.SurfaceProtocol &&
		rec.Capability == evidence.CapabilityPermissionHandling &&
		rec.InputID == evidence.InputPermissionProbe && rec.SemanticCase == nil:
		return evidence.RowPermission, nil

	case rec.Scenario == evidence.ScenarioToolServer &&
		rec.Surface == evidence.SurfaceProtocol &&
		rec.Capability == evidence.CapabilityToolServerDelivery &&
		rec.InputID == evidence.InputMCPProbe && rec.SemanticCase == nil:
		return evidence.RowMCPDelivery, nil

	case rec.Scenario == evidence.ScenarioContinuation &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.Capability == evidence.CapabilitySessionContinuation:
		switch rec.InputID {
		case evidence.InputContinuationSeed:
			return evidence.RowContinuationSeed, nil
		case evidence.InputContinuationRecall:
			return evidence.RowContinuationRecall, nil
		}

	case rec.Scenario == evidence.ScenarioRuntimeIdentity &&
		rec.Surface == evidence.SurfaceProtocol &&
		rec.Capability == evidence.CapabilityRuntimeIdentity &&
		rec.InputID == evidence.InputIdentity && rec.SemanticCase == nil:
		return evidence.RowRuntimeIdentity, nil

	case rec.Scenario == evidence.ScenarioProcessCleanup &&
		rec.Surface == evidence.SurfaceAggregate &&
		rec.Capability == evidence.CapabilityProcessCleanup &&
		rec.InputID == evidence.InputCleanup && rec.SemanticCase == nil:
		return evidence.RowProcessCleanup, nil

	case rec.Scenario == evidence.ScenarioEndToEnd &&
		rec.Surface == evidence.SurfaceProtocol &&
		rec.Capability == evidence.CapabilityTurnDisposition &&
		rec.InputID == evidence.InputE2E && rec.SemanticCase == nil:
		return evidence.RowEndToEnd, nil
	}

	return evidence.RowNone, fmt.Errorf("tuple (%s, %s, %s, %s) is outside the closed table", rec.Scenario, rec.Surface, rec.Capability, rec.InputID)
}

func numericGrade(classification evidence.Grade) (int, bool) {
	switch classification {
	case evidence.GradeUsable:
		return 1, true
	case evidence.GradeGap:
		return 0, true
	}
	return 0, false
}

// richestNativeReference combines the structured native surfaces' grades for
// one capability: the highest observed grade, but any member that is
// not_observed or carries no numeric grade makes the reference not_observed.
func richestNativeReference(grades ...evidence.Grade) evidence.Grade {
	best := evidence.Grade("")
	bestRank := -1
	for _, grade := range grades {
		if grade == evidence.GradeNotObserved {
			return evidence.GradeNotObserved
		}
		rank, ok := numericGrade(grade)
		if !ok {
			return evidence.GradeNotObserved
		}
		if best == "" || rank >= bestRank {
			bestRank = rank
			best = grade
		}
	}
	return best
}

func baselineGrades(records []evidence.Record) map[evidence.Surface]map[evidence.Capability]evidence.Grade {
	grades := map[evidence.Surface]map[evidence.Capability]evidence.Grade{}
	for i := range records {
		rec := &records[i]
		if rec.Scenario != evidence.ScenarioSurfaceBaseline {
			continue
		}
		if grades[rec.Surface] == nil {
			grades[rec.Surface] = map[evidence.Capability]evidence.Grade{}
		}
		grades[rec.Surface][rec.Capability] = rec.Grade
	}
	return grades
}

func firstRecordOfClass(records []evidence.Record, class evidence.RowClass) *evidence.Record {
	for i := range records {
		rec := &records[i]
		if got, err := ClassifyRecord(rec); err == nil && got == class {
			return rec
		}
	}
	return nil
}

// Standing is the three-way standing of one load-bearing row.
type Standing string

const (
	StandingSatisfied  Standing = "satisfied"
	StandingBelow      Standing = "below"
	StandingUnmeasured Standing = "unmeasured"
)

// RowOutcome is one load-bearing row's contribution to the two verdicts:
// Standing/Cause answer transport parity, Conformance/ConformanceCause
// answer product conformance.
type RowOutcome struct {
	Label            string
	Standing         Standing
	Cause            string
	Conformance      Standing
	ConformanceCause string
}

// EligibilityReport carries both verdicts and every row that produced them, in
// canonical row order. Verdict answers QuestionTransportParity and Conformance
// answers QuestionProductConformance; neither stands for the other.
type EligibilityReport struct {
	Verdict     evidence.Verdict
	Conformance evidence.Verdict
	Rows        []RowOutcome
	// NativeReferenceAbsent reports that the run measured no structured native
	// surface, so every comparison row stands on the protocol surface alone.
	NativeReferenceAbsent bool
}

// nativeReferenceState is the three-way state of one capability's native
// structured reference.
type nativeReferenceState int

const (
	nativeReferenceGraded nativeReferenceState = iota
	// nativeReferenceIncomplete reports that a measured structured native
	// surface carries no numeric baseline grade.
	nativeReferenceIncomplete
	// nativeReferenceMissing reports that the run measured no structured native
	// surface at all.
	nativeReferenceMissing
)

func presentGrade(grades map[evidence.Surface]map[evidence.Capability]evidence.Grade, surface evidence.Surface, capability evidence.Capability) (evidence.Grade, bool) {
	bySurface, ok := grades[surface]
	if !ok {
		return "", false
	}
	grade, ok := bySurface[capability]
	return grade, ok
}

// nativeReferenceStanding derives one capability's native structured
// reference from the structured native surfaces this run measured.
func nativeReferenceStanding(grades map[evidence.Surface]map[evidence.Capability]evidence.Grade, capability evidence.Capability, measured []evidence.Surface) (evidence.Grade, evidence.Surface, nativeReferenceState) {
	structured := measuredStructuredNatives(measured)
	if len(structured) == 0 {
		return "", "", nativeReferenceMissing
	}
	surfaceGrades := make([]evidence.Grade, 0, len(structured))
	for _, surface := range structured {
		grade, _ := presentGrade(grades, surface, capability)
		if _, ok := numericGrade(grade); !ok {
			return "", surface, nativeReferenceIncomplete
		}
		surfaceGrades = append(surfaceGrades, grade)
	}
	return richestNativeReference(surfaceGrades...), "", nativeReferenceGraded
}

// measuredStructuredNatives returns the structured native surfaces this run
// measured, in evidence.MeasurableSurfaces order. native_text is excluded; it
// is not a structured surface.
func measuredStructuredNatives(measured []evidence.Surface) []evidence.Surface {
	var structured []evidence.Surface
	for _, surface := range evidence.MeasurableSurfaces() {
		if surface != evidence.SurfaceNativeJSON && surface != evidence.SurfaceNativeStreamJSON {
			continue
		}
		if slices.Contains(measured, surface) {
			structured = append(structured, surface)
		}
	}
	return structured
}

// comparableCaseGrades derives, for one capability, each measured surface's
// per-case grade as the comparison reads it: a surface silent on a
// condition that does arise reads as a gap, not a missing obligation.
func comparableCaseGrades(records []evidence.Record, p profile.RuntimeProfile, capability evidence.Capability, measured []evidence.Surface) map[evidence.Surface]map[evidence.Case]evidence.Grade {
	observed := map[evidence.Surface]map[evidence.Case]evidence.Grade{}
	for i := range records {
		rec := &records[i]
		if rec.Scenario != evidence.ScenarioSemanticProbe || rec.Capability != capability || rec.SemanticCase == nil {
			continue
		}
		if observed[rec.Surface] == nil {
			observed[rec.Surface] = map[evidence.Case]evidence.Grade{}
		}
		observed[rec.Surface][*rec.SemanticCase] = rec.Grade
	}

	grades := map[evidence.Surface]map[evidence.Case]evidence.Grade{}
	put := func(surface evidence.Surface, caseID evidence.Case, grade evidence.Grade) {
		if grades[surface] == nil {
			grades[surface] = map[evidence.Case]evidence.Grade{}
		}
		grades[surface][caseID] = grade
	}
	for _, surface := range measured {
		for _, caseID := range evidence.CapabilityCases[capability] {
			grade, ok := observed[surface][caseID]
			if !ok {
				continue
			}
			switch grade {
			case evidence.GradeDeclaredGap, evidence.GradeNotApplicable:
				continue
			case evidence.GradeNotInducible:
				if p.CaseExclusion(surface, capability, caseID) == evidence.ExclusionSurfaceSilent {
					put(surface, caseID, evidence.GradeGap)
				}
				continue
			}
			put(surface, caseID, grade)
		}
	}
	return grades
}

// caseNativeReference derives one case's native reference from the
// structured native surfaces that carry that case's obligation.
func caseNativeReference(grades map[evidence.Surface]map[evidence.Case]evidence.Grade, structured []evidence.Surface, caseID evidence.Case) (evidence.Grade, evidence.Surface, nativeReferenceState) {
	collected := make([]evidence.Grade, 0, len(structured))
	for _, surface := range structured {
		grade, ok := grades[surface][caseID]
		if !ok {
			continue
		}
		if _, numeric := numericGrade(grade); !numeric {
			return "", surface, nativeReferenceIncomplete
		}
		collected = append(collected, grade)
	}
	if len(collected) == 0 {
		return "", "", nativeReferenceMissing
	}
	return richestNativeReference(collected...), "", nativeReferenceGraded
}

// explainSemanticComparisonRow derives one semantic capability's standing case
// by case, so each side answers for the same obligation. A case only one side
// carries settles nothing and drops out for both.
func explainSemanticComparisonRow(records []evidence.Record, p profile.RuntimeProfile, capability evidence.Capability, measured []evidence.Surface) RowOutcome {
	label := string(capability)
	if !slices.Contains(measured, evidence.SurfaceProtocol) {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "protocol surface not measured"}
	}
	structured := measuredStructuredNatives(measured)
	if len(structured) == 0 {
		// With no structured native surface, there is nothing for the protocol
		// surface to be below.
		return RowOutcome{Label: label, Standing: StandingSatisfied}
	}

	grades := comparableCaseGrades(records, p, capability, measured)
	for _, caseID := range evidence.CapabilityCases[capability] {
		protocolGrade, protocolCarried := grades[evidence.SurfaceProtocol][caseID]
		protocolRank, protocolNumeric := numericGrade(protocolGrade)
		if protocolCarried && !protocolNumeric {
			// An obligation the protocol surface carries and nobody measured
			// settles nothing about parity.
			return RowOutcome{
				Label:    label,
				Standing: StandingUnmeasured,
				Cause:    fmt.Sprintf("protocol %s not measured", caseID),
			}
		}
		referenceGrade, unmeasuredSurface, standing := caseNativeReference(grades, structured, caseID)
		switch standing {
		case nativeReferenceMissing:
			continue
		case nativeReferenceIncomplete:
			return RowOutcome{
				Label:    label,
				Standing: StandingUnmeasured,
				Cause:    fmt.Sprintf("native reference incomplete: %s %s not measured", unmeasuredSurface, caseID),
			}
		}
		if !protocolCarried {
			continue
		}
		referenceRank, _ := numericGrade(referenceGrade)
		if protocolRank < referenceRank {
			return RowOutcome{
				Label:    label,
				Standing: StandingBelow,
				Cause:    fmt.Sprintf("protocol %s %s below native reference %s", caseID, protocolGrade, referenceGrade),
			}
		}
	}
	return RowOutcome{Label: label, Standing: StandingSatisfied}
}

// explainComparisonRow derives one comparison capability's standing against the
// ordered condition table. A capability that owns a case set is compared case
// by case against the same obligations on both sides.
func explainComparisonRow(records []evidence.Record, grades map[evidence.Surface]map[evidence.Capability]evidence.Grade, p profile.RuntimeProfile, capability evidence.Capability, measured []evidence.Surface) RowOutcome {
	label := string(capability)
	protocolGrade, present := presentGrade(grades, evidence.SurfaceProtocol, capability)
	if !present {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "baseline record missing"}
	}
	if len(evidence.CapabilityCases[capability]) > 0 {
		return explainSemanticComparisonRow(records, p, capability, measured)
	}
	if protocolGrade == evidence.GradeNotObserved {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "protocol surface not measured"}
	}
	referenceGrade, unmeasuredSurface, standing := nativeReferenceStanding(grades, capability, measured)
	if standing == nativeReferenceIncomplete {
		return RowOutcome{
			Label:    label,
			Standing: StandingUnmeasured,
			Cause:    fmt.Sprintf("native reference incomplete: %s not measured", unmeasuredSurface),
		}
	}
	protocolRank, protocolOK := numericGrade(protocolGrade)
	if !protocolOK {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "protocol surface not measured"}
	}
	if standing == nativeReferenceMissing {
		// With no structured native surface, there is nothing for the protocol
		// surface to be below.
		return RowOutcome{Label: label, Standing: StandingSatisfied}
	}
	referenceRank, referenceOK := numericGrade(referenceGrade)
	if !referenceOK {
		// Blame the side that is actually unmeasured. The caller filters an
		// unranked reference out before this point, so a wrong cause here would
		// misdirect the operator the moment either arm became reachable.
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "native reference not measured"}
	}
	if protocolRank >= referenceRank {
		return RowOutcome{Label: label, Standing: StandingSatisfied}
	}
	return RowOutcome{
		Label:    label,
		Standing: StandingBelow,
		Cause:    fmt.Sprintf("protocol %s below native reference %s", protocolGrade, referenceGrade),
	}
}

// explainSingletonRow derives one singleton row's standing: an absent record, a
// not_observed grade, or a usable-and-pass record against anything else.
func explainSingletonRow(records []evidence.Record, class evidence.RowClass) RowOutcome {
	label := evidence.RowLabel(class)
	rec := firstRecordOfClass(records, class)
	if rec == nil {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "record missing"}
	}
	if rec.Grade == evidence.GradeNotObserved {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: fmt.Sprintf("outcome %s", rec.Outcome)}
	}
	if rec.Grade == evidence.GradeUsable && rec.Outcome == evidence.OutcomePass {
		return RowOutcome{Label: label, Standing: StandingSatisfied}
	}
	return RowOutcome{
		Label:    label,
		Standing: StandingBelow,
		Cause:    fmt.Sprintf("grade %s with outcome %s", rec.Grade, rec.Outcome),
	}
}

// tokenCeilingStopUnobserved is the cause a received spend figure leaves the
// token-ceiling conformance row with: no collection observes a run crossing a
// finite ceiling or the absence of a dispatch after that crossing stopped it.
const tokenCeilingStopUnobserved = "a spend figure reaches Sortie outside the protocol, but no run crossed a finite ceiling, so the stop and the absence of a dispatch after it stay unobserved"

// compensatingRecord returns the record by which Sortie's code supplies one
// capability outside the protocol, or nil. Only a usable reading of a completed
// observation compensates.
func compensatingRecord(records []evidence.Record, capability evidence.Capability) *evidence.Record {
	for i := range records {
		rec := &records[i]
		if rec.Surface != evidence.SurfaceProtocol || rec.Capability != capability {
			continue
		}
		if evidence.SuppliedOutsideProtocol(rec.Source) && rec.Grade == evidence.GradeUsable && rec.Outcome == evidence.OutcomePass {
			return rec
		}
	}
	return nil
}

// conformanceCaseStanding maps one protocol-surface case record onto what
// it says about the effective adapter, reporting whether the case carries
// an obligation.
func conformanceCaseStanding(grade evidence.Grade, exclusion evidence.ExclusionKind) (Standing, bool) {
	switch grade {
	case evidence.GradeUsable:
		return StandingSatisfied, true
	case evidence.GradeGap, evidence.GradeCorroborationOnly:
		return StandingBelow, true
	case evidence.GradeDeclaredGap, evidence.GradeNotApplicable:
		return "", false
	case evidence.GradeNotInducible:
		switch exclusion {
		case evidence.ExclusionSurfaceSilent:
			return StandingBelow, true
		case evidence.ExclusionNotApplicable:
			return "", false
		}
	}
	return StandingUnmeasured, true
}

// explainSemanticConformance derives one semantic capability's product-
// conformance standing. Unlike the baseline derivation, a case the
// measurer could not induce is unmeasured here, never dropped.
func explainSemanticConformance(records []evidence.Record, p profile.RuntimeProfile, capability evidence.Capability) (Standing, string) {
	observed := map[evidence.Case]*evidence.Record{}
	for i := range records {
		rec := &records[i]
		if rec.Scenario == evidence.ScenarioSemanticProbe && rec.Surface == evidence.SurfaceProtocol &&
			rec.Capability == capability && rec.SemanticCase != nil {
			observed[*rec.SemanticCase] = rec
		}
	}

	obligations := 0
	unmeasured := ""
	for _, caseID := range evidence.CapabilityCases[capability] {
		rec, ok := observed[caseID]
		if !ok {
			if unmeasured == "" {
				unmeasured = fmt.Sprintf("protocol %s record missing", caseID)
			}
			continue
		}
		standing, carries := conformanceCaseStanding(rec.Grade, p.CaseExclusion(evidence.SurfaceProtocol, capability, caseID))
		if !carries {
			continue
		}
		obligations++
		switch standing {
		case StandingBelow:
			return StandingBelow, conformanceCaseCause(caseID, rec)
		case StandingUnmeasured:
			if unmeasured == "" {
				unmeasured = conformanceCaseCause(caseID, rec)
			}
		}
	}
	switch {
	case unmeasured != "":
		return StandingUnmeasured, unmeasured
	case obligations == 0:
		return StandingUnmeasured, "no case carries an obligation"
	}
	return StandingSatisfied, ""
}

// conformanceCaseCause names one case's standing on the protocol surface. A
// not-inducible row carries its reason, the only thing separating a condition
// no measurer can induce from one the surface never reports.
func conformanceCaseCause(caseID evidence.Case, rec *evidence.Record) string {
	if rec.Grade == evidence.GradeNotInducible {
		return fmt.Sprintf("protocol %s %s: %s", caseID, rec.Grade, rec.Detail)
	}
	return fmt.Sprintf("protocol %s %s", caseID, rec.Grade)
}

// explainConformanceRow derives one comparison capability's product-
// conformance standing: what the operator gets from the effective adapter.
// A shortfall this runtime's native surfaces share does not excuse it.
func explainConformanceRow(records []evidence.Record, grades map[evidence.Surface]map[evidence.Capability]evidence.Grade, p profile.RuntimeProfile, capability evidence.Capability) (Standing, string) {
	// A reading supplied outside the protocol settles that the figure reaches
	// the operator, but no collection induces the ceiling crossing that
	// capability's row actually asserts, so the row stays unknown.
	if compensatingRecord(records, capability) != nil {
		return StandingUnmeasured, tokenCeilingStopUnobserved
	}
	if len(evidence.CapabilityCases[capability]) > 0 {
		return explainSemanticConformance(records, p, capability)
	}
	grade, present := presentGrade(grades, evidence.SurfaceProtocol, capability)
	if !present {
		return StandingUnmeasured, "baseline record missing"
	}
	switch grade {
	case evidence.GradeUsable:
		return StandingSatisfied, ""
	case evidence.GradeGap:
		return StandingBelow, fmt.Sprintf("protocol %s%s, and nothing outside the protocol supplies it", grade, extensionAccount(records, capability))
	}
	return StandingUnmeasured, fmt.Sprintf("protocol %s%s", grade, extensionAccount(records, capability))
}

// extensionAccount names what the run read of the protocol's extension
// point for capability, distinguishing a present-but-unadmitted source
// from no source at all.
func extensionAccount(records []evidence.Record, capability evidence.Capability) string {
	for i := range records {
		rec := &records[i]
		if rec.Surface != evidence.SurfaceProtocol || rec.Capability != capability || rec.ExtensionSource == nil || rec.ExtensionAdmitted == nil {
			continue
		}
		if *rec.ExtensionSource == evidence.ExtensionSourcePresent && !*rec.ExtensionAdmitted {
			return fmt.Sprintf(" with an extension source %s and not admitted", *rec.ExtensionSource)
		}
		return fmt.Sprintf(" with an extension source %s", *rec.ExtensionSource)
	}
	return ""
}

// singletonRowClasses is the fixed order of the four singleton rows, following
// the four comparison capabilities.
var singletonRowClasses = []evidence.RowClass{evidence.RowPolicyPrecondition, evidence.RowPermission, evidence.RowMCPDelivery, evidence.RowEndToEnd}

// ExplainEligibility derives both verdicts and the per-row standings. It is
// pure and total, never panicking on a missing baseline or row.
func ExplainEligibility(records []evidence.Record, declarations profile.RuntimeProfile) EligibilityReport {
	measured := declarations.DeclaredMeasuredSurfaces()
	grades := baselineGrades(records)
	report := EligibilityReport{
		NativeReferenceAbsent: !slices.Contains(measured, evidence.SurfaceNativeJSON) && !slices.Contains(measured, evidence.SurfaceNativeStreamJSON),
	}
	for _, capability := range evidence.ComparisonCapabilities() {
		row := explainComparisonRow(records, grades, declarations, capability, measured)
		row.Conformance, row.ConformanceCause = explainConformanceRow(records, grades, declarations, capability)
		report.Rows = append(report.Rows, row)
	}
	for _, class := range singletonRowClasses {
		row := explainSingletonRow(records, class)
		// A singleton row reads no native reference, so nothing about it changes
		// when the native surfaces stop being consulted.
		row.Conformance, row.ConformanceCause = row.Standing, row.Cause
		report.Rows = append(report.Rows, row)
	}

	report.Verdict = verdictOver(report.Rows, func(row RowOutcome) Standing { return row.Standing })
	report.Conformance = verdictOver(report.Rows, func(row RowOutcome) Standing { return row.Conformance })
	return report
}

// verdictOver reduces one question's row standings to its verdict, giving a
// below standing priority over an unmeasured one.
func verdictOver(rows []RowOutcome, standingOf func(RowOutcome) Standing) evidence.Verdict {
	below, unmeasured := false, false
	for _, row := range rows {
		switch standingOf(row) {
		case StandingBelow:
			below = true
		case StandingUnmeasured:
			unmeasured = true
		}
	}
	switch {
	case below:
		return evidence.VerdictNotQualified
	case unmeasured:
		return evidence.VerdictUnmeasured
	}
	return evidence.VerdictQualified
}

// ComputeEligibility derives the qualification verdict from the non-final
// records and the declaration set, returning ExplainEligibility(...).Verdict so
// the two cannot disagree.
func ComputeEligibility(records []evidence.Record, declarations profile.RuntimeProfile) evidence.Verdict {
	return ExplainEligibility(records, declarations).Verdict
}

// validateSequence enforces one-based, contiguous, strictly increasing sequence
// numbers in file order.
func validateSequence(records []evidence.Record) error {
	for i, rec := range records {
		if rec.Sequence != i+1 {
			return fmt.Errorf("record at position %d has sequence %d, want %d", i+1, rec.Sequence, i+1)
		}
	}
	return nil
}

// validateOrder enforces the canonical scenario, surface, capability, and
// per-scenario tiebreak ordering across file order.
func validateOrder(records []evidence.Record) error {
	for i := 1; i < len(records); i++ {
		if evidence.OrderCompare(records[i-1], records[i]) > 0 {
			prev, cur := records[i-1], records[i]
			return fmt.Errorf("record %d (%s/%s/%s/%s) is out of canonical order after record %d (%s/%s/%s/%s)",
				i+1, cur.Scenario, cur.Surface, cur.Capability, cur.InputID,
				i, prev.Scenario, prev.Surface, prev.Capability, prev.InputID)
		}
	}
	return nil
}

// setValidation carries the per-record lookups the strict set checks assemble
// as they walk the file.
type setValidation struct {
	records      []evidence.Record
	declarations profile.RuntimeProfile
	measured     []evidence.Surface
	seen         map[string]bool
	counts       map[evidence.RowClass]int
	semantic     map[evidence.Surface]map[evidence.Capability]map[evidence.Case]*evidence.Record
	tokens       map[evidence.Surface][]*evidence.Record
	seeds        map[evidence.Surface]*evidence.Record
	recalls      map[evidence.Surface]*evidence.Record
	identity     map[string]bool
	policy       *evidence.Record
	permission   *evidence.Record
	mcp          *evidence.Record
	e2e          *evidence.Record
}

// validateNonFinalSet enforces the closed first-pass evidence set and
// returns the eligibility verdict.
func validateNonFinalSet(records []evidence.Record, declarations profile.RuntimeProfile) (evidence.Verdict, error) {
	v := &setValidation{
		records:      records,
		declarations: declarations,
		measured:     declarations.DeclaredMeasuredSurfaces(),
		seen:         map[string]bool{},
		counts:       map[evidence.RowClass]int{},
		semantic:     map[evidence.Surface]map[evidence.Capability]map[evidence.Case]*evidence.Record{},
		tokens:       map[evidence.Surface][]*evidence.Record{},
		seeds:        map[evidence.Surface]*evidence.Record{},
		recalls:      map[evidence.Surface]*evidence.Record{},
		identity:     map[string]bool{},
	}

	if err := v.checkClosedScenario(); err != nil {
		return "", err
	}
	if err := validateOrder(records); err != nil {
		return "", err
	}
	if err := v.ClassifyRecords(); err != nil {
		return "", err
	}
	if err := v.checkDeclaredAbsentSurfaces(); err != nil {
		return "", err
	}
	if err := v.checkExcludedCases(declarations); err != nil {
		return "", err
	}
	if err := v.checkContinuationRelations(); err != nil {
		return "", err
	}
	if err := v.checkRecallRecords(); err != nil {
		return "", err
	}
	if err := v.checkTokenInventories(); err != nil {
		return "", err
	}
	if err := v.checkDerivedBaselines(); err != nil {
		return "", err
	}
	if err := v.checkIdentityCoverage(); err != nil {
		return "", err
	}
	if err := v.checkCardinality(); err != nil {
		return "", err
	}
	return ComputeEligibility(records, declarations), nil
}

// checkDeclaredAbsentSurfaces rejects any record whose surface the declaration
// set names absent: it describes neither the runtime nor the declaration
// honestly.
func (v *setValidation) checkDeclaredAbsentSurfaces() error {
	for i := range v.records {
		rec := &v.records[i]
		if reason, declared := v.declarations.AbsentSurfaceDeclared(rec.Surface); declared {
			return fmt.Errorf("record %d: surface %s is declared absent (%s), so it cannot carry a record", rec.Sequence, rec.Surface, reason)
		}
	}
	return nil
}

// checkClosedScenario rejects any final qualification record from the non-final
// set.
func (v *setValidation) checkClosedScenario() error {
	for i := range v.records {
		rec := &v.records[i]
		if rec.Scenario == evidence.ScenarioQualification {
			return fmt.Errorf("record %d carries scenario %s, which belongs to the final pass only", rec.Sequence, rec.Scenario)
		}
		if rec.Capability == evidence.CapabilityEligibility {
			return fmt.Errorf("record %d carries capability %s, which belongs to the final pass only", rec.Sequence, rec.Capability)
		}
	}
	return nil
}

// ClassifyRecords classifies every record against the closed table and
// applies its per-record field rules.
func (v *setValidation) ClassifyRecords() error {
	for i := range v.records {
		rec := &v.records[i]
		class, err := ClassifyRecord(rec)
		if err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		v.counts[class]++

		if (rec.Grade == evidence.GradeNotApplicable || rec.Outcome == evidence.OutcomeNotApplicable) && class == evidence.RowSemantic {
			admitted := rec.Surface == evidence.SurfaceProtocol &&
				rec.Capability == evidence.CapabilityRetryClassification &&
				rec.SemanticCase != nil && *rec.SemanticCase == evidence.CaseHumanInput
			if !admitted {
				return fmt.Errorf("record %d: not_applicable is invalid for surface %s capability %s case %v", rec.Sequence, rec.Surface, rec.Capability, rec.SemanticCase)
			}
		}
		if err := CheckOutcomeGradePairing(rec); err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		if rec.Grade == evidence.GradeCorroborationOnly &&
			(class != evidence.RowToken || rec.EvidencePath == nil) {
			return fmt.Errorf("record %d: corroboration_only is valid only on a non-sentinel token_source record", rec.Sequence)
		}
		if (rec.Grade == evidence.GradeDeclaredGap || rec.Grade == evidence.GradeNotInducible) && class != evidence.RowSemantic {
			return fmt.Errorf("record %d: %s is valid only on a semantic probe record", rec.Sequence, rec.Grade)
		}

		if rec.Grade == evidence.GradeDeclaredGap && rec.EvidencePath == nil {
			return fmt.Errorf("record %d: declared_gap record must carry a non-null evidence_path", rec.Sequence)
		}

		pathNullAllowed := (class == evidence.RowSemantic && rec.Grade == evidence.GradeNotObserved) ||
			(class == evidence.RowSemantic && rec.Grade == evidence.GradeNotInducible) ||
			(class == evidence.RowToken && rec.EvidencePath == nil)
		if rec.EvidencePath == nil && !pathNullAllowed {
			return fmt.Errorf("record %d: evidence_path must be set for a %s record", rec.Sequence, evidence.RowLabel(class))
		}
		if rec.Grade == evidence.GradeNotInducible && rec.EvidencePath != nil {
			return fmt.Errorf("record %d: not_inducible record must carry a null evidence_path", rec.Sequence)
		}
		if rec.Grade == evidence.GradeDeclaredGap && !slices.Contains(evidence.DeclarableSurfaces, rec.Surface) {
			return fmt.Errorf("record %d: declared_gap record must carry a surface in DeclarableSurfaces, got %s", rec.Sequence, rec.Surface)
		}
		if rec.Grade == evidence.GradeDeclaredGap && !slices.Contains(evidence.DeclaredGapReasons, rec.Detail) {
			return fmt.Errorf("record %d: declared_gap record detail %q is outside the closed reason set", rec.Sequence, rec.Detail)
		}
		if rec.Grade == evidence.GradeNotInducible && !evidence.IsNotInducibleDetail(rec.Detail) {
			return fmt.Errorf("record %d: not_inducible record detail %q is outside the closed reason set", rec.Sequence, rec.Detail)
		}

		if rec.PriorSessionID != nil && class != evidence.RowContinuationRecall {
			return fmt.Errorf("record %d: prior_session_id is only valid on a continuation recall record", rec.Sequence)
		}
		if rec.ProtocolVersion != nil && rec.Surface != evidence.SurfaceProtocol {
			return fmt.Errorf("record %d: protocol_version is only valid on protocol records", rec.Sequence)
		}
		if rec.AgentName != nil && rec.Surface != evidence.SurfaceProtocol {
			return fmt.Errorf("record %d: agent_name is only valid on protocol records", rec.Sequence)
		}
		if err := checkExtensionReading(rec, class); err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}

		if err := v.checkUniqueness(rec, class); err != nil {
			return err
		}
		if class == evidence.RowContinuationRecall {
			// Recall records are checked later, once seeds and recalls are
			// known, so a prior id that resolves to another surface's seed
			// reports that failure rather than a detail mismatch.
		} else if err := v.checkSessionRelation(rec, class); err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		v.indexRecord(rec, class)
	}
	return v.checkSemanticSessionRelations()
}

// checkExtensionReading enforces where a reading of the protocol's
// extension point may be stated and which combinations of its two members
// mean anything.
func checkExtensionReading(rec *evidence.Record, class evidence.RowClass) error {
	onProtocolInventory := class == evidence.RowToken && rec.Surface == evidence.SurfaceProtocol
	if !onProtocolInventory && (rec.ExtensionSource != nil || rec.ExtensionAdmitted != nil) {
		return errors.New("an extension reading is only valid on a protocol token_source record")
	}
	if rec.ExtensionSource == nil && rec.ExtensionAdmitted != nil {
		return errors.New("extension_admitted requires the extension_source it judges")
	}
	if rec.ExtensionSource != nil && rec.ExtensionAdmitted == nil {
		return errors.New("extension_source requires the admission verdict on it")
	}
	if rec.ExtensionSource != nil && *rec.ExtensionSource != evidence.ExtensionSourcePresent && *rec.ExtensionAdmitted {
		return fmt.Errorf("extension_source %s cannot be admitted to a budget", *rec.ExtensionSource)
	}
	return nil
}

// CheckOutcomeGradePairing enforces the closed pairing between a record's
// outcome and its grade. It is exported so a collector building one record at a
// time can check it against the same rule the set validator applies.
func CheckOutcomeGradePairing(rec *evidence.Record) error {
	classification := rec.Grade
	verdict := rec.Outcome
	switch classification {
	case evidence.GradeUsable, evidence.GradeGap, evidence.GradeCorroborationOnly:
		if verdict != evidence.OutcomePass {
			return fmt.Errorf("classification %s requires verdict pass, got %s", classification, verdict)
		}
	case evidence.GradeNotObserved:
		if verdict == evidence.OutcomePass || verdict == evidence.OutcomeNotApplicable {
			return fmt.Errorf("classification not_observed is inconsistent with verdict %s", verdict)
		}
	case evidence.GradeNotApplicable:
		if verdict != evidence.OutcomeNotApplicable {
			return fmt.Errorf("classification not_applicable requires verdict not_applicable, got %s", verdict)
		}
	case evidence.GradeDeclaredGap:
		if verdict != evidence.OutcomeNotProducible {
			return fmt.Errorf("classification declared_gap requires verdict not_producible, got %s", verdict)
		}
	case evidence.GradeNotInducible:
		if verdict != evidence.OutcomeNotInducible {
			return fmt.Errorf("classification not_inducible requires verdict not_inducible, got %s", verdict)
		}
	case evidence.GradeQualified, evidence.GradeNotQualified, evidence.GradeUnmeasured:
		return fmt.Errorf("classification %s is valid only for capability eligibility", classification)
	}
	switch verdict {
	case evidence.OutcomePass:
		if classification != evidence.GradeUsable &&
			classification != evidence.GradeGap &&
			classification != evidence.GradeCorroborationOnly {
			return fmt.Errorf("verdict pass requires classification usable, gap, or corroboration_only, got %s", classification)
		}
	case evidence.OutcomeNotObserved:
		if classification != evidence.GradeNotObserved {
			return fmt.Errorf("verdict not_observed requires classification not_observed, got %s", classification)
		}
	case evidence.OutcomeNotApplicable:
		if classification != evidence.GradeNotApplicable {
			return fmt.Errorf("verdict not_applicable requires classification not_applicable, got %s", classification)
		}
	case evidence.OutcomePrerequisiteFailed, evidence.OutcomeFixtureInductionFailed, evidence.OutcomeAdapterUnanswered, evidence.OutcomeRuntimeFailed:
		if classification != evidence.GradeNotObserved {
			return fmt.Errorf("verdict %s requires classification not_observed, got %s", verdict, classification)
		}
	case evidence.OutcomeNotProducible:
		if classification != evidence.GradeDeclaredGap {
			return fmt.Errorf("verdict not_producible requires classification declared_gap, got %s", classification)
		}
	case evidence.OutcomeNotInducible:
		if classification != evidence.GradeNotInducible {
			return fmt.Errorf("verdict not_inducible requires classification not_inducible, got %s", classification)
		}
	}
	return nil
}

// checkUniqueness enforces the per-class uniqueness key and counts each
// key once.
func (v *setValidation) checkUniqueness(rec *evidence.Record, class evidence.RowClass) error {
	var key string
	switch class {
	case evidence.RowWorkspaceSecurity:
		key = "workspace"
	case evidence.RowPolicyPrecondition:
		key = "policy"
	case evidence.RowPermission:
		key = "permission"
	case evidence.RowMCPDelivery:
		key = "mcp"
	case evidence.RowProcessCleanup:
		key = "cleanup"
	case evidence.RowEndToEnd:
		key = "e2e"
	case evidence.RowSemantic:
		key = fmt.Sprintf("semantic|%s|%s|%s", rec.Surface, rec.Capability, *rec.SemanticCase)
	case evidence.RowBaseline:
		key = fmt.Sprintf("baseline|%s|%s", rec.Surface, rec.Capability)
	case evidence.RowToken:
		key = fmt.Sprintf("token|%s|%s", rec.Surface, nullableKey(rec.EvidencePath))
	case evidence.RowContinuationSeed:
		key = fmt.Sprintf("seed|%s", rec.Surface)
	case evidence.RowContinuationRecall:
		key = fmt.Sprintf("recall|%s", rec.Surface)
	case evidence.RowRuntimeIdentity:
		key = fmt.Sprintf("identity|%s", nullableKey(rec.SessionID))
	default:
		return nil
	}
	if v.seen[key] {
		return fmt.Errorf("record %d: duplicate uniqueness key %q", rec.Sequence, key)
	}
	v.seen[key] = true
	return nil
}

func nullableKey(v *string) string {
	if v == nil {
		return "<null>"
	}
	return *v
}

// checkSessionRelation enforces the per-class session_id rules.
func (v *setValidation) checkSessionRelation(rec *evidence.Record, class evidence.RowClass) error {
	switch class {
	case evidence.RowWorkspaceSecurity, evidence.RowProcessCleanup, evidence.RowBaseline:
		if rec.SessionID != nil {
			return fmt.Errorf("%s record must use a null session_id", evidence.RowLabel(class))
		}
	case evidence.RowPolicyPrecondition, evidence.RowPermission, evidence.RowMCPDelivery:
		if rec.SessionID == nil {
			return fmt.Errorf("%s record must reference a non-null session_id", evidence.RowLabel(class))
		}
	case evidence.RowEndToEnd:
		// A launch that never started has no session; one generated to fill the
		// member would read as a session the workflow actually ran in.
		if rec.SessionID == nil && rec.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("%s record reporting an observed run must reference the session it ran in", evidence.RowLabel(class))
		}
	case evidence.RowContinuationSeed:
		// A surface whose runtime reports no identifier names none here; an
		// invented one would be indistinguishable from a reported one.
	case evidence.RowRuntimeIdentity:
		if rec.SessionID == nil {
			return fmt.Errorf("%s record must reference a non-null session_id", evidence.RowLabel(class))
		}
		if rec.ProtocolVersion == nil {
			return fmt.Errorf("runtime identity record must carry the session's negotiated protocol_version")
		}
		if rec.Grade == evidence.GradeUsable &&
			(rec.AgentName == nil || rec.AgentVersion == nil) {
			return fmt.Errorf("usable runtime identity record must carry agent_name and agent_version")
		}
	case evidence.RowToken:
		if rec.EvidencePath == nil {
			if rec.SessionID != nil {
				return fmt.Errorf("token sentinel record must use a null session_id")
			}
			if rec.Source != evidence.SourceNone {
				return fmt.Errorf("token sentinel record must carry source none, got %s", rec.Source)
			}
			if rec.Grade != evidence.GradeGap && rec.Grade != evidence.GradeNotObserved {
				return fmt.Errorf("token sentinel classification = %s, want gap or not_observed", rec.Grade)
			}
		} else {
			if rec.SessionID == nil {
				return fmt.Errorf("token inventory record must reference the session that emitted the path")
			}
			if rec.Source == evidence.SourceNone {
				return fmt.Errorf("token record with a path must not carry source none")
			}
			if rec.Grade != evidence.GradeUsable && rec.Grade != evidence.GradeCorroborationOnly {
				return fmt.Errorf("non-sentinel token classification = %s, want usable or corroboration_only", rec.Grade)
			}
		}
	case evidence.RowContinuationRecall:
		return nil
	case evidence.RowSemantic:
		if rec.Surface == evidence.SurfaceProtocol && rec.Outcome == evidence.OutcomePass && rec.SessionID == nil {
			return fmt.Errorf("passing semantic probe must carry its own session_id")
		}
		if rec.Grade == evidence.GradeDeclaredGap && rec.SessionID == nil {
			return fmt.Errorf("declared_gap record must carry its own session_id")
		}
		if rec.Grade == evidence.GradeNotApplicable && rec.SessionID == nil {
			return fmt.Errorf("not_applicable record must carry its own session_id")
		}
		if rec.Grade == evidence.GradeNotInducible && rec.SessionID != nil {
			return fmt.Errorf("not_inducible record must carry a null session_id")
		}
	}
	return nil
}

// checkRecallRecords validates every surface's recall record after the
// continuation relations are known.
func (v *setValidation) checkRecallRecords() error {
	for _, surface := range v.measured {
		recall := v.recalls[surface]
		if recall == nil {
			continue
		}
		if err := v.checkRecallRecord(recall); err != nil {
			return fmt.Errorf("record %d: %w", recall.Sequence, err)
		}
	}
	return nil
}

// checkRecallRecord enforces the closed detail set and the session relation
// each recall outcome requires.
func (v *setValidation) checkRecallRecord(rec *evidence.Record) error {
	if rec.PriorSessionID == nil && recallDetailNamesPriorSession(rec.Detail) {
		return fmt.Errorf("recall detail %q requires the prior session id the seed reported", rec.Detail)
	}
	switch rec.Detail {
	case evidence.RecallConfirmedSameSession:
		if !sameSessionRef(rec.SessionID, rec.PriorSessionID) {
			return fmt.Errorf("confirmed_same_session requires the actual and prior session ids to agree")
		}
		if rec.Grade != evidence.GradeUsable {
			return fmt.Errorf("confirmed_same_session requires classification usable, got %s", rec.Grade)
		}
	case evidence.RecallFreshFallback:
		if rec.SessionID == nil || *rec.SessionID == *rec.PriorSessionID {
			return fmt.Errorf("fresh_session_fallback requires a non-null actual session id distinct from prior_session_id")
		}
		if rec.Grade != evidence.GradeGap {
			return fmt.Errorf("fresh_session_fallback requires classification gap, got %s", rec.Grade)
		}
	case evidence.RecallSameSessionWithoutRecall:
		if rec.SessionID == nil || *rec.SessionID != *rec.PriorSessionID {
			return fmt.Errorf("same_session_without_recall requires equal non-null actual and prior session ids")
		}
		if rec.Grade != evidence.GradeGap {
			return fmt.Errorf("same_session_without_recall requires classification gap, got %s", rec.Grade)
		}
	case evidence.RecallDeclined:
		if rec.SessionID == nil || *rec.SessionID != *rec.PriorSessionID {
			return fmt.Errorf("same_session_answer_declined requires equal non-null actual and prior session ids")
		}
		if rec.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("same_session_answer_declined requires classification not_observed, got %s", rec.Grade)
		}
		if rec.Outcome != evidence.OutcomeFixtureInductionFailed {
			return fmt.Errorf("same_session_answer_declined requires verdict fixture_induction_failed, got %s", rec.Outcome)
		}
	case evidence.RecallUnobservedActual:
		if rec.SessionID != nil {
			return fmt.Errorf("unobserved_actual_session requires a null session_id")
		}
		if rec.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("unobserved_actual_session requires classification not_observed, got %s", rec.Grade)
		}
	case evidence.RecallPreconditionUnmet:
		if rec.SessionID != nil {
			return fmt.Errorf("recall_precondition_unmet requires a null session_id")
		}
		if rec.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("recall_precondition_unmet requires classification not_observed, got %s", rec.Grade)
		}
		if rec.Outcome != evidence.OutcomePrerequisiteFailed {
			return fmt.Errorf("recall_precondition_unmet requires verdict prerequisite_failed, got %s", rec.Outcome)
		}
	default:
		return fmt.Errorf("recall detail %q is outside the closed set", rec.Detail)
	}
	return nil
}

// recallDetailNamesPriorSession reports whether a recall detail states a
// relation to the seed's session, which only a seed that named one can carry.
func recallDetailNamesPriorSession(detail string) bool {
	switch detail {
	case evidence.RecallFreshFallback, evidence.RecallSameSessionWithoutRecall, evidence.RecallDeclined:
		return true
	}
	return false
}

// sameSessionRef reports whether two session references name the same session.
// Two absent references agree: a surface that names no session names none on
// either row.
func sameSessionRef(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// indexRecord files a classified record into the lookups the relation and
// derivation checks consume.
func (v *setValidation) indexRecord(rec *evidence.Record, class evidence.RowClass) {
	switch class {
	case evidence.RowPolicyPrecondition:
		v.policy = rec
	case evidence.RowPermission:
		v.permission = rec
	case evidence.RowMCPDelivery:
		v.mcp = rec
	case evidence.RowEndToEnd:
		v.e2e = rec
	case evidence.RowSemantic:
		if v.semantic[rec.Surface] == nil {
			v.semantic[rec.Surface] = map[evidence.Capability]map[evidence.Case]*evidence.Record{}
		}
		if v.semantic[rec.Surface][rec.Capability] == nil {
			v.semantic[rec.Surface][rec.Capability] = map[evidence.Case]*evidence.Record{}
		}
		v.semantic[rec.Surface][rec.Capability][*rec.SemanticCase] = rec
	case evidence.RowToken:
		v.tokens[rec.Surface] = append(v.tokens[rec.Surface], rec)
	case evidence.RowContinuationSeed:
		v.seeds[rec.Surface] = rec
	case evidence.RowContinuationRecall:
		v.recalls[rec.Surface] = rec
	case evidence.RowRuntimeIdentity:
		if rec.SessionID != nil {
			v.identity[*rec.SessionID] = true
		}
	}
}

// checkSemanticSessionRelations enforces the session-reuse rules between
// semantic records that share a physical run.
func (v *setValidation) checkSemanticSessionRelations() error {
	for _, surface := range v.measured {
		dispositionRefusal := v.semanticRecord(surface, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)
		retryRefusal := v.semanticRecord(surface, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal)
		if dispositionRefusal != nil && retryRefusal != nil &&
			dispositionRefusal.SessionID != nil && retryRefusal.SessionID != nil {
			// A side carrying no session made no such run, so the comparison is
			// skipped rather than failed.
			if evidence.CompareNullableString(dispositionRefusal.SessionID, retryRefusal.SessionID) != 0 {
				return fmt.Errorf("%s refusal retry record does not reuse the matching disposition-refusal session id", surface)
			}
		}
	}

	if v.permission != nil {
		humanInput := v.semanticRecord(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput)
		if humanInput != nil && humanInput.SessionID != nil && *humanInput.SessionID != *v.permission.SessionID {
			return fmt.Errorf("protocol human_input record does not reuse the permission attempt session id")
		}
	}
	return nil
}

func (v *setValidation) semanticRecord(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) *evidence.Record {
	return v.semantic[surface][capability][caseID]
}

// checkExcludedCases enforces the closed excluded-case rules for every
// (capability, case) pair, then requires every declaration to match a record,
// mirroring checkIdentityCoverage's bidirectional shape.
func (v *setValidation) checkExcludedCases(declarations profile.RuntimeProfile) error {
	for _, capability := range evidence.Capabilities {
		cases, ok := evidence.CapabilityCases[capability]
		if !ok {
			continue
		}
		for _, caseID := range cases {
			if err := v.checkExcludedCase(declarations, capability, caseID); err != nil {
				return err
			}
		}
	}

	for _, entry := range declarations.Declarations {
		matched := false
		for _, surface := range evidence.DeclarableSurfaces {
			if rec := v.semanticRecord(surface, entry.Capability, entry.Case); rec != nil && rec.Grade == evidence.GradeDeclaredGap {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("declaration for capability %s case %s matches no declared_gap record", entry.Capability, entry.Case)
		}
	}
	return nil
}

// notInducibleRequiredSurfaces returns the surface set a not_inducible
// grade must cover for one (capability, case) pair, excluding any
// declared-absent surface.
func notInducibleRequiredSurfaces(declarations profile.RuntimeProfile, caseID evidence.Case, measured []evidence.Surface) []evidence.Surface {
	if slices.Contains(evidence.CatalogNotInducibleCases, caseID) {
		return measured
	}
	var required []evidence.Surface
	for _, entry := range declarations.NotInducibleCases {
		if entry.Case == caseID {
			required = append(required, entry.Surface)
		}
	}
	return evidence.IntersectSurfaces(required, measured)
}

// checkExcludedCase enforces the closed excluded-case rules for one
// (capability, case) pair.
func (v *setValidation) checkExcludedCase(declarations profile.RuntimeProfile, capability evidence.Capability, caseID evidence.Case) error {
	var declared, catalog []evidence.Surface
	var declaredDetail string
	for _, surface := range v.measured {
		rec := v.semanticRecord(surface, capability, caseID)
		if rec == nil {
			continue
		}
		switch rec.Grade {
		case evidence.GradeDeclaredGap:
			declared = append(declared, surface)
			declaredDetail = rec.Detail
		case evidence.GradeNotInducible:
			catalog = append(catalog, surface)
		}
	}

	declarableMeasured := evidence.IntersectSurfaces(evidence.DeclarableSurfaces, v.measured)
	if len(declared) > 0 && len(catalog) > 0 {
		return fmt.Errorf("capability %s case %s carries both a declared gap and a not-inducible grade", capability, caseID)
	}
	if len(declared) > 0 {
		if missing := missingSurfaces(declared, declarableMeasured); len(missing) > 0 {
			return fmt.Errorf("capability %s case %s declared gap is missing on surfaces %v", capability, caseID, missing)
		}
	}
	if len(catalog) > 0 {
		required := notInducibleRequiredSurfaces(declarations, caseID, v.measured)
		if missing := missingSurfaces(catalog, required); len(missing) > 0 {
			return fmt.Errorf("capability %s case %s not-inducible grade is missing on surfaces %v", capability, caseID, missing)
		}
		if extra := missingSurfaces(required, catalog); len(extra) > 0 {
			return fmt.Errorf("capability %s case %s carries an unexpected not-inducible grade on surfaces %v", capability, caseID, extra)
		}
	}
	if len(declared) == 0 && len(catalog) == 0 {
		return nil
	}

	if len(declared) > 0 {
		for _, surface := range declared {
			if rec := v.semanticRecord(surface, capability, caseID); rec.Detail != declaredDetail {
				return fmt.Errorf("capability %s case %s carries differing declared-gap details", capability, caseID)
			}
		}
		if _, found := declarations.Declared(capability, caseID); !found {
			return fmt.Errorf("capability %s case %s carries a declared gap no declaration authorizes", capability, caseID)
		}
		if peer, hasPeer := evidence.DeclaredGapPeers[caseID]; hasPeer {
			peerCapability := evidence.CapabilityOwning(peer)
			var peerDeclared []evidence.Surface
			var peerDetail string
			for _, surface := range v.measured {
				rec := v.semanticRecord(surface, peerCapability, peer)
				if rec == nil || rec.Grade != evidence.GradeDeclaredGap {
					continue
				}
				peerDeclared = append(peerDeclared, surface)
				peerDetail = rec.Detail
			}
			if missing := missingSurfaces(peerDeclared, declarableMeasured); len(missing) > 0 {
				return fmt.Errorf("capability %s case %s's peer %s case %s does not carry a declared gap on surfaces %v", capability, caseID, peerCapability, peer, missing)
			}
			if peerDetail != declaredDetail {
				return fmt.Errorf("capability %s case %s and its peer %s case %s carry differing declared-gap reasons", capability, caseID, peerCapability, peer)
			}
		}
	}
	if len(catalog) > 0 {
		for _, surface := range catalog {
			rec := v.semanticRecord(surface, capability, caseID)
			reason, scoped := declarations.NotInducibleDeclared(surface, caseID)
			if scoped && rec.Detail != reason {
				return fmt.Errorf("capability %s case %s not-inducible record on surface %s carries detail %q, want the reason %q the profile scopes to that surface", capability, caseID, surface, rec.Detail, reason)
			}
			if !scoped && rec.Detail != evidence.NotInducibleDetail {
				return fmt.Errorf("capability %s case %s not-inducible record on surface %s carries detail %q, want %q: no profile entry scopes the case to that surface", capability, caseID, surface, rec.Detail, evidence.NotInducibleDetail)
			}
		}
	}
	return nil
}

func missingSurfaces(have, want []evidence.Surface) []evidence.Surface {
	var missing []evidence.Surface
	for _, surface := range want {
		if !slices.Contains(have, surface) {
			missing = append(missing, surface)
		}
	}
	return missing
}

// checkContinuationRelations enforces that every non-null prior_session_id
// resolves to exactly one same-surface seed and that each surface carries one
// seed and one recall.
func (v *setValidation) checkContinuationRelations() error {
	for _, surface := range v.measured {
		if v.seeds[surface] == nil {
			return fmt.Errorf("surface %s has no continuation seed record", surface)
		}
		if v.recalls[surface] == nil {
			return fmt.Errorf("surface %s has no continuation recall record", surface)
		}
		recall := v.recalls[surface]
		seed := v.seeds[surface]
		switch {
		case seed.SessionID == nil && recall.PriorSessionID != nil:
			return fmt.Errorf("surface %s recall prior_session_id %s resolves to nothing: that surface's seed named no session", surface, *recall.PriorSessionID)
		case seed.SessionID != nil && recall.PriorSessionID == nil:
			return fmt.Errorf("surface %s recall record carries no prior_session_id", surface)
		case seed.SessionID != nil && *seed.SessionID != *recall.PriorSessionID:
			return fmt.Errorf("surface %s recall prior_session_id %s does not resolve to that surface's seed session", surface, *recall.PriorSessionID)
		}
	}
	return nil
}

// checkTokenInventories enforces the sentinel rules over the inventory each
// surface itself carried. A reading supplied outside the protocol is not part
// of that inventory.
func (v *setValidation) checkTokenInventories() error {
	for _, surface := range v.measured {
		records := evidence.SurfaceBorne(v.tokens[surface])
		if len(records) == 0 {
			return fmt.Errorf("surface %s has no token inventory record", surface)
		}
		sentinels := 0
		nonSentinels := 0
		for _, rec := range records {
			if rec.EvidencePath == nil {
				sentinels++
			} else {
				nonSentinels++
			}
		}
		if sentinels > 1 {
			return fmt.Errorf("surface %s has %d token sentinel records, want at most 1", surface, sentinels)
		}
		if sentinels == 1 && nonSentinels > 0 {
			return fmt.Errorf("surface %s carries both a token sentinel and non-sentinel records; they are mutually exclusive", surface)
		}
		if err := checkExtensionAgreement(surface, records); err != nil {
			return err
		}
	}
	return nil
}

// checkExtensionAgreement requires one inventory to state one reading, since
// every row of it answers for the same collection.
func checkExtensionAgreement(surface evidence.Surface, records []*evidence.Record) error {
	first := records[0]
	for _, rec := range records[1:] {
		if !evidence.NullableEqual(rec.ExtensionSource, first.ExtensionSource) ||
			!evidence.NullableEqual(rec.ExtensionAdmitted, first.ExtensionAdmitted) {
			return fmt.Errorf("surface %s token inventory states more than one extension reading", surface)
		}
	}
	return nil
}

// checkDerivedBaselines requires every written baseline grade to equal the
// grade derived from the records that own it: the disposition and retry case
// records per surface, the per-surface token inventory, and the recall outcome.
func (v *setValidation) checkDerivedBaselines() error {
	grades := baselineGrades(v.records)

	for _, surface := range v.measured {
		for _, capability := range evidence.ComparisonCapabilities() {
			written, ok := grades[surface][capability]
			if !ok {
				return fmt.Errorf("surface %s has no %s baseline record", surface, capability)
			}
			var derived evidence.Grade
			var contributing []evidence.Outcome
			switch capability {
			case evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification:
				var classes []evidence.Grade
				allExcluded := true
				for _, caseID := range evidence.CapabilityCases[capability] {
					rec := v.semanticRecord(surface, capability, caseID)
					if rec == nil {
						return fmt.Errorf("surface %s is missing its %s %s semantic record", surface, capability, caseID)
					}
					class := evidence.BaselineClassification(rec.Grade, rec.Detail)
					classes = append(classes, class)
					contributing = append(contributing, rec.Outcome)
					if class != evidence.GradeDeclaredGap && class != evidence.GradeNotInducible && class != evidence.GradeNotApplicable {
						allExcluded = false
					}
				}
				if allExcluded {
					return fmt.Errorf("surface %s capability %s has every case excluded, leaving no case to derive a baseline from", surface, capability)
				}
				derived = evidence.DeriveBaselineGrade(classes)
			case evidence.CapabilityTokenCeiling:
				derived = evidence.TokenBaselineGrade(v.tokens[surface])
				for _, rec := range v.tokens[surface] {
					contributing = append(contributing, rec.Outcome)
				}
			case evidence.CapabilitySessionContinuation:
				derived = v.recalls[surface].Grade
				contributing = []evidence.Outcome{v.recalls[surface].Outcome}
			}
			if written != derived {
				return fmt.Errorf("surface %s %s baseline = %s, want derived %s", surface, capability, written, derived)
			}
			var baseline *evidence.Record
			for i := range v.records {
				if evidence.MatchBaseline(surface, capability)(&v.records[i]) {
					baseline = &v.records[i]
					break
				}
			}
			if baseline == nil {
				return fmt.Errorf("surface %s has no %s baseline record", surface, capability)
			}
			wantOutcome := evidence.DeriveBaselineOutcome(derived, contributing)
			if baseline.Outcome != wantOutcome {
				return fmt.Errorf("surface %s %s baseline outcome = %s, want derived %s", surface, capability, baseline.Outcome, wantOutcome)
			}
		}
	}
	return nil
}

// checkIdentityCoverage requires exactly one runtime-identity record per
// referenced protocol session id, skipping an identity record while
// collecting ids so it cannot satisfy its own coverage.
func (v *setValidation) checkIdentityCoverage() error {
	referenced := map[string]bool{}
	for i := range v.records {
		rec := &v.records[i]
		if rec.Scenario == evidence.ScenarioRuntimeIdentity {
			continue
		}
		if rec.Surface == evidence.SurfaceProtocol && rec.SessionID != nil {
			referenced[*rec.SessionID] = true
		}
	}

	for id := range referenced {
		if !v.identity[id] {
			return fmt.Errorf("protocol session %s is referenced by non-final evidence but carries no runtime identity record", id)
		}
	}
	for id := range v.identity {
		if !referenced[id] {
			return fmt.Errorf("runtime identity record for protocol session %s references no non-final evidence", id)
		}
	}
	return nil
}

// singletonRowCount is the per-scenario count of the six singleton
// scenarios, summed by checkCardinality so a changed count here cannot
// drift from the total it is checked against.
const singletonRowCount = 1

// checkCardinality enforces the closed cardinality formula fixed+T+N: the
// singleton and per-surface row counts, the token-inventory count, and the
// distinct protocol session ids referenced.
func (v *setValidation) checkCardinality() error {
	fixed := map[evidence.RowClass]int{
		evidence.RowWorkspaceSecurity:  singletonRowCount,
		evidence.RowPolicyPrecondition: singletonRowCount,
		evidence.RowPermission:         singletonRowCount,
		evidence.RowMCPDelivery:        singletonRowCount,
		evidence.RowProcessCleanup:     singletonRowCount,
		evidence.RowEndToEnd:           singletonRowCount,
	}
	fixedTotal := 0
	for class, want := range fixed {
		fixedTotal += want
		if err := checkCount(evidence.RowLabel(class), v.counts[class], want); err != nil {
			return err
		}
	}

	measuredCount := len(v.measured)
	dispositionCount := len(evidence.CapabilityCases[evidence.CapabilityTurnDisposition]) * measuredCount
	retryCount := len(evidence.CapabilityCases[evidence.CapabilityRetryClassification]) * measuredCount
	surfaceBaselineCount := len(evidence.ComparisonCapabilities()) * measuredCount
	continuationSeedCount := measuredCount
	continuationRecallCount := measuredCount

	if err := checkCount("disposition semantic", v.countSemanticByCapability(evidence.CapabilityTurnDisposition), dispositionCount); err != nil {
		return err
	}
	if err := checkCount("retry semantic", v.countSemanticByCapability(evidence.CapabilityRetryClassification), retryCount); err != nil {
		return err
	}
	if err := checkCount("surface baseline", v.counts[evidence.RowBaseline], surfaceBaselineCount); err != nil {
		return err
	}
	if err := checkCount("continuation seed", v.counts[evidence.RowContinuationSeed], continuationSeedCount); err != nil {
		return err
	}
	if err := checkCount("continuation recall", v.counts[evidence.RowContinuationRecall], continuationRecallCount); err != nil {
		return err
	}
	fixedTotal += dispositionCount + retryCount + surfaceBaselineCount + continuationSeedCount + continuationRecallCount

	T := 0
	for _, records := range v.tokens {
		T += len(records)
	}
	N := len(v.identity)
	want := fixedTotal + T + N
	if len(v.records) != want {
		return fmt.Errorf("evidence set holds %d records, want exactly %d (%d fixed + %d token + %d identity)", len(v.records), want, fixedTotal, T, N)
	}
	return nil
}

// countSemanticByCapability counts classified semantic records of one
// capability, excluding the derived baselines that share its name.
func (v *setValidation) countSemanticByCapability(capability evidence.Capability) int {
	count := 0
	for i := range v.records {
		if v.records[i].Scenario == evidence.ScenarioSemanticProbe && v.records[i].Capability == capability {
			count++
		}
	}
	return count
}

// checkCount reports a cardinality mismatch, naming a duplicate or a missing
// record so controls can pin the cause.
func checkCount(name string, got, want int) error {
	if got == want {
		return nil
	}
	if got > want {
		return fmt.Errorf("%s row holds %d records, want exactly %d (duplicate record present)", name, got, want)
	}
	return fmt.Errorf("%s row holds %d records, want exactly %d (required record missing)", name, got, want)
}

// AggregateGradeFor maps a verdict to the grade the final aggregate record
// carries. It is total over Verdicts; a value outside the set returns the zero
// value rather than panicking.
func AggregateGradeFor(verdict evidence.Verdict) evidence.Grade {
	switch verdict {
	case evidence.VerdictQualified:
		return evidence.GradeQualified
	case evidence.VerdictNotQualified:
		return evidence.GradeNotQualified
	case evidence.VerdictUnmeasured:
		return evidence.GradeUnmeasured
	}
	return ""
}

// VerdictRationale returns the operator-facing rationale line for a
// transport-parity verdict.
func VerdictRationale(verdict evidence.Verdict) string {
	return QuestionRationale(evidence.QuestionTransportParity, verdict)
}

// QuestionRationale returns the operator-facing rationale line for one
// question's verdict. It is total over Questions and Verdicts, names no
// runtime, and returns the zero value for a value outside either set.
func QuestionRationale(question evidence.Question, verdict evidence.Verdict) string {
	switch question {
	case evidence.QuestionTransportParity:
		switch verdict {
		case evidence.VerdictQualified:
			return "Every load-bearing row was measured, and where a native reference was measured, the protocol surface was not below it."
		case evidence.VerdictNotQualified:
			return "The protocol surface is below the richest measured native reference on at least one load-bearing row, so the protocol route would cost this runtime's operator something the native route gave them."
		case evidence.VerdictUnmeasured:
			return "At least one load-bearing row was not measured. This runtime waits; re-run the profile after the causes listed below are removed."
		}
	case evidence.QuestionProductConformance:
		switch verdict {
		case evidence.VerdictQualified:
			return "The effective adapter meets every load-bearing obligation, counting what Sortie's own code supplies outside the protocol."
		case evidence.VerdictNotQualified:
			return "The effective adapter does not meet at least one load-bearing obligation, and nothing outside the protocol supplies it. The capability does not work for the operator on this runtime, whichever route they take."
		case evidence.VerdictUnmeasured:
			return "At least one load-bearing obligation was not measured, so whether the effective adapter meets it is unknown; re-run the profile after the causes listed below are removed."
		}
	}
	return ""
}

// ValidateObservations validates the closed non-final set in one evidence file
// against the declaration set the capture ran under, rejecting any final
// qualification record.
func ValidateObservations(path string, p profile.RuntimeProfile) (evidence.Verdict, error) {
	records, err := evidence.ReadEvidenceFile(path)
	if err != nil {
		return "", err
	}
	if err := validateSequence(records); err != nil {
		return "", err
	}
	return validateNonFinalSet(records, p)
}

// The final-pass aggregate causes, wrapped by ValidateEvidence so controls can
// pin the exact rejection.
var (
	errFinalRecordMissing    = errors.New("final qualification record missing from the evidence file")
	errFinalRecordDuplicated = errors.New("final qualification record is duplicated in the evidence file")
	errRecordAfterAggregate  = errors.New("a record follows the final qualification record; the aggregate must be last")
)

// ValidateEvidence validates that set plus exactly one terminal aggregate
// record last, equal to an independent recomputation, against the declaration
// set the capture ran under.
func ValidateEvidence(path string, p profile.RuntimeProfile) (evidence.Verdict, error) {
	records, err := evidence.ReadEvidenceFile(path)
	if err != nil {
		return "", err
	}
	if err := validateSequence(records); err != nil {
		return "", err
	}

	finalIndex := -1
	finalCount := 0
	for i := range records {
		if records[i].Scenario == evidence.ScenarioQualification || records[i].Capability == evidence.CapabilityEligibility {
			finalCount++
			finalIndex = i
		}
	}
	switch {
	case finalCount == 0:
		return "", errFinalRecordMissing
	case finalCount > 1:
		return "", fmt.Errorf("evidence file holds %d final qualification records: %w", finalCount, errFinalRecordDuplicated)
	case finalIndex != len(records)-1:
		return "", fmt.Errorf("record %d: %w", finalIndex+2, errRecordAfterAggregate)
	}
	aggregate := records[finalIndex]
	nonFinal := records[:finalIndex]

	// The final record is located by either half of its tuple so a stray one
	// anywhere is counted and rejected above. That leaves the located record's
	// own tuple unchecked, so it is closed here.
	if aggregate.Scenario != evidence.ScenarioQualification || aggregate.Surface != evidence.SurfaceAggregate ||
		aggregate.Capability != evidence.CapabilityEligibility || aggregate.Source != evidence.SourceComparison {
		return "", fmt.Errorf(
			"final qualification record tuple = %s/%s/%s/%s, want %s/%s/%s/%s",
			aggregate.Scenario, aggregate.Surface, aggregate.Capability, aggregate.Source,
			evidence.ScenarioQualification, evidence.SurfaceAggregate, evidence.CapabilityEligibility, evidence.SourceComparison)
	}
	if aggregate.Outcome != evidence.OutcomePass {
		return "", fmt.Errorf("final qualification record verdict = %s, want pass", aggregate.Outcome)
	}
	if aggregate.InputID != evidence.InputAggregate {
		return "", fmt.Errorf("final qualification record input_id = %s, want %s", aggregate.InputID, evidence.InputAggregate)
	}
	if aggregate.EvidencePath == nil || *aggregate.EvidencePath != evidence.EvidencePathQualificationVerdict {
		return "", fmt.Errorf("final qualification record evidence_path must be %s", evidence.EvidencePathQualificationVerdict)
	}
	if aggregate.SessionID != nil || aggregate.PriorSessionID != nil || aggregate.SemanticCase != nil ||
		aggregate.AgentName != nil || aggregate.AgentVersion != nil || aggregate.ProtocolVersion != nil {
		return "", fmt.Errorf("final qualification record must carry null session, prior, semantic case, agent, and protocol fields")
	}

	verdict, err := validateNonFinalSet(nonFinal, p)
	if err != nil {
		return "", err
	}
	want := AggregateGradeFor(verdict)
	if aggregate.Grade != want {
		return "", fmt.Errorf("final qualification record classification = %s, want %s from independent recomputation", aggregate.Grade, want)
	}
	return verdict, nil
}
