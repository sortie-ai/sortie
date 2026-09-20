// Package evidencetest provides synthetic evidence fixtures for tests
// spanning more than one tool-module package. It exports no live-composition
// entry point; tools/qualify/probe builds live records on its own.
package evidencetest

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// The synthetic identifiers every fixture uses: public test values only, no
// credential, path, or captured runtime value.
const (
	FixtureTime      = "2026-03-04T05:06:07Z"
	FixtureAgentName = "fixture-agent"
	FixtureAgentVer  = "1.0.0-fixture"

	FixtureQualified    = "qualified"
	FixtureNotQualified = "not_qualified"
	FixtureUnmeasured   = "unmeasured"
	FixtureDeclaredGap  = "declared_gap"
	FixtureNotObserved  = "not_observed"
)

// FixtureSession builds the synthetic session identifier for one named session
// on one surface.
func FixtureSession(surface evidence.Surface, name string) string {
	return fmt.Sprintf("sess-%s-%s", surface, name)
}

// Fixture is a complete, canonically ordered evidence set. Each constructor
// variant (qualified, not_qualified, unmeasured, declared_gap, not_observed)
// records a different shape of the required observations.
type Fixture struct {
	Records []evidence.Record

	// declared accumulates one DeclaredGap per distinct (capability, case)
	// SetSemanticDeclaredGap rewrote, so Declarations can return the exact set
	// the fixture's records assume.
	declared []profile.DeclaredGap

	// absent is the set of surfaces this fixture declares absent, so its
	// records never cover them.
	absent []profile.AbsentSurface

	// variant is the constructor variant, so Finalize and AppendIdentity choose
	// the runtime-identity record the variant requires.
	variant string

	// observedAt is the timestamp every record carries: FixtureTime for a
	// synthetic fixture.
	observedAt string

	// identitySet reports whether SetRuntimeIdentity captured a live handshake;
	// when true, Finalize builds identity records from it rather than from the
	// variant's fixed constants.
	identitySet             bool
	identityObs             evidence.Observation
	identities              map[string]evidence.SessionIdentity
	identityProtocolVersion int
}

// NewFixture builds the non-final records of one variant in canonical order;
// each surface in absent gets no records at all. Finalize adds the identity records.
func NewFixture(variant string, absent ...profile.AbsentSurface) *Fixture {
	return newFixtureAt(FixtureTime, variant, absent...)
}

func newFixtureAt(observedAt, variant string, absent ...profile.AbsentSurface) *Fixture {
	f := &Fixture{absent: slices.Clone(absent), variant: variant, observedAt: observedAt}
	if variant == FixtureNotObserved {
		f.addWorkspaceSecurityNotObserved()
		f.addPolicyPreconditionNotObserved()
		f.addSemanticProbesNotObserved()
		f.addBaselinesNotObserved()
		f.addTokenInventoriesNotObserved()
		f.addPermissionNotObserved()
		f.addToolServerNotObserved()
		f.addContinuationNotObserved()
		f.addEndToEndNotObserved()
		f.addProcessCleanupNotObserved()
		return f
	}
	f.addWorkspaceSecurity()
	f.addPolicyPrecondition()
	f.addSemanticProbes()
	f.addBaselines()
	f.addTokenInventories()
	f.addPermission()
	f.addToolServer()
	f.addContinuation()
	f.addEndToEnd()
	f.addProcessCleanup()
	switch variant {
	case FixtureNotQualified:
		f.setProtocolRefusalGap()
	case FixtureUnmeasured:
		f.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)
	case FixtureDeclaredGap:
		f.SetSemanticDeclaredGap(evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal, evidence.DeclaredGapNeverProduced)
	}
	return f
}

// setProtocolRefusalGap rewrites the protocol surface's runtime_refusal
// disposition record to a conflated gap, so its baseline grades gap while the
// structured native reference stays usable.
func (f *Fixture) setProtocolRefusalGap() {
	rec := f.FindFirst(MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal))
	if rec == nil {
		return
	}
	rec.Grade = evidence.GradeGap
	rec.Detail = fmt.Sprintf("%s %s case completed with a conflated outcome on %s", evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal, evidence.SurfaceProtocol)
	f.UpdateSemanticBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition)
}

// base returns a Record stub with the fixed identity fields every fixture
// Record shares.
func (f *Fixture) base() evidence.Record {
	return evidence.Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    f.observedAt,
	}
}

func (f *Fixture) Add(rec evidence.Record) {
	f.Records = append(f.Records, rec)
}

// Finalize appends one runtime-identity record per distinct non-null actual
// protocol session id the current records reference, sorts into canonical
// order, and renumbers the sequences.
func (f *Fixture) Finalize() {
	referenced := map[string]bool{}
	for i := range f.Records {
		rec := &f.Records[i]
		if rec.Surface == evidence.SurfaceProtocol && rec.SessionID != nil {
			referenced[*rec.SessionID] = true
		}
	}
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		f.Add(f.identityRecord(id))
	}
	slices.SortStableFunc(f.Records, evidence.OrderCompare)
	f.Renumber()
}

// identityRecord builds one runtime-identity record for sessionID, choosing the
// not-observed shape on a FixtureNotObserved fixture and the qualified shape
// otherwise.
func (f *Fixture) identityRecord(sessionID string) evidence.Record {
	if f.identitySet {
		return f.liveIdentityRecord(sessionID)
	}
	rec := IdentityFixtureRecord(sessionID)
	if f.variant == FixtureNotObserved {
		rec = identityFixtureRecordNotObserved(sessionID)
	}
	rec.ObservedAt = f.observedAt
	return rec
}

// liveIdentityRecord builds one runtime-identity record for sessionID from that
// session's own handshake. A session no handshake named is recorded as
// unidentified.
func (f *Fixture) liveIdentityRecord(sessionID string) evidence.Record {
	rec := evidence.Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    f.observedAt,
		Scenario:      evidence.ScenarioRuntimeIdentity,
		Surface:       evidence.SurfaceProtocol,
		Capability:    evidence.CapabilityRuntimeIdentity,
		Source:        evidence.SourceProtocolStable,
		Grade:         f.identityObs.Grade,
		Outcome:       f.identityObs.Outcome,
		InputID:       evidence.InputIdentity,
		EvidencePath:  new("/handshake/agent_info"),
		SessionID:     new(sessionID),
		Detail:        evidence.BoundDetail(f.identityObs.Detail),
	}
	if f.identityProtocolVersion != 0 {
		rec.ProtocolVersion = new(f.identityProtocolVersion)
	}
	identity, named := f.identities[sessionID]
	if !named {
		rec.Grade = evidence.GradeNotObserved
		if f.identityObs.Grade != evidence.GradeNotObserved {
			rec.Outcome = evidence.OutcomeAdapterUnanswered
			rec.Detail = "no handshake record named this session's own agent"
		}
		return rec
	}
	rec.AgentName = new(identity.Name)
	rec.AgentVersion = new(identity.Version)
	return rec
}

// Renumber assigns one-based contiguous Sequence numbers in slice order.
func (f *Fixture) Renumber() {
	for i := range f.Records {
		f.Records[i].Sequence = i + 1
	}
}

// FindFirst returns the first Record matching match, or nil.
func (f *Fixture) FindFirst(match func(*evidence.Record) bool) *evidence.Record {
	for i := range f.Records {
		rec := &f.Records[i]
		if match(rec) {
			return rec
		}
	}
	return nil
}

// Remove deletes the Record pointed to by target, which must point into the
// fixture's own slice.
func (f *Fixture) Remove(target *evidence.Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Delete(f.Records, i, i+1)
			return
		}
	}
}

// RemoveAll deletes every Record matching match.
func (f *Fixture) RemoveAll(match func(*evidence.Record) bool) {
	f.Records = slices.DeleteFunc(f.Records, func(rec evidence.Record) bool {
		return match(&rec)
	})
}

// SetSemanticNotObserved rewrites one semantic case as not observed and
// rewrites the owning capability's baseline to the derived grade.
func (f *Fixture) SetSemanticNotObserved(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) {
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		return
	}
	rec.Outcome = evidence.OutcomeNotObserved
	rec.Grade = evidence.GradeNotObserved
	rec.SessionID = nil
	rec.EvidencePath = nil
	rec.Detail = fmt.Sprintf("%s %s case was not observed on %s", capability, caseID, surface)
	f.UpdateSemanticBaseline(surface, capability)
}

// UpdateSemanticBaseline recomputes one surface-capability baseline from that
// capability's current case records and writes the derived grade and outcome.
func (f *Fixture) UpdateSemanticBaseline(surface evidence.Surface, capability evidence.Capability) {
	var classes []evidence.Grade
	var outcomes []evidence.Outcome
	for _, caseID := range evidence.CapabilityCases[capability] {
		rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
		if rec == nil {
			return
		}
		classes = append(classes, evidence.BaselineClassification(rec.Grade, rec.Detail))
		outcomes = append(outcomes, rec.Outcome)
	}
	baseline := f.FindFirst(evidence.MatchBaseline(surface, capability))
	if baseline != nil {
		baseline.Grade = evidence.DeriveBaselineGrade(classes)
		baseline.Outcome = evidence.DeriveBaselineOutcome(baseline.Grade, outcomes)
	}
}

// SetSemanticDeclaredGap rewrites the named case to a declared gap with
// reason on every DeclarableSurfaces surface, and applies DeclaredGapPeers so its peer follows.
func (f *Fixture) SetSemanticDeclaredGap(capability evidence.Capability, caseID evidence.Case, reason string) {
	f.setSemanticDeclaredGapOne(capability, caseID, reason)
	if peer, ok := evidence.DeclaredGapPeers[caseID]; ok {
		f.setSemanticDeclaredGapOne(evidence.CapabilityOwning(peer), peer, reason)
	}
}

// setSemanticDeclaredGapOne rewrites one case's declarable-surface records and
// baselines, without the DeclaredGapPeers closure. It never touches a surface
// this fixture also declares absent.
func (f *Fixture) setSemanticDeclaredGapOne(capability evidence.Capability, caseID evidence.Case, reason string) {
	declarable := evidence.IntersectSurfaces(evidence.DeclarableSurfaces, f.measured())
	for _, surface := range declarable {
		rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
		if rec == nil {
			continue
		}
		sessionID, evidencePath := semanticIdentity(surface, caseID)
		rec.Outcome = evidence.OutcomeNotProducible
		rec.Grade = evidence.GradeDeclaredGap
		rec.Detail = reason
		rec.SessionID = new(sessionID)
		rec.EvidencePath = new(evidencePath)
	}
	f.recordDeclaration(capability, caseID, reason)
	for _, surface := range declarable {
		f.UpdateSemanticBaseline(surface, capability)
	}
}

// recordDeclaration adds one declaration entry, or rewrites its reason in
// place when the pair is already recorded, since Decode rejects a duplicate pair.
func (f *Fixture) recordDeclaration(capability evidence.Capability, caseID evidence.Case, reason string) {
	for i := range f.declared {
		if f.declared[i].Capability == capability && f.declared[i].Case == caseID {
			f.declared[i].Reason = reason
			return
		}
	}
	f.declared = append(f.declared, profile.DeclaredGap{Capability: capability, Case: caseID, Reason: reason})
}

// Declarations returns the runtime profile the fixture's declared_gap and
// absent-surface records were built from. Every other RuntimeProfile member
// stays zero: no control in this package reads them off a fixture-built profile.
func (f *Fixture) Declarations() profile.RuntimeProfile {
	return profile.RuntimeProfile{Declarations: slices.Clone(f.declared), AbsentSurfaces: slices.Clone(f.absent)}
}

// measured returns the surfaces this fixture measures: the closed measurable
// order minus every surface it declares absent.
func (f *Fixture) measured() []evidence.Surface {
	return f.Declarations().DeclaredMeasuredSurfaces()
}

func (f *Fixture) addWorkspaceSecurity() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioWorkspaceSecurity
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityWorkspaceSecurity
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputSecurity
	rec.EvidencePath = new("/workspace/settings")
	rec.Detail = "controlled checkout without project settings; run-scoped homes; allowlisted environment names only; skip-trust accepted"
	f.Add(rec)
}

func (f *Fixture) addPolicyPrecondition() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioPolicyPrecondition
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityPermissionHandling
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputPolicyControl
	rec.EvidencePath = new("policy.deny_marker")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "policy"))
	rec.AgentVersion = new(FixtureAgentVer)
	rec.Detail = "policy deny marker returned and the probe side effect stayed absent"
	f.Add(rec)
}

func (f *Fixture) addSemanticProbes() {
	for _, surface := range f.measured() {
		for _, capability := range []evidence.Capability{evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification} {
			for _, caseID := range evidence.CapabilityCases[capability] {
				f.Add(f.semanticRecord(surface, capability, caseID))
			}
		}
	}
}

// semanticRecord builds one semantic probe record, grading usable on every
// surface. Refusal retry records reuse the refusal disposition session, and the
// protocol human-input record reuses the permission attempt session.
func (f *Fixture) semanticRecord(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) evidence.Record {
	rec := f.base()
	rec.Scenario = evidence.ScenarioSemanticProbe
	rec.Surface = surface
	rec.Capability = capability
	rec.SemanticCase = new(caseID)
	rec.InputID = evidence.CaseInputs[caseID]
	rec.Outcome = evidence.OutcomePass
	rec.Grade = evidence.GradeUsable
	switch surface {
	case evidence.SurfaceProtocol:
		rec.Source = evidence.SourceProtocolStable
		rec.AgentName = new(FixtureAgentName)
		rec.AgentVersion = new(FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	default:
		rec.Source = evidence.SourceNativeStructured
		rec.AgentVersion = new(FixtureAgentVer)
	}
	sessionID, evidencePath := semanticIdentity(surface, caseID)
	rec.SessionID = new(sessionID)
	rec.EvidencePath = new(evidencePath)
	rec.Detail = fmt.Sprintf("%s %s case completed with a distinct outcome on %s", capability, caseID, surface)
	return rec
}

// semanticIdentity returns the session_id and evidence_path semanticRecord
// assigns for one surface-case tuple, factored out so SetSemanticDeclaredGap
// can restore exactly what semanticRecord would have assigned.
func semanticIdentity(surface evidence.Surface, caseID evidence.Case) (sessionID, evidencePath string) {
	switch {
	case caseID == evidence.CaseRuntimeRefusal, caseID == evidence.CaseNonRetryableRefusal:
		sessionID = FixtureSession(surface, "runtime-refusal")
	case caseID == evidence.CaseHumanInput && surface == evidence.SurfaceProtocol:
		sessionID = FixtureSession(surface, "permission")
	case caseID == evidence.CaseHumanInput:
		sessionID = FixtureSession(surface, "human-input")
	default:
		sessionID = FixtureSession(surface, string(caseID))
	}
	return sessionID, evidence.SemanticEvidencePath(surface)
}

// BaselineVerdictFor returns the outcome a derived baseline record carries for
// its grade: not_observed derives not_observed, every other grade derives pass.
func BaselineVerdictFor(classification evidence.Grade) evidence.Outcome {
	if classification == evidence.GradeNotObserved {
		return evidence.OutcomeNotObserved
	}
	return evidence.OutcomePass
}

func (f *Fixture) addBaselines() {
	for _, surface := range f.measured() {
		for _, capability := range evidence.ComparisonCapabilities() {
			rec := f.base()
			rec.Scenario = evidence.ScenarioSurfaceBaseline
			rec.Surface = surface
			rec.Capability = capability
			rec.Source = evidence.SourceComparison
			rec.InputID = evidence.InputBaseline
			rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", surface, capability))
			rec.Detail = fmt.Sprintf("derived %s grade for %s", capability, surface)
			switch capability {
			case evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification:
				var classes []evidence.Grade
				for _, caseID := range evidence.CapabilityCases[capability] {
					caseRec := f.FindFirst(MatchSemantic(surface, capability, caseID))
					classes = append(classes, evidence.BaselineClassification(caseRec.Grade, caseRec.Detail))
				}
				rec.Grade = evidence.DeriveBaselineGrade(classes)
			case evidence.CapabilityTokenCeiling:
				rec.Grade = evidence.GradeUsable
			case evidence.CapabilitySessionContinuation:
				rec.Grade = evidence.GradeUsable
			}
			rec.Outcome = BaselineVerdictFor(rec.Grade)
			f.Add(rec)
		}
	}
}

// addTokenInventories adds each measured surface's token-bearing paths. A
// surface declared absent contributes no token record.
func (f *Fixture) addTokenInventories() {
	for _, surface := range f.measured() {
		switch surface {
		case evidence.SurfaceProtocol:
			f.Add(f.tokenRecord(evidence.SurfaceProtocol, "session/prompt/result/usage",
				evidence.SourceProtocolStable, evidence.GradeUsable,
				FixtureSession(evidence.SurfaceProtocol, "success"),
				"standard usage member on the final prompt result"))
			f.Add(f.tokenRecord(evidence.SurfaceProtocol, "session/update/vendor_usage",
				evidence.SourceProtocolExtension, evidence.GradeCorroborationOnly,
				FixtureSession(evidence.SurfaceProtocol, "runtime-failure"),
				"vendor extension reports totals outside the shared contract"))
		case evidence.SurfaceNativeJSON:
			f.Add(f.tokenRecord(evidence.SurfaceNativeJSON, "/response/stats",
				evidence.SourceNativeStructured, evidence.GradeUsable,
				FixtureSession(evidence.SurfaceNativeJSON, "success"),
				"structured response carries the usage stats member"))
		case evidence.SurfaceNativeStreamJSON:
			f.Add(f.tokenRecord(evidence.SurfaceNativeStreamJSON, "/stream/final/usage",
				evidence.SourceNativeStructured, evidence.GradeUsable,
				FixtureSession(evidence.SurfaceNativeStreamJSON, "success"),
				"final stream event carries the usage member"))
		}
	}
}

func (f *Fixture) tokenRecord(surface evidence.Surface, path string, source evidence.Source, classification evidence.Grade, sessionID, detail string) evidence.Record {
	rec := f.base()
	rec.Scenario = evidence.ScenarioTokenSource
	rec.Surface = surface
	rec.Capability = evidence.CapabilityTokenCeiling
	rec.Source = source
	rec.Grade = classification
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputTokenInventory
	rec.EvidencePath = new(path)
	rec.SessionID = new(sessionID)
	rec.Detail = detail
	if surface == evidence.SurfaceProtocol {
		rec.AgentName = new(FixtureAgentName)
		rec.AgentVersion = new(FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	} else {
		rec.AgentVersion = new(FixtureAgentVer)
	}
	return rec
}

func (f *Fixture) addPermission() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioPermissionRequest
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityPermissionHandling
	rec.Source = evidence.SourceProtocolStable
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputPermissionProbe
	rec.EvidencePath = new("session/request_permission")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "permission"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "request answered with a refusing option and no request left pending"
	f.Add(rec)
}

func (f *Fixture) addToolServer() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioToolServer
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityToolServerDelivery
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputMCPProbe
	rec.EvidencePath = new("mcp_server.receipt")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "mcp"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "test server received the generated nonce and the turn consumed it"
	f.Add(rec)
}

func (f *Fixture) addContinuation() {
	for _, surface := range f.measured() {
		seedSession := FixtureSession(surface, "seed")

		seed := f.base()
		seed.Scenario = evidence.ScenarioContinuation
		seed.Surface = surface
		seed.Capability = evidence.CapabilitySessionContinuation
		seed.InputID = evidence.InputContinuationSeed
		seed.Outcome = evidence.OutcomePass
		seed.Grade = evidence.GradeUsable
		seed.Source = evidence.SourceNativeStructured
		if surface == evidence.SurfaceProtocol {
			seed.Source = evidence.SourceProtocolStable
			seed.AgentName = new(FixtureAgentName)
			seed.AgentVersion = new(FixtureAgentVer)
			seed.ProtocolVersion = new(1)
		} else {
			seed.AgentVersion = new(FixtureAgentVer)
		}
		seed.EvidencePath = new("/continuation/seed")
		seed.SessionID = new(seedSession)
		seed.Detail = "seed conversation stored the generated nonce"
		f.Add(seed)

		recall := f.base()
		recall.Scenario = evidence.ScenarioContinuation
		recall.Surface = surface
		recall.Capability = evidence.CapabilitySessionContinuation
		recall.InputID = evidence.InputContinuationRecall
		recall.Outcome = evidence.OutcomePass
		recall.Grade = evidence.GradeUsable
		recall.Source = seed.Source
		recall.EvidencePath = new("/continuation/recall")
		recall.SessionID = new(seedSession)
		recall.PriorSessionID = new(seedSession)
		recall.Detail = evidence.RecallConfirmedSameSession
		if surface == evidence.SurfaceProtocol {
			recall.AgentName = new(FixtureAgentName)
			recall.AgentVersion = new(FixtureAgentVer)
			recall.ProtocolVersion = new(1)
		} else {
			recall.AgentVersion = new(FixtureAgentVer)
		}
		f.Add(recall)
	}
}

func (f *Fixture) addEndToEnd() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioEndToEnd
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityTurnDisposition
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputE2E
	rec.EvidencePath = new("/run_history/status")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "e2e"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "one succeeded history row and the issue reached its handoff state"
	f.Add(rec)
}

func (f *Fixture) addProcessCleanup() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioProcessCleanup
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityProcessCleanup
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeUsable
	rec.Outcome = evidence.OutcomePass
	rec.InputID = evidence.InputCleanup
	rec.EvidencePath = new("process_group.liveness")
	rec.Detail = "checked_groups=9"
	f.Add(rec)
}

// notObservedDetail renders the not-observed label for every row outside those
// with their own closed detail vocabulary (semantic probes and continuation
// recalls).
func notObservedDetail(class evidence.RowClass) string {
	return evidence.RowLabel(class) + " was not observed"
}

func (f *Fixture) addWorkspaceSecurityNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioWorkspaceSecurity
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityWorkspaceSecurity
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputSecurity
	rec.EvidencePath = new("/workspace/settings")
	rec.Detail = notObservedDetail(evidence.RowWorkspaceSecurity)
	f.Add(rec)
}

func (f *Fixture) addPolicyPreconditionNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioPolicyPrecondition
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityPermissionHandling
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputPolicyControl
	rec.EvidencePath = new("policy.deny_marker")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "policy"))
	rec.AgentVersion = new(FixtureAgentVer)
	rec.Detail = notObservedDetail(evidence.RowPolicyPrecondition)
	f.Add(rec)
}

// addSemanticProbesNotObserved adds all not-observed semantic records, built
// from semanticRecord so every field this row class leaves unchanged (source,
// input_id, agent fields, protocol version) tracks the qualified variant.
func (f *Fixture) addSemanticProbesNotObserved() {
	for _, surface := range f.measured() {
		for _, capability := range []evidence.Capability{evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification} {
			for _, caseID := range evidence.CapabilityCases[capability] {
				rec := f.semanticRecord(surface, capability, caseID)
				rec.Grade = evidence.GradeNotObserved
				rec.Outcome = evidence.OutcomeNotObserved
				rec.SessionID = nil
				rec.EvidencePath = nil
				rec.Detail = fmt.Sprintf("%s %s case was not observed on %s", capability, caseID, surface)
				f.Add(rec)
			}
		}
	}
}

// addBaselinesNotObserved adds the not-observed per-surface capability
// summaries. The detail keeps the qualified variant's wording: it describes the
// derivation, not an outcome.
func (f *Fixture) addBaselinesNotObserved() {
	for _, surface := range f.measured() {
		for _, capability := range evidence.ComparisonCapabilities() {
			rec := f.base()
			rec.Scenario = evidence.ScenarioSurfaceBaseline
			rec.Surface = surface
			rec.Capability = capability
			rec.Source = evidence.SourceComparison
			rec.InputID = evidence.InputBaseline
			rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", surface, capability))
			rec.Detail = fmt.Sprintf("derived %s grade for %s", capability, surface)
			rec.Grade = evidence.GradeNotObserved
			rec.Outcome = evidence.OutcomeNotObserved
			f.Add(rec)
		}
	}
}

// addTokenInventoriesNotObserved adds one not-observed token sentinel per
// measured surface: a not-observed fixture never fabricates a token-bearing
// path.
func (f *Fixture) addTokenInventoriesNotObserved() {
	for _, surface := range f.measured() {
		rec := f.base()
		rec.Scenario = evidence.ScenarioTokenSource
		rec.Surface = surface
		rec.Capability = evidence.CapabilityTokenCeiling
		rec.Source = evidence.SourceNone
		rec.Grade = evidence.GradeNotObserved
		rec.Outcome = evidence.OutcomeNotObserved
		rec.InputID = evidence.InputTokenInventory
		rec.Detail = notObservedDetail(evidence.RowToken)
		f.Add(rec)
	}
}

func (f *Fixture) addPermissionNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioPermissionRequest
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityPermissionHandling
	rec.Source = evidence.SourceProtocolStable
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputPermissionProbe
	rec.EvidencePath = new("session/request_permission")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "permission"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(evidence.RowPermission)
	f.Add(rec)
}

func (f *Fixture) addToolServerNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioToolServer
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityToolServerDelivery
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputMCPProbe
	rec.EvidencePath = new("mcp_server.receipt")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "mcp"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(evidence.RowMCPDelivery)
	f.Add(rec)
}

// addContinuationNotObserved adds one not-observed seed and recall per measured
// surface, the recall's prior_session_id resolving to that surface's own seed
// session so checkContinuationRelations is satisfied from construction.
func (f *Fixture) addContinuationNotObserved() {
	for _, surface := range f.measured() {
		seedSession := FixtureSession(surface, "seed")

		seed := f.base()
		seed.Scenario = evidence.ScenarioContinuation
		seed.Surface = surface
		seed.Capability = evidence.CapabilitySessionContinuation
		seed.InputID = evidence.InputContinuationSeed
		seed.Grade = evidence.GradeNotObserved
		seed.Outcome = evidence.OutcomeNotObserved
		seed.Source = evidence.SourceNativeStructured
		if surface == evidence.SurfaceProtocol {
			seed.Source = evidence.SourceProtocolStable
			seed.AgentName = new(FixtureAgentName)
			seed.AgentVersion = new(FixtureAgentVer)
			seed.ProtocolVersion = new(1)
		} else {
			seed.AgentVersion = new(FixtureAgentVer)
		}
		seed.EvidencePath = new("/continuation/seed")
		seed.SessionID = new(seedSession)
		seed.Detail = notObservedDetail(evidence.RowContinuationSeed)
		f.Add(seed)

		recall := f.base()
		recall.Scenario = evidence.ScenarioContinuation
		recall.Surface = surface
		recall.Capability = evidence.CapabilitySessionContinuation
		recall.InputID = evidence.InputContinuationRecall
		recall.Grade = evidence.GradeNotObserved
		recall.Outcome = evidence.OutcomeNotObserved
		recall.Source = seed.Source
		recall.EvidencePath = new("/continuation/recall")
		recall.PriorSessionID = new(seedSession)
		recall.Detail = evidence.RecallUnobservedActual
		if surface == evidence.SurfaceProtocol {
			recall.AgentName = new(FixtureAgentName)
			recall.AgentVersion = new(FixtureAgentVer)
			recall.ProtocolVersion = new(1)
		} else {
			recall.AgentVersion = new(FixtureAgentVer)
		}
		f.Add(recall)
	}
}

func (f *Fixture) addEndToEndNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioEndToEnd
	rec.Surface = evidence.SurfaceProtocol
	rec.Capability = evidence.CapabilityTurnDisposition
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputE2E
	rec.EvidencePath = new("/run_history/status")
	rec.SessionID = new(FixtureSession(evidence.SurfaceProtocol, "e2e"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(evidence.RowEndToEnd)
	f.Add(rec)
}

func (f *Fixture) addProcessCleanupNotObserved() {
	rec := f.base()
	rec.Scenario = evidence.ScenarioProcessCleanup
	rec.Surface = evidence.SurfaceAggregate
	rec.Capability = evidence.CapabilityProcessCleanup
	rec.Source = evidence.SourceProcessObservation
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.InputID = evidence.InputCleanup
	rec.EvidencePath = new("process_group.liveness")
	rec.Detail = notObservedDetail(evidence.RowProcessCleanup)
	f.Add(rec)
}

// MatchSemantic matches one semantic tuple.
func MatchSemantic(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		if rec.Scenario != evidence.ScenarioSemanticProbe || rec.Surface != surface ||
			rec.Capability != capability || rec.SemanticCase == nil {
			return false
		}
		return *rec.SemanticCase == caseID
	}
}

// MatchContinuation matches one surface's seed or recall record.
func MatchContinuation(surface evidence.Surface, inputID evidence.InputID) func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioContinuation && rec.Surface == surface && rec.InputID == inputID
	}
}

// WriteEvidenceFile serializes records as UTF-8 JSON Lines under the test's
// temporary directory and returns the file path.
func WriteEvidenceFile(t *testing.T, records []evidence.Record) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "qualification")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	var lines []string
	for _, rec := range records {
		line, err := evidence.MarshalRecord(rec)
		if err != nil {
			t.Fatalf("marshal evidence record %d: %v", rec.Sequence, err)
		}
		lines = append(lines, string(line))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write evidence file: %v", err)
	}
	return path
}

// WriteFinalEvidenceFile serializes a non-final set plus the terminal aggregate
// record and returns the file path.
func WriteFinalEvidenceFile(t *testing.T, records []evidence.Record, classification evidence.Grade) string {
	t.Helper()
	complete := append(slices.Clone(records), aggregateFixtureRecord(classification))
	for i := range complete {
		complete[i].Sequence = i + 1
	}
	return WriteEvidenceFile(t, complete)
}

// IdentityFixtureRecord builds one runtime-identity record for one actual
// protocol session.
func IdentityFixtureRecord(sessionID string) evidence.Record {
	return evidence.Record{
		SchemaVersion:   1,
		Sequence:        0,
		ObservedAt:      FixtureTime,
		Scenario:        evidence.ScenarioRuntimeIdentity,
		Surface:         evidence.SurfaceProtocol,
		Capability:      evidence.CapabilityRuntimeIdentity,
		Source:          evidence.SourceProtocolStable,
		Grade:           evidence.GradeUsable,
		Outcome:         evidence.OutcomePass,
		InputID:         evidence.InputIdentity,
		EvidencePath:    new("/handshake/agent_info"),
		SessionID:       new(sessionID),
		AgentName:       new(FixtureAgentName),
		AgentVersion:    new(FixtureAgentVer),
		ProtocolVersion: new(1),
		Detail:          "handshake reported agent name and version",
	}
}

// identityFixtureRecordNotObserved builds one not-observed runtime-identity
// record: the id is inferred from another record's reference, but the handshake
// that would confirm the agent's name and version was not observed.
func identityFixtureRecordNotObserved(sessionID string) evidence.Record {
	return evidence.Record{
		SchemaVersion:   1,
		Sequence:        0,
		ObservedAt:      FixtureTime,
		Scenario:        evidence.ScenarioRuntimeIdentity,
		Surface:         evidence.SurfaceProtocol,
		Capability:      evidence.CapabilityRuntimeIdentity,
		Source:          evidence.SourceProtocolStable,
		Grade:           evidence.GradeNotObserved,
		Outcome:         evidence.OutcomeNotObserved,
		InputID:         evidence.InputIdentity,
		EvidencePath:    new("/handshake/agent_info"),
		SessionID:       new(sessionID),
		AgentName:       new(FixtureAgentName),
		AgentVersion:    new(FixtureAgentVer),
		ProtocolVersion: new(1),
		Detail:          notObservedDetail(evidence.RowRuntimeIdentity),
	}
}

// aggregateFixtureRecord builds the terminal qualification record.
func aggregateFixtureRecord(classification evidence.Grade) evidence.Record {
	return evidence.Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    FixtureTime,
		Scenario:      evidence.ScenarioQualification,
		Surface:       evidence.SurfaceAggregate,
		Capability:    evidence.CapabilityEligibility,
		Source:        evidence.SourceComparison,
		Grade:         classification,
		Outcome:       evidence.OutcomePass,
		InputID:       evidence.InputAggregate,
		EvidencePath:  new(evidence.EvidencePathQualificationVerdict),
		Detail:        "aggregate qualification verdict recomputed from the closed non-final evidence set",
	}
}

// SetTokenSentinel replaces one surface's token inventory with the sentinel
// record: a zero-source success when failed is false, a failed inventory
// otherwise, rewriting the token baseline to the derived grade.
func (f *Fixture) SetTokenSentinel(surface evidence.Surface, failed bool) {
	f.RemoveAll(matchTokenSurface(surface))
	sentinel := f.base()
	sentinel.Scenario = evidence.ScenarioTokenSource
	sentinel.Surface = surface
	sentinel.Capability = evidence.CapabilityTokenCeiling
	sentinel.Source = evidence.SourceNone
	sentinel.Outcome = evidence.OutcomePass
	sentinel.InputID = evidence.InputTokenInventory
	if failed {
		sentinel.Grade = evidence.GradeNotObserved
		sentinel.Outcome = evidence.OutcomeRuntimeFailed
		sentinel.Detail = "token inventory collection failed"
	} else {
		sentinel.Grade = evidence.GradeGap
		sentinel.Detail = "inventory completed with no token-bearing path"
	}
	f.Add(sentinel)

	baseline := f.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilityTokenCeiling))
	if baseline != nil {
		baseline.Grade = evidence.GradeGap
		if failed {
			baseline.Grade = evidence.GradeNotObserved
		}
		baseline.Outcome = evidence.DeriveBaselineOutcome(baseline.Grade, []evidence.Outcome{sentinel.Outcome})
	}
}

func matchTokenSurface(surface evidence.Surface) func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioTokenSource && rec.Surface == surface
	}
}

func matchToolServer() func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioToolServer && rec.Surface == evidence.SurfaceProtocol &&
			rec.Capability == evidence.CapabilityToolServerDelivery
	}
}

// matchPermission matches the single protocol permission record. It does not
// match addPolicyPrecondition's record, which classifies as a
// policy-precondition row rather than a graded permission-handling row.
func matchPermission() func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioPermissionRequest && rec.Surface == evidence.SurfaceProtocol &&
			rec.Capability == evidence.CapabilityPermissionHandling
	}
}

// requireObservedSession rejects obs with an empty SessionID, for the row
// classes checkSessionRelation requires one on.
func requireObservedSession(rowLabel string, obs evidence.Observation) error {
	if obs.SessionID == "" {
		return fmt.Errorf("%s requires a non-empty session id", rowLabel)
	}
	return nil
}

func (f *Fixture) SetSemanticObservation(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case, obs evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		return fmt.Errorf("no semantic record for surface %s capability %s case %s", surface, capability, caseID)
	}
	evidence.ApplyObservation(rec, obs)
	f.UpdateSemanticBaseline(surface, capability)
	return nil
}

// SetSemanticNotInducible writes the closed not_inducible shape onto the
// matching record and rewrites the baseline; reason is NotInducibleDetail when no inducer exists.
func (f *Fixture) SetSemanticNotInducible(surface evidence.Surface, capability evidence.Capability, caseID evidence.Case, reason string) {
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		return
	}
	rec.Grade = evidence.GradeNotInducible
	rec.Outcome = evidence.OutcomeNotInducible
	rec.Detail = reason
	rec.SessionID = nil
	rec.EvidencePath = nil
	f.UpdateSemanticBaseline(surface, capability)
}

// SetSemanticLiveDeclaredGap writes caseID's live declared-gap shape, and its
// DeclaredGapPeers partner's, onto every declarable surface using its
// observed session id; it errors, writing nothing, when one is missing.
func (f *Fixture) SetSemanticLiveDeclaredGap(capability evidence.Capability, caseID evidence.Case, reason string, observed map[evidence.Surface]string) error {
	declarable := evidence.IntersectSurfaces(evidence.DeclarableSurfaces, f.measured())
	for _, surface := range declarable {
		if observed[surface] == "" {
			return fmt.Errorf("no observed session identifier for surface %s", surface)
		}
	}
	f.setSemanticLiveDeclaredGapOne(capability, caseID, reason, observed, declarable)
	if peer, ok := evidence.DeclaredGapPeers[caseID]; ok {
		f.setSemanticLiveDeclaredGapOne(evidence.CapabilityOwning(peer), peer, reason, observed, declarable)
	}
	return nil
}

// setSemanticLiveDeclaredGapOne rewrites one case's declarable-surface records
// and baselines from observed, without the DeclaredGapPeers closure.
func (f *Fixture) setSemanticLiveDeclaredGapOne(capability evidence.Capability, caseID evidence.Case, reason string, observed map[evidence.Surface]string, declarable []evidence.Surface) {
	for _, surface := range declarable {
		rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
		if rec == nil {
			continue
		}
		rec.Outcome = evidence.OutcomeNotProducible
		rec.Grade = evidence.GradeDeclaredGap
		rec.Detail = evidence.BoundDetail(reason)
		sessionID := observed[surface]
		rec.SessionID = new(sessionID)
		rec.EvidencePath = new(evidence.SemanticEvidencePath(surface))
	}
	f.recordDeclaration(capability, caseID, reason)
	for _, surface := range declarable {
		f.UpdateSemanticBaseline(surface, capability)
	}
}

// SetToolServerDelivery writes obs onto the protocol tool-server delivery
// record, requiring a non-empty obs.SessionID.
func (f *Fixture) SetToolServerDelivery(obs evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(evidence.RowLabel(evidence.RowMCPDelivery), obs); err != nil {
		return err
	}
	rec := f.FindFirst(matchToolServer())
	if rec == nil {
		return errors.New("no tool server delivery record")
	}
	evidence.ApplyObservation(rec, obs)
	return nil
}

// SetPermissionHandling writes obs onto the protocol permission-handling
// record, requiring a non-empty obs.SessionID.
func (f *Fixture) SetPermissionHandling(obs evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(evidence.RowLabel(evidence.RowPermission), obs); err != nil {
		return err
	}
	rec := f.FindFirst(matchPermission())
	if rec == nil {
		return errors.New("no permission handling record")
	}
	evidence.ApplyObservation(rec, obs)
	return nil
}

// SetPolicyPrecondition writes obs onto the aggregate policy-precondition
// record, requiring a non-empty obs.SessionID.
func (f *Fixture) SetPolicyPrecondition(obs evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(evidence.RowLabel(evidence.RowPolicyPrecondition), obs); err != nil {
		return err
	}
	rec := f.FindFirst(func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioPolicyPrecondition && rec.Surface == evidence.SurfaceAggregate
	})
	if rec == nil {
		return errors.New("no policy precondition record")
	}
	evidence.ApplyObservation(rec, obs)
	return nil
}

// SetEndToEnd writes obs onto the isolated end-to-end record, requiring a
// non-empty obs.SessionID unless the run is not_observed.
func (f *Fixture) SetEndToEnd(obs evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}
	if obs.Grade != evidence.GradeNotObserved {
		if err := requireObservedSession(evidence.RowLabel(evidence.RowEndToEnd), obs); err != nil {
			return err
		}
	}
	rec := f.FindFirst(func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioEndToEnd && rec.Surface == evidence.SurfaceProtocol
	})
	if rec == nil {
		return errors.New("no end-to-end record")
	}
	evidence.ApplyObservation(rec, obs)
	return nil
}

// SetWorkspaceSecurity writes obs onto the aggregate workspace-security record.
// It carries no identifier, so it cannot violate the session_id rule and takes
// no error return.
func (f *Fixture) SetWorkspaceSecurity(obs evidence.Observation) {
	rec := f.FindFirst(func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioWorkspaceSecurity && rec.Surface == evidence.SurfaceAggregate
	})
	if rec == nil {
		return
	}
	evidence.ApplyObservation(rec, evidence.Observation{Grade: obs.Grade, Outcome: obs.Outcome, Detail: obs.Detail})
}

// SetProcessCleanup writes obs onto the aggregate process-cleanup record. It
// carries no identifier, so it cannot violate the session_id rule and takes no
// error return.
func (f *Fixture) SetProcessCleanup(obs evidence.Observation) {
	rec := f.FindFirst(func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioProcessCleanup && rec.Surface == evidence.SurfaceAggregate
	})
	if rec == nil {
		return
	}
	evidence.ApplyObservation(rec, evidence.Observation{Grade: obs.Grade, Outcome: obs.Outcome, Detail: obs.Detail})
}

// SetSessionContinuationObserved writes surface's seed and recall records,
// carrying seed.SessionID into the recall's prior_session_id; an empty seed.SessionID writes null for both.
func (f *Fixture) SetSessionContinuationObserved(surface evidence.Surface, seed, recall evidence.Observation) error {
	if err := evidence.CheckObservationAdmitted(seed); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := evidence.CheckObservationAdmitted(recall); err != nil {
		return fmt.Errorf("recall: %w", err)
	}
	if err := checkRecallObservation(seed.SessionID, recall); err != nil {
		return err
	}

	seedRec := f.FindFirst(MatchContinuation(surface, evidence.InputContinuationSeed))
	if seedRec == nil {
		return fmt.Errorf("no continuation seed record for surface %s", surface)
	}
	evidence.ApplyObservation(seedRec, seed)

	recallRec := f.FindFirst(MatchContinuation(surface, evidence.InputContinuationRecall))
	if recallRec == nil {
		return fmt.Errorf("no continuation recall record for surface %s", surface)
	}
	evidence.ApplyObservation(recallRec, recall)
	recallRec.PriorSessionID = nil
	if seed.SessionID != "" {
		recallRec.PriorSessionID = new(seed.SessionID)
	}
	recallRec.Detail = recall.Detail

	if baseline := f.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilitySessionContinuation)); baseline != nil {
		baseline.Grade = recall.Grade
		baseline.Detail = evidence.BoundDetail(recall.Detail)
		baseline.Outcome = evidence.DeriveBaselineOutcome(recall.Grade, []evidence.Outcome{recall.Outcome})
	}
	return nil
}

// checkRecallObservation mirrors checkRecallRecord's rules against an
// about-to-be-written recall observation, so a rejection is caught before the
// next paid turn rather than at validation time.
func checkRecallObservation(seedSessionID string, recall evidence.Observation) error {
	switch recall.Detail {
	case evidence.RecallConfirmedSameSession:
		// The proof is the answer carrying the seed's nonce, so a surface that
		// named no session on either turn still matches: two absent ids agree.
		if recall.SessionID != seedSessionID {
			return errors.New("confirmed_same_session requires the actual session id to be the seed's own")
		}
		if recall.Grade != evidence.GradeUsable {
			return fmt.Errorf("confirmed_same_session requires classification usable, got %s", recall.Grade)
		}
	case evidence.RecallFreshFallback:
		if seedSessionID == "" {
			// A run that never learned the seed's session cannot have seen the
			// runtime open a different one.
			return errors.New("fresh_session_fallback requires the seed's own observed session id")
		}
		if recall.SessionID == "" || recall.SessionID == seedSessionID {
			return errors.New("fresh_session_fallback requires a non-empty actual session id distinct from the seed's own")
		}
		if recall.Grade != evidence.GradeGap {
			return fmt.Errorf("fresh_session_fallback requires classification gap, got %s", recall.Grade)
		}
	case evidence.RecallSameSessionWithoutRecall:
		if recall.SessionID == "" || recall.SessionID != seedSessionID {
			return errors.New("same_session_without_recall requires a non-empty actual session id equal to the seed's own")
		}
		if recall.Grade != evidence.GradeGap {
			return fmt.Errorf("same_session_without_recall requires classification gap, got %s", recall.Grade)
		}
	case evidence.RecallDeclined:
		if recall.SessionID == "" || recall.SessionID != seedSessionID {
			return errors.New("same_session_answer_declined requires a non-empty actual session id equal to the seed's own")
		}
		if recall.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("same_session_answer_declined requires classification not_observed, got %s", recall.Grade)
		}
		if recall.Outcome != evidence.OutcomeFixtureInductionFailed {
			return fmt.Errorf("same_session_answer_declined requires verdict fixture_induction_failed, got %s", recall.Outcome)
		}
	case evidence.RecallUnobservedActual:
		if recall.SessionID != "" {
			return errors.New("unobserved_actual_session requires an empty session id")
		}
		if recall.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("unobserved_actual_session requires classification not_observed, got %s", recall.Grade)
		}
	case evidence.RecallPreconditionUnmet:
		if recall.SessionID != "" {
			return errors.New("recall_precondition_unmet requires an empty session id")
		}
		if recall.Grade != evidence.GradeNotObserved {
			return fmt.Errorf("recall_precondition_unmet requires classification not_observed, got %s", recall.Grade)
		}
		if recall.Outcome != evidence.OutcomePrerequisiteFailed {
			return fmt.Errorf("recall_precondition_unmet requires verdict prerequisite_failed, got %s", recall.Outcome)
		}
	default:
		return fmt.Errorf("recall detail %q is outside the closed set", recall.Detail)
	}
	return nil
}

// SetTokenInventory replaces surface's token records from paths and
// inventory, and rewrites the token baseline; it errors on an inadmissible
// inventory pair or a non-nil extension named for a native surface.
func (f *Fixture) SetTokenInventory(surface evidence.Surface, sessionID string, paths []evidence.TokenObservation, inventory evidence.Observation, extension *evidence.ExtensionReading) error {
	admitted := (inventory.Grade == evidence.GradeGap && inventory.Outcome == evidence.OutcomePass) ||
		(inventory.Grade == evidence.GradeNotObserved && (inventory.Outcome == evidence.OutcomeFixtureInductionFailed || inventory.Outcome == evidence.OutcomeRuntimeFailed))
	if !admitted {
		return fmt.Errorf("token inventory grade %s outcome %s is outside the admitted pairs", inventory.Grade, inventory.Outcome)
	}
	if extension != nil {
		if surface != evidence.SurfaceProtocol {
			return fmt.Errorf("surface %s cannot state a reading of the protocol extension point", surface)
		}
		if !slices.Contains(evidence.ExtensionSources, extension.Source) {
			return fmt.Errorf("extension source %q is outside the closed set", extension.Source)
		}
		if extension.Admitted && extension.Source != evidence.ExtensionSourcePresent {
			return fmt.Errorf("extension source %s cannot be admitted to a budget", extension.Source)
		}
	}
	if inventory.Grade == evidence.GradeNotObserved && len(paths) > 0 {
		return errors.New("a not_observed inventory must not carry any resolved token path")
	}
	if len(paths) > 0 && sessionID == "" {
		return errors.New("a resolved token path requires the session id that emitted it")
	}

	f.RemoveAll(matchTokenSurface(surface))

	if len(paths) == 0 {
		sentinel := f.base()
		sentinel.Scenario = evidence.ScenarioTokenSource
		sentinel.Surface = surface
		sentinel.Capability = evidence.CapabilityTokenCeiling
		sentinel.Source = evidence.SourceNone
		sentinel.Grade = inventory.Grade
		sentinel.Outcome = inventory.Outcome
		sentinel.InputID = evidence.InputTokenInventory
		sentinel.Detail = evidence.BoundDetail(inventory.Detail)
		extension.WriteOnto(&sentinel)
		f.Add(sentinel)
	} else {
		source := evidence.SourceNativeStructured
		if surface == evidence.SurfaceProtocol {
			source = evidence.SourceProtocolStable
		}
		for _, path := range paths {
			rec := f.base()
			rec.Scenario = evidence.ScenarioTokenSource
			rec.Surface = surface
			rec.Capability = evidence.CapabilityTokenCeiling
			rec.Source = source
			rec.Outcome = evidence.OutcomePass
			rec.InputID = evidence.InputTokenInventory
			rec.EvidencePath = new(path.EvidencePath)
			rec.SessionID = new(sessionID)
			rec.Detail = evidence.BoundDetail(inventory.Detail)
			rec.Grade = evidence.TokenPathGrade(path.Kind)
			extension.WriteOnto(&rec)
			f.Add(rec)
		}
	}

	if baseline := f.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilityTokenCeiling)); baseline != nil {
		var records []*evidence.Record
		for i := range f.Records {
			if matchTokenSurface(surface)(&f.Records[i]) {
				records = append(records, &f.Records[i])
			}
		}
		baseline.Grade = evidence.TokenBaselineGrade(records)
		var outcomes []evidence.Outcome
		for _, rec := range records {
			outcomes = append(outcomes, rec.Outcome)
		}
		baseline.Outcome = evidence.DeriveBaselineOutcome(baseline.Grade, outcomes)
	}
	return nil
}

// SetRuntimeIdentity captures each session's handshake onto every protocol
// record it names, and onto every identity record Finalize later builds. It
// rejects a non-empty obs.SessionID and an incomplete identity.
func (f *Fixture) SetRuntimeIdentity(obs evidence.Observation, identities map[string]evidence.SessionIdentity, protocolVersion int) error {
	if obs.SessionID != "" {
		return errors.New("runtime identity observation must carry an empty session id")
	}
	if obs.Grade == evidence.GradeUsable && len(identities) == 0 {
		return errors.New("a usable runtime identity observation requires at least one session's handshake")
	}
	if obs.Grade == evidence.GradeNotObserved && len(identities) > 0 {
		return errors.New("a not_observed runtime identity observation cannot carry a session handshake")
	}
	for sessionID, identity := range identities {
		if identity.Name == "" || identity.Version == "" {
			return fmt.Errorf("the handshake for session %s requires a non-empty name and version", sessionID)
		}
	}
	if obs.Outcome == evidence.OutcomeNotObserved {
		return errors.New("runtime identity observation must not carry outcome not_observed")
	}
	if protocolVersion == 0 {
		return errors.New("runtime identity observation requires a non-zero protocol_version")
	}
	if err := evidence.CheckObservationAdmitted(obs); err != nil {
		return err
	}

	f.identitySet = true
	f.identityObs = obs
	f.identities = maps.Clone(identities)
	f.identityProtocolVersion = protocolVersion

	for i := range f.Records {
		rec := &f.Records[i]
		if rec.Surface != evidence.SurfaceProtocol {
			rec.AgentVersion = nil
			continue
		}
		rec.ProtocolVersion = new(protocolVersion)
		identity, named := f.sessionIdentity(rec.SessionID)
		if !named {
			rec.AgentName = nil
			rec.AgentVersion = nil
			continue
		}
		rec.AgentName = new(identity.Name)
		rec.AgentVersion = new(identity.Version)
	}
	return nil
}

// sessionIdentity returns the handshake reading for the session
// sessionID names, and reports whether one was observed at all.
func (f *Fixture) sessionIdentity(sessionID *string) (evidence.SessionIdentity, bool) {
	if sessionID == nil {
		return evidence.SessionIdentity{}, false
	}
	identity, named := f.identities[*sessionID]
	return identity, named
}

// SetSessionContinuation rewrites surface's continuation baseline, recall,
// and seed records to grade; a grade outside usable, gap, and not_observed
// leaves every record unchanged.
func (f *Fixture) SetSessionContinuation(surface evidence.Surface, grade evidence.Grade, detail string) {
	if grade != evidence.GradeUsable && grade != evidence.GradeGap && grade != evidence.GradeNotObserved {
		return
	}
	outcome := BaselineVerdictFor(grade)

	if baseline := f.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilitySessionContinuation)); baseline != nil {
		baseline.Grade = grade
		baseline.Detail = evidence.BoundDetail(detail)
		baseline.Outcome = outcome
	}

	if recall := f.FindFirst(MatchContinuation(surface, evidence.InputContinuationRecall)); recall != nil {
		recall.Grade = grade
		recall.Outcome = outcome
		switch grade {
		case evidence.GradeUsable:
			recall.Detail = evidence.RecallConfirmedSameSession
			recall.SessionID = new(*recall.PriorSessionID)
		case evidence.GradeGap:
			recall.Detail = evidence.RecallFreshFallback
			recall.SessionID = new(FixtureSession(surface, "recall-fallback"))
		case evidence.GradeNotObserved:
			recall.Detail = evidence.RecallUnobservedActual
			recall.SessionID = nil
		}
	}

	if seed := f.FindFirst(MatchContinuation(surface, evidence.InputContinuationSeed)); seed != nil {
		if grade == evidence.GradeNotObserved {
			seed.Grade = evidence.GradeNotObserved
			seed.Outcome = evidence.OutcomeNotObserved
			seed.Detail = notObservedDetail(evidence.RowContinuationSeed)
		} else {
			seed.Grade = evidence.GradeUsable
			seed.Outcome = evidence.OutcomePass
			seed.Detail = "seed session completed a turn that left history"
		}
	}
}

// SetTokenCorroborationOnly rewrites every non-sentinel token record of one
// surface to corroboration_only and the token baseline to gap: the inventory
// completed but supplied no contract-usable source.
func (f *Fixture) SetTokenCorroborationOnly(surface evidence.Surface) {
	for i := range f.Records {
		rec := &f.Records[i]
		if matchTokenSurface(surface)(rec) && rec.EvidencePath != nil {
			rec.Grade = evidence.GradeCorroborationOnly
		}
	}
	if baseline := f.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilityTokenCeiling)); baseline != nil {
		baseline.Grade = evidence.GradeGap
	}
}

// SetTokenCompensated adds the protocol-surface token reading Sortie's code
// supplies outside the protocol, at path. The surface's inventory is left
// intact, so the published baseline keeps reporting what the wire carried.
func (f *Fixture) SetTokenCompensated(path string) {
	f.SetTokenCompensatedObserved(FixtureSession(evidence.SurfaceProtocol, "success"), path,
		"Sortie reads the spend from its own record of the turn")
}

// SetTokenCompensatedObserved adds the protocol-surface token reading Sortie's
// code supplied outside the protocol, for sessionID at path with detail; call
// it only from a run that watched its own adapter return the figure.
func (f *Fixture) SetTokenCompensatedObserved(sessionID, path, detail string) {
	rec := f.tokenRecord(evidence.SurfaceProtocol, path, evidence.SourceSortieShared, evidence.GradeUsable, sessionID, evidence.BoundDetail(detail))
	f.Add(rec)
	slices.SortStableFunc(f.Records, evidence.OrderCompare)
	f.Renumber()
}

// DuplicateAfter inserts a copy of target directly behind it, keeping canonical
// ordering intact so the duplicate key check is the check that fires.
func (f *Fixture) DuplicateAfter(target *evidence.Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Insert(f.Records, i+1, *target)
			return
		}
	}
}

// AppendIdentity adds one runtime-identity record for sessionID at its
// canonical position, for controls that introduce a new actual protocol session
// id after Finalize.
func (f *Fixture) AppendIdentity(sessionID string) {
	f.Add(f.identityRecord(sessionID))
	slices.SortStableFunc(f.Records, evidence.OrderCompare)
	f.Renumber()
}

// TokenRecordCount counts records with the token-source scenario.
func TokenRecordCount(records []evidence.Record) int {
	count := 0
	for i := range records {
		if records[i].Scenario == evidence.ScenarioTokenSource {
			count++
		}
	}
	return count
}

// ProtocolSessionCount counts the distinct non-null actual protocol session ids
// a record set references.
func ProtocolSessionCount(records []evidence.Record) int {
	ids := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == evidence.SurfaceProtocol && rec.SessionID != nil {
			ids[*rec.SessionID] = true
		}
	}
	return len(ids)
}
