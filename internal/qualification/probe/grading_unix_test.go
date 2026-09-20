//go:build unix

package probe

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
)

var inducedGrades = []qualification.Grade{qualification.GradeUsable, qualification.GradeGap, qualification.GradeNotObserved}

func outcomeForSweptGrade(grade qualification.Grade) qualification.Outcome {
	if grade == qualification.GradeNotObserved {
		return qualification.OutcomeFixtureInductionFailed
	}
	return qualification.OutcomePass
}

func fullyObservedCollected(profile qualification.RuntimeProfile, toolGrade, permissionGrade, continuationGrade qualification.Grade) collectedObservations {
	measured := profile.MeasuredSurfaces()

	declared := map[[2]string]bool{}
	for _, d := range profile.Declarations {
		declared[[2]string{string(d.Capability), string(d.Case)}] = true
	}
	notInducible := map[[2]string]bool{}
	for _, entry := range profile.NotInducibleCases {
		notInducible[[2]string{string(entry.Surface), string(entry.Case)}] = true
	}

	collected := collectedObservations{
		semantic:           map[qualification.Surface]map[qualification.Case]qualification.Observation{},
		continuationSeed:   map[qualification.Surface]qualification.Observation{},
		continuationRecall: map[qualification.Surface]qualification.Observation{},
		tokenSessionID:     map[qualification.Surface]string{},
		tokenPaths:         map[qualification.Surface][]qualification.TokenObservation{},
		tokenInventory:     map[qualification.Surface]qualification.Observation{},
	}

	permissionSessionID := "sess-protocol-permission"

	for _, surface := range measured {
		byCase := map[qualification.Case]qualification.Observation{}
		for _, capability := range []qualification.Capability{qualification.CapabilityTurnDisposition, qualification.CapabilityRetryClassification} {
			for _, caseID := range qualification.CapabilityCases[capability] {
				if slices.Contains(qualification.CatalogNotInducibleCases, caseID) || notInducible[[2]string{string(surface), string(caseID)}] {
					continue
				}
				// The refusal disposition and its non_retryable_refusal retry
				// peer derive from one physical run, so both share one session
				// id per surface; protocol human_input reuses the permission
				// attempt's session.
				sessionKey := string(caseID)
				if _, hasPeer := qualification.DeclaredGapPeers[caseID]; hasPeer {
					sessionKey = "refusal-pair"
				}
				sessionID := fmt.Sprintf("sess-%s-%s", surface, sessionKey)
				if surface == qualification.SurfaceProtocol && caseID == qualification.CaseHumanInput {
					sessionID = permissionSessionID
				}
				if declared[[2]string{string(capability), string(caseID)}] {
					byCase[caseID] = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "declared", SessionID: sessionID}
					continue
				}
				byCase[caseID] = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "migrated fixture observation", SessionID: sessionID, EvidencePath: "/turn/stop_reason"}
			}
		}
		collected.semantic[surface] = byCase

		seedID := fmt.Sprintf("sess-%s-seed", surface)
		collected.continuationSeed[surface] = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "seed", SessionID: seedID}
		switch continuationGrade {
		case qualification.GradeNotObserved:
			collected.continuationRecall[surface] = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: qualification.RecallUnobservedActual}
		case qualification.GradeUsable:
			collected.continuationRecall[surface] = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: qualification.RecallConfirmedSameSession, SessionID: seedID}
		default:
			collected.continuationRecall[surface] = qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: qualification.RecallFreshFallback, SessionID: fmt.Sprintf("sess-%s-recall-fallback", surface)}
		}

		collected.tokenSessionID[surface] = fmt.Sprintf("sess-%s-token", surface)
		collected.tokenPaths[surface] = []qualification.TokenObservation{{EvidencePath: "/token/path", Kind: "spend"}}
		collected.tokenInventory[surface] = qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: "inventory completed"}
	}

	collected.toolServer = qualification.Observation{Grade: toolGrade, Outcome: outcomeForSweptGrade(toolGrade), Detail: "tool server induction: " + string(toolGrade), SessionID: "sess-protocol-mcp"}
	collected.permission = qualification.Observation{Grade: permissionGrade, Outcome: outcomeForSweptGrade(permissionGrade), Detail: "permission induction: " + string(permissionGrade), SessionID: "sess-protocol-permission"}
	collected.policy = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "policy precondition", SessionID: "sess-protocol-policy"}

	collected.workspaceSecurity = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "workspace security"}
	collected.processCleanup = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "checked_groups=0"}
	collected.endToEnd = qualification.Record{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "one succeeded history row", SessionID: new("sess-protocol-e2e")}
	collected.identityObs = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "handshake reported name and version"}
	collected.identities = protocolSessionIdentities(collected)
	collected.identityProtocolVersion = 1

	return collected
}

// protocolSessionIdentities gives every protocol session one handshake reading,
// the shape a collection that observed every session it opened produces.
func protocolSessionIdentities(collected collectedObservations) map[string]qualification.SessionIdentity {
	identity := qualification.SessionIdentity{Name: "fixture-agent", Version: "1.0.0-fixture"}
	identities := map[string]qualification.SessionIdentity{}
	named := func(sessionID string) {
		if sessionID != "" {
			identities[sessionID] = identity
		}
	}
	for _, obs := range collected.semantic[qualification.SurfaceProtocol] {
		named(obs.SessionID)
	}
	named(collected.continuationSeed[qualification.SurfaceProtocol].SessionID)
	named(collected.continuationRecall[qualification.SurfaceProtocol].SessionID)
	named(collected.tokenSessionID[qualification.SurfaceProtocol])
	named(collected.toolServer.SessionID)
	named(collected.permission.SessionID)
	named(collected.policy.SessionID)
	if collected.endToEnd.SessionID != nil {
		named(*collected.endToEnd.SessionID)
	}
	return identities
}

func gradeOfClass(t *testing.T, records []qualification.Record, class qualification.RowClass) qualification.Grade {
	t.Helper()
	for i := range records {
		got, err := qualification.ClassifyRecord(&records[i])
		if err != nil {
			t.Fatalf("ClassifyRecord(%+v) error = %v, want nil", records[i], err)
		}
		if got == class {
			return records[i].Grade
		}
	}
	return ""
}

func continuationBaselineGrade(records []qualification.Record) qualification.Grade {
	for i := range records {
		rec := &records[i]
		if rec.Scenario == qualification.ScenarioSurfaceBaseline && rec.Surface == qualification.SurfaceProtocol && rec.Capability == qualification.CapabilitySessionContinuation {
			return rec.Grade
		}
	}
	return ""
}

func TestGradedEvidenceValidatesAgainstEveryProfile(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}

		t.Run(profilePath, func(t *testing.T) {
			t.Parallel()

			for _, toolGrade := range inducedGrades {
				for _, permissionGrade := range inducedGrades {
					for _, continuationGrade := range inducedGrades {
						name := string(toolGrade) + "_" + string(permissionGrade) + "_" + string(continuationGrade)
						t.Run(name, func(t *testing.T) {
							t.Parallel()

							collected := fullyObservedCollected(profile, toolGrade, permissionGrade, continuationGrade)
							fixture, err := gradedEvidence(profile, collected, time.Now().UTC())
							if err != nil {
								t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
							}

							path := qualification.WriteEvidenceFile(t, fixture.Records)
							verdict, err := qualification.ValidateObservationsWithDeclarations(path, profile)
							if err != nil {
								t.Fatalf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
							}
							_ = verdict

							if got := gradeOfClass(t, fixture.Records, qualification.RowMCPDelivery); got != toolGrade {
								t.Errorf("tool server delivery grade = %s, want %s", got, toolGrade)
							}
							if got := gradeOfClass(t, fixture.Records, qualification.RowPermission); got != permissionGrade {
								t.Errorf("permission handling grade = %s, want %s", got, permissionGrade)
							}
							if got := continuationBaselineGrade(fixture.Records); got != continuationGrade {
								t.Errorf("protocol continuation baseline grade = %s, want %s", got, continuationGrade)
							}
						})
					}
				}
			}
		})
	}
}

func TestCheckNoBarePair(t *testing.T) {
	t.Parallel()

	t.Run("a bare not_observed pair is rejected", func(t *testing.T) {
		t.Parallel()

		records := []qualification.Record{{Sequence: 1, Scenario: qualification.ScenarioSemanticProbe, Surface: qualification.SurfaceProtocol, Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeNotObserved}}
		if err := checkNoBarePair(records); err == nil {
			t.Error("checkNoBarePair(...) = nil error, want a rejection of the bare pair")
		}
	})

	t.Run("a not_observed row with a stated failure outcome is accepted", func(t *testing.T) {
		t.Parallel()

		records := []qualification.Record{{Sequence: 1, Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed}}
		if err := checkNoBarePair(records); err != nil {
			t.Errorf("checkNoBarePair(...) error = %v, want nil", err)
		}
	})

	t.Run("every tracked profile's fully-observed composition carries no bare pair", func(t *testing.T) {
		t.Parallel()

		for _, profilePath := range stalenessProfilePaths(t) {
			profile, err := qualification.ReadRuntimeProfileFile(profilePath)
			if err != nil {
				t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
			}
			collected := fullyObservedCollected(profile, qualification.GradeUsable, qualification.GradeUsable, qualification.GradeUsable)
			fixture, err := gradedEvidence(profile, collected, time.Now().UTC())
			if err != nil {
				t.Fatalf("gradedEvidence(%s, ...) error = %v, want nil", profilePath, err)
			}
			if err := checkNoBarePair(fixture.Records); err != nil {
				t.Errorf("checkNoBarePair(...) error = %v, want nil for profile %s", err, profilePath)
			}
		}
	})
}

// Swept together because the sentinel's zero-source flag would mask a
// compensating record leaked into the derivation, and the corroborating
// inventory would not.
const (
	wireSentinel      = "the wire offered no source at all"
	wireCorroborating = "the wire offered a source no budget can be kept in"
)

// sourcelessWireCollected returns one collection whose protocol wire offered no
// source a budget can be kept in, the only state in which a reading supplied
// outside the protocol has anything to credit. supplied states whether the
// adapter really returned a figure.
func sourcelessWireCollected(profile qualification.RuntimeProfile, wire string, supplied bool) collectedObservations {
	collected := fullyObservedCollected(profile, qualification.GradeUsable, qualification.GradeUsable, qualification.GradeUsable)
	protocol := qualification.SurfaceProtocol

	if wire == wireSentinel {
		collected.tokenSessionID[protocol] = ""
		collected.tokenPaths[protocol] = nil
	} else {
		collected.tokenPaths[protocol] = []qualification.TokenObservation{
			{EvidencePath: "/_meta/usage/context_tokens", Kind: "occupancy"},
		}
	}
	collected.tokenInventory[protocol] = qualification.Observation{
		Grade:   qualification.GradeGap,
		Outcome: qualification.OutcomePass,
		Detail:  "read 7 raw result(s) carrying a block no budget can be kept in",
	}
	collected.tokenExtension = &qualification.ExtensionReading{
		Source:   qualification.ExtensionSourcePresent,
		Admitted: false,
	}
	if supplied {
		collected.tokenCompensation = tokenCompensation{
			supplied:  true,
			sessionID: collected.continuationSeed[protocol].SessionID,
		}
	}
	collected.identities = protocolSessionIdentities(collected)
	return collected
}

// publishedOutcome grades one collection and reads the report back out of the
// evidence file it published, so assertions stand on the artifact an operator
// receives rather than on collection state.
func publishedOutcome(t *testing.T, profile qualification.RuntimeProfile, collected collectedObservations) (qualification.EligibilityReport, []qualification.Record) {
	t.Helper()

	fixture, err := gradedEvidence(profile, collected, time.Now().UTC())
	if err != nil {
		t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
	}
	path := qualification.WriteEvidenceFile(t, fixture.Records)
	if _, err := qualification.ValidateObservationsWithDeclarations(path, profile); err != nil {
		t.Fatalf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
	}
	published, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("ReadEvidenceFile(%s) error = %v, want nil", path, err)
	}
	return qualification.ExplainEligibility(published, profile), published
}

func rowLabelled(t *testing.T, report qualification.EligibilityReport, label string) qualification.RowOutcome {
	t.Helper()
	for _, row := range report.Rows {
		if row.Label == label {
			return row
		}
	}
	t.Fatalf("published report carries no %q row", label)
	return qualification.RowOutcome{}
}

func publishedTokenBaseline(t *testing.T, records []qualification.Record) qualification.Grade {
	t.Helper()
	for i := range records {
		if qualification.MatchBaseline(qualification.SurfaceProtocol, qualification.CapabilityTokenCeiling)(&records[i]) {
			return records[i].Grade
		}
	}
	t.Fatal("published evidence carries no protocol token_ceiling baseline")
	return ""
}

func compensatingRecords(records []qualification.Record) []qualification.Record {
	var found []qualification.Record
	for i := range records {
		if qualification.SuppliedOutsideProtocol(records[i].Source) {
			found = append(found, records[i])
		}
	}
	return found
}

func TestCompensationReportsTheFigureWithoutCreditingTheCeilingStop(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}

		for _, wire := range []string{wireSentinel, wireCorroborating} {
			t.Run(profilePath+" "+wire, func(t *testing.T) {
				t.Parallel()

				without, withoutRecords := publishedOutcome(t, profile, sourcelessWireCollected(profile, wire, false))
				with, withRecords := publishedOutcome(t, profile, sourcelessWireCollected(profile, wire, true))

				label := string(qualification.CapabilityTokenCeiling)
				before := rowLabelled(t, without, label)
				after := rowLabelled(t, with, label)

				if before.Conformance != qualification.StandingBelow {
					t.Errorf("unsupplied conformance standing = %s, want below", before.Conformance)
				}
				if !strings.Contains(before.ConformanceCause, "nothing outside the protocol supplies it") {
					t.Errorf("unsupplied conformance cause = %q, want it to state that nothing outside the protocol supplies it", before.ConformanceCause)
				}
				if after.Conformance == qualification.StandingSatisfied {
					t.Errorf("supplied conformance standing = satisfied, want it short of satisfied: a returned figure is not an observed ceiling stop")
				}
				if after.Conformance != qualification.StandingUnmeasured {
					t.Errorf("supplied conformance standing = %s, want unmeasured: the stop was neither observed nor shown absent", after.Conformance)
				}
				if !strings.Contains(after.ConformanceCause, "stop") {
					t.Errorf("supplied conformance cause = %q, want it to name the ceiling stop nothing observed", after.ConformanceCause)
				}
				if strings.Contains(after.ConformanceCause, "nothing outside the protocol supplies it") {
					t.Errorf("supplied conformance cause = %q, want it to stop claiming nothing supplies the figure", after.ConformanceCause)
				}

				if after.Standing != before.Standing || after.Cause != before.Cause {
					t.Errorf("parity row moved: %s/%q became %s/%q; the wire carries the same nothing either way",
						before.Standing, before.Cause, after.Standing, after.Cause)
				}
				if with.Verdict != without.Verdict {
					t.Errorf("transport parity verdict = %s, want %s: compensation must not reach the transport answer", with.Verdict, without.Verdict)
				}

				baseline := publishedTokenBaseline(t, withoutRecords)
				if baseline != qualification.GradeGap {
					t.Errorf("unsupplied protocol token_ceiling baseline = %s, want gap", baseline)
				}
				if got := publishedTokenBaseline(t, withRecords); got != baseline {
					t.Errorf("supplied protocol token_ceiling baseline = %s, want %s: the baseline states what the surface reported", got, baseline)
				}
			})
		}
	}
}

func TestUnsuppliedCompensationPublishesNothing(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}

		for _, wire := range []string{wireSentinel, wireCorroborating} {
			t.Run(profilePath+" "+wire, func(t *testing.T) {
				t.Parallel()

				_, withoutRecords := publishedOutcome(t, profile, sourcelessWireCollected(profile, wire, false))
				if found := compensatingRecords(withoutRecords); len(found) != 0 {
					t.Errorf("a run that measured nothing published %d reading(s) supplied outside the protocol, want 0", len(found))
				}

				supplied := sourcelessWireCollected(profile, wire, true)
				_, withRecords := publishedOutcome(t, profile, supplied)
				found := compensatingRecords(withRecords)
				if len(found) != 1 {
					t.Fatalf("a run that measured a figure published %d reading(s) supplied outside the protocol, want 1", len(found))
				}
				rec := found[0]
				if rec.Surface != qualification.SurfaceProtocol || rec.Capability != qualification.CapabilityTokenCeiling {
					t.Errorf("compensating record is %s/%s, want protocol/token_ceiling", rec.Surface, rec.Capability)
				}
				if rec.Grade != qualification.GradeUsable || rec.Outcome != qualification.OutcomePass {
					t.Errorf("compensating record is %s/%s, want usable/pass", rec.Grade, rec.Outcome)
				}
				if rec.SessionID == nil || *rec.SessionID != supplied.tokenCompensation.sessionID {
					t.Errorf("compensating record names session %v, want the session the figure was measured for %q", rec.SessionID, supplied.tokenCompensation.sessionID)
				}
				if !strings.Contains(rec.Detail, ceilingStopUnverified) {
					t.Errorf("compensating record detail = %q, want it to name the ceiling stop this collection never induced", rec.Detail)
				}
			})
		}
	}
}
