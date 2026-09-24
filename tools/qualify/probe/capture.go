//go:build unix

package probe

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// collectedObservations bundles every row the collection's inducers
// produced. It launches nothing and reads no environment.
type collectedObservations struct {
	semantic map[evidence.Surface]map[evidence.Case]evidence.Observation

	toolServer evidence.Observation
	permission evidence.Observation
	policy     evidence.Observation

	continuationSeed   map[evidence.Surface]evidence.Observation
	continuationRecall map[evidence.Surface]evidence.Observation

	tokenSessionID map[evidence.Surface]string
	tokenPaths     map[evidence.Surface][]evidence.TokenObservation
	tokenInventory map[evidence.Surface]evidence.Observation
	// tokenExtension is the protocol surface's extension-point reading,
	// which no native surface has.
	tokenExtension *evidence.ExtensionReading
	// tokenCompensation answers for the effective adapter alone, kept
	// apart from the inventory, which answers for the wire.
	tokenCompensation tokenCompensation

	workspaceSecurity evidence.Observation
	processCleanup    evidence.Observation
	endToEnd          evidence.Record

	identityObs             evidence.Observation
	identities              map[string]evidence.SessionIdentity
	identityProtocolVersion int
}

// endToEndObservation renders an end-to-end record as the Observation
// composeLive consumes.
func endToEndObservation(rec evidence.Record) evidence.Observation {
	obs := evidence.Observation{Grade: rec.Grade, Outcome: rec.Outcome, Detail: rec.Detail}
	if rec.SessionID != nil {
		obs.SessionID = *rec.SessionID
	}
	return obs
}

// declarableSurfaceUnproduced reports whether every declarable surface's
// case observation ran and saw nothing, excluding a launch that failed
// outright: a provider error is not the outcome the declaration names.
func declarableSurfaceUnproduced(collected collectedObservations, measured []evidence.Surface, caseID evidence.Case) bool {
	for _, surface := range evidence.DeclarableSurfaces {
		if !slices.Contains(measured, surface) {
			continue
		}
		obs, ok := collected.semantic[surface][caseID]
		if !ok || obs.SessionID == "" {
			return false
		}
		if !evidence.ObservationUnproduced(obs) {
			return false
		}
	}
	return true
}

// baseRecord returns the fixed identity fields every composed record
// shares: schema version and the collection's own observation moment.
// Sequence is assigned once the full set is known.
func baseRecord(observedAt string) evidence.Record {
	return evidence.Record{SchemaVersion: 1, ObservedAt: observedAt}
}

// notObservedSemanticRecord builds one semantic case row in its
// not-observed shape: the composition default before an observation, if
// any, is applied through evidence.ApplyObservation.
func notObservedSemanticRecord(observedAt string, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) evidence.Record {
	rec := baseRecord(observedAt)
	rec.Scenario = evidence.ScenarioSemanticProbe
	rec.Surface = surface
	rec.Capability = capability
	rec.SemanticCase = new(caseID)
	rec.InputID = evidence.CaseInputs[caseID]
	rec.Source = evidence.SourceNativeStructured
	if surface == evidence.SurfaceProtocol {
		rec.Source = evidence.SourceProtocolStable
	}
	rec.Grade = evidence.GradeNotObserved
	rec.Outcome = evidence.OutcomeNotObserved
	rec.Detail = fmt.Sprintf("%s %s case was not observed on %s", capability, caseID, surface)
	return rec
}

// composeSemanticRecord composes one semantic case row from its own
// observation, or its not-observed shape when obs is the zero value.
func composeSemanticRecord(observedAt string, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case, obs evidence.Observation) evidence.Record {
	rec := notObservedSemanticRecord(observedAt, surface, capability, caseID)
	evidence.ApplyObservation(&rec, obs)
	return rec
}

// composeNotInducibleRecord composes one semantic case row the catalog or
// the profile declares not inducible.
func composeNotInducibleRecord(observedAt string, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case, reason string) evidence.Record {
	rec := notObservedSemanticRecord(observedAt, surface, capability, caseID)
	rec.Grade = evidence.GradeNotInducible
	rec.Outcome = evidence.OutcomeNotInducible
	rec.Detail = reason
	rec.SessionID = nil
	rec.EvidencePath = nil
	return rec
}

// composeDeclaredGapRecord composes one semantic case row the operator
// declared a gap for, using the session identifier that surface's own
// induction observed, empty when none was.
func composeDeclaredGapRecord(observedAt string, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case, reason, observedSessionID string) evidence.Record {
	rec := notObservedSemanticRecord(observedAt, surface, capability, caseID)
	rec.Outcome = evidence.OutcomeNotProducible
	rec.Grade = evidence.GradeDeclaredGap
	rec.Detail = reason
	if observedSessionID != "" {
		rec.SessionID = new(observedSessionID)
	}
	rec.EvidencePath = new(evidence.SemanticEvidencePath(surface))
	return rec
}

// composeBaselineRecord derives one surface-capability baseline record
// from the semantic records it is derived from.
func composeBaselineRecord(observedAt string, surface evidence.Surface, capability evidence.Capability, semantics []evidence.Record) evidence.Record {
	rec := baseRecord(observedAt)
	rec.Scenario = evidence.ScenarioSurfaceBaseline
	rec.Surface = surface
	rec.Capability = capability
	rec.Source = evidence.SourceComparison
	rec.InputID = evidence.InputBaseline
	rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", surface, capability))
	rec.Detail = fmt.Sprintf("derived %s grade for %s", capability, surface)

	classes := make([]evidence.Grade, 0, len(semantics))
	var contributing []evidence.Outcome
	for _, semantic := range semantics {
		classes = append(classes, evidence.BaselineClassification(semantic.Grade, semantic.Detail))
		contributing = append(contributing, semantic.Outcome)
	}
	rec.Grade = evidence.DeriveBaselineGrade(classes)
	rec.Outcome = evidence.DeriveBaselineOutcome(rec.Grade, contributing)
	return rec
}

// composeLive builds the evidence one live collection publishes. Nothing
// is synthesized to satisfy the schema: an absent or failed observation
// composes to its own honest not-observed or error record.
func composeLive(p profile.RuntimeProfile, collected collectedObservations, startedAt time.Time) ([]evidence.Record, error) {
	if startedAt.IsZero() {
		return nil, errors.New("composing live evidence requires the collection's own start time")
	}
	observedAt := startedAt.UTC().Format(time.RFC3339)
	measured := p.DeclaredMeasuredSurfaces()

	notInducibleReason := map[[2]string]string{}
	for _, entry := range p.NotInducibleCases {
		notInducibleReason[[2]string{string(entry.Surface), string(entry.Case)}] = entry.Reason
	}

	deferredDeclared := map[[2]string]bool{}
	for _, d := range p.Declarations {
		if declarableSurfaceUnproduced(collected, measured, d.Case) {
			deferredDeclared[[2]string{string(d.Capability), string(d.Case)}] = true
		}
	}

	var records []evidence.Record
	semanticsByCapability := map[evidence.Surface]map[evidence.Capability][]evidence.Record{}

	for _, surface := range measured {
		for _, capability := range []evidence.Capability{evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification} {
			for _, caseID := range evidence.CapabilityCases[capability] {
				var rec evidence.Record
				switch {
				case slices.Contains(evidence.CatalogNotInducibleCases, caseID):
					rec = composeNotInducibleRecord(observedAt, surface, capability, caseID, evidence.NotInducibleDetail)
				case notInducibleReason[[2]string{string(surface), string(caseID)}] != "":
					rec = composeNotInducibleRecord(observedAt, surface, capability, caseID, notInducibleReason[[2]string{string(surface), string(caseID)}])
				case deferredDeclared[[2]string{string(capability), string(caseID)}]:
					continue
				default:
					obs, ok := collected.semantic[surface][caseID]
					if !ok {
						return nil, fmt.Errorf("no collected observation for surface %s capability %s case %s", surface, capability, caseID)
					}
					rec = composeSemanticRecord(observedAt, surface, capability, caseID, obs)
				}
				records = append(records, rec)
				if semanticsByCapability[surface] == nil {
					semanticsByCapability[surface] = map[evidence.Capability][]evidence.Record{}
				}
				semanticsByCapability[surface][capability] = append(semanticsByCapability[surface][capability], rec)
			}
		}
	}

	for _, d := range p.Declarations {
		if !deferredDeclared[[2]string{string(d.Capability), string(d.Case)}] {
			continue
		}
		for _, surface := range evidence.DeclarableSurfaces {
			if !slices.Contains(measured, surface) {
				continue
			}
			observedSessionID := collected.semantic[surface][d.Case].SessionID
			rec := composeDeclaredGapRecord(observedAt, surface, d.Capability, d.Case, d.Reason, observedSessionID)
			records = append(records, rec)
			semanticsByCapability[surface][d.Capability] = append(semanticsByCapability[surface][d.Capability], rec)
		}
	}

	for _, surface := range measured {
		for _, capability := range evidence.ComparisonCapabilities() {
			switch capability {
			case evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification:
				records = append(records, composeBaselineRecord(observedAt, surface, capability, semanticsByCapability[surface][capability]))
			case evidence.CapabilityTokenCeiling:
				records = append(records, composeTokenBaseline(observedAt, surface, collected))
			case evidence.CapabilitySessionContinuation:
				records = append(records, composeContinuationBaseline(observedAt, surface, collected))
			}
		}
	}

	policyRec := baseRecord(observedAt)
	policyRec.Scenario = evidence.ScenarioPolicyPrecondition
	policyRec.Surface = evidence.SurfaceAggregate
	policyRec.Capability = evidence.CapabilityPermissionHandling
	policyRec.Source = evidence.SourceProcessObservation
	policyRec.InputID = evidence.InputPolicyControl
	policyRec.EvidencePath = new("policy.deny_marker")
	evidence.ApplyObservation(&policyRec, collected.policy)
	records = append(records, policyRec)

	permissionRec := baseRecord(observedAt)
	permissionRec.Scenario = evidence.ScenarioPermissionRequest
	permissionRec.Surface = evidence.SurfaceProtocol
	permissionRec.Capability = evidence.CapabilityPermissionHandling
	permissionRec.Source = evidence.SourceProtocolStable
	permissionRec.InputID = evidence.InputPermissionProbe
	permissionRec.EvidencePath = new("session/request_permission")
	evidence.ApplyObservation(&permissionRec, collected.permission)
	records = append(records, permissionRec)

	toolServerRec := baseRecord(observedAt)
	toolServerRec.Scenario = evidence.ScenarioToolServer
	toolServerRec.Surface = evidence.SurfaceProtocol
	toolServerRec.Capability = evidence.CapabilityToolServerDelivery
	toolServerRec.Source = evidence.SourceProcessObservation
	toolServerRec.InputID = evidence.InputMCPProbe
	toolServerRec.EvidencePath = new("mcp_server.receipt")
	evidence.ApplyObservation(&toolServerRec, collected.toolServer)
	records = append(records, toolServerRec)

	workspaceRec := baseRecord(observedAt)
	workspaceRec.Scenario = evidence.ScenarioWorkspaceSecurity
	workspaceRec.Surface = evidence.SurfaceAggregate
	workspaceRec.Capability = evidence.CapabilityWorkspaceSecurity
	workspaceRec.Source = evidence.SourceProcessObservation
	workspaceRec.InputID = evidence.InputSecurity
	workspaceRec.EvidencePath = new("/workspace/settings")
	// Neither workspace security nor process cleanup carries an
	// identifier, so an Observation.SessionID on either input is not
	// written: it cannot be about a specific launch.
	evidence.ApplyObservation(&workspaceRec, evidence.Observation{Grade: collected.workspaceSecurity.Grade, Outcome: collected.workspaceSecurity.Outcome, Detail: collected.workspaceSecurity.Detail})
	records = append(records, workspaceRec)

	cleanupRec := baseRecord(observedAt)
	cleanupRec.Scenario = evidence.ScenarioProcessCleanup
	cleanupRec.Surface = evidence.SurfaceAggregate
	cleanupRec.Capability = evidence.CapabilityProcessCleanup
	cleanupRec.Source = evidence.SourceProcessObservation
	cleanupRec.InputID = evidence.InputCleanup
	cleanupRec.EvidencePath = new("process_group.liveness")
	evidence.ApplyObservation(&cleanupRec, evidence.Observation{Grade: collected.processCleanup.Grade, Outcome: collected.processCleanup.Outcome, Detail: collected.processCleanup.Detail})
	records = append(records, cleanupRec)

	endToEndRec := baseRecord(observedAt)
	endToEndRec.Scenario = evidence.ScenarioEndToEnd
	endToEndRec.Surface = evidence.SurfaceProtocol
	endToEndRec.Capability = evidence.CapabilityTurnDisposition
	endToEndRec.Source = evidence.SourceProcessObservation
	endToEndRec.InputID = evidence.InputE2E
	endToEndRec.EvidencePath = new("/run_history/status")
	evidence.ApplyObservation(&endToEndRec, endToEndObservation(collected.endToEnd))
	records = append(records, endToEndRec)

	for _, surface := range measured {
		records = append(records, composeContinuationRecords(observedAt, surface, collected)...)
		records = append(records, composeTokenRecords(observedAt, surface, collected)...)
	}

	if collected.tokenCompensation.supplied {
		rec := baseRecord(observedAt)
		rec.Scenario = evidence.ScenarioTokenSource
		rec.Surface = evidence.SurfaceProtocol
		rec.Capability = evidence.CapabilityTokenCeiling
		rec.Source = evidence.SourceSortieShared
		rec.Grade = evidence.GradeUsable
		rec.Outcome = evidence.OutcomePass
		rec.InputID = evidence.InputTokenInventory
		rec.EvidencePath = new(compensatedTokenPath)
		rec.SessionID = new(collected.tokenCompensation.sessionID)
		rec.Detail = evidence.BoundDetail(compensatedTokenDetail)
		records = append(records, rec)
	}

	records = append(records, composeIdentityRecords(observedAt, records, collected)...)

	slices.SortStableFunc(records, evidence.OrderCompare)
	for i := range records {
		records[i].Sequence = i + 1
	}
	return records, nil
}

// composeTokenBaseline derives surface's token-ceiling baseline from its
// own token inventory records.
func composeTokenBaseline(observedAt string, surface evidence.Surface, collected collectedObservations) evidence.Record {
	tokenRecs := composeTokenRecords(observedAt, surface, collected)
	pointers := make([]*evidence.Record, len(tokenRecs))
	for i := range tokenRecs {
		pointers[i] = &tokenRecs[i]
	}
	rec := baseRecord(observedAt)
	rec.Scenario = evidence.ScenarioSurfaceBaseline
	rec.Surface = surface
	rec.Capability = evidence.CapabilityTokenCeiling
	rec.Source = evidence.SourceComparison
	rec.InputID = evidence.InputBaseline
	rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", surface, evidence.CapabilityTokenCeiling))
	rec.Detail = fmt.Sprintf("derived %s grade for %s", evidence.CapabilityTokenCeiling, surface)
	rec.Grade = evidence.TokenBaselineGrade(pointers)
	var outcomes []evidence.Outcome
	for _, r := range tokenRecs {
		outcomes = append(outcomes, r.Outcome)
	}
	rec.Outcome = evidence.DeriveBaselineOutcome(rec.Grade, outcomes)
	return rec
}

// composeContinuationBaseline derives surface's session-continuation
// baseline from its own recall observation.
func composeContinuationBaseline(observedAt string, surface evidence.Surface, collected collectedObservations) evidence.Record {
	recall := collected.continuationRecall[surface]
	rec := baseRecord(observedAt)
	rec.Scenario = evidence.ScenarioSurfaceBaseline
	rec.Surface = surface
	rec.Capability = evidence.CapabilitySessionContinuation
	rec.Source = evidence.SourceComparison
	rec.InputID = evidence.InputBaseline
	rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", surface, evidence.CapabilitySessionContinuation))
	rec.Grade = recall.Grade
	rec.Detail = evidence.BoundDetail(recall.Detail)
	rec.Outcome = evidence.DeriveBaselineOutcome(recall.Grade, []evidence.Outcome{recall.Outcome})
	return rec
}

// composeContinuationRecords composes surface's continuation seed and
// recall records from their own observations.
func composeContinuationRecords(observedAt string, surface evidence.Surface, collected collectedObservations) []evidence.Record {
	seedObs := collected.continuationSeed[surface]
	recallObs := collected.continuationRecall[surface]

	seed := baseRecord(observedAt)
	seed.Scenario = evidence.ScenarioContinuation
	seed.Surface = surface
	seed.Capability = evidence.CapabilitySessionContinuation
	seed.InputID = evidence.InputContinuationSeed
	seed.Source = evidence.SourceNativeStructured
	if surface == evidence.SurfaceProtocol {
		seed.Source = evidence.SourceProtocolStable
	}
	seed.EvidencePath = new("/continuation/seed")
	evidence.ApplyObservation(&seed, seedObs)

	recall := baseRecord(observedAt)
	recall.Scenario = evidence.ScenarioContinuation
	recall.Surface = surface
	recall.Capability = evidence.CapabilitySessionContinuation
	recall.InputID = evidence.InputContinuationRecall
	recall.Source = seed.Source
	recall.EvidencePath = new("/continuation/recall")
	evidence.ApplyObservation(&recall, recallObs)
	if seedObs.SessionID != "" {
		recall.PriorSessionID = new(seedObs.SessionID)
	}

	return []evidence.Record{seed, recall}
}

// composeTokenRecords composes surface's token inventory records: one
// sentinel when no path was resolved, one record per resolved path
// otherwise.
func composeTokenRecords(observedAt string, surface evidence.Surface, collected collectedObservations) []evidence.Record {
	sessionID := collected.tokenSessionID[surface]
	paths := collected.tokenPaths[surface]
	inventory := collected.tokenInventory[surface]

	var extension *evidence.ExtensionReading
	if surface == evidence.SurfaceProtocol {
		extension = collected.tokenExtension
	}

	if len(paths) == 0 {
		rec := baseRecord(observedAt)
		rec.Scenario = evidence.ScenarioTokenSource
		rec.Surface = surface
		rec.Capability = evidence.CapabilityTokenCeiling
		rec.Source = evidence.SourceNone
		rec.InputID = evidence.InputTokenInventory
		evidence.ApplyObservation(&rec, inventory)
		extension.WriteOnto(&rec)
		return []evidence.Record{rec}
	}

	source := evidence.SourceNativeStructured
	if surface == evidence.SurfaceProtocol {
		source = evidence.SourceProtocolStable
	}
	records := make([]evidence.Record, 0, len(paths))
	for _, path := range paths {
		rec := baseRecord(observedAt)
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
		records = append(records, rec)
	}
	return records
}

// composeIdentityRecords mutates every protocol record in records with
// resolved identity fields. A native record carries no agent_version:
// only the protocol handshake answers for identity.
func composeIdentityRecords(observedAt string, records []evidence.Record, collected collectedObservations) []evidence.Record {
	referenced := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface != evidence.SurfaceProtocol {
			rec.AgentVersion = nil
			continue
		}
		if collected.identityProtocolVersion != 0 {
			rec.ProtocolVersion = new(collected.identityProtocolVersion)
		}
		if rec.SessionID == nil {
			continue
		}
		referenced[*rec.SessionID] = true
		identity, named := collected.identities[*rec.SessionID]
		if !named {
			rec.AgentName = nil
			rec.AgentVersion = nil
			continue
		}
		rec.AgentName = new(identity.Name)
		rec.AgentVersion = new(identity.Version)
	}

	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	identityRecords := make([]evidence.Record, 0, len(ids))
	for _, sessionID := range ids {
		rec := baseRecord(observedAt)
		rec.Scenario = evidence.ScenarioRuntimeIdentity
		rec.Surface = evidence.SurfaceProtocol
		rec.Capability = evidence.CapabilityRuntimeIdentity
		rec.Source = evidence.SourceProtocolStable
		rec.InputID = evidence.InputIdentity
		rec.EvidencePath = new("/handshake/agent_info")
		rec.SessionID = new(sessionID)
		if collected.identityProtocolVersion != 0 {
			rec.ProtocolVersion = new(collected.identityProtocolVersion)
		}
		rec.Grade = collected.identityObs.Grade
		rec.Outcome = collected.identityObs.Outcome
		rec.Detail = evidence.BoundDetail(collected.identityObs.Detail)
		identity, named := collected.identities[sessionID]
		if !named {
			rec.Grade = evidence.GradeNotObserved
			if collected.identityObs.Grade != evidence.GradeNotObserved {
				rec.Outcome = evidence.OutcomeAdapterUnanswered
				rec.Detail = "no handshake record named this session's own agent"
			}
		} else {
			rec.AgentName = new(identity.Name)
			rec.AgentVersion = new(identity.Version)
		}
		identityRecords = append(identityRecords, rec)
	}
	return identityRecords
}
