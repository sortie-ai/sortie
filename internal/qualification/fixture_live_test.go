package qualification

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

// admittedObservationPairs is kept independent of observationAdmission so the
// test catches an accidental widening or narrowing of the production table.
var admittedObservationPairs = map[Grade]map[Outcome]bool{
	GradeUsable:            {OutcomePass: true},
	GradeGap:               {OutcomePass: true},
	GradeCorroborationOnly: {OutcomePass: true},
	GradeNotObserved: {
		OutcomeNotObserved: true, OutcomePrerequisiteFailed: true,
		OutcomeFixtureInductionFailed: true, OutcomeRuntimeFailed: true,
	},
	GradeDeclaredGap:  {OutcomeNotProducible: true},
	GradeNotInducible: {OutcomeNotInducible: true},
}

func TestSetSemanticObservationAdmission(t *testing.T) {
	t.Parallel()

	for _, grade := range Grades {
		for _, outcome := range Outcomes {
			wantAdmit := admittedObservationPairs[grade][outcome]
			t.Run(fmt.Sprintf("%s/%s", grade, outcome), func(t *testing.T) {
				t.Parallel()

				fixture := NewFixture(FixtureQualified)
				err := fixture.SetSemanticObservation(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess,
					Observation{Grade: grade, Outcome: outcome, Detail: "d", SessionID: "sess-live"})

				if wantAdmit && err != nil {
					t.Errorf("SetSemanticObservation(%s, %s) error = %v, want nil", grade, outcome, err)
				}
				if !wantAdmit && err == nil {
					t.Errorf("SetSemanticObservation(%s, %s) = nil error, want a rejection", grade, outcome)
				}
			})
		}
	}
}

func TestSetSemanticObservationRejectsZeroOutcome(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureQualified)
	err := fixture.SetSemanticObservation(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess, Observation{Grade: GradeUsable})
	if err == nil {
		t.Error("SetSemanticObservation(usable, \"\") = nil error, want a rejection of the zero-value outcome")
	}
}

func driveFullyObservedLiveFixture(t *testing.T) *Fixture {
	t.Helper()

	fixture := NewFixture(FixtureNotObserved)
	surfaces := fixture.measured()

	for _, surface := range surfaces {
		for _, capability := range []Capability{CapabilityTurnDisposition, CapabilityRetryClassification} {
			for _, caseID := range CapabilityCases[capability] {
				obs := Observation{
					Grade: GradeUsable, Outcome: OutcomePass,
					Detail:       "live induction observed the case",
					SessionID:    fmt.Sprintf("live-%s-%s", surface, caseID),
					EvidencePath: "/live/evidence",
				}
				if err := fixture.SetSemanticObservation(surface, capability, caseID, obs); err != nil {
					t.Fatalf("SetSemanticObservation(%s, %s, %s, ...) error = %v, want nil", surface, capability, caseID, err)
				}
			}
		}
	}

	if err := fixture.SetToolServerDelivery(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live tool server delivery", SessionID: "live-mcp-session"}); err != nil {
		t.Fatalf("SetToolServerDelivery(...) error = %v, want nil", err)
	}
	if err := fixture.SetPermissionHandling(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live permission handling", SessionID: "live-permission-session"}); err != nil {
		t.Fatalf("SetPermissionHandling(...) error = %v, want nil", err)
	}
	if err := fixture.SetPolicyPrecondition(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live policy precondition", SessionID: "live-policy-session"}); err != nil {
		t.Fatalf("SetPolicyPrecondition(...) error = %v, want nil", err)
	}

	for _, surface := range surfaces {
		seedSession := fmt.Sprintf("live-%s-seed", surface)
		seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live continuation seed", SessionID: seedSession}
		recall := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: RecallConfirmedSameSession, SessionID: seedSession}
		if err := fixture.SetSessionContinuationObserved(surface, seed, recall); err != nil {
			t.Fatalf("SetSessionContinuationObserved(%s, ...) error = %v, want nil", surface, err)
		}
		if err := fixture.SetTokenInventory(surface, fmt.Sprintf("live-%s-tokens", surface),
			[]TokenObservation{{EvidencePath: "/live/usage", Kind: "spend"}},
			Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: "live token spend observed"}, nil); err != nil {
			t.Fatalf("SetTokenInventory(%s, ...) error = %v, want nil", surface, err)
		}
	}

	fixture.SetWorkspaceSecurity(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live workspace security"})
	fixture.SetProcessCleanup(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live process cleanup"})
	if err := fixture.SetEndToEnd(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live end to end", SessionID: "live-e2e-session"}); err != nil {
		t.Fatalf("SetEndToEnd(...) error = %v, want nil", err)
	}
	identities := map[string]SessionIdentity{}
	for _, rec := range fixture.Records {
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			identities[*rec.SessionID] = SessionIdentity{Name: "real-agent", Version: "9.9.9"}
		}
	}
	if err := fixture.SetRuntimeIdentity(Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "live handshake"}, identities, 3); err != nil {
		t.Fatalf("SetRuntimeIdentity(...) error = %v, want nil", err)
	}

	fixture.Finalize()
	return fixture
}

func TestLiveComposedFixtureCarriesNoFixtureSessionID(t *testing.T) {
	t.Parallel()

	fixture := driveFullyObservedLiveFixture(t)

	for _, rec := range fixture.Records {
		if rec.SessionID != nil && strings.HasPrefix(*rec.SessionID, "sess-") {
			t.Errorf("record %d (%s/%s) carries FixtureSession-shaped session_id %q on a fully live-observed set", rec.Sequence, rec.Scenario, rec.Surface, *rec.SessionID)
		}
	}
}

func TestLiveComposedFixtureCarriesNoFixtureAgentName(t *testing.T) {
	t.Parallel()

	fixture := driveFullyObservedLiveFixture(t)

	for _, rec := range fixture.Records {
		if rec.AgentName != nil && *rec.AgentName == FixtureAgentName {
			t.Errorf("record %d (%s/%s) carries FixtureAgentName %q on a fully live-observed set", rec.Sequence, rec.Scenario, rec.Surface, *rec.AgentName)
		}
	}
}

func TestLiveComposedFixtureCarriesNoFixtureAgentVersion(t *testing.T) {
	t.Parallel()

	fixture := driveFullyObservedLiveFixture(t)

	for _, rec := range fixture.Records {
		if rec.AgentVersion != nil && *rec.AgentVersion == FixtureAgentVer {
			t.Errorf("record %d (%s/%s) carries FixtureAgentVer %q on a fully live-observed set", rec.Sequence, rec.Scenario, rec.Surface, *rec.AgentVersion)
		}
	}
}

func TestFinalizeIdentityCoverageMatchesReferencedSessions(t *testing.T) {
	t.Parallel()

	fixture := driveFullyObservedLiveFixture(t)

	referenced := map[string]bool{}
	identityIDs := map[string]bool{}
	for _, rec := range fixture.Records {
		if rec.Scenario == ScenarioRuntimeIdentity {
			if rec.SessionID != nil {
				identityIDs[*rec.SessionID] = true
			}
			continue
		}
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			referenced[*rec.SessionID] = true
		}
	}
	if len(identityIDs) == 0 {
		t.Fatal("Finalize() produced no runtime-identity record, want one per distinct referenced protocol session")
	}
	if !maps.Equal(referenced, identityIDs) {
		t.Errorf("Finalize() identity coverage = %v, want exactly the referenced protocol session set %v", identityIDs, referenced)
	}
}

func TestSetSemanticLiveDeclaredGap(t *testing.T) {
	t.Parallel()

	t.Run("a missing declarable surface identifier is rejected and writes nothing", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		before := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal))
		beforeCopy := *before

		observed := map[Surface]string{
			SurfaceProtocol:   "live-protocol-refusal",
			SurfaceNativeJSON: "live-native-json-refusal",
			// SurfaceNativeStreamJSON intentionally omitted.
		}
		err := fixture.SetSemanticLiveDeclaredGap(CapabilityTurnDisposition, CaseRuntimeRefusal, DeclaredGapNeverProduced, observed)
		if err == nil {
			t.Fatal("SetSemanticLiveDeclaredGap(...) = nil error, want rejection of the missing surface identifier")
		}
		if !strings.Contains(err.Error(), string(SurfaceNativeStreamJSON)) {
			t.Errorf("SetSemanticLiveDeclaredGap(...) error = %v, want it to name %s", err, SurfaceNativeStreamJSON)
		}

		after := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal))
		if *after != beforeCopy {
			t.Errorf("SetSemanticLiveDeclaredGap(...) rewrote a record despite rejecting the call: got %+v, want %+v unchanged", *after, beforeCopy)
		}
	})

	t.Run("a complete observed map writes each surface's own identifier", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		observed := map[Surface]string{
			SurfaceProtocol:         "live-protocol-refusal",
			SurfaceNativeJSON:       "live-native-json-refusal",
			SurfaceNativeStreamJSON: "live-native-stream-refusal",
		}
		if err := fixture.SetSemanticLiveDeclaredGap(CapabilityTurnDisposition, CaseRuntimeRefusal, DeclaredGapNeverProduced, observed); err != nil {
			t.Fatalf("SetSemanticLiveDeclaredGap(...) error = %v, want nil", err)
		}

		for surface, sessionID := range observed {
			rec := fixture.FindFirst(MatchSemantic(surface, CapabilityTurnDisposition, CaseRuntimeRefusal))
			if rec == nil {
				t.Fatalf("no semantic record for surface %s after SetSemanticLiveDeclaredGap", surface)
			}
			if rec.Grade != GradeDeclaredGap {
				t.Errorf("surface %s record.Grade = %s, want %s", surface, rec.Grade, GradeDeclaredGap)
			}
			if rec.SessionID == nil || *rec.SessionID != sessionID {
				t.Errorf("surface %s record.SessionID = %s, want the supplied identifier %q", surface, continuationRecordDetail(rec.SessionID), sessionID)
			}
		}

		// DeclaredGapPeers rewrites CaseNonRetryableRefusal too, from the same
		// observed map.
		peer := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal))
		if peer == nil || peer.Grade != GradeDeclaredGap {
			t.Error("SetSemanticLiveDeclaredGap(...) did not rewrite the declared peer case")
		}
	})
}

func TestSetSessionContinuationObservedRejections(t *testing.T) {
	t.Parallel()

	validSeed := func() Observation {
		return Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "live-seed"}
	}

	tests := []struct {
		name   string
		seed   Observation
		recall Observation
	}{
		{
			name:   "an empty seed session id is rejected",
			seed:   Observation{Grade: GradeUsable, Outcome: OutcomePass},
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "live-seed", Detail: RecallConfirmedSameSession},
		},
		{
			name:   "an inadmissible seed grade-outcome pair is rejected",
			seed:   Observation{Grade: GradeUsable, Outcome: OutcomeNotObserved, SessionID: "live-seed"},
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "live-seed", Detail: RecallConfirmedSameSession},
		},
		{
			name:   "an inadmissible recall grade-outcome pair is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeUsable, Outcome: OutcomeNotObserved, SessionID: "live-seed", Detail: RecallConfirmedSameSession},
		},
		{
			name:   "a recall detail outside the closed set is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "live-seed", Detail: "made_up_detail"},
		},
		{
			name:   "confirmed_same_session with an empty recall session id is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: RecallConfirmedSameSession},
		},
		{
			name:   "confirmed_same_session with a session id differing from the seed is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "a-different-session", Detail: RecallConfirmedSameSession},
		},
		{
			name:   "confirmed_same_session with grade gap instead of usable is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, SessionID: "live-seed", Detail: RecallConfirmedSameSession},
		},
		{
			name:   "fresh_session_fallback with the seed's own session id is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, SessionID: "live-seed", Detail: RecallFreshFallback},
		},
		{
			name:   "fresh_session_fallback with grade usable instead of gap is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "a-different-session", Detail: RecallFreshFallback},
		},
		{
			name:   "unobserved_actual_session with a non-empty session id is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed, SessionID: "a-session", Detail: RecallUnobservedActual},
		},
		{
			name:   "unobserved_actual_session with grade other than not_observed is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: RecallUnobservedActual},
		},
		{
			name:   "recall_precondition_unmet with a non-empty session id is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeNotObserved, Outcome: OutcomePrerequisiteFailed, SessionID: "a-session", Detail: RecallPreconditionUnmet},
		},
		{
			name:   "recall_precondition_unmet with grade other than not_observed is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: RecallPreconditionUnmet},
		},
		{
			name:   "recall_precondition_unmet with an outcome other than prerequisite_failed is rejected",
			seed:   validSeed(),
			recall: Observation{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed, Detail: RecallPreconditionUnmet},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, tt.seed, tt.recall); err == nil {
				t.Errorf("SetSessionContinuationObserved(protocol, %+v, %+v) = nil error, want a rejection", tt.seed, tt.recall)
			}
		})
	}

	t.Run("a valid combination writes the seed's own identifier into prior_session_id", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		seed := validSeed()
		recall := Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: seed.SessionID, Detail: RecallConfirmedSameSession}
		if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, seed, recall); err != nil {
			t.Fatalf("SetSessionContinuationObserved(protocol, ...) error = %v, want nil", err)
		}

		recallRec := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
		if recallRec == nil {
			t.Fatal("no continuation recall record after SetSessionContinuationObserved")
		}
		if recallRec.PriorSessionID == nil || *recallRec.PriorSessionID != seed.SessionID {
			t.Errorf("recall.PriorSessionID = %s, want the seed's own session id %q", continuationRecordDetail(recallRec.PriorSessionID), seed.SessionID)
		}
	})
}

func TestSetRuntimeIdentityRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		obs             Observation
		identities      map[string]SessionIdentity
		protocolVersion int
	}{
		{
			name:            "a non-empty observation session id is rejected",
			obs:             Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: "a-session"},
			identities:      map[string]SessionIdentity{"a-session": {Name: "real-agent", Version: "9.9.9"}},
			protocolVersion: 1,
		},
		{
			name:            "a usable observation with an empty name is rejected",
			obs:             Observation{Grade: GradeUsable, Outcome: OutcomePass},
			identities:      map[string]SessionIdentity{"a-session": {Name: "", Version: "9.9.9"}},
			protocolVersion: 1,
		},
		{
			name:            "a usable observation with an empty version is rejected",
			obs:             Observation{Grade: GradeUsable, Outcome: OutcomePass},
			identities:      map[string]SessionIdentity{"a-session": {Name: "real-agent", Version: ""}},
			protocolVersion: 1,
		},
		{
			name:            "a usable observation naming no session's handshake is rejected",
			obs:             Observation{Grade: GradeUsable, Outcome: OutcomePass},
			protocolVersion: 1,
		},
		{
			name:            "a not_observed observation carrying a handshake is rejected",
			obs:             Observation{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed},
			identities:      map[string]SessionIdentity{"a-session": {Name: "real-agent", Version: "9.9.9"}},
			protocolVersion: 1,
		},
		{
			name:            "outcome not_observed is rejected",
			obs:             Observation{Grade: GradeNotObserved, Outcome: OutcomeNotObserved},
			protocolVersion: 1,
		},
		{
			name:            "a zero protocol_version is rejected",
			obs:             Observation{Grade: GradeUsable, Outcome: OutcomePass},
			identities:      map[string]SessionIdentity{"a-session": {Name: "real-agent", Version: "9.9.9"}},
			protocolVersion: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			err := fixture.SetRuntimeIdentity(tt.obs, tt.identities, tt.protocolVersion)
			if err == nil {
				t.Errorf("SetRuntimeIdentity(%+v, %v, %d) = nil error, want a rejection", tt.obs, tt.identities, tt.protocolVersion)
			}
		})
	}

	t.Run("a not_observed observation clears every non-aggregate agent field yet keeps protocol_version", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		if err := fixture.SetRuntimeIdentity(Observation{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed, Detail: "handshake never completed"}, nil, 1); err != nil {
			t.Fatalf("SetRuntimeIdentity(not_observed, ...) error = %v, want nil", err)
		}

		for i := range fixture.Records {
			rec := &fixture.Records[i]
			if rec.Surface == SurfaceAggregate {
				continue
			}
			if rec.AgentName != nil {
				t.Errorf("record %d (%s/%s) carries a non-nil agent_name after a not_observed identity observation", rec.Sequence, rec.Scenario, rec.Surface)
			}
			if rec.AgentVersion != nil {
				t.Errorf("record %d (%s/%s) carries a non-nil agent_version after a not_observed identity observation", rec.Sequence, rec.Scenario, rec.Surface)
			}
			if rec.Surface == SurfaceProtocol && rec.ProtocolVersion == nil {
				t.Errorf("protocol record %d (%s) lost its protocol_version after a not_observed identity observation", rec.Sequence, rec.Scenario)
			}
		}
	})
}
