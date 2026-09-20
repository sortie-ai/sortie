//go:build unix

package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

const stalenessProfilesDir = "../profiles"

// stalenessProfilePaths lists every tracked runtime profile, so a control can
// sweep the full shipped set rather than one fixture profile.
func stalenessProfilePaths(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(stalenessProfilesDir)
	if err != nil {
		t.Fatalf("read %s: %v", stalenessProfilesDir, err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(stalenessProfilesDir, entry.Name()))
	}
	if len(paths) == 0 {
		t.Fatalf("%s yielded no profile, want at least one", stalenessProfilesDir)
	}
	return paths
}

func outcomeForSweptGrade(grade evidence.Grade) evidence.Outcome {
	if grade == evidence.GradeNotObserved {
		return evidence.OutcomeFixtureInductionFailed
	}
	return evidence.OutcomePass
}

// fullyObservedCollected builds a collectedObservations complete enough for
// composeLive to compose from with no "no collected observation" error.
func fullyObservedCollected(p profile.RuntimeProfile, toolGrade, permissionGrade, continuationGrade evidence.Grade) collectedObservations {
	measured := p.DeclaredMeasuredSurfaces()

	declared := map[[2]string]bool{}
	for _, d := range p.Declarations {
		declared[[2]string{string(d.Capability), string(d.Case)}] = true
	}
	notInducible := map[[2]string]bool{}
	for _, entry := range p.NotInducibleCases {
		notInducible[[2]string{string(entry.Surface), string(entry.Case)}] = true
	}

	collected := collectedObservations{
		semantic:           map[evidence.Surface]map[evidence.Case]evidence.Observation{},
		continuationSeed:   map[evidence.Surface]evidence.Observation{},
		continuationRecall: map[evidence.Surface]evidence.Observation{},
		tokenSessionID:     map[evidence.Surface]string{},
		tokenPaths:         map[evidence.Surface][]evidence.TokenObservation{},
		tokenInventory:     map[evidence.Surface]evidence.Observation{},
	}

	permissionSessionID := "sess-protocol-permission"

	for _, surface := range measured {
		byCase := map[evidence.Case]evidence.Observation{}
		for _, capability := range []evidence.Capability{evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification} {
			for _, caseID := range evidence.CapabilityCases[capability] {
				if slices.Contains(evidence.CatalogNotInducibleCases, caseID) || notInducible[[2]string{string(surface), string(caseID)}] {
					continue
				}
				// A refusal disposition and its retry peer share one session id.
				sessionKey := string(caseID)
				if _, hasPeer := evidence.DeclaredGapPeers[caseID]; hasPeer {
					sessionKey = "refusal-pair"
				}
				sessionID := fmt.Sprintf("sess-%s-%s", surface, sessionKey)
				if surface == evidence.SurfaceProtocol && caseID == evidence.CaseHumanInput {
					sessionID = permissionSessionID
				}
				if declared[[2]string{string(capability), string(caseID)}] {
					byCase[caseID] = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "declared", SessionID: sessionID}
					continue
				}
				byCase[caseID] = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "migrated fixture observation", SessionID: sessionID, EvidencePath: "/turn/stop_reason"}
			}
		}
		collected.semantic[surface] = byCase

		seedID := fmt.Sprintf("sess-%s-seed", surface)
		collected.continuationSeed[surface] = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "seed", SessionID: seedID}
		switch continuationGrade {
		case evidence.GradeNotObserved:
			collected.continuationRecall[surface] = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: evidence.RecallUnobservedActual}
		case evidence.GradeUsable:
			collected.continuationRecall[surface] = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: evidence.RecallConfirmedSameSession, SessionID: seedID}
		default:
			collected.continuationRecall[surface] = evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: evidence.RecallFreshFallback, SessionID: fmt.Sprintf("sess-%s-recall-fallback", surface)}
		}

		collected.tokenSessionID[surface] = fmt.Sprintf("sess-%s-token", surface)
		collected.tokenPaths[surface] = []evidence.TokenObservation{{EvidencePath: "/token/path", Kind: "spend"}}
		collected.tokenInventory[surface] = evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "inventory completed"}
	}

	collected.toolServer = evidence.Observation{Grade: toolGrade, Outcome: outcomeForSweptGrade(toolGrade), Detail: "tool server induction: " + string(toolGrade), SessionID: "sess-protocol-mcp"}
	collected.permission = evidence.Observation{Grade: permissionGrade, Outcome: outcomeForSweptGrade(permissionGrade), Detail: "permission induction: " + string(permissionGrade), SessionID: "sess-protocol-permission"}
	collected.policy = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "policy precondition", SessionID: "sess-protocol-policy"}

	collected.workspaceSecurity = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "workspace security"}
	collected.processCleanup = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "checked_groups=0"}
	collected.endToEnd = evidence.Record{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "one succeeded history row", SessionID: new("sess-protocol-e2e")}
	collected.identityObs = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "handshake reported name and version"}
	collected.identities = protocolSessionIdentities(collected)
	collected.identityProtocolVersion = 1

	return collected
}

// protocolSessionIdentities gives every protocol session one handshake
// reading, the shape a collection that observed every session it opened
// produces.
func protocolSessionIdentities(collected collectedObservations) map[string]evidence.SessionIdentity {
	identity := evidence.SessionIdentity{Name: "fixture-agent", Version: "1.0.0-fixture"}
	identities := map[string]evidence.SessionIdentity{}
	named := func(sessionID string) {
		if sessionID != "" {
			identities[sessionID] = identity
		}
	}
	for _, obs := range collected.semantic[evidence.SurfaceProtocol] {
		named(obs.SessionID)
	}
	named(collected.continuationSeed[evidence.SurfaceProtocol].SessionID)
	named(collected.continuationRecall[evidence.SurfaceProtocol].SessionID)
	named(collected.tokenSessionID[evidence.SurfaceProtocol])
	named(collected.toolServer.SessionID)
	named(collected.permission.SessionID)
	named(collected.policy.SessionID)
	if collected.endToEnd.SessionID != nil {
		named(*collected.endToEnd.SessionID)
	}
	return identities
}

// gradedEvidence composes p's live evidence from collected, dated by
// startedAt, wrapping composeLive's return in the struct{ Records }{} shape
// callers in this package expect.
func gradedEvidence(p profile.RuntimeProfile, collected collectedObservations, startedAt time.Time) (struct{ Records []evidence.Record }, error) {
	records, err := composeLive(p, collected, startedAt)
	return struct{ Records []evidence.Record }{Records: records}, err
}

var inducedGrades = []evidence.Grade{evidence.GradeUsable, evidence.GradeGap, evidence.GradeNotObserved}

func gradeOfClass(t *testing.T, records []evidence.Record, class evidence.RowClass) evidence.Grade {
	t.Helper()
	for i := range records {
		got, err := eval.ClassifyRecord(&records[i])
		if err != nil {
			t.Fatalf("ClassifyRecord(%+v) error = %v, want nil", records[i], err)
		}
		if got == class {
			return records[i].Grade
		}
	}
	return ""
}

func continuationBaselineGrade(records []evidence.Record) evidence.Grade {
	for i := range records {
		rec := &records[i]
		if rec.Scenario == evidence.ScenarioSurfaceBaseline && rec.Surface == evidence.SurfaceProtocol && rec.Capability == evidence.CapabilitySessionContinuation {
			return rec.Grade
		}
	}
	return ""
}

func TestGradedEvidenceValidatesAgainstEveryProfile(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		p, err := profile.Load(mustRepositoryRoot(t), profilePath)
		if err != nil {
			t.Fatalf("profile.Load(%s) error = %v, want nil", profilePath, err)
		}

		t.Run(profilePath, func(t *testing.T) {
			t.Parallel()

			for _, toolGrade := range inducedGrades {
				for _, permissionGrade := range inducedGrades {
					for _, continuationGrade := range inducedGrades {
						name := string(toolGrade) + "_" + string(permissionGrade) + "_" + string(continuationGrade)
						t.Run(name, func(t *testing.T) {
							t.Parallel()

							collected := fullyObservedCollected(p, toolGrade, permissionGrade, continuationGrade)
							fixture, err := gradedEvidence(p, collected, time.Now().UTC())
							if err != nil {
								t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
							}

							if got := gradeOfClass(t, fixture.Records, evidence.RowMCPDelivery); got != toolGrade {
								t.Errorf("tool server delivery grade = %s, want %s", got, toolGrade)
							}
							if got := gradeOfClass(t, fixture.Records, evidence.RowPermission); got != permissionGrade {
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

		records := []evidence.Record{{Sequence: 1, Scenario: evidence.ScenarioSemanticProbe, Surface: evidence.SurfaceProtocol, Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeNotObserved}}
		if err := checkNoBarePair(records); err == nil {
			t.Error("checkNoBarePair(...) = nil error, want a rejection of the bare pair")
		}
	})

	t.Run("a not_observed row with a stated failure outcome is accepted", func(t *testing.T) {
		t.Parallel()

		records := []evidence.Record{{Sequence: 1, Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed}}
		if err := checkNoBarePair(records); err != nil {
			t.Errorf("checkNoBarePair(...) error = %v, want nil", err)
		}
	})

	t.Run("every tracked profile's fully-observed composition carries no bare pair", func(t *testing.T) {
		t.Parallel()

		for _, profilePath := range stalenessProfilePaths(t) {
			p, err := profile.Load(mustRepositoryRoot(t), profilePath)
			if err != nil {
				t.Fatalf("profile.Load(%s) error = %v, want nil", profilePath, err)
			}
			collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)
			fixture, err := gradedEvidence(p, collected, time.Now().UTC())
			if err != nil {
				t.Fatalf("gradedEvidence(%s, ...) error = %v, want nil", profilePath, err)
			}
			if err := checkNoBarePair(fixture.Records); err != nil {
				t.Errorf("checkNoBarePair(...) error = %v, want nil for profile %s", err, profilePath)
			}
		}
	})
}
