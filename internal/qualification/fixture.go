package qualification

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
// on one Surface.
func FixtureSession(Surface Surface, name string) string {
	return fmt.Sprintf("sess-%s-%s", Surface, name)
}

// Fixture is a complete, canonically ordered evidence set. Each constructor
// variant (qualified, not_qualified, unmeasured, declared_gap, not_observed)
// records a different shape of the required observations.
type Fixture struct {
	Records []Record

	// declared accumulates one DeclaredGap per distinct (capability, case)
	// SetSemanticDeclaredGap rewrote, so Declarations can return the exact set
	// the fixture's records assume.
	declared []DeclaredGap

	// absent is the set of surfaces this fixture declares absent, so its
	// records never cover them.
	absent []AbsentSurface

	// variant is the constructor variant, so Finalize and AppendIdentity choose
	// the runtime-identity record the variant requires.
	variant string

	// observedAt is the timestamp every record carries: FixtureTime for a
	// synthetic fixture, the collection's start moment for a live one, so no
	// published row is dated by a compiled-in constant.
	observedAt string

	// identitySet reports whether SetRuntimeIdentity captured a live handshake;
	// when true, Finalize builds identity records from it rather than from the
	// variant's fixed constants.
	identitySet             bool
	identityObs             Observation
	identities              map[string]SessionIdentity
	identityProtocolVersion int
}

// SessionIdentity is what one session's handshake reported about the agent
// serving it.
type SessionIdentity struct {
	Name    string
	Version string
}

// NewFixture builds the non-final records of one variant in canonical order,
// declaring every surface in absent as one the runtime does not offer. An
// absent surface carries no records at all, and the comparison rows that would
// read it stand on the protocol surface alone. Finalize adds the identity
// records.
func NewFixture(variant string, absent ...AbsentSurface) *Fixture {
	return newFixtureAt(FixtureTime, variant, absent...)
}

// NewLiveFixture builds the not-observed skeleton a live collection composes
// its observations onto, dating every record by observedAt. Reusing NewFixture
// would publish the synthetic fixture timestamp as the time of a run that
// happened elsewhere.
func NewLiveFixture(observedAt time.Time, absent ...AbsentSurface) *Fixture {
	return newFixtureAt(observedAt.UTC().Format(time.RFC3339), FixtureNotObserved, absent...)
}

func newFixtureAt(observedAt, variant string, absent ...AbsentSurface) *Fixture {
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
		f.SetSemanticNotObserved(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal)
	case FixtureDeclaredGap:
		f.SetSemanticDeclaredGap(CapabilityTurnDisposition, CaseRuntimeRefusal, DeclaredGapNeverProduced)
	}
	return f
}

// setProtocolRefusalGap rewrites the protocol surface's runtime_refusal
// disposition record to a conflated gap, so its baseline grades gap while the
// structured native reference stays usable.
func (f *Fixture) setProtocolRefusalGap() {
	rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal))
	if rec == nil {
		return
	}
	rec.Grade = GradeGap
	rec.Detail = fmt.Sprintf("%s %s case completed with a conflated outcome on %s", CapabilityTurnDisposition, CaseRuntimeRefusal, SurfaceProtocol)
	f.UpdateSemanticBaseline(SurfaceProtocol, CapabilityTurnDisposition)
}

// base returns a Record stub with the fixed identity fields every fixture
// Record shares.
func (f *Fixture) base() Record {
	return Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    f.observedAt,
	}
}

func (f *Fixture) Add(rec Record) {
	f.Records = append(f.Records, rec)
}

// Finalize appends one runtime-identity record per distinct non-null actual
// protocol session id the current records reference, sorts into canonical
// order, and renumbers the sequences.
func (f *Fixture) Finalize() {
	referenced := map[string]bool{}
	for i := range f.Records {
		rec := &f.Records[i]
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
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
	slices.SortStableFunc(f.Records, OrderCompare)
	f.Renumber()
}

// identityRecord builds one runtime-identity record for sessionID, choosing the
// not-observed shape on a FixtureNotObserved fixture and the qualified shape
// otherwise.
func (f *Fixture) identityRecord(sessionID string) Record {
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
func (f *Fixture) liveIdentityRecord(sessionID string) Record {
	rec := Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    f.observedAt,
		Scenario:      ScenarioRuntimeIdentity,
		Surface:       SurfaceProtocol,
		Capability:    CapabilityRuntimeIdentity,
		Source:        SourceProtocolStable,
		Grade:         f.identityObs.Grade,
		Outcome:       f.identityObs.Outcome,
		InputID:       InputIdentity,
		EvidencePath:  new("/handshake/agent_info"),
		SessionID:     new(sessionID),
		Detail:        boundDetail(f.identityObs.Detail),
	}
	if f.identityProtocolVersion != 0 {
		rec.ProtocolVersion = new(f.identityProtocolVersion)
	}
	identity, named := f.identities[sessionID]
	if !named {
		rec.Grade = GradeNotObserved
		if f.identityObs.Grade != GradeNotObserved {
			rec.Outcome = OutcomeAdapterUnanswered
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
func (f *Fixture) FindFirst(match func(*Record) bool) *Record {
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
func (f *Fixture) Remove(target *Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Delete(f.Records, i, i+1)
			return
		}
	}
}

// RemoveAll deletes every Record matching match.
func (f *Fixture) RemoveAll(match func(*Record) bool) {
	f.Records = slices.DeleteFunc(f.Records, func(rec Record) bool {
		return match(&rec)
	})
}

// SetSemanticNotObserved rewrites one semantic Case as not observed and
// rewrites the owning Capability's baseline to the derived Grade.
func (f *Fixture) SetSemanticNotObserved(Surface Surface, Capability Capability, caseID Case) {
	rec := f.FindFirst(MatchSemantic(Surface, Capability, caseID))
	if rec == nil {
		return
	}
	rec.Outcome = OutcomeNotObserved
	rec.Grade = GradeNotObserved
	rec.SessionID = nil
	rec.EvidencePath = nil
	rec.Detail = fmt.Sprintf("%s %s Case was not observed on %s", Capability, caseID, Surface)
	f.UpdateSemanticBaseline(Surface, Capability)
}

// UpdateSemanticBaseline recomputes one Surface-Capability baseline from that
// Capability's current Case records and writes the derived Grade and outcome.
func (f *Fixture) UpdateSemanticBaseline(Surface Surface, Capability Capability) {
	var classes []Grade
	var outcomes []Outcome
	for _, caseID := range CapabilityCases[Capability] {
		rec := f.FindFirst(MatchSemantic(Surface, Capability, caseID))
		if rec == nil {
			return
		}
		classes = append(classes, BaselineClassification(rec.Grade, rec.Detail))
		outcomes = append(outcomes, rec.Outcome)
	}
	baseline := f.FindFirst(MatchBaseline(Surface, Capability))
	if baseline != nil {
		baseline.Grade = DeriveBaselineGrade(classes)
		baseline.Outcome = DeriveBaselineOutcome(baseline.Grade, outcomes)
	}
}

// SetSemanticDeclaredGap rewrites the named case's record on every
// DeclarableSurfaces surface to a declared gap with reason, rewrites each
// owning capability's baseline, and applies the DeclaredGapPeers closure so
// declaring one case also rewrites its peer.
func (f *Fixture) SetSemanticDeclaredGap(capability Capability, caseID Case, reason string) {
	f.setSemanticDeclaredGapOne(capability, caseID, reason)
	if peer, ok := DeclaredGapPeers[caseID]; ok {
		f.setSemanticDeclaredGapOne(capabilityOwning(peer), peer, reason)
	}
}

// setSemanticDeclaredGapOne rewrites one case's declarable-surface records and
// baselines, without the DeclaredGapPeers closure. It never touches a surface
// this fixture also declares absent.
func (f *Fixture) setSemanticDeclaredGapOne(capability Capability, caseID Case, reason string) {
	declarable := intersectSurfaces(DeclarableSurfaces, f.measured())
	for _, surface := range declarable {
		rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
		if rec == nil {
			continue
		}
		sessionID, evidencePath := semanticIdentity(surface, caseID)
		rec.Outcome = OutcomeNotProducible
		rec.Grade = GradeDeclaredGap
		rec.Detail = reason
		rec.SessionID = new(sessionID)
		rec.EvidencePath = new(evidencePath)
	}
	f.recordDeclaration(capability, caseID, reason)
	for _, surface := range declarable {
		f.UpdateSemanticBaseline(surface, capability)
	}
}

// recordDeclaration adds one declaration entry, or rewrites its reason in place
// when the pair is already recorded. A pair reaches here twice when the peer
// closure and a direct call declare the same case; DecodeRuntimeProfile rejects
// a duplicate pair, so recording it twice would build a document the fixture's
// declared_gap records could never be authorized under.
func (f *Fixture) recordDeclaration(capability Capability, caseID Case, reason string) {
	for i := range f.declared {
		if f.declared[i].Capability == capability && f.declared[i].Case == caseID {
			f.declared[i].Reason = reason
			return
		}
	}
	f.declared = append(f.declared, DeclaredGap{Capability: capability, Case: caseID, Reason: reason})
}

// Declarations returns the runtime profile the fixture's declared_gap and
// absent-surface records were built from. Every other RuntimeProfile member
// stays zero: no control in this package reads them off a fixture-built profile.
func (f *Fixture) Declarations() RuntimeProfile {
	return RuntimeProfile{Declarations: slices.Clone(f.declared), AbsentSurfaces: slices.Clone(f.absent)}
}

// measured returns the surfaces this fixture measures: the closed measurable
// order minus every surface it declares absent.
func (f *Fixture) measured() []Surface {
	return measuredSurfaces(f.Declarations())
}

func (f *Fixture) addWorkspaceSecurity() {
	rec := f.base()
	rec.Scenario = ScenarioWorkspaceSecurity
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityWorkspaceSecurity
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputSecurity
	rec.EvidencePath = new("/workspace/settings")
	rec.Detail = "controlled checkout without project settings; run-scoped homes; allowlisted environment names only; skip-trust accepted"
	f.Add(rec)
}

func (f *Fixture) addPolicyPrecondition() {
	rec := f.base()
	rec.Scenario = ScenarioPolicyPrecondition
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputPolicyControl
	rec.EvidencePath = new("policy.deny_marker")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "policy"))
	rec.AgentVersion = new(FixtureAgentVer)
	rec.Detail = "policy deny marker returned and the probe side effect stayed absent"
	f.Add(rec)
}

func (f *Fixture) addSemanticProbes() {
	for _, Surface := range f.measured() {
		for _, Capability := range []Capability{CapabilityTurnDisposition, CapabilityRetryClassification} {
			for _, caseID := range CapabilityCases[Capability] {
				f.Add(f.semanticRecord(Surface, Capability, caseID))
			}
		}
	}
}

// semanticRecord builds one semantic probe Record, Grading usable on every
// Surface. Refusal retry records reuse the refusal disposition session, and the
// protocol human-input Record reuses the permission attempt session.
func (f *Fixture) semanticRecord(Surface Surface, Capability Capability, caseID Case) Record {
	rec := f.base()
	rec.Scenario = ScenarioSemanticProbe
	rec.Surface = Surface
	rec.Capability = Capability
	rec.SemanticCase = new(caseID)
	rec.InputID = CaseInputs[caseID]
	rec.Outcome = OutcomePass
	rec.Grade = GradeUsable
	switch Surface {
	case SurfaceProtocol:
		rec.Source = SourceProtocolStable
		rec.AgentName = new(FixtureAgentName)
		rec.AgentVersion = new(FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	default:
		rec.Source = SourceNativeStructured
		rec.AgentVersion = new(FixtureAgentVer)
	}
	sessionID, evidencePath := semanticIdentity(Surface, caseID)
	rec.SessionID = new(sessionID)
	rec.EvidencePath = new(evidencePath)
	rec.Detail = fmt.Sprintf("%s %s Case completed with a distinct Outcome on %s", Capability, caseID, Surface)
	return rec
}

// semanticIdentity returns the session_id and evidence_path semanticRecord
// assigns for one Surface-case tuple, factored out so SetSemanticDeclaredGap
// can restore exactly what semanticRecord would have assigned.
func semanticIdentity(Surface Surface, caseID Case) (sessionID, evidencePath string) {
	switch {
	case caseID == CaseRuntimeRefusal, caseID == CaseNonRetryableRefusal:
		sessionID = FixtureSession(Surface, "runtime-refusal")
	case caseID == CaseHumanInput && Surface == SurfaceProtocol:
		sessionID = FixtureSession(Surface, "permission")
	case caseID == CaseHumanInput:
		sessionID = FixtureSession(Surface, "human-input")
	default:
		sessionID = FixtureSession(Surface, string(caseID))
	}
	return sessionID, SemanticEvidencePath(Surface)
}

// SemanticEvidencePath returns the per-surface evidence-path convention a
// semantic probe record carries: which field of that surface's output a
// recognized terminal is read from.
func SemanticEvidencePath(Surface Surface) string {
	switch Surface {
	case SurfaceProtocol:
		return "/turn/stop_reason"
	case SurfaceNativeJSON:
		return "/response/terminal"
	default:
		return "/stream/terminal"
	}
}

// BaselineVerdictFor returns the Outcome a derived baseline Record carries for
// its Grade: not_observed derives not_observed, every other Grade derives pass.
func BaselineVerdictFor(classification Grade) Outcome {
	if classification == GradeNotObserved {
		return OutcomeNotObserved
	}
	return OutcomePass
}

// outcomeFailureRank ranks the not_observed failure outcomes
// DeriveBaselineOutcome chooses among, highest first.
var outcomeFailureRank = map[Outcome]int{
	OutcomeRuntimeFailed:          3,
	OutcomeFixtureInductionFailed: 2,
	OutcomePrerequisiteFailed:     1,
	OutcomeNotObserved:            0,
}

// DeriveBaselineOutcome derives a baseline Record's Outcome from its Grade and
// contributing Outcomes: a Grade other than not_observed derives pass; a
// not_observed Grade derives the highest-ranked contributing failure, and an
// empty contributing derives not_observed.
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

func (f *Fixture) addBaselines() {
	for _, Surface := range f.measured() {
		for _, Capability := range comparisonCapabilities {
			rec := f.base()
			rec.Scenario = ScenarioSurfaceBaseline
			rec.Surface = Surface
			rec.Capability = Capability
			rec.Source = SourceComparison
			rec.InputID = InputBaseline
			rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", Surface, Capability))
			rec.Detail = fmt.Sprintf("derived %s Grade for %s", Capability, Surface)
			switch Capability {
			case CapabilityTurnDisposition, CapabilityRetryClassification:
				var classes []Grade
				for _, caseID := range CapabilityCases[Capability] {
					rec := f.FindFirst(MatchSemantic(Surface, Capability, caseID))
					classes = append(classes, BaselineClassification(rec.Grade, rec.Detail))
				}
				rec.Grade = DeriveBaselineGrade(classes)
			case CapabilityTokenCeiling:
				rec.Grade = GradeUsable
			case CapabilitySessionContinuation:
				rec.Grade = GradeUsable
			}
			rec.Outcome = BaselineVerdictFor(rec.Grade)
			f.Add(rec)
		}
	}
}

// addTokenInventories adds each measured Surface's token-bearing paths. A
// Surface declared absent contributes no token record.
func (f *Fixture) addTokenInventories() {
	for _, Surface := range f.measured() {
		switch Surface {
		case SurfaceProtocol:
			f.Add(f.tokenRecord(SurfaceProtocol, "session/prompt/result/usage",
				SourceProtocolStable, GradeUsable,
				FixtureSession(SurfaceProtocol, "success"),
				"standard usage member on the final prompt result"))
			f.Add(f.tokenRecord(SurfaceProtocol, "session/update/vendor_usage",
				SourceProtocolExtension, GradeCorroborationOnly,
				FixtureSession(SurfaceProtocol, "runtime-failure"),
				"vendor extension reports totals outside the shared contract"))
		case SurfaceNativeJSON:
			f.Add(f.tokenRecord(SurfaceNativeJSON, "/response/stats",
				SourceNativeStructured, GradeUsable,
				FixtureSession(SurfaceNativeJSON, "success"),
				"structured response carries the usage stats member"))
		case SurfaceNativeStreamJSON:
			f.Add(f.tokenRecord(SurfaceNativeStreamJSON, "/stream/final/usage",
				SourceNativeStructured, GradeUsable,
				FixtureSession(SurfaceNativeStreamJSON, "success"),
				"final stream event carries the usage member"))
		}
	}
}

func (f *Fixture) tokenRecord(Surface Surface, path string, Source Source, classification Grade, SessionID, Detail string) Record {
	rec := f.base()
	rec.Scenario = ScenarioTokenSource
	rec.Surface = Surface
	rec.Capability = CapabilityTokenCeiling
	rec.Source = Source
	rec.Grade = classification
	rec.Outcome = OutcomePass
	rec.InputID = InputTokenInventory
	rec.EvidencePath = new(path)
	rec.SessionID = new(SessionID)
	rec.Detail = Detail
	if Surface == SurfaceProtocol {
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
	rec.Scenario = ScenarioPermissionRequest
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProtocolStable
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputPermissionProbe
	rec.EvidencePath = new("session/request_permission")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "permission"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "request answered with a refusing option and no request left pending"
	f.Add(rec)
}

func (f *Fixture) addToolServer() {
	rec := f.base()
	rec.Scenario = ScenarioToolServer
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityToolServerDelivery
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputMCPProbe
	rec.EvidencePath = new("mcp_server.receipt")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "mcp"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "test server received the generated nonce and the turn consumed it"
	f.Add(rec)
}

func (f *Fixture) addContinuation() {
	for _, Surface := range f.measured() {
		seedSession := FixtureSession(Surface, "seed")

		seed := f.base()
		seed.Scenario = ScenarioContinuation
		seed.Surface = Surface
		seed.Capability = CapabilitySessionContinuation
		seed.InputID = InputContinuationSeed
		seed.Outcome = OutcomePass
		seed.Grade = GradeUsable
		seed.Source = SourceNativeStructured
		if Surface == SurfaceProtocol {
			seed.Source = SourceProtocolStable
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
		recall.Scenario = ScenarioContinuation
		recall.Surface = Surface
		recall.Capability = CapabilitySessionContinuation
		recall.InputID = InputContinuationRecall
		recall.Outcome = OutcomePass
		recall.Grade = GradeUsable
		recall.Source = seed.Source
		recall.EvidencePath = new("/continuation/recall")
		recall.SessionID = new(seedSession)
		recall.PriorSessionID = new(seedSession)
		recall.Detail = RecallConfirmedSameSession
		if Surface == SurfaceProtocol {
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
	rec.Scenario = ScenarioEndToEnd
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityTurnDisposition
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputE2E
	rec.EvidencePath = new("/run_history/status")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "e2e"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "one succeeded history row and the issue reached its handoff state"
	f.Add(rec)
}

func (f *Fixture) addProcessCleanup() {
	rec := f.base()
	rec.Scenario = ScenarioProcessCleanup
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityProcessCleanup
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputCleanup
	rec.EvidencePath = new("process_group.liveness")
	rec.Detail = "checked_groups=9"
	f.Add(rec)
}

// notObservedDetail renders the not-observed label for every row outside those
// with their own closed detail vocabulary (semantic probes and continuation
// recalls).
func notObservedDetail(class RowClass) string {
	return rowLabel(class) + " was not observed"
}

func (f *Fixture) addWorkspaceSecurityNotObserved() {
	rec := f.base()
	rec.Scenario = ScenarioWorkspaceSecurity
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityWorkspaceSecurity
	rec.Source = SourceProcessObservation
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputSecurity
	rec.EvidencePath = new("/workspace/settings")
	rec.Detail = notObservedDetail(RowWorkspaceSecurity)
	f.Add(rec)
}

func (f *Fixture) addPolicyPreconditionNotObserved() {
	rec := f.base()
	rec.Scenario = ScenarioPolicyPrecondition
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProcessObservation
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputPolicyControl
	rec.EvidencePath = new("policy.deny_marker")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "policy"))
	rec.AgentVersion = new(FixtureAgentVer)
	rec.Detail = notObservedDetail(RowPolicyPrecondition)
	f.Add(rec)
}

// addSemanticProbesNotObserved adds all not-observed semantic records, built
// from semanticRecord so every field this row class leaves unchanged (source,
// input_id, agent fields, protocol version) tracks the qualified variant.
func (f *Fixture) addSemanticProbesNotObserved() {
	for _, Surface := range f.measured() {
		for _, Capability := range []Capability{CapabilityTurnDisposition, CapabilityRetryClassification} {
			for _, caseID := range CapabilityCases[Capability] {
				rec := f.semanticRecord(Surface, Capability, caseID)
				rec.Grade = GradeNotObserved
				rec.Outcome = OutcomeNotObserved
				rec.SessionID = nil
				rec.EvidencePath = nil
				rec.Detail = fmt.Sprintf("%s %s Case was not observed on %s", Capability, caseID, Surface)
				f.Add(rec)
			}
		}
	}
}

// addBaselinesNotObserved adds the not-observed per-Surface Capability
// summaries. The detail keeps the qualified variant's wording: it describes the
// derivation, not an outcome.
func (f *Fixture) addBaselinesNotObserved() {
	for _, Surface := range f.measured() {
		for _, Capability := range comparisonCapabilities {
			rec := f.base()
			rec.Scenario = ScenarioSurfaceBaseline
			rec.Surface = Surface
			rec.Capability = Capability
			rec.Source = SourceComparison
			rec.InputID = InputBaseline
			rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", Surface, Capability))
			rec.Detail = fmt.Sprintf("derived %s Grade for %s", Capability, Surface)
			rec.Grade = GradeNotObserved
			rec.Outcome = OutcomeNotObserved
			f.Add(rec)
		}
	}
}

// addTokenInventoriesNotObserved adds one not-observed token sentinel per
// measured Surface: a not-observed fixture never fabricates a token-bearing
// path.
func (f *Fixture) addTokenInventoriesNotObserved() {
	for _, Surface := range f.measured() {
		rec := f.base()
		rec.Scenario = ScenarioTokenSource
		rec.Surface = Surface
		rec.Capability = CapabilityTokenCeiling
		rec.Source = SourceNone
		rec.Grade = GradeNotObserved
		rec.Outcome = OutcomeNotObserved
		rec.InputID = InputTokenInventory
		rec.Detail = notObservedDetail(RowToken)
		f.Add(rec)
	}
}

func (f *Fixture) addPermissionNotObserved() {
	rec := f.base()
	rec.Scenario = ScenarioPermissionRequest
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProtocolStable
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputPermissionProbe
	rec.EvidencePath = new("session/request_permission")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "permission"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(RowPermission)
	f.Add(rec)
}

func (f *Fixture) addToolServerNotObserved() {
	rec := f.base()
	rec.Scenario = ScenarioToolServer
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityToolServerDelivery
	rec.Source = SourceProcessObservation
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputMCPProbe
	rec.EvidencePath = new("mcp_server.receipt")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "mcp"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(RowMCPDelivery)
	f.Add(rec)
}

// addContinuationNotObserved adds one not-observed seed and recall per measured
// Surface, the recall's prior_session_id resolving to that Surface's own seed
// session so checkContinuationRelations is satisfied from construction.
func (f *Fixture) addContinuationNotObserved() {
	for _, Surface := range f.measured() {
		seedSession := FixtureSession(Surface, "seed")

		seed := f.base()
		seed.Scenario = ScenarioContinuation
		seed.Surface = Surface
		seed.Capability = CapabilitySessionContinuation
		seed.InputID = InputContinuationSeed
		seed.Grade = GradeNotObserved
		seed.Outcome = OutcomeNotObserved
		seed.Source = SourceNativeStructured
		if Surface == SurfaceProtocol {
			seed.Source = SourceProtocolStable
			seed.AgentName = new(FixtureAgentName)
			seed.AgentVersion = new(FixtureAgentVer)
			seed.ProtocolVersion = new(1)
		} else {
			seed.AgentVersion = new(FixtureAgentVer)
		}
		seed.EvidencePath = new("/continuation/seed")
		seed.SessionID = new(seedSession)
		seed.Detail = notObservedDetail(RowContinuationSeed)
		f.Add(seed)

		recall := f.base()
		recall.Scenario = ScenarioContinuation
		recall.Surface = Surface
		recall.Capability = CapabilitySessionContinuation
		recall.InputID = InputContinuationRecall
		recall.Grade = GradeNotObserved
		recall.Outcome = OutcomeNotObserved
		recall.Source = seed.Source
		recall.EvidencePath = new("/continuation/recall")
		recall.PriorSessionID = new(seedSession)
		recall.Detail = RecallUnobservedActual
		if Surface == SurfaceProtocol {
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
	rec.Scenario = ScenarioEndToEnd
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityTurnDisposition
	rec.Source = SourceProcessObservation
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputE2E
	rec.EvidencePath = new("/run_history/status")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "e2e"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = notObservedDetail(RowEndToEnd)
	f.Add(rec)
}

func (f *Fixture) addProcessCleanupNotObserved() {
	rec := f.base()
	rec.Scenario = ScenarioProcessCleanup
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityProcessCleanup
	rec.Source = SourceProcessObservation
	rec.Grade = GradeNotObserved
	rec.Outcome = OutcomeNotObserved
	rec.InputID = InputCleanup
	rec.EvidencePath = new("process_group.liveness")
	rec.Detail = notObservedDetail(RowProcessCleanup)
	f.Add(rec)
}

// MatchSemantic matches one semantic tuple.
func MatchSemantic(Surface Surface, Capability Capability, caseID Case) func(*Record) bool {
	return func(rec *Record) bool {
		if rec.Scenario != ScenarioSemanticProbe || rec.Surface != Surface ||
			rec.Capability != Capability || rec.SemanticCase == nil {
			return false
		}
		return *rec.SemanticCase == caseID
	}
}

// MatchBaseline matches one Surface-Capability baseline Record.
func MatchBaseline(Surface Surface, Capability Capability) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioSurfaceBaseline &&
			rec.Surface == Surface && rec.Capability == Capability
	}
}

// MatchContinuation matches one Surface's seed or recall Record.
func MatchContinuation(Surface Surface, InputID InputID) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioContinuation && rec.Surface == Surface && rec.InputID == InputID
	}
}

// WriteEvidenceFile serializes records as UTF-8 JSON Lines under the test's
// temporary directory and returns the file path.
func WriteEvidenceFile(T *testing.T, records []Record) string {
	T.Helper()
	dir := filepath.Join(T.TempDir(), "qualification")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		T.Fatalf("create evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	var lines []string
	for _, rec := range records {
		line, err := MarshalRecord(rec)
		if err != nil {
			T.Fatalf("marshal evidence Record %d: %v", rec.Sequence, err)
		}
		lines = append(lines, string(line))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		T.Fatalf("write evidence file: %v", err)
	}
	return path
}

// WriteFinalEvidenceFile serializes a non-final set plus the terminal aggregate
// Record and returns the file path.
func WriteFinalEvidenceFile(T *testing.T, records []Record, classification Grade) string {
	T.Helper()
	complete := append(slices.Clone(records), aggregateFixtureRecord(classification))
	for i := range complete {
		complete[i].Sequence = i + 1
	}
	return WriteEvidenceFile(T, complete)
}

// RequireObservationVerdict validates a first-pass file and fails T unless it
// validates with the wanted Verdict.
func RequireObservationVerdict(T *testing.T, path string, wantVerdict Verdict) {
	T.Helper()
	v, err := ValidateObservations(path)
	if err != nil {
		T.Errorf("ValidateObservations(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		T.Errorf("ValidateObservations(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// IdentityFixtureRecord builds one runtime-identity record for one actual
// protocol session.
func IdentityFixtureRecord(SessionID string) Record {
	return Record{
		SchemaVersion:   1,
		Sequence:        0,
		ObservedAt:      FixtureTime,
		Scenario:        ScenarioRuntimeIdentity,
		Surface:         SurfaceProtocol,
		Capability:      CapabilityRuntimeIdentity,
		Source:          SourceProtocolStable,
		Grade:           GradeUsable,
		Outcome:         OutcomePass,
		InputID:         InputIdentity,
		EvidencePath:    new("/handshake/agent_info"),
		SessionID:       new(SessionID),
		AgentName:       new(FixtureAgentName),
		AgentVersion:    new(FixtureAgentVer),
		ProtocolVersion: new(1),
		Detail:          "handshake reported agent name and version",
	}
}

// identityFixtureRecordNotObserved builds one not-observed runtime-identity
// record: the id is inferred from another record's reference, but the handshake
// that would confirm the agent's name and version was not observed.
func identityFixtureRecordNotObserved(SessionID string) Record {
	return Record{
		SchemaVersion:   1,
		Sequence:        0,
		ObservedAt:      FixtureTime,
		Scenario:        ScenarioRuntimeIdentity,
		Surface:         SurfaceProtocol,
		Capability:      CapabilityRuntimeIdentity,
		Source:          SourceProtocolStable,
		Grade:           GradeNotObserved,
		Outcome:         OutcomeNotObserved,
		InputID:         InputIdentity,
		EvidencePath:    new("/handshake/agent_info"),
		SessionID:       new(SessionID),
		AgentName:       new(FixtureAgentName),
		AgentVersion:    new(FixtureAgentVer),
		ProtocolVersion: new(1),
		Detail:          notObservedDetail(RowRuntimeIdentity),
	}
}

// aggregateFixtureRecord builds the terminal qualification Record.
func aggregateFixtureRecord(classification Grade) Record {
	return Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    FixtureTime,
		Scenario:      ScenarioQualification,
		Surface:       SurfaceAggregate,
		Capability:    CapabilityEligibility,
		Source:        SourceComparison,
		Grade:         classification,
		Outcome:       OutcomePass,
		InputID:       InputAggregate,
		EvidencePath:  new(EvidencePathQualificationVerdict),
		Detail:        "aggregate qualification Verdict recomputed from the closed non-final evidence set",
	}
}

// RequireFinalVerdict validates a final-pass file and fails T unless it
// validates with the wanted Verdict.
func RequireFinalVerdict(T *testing.T, path string, wantVerdict Verdict) {
	T.Helper()
	v, err := ValidateEvidence(path)
	if err != nil {
		T.Errorf("ValidateEvidence(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		T.Errorf("ValidateEvidence(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// SetTokenSentinel replaces one Surface's token inventory with the sentinel
// Record: a zero-Source success when failed is false, a failed inventory
// otherwise, rewriting the token baseline to the derived Grade.
func (f *Fixture) SetTokenSentinel(Surface Surface, failed bool) {
	f.RemoveAll(matchTokenSurface(Surface))
	sentinel := f.base()
	sentinel.Scenario = ScenarioTokenSource
	sentinel.Surface = Surface
	sentinel.Capability = CapabilityTokenCeiling
	sentinel.Source = SourceNone
	sentinel.Outcome = OutcomePass
	sentinel.InputID = InputTokenInventory
	if failed {
		sentinel.Grade = GradeNotObserved
		sentinel.Outcome = OutcomeRuntimeFailed
		sentinel.Detail = "token inventory collection failed"
	} else {
		sentinel.Grade = GradeGap
		sentinel.Detail = "inventory completed with no token-bearing path"
	}
	f.Add(sentinel)

	baseline := f.FindFirst(MatchBaseline(Surface, CapabilityTokenCeiling))
	if baseline != nil {
		baseline.Grade = GradeGap
		if failed {
			baseline.Grade = GradeNotObserved
		}
		baseline.Outcome = DeriveBaselineOutcome(baseline.Grade, []Outcome{sentinel.Outcome})
	}
}

func matchTokenSurface(Surface Surface) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioTokenSource && rec.Surface == Surface
	}
}

func matchToolServer() func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioToolServer && rec.Surface == SurfaceProtocol &&
			rec.Capability == CapabilityToolServerDelivery
	}
}

// matchPermission matches the single protocol permission Record. It does not
// match addPolicyPrecondition's record, which classifies as a
// policy-precondition row rather than a graded permission-handling row.
func matchPermission() func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioPermissionRequest && rec.Surface == SurfaceProtocol &&
			rec.Capability == CapabilityPermissionHandling
	}
}

// boundDetail truncates detail to DetailBound Unicode code points, so a
// caller-supplied description cannot make a rewritten Record fail the decoder's
// length check.
func boundDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= DetailBound {
		return detail
	}
	return string(runes[:DetailBound])
}

// Observation is one live-collected grade, outcome, detail, and identifier a
// setter writes onto a matching record. It never derives Outcome from Grade;
// the caller states both, and the setter admits only the pairs the admission
// table names.
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

// TokenObservation is one resolved token-bearing path SetTokenInventory writes
// as its own record.
type TokenObservation struct {
	EvidencePath string
	Kind         string // "spend" or "occupancy"
}

// ExtensionReading is what one collection established about the token-bearing
// extension on the protocol's extension point: whether a source was read, and
// whether the rules let it stand as the figure a budget is kept in. The two
// travel together because a source present but not admitted is a state of its
// own.
type ExtensionReading struct {
	Source   ExtensionSource
	Admitted bool
}

// writeOnto states the reading on one record. A nil reading writes nothing.
func (r *ExtensionReading) writeOnto(rec *Record) {
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

// checkObservationAdmitted rejects a zero-value Outcome and any grade-outcome
// pair outside observationAdmission.
func checkObservationAdmitted(obs Observation) error {
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

// applyObservation writes obs's grade, outcome, and bounded detail onto rec,
// rewrites its session_id (null when obs.SessionID is empty), and rewrites its
// evidence_path only when obs.EvidencePath is non-empty.
func applyObservation(rec *Record, obs Observation) {
	rec.Grade = obs.Grade
	rec.Outcome = obs.Outcome
	rec.Detail = boundDetail(obs.Detail)
	if obs.SessionID == "" {
		rec.SessionID = nil
	} else {
		rec.SessionID = new(obs.SessionID)
	}
	if obs.EvidencePath != "" {
		rec.EvidencePath = new(obs.EvidencePath)
	}
}

// requireObservedSession rejects obs with an empty SessionID, for the row
// classes checkSessionRelation requires one on.
func requireObservedSession(rowLabel string, obs Observation) error {
	if obs.SessionID == "" {
		return fmt.Errorf("%s observation requires a non-empty session id", rowLabel)
	}
	return nil
}

// SetSemanticObservation writes obs onto the matching semantic Case record and
// rewrites the owning Capability's baseline. It writes nothing and returns the
// admission error when obs carries a grade-outcome pair outside the table.
func (f *Fixture) SetSemanticObservation(surface Surface, capability Capability, caseID Case, obs Observation) error {
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		return fmt.Errorf("no semantic record for surface %s capability %s case %s", surface, capability, caseID)
	}
	applyObservation(rec, obs)
	f.UpdateSemanticBaseline(surface, capability)
	return nil
}

// SetSemanticNotInducible writes the closed not_inducible shape onto the
// matching semantic Case record (GradeNotInducible, OutcomeNotInducible, reason
// as detail, null session_id and evidence_path) and rewrites the baseline.
// reason states why the case was not induced here, or NotInducibleDetail where
// no inducer exists.
func (f *Fixture) SetSemanticNotInducible(surface Surface, capability Capability, caseID Case, reason string) {
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		return
	}
	rec.Grade = GradeNotInducible
	rec.Outcome = OutcomeNotInducible
	rec.Detail = reason
	rec.SessionID = nil
	rec.EvidencePath = nil
	f.UpdateSemanticBaseline(surface, capability)
}

// SetSemanticLiveDeclaredGap writes the live declared-gap shape for caseID (and
// its DeclaredGapPeers partner, if any) onto every declarable measured surface,
// using that surface's observed session identifier. It returns an error naming
// the missing surface, writing nothing, when observed lacks one.
func (f *Fixture) SetSemanticLiveDeclaredGap(capability Capability, caseID Case, reason string, observed map[Surface]string) error {
	declarable := intersectSurfaces(DeclarableSurfaces, f.measured())
	for _, surface := range declarable {
		if observed[surface] == "" {
			return fmt.Errorf("no observed session identifier for surface %s", surface)
		}
	}
	f.setSemanticLiveDeclaredGapOne(capability, caseID, reason, observed, declarable)
	if peer, ok := DeclaredGapPeers[caseID]; ok {
		f.setSemanticLiveDeclaredGapOne(capabilityOwning(peer), peer, reason, observed, declarable)
	}
	return nil
}

// setSemanticLiveDeclaredGapOne rewrites one case's declarable-surface records
// and baselines from observed, without the DeclaredGapPeers closure.
func (f *Fixture) setSemanticLiveDeclaredGapOne(capability Capability, caseID Case, reason string, observed map[Surface]string, declarable []Surface) {
	for _, surface := range declarable {
		rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
		if rec == nil {
			continue
		}
		rec.Outcome = OutcomeNotProducible
		rec.Grade = GradeDeclaredGap
		rec.Detail = boundDetail(reason)
		sessionID := observed[surface]
		rec.SessionID = new(sessionID)
		rec.EvidencePath = new(SemanticEvidencePath(surface))
	}
	f.recordDeclaration(capability, caseID, reason)
	for _, surface := range declarable {
		f.UpdateSemanticBaseline(surface, capability)
	}
}

// SetToolServerDelivery writes obs onto the protocol tool-server delivery
// Record. It returns the admission error for a pair outside the table, and an
// error for an empty obs.SessionID, which RowMCPDelivery requires non-null.
func (f *Fixture) SetToolServerDelivery(obs Observation) error {
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(rowLabel(RowMCPDelivery), obs); err != nil {
		return err
	}
	rec := f.FindFirst(matchToolServer())
	if rec == nil {
		return errors.New("no tool server delivery record")
	}
	applyObservation(rec, obs)
	return nil
}

// SetPermissionHandling writes obs onto the protocol permission-handling
// Record. It returns the admission error for a pair outside the table, and an
// error for an empty obs.SessionID, which RowPermission requires non-null.
func (f *Fixture) SetPermissionHandling(obs Observation) error {
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(rowLabel(RowPermission), obs); err != nil {
		return err
	}
	rec := f.FindFirst(matchPermission())
	if rec == nil {
		return errors.New("no permission handling record")
	}
	applyObservation(rec, obs)
	return nil
}

// SetPolicyPrecondition writes obs onto the aggregate policy precondition
// Record. It returns the admission error for a pair outside the table, and an
// error for an empty obs.SessionID, which RowPolicyPrecondition requires
// non-null.
func (f *Fixture) SetPolicyPrecondition(obs Observation) error {
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}
	if err := requireObservedSession(rowLabel(RowPolicyPrecondition), obs); err != nil {
		return err
	}
	rec := f.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioPolicyPrecondition && rec.Surface == SurfaceAggregate
	})
	if rec == nil {
		return errors.New("no policy precondition record")
	}
	applyObservation(rec, obs)
	return nil
}

// SetEndToEnd writes obs onto the isolated end-to-end Record. It returns the
// admission error for a pair outside the table, and an error for an empty
// obs.SessionID on an observation that reports a run: a not_observed launch
// never started and has none, and an identifier invented here would read as one
// the run observed.
func (f *Fixture) SetEndToEnd(obs Observation) error {
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}
	if obs.Grade != GradeNotObserved {
		if err := requireObservedSession(rowLabel(RowEndToEnd), obs); err != nil {
			return err
		}
	}
	rec := f.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioEndToEnd && rec.Surface == SurfaceProtocol
	})
	if rec == nil {
		return errors.New("no end-to-end record")
	}
	applyObservation(rec, obs)
	return nil
}

// SetWorkspaceSecurity writes obs onto the aggregate workspace-security Record.
// It carries no identifier, so it cannot violate the session_id rule and takes
// no error return.
func (f *Fixture) SetWorkspaceSecurity(obs Observation) {
	rec := f.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioWorkspaceSecurity && rec.Surface == SurfaceAggregate
	})
	if rec == nil {
		return
	}
	applyObservation(rec, Observation{Grade: obs.Grade, Outcome: obs.Outcome, Detail: obs.Detail})
}

// SetProcessCleanup writes obs onto the aggregate process-cleanup Record. It
// carries no identifier, so it cannot violate the session_id rule and takes no
// error return.
func (f *Fixture) SetProcessCleanup(obs Observation) {
	rec := f.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioProcessCleanup && rec.Surface == SurfaceAggregate
	})
	if rec == nil {
		return
	}
	applyObservation(rec, Observation{Grade: obs.Grade, Outcome: obs.Outcome, Detail: obs.Detail})
}

// SetSessionContinuationObserved writes surface's seed and recall records and
// rewrites its continuation baseline, writing seed.SessionID into the recall's
// prior_session_id. An empty seed.SessionID writes a null session_id and
// prior_session_id: a value invented here would be indistinguishable from one a
// runtime reported.
func (f *Fixture) SetSessionContinuationObserved(surface Surface, seed, recall Observation) error {
	if err := checkObservationAdmitted(seed); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := checkObservationAdmitted(recall); err != nil {
		return fmt.Errorf("recall: %w", err)
	}
	if err := checkRecallObservation(seed.SessionID, recall); err != nil {
		return err
	}

	seedRec := f.FindFirst(MatchContinuation(surface, InputContinuationSeed))
	if seedRec == nil {
		return fmt.Errorf("no continuation seed record for surface %s", surface)
	}
	applyObservation(seedRec, seed)

	recallRec := f.FindFirst(MatchContinuation(surface, InputContinuationRecall))
	if recallRec == nil {
		return fmt.Errorf("no continuation recall record for surface %s", surface)
	}
	applyObservation(recallRec, recall)
	recallRec.PriorSessionID = nil
	if seed.SessionID != "" {
		recallRec.PriorSessionID = new(seed.SessionID)
	}
	recallRec.Detail = recall.Detail

	if baseline := f.FindFirst(MatchBaseline(surface, CapabilitySessionContinuation)); baseline != nil {
		baseline.Grade = recall.Grade
		baseline.Detail = boundDetail(recall.Detail)
		baseline.Outcome = DeriveBaselineOutcome(recall.Grade, []Outcome{recall.Outcome})
	}
	return nil
}

// checkRecallObservation mirrors checkRecallRecord's rules against an
// about-to-be-written recall observation, so a rejection is caught before the
// next paid turn rather than at validation time.
func checkRecallObservation(seedSessionID string, recall Observation) error {
	switch recall.Detail {
	case RecallConfirmedSameSession:
		// The proof is the answer carrying the seed's nonce, so a surface that
		// named no session on either turn still matches: two absent ids agree.
		if recall.SessionID != seedSessionID {
			return errors.New("confirmed_same_session requires the actual session id to be the seed's own")
		}
		if recall.Grade != GradeUsable {
			return fmt.Errorf("confirmed_same_session requires classification usable, got %s", recall.Grade)
		}
	case RecallFreshFallback:
		if seedSessionID == "" {
			// A run that never learned the seed's session cannot have seen the
			// runtime open a different one.
			return errors.New("fresh_session_fallback requires the seed's own observed session id")
		}
		if recall.SessionID == "" || recall.SessionID == seedSessionID {
			return errors.New("fresh_session_fallback requires a non-empty actual session id distinct from the seed's own")
		}
		if recall.Grade != GradeGap {
			return fmt.Errorf("fresh_session_fallback requires classification gap, got %s", recall.Grade)
		}
	case RecallSameSessionWithoutRecall:
		if recall.SessionID == "" || recall.SessionID != seedSessionID {
			return errors.New("same_session_without_recall requires a non-empty actual session id equal to the seed's own")
		}
		if recall.Grade != GradeGap {
			return fmt.Errorf("same_session_without_recall requires classification gap, got %s", recall.Grade)
		}
	case RecallDeclined:
		if recall.SessionID == "" || recall.SessionID != seedSessionID {
			return errors.New("same_session_answer_declined requires a non-empty actual session id equal to the seed's own")
		}
		if recall.Grade != GradeNotObserved {
			return fmt.Errorf("same_session_answer_declined requires classification not_observed, got %s", recall.Grade)
		}
		if recall.Outcome != OutcomeFixtureInductionFailed {
			return fmt.Errorf("same_session_answer_declined requires verdict fixture_induction_failed, got %s", recall.Outcome)
		}
	case RecallUnobservedActual:
		if recall.SessionID != "" {
			return errors.New("unobserved_actual_session requires an empty session id")
		}
		if recall.Grade != GradeNotObserved {
			return fmt.Errorf("unobserved_actual_session requires classification not_observed, got %s", recall.Grade)
		}
	case RecallPreconditionUnmet:
		if recall.SessionID != "" {
			return errors.New("recall_precondition_unmet requires an empty session id")
		}
		if recall.Grade != GradeNotObserved {
			return fmt.Errorf("recall_precondition_unmet requires classification not_observed, got %s", recall.Grade)
		}
		if recall.Outcome != OutcomePrerequisiteFailed {
			return fmt.Errorf("recall_precondition_unmet requires verdict prerequisite_failed, got %s", recall.Outcome)
		}
	default:
		return fmt.Errorf("recall detail %q is outside the closed set", recall.Detail)
	}
	return nil
}

// SetTokenInventory removes surface's existing token records and writes from
// paths and inventory: an empty paths writes a single sentinel; a non-empty
// paths writes one record per entry carrying sessionID. It rewrites the token
// baseline. It returns an error for an inventory pair outside the admitted set,
// a not_observed inventory with a non-empty paths, and a non-empty paths with
// an empty sessionID.
//
// A non-nil extension states what this collection read of the protocol's
// extension point and belongs to the protocol surface alone; naming one for a
// native surface is an error.
func (f *Fixture) SetTokenInventory(surface Surface, sessionID string, paths []TokenObservation, inventory Observation, extension *ExtensionReading) error {
	admitted := (inventory.Grade == GradeGap && inventory.Outcome == OutcomePass) ||
		(inventory.Grade == GradeNotObserved && (inventory.Outcome == OutcomeFixtureInductionFailed || inventory.Outcome == OutcomeRuntimeFailed))
	if !admitted {
		return fmt.Errorf("token inventory grade %s outcome %s is outside the admitted pairs", inventory.Grade, inventory.Outcome)
	}
	if extension != nil {
		if surface != SurfaceProtocol {
			return fmt.Errorf("surface %s cannot state a reading of the protocol extension point", surface)
		}
		if !slices.Contains(ExtensionSources, extension.Source) {
			return fmt.Errorf("extension source %q is outside the closed set", extension.Source)
		}
		if extension.Admitted && extension.Source != ExtensionSourcePresent {
			return fmt.Errorf("extension source %s cannot be admitted to a budget", extension.Source)
		}
	}
	if inventory.Grade == GradeNotObserved && len(paths) > 0 {
		return errors.New("a not_observed inventory must not carry any resolved token path")
	}
	if len(paths) > 0 && sessionID == "" {
		return errors.New("a resolved token path requires the session id that emitted it")
	}

	f.RemoveAll(matchTokenSurface(surface))

	if len(paths) == 0 {
		sentinel := f.base()
		sentinel.Scenario = ScenarioTokenSource
		sentinel.Surface = surface
		sentinel.Capability = CapabilityTokenCeiling
		sentinel.Source = SourceNone
		sentinel.Grade = inventory.Grade
		sentinel.Outcome = inventory.Outcome
		sentinel.InputID = InputTokenInventory
		sentinel.Detail = boundDetail(inventory.Detail)
		extension.writeOnto(&sentinel)
		f.Add(sentinel)
	} else {
		source := SourceNativeStructured
		if surface == SurfaceProtocol {
			source = SourceProtocolStable
		}
		for _, path := range paths {
			rec := f.base()
			rec.Scenario = ScenarioTokenSource
			rec.Surface = surface
			rec.Capability = CapabilityTokenCeiling
			rec.Source = source
			rec.Outcome = OutcomePass
			rec.InputID = InputTokenInventory
			rec.EvidencePath = new(path.EvidencePath)
			rec.SessionID = new(sessionID)
			rec.Detail = boundDetail(inventory.Detail)
			if path.Kind == "spend" {
				rec.Grade = GradeUsable
			} else {
				rec.Grade = GradeCorroborationOnly
			}
			extension.writeOnto(&rec)
			f.Add(rec)
		}
	}

	if baseline := f.FindFirst(MatchBaseline(surface, CapabilityTokenCeiling)); baseline != nil {
		var records []*Record
		for i := range f.Records {
			if matchTokenSurface(surface)(&f.Records[i]) {
				records = append(records, &f.Records[i])
			}
		}
		baseline.Grade = tokenBaselineGrade(records)
		var outcomes []Outcome
		for _, rec := range records {
			outcomes = append(outcomes, rec.Outcome)
		}
		baseline.Outcome = DeriveBaselineOutcome(baseline.Grade, outcomes)
	}
	return nil
}

// SetRuntimeIdentity captures what each session's handshake reported, writing
// agent_name, agent_version, and protocol_version on every protocol record
// composed from the identity of the session it names, clearing agent_version
// elsewhere, and supplying the same readings to every identity record Finalize
// later builds. A record naming no session carries no agent fields.
//
// It rejects a non-empty obs.SessionID (Finalize supplies each record's session
// id), a usable observation with no identity, an identity with an empty name or
// version, identities alongside a not-observed observation, an
// OutcomeNotObserved observation, and a zero protocolVersion.
func (f *Fixture) SetRuntimeIdentity(obs Observation, identities map[string]SessionIdentity, protocolVersion int) error {
	if obs.SessionID != "" {
		return errors.New("runtime identity observation must carry an empty session id")
	}
	if obs.Grade == GradeUsable && len(identities) == 0 {
		return errors.New("a usable runtime identity observation requires at least one session's handshake")
	}
	if obs.Grade == GradeNotObserved && len(identities) > 0 {
		return errors.New("a not_observed runtime identity observation cannot carry a session handshake")
	}
	for sessionID, identity := range identities {
		if identity.Name == "" || identity.Version == "" {
			return fmt.Errorf("the handshake for session %s requires a non-empty name and version", sessionID)
		}
	}
	if obs.Outcome == OutcomeNotObserved {
		return errors.New("runtime identity observation must not carry outcome not_observed")
	}
	if protocolVersion == 0 {
		return errors.New("runtime identity observation requires a non-zero protocol_version")
	}
	if err := checkObservationAdmitted(obs); err != nil {
		return err
	}

	f.identitySet = true
	f.identityObs = obs
	f.identities = maps.Clone(identities)
	f.identityProtocolVersion = protocolVersion

	for i := range f.Records {
		rec := &f.Records[i]
		if rec.Surface != SurfaceProtocol {
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
func (f *Fixture) sessionIdentity(sessionID *string) (SessionIdentity, bool) {
	if sessionID == nil {
		return SessionIdentity{}, false
	}
	identity, named := f.identities[*sessionID]
	return identity, named
}

// SetSessionContinuation rewrites surface's session-continuation baseline,
// recall, and seed Records to grade, following a live replay. The recall
// carries the closed detail token checkRecallRecord requires for grade, not
// free text. The seed reads usable whenever a turn completed (usable or gap)
// and not_observed only when nothing was observed. Any grade outside usable,
// gap, and not_observed leaves every record unchanged.
func (f *Fixture) SetSessionContinuation(surface Surface, grade Grade, detail string) {
	if grade != GradeUsable && grade != GradeGap && grade != GradeNotObserved {
		return
	}
	outcome := BaselineVerdictFor(grade)

	if baseline := f.FindFirst(MatchBaseline(surface, CapabilitySessionContinuation)); baseline != nil {
		baseline.Grade = grade
		baseline.Detail = boundDetail(detail)
		baseline.Outcome = outcome
	}

	if recall := f.FindFirst(MatchContinuation(surface, InputContinuationRecall)); recall != nil {
		recall.Grade = grade
		recall.Outcome = outcome
		switch grade {
		case GradeUsable:
			recall.Detail = RecallConfirmedSameSession
			recall.SessionID = new(*recall.PriorSessionID)
		case GradeGap:
			recall.Detail = RecallFreshFallback
			recall.SessionID = new(FixtureSession(surface, "recall-fallback"))
		case GradeNotObserved:
			recall.Detail = RecallUnobservedActual
			recall.SessionID = nil
		}
	}

	if seed := f.FindFirst(MatchContinuation(surface, InputContinuationSeed)); seed != nil {
		if grade == GradeNotObserved {
			seed.Grade = GradeNotObserved
			seed.Outcome = OutcomeNotObserved
			seed.Detail = notObservedDetail(RowContinuationSeed)
		} else {
			seed.Grade = GradeUsable
			seed.Outcome = OutcomePass
			seed.Detail = "seed session completed a turn that left history"
		}
	}
}

// SetTokenCorroborationOnly rewrites every non-sentinel token Record of one
// Surface to corroboration_only and the token baseline to gap: the inventory
// completed but supplied no contract-usable Source.
func (f *Fixture) SetTokenCorroborationOnly(Surface Surface) {
	for i := range f.Records {
		rec := &f.Records[i]
		if matchTokenSurface(Surface)(rec) && rec.EvidencePath != nil {
			rec.Grade = GradeCorroborationOnly
		}
	}
	if baseline := f.FindFirst(MatchBaseline(Surface, CapabilityTokenCeiling)); baseline != nil {
		baseline.Grade = GradeGap
	}
}

// SetTokenCompensated adds the protocol-surface token reading Sortie's code
// supplies outside the protocol, at path. The surface's inventory is left
// intact, so the published baseline keeps reporting what the wire carried.
func (f *Fixture) SetTokenCompensated(path string) {
	f.SetTokenCompensatedObserved(FixtureSession(SurfaceProtocol, "success"), path,
		"Sortie reads the spend from its own record of the turn")
}

// SetTokenCompensatedObserved adds the protocol-surface token reading Sortie's
// code supplied outside the protocol for sessionID, at path, accounted for by
// detail. Only a run that watched its own adapter return a figure may call it,
// since the record it writes is read as a measurement, not a fitted capability.
func (f *Fixture) SetTokenCompensatedObserved(sessionID, path, detail string) {
	rec := f.tokenRecord(SurfaceProtocol, path, SourceSortieShared, GradeUsable, sessionID, boundDetail(detail))
	f.Add(rec)
	slices.SortStableFunc(f.Records, OrderCompare)
	f.Renumber()
}

// DuplicateAfter inserts a copy of target directly behind it, keeping canonical
// ordering intact so the duplicate key check is the check that fires.
func (f *Fixture) DuplicateAfter(target *Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Insert(f.Records, i+1, *target)
			return
		}
	}
}

// AppendIdentity adds one runtime-identity record for SessionID at its
// canonical position, for controls that introduce a new actual protocol session
// id after Finalize.
func (f *Fixture) AppendIdentity(SessionID string) {
	f.Add(f.identityRecord(SessionID))
	slices.SortStableFunc(f.Records, OrderCompare)
	f.Renumber()
}

// TokenRecordCount counts records with the token-source scenario.
func TokenRecordCount(records []Record) int {
	count := 0
	for i := range records {
		if records[i].Scenario == ScenarioTokenSource {
			count++
		}
	}
	return count
}

// ProtocolSessionCount counts the distinct non-null actual protocol session ids
// a record set references.
func ProtocolSessionCount(records []Record) int {
	ids := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			ids[*rec.SessionID] = true
		}
	}
	return len(ids)
}

// ComparisonCapabilities is the set of capabilities measured in comparison
// scenarios. It is a copy of the list the validator reads, so a collector
// iterating it cannot come to measure a set the cardinality rules no longer
// enforce.
var ComparisonCapabilities = slices.Clone(comparisonCapabilities)

// measuredSurfaces returns the surfaces one offline evidence pass measures: the
// closed measurable order minus every surface the profile declares absent. The
// zero RuntimeProfile returns all three.
//
// It differs from RuntimeProfile.MeasuredSurfaces, which reads a live profile's
// entry_points: the offline validator and this package's fixtures work from a
// profile's declarations alone.
func measuredSurfaces(profile RuntimeProfile) []Surface {
	surfaces := make([]Surface, 0, len(measurableSurfaces))
	for _, surface := range measurableSurfaces {
		if _, absent := profile.AbsentSurfaceDeclared(surface); !absent {
			surfaces = append(surfaces, surface)
		}
	}
	return surfaces
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
