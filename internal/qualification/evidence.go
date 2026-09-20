package qualification

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// Verdict is the final qualification outcome. An unmeasured row is reported
// apart from one measured and below; there is no manual override.
type Verdict string

const (
	VerdictQualified    Verdict = "qualified"
	VerdictNotQualified Verdict = "not_qualified"
	VerdictUnmeasured   Verdict = "unmeasured"
)

// Verdicts is the closed verdict set.
var Verdicts = []Verdict{VerdictQualified, VerdictNotQualified, VerdictUnmeasured}

// Scenario names the kind of observation one evidence line carries.
type Scenario string

const (
	ScenarioSemanticProbe      Scenario = "semantic_probe"
	ScenarioSurfaceBaseline    Scenario = "surface_baseline"
	ScenarioTokenSource        Scenario = "token_source"
	ScenarioPolicyPrecondition Scenario = "policy_precondition"
	ScenarioPermissionRequest  Scenario = "permission_request"
	ScenarioToolServer         Scenario = "tool_server"
	ScenarioContinuation       Scenario = "continuation"
	ScenarioRuntimeIdentity    Scenario = "runtime_identity"
	ScenarioWorkspaceSecurity  Scenario = "workspace_security"
	ScenarioProcessCleanup     Scenario = "process_cleanup"
	ScenarioEndToEnd           Scenario = "end_to_end"
	ScenarioQualification      Scenario = "qualification"
)

// Surface names the measured surface one evidence line describes.
// SurfaceAggregate denotes a cross-surface qualification observation.
type Surface string

const (
	SurfaceProtocol         Surface = "protocol"
	SurfaceNativeJSON       Surface = "native_json"
	SurfaceNativeStreamJSON Surface = "native_stream_json"
	SurfaceAggregate        Surface = "aggregate"
)

// Capability names the load-bearing capability one evidence line
// grades.
type Capability string

const (
	CapabilityTurnDisposition     Capability = "turn_disposition"
	CapabilityRetryClassification Capability = "retry_classification"
	CapabilityTokenCeiling        Capability = "token_ceiling"
	CapabilityToolServerDelivery  Capability = "tool_server_delivery"
	CapabilitySessionContinuation Capability = "session_continuation"
	CapabilityPermissionHandling  Capability = "permission_handling"
	CapabilityRuntimeIdentity     Capability = "runtime_identity"
	CapabilityWorkspaceSecurity   Capability = "workspace_security"
	CapabilityProcessCleanup      Capability = "process_cleanup"
	CapabilityEligibility         Capability = "eligibility"
)

// Source names the evidence channel a record was derived from.
type Source string

const (
	SourceProtocolStable     Source = "protocol_stable"
	SourceProtocolExtension  Source = "protocol_extension"
	SourceNativeStructured   Source = "native_structured"
	SourceNativeText         Source = "native_text"
	SourceSortieShared       Source = "sortie_shared"
	SourceProcessObservation Source = "process_observation"
	SourceNone               Source = "none"
	SourceComparison         Source = "comparison"
)

// ExtensionSource is what one collection established about a token-bearing
// extension on the protocol's extension point. The three values stay apart
// because an output nothing read is not a measured absence.
type ExtensionSource string

const (
	// ExtensionSourcePresent states that a read result carried a token-bearing
	// extension. A block reporting only zeros is present: a turn that spent
	// nothing answers with real zeros.
	ExtensionSourcePresent ExtensionSource = "present"
	// ExtensionSourceAbsent states that results were read and none carried a
	// token-bearing extension.
	ExtensionSourceAbsent ExtensionSource = "absent"
	// ExtensionSourceNotObserved states that no result was read, so what the
	// transport carried stays unknown.
	ExtensionSourceNotObserved ExtensionSource = "not_observed"
)

// ExtensionSources is the closed extension-source set.
var ExtensionSources = []ExtensionSource{
	ExtensionSourcePresent, ExtensionSourceAbsent, ExtensionSourceNotObserved,
}

// SuppliedOutsideProtocol reports whether a record's reading reached Sortie
// through its own code rather than over the protocol wire. Such a record
// answers for the effective adapter, not the transport, so it takes no part in
// any surface's baseline.
func SuppliedOutsideProtocol(source Source) bool {
	return source == SourceSortieShared
}

// Grade is the comparison grade or outcome class a record carries.
// GradeQualified and GradeNotQualified are valid only on capability
// eligibility; GradeCorroborationOnly never receives numeric grade 1.
type Grade string

const (
	GradeUsable            Grade = "usable"
	GradeGap               Grade = "gap"
	GradeCorroborationOnly Grade = "corroboration_only"
	GradeNotObserved       Grade = "not_observed"
	GradeNotApplicable     Grade = "not_applicable"
	GradeQualified         Grade = "qualified"
	GradeNotQualified      Grade = "not_qualified"
	// GradeDeclaredGap is valid only on a semantic case row: the runtime cannot
	// produce the case and the operator declared the gap.
	GradeDeclaredGap Grade = "declared_gap"
	// GradeNotInducible is valid only on a semantic case row: the catalog has
	// no deterministic inducer for the case, on any runtime.
	GradeNotInducible Grade = "not_inducible"
	// GradeUnmeasured is valid only on the final aggregate row.
	GradeUnmeasured Grade = "unmeasured"
)

// RowGrades is the closed set of grades a non-final row may carry, excluding
// the three eligibility-only grades valid only on the final aggregate row.
var RowGrades = []Grade{
	GradeUsable, GradeGap, GradeDeclaredGap, GradeNotInducible,
	GradeCorroborationOnly, GradeNotObserved, GradeNotApplicable,
}

// Outcome is the bounded probe outcome a record carries.
type Outcome string

const (
	OutcomePass                   Outcome = "pass"
	OutcomeNotObserved            Outcome = "not_observed"
	OutcomeNotApplicable          Outcome = "not_applicable"
	OutcomePrerequisiteFailed     Outcome = "prerequisite_failed"
	OutcomeFixtureInductionFailed Outcome = "fixture_induction_failed"
	OutcomeAdapterUnanswered      Outcome = "adapter_unanswered"
	OutcomeRuntimeFailed          Outcome = "runtime_failed"
	// OutcomeNotProducible pairs with GradeDeclaredGap.
	OutcomeNotProducible Outcome = "not_producible"
	// OutcomeNotInducible pairs with GradeNotInducible.
	OutcomeNotInducible Outcome = "not_inducible"
)

// Case names one required disposition or retry class. A record carries it only
// for semantic probes; every other record stores null.
type Case string

const (
	CaseSuccess             Case = "success"
	CaseRuntimeFailure      Case = "runtime_failure"
	CaseRuntimeRefusal      Case = "runtime_refusal"
	CaseCancellation        Case = "cancellation"
	CaseLimitReached        Case = "limit_reached"
	CaseRetryableTransport  Case = "retryable_runtime_or_transport_failure"
	CaseNonRetryableRefusal Case = "non_retryable_refusal"
	CaseHumanInput          Case = "human_input"
	CaseUnknownOutcome      Case = "unknown_outcome"
)

// InputID names the fixed input contract a record was collected under.
type InputID string

const (
	InputDispositionSuccess        InputID = "disposition_success_v1"
	InputDispositionRuntimeFailure InputID = "disposition_runtime_failure_v1"
	InputDispositionRuntimeRefusal InputID = "disposition_runtime_refusal_v1"
	InputDispositionCancellation   InputID = "disposition_cancellation_v1"
	InputDispositionLimitReached   InputID = "disposition_limit_reached_v1"
	InputRetryableTransport        InputID = "retryable_transport_failure_v1"
	InputRetryNonRetryableRefusal  InputID = "retry_non_retryable_refusal_v1"
	InputRetryHumanInput           InputID = "retry_human_input_v1"
	InputRetryUnknownOutcome       InputID = "retry_unknown_outcome_v1"
	InputBaseline                  InputID = "baseline_v1"
	InputTokenInventory            InputID = "token_inventory_v1"
	InputPolicyControl             InputID = "policy_control_v1"
	InputPermissionProbe           InputID = "permission_probe_v1"
	InputMCPProbe                  InputID = "mcp_probe_v1"
	InputContinuationSeed          InputID = "continuation_seed_v1"
	InputContinuationRecall        InputID = "continuation_recall_v1"
	InputIdentity                  InputID = "identity_v1"
	InputSecurity                  InputID = "security_v1"
	InputCleanup                   InputID = "cleanup_v1"
	InputE2E                       InputID = "e2e_v1"
	InputAggregate                 InputID = "aggregate_v1"
)

// The closed value sets. Everything outside them is invalid.
var (
	Scenarios = []Scenario{
		ScenarioSemanticProbe, ScenarioSurfaceBaseline,
		ScenarioTokenSource, ScenarioPolicyPrecondition,
		ScenarioPermissionRequest, ScenarioToolServer,
		ScenarioContinuation, ScenarioRuntimeIdentity,
		ScenarioWorkspaceSecurity, ScenarioProcessCleanup,
		ScenarioEndToEnd, ScenarioQualification,
	}
	Surfaces = []Surface{
		SurfaceProtocol,
		SurfaceNativeJSON, SurfaceNativeStreamJSON,
		SurfaceAggregate,
	}
	Capabilities = []Capability{
		CapabilityTurnDisposition, CapabilityRetryClassification,
		CapabilityTokenCeiling, CapabilityToolServerDelivery,
		CapabilitySessionContinuation, CapabilityPermissionHandling,
		CapabilityRuntimeIdentity, CapabilityWorkspaceSecurity,
		CapabilityProcessCleanup, CapabilityEligibility,
	}
	Sources = []Source{
		SourceProtocolStable, SourceProtocolExtension,
		SourceNativeStructured, SourceNativeText,
		SourceSortieShared, SourceProcessObservation,
		SourceNone, SourceComparison,
	}
	Grades = []Grade{
		GradeUsable, GradeGap,
		GradeCorroborationOnly, GradeNotObserved,
		GradeNotApplicable, GradeQualified,
		GradeNotQualified, GradeDeclaredGap,
		GradeNotInducible, GradeUnmeasured,
	}
	Outcomes = []Outcome{
		OutcomePass, OutcomeNotObserved, OutcomeNotApplicable,
		OutcomePrerequisiteFailed, OutcomeFixtureInductionFailed,
		OutcomeAdapterUnanswered, OutcomeRuntimeFailed,
		OutcomeNotProducible, OutcomeNotInducible,
	}
	Cases = []Case{
		CaseSuccess, CaseRuntimeFailure, CaseRuntimeRefusal,
		CaseCancellation, CaseLimitReached,
		CaseRetryableTransport, CaseNonRetryableRefusal,
		CaseHumanInput, CaseUnknownOutcome,
	}
	InputIDs = []InputID{
		InputDispositionSuccess, InputDispositionRuntimeFailure,
		InputDispositionRuntimeRefusal, InputDispositionCancellation,
		InputDispositionLimitReached, InputRetryableTransport,
		InputRetryNonRetryableRefusal, InputRetryHumanInput,
		InputRetryUnknownOutcome, InputBaseline,
		InputTokenInventory, InputPolicyControl,
		InputPermissionProbe, InputMCPProbe,
		InputContinuationSeed, InputContinuationRecall,
		InputIdentity, InputSecurity, InputCleanup,
		InputE2E, InputAggregate,
	}
)

// DeclarableSurfaces are the surfaces whose evidence can confirm or
// contradict an operator declaration.
var DeclarableSurfaces = []Surface{
	SurfaceProtocol, SurfaceNativeJSON, SurfaceNativeStreamJSON,
}

// SurfaceNotOffered states that the runtime exposes no entry point for
// this surface.
const SurfaceNotOffered = "surface_not_offered"

// AbsentSurfaceReasons is the closed set of reasons an absent-surface
// declaration may carry.
var AbsentSurfaceReasons = []string{SurfaceNotOffered}

// DeclarableAbsentSurfaces are the surfaces an operator may declare
// absent. SurfaceProtocol is excluded because it is the surface under
// test, and SurfaceAggregate is excluded because it is not measured.
var DeclarableAbsentSurfaces = []Surface{SurfaceNativeJSON, SurfaceNativeStreamJSON}

// CapabilityCases maps each semantic capability to its closed case set,
// in the order the evidence contract requires records to be written.
var CapabilityCases = map[Capability][]Case{
	CapabilityTurnDisposition: {
		CaseSuccess, CaseRuntimeFailure, CaseRuntimeRefusal,
		CaseCancellation, CaseLimitReached,
	},
	CapabilityRetryClassification: {
		CaseRetryableTransport, CaseNonRetryableRefusal,
		CaseHumanInput, CaseUnknownOutcome,
	},
}

// CaseInputs maps every semantic case to the one input_id that may
// record it.
var CaseInputs = map[Case]InputID{
	CaseSuccess:             InputDispositionSuccess,
	CaseRuntimeFailure:      InputDispositionRuntimeFailure,
	CaseRuntimeRefusal:      InputDispositionRuntimeRefusal,
	CaseCancellation:        InputDispositionCancellation,
	CaseLimitReached:        InputDispositionLimitReached,
	CaseRetryableTransport:  InputRetryableTransport,
	CaseNonRetryableRefusal: InputRetryNonRetryableRefusal,
	CaseHumanInput:          InputRetryHumanInput,
	CaseUnknownOutcome:      InputRetryUnknownOutcome,
}

// CatalogNotInducibleCases is the closed set of cases the input catalog has no
// deterministic inducer for, on any runtime. CaseUnknownOutcome's inducer would
// ask the model for a stop reason outside the protocol's closed enum, which no
// conforming runtime can produce.
var CatalogNotInducibleCases = []Case{CaseUnknownOutcome}

// The excluded-case detail constants occupy the record's existing detail
// member; no new record field is introduced.
const (
	// DeclaredGapNeverProduced states that the runtime has no code path that
	// emits this outcome.
	DeclaredGapNeverProduced = "outcome_never_produced"
	// DeclaredGapFolded states that the runtime detects the condition but
	// reports it under another case's outcome.
	DeclaredGapFolded = "outcome_folded_into_another"
	// NotInducibleDetail is the one detail a not_inducible row carries.
	NotInducibleDetail = "no_deterministic_inducer"
)

func isNotInducibleDetail(detail string) bool {
	return detail == NotInducibleDetail || slices.Contains(NotInducibleReasons, detail)
}

// DeclaredGapReasons is the closed set of reasons an operator
// declaration may carry.
var DeclaredGapReasons = []string{DeclaredGapNeverProduced, DeclaredGapFolded}

// NotInducibleChannelTooSmall states that the surface's prompt channel cannot
// carry the input a case's induction requires.
const NotInducibleChannelTooSmall = "prompt_channel_too_small"

// NotInducibleOutputSilentOnFailure states that the surface emits no bytes
// when the runtime fails, so no terminal is located and no recognizer can
// grade that case.
const NotInducibleOutputSilentOnFailure = "output_channel_silent_on_failure"

// NotInducibleTerminalAtExitOnly states that the surface writes its terminal
// only at process exit, so a cancellation signal leaves no cancellation-shaped
// terminal to grade.
const NotInducibleTerminalAtExitOnly = "terminal_written_at_exit_only"

// NotInducibleTerminalVocabularyClosed states that the surface's recognized
// terminal carries no member this case could set, so no recognizer can grade
// it.
const NotInducibleTerminalVocabularyClosed = "terminal_vocabulary_closed"

// NotInducibleReasons is the closed set of reasons a profile's own
// not_inducible_cases entry may carry.
var NotInducibleReasons = []string{
	NotInducibleChannelTooSmall,
	NotInducibleOutputSilentOnFailure,
	NotInducibleTerminalAtExitOnly,
	NotInducibleTerminalVocabularyClosed,
}

// ExclusionKind separates what a case's absence from a surface's observations
// can mean. Collapsing them lets a surface that reports nothing look like one
// with nothing to report, turning a weakness into an advantage.
type ExclusionKind uint8

const (
	// ExclusionNone means the case carries its obligation and is graded on what
	// the surface reported.
	ExclusionNone ExclusionKind = iota

	// ExclusionNotApplicable means the runtime reaches this outcome through no
	// code path, so there is nothing to report and no obligation to compare.
	ExclusionNotApplicable

	// ExclusionNotInduced means the measurer cannot bring the condition about
	// on this surface; the case stays unmeasured. It is a limitation of the
	// measurer, not a finding about the runtime.
	ExclusionNotInduced

	// ExclusionSurfaceSilent means the condition arises and the surface carries
	// no account of it, so the case keeps its obligation and the surface fails
	// it.
	ExclusionSurfaceSilent
)

// notInducibleExclusion maps each not_inducible_cases reason onto what its
// absence means. Only prompt_channel_too_small describes the measurer's reach;
// the others describe a channel silent on a condition the measurer can create.
var notInducibleExclusion = map[string]ExclusionKind{
	NotInducibleChannelTooSmall:          ExclusionNotInduced,
	NotInducibleOutputSilentOnFailure:    ExclusionSurfaceSilent,
	NotInducibleTerminalAtExitOnly:       ExclusionSurfaceSilent,
	NotInducibleTerminalVocabularyClosed: ExclusionSurfaceSilent,
}

// NotInducibleExclusion reports what one not_inducible_cases reason means for
// the case's obligation. An unknown reason reports ExclusionNone, keeping the
// obligation rather than quietly dropping it.
func NotInducibleExclusion(reason string) ExclusionKind {
	return notInducibleExclusion[reason]
}

// The closed recall detail set. A continuation recall record carries
// exactly one of these; every other value is invalid.
const (
	RecallConfirmedSameSession = "confirmed_same_session"
	RecallFreshFallback        = "fresh_session_fallback"
	// RecallSameSessionWithoutRecall states that the runtime carried the recall
	// turn in the seed's session and still did not return what the seed asked
	// it to remember: neither an unobserved identifier nor a fresh-session
	// fallback fits.
	RecallSameSessionWithoutRecall = "same_session_without_recall"
	// RecallDeclined states that the runtime carried the recall turn in the
	// seed's session and the answer declined to give one, establishing neither
	// that the history reached the model nor that it did not.
	RecallDeclined         = "same_session_answer_declined"
	RecallUnobservedActual = "unobserved_actual_session"
	// RecallPreconditionUnmet states that the probe could not
	// establish the condition its observation requires, so no
	// continuation was tried and nothing about the runtime was
	// observed.
	RecallPreconditionUnmet = "recall_precondition_unmet"
)

// DeclaredGapPeers pairs the two semantic cases whose outcomes derive from one
// physical run, so a declaration of one without the other is incoherent.
var DeclaredGapPeers = map[Case]Case{
	CaseRuntimeRefusal:      CaseNonRetryableRefusal,
	CaseNonRetryableRefusal: CaseRuntimeRefusal,
}

// EvidencePathQualificationVerdict is the evidence_path the single final
// aggregate record carries.
const EvidencePathQualificationVerdict = "qualification.verdict"

// Record is one strict evidence line. The field set is closed: unknown or
// missing fields are invalid, and every field's nullability is fixed.
type Record struct {
	SchemaVersion   int        `json:"schema_version"`
	Sequence        int        `json:"sequence"`
	ObservedAt      string     `json:"observed_at"`
	Scenario        Scenario   `json:"scenario"`
	Surface         Surface    `json:"surface"`
	Capability      Capability `json:"capability"`
	Source          Source     `json:"source"`
	Grade           Grade      `json:"grade"`
	Outcome         Outcome    `json:"outcome"`
	SemanticCase    *Case      `json:"semantic_case"`
	InputID         InputID    `json:"input_id"`
	EvidencePath    *string    `json:"evidence_path"`
	SessionID       *string    `json:"session_id"`
	PriorSessionID  *string    `json:"prior_session_id"`
	AgentName       *string    `json:"agent_name"`
	AgentVersion    *string    `json:"agent_version"`
	ProtocolVersion *int       `json:"protocol_version"`
	Detail          string     `json:"detail"`
	// ExtensionSource is the protocol surface's reading of the token-bearing
	// extension, carried by the token inventory rows it belongs to and null
	// elsewhere.
	ExtensionSource *ExtensionSource `json:"extension_source"`
	// ExtensionAdmitted reports whether the rules let that source stand as the
	// figure a budget is kept in. Reading a present-but-unadmitted source as no
	// source lets a partial reading be spent as a total.
	ExtensionAdmitted *bool `json:"extension_admitted"`
}

var recordFields = map[string]bool{
	"schema_version": true, "sequence": true, "observed_at": true,
	"scenario": true, "surface": true, "capability": true, "source": true,
	"grade": true, "outcome": true, "semantic_case": true,
	"input_id": true, "evidence_path": true, "session_id": true,
	"prior_session_id": true, "agent_name": true, "agent_version": true,
	"protocol_version": true, "detail": true,
	"extension_source": true, "extension_admitted": true,
}

// DetailBound is the maximum number of Unicode code points a detail string may
// carry.
const DetailBound = 256

// DecodeRecord strictly decodes one evidence line. It rejects unknown and
// missing fields, wrong types, null where a value is required, values outside
// the closed enum sets, a schema version other than 1, a non-UTC or
// unparseable timestamp, an empty or over-bound detail, and an empty string in
// any nullable string field.
func DecodeRecord(line []byte) (Record, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Record{}, fmt.Errorf("decode evidence line: %w", err)
	}
	for name := range fields {
		if !recordFields[name] {
			return Record{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range recordFields {
		if _, ok := fields[name]; !ok {
			return Record{}, fmt.Errorf("missing field %q", name)
		}
	}

	var rec Record
	var err error
	if rec.SchemaVersion, err = decodeInt(fields["schema_version"]); err != nil {
		return Record{}, fmt.Errorf("schema_version: %w", err)
	}
	if rec.SchemaVersion != 1 {
		return Record{}, fmt.Errorf("schema_version = %d, want 1", rec.SchemaVersion)
	}
	if rec.Sequence, err = decodeInt(fields["sequence"]); err != nil {
		return Record{}, fmt.Errorf("sequence: %w", err)
	}
	if rec.ObservedAt, err = decodeString(fields["observed_at"]); err != nil {
		return Record{}, fmt.Errorf("observed_at: %w", err)
	}
	ts, tsErr := time.Parse(time.RFC3339, rec.ObservedAt)
	if tsErr != nil {
		return Record{}, fmt.Errorf("observed_at %q: %w", rec.ObservedAt, tsErr)
	}
	if _, offset := ts.Zone(); offset != 0 {
		return Record{}, fmt.Errorf("observed_at %q is not UTC", rec.ObservedAt)
	}
	if rec.Scenario, err = decodeEnum(fields["scenario"], Scenarios); err != nil {
		return Record{}, fmt.Errorf("scenario: %w", err)
	}
	if rec.Surface, err = decodeEnum(fields["surface"], Surfaces); err != nil {
		return Record{}, fmt.Errorf("surface: %w", err)
	}
	if rec.Capability, err = decodeEnum(fields["capability"], Capabilities); err != nil {
		return Record{}, fmt.Errorf("capability: %w", err)
	}
	if rec.Source, err = decodeEnum(fields["source"], Sources); err != nil {
		return Record{}, fmt.Errorf("source: %w", err)
	}
	if rec.Grade, err = decodeEnum(fields["grade"], Grades); err != nil {
		return Record{}, fmt.Errorf("grade: %w", err)
	}
	if rec.Outcome, err = decodeEnum(fields["outcome"], Outcomes); err != nil {
		return Record{}, fmt.Errorf("outcome: %w", err)
	}
	if rec.SemanticCase, err = decodeNullableEnum(fields["semantic_case"], Cases); err != nil {
		return Record{}, fmt.Errorf("semantic_case: %w", err)
	}
	if rec.InputID, err = decodeEnum(fields["input_id"], InputIDs); err != nil {
		return Record{}, fmt.Errorf("input_id: %w", err)
	}
	if rec.EvidencePath, err = decodeNullableString(fields["evidence_path"]); err != nil {
		return Record{}, fmt.Errorf("evidence_path: %w", err)
	}
	if rec.SessionID, err = decodeNullableString(fields["session_id"]); err != nil {
		return Record{}, fmt.Errorf("session_id: %w", err)
	}
	if rec.PriorSessionID, err = decodeNullableString(fields["prior_session_id"]); err != nil {
		return Record{}, fmt.Errorf("prior_session_id: %w", err)
	}
	if rec.AgentName, err = decodeNullableString(fields["agent_name"]); err != nil {
		return Record{}, fmt.Errorf("agent_name: %w", err)
	}
	if rec.AgentVersion, err = decodeNullableString(fields["agent_version"]); err != nil {
		return Record{}, fmt.Errorf("agent_version: %w", err)
	}
	if rec.ProtocolVersion, err = decodeNullableInt(fields["protocol_version"]); err != nil {
		return Record{}, fmt.Errorf("protocol_version: %w", err)
	}
	if rec.Detail, err = decodeString(fields["detail"]); err != nil {
		return Record{}, fmt.Errorf("detail: %w", err)
	}
	if points := utf8.RuneCountInString(rec.Detail); points == 0 {
		return Record{}, errors.New("detail is empty")
	} else if points > DetailBound {
		return Record{}, fmt.Errorf("detail carries %d code points, want at most %d", points, DetailBound)
	}
	if rec.ExtensionSource, err = decodeNullableEnum(fields["extension_source"], ExtensionSources); err != nil {
		return Record{}, fmt.Errorf("extension_source: %w", err)
	}
	if rec.ExtensionAdmitted, err = decodeNullableBool(fields["extension_admitted"]); err != nil {
		return Record{}, fmt.Errorf("extension_admitted: %w", err)
	}
	return rec, nil
}

func decodeNullableBool(raw json.RawMessage) (*bool, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// decodeString decodes a JSON string, rejecting null and any
// non-string value.
func decodeString(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", errors.New("got null, want a string")
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v, nil
}

// decodeNullableString decodes a JSON string or null, rejecting an empty
// non-null string: a nullable string field attests to an observed value, and
// an empty string would satisfy a session-relation or identity check while
// attesting to nothing.
func decodeNullableString(raw json.RawMessage) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	v, err := decodeString(raw)
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, errors.New("is empty, want null or a non-empty string")
	}
	return &v, nil
}

func decodeInt(raw json.RawMessage) (int, error) {
	if string(raw) == "null" {
		return 0, errors.New("got null, want an integer")
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func decodeNullableInt(raw json.RawMessage) (*int, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	v, err := decodeInt(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func decodeEnum[T ~string](raw json.RawMessage, allowed []T) (T, error) {
	v, err := decodeString(raw)
	if err != nil {
		return "", err
	}
	value := T(v)
	if !slices.Contains(allowed, value) {
		return "", fmt.Errorf("%q is outside the closed value set", v)
	}
	return value, nil
}

func decodeNullableEnum[T ~string](raw json.RawMessage, allowed []T) (*T, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	v, err := decodeEnum(raw, allowed)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// MarshalRecord renders one record as an evidence line body, always carrying
// the exact closed field set.
func MarshalRecord(rec Record) ([]byte, error) {
	return json.Marshal(rec)
}

// ValidRecord returns a fully populated, schema-valid record for decode tests
// to mutate.
func ValidRecord() Record {
	return Record{
		SchemaVersion:   1,
		Sequence:        1,
		ObservedAt:      "2026-01-01T00:00:00Z",
		Scenario:        ScenarioSemanticProbe,
		Surface:         SurfaceProtocol,
		Capability:      CapabilityTurnDisposition,
		Source:          SourceProtocolStable,
		Grade:           GradeUsable,
		Outcome:         OutcomePass,
		SemanticCase:    new(CaseSuccess),
		InputID:         InputDispositionSuccess,
		EvidencePath:    new("/turn/stop_reason"),
		SessionID:       new("sess-fixture"),
		AgentName:       new("fixture-agent"),
		AgentVersion:    new("1.0.0-fixture"),
		ProtocolVersion: new(1),
		Detail:          "bounded fixture detail",
	}
}

// NullableEqual compares two nullable values by pointee, treating two nils as
// equal.
func NullableEqual[T comparable](a, b *T) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}

// RecordsEqual reports whether two records carry equal values, dereferencing
// the nullable fields rather than comparing pointers.
func RecordsEqual(a, b Record) bool {
	if a.SchemaVersion != b.SchemaVersion || a.Sequence != b.Sequence || a.ObservedAt != b.ObservedAt ||
		a.Scenario != b.Scenario || a.Surface != b.Surface || a.Capability != b.Capability ||
		a.Source != b.Source || a.Grade != b.Grade ||
		a.Outcome != b.Outcome || a.InputID != b.InputID || a.Detail != b.Detail {
		return false
	}
	return NullableEqual(a.SemanticCase, b.SemanticCase) &&
		NullableEqual(a.EvidencePath, b.EvidencePath) &&
		NullableEqual(a.SessionID, b.SessionID) &&
		NullableEqual(a.PriorSessionID, b.PriorSessionID) &&
		NullableEqual(a.AgentName, b.AgentName) &&
		NullableEqual(a.AgentVersion, b.AgentVersion) &&
		NullableEqual(a.ProtocolVersion, b.ProtocolVersion)
}
