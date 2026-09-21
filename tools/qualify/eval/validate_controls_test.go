package eval

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func matchTokenSurface(surface evidence.Surface) func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioTokenSource && rec.Surface == surface
	}
}

func tokenRecordCount(records []evidence.Record) int {
	count := 0
	for i := range records {
		if records[i].Scenario == evidence.ScenarioTokenSource {
			count++
		}
	}
	return count
}

func protocolSessionCount(records []evidence.Record) int {
	ids := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == evidence.SurfaceProtocol && rec.SessionID != nil {
			ids[*rec.SessionID] = true
		}
	}
	return len(ids)
}

func TestValidatorMissingAndDuplicateControls(T *testing.T) {
	T.Parallel()

	pickers := []struct {
		name string
		pick func(*evidencetest.Fixture) *evidence.Record
	}{
		{"workspace security", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowWorkspaceSecurity))
		}},
		{"policy precondition", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowPolicyPrecondition))
		}},
		{"permission handling", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowPermission))
		}},
		{"tool server delivery", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowMCPDelivery))
		}},
		{"process cleanup", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowProcessCleanup))
		}},
		{"end to end", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowEndToEnd))
		}},
		{"disposition semantic", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		}},
		{"retry semantic", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseRetryableTransport))
		}},
		{"surface baseline", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
		}},
		{"token inventory", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchTokenSurface(evidence.SurfaceNativeJSON))
		}},
		{"continuation seed", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationSeed))
		}},
		{"continuation recall", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		}},
		{"runtime identity", func(f *evidencetest.Fixture) *evidence.Record {
			return f.FindFirst(matchRowClass(evidence.RowRuntimeIdentity))
		}},
	}

	for _, tt := range pickers {
		T.Run("missing "+tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
			fixture.Finalize()
			target := tt.pick(fixture)
			if target == nil {
				T.Fatalf("fixture carries no %s record to remove", tt.name)
			}
			fixture.Remove(target)
			fixture.Renumber()
			path := evidencetest.WriteEvidenceFile(T, fixture.Records)
			if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
				T.Errorf("ValidateObservations(%s) = nil error, want rejection when the %s record is missing", path, tt.name)
			}
		})

		T.Run("duplicate "+tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
			fixture.Finalize()
			target := tt.pick(fixture)
			if target == nil {
				T.Fatalf("fixture carries no %s record to duplicate", tt.name)
			}
			fixture.DuplicateAfter(target)
			fixture.Renumber()
			path := evidencetest.WriteEvidenceFile(T, fixture.Records)
			_, err := ValidateObservations(path, profile.RuntimeProfile{})
			if err == nil {
				T.Errorf("ValidateObservations(%s) = nil error, want rejection when the %s record is duplicated", path, tt.name)
			} else if !strings.Contains(err.Error(), "duplicate") {
				T.Errorf("ValidateObservations() error = %v, want it to report a duplicate", err)
			}
		})
	}
}

func TestValidatorSemanticControls(T *testing.T) {
	T.Parallel()

	T.Run("missing disposition cases", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range evidence.CapabilityCases[evidence.CapabilityTurnDisposition] {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
				fixture.Finalize()
				target := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, caseID))
				if target == nil {
					T.Fatalf("fixture carries no protocol disposition record for case %s", caseID)
				}
				fixture.Remove(target)
				fixture.Renumber()
				path := evidencetest.WriteEvidenceFile(T, fixture.Records)
				if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
					T.Errorf("ValidateObservations() = nil error, want rejection when disposition case %s is missing", caseID)
				}
			})
		}
	})

	T.Run("missing retry cases", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range evidence.CapabilityCases[evidence.CapabilityRetryClassification] {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
				fixture.Finalize()
				target := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, caseID))
				if target == nil {
					T.Fatalf("fixture carries no protocol retry record for case %s", caseID)
				}
				fixture.Remove(target)
				fixture.Renumber()
				path := evidencetest.WriteEvidenceFile(T, fixture.Records)
				if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
					T.Errorf("ValidateObservations() = nil error, want rejection when retry case %s is missing", caseID)
				}
			})
		}
	})

	T.Run("wrong disposition order", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		first := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		second := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeFailure))
		if first == nil || second == nil {
			T.Fatal("fixture carries no protocol disposition pair to swap")
		}
		swapRecords(fixture, first, second)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when disposition cases are out of order")
		}
	})

	T.Run("wrong retry order", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		first := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseRetryableTransport))
		second := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput))
		if first == nil || second == nil {
			T.Fatal("fixture carries no protocol retry pair to swap")
		}
		swapRecords(fixture, first, second)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when retry cases are out of order")
		}
	})

	T.Run("disposition tuple carrying a retry case", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		if rec == nil {
			T.Fatal("fixture carries no protocol success record")
		}
		rec.SemanticCase = new(evidence.CaseRetryableTransport)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a disposition tuple carrying a retry case")
		}
	})

	T.Run("retry tuple carrying a disposition case", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseRetryableTransport))
		if rec == nil {
			T.Fatal("fixture carries no protocol retryable record")
		}
		rec.SemanticCase = new(evidence.CaseSuccess)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a retry tuple carrying a disposition case")
		}
	})

	T.Run("baseline grade written before its own cases", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
		if baseline == nil {
			T.Fatal("fixture carries no protocol disposition baseline")
		}
		firstSemantic := 0
		for i := range fixture.Records {
			if fixture.Records[i].Scenario == evidence.ScenarioSemanticProbe {
				firstSemantic = i
				break
			}
		}
		fixture.Remove(baseline)
		fixture.Records = slices.Insert(fixture.Records, firstSemantic, *baseline)
		fixture.Renumber()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a baseline precedes its own cases")
		}
	})

	T.Run("written baseline grade disagrees with its cases", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
		if baseline == nil {
			T.Fatal("fixture carries no protocol disposition baseline")
		}
		baseline.Grade = evidence.GradeGap
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a baseline grade that differs from its derivation")
		}
	})

	T.Run("unobserved retry case lowers only retry classification, to an unmeasured verdict", func(T *testing.T) {
		T.Parallel()

		// Never measured on the protocol surface, so the row reports
		// unmeasured rather than the not_qualified a measured failure gives.
		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictUnmeasured)

		retryBaseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification))
		dispositionBaseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
		if retryBaseline == nil || dispositionBaseline == nil {
			T.Fatal("fixture carries no protocol baselines")
		}
		if retryBaseline.Grade != evidence.GradeNotObserved {
			T.Errorf("retry baseline = %s, want not_observed", retryBaseline.Grade)
		}
		if dispositionBaseline.Grade != evidence.GradeUsable {
			T.Errorf("disposition baseline = %s, want usable to stay untouched", dispositionBaseline.Grade)
		}
	})
}

func swapRecords(fixture *evidencetest.Fixture, a, b *evidence.Record) {
	indexA, indexB := -1, -1
	for i := range fixture.Records {
		switch &fixture.Records[i] {
		case a:
			indexA = i
		case b:
			indexB = i
		}
	}
	if indexA < 0 || indexB < 0 {
		panic("swapRecords: records not found")
	}
	fixture.Records[indexA], fixture.Records[indexB] = fixture.Records[indexB], fixture.Records[indexA]
	fixture.Renumber()
}

func TestValidatorTokenControls(T *testing.T) {
	T.Parallel()

	T.Run("zero-source sentinel is valid", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetTokenSentinel(evidence.SurfaceNativeJSON, false)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictQualified)
	})

	T.Run("failed inventory sentinel is valid and unmeasured", func(T *testing.T) {
		T.Parallel()

		// A failed token inventory leaves the protocol token_ceiling
		// baseline unmeasured, so the row is unmeasured, not a failure.
		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetTokenSentinel(evidence.SurfaceProtocol, true)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictUnmeasured)
	})

	T.Run("missing surface inventory", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(matchTokenSurface(evidence.SurfaceNativeJSON))
		fixture.Renumber()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a surface has no token inventory")
		}
	})

	T.Run("duplicate surface and evidence path", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		target := fixture.FindFirst(matchTokenSurface(evidence.SurfaceNativeJSON))
		if target == nil {
			T.Fatal("fixture carries no native_json token record")
		}
		fixture.DuplicateAfter(target)
		fixture.Renumber()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a duplicate token key")
		} else if !strings.Contains(err.Error(), "duplicate") {
			T.Errorf("ValidateObservations() error = %v, want it to report a duplicate", err)
		}
	})

	T.Run("two sentinels on one surface", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetTokenSentinel(evidence.SurfaceNativeJSON, false)
		fixture.Finalize()
		sentinel := fixture.FindFirst(matchTokenSurface(evidence.SurfaceNativeJSON))
		if sentinel == nil {
			T.Fatal("fixture carries no native_json sentinel")
		}
		fixture.DuplicateAfter(sentinel)
		fixture.Renumber()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a surface carries two token sentinels")
		}
	})

	T.Run("sentinel and non-sentinel on one surface", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		sentinel := sentinelFixtureRecord(evidence.SurfaceProtocol)
		insertAt := 0
		for i := range fixture.Records {
			if matchTokenSurface(evidence.SurfaceProtocol)(&fixture.Records[i]) {
				insertAt = i
				break
			}
		}
		fixture.Records = slices.Insert(fixture.Records, insertAt, sentinel)
		fixture.Renumber()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a sentinel beside non-sentinel records")
		} else if !strings.Contains(err.Error(), "sentinel") {
			T.Errorf("ValidateObservations() error = %v, want it to name the sentinel conflict", err)
		}
	})

	T.Run("token baseline disagrees with inventory", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTokenCeiling))
		if baseline == nil {
			T.Fatal("fixture carries no protocol token baseline")
		}
		baseline.Grade = evidence.GradeGap
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a token baseline inconsistent with its inventory")
		}
	})
}

func sentinelFixtureRecord(surface evidence.Surface) evidence.Record {
	return evidence.Record{
		SchemaVersion: 1,
		ObservedAt:    evidencetest.FixtureTime,
		Scenario:      evidence.ScenarioTokenSource,
		Surface:       surface,
		Capability:    evidence.CapabilityTokenCeiling,
		Source:        evidence.SourceNone,
		Grade:         evidence.GradeGap,
		Outcome:       evidence.OutcomePass,
		InputID:       evidence.InputTokenInventory,
		Detail:        "inventory completed with no token-bearing path",
	}
}

func TestValidatorContinuationControls(T *testing.T) {
	T.Parallel()

	T.Run("same-id confirmed continuation is positive", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictQualified)
	})

	T.Run("different-id fresh fallback with identity for both ids is positive", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "fallback"))
		recall.Detail = evidence.RecallFreshFallback
		recall.Grade = evidence.GradeGap
		if baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = evidence.GradeGap
		}
		fixture.AppendIdentity(*recall.SessionID)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictNotQualified)
	})

	T.Run("dangling prior session id", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.PriorSessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "dangling"))
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a dangling prior_session_id")
		}
	})

	T.Run("prior id on a forbidden record", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		seed := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationSeed))
		if seed == nil {
			T.Fatal("fixture carries no protocol seed record")
		}
		seed.PriorSessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "seed"))
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a prior id on a non-recall record")
		}
	})

	T.Run("missing runtime identity for the fallback actual id", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "fallback"))
		recall.Detail = evidence.RecallFreshFallback
		recall.Grade = evidence.GradeGap
		if baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = evidence.GradeGap
		}
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when the fallback actual id lacks a runtime identity record")
		} else if !strings.Contains(err.Error(), "runtime identity record") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing identity record", err)
		}
	})

	T.Run("a runtime identity for a session no other evidence references", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.AppendIdentity("sess-referenced-by-nothing")
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of an identity record no other evidence references")
		} else if !strings.Contains(err.Error(), "references no non-final evidence") {
			T.Errorf("ValidateObservations() error = %v, want it to name the unreferenced identity record", err)
		}
	})

	T.Run("a well-formed recall_precondition_unmet record validates", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.SessionID = nil
		recall.Detail = evidence.RecallPreconditionUnmet
		recall.Grade = evidence.GradeNotObserved
		recall.Outcome = evidence.OutcomePrerequisiteFailed
		if baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = evidence.GradeNotObserved
			baseline.Outcome = evidence.OutcomePrerequisiteFailed
		}
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, evidence.VerdictUnmeasured)
	})

	T.Run("recall_precondition_unmet rejects a non-null session_id", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.Detail = evidence.RecallPreconditionUnmet
		recall.Grade = evidence.GradeNotObserved
		recall.Outcome = evidence.OutcomePrerequisiteFailed
		if baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = evidence.GradeNotObserved
			baseline.Outcome = evidence.OutcomeNotObserved
		}
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of recall_precondition_unmet paired with a non-null session_id")
		} else if !strings.Contains(err.Error(), "recall_precondition_unmet requires a null session_id") {
			T.Errorf("ValidateObservations() error = %v, want the recall arm's session rejection", err)
		}
	})

	T.Run("recall_precondition_unmet rejects a grade other than not_observed", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.SessionID = nil
		recall.Detail = evidence.RecallPreconditionUnmet
		recall.Grade = evidence.GradeUsable
		recall.Outcome = evidence.OutcomePass
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of recall_precondition_unmet paired with a grade other than not_observed")
		} else if !strings.Contains(err.Error(), "recall_precondition_unmet requires classification not_observed") {
			T.Errorf("ValidateObservations() error = %v, want the recall arm's grade rejection", err)
		}
	})

	T.Run("recall_precondition_unmet rejects an outcome other than prerequisite_failed", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.SessionID = nil
		recall.Detail = evidence.RecallPreconditionUnmet
		recall.Grade = evidence.GradeNotObserved
		recall.Outcome = evidence.OutcomeNotObserved
		if baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = evidence.GradeNotObserved
			baseline.Outcome = evidence.OutcomeNotObserved
		}
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of recall_precondition_unmet paired with an outcome other than prerequisite_failed")
		} else if !strings.Contains(err.Error(), "recall_precondition_unmet requires verdict prerequisite_failed") {
			T.Errorf("ValidateObservations() error = %v, want the recall arm's verdict rejection", err)
		}
	})

	T.Run("a recall detail outside the closed set is still rejected", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall record")
		}
		recall.Detail = "fabricated_recall_detail"
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a recall detail outside the closed set")
		} else if !strings.Contains(err.Error(), "outside the closed set") {
			T.Errorf("ValidateObservations() error = %v, want it to name the closed set rejection", err)
		}
	})
}

func TestValidatorCrossSurfacePriorSessionControl(T *testing.T) {
	T.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	recall := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
	nativeSeed := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceNativeJSON, evidence.InputContinuationSeed))
	if recall == nil || nativeSeed == nil {
		T.Fatal("fixture carries no protocol recall or native_json seed record")
	}
	if nativeSeed.SessionID == nil {
		T.Fatal("native_json seed carries no session id")
	}
	recall.PriorSessionID = new(*nativeSeed.SessionID)
	path := evidencetest.WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path, profile.RuntimeProfile{})
	if err == nil {
		T.Error("ValidateObservations() = nil error, want rejection of a cross-surface prior_session_id")
	} else if !strings.Contains(err.Error(), "does not resolve to that surface's seed session") {
		T.Errorf("ValidateObservations() error = %v, want a same-surface resolution failure", err)
	}
}

func TestValidatorFinalTupleControls(T *testing.T) {
	T.Parallel()

	T.Run("qualification tuple in the first pass", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeQualified)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a qualification tuple in the first pass")
		}
	})

	T.Run("final pass whose aggregate carries a foreign tuple", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		final := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeQualified)
		published, err := evidence.ReadEvidenceFile(final)
		if err != nil {
			T.Fatalf("read back the published evidence: %v", err)
		}
		published[len(published)-1].Capability = evidence.CapabilityWorkspaceSecurity
		path := evidencetest.WriteEvidenceFile(T, published)
		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of an aggregate outside the closed tuple")
		} else if !strings.Contains(err.Error(), "tuple") {
			T.Errorf("ValidateEvidence() error = %v, want it to name the rejected tuple", err)
		}
	})

	T.Run("final pass with two aggregates", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		final := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeQualified)
		published, err := evidence.ReadEvidenceFile(final)
		if err != nil {
			T.Fatalf("read back the published evidence: %v", err)
		}
		withAggregates := append(published, published[len(published)-1])
		for i := range withAggregates {
			withAggregates[i].Sequence = i + 1
		}
		path := evidencetest.WriteEvidenceFile(T, withAggregates)
		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of two aggregate records")
		} else if !errors.Is(err, errFinalRecordDuplicated) {
			T.Errorf("ValidateEvidence() error = %v, want the duplicated-aggregate cause", err)
		}
	})

	T.Run("final pass without the aggregate", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection when the aggregate is missing")
		} else if !errors.Is(err, errFinalRecordMissing) {
			T.Errorf("ValidateEvidence() error = %v, want the missing-aggregate cause", err)
		}
	})

	T.Run("record after the aggregate", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		stray := fixture.FindFirst(matchRowClass(evidence.RowProcessCleanup))
		if stray == nil {
			T.Fatal("fixture carries no cleanup record to copy")
		}
		final := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeQualified)
		published, err := evidence.ReadEvidenceFile(final)
		if err != nil {
			T.Fatalf("read back the published evidence: %v", err)
		}
		withStray := append(published, *stray)
		for i := range withStray {
			withStray[i].Sequence = i + 1
		}
		path := evidencetest.WriteEvidenceFile(T, withStray)
		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of a record after the aggregate")
		} else if !errors.Is(err, errRecordAfterAggregate) {
			T.Errorf("ValidateEvidence() error = %v, want the record-after-aggregate cause", err)
		}
	})

	T.Run("final verdict differs from recomputation", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeNotQualified)
		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of a mismatched final verdict")
		}
	})

	T.Run("complete qualified final file validates", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		tokenCount := tokenRecordCount(fixture.Records)
		sessionCount := protocolSessionCount(fixture.Records)
		if tokenCount < 4 {
			T.Fatalf("fixture token count = %d, want at least 4", tokenCount)
		}
		path := evidencetest.WriteFinalEvidenceFile(T, fixture.Records, evidence.GradeQualified)
		RequireFinalVerdict(T, path, evidence.VerdictQualified)
		_ = sessionCount
	})
}

func declareSemanticGap(f *evidencetest.Fixture, surface evidence.Surface, caseID evidence.Case, reason string) {
	rec := f.FindFirst(evidencetest.MatchSemantic(surface, evidence.CapabilityTurnDisposition, caseID))
	rec.Grade = evidence.GradeDeclaredGap
	rec.Outcome = evidence.OutcomeNotProducible
	rec.Detail = reason
}

func declareNotInducible(f *evidencetest.Fixture, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) {
	rec := f.FindFirst(evidencetest.MatchSemantic(surface, capability, caseID))
	rec.Grade = evidence.GradeNotInducible
	rec.Outcome = evidence.OutcomeNotInducible
	rec.Detail = evidence.NotInducibleDetail
	rec.EvidencePath = nil
	rec.SessionID = nil
}

func TestValidatorExcludedCaseControls(T *testing.T) {
	T.Parallel()

	T.Run("both a declared gap and a not-inducible grade on one case", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, evidence.SurfaceProtocol, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		declareNotInducible(fixture, evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseCancellation)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of both exclusion kinds on one case")
		}
		if !strings.Contains(err.Error(), "carries both a declared gap and a not-inducible grade") {
			T.Errorf("ValidateObservations() error = %v, want it to name the conflicting exclusion kinds", err)
		}
	})

	T.Run("declared set is not exactly DeclarableSurfaces", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, evidence.SurfaceProtocol, evidence.CaseRuntimeRefusal, evidence.DeclaredGapNeverProduced)
		declareSemanticGap(fixture, evidence.SurfaceNativeJSON, evidence.CaseRuntimeRefusal, evidence.DeclaredGapNeverProduced)
		// native_stream_json left observed, so the declared set is incomplete.
		fixture.UpdateSemanticBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition)
		fixture.UpdateSemanticBaseline(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an incomplete declared set")
		}
		if !strings.Contains(err.Error(), "declared gap is missing on surfaces") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing surfaces", err)
		}
	})

	T.Run("catalog set is not exactly every measured surface", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		declareNotInducible(fixture, evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome)
		declareNotInducible(fixture, evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome)
		// native_stream_json left observed: a catalog-wide not-inducible case
		// must cover every measured surface.
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an incomplete not-inducible catalog set")
		}
		if !strings.Contains(err.Error(), "not-inducible grade is missing on surfaces") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing surfaces", err)
		}
	})

	T.Run("mixed detail within one declared set", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, evidence.SurfaceProtocol, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		declareSemanticGap(fixture, evidence.SurfaceNativeJSON, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		declareSemanticGap(fixture, evidence.SurfaceNativeStreamJSON, evidence.CaseCancellation, evidence.DeclaredGapFolded)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a non-uniform declared-gap detail")
		}
		if !strings.Contains(err.Error(), "differing declared-gap details") {
			T.Errorf("ValidateObservations() error = %v, want it to name the differing details", err)
		}
	})

	T.Run("declared record no declaration authorizes", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, evidence.SurfaceProtocol, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		declareSemanticGap(fixture, evidence.SurfaceNativeJSON, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		declareSemanticGap(fixture, evidence.SurfaceNativeStreamJSON, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an unauthorized declared record")
		}
		if !strings.Contains(err.Error(), "no declaration authorizes") {
			T.Errorf("ValidateObservations() error = %v, want it to name the unauthorized record", err)
		}
	})

	T.Run("a declaration no record matches", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		declarations := profile.RuntimeProfile{
			Declarations: []profile.DeclaredGap{{Capability: evidence.CapabilityTurnDisposition, Case: evidence.CaseRuntimeRefusal, Reason: evidence.DeclaredGapNeverProduced}},
		}
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an unmatched declaration")
		}
		if !strings.Contains(err.Error(), "matches no declared_gap record") {
			T.Errorf("ValidateObservations() error = %v, want it to name the unmatched declaration", err)
		}
	})

	T.Run("a peer whose declared set differs from DeclarableSurfaces", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		declarations := fixture.Declarations()
		// Reverting one surface leaves the peer's declared set incomplete
		// while the case itself stays uniformly declared.
		peer := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceNativeStreamJSON, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
		peer.Grade = evidence.GradeUsable
		peer.Outcome = evidence.OutcomePass
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a peer whose declared set is incomplete")
		}
		if !strings.Contains(err.Error(), "peer") || !strings.Contains(err.Error(), "does not carry a declared gap on surfaces") {
			T.Errorf("ValidateObservations() error = %v, want it to name the peer's incomplete declared set", err)
		}
	})

	T.Run("a peer whose reason differs", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		declarations := fixture.Declarations()
		for _, surface := range evidence.DeclarableSurfaces {
			rec := fixture.FindFirst(evidencetest.MatchSemantic(surface, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
			rec.Detail = evidence.DeclaredGapFolded
		}
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a peer reason mismatch")
		}
		if !strings.Contains(err.Error(), "differing declared-gap reasons") {
			T.Errorf("ValidateObservations() error = %v, want it to name the differing reasons", err)
		}
	})

	T.Run("a not-inducible detail other than the reason the profile scopes to the surface", func(T *testing.T) {
		T.Parallel()

		fixture, declarations := scopedNotInducibleHumanInput(evidence.NotInducibleTerminalVocabularyClosed, evidence.NotInducibleDetail)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a detail the profile does not scope to the surface")
		}
		if !strings.Contains(err.Error(), "the profile scopes to that surface") {
			T.Errorf("ValidateObservations() error = %v, want it to name the scoped reason", err)
		}
	})

	T.Run("a not-inducible detail equal to the reason the profile scopes to the surface", func(T *testing.T) {
		T.Parallel()

		fixture, declarations := scopedNotInducibleHumanInput(evidence.NotInducibleChannelTooSmall, evidence.NotInducibleChannelTooSmall)
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, declarations); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil", err)
		}
	})
}

// scopedNotInducibleHumanInput grades human_input not-inducible on every
// measured surface with detail, against a profile scoping reason to each of
// those surfaces.
func scopedNotInducibleHumanInput(reason, detail string) (*evidencetest.Fixture, profile.RuntimeProfile) {
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	declarations := fixture.Declarations()
	for _, surface := range declarations.DeclaredMeasuredSurfaces() {
		declareNotInducible(fixture, surface, evidence.CapabilityRetryClassification, evidence.CaseHumanInput)
		fixture.FindFirst(evidencetest.MatchSemantic(surface, evidence.CapabilityRetryClassification, evidence.CaseHumanInput)).Detail = detail
		declarations.NotInducibleCases = append(declarations.NotInducibleCases,
			profile.SurfaceNotInducible{Surface: surface, Case: evidence.CaseHumanInput, Reason: reason})
	}
	return fixture, declarations
}

func TestValidatorRejectsSemanticRecordWithoutEvidencePath(T *testing.T) {
	T.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
	if rec == nil {
		T.Fatal("FindFirst(MatchSemantic(protocol, turn_disposition, success)) = nil, want the fixture's own success record")
	}
	if rec.Grade != evidence.GradeUsable {
		T.Fatalf("fixture semantic success record grade = %s, want %s before the mutation this test relies on", rec.Grade, evidence.GradeUsable)
	}
	rec.EvidencePath = nil

	path := evidencetest.WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path, profile.RuntimeProfile{})
	if err == nil {
		T.Fatal("ValidateObservations() = nil error, want rejection of a usable semantic record with no evidence_path")
	}
	if !strings.Contains(err.Error(), "evidence_path must be set for a semantic probe record") {
		T.Errorf("ValidateObservations() error = %v, want it to name the missing evidence_path on the semantic probe record", err)
	}
}

func TestValidatorAllExcludedBaselineControl(T *testing.T) {
	T.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	var notInducible []profile.SurfaceNotInducible
	for _, surface := range fixture.Declarations().DeclaredMeasuredSurfaces() {
		declareNotInducible(fixture, surface, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)
		fixture.FindFirst(evidencetest.MatchSemantic(surface, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)).Detail = evidence.NotInducibleChannelTooSmall
		notInducible = append(notInducible, profile.SurfaceNotInducible{Surface: surface, Case: evidence.CaseRuntimeRefusal, Reason: evidence.NotInducibleChannelTooSmall})
		for _, caseID := range evidence.CapabilityCases[evidence.CapabilityRetryClassification] {
			declareNotInducible(fixture, surface, evidence.CapabilityRetryClassification, caseID)
			if caseID != evidence.CaseUnknownOutcome {
				fixture.FindFirst(evidencetest.MatchSemantic(surface, evidence.CapabilityRetryClassification, caseID)).Detail = evidence.NotInducibleChannelTooSmall
				notInducible = append(notInducible, profile.SurfaceNotInducible{Surface: surface, Case: caseID, Reason: evidence.NotInducibleChannelTooSmall})
			}
		}
	}
	path := evidencetest.WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path, profile.RuntimeProfile{NotInducibleCases: notInducible})
	if err == nil {
		T.Fatal("ValidateObservations() = nil error, want rejection of a surface-capability with every case excluded")
	}
	if !strings.Contains(err.Error(), "has every case excluded") {
		T.Errorf("ValidateObservations() error = %v, want it to name the all-excluded surface and capability", err)
	}
}

func TestValidatorClassifyRecordsExcludedRowControls(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name    string
		mutate  func(*evidencetest.Fixture)
		wantSub string
	}{
		{
			name: "declared_gap grade on a non-semantic row class",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowPermission))
				rec.Grade = evidence.GradeDeclaredGap
				rec.Outcome = evidence.OutcomeNotProducible
			},
			wantSub: "is valid only on a semantic probe record",
		},
		{
			name: "not_inducible grade on a non-semantic row class",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowMCPDelivery))
				rec.Grade = evidence.GradeNotInducible
				rec.Outcome = evidence.OutcomeNotInducible
			},
			wantSub: "is valid only on a semantic probe record",
		},
		{
			// The declared_gap check runs ahead of the generic semantic-probe
			// path check, so the operator reads which declaration rule broke.
			name: "declared_gap record with a null evidence_path",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeDeclaredGap
				rec.Outcome = evidence.OutcomeNotProducible
				rec.Detail = evidence.DeclaredGapNeverProduced
				rec.EvidencePath = nil
			},
			wantSub: "declared_gap record must carry a non-null evidence_path",
		},
		{
			name: "not_inducible record with a non-null evidence_path",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeNotInducible
				rec.Outcome = evidence.OutcomeNotInducible
				rec.Detail = evidence.NotInducibleDetail
			},
			wantSub: "not_inducible record must carry a null evidence_path",
		},
		{
			name: "not_inducible record with a non-null session_id",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeNotInducible
				rec.Outcome = evidence.OutcomeNotInducible
				rec.Detail = evidence.NotInducibleDetail
				rec.EvidencePath = nil
			},
			wantSub: "not_inducible record must carry a null session_id",
		},
		{
			name: "declared_gap record with a null session_id",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeDeclaredGap
				rec.Outcome = evidence.OutcomeNotProducible
				rec.Detail = evidence.DeclaredGapNeverProduced
				rec.SessionID = nil
			},
			wantSub: "declared_gap record must carry its own session_id",
		},
		{
			name: "declared_gap detail outside the closed reason set",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeDeclaredGap
				rec.Outcome = evidence.OutcomeNotProducible
				rec.Detail = "bogus_reason"
			},
			wantSub: "outside the closed reason set",
		},
		{
			name: "not_inducible detail differs from the one closed value",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseCancellation))
				rec.Grade = evidence.GradeNotInducible
				rec.Outcome = evidence.OutcomeNotInducible
				rec.Detail = "bogus_detail"
				rec.EvidencePath = nil
				rec.SessionID = nil
			},
			wantSub: "not_inducible record detail",
		},
		{
			name: "unmeasured grade on a non-final row",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
				rec.Grade = evidence.GradeUnmeasured
			},
			wantSub: "is valid only for capability eligibility",
		},
	}

	for _, tt := range tests {
		T.Run(tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
			fixture.Finalize()
			tt.mutate(fixture)
			path := evidencetest.WriteEvidenceFile(T, fixture.Records)
			_, err := ValidateObservations(path, profile.RuntimeProfile{})
			if err == nil {
				T.Fatalf("ValidateObservations() = nil error, want rejection naming %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				T.Errorf("ValidateObservations() error = %v, want it to mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidatorSemanticSessionRelationExemption(T *testing.T) {
	T.Parallel()

	T.Run("a session mismatch between two observed peers is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		retry := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
		retry.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "diverged"))
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a session mismatch between two observed peers")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservations() error = %v, want the session-reuse cause", err)
		}
	})

	T.Run("a not_observed disposition peer is exempt even when the retry session diverges", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)
		retry := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
		retry.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil: a not_observed disposition peer carries no session to check", err)
		}
	})

	T.Run("a not_observed retry peer is exempt even when the disposition session diverges", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal)
		disposition := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal))
		disposition.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil: a not_observed retry peer carries no session to check", err)
		}
	})

	T.Run("a session mismatch between two declared_gap peers is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		retry := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
		retry.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, fixture.Declarations())
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a session mismatch between two declared_gap peers: the exemption must not widen to a grade that still carries a session")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservations() error = %v, want the session-reuse cause", err)
		}
	})

	T.Run("a not_observed disposition peer that still carries a session is not exempt", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal)
		disposition := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal))
		disposition.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "not-observed-but-sessioned"))
		retry := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal))
		retry.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection: a not_observed peer that still carries a session must not be exempt, proving the skip keys on the absent session rather than on the not_observed grade")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservations() error = %v, want the session-reuse cause", err)
		}
	})
}

func TestCheckSessionRelationSessionlessSurfacePartition(T *testing.T) {
	T.Parallel()

	T.Run("the protocol surface still requires a session_id on a passing record", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		if rec == nil {
			T.Fatal("fixture carries no protocol disposition success record")
		}
		rec.SessionID = nil
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a passing protocol record with no session_id")
		}
		if !strings.Contains(err.Error(), "passing semantic probe must carry its own session_id") {
			T.Errorf("ValidateObservations() error = %v, want the missing-session cause", err)
		}
	})

	T.Run("a structured native passing record is accepted with a null session_id", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		if rec == nil {
			T.Fatal("fixture carries no native_json disposition success record")
		}
		rec.SessionID = nil
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil: a structured native record may carry either shape", err)
		}
	})

	T.Run("a structured native passing record is accepted with a non-null session_id", func(T *testing.T) {
		T.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(T, fixture.Records)
		// The qualified fixture already carries a non-null session_id on
		// every structured native record, so this arm needs no mutation.
		rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceNativeStreamJSON, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))
		if rec == nil || rec.SessionID == nil {
			T.Fatal("fixture carries no native_stream_json disposition success record with a non-null session_id")
		}
		RequireObservationVerdict(T, path, evidence.VerdictQualified)
	})
}
