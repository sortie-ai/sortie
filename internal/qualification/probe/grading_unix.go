//go:build unix

package probe

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// collectedObservations bundles every row the collection's inducers
// produced. It launches nothing and reads no environment.
type collectedObservations struct {
	semantic map[qualification.Surface]map[qualification.Case]qualification.Observation

	toolServer qualification.Observation
	permission qualification.Observation
	policy     qualification.Observation

	continuationSeed   map[qualification.Surface]qualification.Observation
	continuationRecall map[qualification.Surface]qualification.Observation

	tokenSessionID map[qualification.Surface]string
	tokenPaths     map[qualification.Surface][]qualification.TokenObservation
	tokenInventory map[qualification.Surface]qualification.Observation
	// tokenExtension is the protocol surface's extension-point reading,
	// which no native surface has.
	tokenExtension *qualification.ExtensionReading
	// tokenCompensation answers for the effective adapter alone, kept
	// apart from the inventory, which answers for the wire.
	tokenCompensation tokenCompensation

	workspaceSecurity qualification.Observation
	processCleanup    qualification.Observation
	endToEnd          qualification.Record

	identityObs             qualification.Observation
	identities              map[string]qualification.SessionIdentity
	identityProtocolVersion int
}

// endToEndObservation renders an end-to-end record as the Observation
// SetEndToEnd consumes.
func endToEndObservation(rec qualification.Record) qualification.Observation {
	obs := qualification.Observation{Grade: rec.Grade, Outcome: rec.Outcome, Detail: rec.Detail}
	if rec.SessionID != nil {
		obs.SessionID = *rec.SessionID
	}
	return obs
}

// declarableSurfaceUnproduced reports whether every declarable measured
// surface's observation for caseID is not_observed with a known session
// and an induction that ran and saw nothing. A launch the runtime
// itself failed is excluded, since reading a provider error as the
// declaration would confirm an outcome that never occurred.
func declarableSurfaceUnproduced(collected collectedObservations, measured []qualification.Surface, caseID qualification.Case) bool {
	for _, surface := range qualification.DeclarableSurfaces {
		if !slices.Contains(measured, surface) {
			continue
		}
		obs, ok := collected.semantic[surface][caseID]
		if !ok || obs.Grade != qualification.GradeNotObserved || obs.SessionID == "" {
			return false
		}
		if obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			return false
		}
	}
	return true
}

// gradedEvidence builds the evidence one live collection publishes, one
// setter call per row class dated by observedAt. It launches nothing
// and reads no environment.
func gradedEvidence(profile qualification.RuntimeProfile, collected collectedObservations, observedAt time.Time) (*qualification.Fixture, error) {
	if observedAt.IsZero() {
		return nil, errors.New("composing live evidence requires the collection's own start time")
	}
	fixture := qualification.NewLiveFixture(observedAt, profile.AbsentSurfaces...)
	measured := profile.MeasuredSurfaces()

	notInducibleReason := map[[2]string]string{}
	for _, entry := range profile.NotInducibleCases {
		notInducibleReason[[2]string{string(entry.Surface), string(entry.Case)}] = entry.Reason
	}

	deferredDeclared := map[[2]string]bool{}
	for _, d := range profile.Declarations {
		if declarableSurfaceUnproduced(collected, measured, d.Case) {
			deferredDeclared[[2]string{string(d.Capability), string(d.Case)}] = true
		}
	}

	for _, surface := range measured {
		for _, capability := range []qualification.Capability{qualification.CapabilityTurnDisposition, qualification.CapabilityRetryClassification} {
			for _, caseID := range qualification.CapabilityCases[capability] {
				if slices.Contains(qualification.CatalogNotInducibleCases, caseID) {
					fixture.SetSemanticNotInducible(surface, capability, caseID, qualification.NotInducibleDetail)
					continue
				}
				if reason, ok := notInducibleReason[[2]string{string(surface), string(caseID)}]; ok {
					fixture.SetSemanticNotInducible(surface, capability, caseID, reason)
					continue
				}
				if deferredDeclared[[2]string{string(capability), string(caseID)}] {
					continue
				}
				obs, ok := collected.semantic[surface][caseID]
				if !ok {
					return nil, fmt.Errorf("no collected observation for surface %s capability %s case %s", surface, capability, caseID)
				}
				if err := fixture.SetSemanticObservation(surface, capability, caseID, obs); err != nil {
					return nil, fmt.Errorf("surface %s capability %s case %s: %w", surface, capability, caseID, err)
				}
			}
		}
	}

	for _, d := range profile.Declarations {
		if !deferredDeclared[[2]string{string(d.Capability), string(d.Case)}] {
			continue
		}
		observed := map[qualification.Surface]string{}
		for _, surface := range qualification.DeclarableSurfaces {
			if !slices.Contains(measured, surface) {
				continue
			}
			observed[surface] = collected.semantic[surface][d.Case].SessionID
		}
		if err := fixture.SetSemanticLiveDeclaredGap(d.Capability, d.Case, d.Reason, observed); err != nil {
			return nil, fmt.Errorf("declared gap for capability %s case %s: %w", d.Capability, d.Case, err)
		}
	}

	if err := fixture.SetToolServerDelivery(collected.toolServer); err != nil {
		return nil, fmt.Errorf("tool server delivery: %w", err)
	}
	if err := fixture.SetPermissionHandling(collected.permission); err != nil {
		return nil, fmt.Errorf("permission handling: %w", err)
	}
	if err := fixture.SetPolicyPrecondition(collected.policy); err != nil {
		return nil, fmt.Errorf("policy precondition: %w", err)
	}

	for _, surface := range measured {
		seed := collected.continuationSeed[surface]
		recall := collected.continuationRecall[surface]
		if err := fixture.SetSessionContinuationObserved(surface, seed, recall); err != nil {
			return nil, fmt.Errorf("session continuation on surface %s: %w", surface, err)
		}
		var extension *qualification.ExtensionReading
		if surface == qualification.SurfaceProtocol {
			extension = collected.tokenExtension
		}
		if err := fixture.SetTokenInventory(surface, collected.tokenSessionID[surface], collected.tokenPaths[surface], collected.tokenInventory[surface], extension); err != nil {
			return nil, fmt.Errorf("token inventory on surface %s: %w", surface, err)
		}
	}

	// Written after every surface's inventory so the token baseline is
	// already derived from the wire and this reading cannot enter it.
	if collected.tokenCompensation.supplied {
		fixture.SetTokenCompensatedObserved(collected.tokenCompensation.sessionID, compensatedTokenPath, compensatedTokenDetail)
	}

	fixture.SetWorkspaceSecurity(collected.workspaceSecurity)
	fixture.SetProcessCleanup(collected.processCleanup)
	if err := fixture.SetEndToEnd(endToEndObservation(collected.endToEnd)); err != nil {
		return nil, fmt.Errorf("end to end: %w", err)
	}

	if err := fixture.SetRuntimeIdentity(collected.identityObs, collected.identities, collected.identityProtocolVersion); err != nil {
		return nil, fmt.Errorf("runtime identity: %w", err)
	}

	fixture.Finalize()
	return fixture, nil
}
