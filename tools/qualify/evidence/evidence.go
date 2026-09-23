// Package evidence defines the qualification harness's evidence, journal, and
// measurement schemas, and imports no other tools/qualify package.
package evidence

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
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

// Question names what one verdict answers. A run answers both over one
// observation set, reported apart because an answer to one is not an answer to
// the other.
type Question string

const (
	// QuestionTransportParity asks whether the protocol route can replace the
	// native route, using this runtime's own native surfaces as the reference.
	QuestionTransportParity Question = "transport_parity"

	// QuestionProductConformance asks whether the effective adapter meets
	// Sortie's obligation, reading no native surface as an excuse.
	QuestionProductConformance Question = "product_conformance"
)

// Questions is the closed question set.
var Questions = []Question{QuestionTransportParity, QuestionProductConformance}

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

// SuppliedOutsideProtocol reports whether source reached Sortie through its
// own code rather than the protocol wire, excluding it from every surface's baseline.
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

// CatalogNotInducibleCases is the closed set of cases no deterministic inducer
// reaches on any runtime: CaseUnknownOutcome would need a stop reason outside
// the protocol's closed enum.
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

// IsNotInducibleDetail reports whether detail names a recognized
// not_inducible reason.
func IsNotInducibleDetail(detail string) bool {
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
	// RecallSameSessionWithoutRecall states the recall turn ran in the seed's
	// session but did not return what it asked to remember.
	RecallSameSessionWithoutRecall = "same_session_without_recall"
	// RecallDeclined states the recall turn ran in the seed's session and its
	// answer declined to give one, so recall is neither confirmed nor denied.
	RecallDeclined         = "same_session_answer_declined"
	RecallUnobservedActual = "unobserved_actual_session"
	// RecallPreconditionUnmet states that the probe could not establish the
	// condition its observation requires, so no continuation was tried.
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

// DecodeRecord strictly decodes one evidence line, rejecting an unknown or
// missing field, a value outside its closed set, and a malformed timestamp or detail.
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
	if rec.SchemaVersion, err = DecodeInt(fields["schema_version"]); err != nil {
		return Record{}, fmt.Errorf("schema_version: %w", err)
	}
	if rec.SchemaVersion != 1 {
		return Record{}, fmt.Errorf("schema_version = %d, want 1", rec.SchemaVersion)
	}
	if rec.Sequence, err = DecodeInt(fields["sequence"]); err != nil {
		return Record{}, fmt.Errorf("sequence: %w", err)
	}
	if rec.ObservedAt, err = DecodeString(fields["observed_at"]); err != nil {
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
	if rec.Detail, err = DecodeString(fields["detail"]); err != nil {
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

// DecodeString decodes raw as a required JSON string, rejecting null.
func DecodeString(raw json.RawMessage) (string, error) {
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
// non-null string, which would satisfy an identity check while attesting to nothing.
func decodeNullableString(raw json.RawMessage) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	v, err := DecodeString(raw)
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, errors.New("is empty, want null or a non-empty string")
	}
	return &v, nil
}

// DecodeInt decodes raw as a required JSON integer, rejecting null.
func DecodeInt(raw json.RawMessage) (int, error) {
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
	v, err := DecodeInt(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func decodeEnum[T ~string](raw json.RawMessage, allowed []T) (T, error) {
	v, err := DecodeString(raw)
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

// NotesGrade is one surface-capability grade a notes document reports, with the
// status label the document's row must carry for it.
type NotesGrade struct {
	Surface    Surface
	Capability Capability
	Grade      Grade
	Label      string
}

// NotesExpectation is what a validated run expects a runtime's notes document to
// state: both verdicts, every surface-capability grade, every excluded case, and
// every unobserved surface.
type NotesExpectation struct {
	Verdict Verdict
	// Conformance is nil when the run answered only the transport question;
	// the document must then state no product-conformance verdict.
	Conformance *Verdict
	Grades      []NotesGrade
	Excluded    []string
	Unobserved  []string
}

// NotesEligibilityPrefix is the line prefix a notes document states its
// transport-parity verdict on, and the label the rendered summary prints it
// under.
const NotesEligibilityPrefix = "Eligibility: "

// NotesConformancePrefix is the line prefix a notes document states its
// product-conformance verdict on.
const NotesConformancePrefix = "Product conformance: "

// NotesScopeStatement is the sentence a notes document MUST carry to state the
// profile's Unix-only live scope.
const NotesScopeStatement = "Windows live qualification is unobserved"

// NotesSections returns the required section headings, matched as an ordered
// subsequence; the document's top-level heading, which names the runtime, is
// unconstrained.
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

// StatusLabel maps a grade to the status label a notes grade row MUST use, or
// the empty string for a grade no row can carry.
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

// NotesBindingRequired reports whether a run at this verdict MUST have a
// readable notes document to compare against; true for VerdictQualified only.
func NotesBindingRequired(v Verdict) bool {
	return v == VerdictQualified
}

func notesAlternation[T ~string](values []T) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, regexp.QuoteMeta(string(value)))
	}
	return strings.Join(quoted, "|")
}

// notesStatusLabelAlternation renders the status labels as a regex alternation,
// skipping the eligibility-only grades whose label is empty.
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
	// notesGradeRowCandidate matches a grade-row shape without constraining
	// label or grade, catching a value outside the vocabulary that
	// notesGradeRowPattern would otherwise let pass unexamined.
	notesGradeRowCandidate = regexp.MustCompile(`^- (` + notesAlternation(Surfaces) + `) ([a-z_]+): [^:]+: \S`)
	notesVersionPattern    = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)
	notesDatePattern       = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	notesEnvValuePattern   = regexp.MustCompile(`^[A-Z][A-Z0-9_]*=\S`)
)

// notesListEntries returns one section's list entries with the marker
// stripped; an unmarked prose line is not an entry.
func notesListEntries(section []string) []string {
	entries := make([]string, 0, len(section))
	for _, line := range section {
		if after, found := strings.CutPrefix(line, "- "); found {
			entries = append(entries, after)
		}
	}
	return entries
}

// matchSectionEntries reports the first disagreement between a section's list
// entries and the run's, checked both ways so neither side may add an unrecorded case.
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

// matchStatedVerdict reports the first disagreement between the verdict a
// document states on lines with prefix and the verdict the run reached. A nil
// want means the run answered no such question, so the document must state none.
func matchStatedVerdict(lines []string, prefix string, want *Verdict) error {
	var stated []string
	for _, line := range lines {
		if after, found := strings.CutPrefix(line, prefix); found {
			stated = append(stated, after)
		}
	}
	if want == nil {
		if len(stated) > 0 {
			return fmt.Errorf("notes state %q%s, an answer the validated run does not carry", prefix, stated[0])
		}
		return nil
	}
	if len(stated) != 1 {
		return fmt.Errorf("notes carry %d %q lines, want exactly 1", len(stated), prefix)
	}
	if stated[0] != string(*want) {
		return fmt.Errorf("notes state %q%s, which does not match the validated %q", prefix, stated[0], *want)
	}
	return nil
}

// ValidateNotes reports the first disagreement between a notes document and a
// validated run's expectation, or nil when they agree. It never writes.
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

	if err := matchStatedVerdict(trimmed, NotesEligibilityPrefix, &want.Verdict); err != nil {
		return err
	}
	if err := matchStatedVerdict(trimmed, NotesConformancePrefix, want.Conformance); err != nil {
		return err
	}

	wantGrades := map[string]NotesGrade{}
	for _, grade := range want.Grades {
		wantGrades[string(grade.Surface)+" "+string(grade.Capability)] = grade
	}
	seenGrades := map[string]bool{}
	for i, line := range trimmed {
		match := notesGradeRowPattern.FindStringSubmatch(line)
		if match == nil {
			// A row shaped like a grade but failing the closed pattern carries a
			// label or grade outside the vocabulary; skipping it would let a
			// document state a contradictory grade and still validate.
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

// CheckObjectFields rejects a decoded object carrying a member outside allowed
// or missing one, catching what a direct struct decode would silently drop.
func CheckObjectFields(fields map[string]json.RawMessage, allowed map[string]bool) error {
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range allowed {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing field %q", name)
		}
	}
	return nil
}

// RowClass names one record class from the closed non-final tuple table. Every
// non-final record classifies into exactly one class.
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

// RowLabel returns the human-readable label for one row class, or
// "unclassified" for RowNone or an out-of-range value.
func RowLabel(class RowClass) string {
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

// measurableSurfaces is the closed set of surfaces the non-final tuple table
// covers. MeasurableSurfaces returns a fresh copy so no caller can edit the
// set the cardinality rules enforce.
var measurableSurfaces = []Surface{
	SurfaceProtocol, SurfaceNativeJSON, SurfaceNativeStreamJSON,
}

// MeasurableSurfaces returns the closed set of surfaces the non-final tuple
// table covers, as a fresh slice.
func MeasurableSurfaces() []Surface {
	return slices.Clone(measurableSurfaces)
}

// comparisonCapabilities is the four capabilities eligibility is decided over.
// ComparisonCapabilities returns a fresh copy so no caller can edit the set
// the cardinality rules enforce.
var comparisonCapabilities = []Capability{
	CapabilityTurnDisposition, CapabilityRetryClassification,
	CapabilityTokenCeiling, CapabilitySessionContinuation,
}

// ComparisonCapabilities returns the four capabilities eligibility is decided
// over, as a fresh slice.
func ComparisonCapabilities() []Capability {
	return slices.Clone(comparisonCapabilities)
}

// scenarioWriteOrder is the canonical record write order, which determines
// sequence. It differs from the enum declaration order in Scenarios, which only
// bounds valid values.
var scenarioWriteOrder = []Scenario{
	ScenarioWorkspaceSecurity, ScenarioPolicyPrecondition,
	ScenarioSemanticProbe, ScenarioSurfaceBaseline,
	ScenarioTokenSource, ScenarioPermissionRequest,
	ScenarioToolServer, ScenarioContinuation,
	ScenarioRuntimeIdentity, ScenarioEndToEnd,
	ScenarioProcessCleanup, ScenarioQualification,
}

func rank[T ~string](order []T, value T) int {
	return slices.Index(order, value)
}

// CompareNullableString orders two nullable strings with null first.
func CompareNullableString(a, b *string) int {
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

// OrderCompare orders two records by the canonical write order and is the
// single source of truth for it: every collector must sort with it rather than
// reimplement the rule.
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
		return CompareNullableString(a.EvidencePath, b.EvidencePath)
	case ScenarioRuntimeIdentity:
		return CompareNullableString(a.SessionID, b.SessionID)
	}
	return cmp.Compare(rank(InputIDs, a.InputID), rank(InputIDs, b.InputID))
}

// BaselineClassification maps a not_inducible grade excluded for surface
// silence to gap; every other grade passes through unchanged.
func BaselineClassification(grade Grade, detail string) Grade {
	if grade == GradeNotInducible && NotInducibleExclusion(detail) == ExclusionSurfaceSilent {
		return GradeGap
	}
	return grade
}

// DeriveBaselineGrade derives one capability's baseline grade from its case
// classifications, after dropping every declared_gap and not_inducible entry.
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

// SurfaceBorne returns the records a surface itself carried, dropping every
// reading Sortie's own code supplied outside the protocol.
func SurfaceBorne(records []*Record) []*Record {
	borne := make([]*Record, 0, len(records))
	for _, rec := range records {
		if !SuppliedOutsideProtocol(rec.Source) {
			borne = append(borne, rec)
		}
	}
	return borne
}

// TokenBaselineGrade derives a surface's token baseline from the inventory it
// carried, excluding any reading supplied outside the protocol.
func TokenBaselineGrade(records []*Record) Grade {
	records = SurfaceBorne(records)
	if len(records) == 0 {
		return GradeNotObserved
	}
	usable := false
	zeroSource := false
	for _, rec := range records {
		if rec.EvidencePath == nil {
			if rec.Grade == GradeNotObserved {
				return GradeNotObserved
			}
			zeroSource = true
			continue
		}
		if rec.Grade == GradeUsable {
			usable = true
		}
	}
	if usable && !zeroSource {
		return GradeUsable
	}
	return GradeGap
}

// TokenPathGrade derives a token-bearing path's grade from its kind: "spend"
// is usable, everything else is corroboration-only.
func TokenPathGrade(kind string) Grade {
	if kind == "spend" {
		return GradeUsable
	}
	return GradeCorroborationOnly
}

// IntersectSurfaces returns the members of want that also appear in have,
// preserving want's order.
func IntersectSurfaces(want, have []Surface) []Surface {
	var kept []Surface
	for _, surface := range want {
		if slices.Contains(have, surface) {
			kept = append(kept, surface)
		}
	}
	return kept
}

// CapabilityOwning returns the capability whose case set contains caseID, or
// the empty string when no capability owns it.
func CapabilityOwning(caseID Case) Capability {
	for _, capability := range Capabilities {
		if slices.Contains(CapabilityCases[capability], caseID) {
			return capability
		}
	}
	return ""
}

// SessionIdentity is what one session's handshake reported about the agent
// serving it.
type SessionIdentity struct {
	Name    string
	Version string
}

// SemanticEvidencePath returns the per-surface evidence-path convention a
// semantic probe record carries: which field of that surface's output a
// recognized terminal is read from.
func SemanticEvidencePath(surface Surface) string {
	switch surface {
	case SurfaceProtocol:
		return "/turn/stop_reason"
	case SurfaceNativeJSON:
		return "/response/terminal"
	default:
		return "/stream/terminal"
	}
}

// MatchBaseline returns a predicate matching the surface baseline record for
// one surface and capability.
func MatchBaseline(surface Surface, capability Capability) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioSurfaceBaseline &&
			rec.Surface == surface && rec.Capability == capability
	}
}

// outcomeFailureRank ranks the not_observed failure outcomes
// DeriveBaselineOutcome chooses among, highest first.
var outcomeFailureRank = map[Outcome]int{
	OutcomeRuntimeFailed:          3,
	OutcomeFixtureInductionFailed: 2,
	OutcomePrerequisiteFailed:     1,
	OutcomeNotObserved:            0,
}

// DeriveBaselineOutcome derives a baseline record's outcome from its grade and
// contributing outcomes.
func DeriveBaselineOutcome(grade Grade, contributing []Outcome) Outcome {
	if grade != GradeNotObserved {
		return OutcomePass
	}
	best := OutcomeNotObserved
	bestRank := -1
	for _, outcome := range contributing {
		rank, ranked := outcomeFailureRank[outcome]
		if ranked && rank > bestRank {
			bestRank = rank
			best = outcome
		}
	}
	return best
}

// BoundDetail truncates detail to DetailBound Unicode code points.
func BoundDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= DetailBound {
		return detail
	}
	return string(runes[:DetailBound])
}

// Observation is one live-collected grade, outcome, detail, and identifier a
// setter writes onto a record, accepting only pairs the admission table names.
type Observation struct {
	Grade   Grade
	Outcome Outcome
	Detail  string
	// SessionID is the launch's actual identifier. Empty writes a null
	// session_id.
	SessionID string
	// EvidencePath is the field the observation was read from. Empty leaves the
	// record's current evidence_path unchanged.
	EvidencePath string
}

// TokenObservation is one resolved token-bearing path a caller writes as its
// own record.
type TokenObservation struct {
	EvidencePath string
	Kind         string // "spend" or "occupancy"
}

// ExtensionReading is what one collection established about the protocol's
// token-bearing extension: whether a source was read, and whether it is
// admitted as a budget figure.
type ExtensionReading struct {
	Source   ExtensionSource
	Admitted bool
}

// WriteOnto states the reading on one record. A nil reading writes nothing.
func (r *ExtensionReading) WriteOnto(rec *Record) {
	if r == nil {
		return
	}
	rec.ExtensionSource = new(r.Source)
	rec.ExtensionAdmitted = new(r.Admitted)
}

// observationAdmission is the closed grade-outcome admission table every live
// setter checks obs against before writing.
var observationAdmission = map[Grade][]Outcome{
	GradeUsable:            {OutcomePass},
	GradeGap:               {OutcomePass},
	GradeCorroborationOnly: {OutcomePass},
	GradeNotObserved: {
		OutcomeNotObserved, OutcomePrerequisiteFailed,
		OutcomeFixtureInductionFailed, OutcomeRuntimeFailed,
	},
	GradeDeclaredGap:  {OutcomeNotProducible},
	GradeNotInducible: {OutcomeNotInducible},
}

// CheckObservationAdmitted rejects a zero-value Outcome and any grade-outcome
// pair outside observationAdmission.
func CheckObservationAdmitted(obs Observation) error {
	if obs.Outcome == "" {
		return errors.New("observation outcome must not be the zero value")
	}
	admitted, ok := observationAdmission[obs.Grade]
	if !ok {
		return fmt.Errorf("grade %q is outside the observation admission table", obs.Grade)
	}
	if !slices.Contains(admitted, obs.Outcome) {
		return fmt.Errorf("grade %s does not admit outcome %s", obs.Grade, obs.Outcome)
	}
	return nil
}

// ApplyObservation writes obs's grade, outcome, and bounded detail onto rec,
// rewrites its session_id (null when obs.SessionID is empty), and rewrites its
// evidence_path only when obs.EvidencePath is non-empty.
func ApplyObservation(rec *Record, obs Observation) {
	rec.Grade = obs.Grade
	rec.Outcome = obs.Outcome
	rec.Detail = BoundDetail(obs.Detail)
	if obs.SessionID == "" {
		rec.SessionID = nil
	} else {
		rec.SessionID = new(obs.SessionID)
	}
	if obs.EvidencePath != "" {
		rec.EvidencePath = new(obs.EvidencePath)
	}
}

// ReadEvidenceFile reads a JSONL evidence file and strictly decodes all records.
func ReadEvidenceFile(path string) ([]Record, error) {
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
