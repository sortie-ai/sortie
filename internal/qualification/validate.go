package qualification

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// RowClass names one record class from the closed non-final tuple
// table. Every non-final record must classify into exactly one class.
type RowClass int

const (
	RowNone RowClass = iota
	RowWorkspaceSecurity
	RowPolicyPrecondition
	RowSemantic
	RowBaseline
	RowToken
	RowPermission
	RowMCPDelivery
	RowContinuationSeed
	RowContinuationRecall
	RowRuntimeIdentity
	RowProcessCleanup
	RowEndToEnd
)

// rowLabel returns the operator-facing name of one row class, used
// in validation diagnostics.
func rowLabel(class RowClass) string {
	switch class {
	case RowWorkspaceSecurity:
		return "workspace security"
	case RowPolicyPrecondition:
		return "policy precondition"
	case RowSemantic:
		return "semantic probe"
	case RowBaseline:
		return "surface baseline"
	case RowToken:
		return "token inventory"
	case RowPermission:
		return "permission handling"
	case RowMCPDelivery:
		return "tool server delivery"
	case RowContinuationSeed:
		return "continuation seed"
	case RowContinuationRecall:
		return "continuation recall"
	case RowRuntimeIdentity:
		return "runtime identity"
	case RowProcessCleanup:
		return "process cleanup"
	case RowEndToEnd:
		return "end to end"
	}
	return "unclassified"
}

// The four comparison capabilities eligibility is decided over, and the
// four surfaces the closed table covers.
var (
	comparisonCapabilities = []Capability{
		CapabilityTurnDisposition, CapabilityRetryClassification,
		CapabilityTokenCeiling, CapabilitySessionContinuation,
	}
	measuredSurfaces = []Surface{
		SurfaceProtocol, SurfaceNativeText,
		SurfaceNativeJSON, SurfaceNativeStreamJSON,
	}
)

// scenarioWriteOrder is the canonical record write order, which
// determines sequence. It differs from the closed enum declaration order
// in Scenarios, which only bounds valid values.
var scenarioWriteOrder = []Scenario{
	ScenarioWorkspaceSecurity, ScenarioPolicyPrecondition,
	ScenarioSemanticProbe, ScenarioSurfaceBaseline,
	ScenarioTokenSource, ScenarioPermissionRequest,
	ScenarioToolServer, ScenarioContinuation,
	ScenarioRuntimeIdentity, ScenarioEndToEnd,
	ScenarioProcessCleanup, ScenarioQualification,
}

// The closed recall detail set for continuation recall records.
const (
	recallConfirmedSameSession = "confirmed_same_session"
	recallFreshFallback        = "fresh_session_fallback"
	recallUnobservedActual     = "unobserved_actual_session"
)

// ClassifyRecord matches one record against the closed tuple table
// and returns its row class, rejecting any tuple the table does not
// declare.
func ClassifyRecord(rec *Record) (RowClass, error) {
	inMeasuredSurfaces := slices.Contains(measuredSurfaces, rec.Surface)

	switch {
	case rec.Scenario == ScenarioWorkspaceSecurity &&
		rec.Surface == SurfaceAggregate &&
		rec.Capability == CapabilityWorkspaceSecurity &&
		rec.InputID == InputSecurity && rec.SemanticCase == nil:
		return RowWorkspaceSecurity, nil

	case rec.Scenario == ScenarioPolicyPrecondition &&
		rec.Surface == SurfaceAggregate &&
		rec.Capability == CapabilityPermissionHandling &&
		rec.InputID == InputPolicyControl && rec.SemanticCase == nil:
		return RowPolicyPrecondition, nil

	case rec.Scenario == ScenarioSemanticProbe &&
		inMeasuredSurfaces && rec.SemanticCase != nil:
		cases, known := CapabilityCases[rec.Capability]
		if !known || !slices.Contains(cases, *rec.SemanticCase) {
			return RowNone, fmt.Errorf("semantic case %v is not valid for capability %s", rec.SemanticCase, rec.Capability)
		}
		if want := CaseInputs[*rec.SemanticCase]; rec.InputID != want {
			return RowNone, fmt.Errorf("semantic case %v requires input_id %s, got %s", *rec.SemanticCase, want, rec.InputID)
		}
		return RowSemantic, nil

	case rec.Scenario == ScenarioSurfaceBaseline &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.InputID == InputBaseline &&
		slices.Contains(comparisonCapabilities, rec.Capability):
		return RowBaseline, nil

	case rec.Scenario == ScenarioTokenSource &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.Capability == CapabilityTokenCeiling &&
		rec.InputID == InputTokenInventory:
		return RowToken, nil

	case rec.Scenario == ScenarioPermissionRequest &&
		rec.Surface == SurfaceProtocol &&
		rec.Capability == CapabilityPermissionHandling &&
		rec.InputID == InputPermissionProbe && rec.SemanticCase == nil:
		return RowPermission, nil

	case rec.Scenario == ScenarioToolServer &&
		rec.Surface == SurfaceProtocol &&
		rec.Capability == CapabilityToolServerDelivery &&
		rec.InputID == InputMCPProbe && rec.SemanticCase == nil:
		return RowMCPDelivery, nil

	case rec.Scenario == ScenarioContinuation &&
		inMeasuredSurfaces && rec.SemanticCase == nil &&
		rec.Capability == CapabilitySessionContinuation:
		switch rec.InputID {
		case InputContinuationSeed:
			return RowContinuationSeed, nil
		case InputContinuationRecall:
			return RowContinuationRecall, nil
		}

	case rec.Scenario == ScenarioRuntimeIdentity &&
		rec.Surface == SurfaceProtocol &&
		rec.Capability == CapabilityRuntimeIdentity &&
		rec.InputID == InputIdentity && rec.SemanticCase == nil:
		return RowRuntimeIdentity, nil

	case rec.Scenario == ScenarioProcessCleanup &&
		rec.Surface == SurfaceAggregate &&
		rec.Capability == CapabilityProcessCleanup &&
		rec.InputID == InputCleanup && rec.SemanticCase == nil:
		return RowProcessCleanup, nil

	case rec.Scenario == ScenarioEndToEnd &&
		rec.Surface == SurfaceProtocol &&
		rec.Capability == CapabilityTurnDisposition &&
		rec.InputID == InputE2E && rec.SemanticCase == nil:
		return RowEndToEnd, nil
	}

	return RowNone, fmt.Errorf("tuple (%s, %s, %s, %s) is outside the closed table", rec.Scenario, rec.Surface, rec.Capability, rec.InputID)
}

// rank returns the canonical position of value in order, or -1.
func rank[T ~string](order []T, value T) int {
	return slices.Index(order, value)
}

// compareNullableString orders two nullable strings with null
// first.
func compareNullableString(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return strings.Compare(*a, *b)
}

// OrderCompare orders two records by the canonical write order:
// the closed scenario write order, then surface order, then capability
// order, then a scenario-specific tiebreak (semantic case, token path
// with the null sentinel first, session identifier, or input catalog
// order). It is the single source of truth for canonical order: every
// collector that writes an evidence file must sort with this
// function rather than reimplementing the rule, so a collector's
// write order and the validator's own order check can never drift
// apart.
func OrderCompare(a, b Record) int {
	if c := cmp.Compare(rank(scenarioWriteOrder, a.Scenario), rank(scenarioWriteOrder, b.Scenario)); c != 0 {
		return c
	}
	if c := cmp.Compare(rank(Surfaces, a.Surface), rank(Surfaces, b.Surface)); c != 0 {
		return c
	}
	if c := cmp.Compare(rank(Capabilities, a.Capability), rank(Capabilities, b.Capability)); c != 0 {
		return c
	}

	switch a.Scenario {
	case ScenarioSemanticProbe:
		var aCase, bCase Case
		if a.SemanticCase != nil {
			aCase = *a.SemanticCase
		}
		if b.SemanticCase != nil {
			bCase = *b.SemanticCase
		}
		return cmp.Compare(rank(CapabilityCases[a.Capability], aCase), rank(CapabilityCases[b.Capability], bCase))
	case ScenarioTokenSource:
		return compareNullableString(a.EvidencePath, b.EvidencePath)
	case ScenarioRuntimeIdentity:
		return compareNullableString(a.SessionID, b.SessionID)
	}
	return cmp.Compare(rank(InputIDs, a.InputID), rank(InputIDs, b.InputID))
}

// numericGrade returns the comparison order of a grade. usable
// outranks gap; corroboration_only and not_observed carry no numeric
// grade and cannot participate in a positive qualification.
func numericGrade(classification Grade) (int, bool) {
	switch classification {
	case GradeUsable:
		return 1, true
	case GradeGap:
		return 0, true
	}
	return 0, false
}

// DeriveBaselineGrade derives one capability's per-surface baseline
// grade from that capability's own case classifications only. It
// first drops every declared_gap and not_inducible entry; an empty
// remainder derives not_observed. Otherwise, any remaining not_observed
// case makes the grade not_observed; else any gap case makes it gap;
// only all-usable cases yield usable. Cases of the other capability
// never influence the result.
func DeriveBaselineGrade(classifications []Grade) Grade {
	remaining := make([]Grade, 0, len(classifications))
	for _, c := range classifications {
		if c == GradeDeclaredGap || c == GradeNotInducible {
			continue
		}
		remaining = append(remaining, c)
	}
	if len(remaining) == 0 {
		return GradeNotObserved
	}
	allUsable := true
	for _, c := range remaining {
		switch c {
		case GradeNotObserved:
			return GradeNotObserved
		case GradeUsable:
		default:
			allUsable = false
		}
	}
	if allUsable {
		return GradeUsable
	}
	return GradeGap
}

// richestNativeReference combines the native JSON and native
// stream-JSON grades for one capability. The reference is the higher
// observed grade, but if either surface is not_observed the reference is
// not_observed even when the other surface is usable.
func richestNativeReference(a, b Grade) Grade {
	if a == GradeNotObserved || b == GradeNotObserved {
		return GradeNotObserved
	}
	aGrade, aOK := numericGrade(a)
	bGrade, bOK := numericGrade(b)
	if !aOK || !bOK {
		return GradeNotObserved
	}
	if aGrade >= bGrade {
		return a
	}
	return b
}

// baselineGrades extracts the written per-surface baseline grades
// from a record set.
func baselineGrades(records []Record) map[Surface]map[Capability]Grade {
	grades := map[Surface]map[Capability]Grade{}
	for i := range records {
		rec := &records[i]
		if rec.Scenario != ScenarioSurfaceBaseline {
			continue
		}
		if grades[rec.Surface] == nil {
			grades[rec.Surface] = map[Capability]Grade{}
		}
		grades[rec.Surface][rec.Capability] = rec.Grade
	}
	return grades
}

// firstRecordOfClass returns the first record of one row class, or
// nil.
func firstRecordOfClass(records []Record, class RowClass) *Record {
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

// RowOutcome is one load-bearing row's contribution to the verdict.
// Cause is assembled only from closed vocabulary and is empty when
// Standing is StandingSatisfied.
type RowOutcome struct {
	Label    string
	Standing Standing
	Cause    string
}

// EligibilityReport carries the verdict and every row that produced
// it, in canonical row order.
type EligibilityReport struct {
	Verdict Verdict
	Rows    []RowOutcome
}

// presentGrade reports a surface-capability baseline grade and whether
// a baseline record for it exists at all.
func presentGrade(grades map[Surface]map[Capability]Grade, surface Surface, capability Capability) (Grade, bool) {
	bySurface, ok := grades[surface]
	if !ok {
		return "", false
	}
	grade, ok := bySurface[capability]
	return grade, ok
}

// nativeReferenceStanding derives one capability's native structured
// reference: the higher-ranked of the two structured native surfaces'
// baseline grades, or an unmeasured standing naming the first
// unmeasured structured surface in measuredSurfaces order. native_text
// stays excluded; it is not a structured surface.
func nativeReferenceStanding(grades map[Surface]map[Capability]Grade, capability Capability) (Grade, Surface, bool) {
	for _, surface := range measuredSurfaces {
		if surface != SurfaceNativeJSON && surface != SurfaceNativeStreamJSON {
			continue
		}
		grade, _ := presentGrade(grades, surface, capability)
		if _, ok := numericGrade(grade); !ok {
			return "", surface, true
		}
	}
	reference := richestNativeReference(
		grades[SurfaceNativeJSON][capability],
		grades[SurfaceNativeStreamJSON][capability],
	)
	return reference, "", false
}

// explainComparisonRow derives one comparison capability's standing
// against the ordered condition table: an absent or unmeasured
// protocol baseline, an incomplete native reference, or a rank
// comparison between the two.
func explainComparisonRow(grades map[Surface]map[Capability]Grade, capability Capability) RowOutcome {
	label := string(capability)
	protocolGrade, present := presentGrade(grades, SurfaceProtocol, capability)
	if !present {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "baseline record missing"}
	}
	if protocolGrade == GradeNotObserved {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "protocol surface not measured"}
	}
	referenceGrade, unmeasuredSurface, unmeasured := nativeReferenceStanding(grades, capability)
	if unmeasured {
		return RowOutcome{
			Label:    label,
			Standing: StandingUnmeasured,
			Cause:    fmt.Sprintf("native reference incomplete: %s not measured", unmeasuredSurface),
		}
	}
	protocolRank, protocolOK := numericGrade(protocolGrade)
	referenceRank, referenceOK := numericGrade(referenceGrade)
	if !protocolOK {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "protocol surface not measured"}
	}
	if !referenceOK {
		// Blame the side that is actually unmeasured. The caller filters
		// an unranked reference out before this point, so neither arm is
		// reachable today; a wrong cause here would send the operator to
		// the wrong surface the moment one of them becomes reachable.
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

// explainSingletonRow derives one singleton row's standing against the
// ordered condition table: an absent record, a not_observed grade, or
// a usable-and-pass record against anything else.
func explainSingletonRow(records []Record, class RowClass) RowOutcome {
	label := rowLabel(class)
	rec := firstRecordOfClass(records, class)
	if rec == nil {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: "record missing"}
	}
	if rec.Grade == GradeNotObserved {
		return RowOutcome{Label: label, Standing: StandingUnmeasured, Cause: fmt.Sprintf("outcome %s", rec.Outcome)}
	}
	if rec.Grade == GradeUsable && rec.Outcome == OutcomePass {
		return RowOutcome{Label: label, Standing: StandingSatisfied}
	}
	return RowOutcome{
		Label:    label,
		Standing: StandingBelow,
		Cause:    fmt.Sprintf("grade %s with outcome %s", rec.Grade, rec.Outcome),
	}
}

// singletonRowClasses is the fixed order of the four singleton rows in
// an EligibilityReport, following the four comparison capabilities.
var singletonRowClasses = []RowClass{RowPolicyPrecondition, RowPermission, RowMCPDelivery, RowEndToEnd}

// ExplainEligibility derives the verdict and the per-row standings from
// the non-final records alone. It is pure and total: it returns a
// report for any record slice, including an empty one, without
// panicking on a missing baseline or a missing singleton row.
func ExplainEligibility(records []Record) EligibilityReport {
	grades := baselineGrades(records)
	report := EligibilityReport{}
	for _, capability := range comparisonCapabilities {
		report.Rows = append(report.Rows, explainComparisonRow(grades, capability))
	}
	for _, class := range singletonRowClasses {
		report.Rows = append(report.Rows, explainSingletonRow(records, class))
	}

	below, unmeasured := false, false
	for _, row := range report.Rows {
		switch row.Standing {
		case StandingBelow:
			below = true
		case StandingUnmeasured:
			unmeasured = true
		}
	}
	switch {
	case below:
		report.Verdict = VerdictNotQualified
	case unmeasured:
		report.Verdict = VerdictUnmeasured
	default:
		report.Verdict = VerdictQualified
	}
	return report
}

// ComputeEligibility derives the qualification verdict from the
// non-final records alone, returning ExplainEligibility(records).Verdict
// so the two can never disagree.
func ComputeEligibility(records []Record) Verdict {
	return ExplainEligibility(records).Verdict
}

// readEvidenceFile reads a JSONL evidence file and strictly decodes
// every line. An empty file is invalid.
func readEvidenceFile(path string) ([]Record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller supplies a path to a file it wrote under its own temp directory
	if err != nil {
		return nil, err
	}
	var records []Record
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rec, err := DecodeRecord([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil, errors.New("evidence file contains no records")
	}
	return records, nil
}

// validateSequence enforces one-based, contiguous, strictly
// increasing sequence numbers in file order.
func validateSequence(records []Record) error {
	for i, rec := range records {
		if rec.Sequence != i+1 {
			return fmt.Errorf("record at position %d has sequence %d, want %d", i+1, rec.Sequence, i+1)
		}
	}
	return nil
}

// validateOrder enforces the canonical scenario, surface,
// capability, and per-scenario tiebreak ordering across file order.
func validateOrder(records []Record) error {
	for i := 1; i < len(records); i++ {
		if OrderCompare(records[i-1], records[i]) > 0 {
			prev, cur := records[i-1], records[i]
			return fmt.Errorf("record %d (%s/%s/%s/%s) is out of canonical order after record %d (%s/%s/%s/%s)",
				i+1, cur.Scenario, cur.Surface, cur.Capability, cur.InputID,
				i, prev.Scenario, prev.Surface, prev.Capability, prev.InputID)
		}
	}
	return nil
}

// setValidation carries the per-record lookups the strict set
// checks assemble as they walk the file.
type setValidation struct {
	records    []Record
	seen       map[string]bool
	counts     map[RowClass]int
	semantic   map[Surface]map[Capability]map[Case]*Record
	tokens     map[Surface][]*Record
	seeds      map[Surface]*Record
	recalls    map[Surface]*Record
	identity   map[string]bool
	policy     *Record
	permission *Record
	mcp        *Record
	e2e        *Record
}

// validateNonFinalSet enforces the closed first-pass evidence set:
// closed tuples, per-record field rules, uniqueness keys, session
// relations, token sentinel rules, derived baselines, runtime-identity
// coverage, and the derived cardinality formula. It returns the
// eligibility verdict computed from the records.
func validateNonFinalSet(records []Record, declarations DeclaredGapSet) (Verdict, error) {
	v := &setValidation{
		records:  records,
		seen:     map[string]bool{},
		counts:   map[RowClass]int{},
		semantic: map[Surface]map[Capability]map[Case]*Record{},
		tokens:   map[Surface][]*Record{},
		seeds:    map[Surface]*Record{},
		recalls:  map[Surface]*Record{},
		identity: map[string]bool{},
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
	return ComputeEligibility(records), nil
}

// checkClosedScenario rejects any final qualification record from the
// non-final set.
func (v *setValidation) checkClosedScenario() error {
	for i := range v.records {
		rec := &v.records[i]
		if rec.Scenario == ScenarioQualification {
			return fmt.Errorf("record %d carries scenario %s, which belongs to the final pass only", rec.Sequence, rec.Scenario)
		}
		if rec.Capability == CapabilityEligibility {
			return fmt.Errorf("record %d carries capability %s, which belongs to the final pass only", rec.Sequence, rec.Capability)
		}
	}
	return nil
}

// ClassifyRecords classifies every record against the closed table and
// applies the per-record field rules: outcome/grade pairing,
// evidence-path nullability, prior-session placement, wire-version and
// agent-name placement, uniqueness keys, and per-class session rules.
func (v *setValidation) ClassifyRecords() error {
	for i := range v.records {
		rec := &v.records[i]
		class, err := ClassifyRecord(rec)
		if err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		v.counts[class]++

		if rec.Outcome == OutcomeNotApplicable && class == RowSemantic {
			return fmt.Errorf("record %d: verdict not_applicable is invalid for a semantic probe", rec.Sequence)
		}
		if err := CheckOutcomeGradePairing(rec); err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		if rec.Grade == GradeCorroborationOnly &&
			(class != RowToken || rec.EvidencePath == nil) {
			return fmt.Errorf("record %d: corroboration_only is valid only on a non-sentinel token_source record", rec.Sequence)
		}
		if (rec.Grade == GradeDeclaredGap || rec.Grade == GradeNotInducible) && class != RowSemantic {
			return fmt.Errorf("record %d: %s is valid only on a semantic probe record", rec.Sequence, rec.Grade)
		}

		if rec.Grade == GradeDeclaredGap && rec.EvidencePath == nil {
			return fmt.Errorf("record %d: declared_gap record must carry a non-null evidence_path", rec.Sequence)
		}

		pathNullAllowed := (class == RowSemantic && rec.Grade == GradeNotObserved) ||
			(class == RowSemantic && rec.Grade == GradeNotInducible) ||
			(class == RowToken && rec.EvidencePath == nil)
		if rec.EvidencePath == nil && !pathNullAllowed {
			return fmt.Errorf("record %d: evidence_path must be set for a %s record", rec.Sequence, rowLabel(class))
		}
		if rec.Grade == GradeNotInducible && rec.EvidencePath != nil {
			return fmt.Errorf("record %d: not_inducible record must carry a null evidence_path", rec.Sequence)
		}
		if rec.Grade == GradeDeclaredGap && !slices.Contains(DeclarableSurfaces, rec.Surface) {
			return fmt.Errorf("record %d: declared_gap record must carry a surface in DeclarableSurfaces, got %s", rec.Sequence, rec.Surface)
		}
		if rec.Grade == GradeDeclaredGap && !slices.Contains(DeclaredGapReasons, rec.Detail) {
			return fmt.Errorf("record %d: declared_gap record detail %q is outside the closed reason set", rec.Sequence, rec.Detail)
		}
		if rec.Grade == GradeNotInducible && rec.Detail != NotInducibleDetail {
			return fmt.Errorf("record %d: not_inducible record detail = %q, want %q", rec.Sequence, rec.Detail, NotInducibleDetail)
		}

		if rec.PriorSessionID != nil && class != RowContinuationRecall {
			return fmt.Errorf("record %d: prior_session_id is only valid on a continuation recall record", rec.Sequence)
		}
		if rec.ProtocolVersion != nil && rec.Surface != SurfaceProtocol {
			return fmt.Errorf("record %d: protocol_version is only valid on protocol records", rec.Sequence)
		}
		if rec.AgentName != nil && rec.Surface != SurfaceProtocol {
			return fmt.Errorf("record %d: agent_name is only valid on protocol records", rec.Sequence)
		}

		if err := v.checkUniqueness(rec, class); err != nil {
			return err
		}
		if class == RowContinuationRecall {
			// Recall records are checked after the seeds and recalls are
			// all known, so a prior id that resolves to another surface's
			// seed reports the resolution failure rather than the
			// per-record detail mismatch.
		} else if err := v.checkSessionRelation(rec, class); err != nil {
			return fmt.Errorf("record %d: %w", rec.Sequence, err)
		}
		v.indexRecord(rec, class)
	}
	return v.checkSemanticSessionRelations()
}

// CheckOutcomeGradePairing enforces the closed pairing between a
// record's outcome and its grade. It is exported so a collector that
// builds one record at a time can check that record against the same
// rule the set validator applies, rather than restating the pairing.
func CheckOutcomeGradePairing(rec *Record) error {
	classification := rec.Grade
	verdict := rec.Outcome
	switch classification {
	case GradeUsable, GradeGap, GradeCorroborationOnly:
		if verdict != OutcomePass {
			return fmt.Errorf("classification %s requires verdict pass, got %s", classification, verdict)
		}
	case GradeNotObserved:
		if verdict == OutcomePass || verdict == OutcomeNotApplicable {
			return fmt.Errorf("classification not_observed is inconsistent with verdict %s", verdict)
		}
	case GradeNotApplicable:
		if verdict != OutcomeNotApplicable {
			return fmt.Errorf("classification not_applicable requires verdict not_applicable, got %s", verdict)
		}
	case GradeDeclaredGap:
		if verdict != OutcomeNotProducible {
			return fmt.Errorf("classification declared_gap requires verdict not_producible, got %s", verdict)
		}
	case GradeNotInducible:
		if verdict != OutcomeNotInducible {
			return fmt.Errorf("classification not_inducible requires verdict not_inducible, got %s", verdict)
		}
	case GradeQualified, GradeNotQualified, GradeUnmeasured:
		return fmt.Errorf("classification %s is valid only for capability eligibility", classification)
	}
	switch verdict {
	case OutcomePass:
		if classification != GradeUsable &&
			classification != GradeGap &&
			classification != GradeCorroborationOnly {
			return fmt.Errorf("verdict pass requires classification usable, gap, or corroboration_only, got %s", classification)
		}
	case OutcomeNotObserved:
		if classification != GradeNotObserved {
			return fmt.Errorf("verdict not_observed requires classification not_observed, got %s", classification)
		}
	case OutcomeNotApplicable:
		if classification != GradeNotApplicable {
			return fmt.Errorf("verdict not_applicable requires classification not_applicable, got %s", classification)
		}
	case OutcomePrerequisiteFailed, OutcomeFixtureInductionFailed, OutcomeAdapterUnanswered, OutcomeRuntimeFailed:
		if classification != GradeNotObserved {
			return fmt.Errorf("verdict %s requires classification not_observed, got %s", verdict, classification)
		}
	case OutcomeNotProducible:
		if classification != GradeDeclaredGap {
			return fmt.Errorf("verdict not_producible requires classification declared_gap, got %s", classification)
		}
	case OutcomeNotInducible:
		if classification != GradeNotInducible {
			return fmt.Errorf("verdict not_inducible requires classification not_inducible, got %s", classification)
		}
	}
	return nil
}

// checkUniqueness enforces the per-class uniqueness key and counts each
// key once.
func (v *setValidation) checkUniqueness(rec *Record, class RowClass) error {
	var key string
	switch class {
	case RowWorkspaceSecurity:
		key = "workspace"
	case RowPolicyPrecondition:
		key = "policy"
	case RowPermission:
		key = "permission"
	case RowMCPDelivery:
		key = "mcp"
	case RowProcessCleanup:
		key = "cleanup"
	case RowEndToEnd:
		key = "e2e"
	case RowSemantic:
		key = fmt.Sprintf("semantic|%s|%s|%s", rec.Surface, rec.Capability, *rec.SemanticCase)
	case RowBaseline:
		key = fmt.Sprintf("baseline|%s|%s", rec.Surface, rec.Capability)
	case RowToken:
		key = fmt.Sprintf("token|%s|%s", rec.Surface, nullableKey(rec.EvidencePath))
	case RowContinuationSeed:
		key = fmt.Sprintf("seed|%s", rec.Surface)
	case RowContinuationRecall:
		key = fmt.Sprintf("recall|%s", rec.Surface)
	case RowRuntimeIdentity:
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

// nullableKey renders a nullable string for a uniqueness key.
func nullableKey(v *string) string {
	if v == nil {
		return "<null>"
	}
	return *v
}

// checkSessionRelation enforces the per-class session_id rules.
func (v *setValidation) checkSessionRelation(rec *Record, class RowClass) error {
	switch class {
	case RowWorkspaceSecurity, RowProcessCleanup, RowBaseline:
		if rec.SessionID != nil {
			return fmt.Errorf("%s record must use a null session_id", rowLabel(class))
		}
	case RowPolicyPrecondition, RowPermission, RowMCPDelivery, RowContinuationSeed, RowEndToEnd:
		if rec.SessionID == nil {
			return fmt.Errorf("%s record must reference a non-null session_id", rowLabel(class))
		}
	case RowRuntimeIdentity:
		if rec.SessionID == nil {
			return fmt.Errorf("%s record must reference a non-null session_id", rowLabel(class))
		}
		if rec.ProtocolVersion == nil {
			return fmt.Errorf("runtime identity record must carry the session's negotiated protocol_version")
		}
		if rec.Grade == GradeUsable &&
			(rec.AgentName == nil || rec.AgentVersion == nil) {
			return fmt.Errorf("usable runtime identity record must carry agent_name and agent_version")
		}
	case RowToken:
		if rec.EvidencePath == nil {
			if rec.SessionID != nil {
				return fmt.Errorf("token sentinel record must use a null session_id")
			}
			if rec.Source != SourceNone {
				return fmt.Errorf("token sentinel record must carry source none, got %s", rec.Source)
			}
			if rec.Grade != GradeGap && rec.Grade != GradeNotObserved {
				return fmt.Errorf("token sentinel classification = %s, want gap or not_observed", rec.Grade)
			}
		} else {
			if rec.SessionID == nil {
				return fmt.Errorf("token inventory record must reference the session that emitted the path")
			}
			if rec.Source == SourceNone {
				return fmt.Errorf("token record with a path must not carry source none")
			}
			if rec.Grade != GradeUsable && rec.Grade != GradeCorroborationOnly {
				return fmt.Errorf("non-sentinel token classification = %s, want usable or corroboration_only", rec.Grade)
			}
		}
	case RowContinuationRecall:
		return nil
	case RowSemantic:
		if rec.Outcome == OutcomePass && rec.SessionID == nil {
			return fmt.Errorf("passing semantic probe must carry its own session_id")
		}
		if rec.Grade == GradeDeclaredGap && rec.SessionID == nil {
			return fmt.Errorf("declared_gap record must carry its own session_id")
		}
		if rec.Grade == GradeNotInducible && rec.SessionID != nil {
			return fmt.Errorf("not_inducible record must carry a null session_id")
		}
	}
	return nil
}

// checkRecallRecords validates every surface's recall record after the
// continuation relations are known.
func (v *setValidation) checkRecallRecords() error {
	for _, surface := range measuredSurfaces {
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

// checkRecallRecord enforces the closed detail set and the session
// relation each recall outcome requires.
func (v *setValidation) checkRecallRecord(rec *Record) error {
	if rec.PriorSessionID == nil {
		return fmt.Errorf("continuation recall record must carry prior_session_id")
	}
	switch rec.Detail {
	case recallConfirmedSameSession:
		if rec.SessionID == nil || *rec.SessionID != *rec.PriorSessionID {
			return fmt.Errorf("confirmed_same_session requires equal non-null actual and prior session ids")
		}
		if rec.Grade != GradeUsable {
			return fmt.Errorf("confirmed_same_session requires classification usable, got %s", rec.Grade)
		}
	case recallFreshFallback:
		if rec.SessionID == nil || *rec.SessionID == *rec.PriorSessionID {
			return fmt.Errorf("fresh_session_fallback requires a non-null actual session id distinct from prior_session_id")
		}
		if rec.Grade != GradeGap {
			return fmt.Errorf("fresh_session_fallback requires classification gap, got %s", rec.Grade)
		}
	case recallUnobservedActual:
		if rec.SessionID != nil {
			return fmt.Errorf("unobserved_actual_session requires a null session_id")
		}
		if rec.Grade != GradeNotObserved {
			return fmt.Errorf("unobserved_actual_session requires classification not_observed, got %s", rec.Grade)
		}
	default:
		return fmt.Errorf("recall detail %q is outside the closed set", rec.Detail)
	}
	return nil
}

// indexRecord files a classified record into the lookups the relation
// and derivation checks consume.
func (v *setValidation) indexRecord(rec *Record, class RowClass) {
	switch class {
	case RowPolicyPrecondition:
		v.policy = rec
	case RowPermission:
		v.permission = rec
	case RowMCPDelivery:
		v.mcp = rec
	case RowEndToEnd:
		v.e2e = rec
	case RowSemantic:
		if v.semantic[rec.Surface] == nil {
			v.semantic[rec.Surface] = map[Capability]map[Case]*Record{}
		}
		if v.semantic[rec.Surface][rec.Capability] == nil {
			v.semantic[rec.Surface][rec.Capability] = map[Case]*Record{}
		}
		v.semantic[rec.Surface][rec.Capability][*rec.SemanticCase] = rec
	case RowToken:
		v.tokens[rec.Surface] = append(v.tokens[rec.Surface], rec)
	case RowContinuationSeed:
		v.seeds[rec.Surface] = rec
	case RowContinuationRecall:
		v.recalls[rec.Surface] = rec
	case RowRuntimeIdentity:
		if rec.SessionID != nil {
			v.identity[*rec.SessionID] = true
		}
	}
}

// checkSemanticSessionRelations enforces the reuse rules between semantic
// records and the records they share a physical run with: refusal retry
// records reuse the matching disposition-refusal session, and the
// protocol human-input record reuses the permission attempt's session.
func (v *setValidation) checkSemanticSessionRelations() error {
	for _, surface := range measuredSurfaces {
		dispositionRefusal := v.semanticRecord(surface, CapabilityTurnDisposition, CaseRuntimeRefusal)
		retryRefusal := v.semanticRecord(surface, CapabilityRetryClassification, CaseNonRetryableRefusal)
		if dispositionRefusal != nil && retryRefusal != nil &&
			dispositionRefusal.SessionID != nil && retryRefusal.SessionID != nil {
			// The rule states that two records of one physical refusal run
			// carry that run's session. A side carrying no session made no
			// such run, so it has none to reuse and none to check against,
			// and the comparison is skipped rather than failed. Keying the
			// skip on the absent session rather than on a grade keeps it
			// exactly as wide as the rule it exempts: whenever both sides
			// carry a session, they must still agree, whatever their grades.
			if compareNullableString(dispositionRefusal.SessionID, retryRefusal.SessionID) != 0 {
				return fmt.Errorf("%s refusal retry record does not reuse the matching disposition-refusal session id", surface)
			}
		}
	}

	if v.permission != nil {
		humanInput := v.semanticRecord(SurfaceProtocol, CapabilityRetryClassification, CaseHumanInput)
		if humanInput != nil && humanInput.SessionID != nil && *humanInput.SessionID != *v.permission.SessionID {
			return fmt.Errorf("protocol human_input record does not reuse the permission attempt session id")
		}
	}
	return nil
}

// semanticRecord returns one semantic record by tuple, or nil.
func (v *setValidation) semanticRecord(surface Surface, capability Capability, caseID Case) *Record {
	return v.semantic[surface][capability][caseID]
}

// capabilityOwning returns the capability CapabilityCases lists caseID
// under, or the zero value if none does.
func capabilityOwning(caseID Case) Capability {
	for _, capability := range Capabilities {
		if slices.Contains(CapabilityCases[capability], caseID) {
			return capability
		}
	}
	return ""
}

// missingSurfaces returns every member of want that is absent from
// have, in want's order.
func missingSurfaces(have, want []Surface) []Surface {
	var missing []Surface
	for _, surface := range want {
		if !slices.Contains(have, surface) {
			missing = append(missing, surface)
		}
	}
	return missing
}

// checkExcludedCases enforces the closed excluded-case rules for every
// (capability, case) pair: a case is one kind of excluded or the
// other, an exclusion kind carries the surface set it requires, a
// declared gap's details agree with one another and with the
// declaration that authorizes it, its peer under DeclaredGapPeers
// carries the identical exclusion, and a not-inducible detail is
// always NotInducibleDetail. It then requires every declaration to
// match a record, mirroring checkIdentityCoverage's bidirectional
// shape.
func (v *setValidation) checkExcludedCases(declarations DeclaredGapSet) error {
	for _, capability := range Capabilities {
		cases, ok := CapabilityCases[capability]
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
		for _, surface := range DeclarableSurfaces {
			if rec := v.semanticRecord(surface, entry.Capability, entry.Case); rec != nil && rec.Grade == GradeDeclaredGap {
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

// checkExcludedCase enforces the closed excluded-case rules for one
// (capability, case) pair.
func (v *setValidation) checkExcludedCase(declarations DeclaredGapSet, capability Capability, caseID Case) error {
	var declared, catalog []Surface
	var declaredDetail string
	for _, surface := range measuredSurfaces {
		rec := v.semanticRecord(surface, capability, caseID)
		if rec == nil {
			continue
		}
		switch rec.Grade {
		case GradeDeclaredGap:
			declared = append(declared, surface)
			declaredDetail = rec.Detail
		case GradeNotInducible:
			catalog = append(catalog, surface)
		}
	}

	if len(declared) > 0 && len(catalog) > 0 {
		return fmt.Errorf("capability %s case %s carries both a declared gap and a not-inducible grade", capability, caseID)
	}
	if len(declared) > 0 {
		if missing := missingSurfaces(declared, DeclarableSurfaces); len(missing) > 0 {
			return fmt.Errorf("capability %s case %s declared gap is missing on surfaces %v", capability, caseID, missing)
		}
	}
	if len(catalog) > 0 {
		if missing := missingSurfaces(catalog, measuredSurfaces); len(missing) > 0 {
			return fmt.Errorf("capability %s case %s not-inducible grade is missing on surfaces %v", capability, caseID, missing)
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
		if peer, hasPeer := DeclaredGapPeers[caseID]; hasPeer {
			peerCapability := capabilityOwning(peer)
			var peerDeclared []Surface
			var peerDetail string
			for _, surface := range measuredSurfaces {
				rec := v.semanticRecord(surface, peerCapability, peer)
				if rec == nil || rec.Grade != GradeDeclaredGap {
					continue
				}
				peerDeclared = append(peerDeclared, surface)
				peerDetail = rec.Detail
			}
			if missing := missingSurfaces(peerDeclared, DeclarableSurfaces); len(missing) > 0 {
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
			if rec.Detail != NotInducibleDetail {
				return fmt.Errorf("capability %s case %s not-inducible record carries detail %q, want %q", capability, caseID, rec.Detail, NotInducibleDetail)
			}
		}
	}
	return nil
}

// checkContinuationRelations enforces that every non-null
// prior_session_id resolves to exactly one same-surface seed and that
// each surface carries one seed and one recall.
func (v *setValidation) checkContinuationRelations() error {
	for _, surface := range measuredSurfaces {
		if v.seeds[surface] == nil {
			return fmt.Errorf("surface %s has no continuation seed record", surface)
		}
		if v.recalls[surface] == nil {
			return fmt.Errorf("surface %s has no continuation recall record", surface)
		}
		recall := v.recalls[surface]
		if recall.PriorSessionID == nil {
			return fmt.Errorf("surface %s recall record carries no prior_session_id", surface)
		}
		seed := v.seeds[surface]
		if seed.SessionID == nil || *seed.SessionID != *recall.PriorSessionID {
			return fmt.Errorf("surface %s recall prior_session_id %s does not resolve to that surface's seed session", surface, *recall.PriorSessionID)
		}
	}
	return nil
}

// checkTokenInventories enforces the per-surface inventory rules:
// at least one record per surface, at most one sentinel, and mutual
// exclusion between the sentinel and non-sentinel records.
func (v *setValidation) checkTokenInventories() error {
	for _, surface := range measuredSurfaces {
		records := v.tokens[surface]
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
	}
	return nil
}

// checkDerivedBaselines requires every written baseline grade to equal
// the grade derived from the records that own it: the five disposition
// and four retry case records per surface, the per-surface token
// inventory, and the surface's recall outcome.
func (v *setValidation) checkDerivedBaselines() error {
	grades := baselineGrades(v.records)

	for _, surface := range measuredSurfaces {
		for _, capability := range comparisonCapabilities {
			written, ok := grades[surface][capability]
			if !ok {
				return fmt.Errorf("surface %s has no %s baseline record", surface, capability)
			}
			var derived Grade
			switch capability {
			case CapabilityTurnDisposition, CapabilityRetryClassification:
				var classes []Grade
				allExcluded := true
				for _, caseID := range CapabilityCases[capability] {
					rec := v.semanticRecord(surface, capability, caseID)
					if rec == nil {
						return fmt.Errorf("surface %s is missing its %s %s semantic record", surface, capability, caseID)
					}
					classes = append(classes, rec.Grade)
					if rec.Grade != GradeDeclaredGap && rec.Grade != GradeNotInducible {
						allExcluded = false
					}
				}
				if allExcluded {
					return fmt.Errorf("surface %s capability %s has every case excluded, leaving no case to derive a baseline from", surface, capability)
				}
				derived = DeriveBaselineGrade(classes)
			case CapabilityTokenCeiling:
				derived = tokenBaselineGrade(v.tokens[surface])
			case CapabilitySessionContinuation:
				derived = v.recalls[surface].Grade
			}
			if written != derived {
				return fmt.Errorf("surface %s %s baseline = %s, want derived %s", surface, capability, written, derived)
			}
		}
	}
	return nil
}

// tokenBaselineGrade derives a surface's token baseline from its
// inventory: a zero-source sentinel grades gap, a failed-inventory
// sentinel grades not_observed, and a completed inventory grades usable
// only when it contains a usable source.
func tokenBaselineGrade(records []*Record) Grade {
	usable := false
	for _, rec := range records {
		if rec.EvidencePath == nil {
			if rec.Grade == GradeNotObserved {
				return GradeNotObserved
			}
			return GradeGap
		}
		if rec.Grade == GradeUsable {
			usable = true
		}
	}
	if usable {
		return GradeUsable
	}
	return GradeGap
}

// checkIdentityCoverage requires exactly one runtime-identity record for
// every distinct non-null actual protocol session id referenced by any
// non-final record that is not itself a runtime identity, including a
// fresh fallback id, and no others. An identity record is skipped while
// the referenced ids are collected: it carries the protocol surface and
// its own session id, so counting it would let it satisfy its own
// coverage requirement and leave an identity for a session no other
// evidence mentions undetected.
func (v *setValidation) checkIdentityCoverage() error {
	referenced := map[string]bool{}
	for i := range v.records {
		rec := &v.records[i]
		if rec.Scenario == ScenarioRuntimeIdentity {
			continue
		}
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
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

// Per-scenario record counts the closed evidence set requires. Each of
// the six singleton scenarios (workspace security, policy precondition,
// permission, tool server delivery, process cleanup, end to end)
// contributes singletonRowCount records; checkCardinality sums these
// same constants into its fixed term, so a changed count here cannot
// drift from the total it is checked against.
const (
	singletonRowCount        = 1
	dispositionSemanticCount = 20
	retrySemanticCount       = 16
	surfaceBaselineCount     = 16
	continuationSeedCount    = 4
	continuationRecallCount  = 4
)

// checkCardinality enforces the fixed row counts and the closed
// cardinality formula fixed+T+N, where fixed is the sum of the per-
// scenario record counts above, T is the token-inventory record count,
// and N is the number of distinct non-null actual protocol session ids
// referenced by non-final records.
func (v *setValidation) checkCardinality() error {
	fixed := map[RowClass]int{
		RowWorkspaceSecurity:  singletonRowCount,
		RowPolicyPrecondition: singletonRowCount,
		RowPermission:         singletonRowCount,
		RowMCPDelivery:        singletonRowCount,
		RowProcessCleanup:     singletonRowCount,
		RowEndToEnd:           singletonRowCount,
	}
	fixedTotal := 0
	for class, want := range fixed {
		fixedTotal += want
		if err := checkCount(rowLabel(class), v.counts[class], want); err != nil {
			return err
		}
	}
	if err := checkCount("disposition semantic", v.countSemanticByCapability(CapabilityTurnDisposition), dispositionSemanticCount); err != nil {
		return err
	}
	if err := checkCount("retry semantic", v.countSemanticByCapability(CapabilityRetryClassification), retrySemanticCount); err != nil {
		return err
	}
	if err := checkCount("surface baseline", v.counts[RowBaseline], surfaceBaselineCount); err != nil {
		return err
	}
	if err := checkCount("continuation seed", v.counts[RowContinuationSeed], continuationSeedCount); err != nil {
		return err
	}
	if err := checkCount("continuation recall", v.counts[RowContinuationRecall], continuationRecallCount); err != nil {
		return err
	}
	fixedTotal += dispositionSemanticCount + retrySemanticCount + surfaceBaselineCount + continuationSeedCount + continuationRecallCount

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
func (v *setValidation) countSemanticByCapability(capability Capability) int {
	count := 0
	for i := range v.records {
		if v.records[i].Scenario == ScenarioSemanticProbe && v.records[i].Capability == capability {
			count++
		}
	}
	return count
}

// checkCount reports a cardinality mismatch, naming a duplicate or
// a missing record so controls can pin the cause.
func checkCount(name string, got, want int) error {
	if got == want {
		return nil
	}
	if got > want {
		return fmt.Errorf("%s row holds %d records, want exactly %d (duplicate record present)", name, got, want)
	}
	return fmt.Errorf("%s row holds %d records, want exactly %d (required record missing)", name, got, want)
}

// AggregateGradeFor maps a verdict to the grade the final aggregate
// record carries. It is total over Verdicts; a value outside the set
// is a programming error and returns the zero value rather than
// panicking.
func AggregateGradeFor(verdict Verdict) Grade {
	switch verdict {
	case VerdictQualified:
		return GradeQualified
	case VerdictNotQualified:
		return GradeNotQualified
	case VerdictUnmeasured:
		return GradeUnmeasured
	}
	return ""
}

// VerdictRationale returns the one operator-facing rationale line for
// a verdict. It is total over Verdicts, names no runtime, and returns
// the zero value for a value outside the set.
func VerdictRationale(verdict Verdict) string {
	switch verdict {
	case VerdictQualified:
		return "Every load-bearing row was measured and the protocol surface is not below the richest measured native reference."
	case VerdictNotQualified:
		return "The protocol surface is below the richest measured native reference on at least one load-bearing row. This runtime stays on its existing integration."
	case VerdictUnmeasured:
		return "At least one load-bearing row was not measured. This runtime waits; re-run the profile after the causes listed below are removed."
	}
	return ""
}

// ValidateObservations strictly validates the closed
// non-final observation set written by the collector and returns the
// computed eligibility verdict. It rejects any final qualification
// record. It delegates to ValidateObservationsWithDeclarations with an
// empty declaration set, which rejects every declared_gap record, so
// an existing caller stays fail-closed without being rewritten.
func ValidateObservations(path string) (Verdict, error) {
	return ValidateObservationsWithDeclarations(path, DeclaredGapSet{})
}

// ValidateObservationsWithDeclarations strictly validates the closed
// non-final observation set written by the collector against the
// declaration set the run was collected under, and returns the
// computed eligibility verdict. It rejects any final qualification
// record.
func ValidateObservationsWithDeclarations(path string, declarations DeclaredGapSet) (Verdict, error) {
	records, err := readEvidenceFile(path)
	if err != nil {
		return "", err
	}
	if err := validateSequence(records); err != nil {
		return "", err
	}
	return validateNonFinalSet(records, declarations)
}

// The final-pass aggregate causes, wrapped by ValidateEvidence
// so controls can pin the exact rejection.
var (
	errFinalRecordMissing    = errors.New("final qualification record missing from the evidence file")
	errFinalRecordDuplicated = errors.New("final qualification record is duplicated in the evidence file")
	errRecordAfterAggregate  = errors.New("a record follows the final qualification record; the aggregate must be last")
)

// ValidateEvidence strictly validates the complete two-pass evidence
// file: the closed non-final set, exactly one terminal aggregate
// record in the last position, and exact equality between the
// aggregate's grade and an independent recomputation of eligibility.
// It delegates to ValidateEvidenceWithDeclarations with an empty
// declaration set, which rejects every declared_gap record, so an
// existing caller stays fail-closed without being rewritten.
func ValidateEvidence(path string) (Verdict, error) {
	return ValidateEvidenceWithDeclarations(path, DeclaredGapSet{})
}

// ValidateEvidenceWithDeclarations strictly validates the complete
// two-pass evidence file against the declaration set the run was
// collected under: the closed non-final set, exactly one terminal
// aggregate record in the last position, and exact equality between
// the aggregate's grade and an independent recomputation of
// eligibility.
func ValidateEvidenceWithDeclarations(path string, declarations DeclaredGapSet) (Verdict, error) {
	records, err := readEvidenceFile(path)
	if err != nil {
		return "", err
	}
	if err := validateSequence(records); err != nil {
		return "", err
	}

	finalIndex := -1
	finalCount := 0
	for i := range records {
		if records[i].Scenario == ScenarioQualification || records[i].Capability == CapabilityEligibility {
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

	// The final record is located by either half of its tuple so that a
	// stray one anywhere in the set is counted and rejected above. That
	// leaves the located record's own tuple unchecked, so it is closed
	// here: nothing but the aggregate eligibility row may occupy the
	// position the verdict is read from.
	if aggregate.Scenario != ScenarioQualification || aggregate.Surface != SurfaceAggregate ||
		aggregate.Capability != CapabilityEligibility || aggregate.Source != SourceComparison {
		return "", fmt.Errorf(
			"final qualification record tuple = %s/%s/%s/%s, want %s/%s/%s/%s",
			aggregate.Scenario, aggregate.Surface, aggregate.Capability, aggregate.Source,
			ScenarioQualification, SurfaceAggregate, CapabilityEligibility, SourceComparison)
	}
	if aggregate.Outcome != OutcomePass {
		return "", fmt.Errorf("final qualification record verdict = %s, want pass", aggregate.Outcome)
	}
	if aggregate.InputID != InputAggregate {
		return "", fmt.Errorf("final qualification record input_id = %s, want %s", aggregate.InputID, InputAggregate)
	}
	if aggregate.EvidencePath == nil || *aggregate.EvidencePath != EvidencePathQualificationVerdict {
		return "", fmt.Errorf("final qualification record evidence_path must be %s", EvidencePathQualificationVerdict)
	}
	if aggregate.SessionID != nil || aggregate.PriorSessionID != nil || aggregate.SemanticCase != nil ||
		aggregate.AgentName != nil || aggregate.AgentVersion != nil || aggregate.ProtocolVersion != nil {
		return "", fmt.Errorf("final qualification record must carry null session, prior, semantic case, agent, and protocol fields")
	}

	verdict, err := validateNonFinalSet(nonFinal, declarations)
	if err != nil {
		return "", err
	}
	want := AggregateGradeFor(verdict)
	if aggregate.Grade != want {
		return "", fmt.Errorf("final qualification record classification = %s, want %s from independent recomputation", aggregate.Grade, want)
	}
	return verdict, nil
}
