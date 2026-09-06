package qualification

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// matchRowClass matches records of one row class.
func matchRowClass(class RowClass) func(*Record) bool {
	return func(rec *Record) bool {
		got, err := ClassifyRecord(rec)
		return err == nil && got == class
	}
}

// tokenRecordCount counts the token-inventory records of a set.
func tokenRecordCount(records []Record) int {
	count := 0
	for i := range records {
		if records[i].Scenario == ScenarioTokenSource {
			count++
		}
	}
	return count
}

// protocolSessionCount counts the distinct non-null actual protocol
// session ids a Record set references.
func protocolSessionCount(records []Record) int {
	ids := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			ids[*rec.SessionID] = true
		}
	}
	return len(ids)
}

func TestValidatorMissingAndDuplicateControls(T *testing.T) {
	T.Parallel()

	pickers := []struct {
		name string
		pick func(*Fixture) *Record
	}{
		{"workspace security", func(f *Fixture) *Record {
			return f.FindFirst(matchRowClass(RowWorkspaceSecurity))
		}},
		{"policy precondition", func(f *Fixture) *Record {
			return f.FindFirst(matchRowClass(RowPolicyPrecondition))
		}},
		{"permission handling", func(f *Fixture) *Record { return f.FindFirst(matchRowClass(RowPermission)) }},
		{"tool server delivery", func(f *Fixture) *Record { return f.FindFirst(matchRowClass(RowMCPDelivery)) }},
		{"process cleanup", func(f *Fixture) *Record {
			return f.FindFirst(matchRowClass(RowProcessCleanup))
		}},
		{"end to end", func(f *Fixture) *Record { return f.FindFirst(matchRowClass(RowEndToEnd)) }},
		{"disposition semantic", func(f *Fixture) *Record {
			return f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess))
		}},
		{"retry semantic", func(f *Fixture) *Record {
			return f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseRetryableTransport))
		}},
		{"Surface baseline", func(f *Fixture) *Record {
			return f.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		}},
		{"token inventory", func(f *Fixture) *Record {
			return f.FindFirst(matchTokenSurface(SurfaceNativeJSON))
		}},
		{"continuation seed", func(f *Fixture) *Record {
			return f.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationSeed))
		}},
		{"continuation recall", func(f *Fixture) *Record {
			return f.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
		}},
		{"runtime identity", func(f *Fixture) *Record {
			return f.FindFirst(matchRowClass(RowRuntimeIdentity))
		}},
	}

	for _, tt := range pickers {
		T.Run("missing "+tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := NewFixture(FixtureQualified)
			fixture.Finalize()
			target := tt.pick(fixture)
			if target == nil {
				T.Fatalf("fixture carries no %s Record to remove", tt.name)
			}
			fixture.Remove(target)
			fixture.Renumber()
			path := WriteEvidenceFile(T, fixture.Records)
			if _, err := ValidateObservations(path); err == nil {
				T.Errorf("ValidateObservations(%s) = nil error, want rejection when the %s Record is missing", path, tt.name)
			}
		})

		T.Run("duplicate "+tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := NewFixture(FixtureQualified)
			fixture.Finalize()
			target := tt.pick(fixture)
			if target == nil {
				T.Fatalf("fixture carries no %s Record to duplicate", tt.name)
			}
			fixture.DuplicateAfter(target)
			fixture.Renumber()
			path := WriteEvidenceFile(T, fixture.Records)
			_, err := ValidateObservations(path)
			if err == nil {
				T.Errorf("ValidateObservations(%s) = nil error, want rejection when the %s Record is duplicated", path, tt.name)
			} else if !strings.Contains(err.Error(), "duplicate") {
				T.Errorf("ValidateObservations() error = %v, want it to report a duplicate", err)
			}
		})
	}
}

// TestValidatorSemanticControls covers the missing-Case, ordering,
// cross-class, and baseline-derivation controls for the semantic rows.
func TestValidatorSemanticControls(T *testing.T) {
	T.Parallel()

	T.Run("missing disposition Cases", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range CapabilityCases[CapabilityTurnDisposition] {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				fixture := NewFixture(FixtureQualified)
				fixture.Finalize()
				target := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, caseID))
				if target == nil {
					T.Fatalf("fixture carries no protocol disposition Record for Case %s", caseID)
				}
				fixture.Remove(target)
				fixture.Renumber()
				path := WriteEvidenceFile(T, fixture.Records)
				if _, err := ValidateObservations(path); err == nil {
					T.Errorf("ValidateObservations() = nil error, want rejection when disposition Case %s is missing", caseID)
				}
			})
		}
	})

	T.Run("missing retry Cases", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range CapabilityCases[CapabilityRetryClassification] {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				fixture := NewFixture(FixtureQualified)
				fixture.Finalize()
				target := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, caseID))
				if target == nil {
					T.Fatalf("fixture carries no protocol retry Record for Case %s", caseID)
				}
				fixture.Remove(target)
				fixture.Renumber()
				path := WriteEvidenceFile(T, fixture.Records)
				if _, err := ValidateObservations(path); err == nil {
					T.Errorf("ValidateObservations() = nil error, want rejection when retry Case %s is missing", caseID)
				}
			})
		}
	})

	T.Run("wrong disposition order", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		first := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess))
		second := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeFailure))
		if first == nil || second == nil {
			T.Fatal("fixture carries no protocol disposition pair to swap")
		}
		swapRecords(fixture, first, second)
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when disposition Cases are out of order")
		}
	})

	T.Run("wrong retry order", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		first := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseRetryableTransport))
		second := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseHumanInput))
		if first == nil || second == nil {
			T.Fatal("fixture carries no protocol retry pair to swap")
		}
		swapRecords(fixture, first, second)
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when retry Cases are out of order")
		}
	})

	T.Run("disposition tuple carrying a retry Case", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess))
		if rec == nil {
			T.Fatal("fixture carries no protocol success Record")
		}
		rec.SemanticCase = new(CaseRetryableTransport)
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a disposition tuple carrying a retry Case")
		}
	})

	T.Run("retry tuple carrying a disposition Case", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		rec := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseRetryableTransport))
		if rec == nil {
			T.Fatal("fixture carries no protocol retryable Record")
		}
		rec.SemanticCase = new(CaseSuccess)
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a retry tuple carrying a disposition Case")
		}
	})

	T.Run("baseline Grade written before its own Cases", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		if baseline == nil {
			T.Fatal("fixture carries no protocol disposition baseline")
		}
		firstSemantic := 0
		for i := range fixture.Records {
			if fixture.Records[i].Scenario == ScenarioSemanticProbe {
				firstSemantic = i
				break
			}
		}
		fixture.Remove(baseline)
		fixture.Records = slices.Insert(fixture.Records, firstSemantic, *baseline)
		fixture.Renumber()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a baseline precedes its own Cases")
		}
	})

	T.Run("written baseline Grade disagrees with its Cases", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		if baseline == nil {
			T.Fatal("fixture carries no protocol disposition baseline")
		}
		baseline.Grade = GradeGap
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a baseline Grade that differs from its derivation")
		}
	})

	T.Run("unobserved retry Case lowers only retry classification, to an unmeasured verdict", func(T *testing.T) {
		T.Parallel()

		// The retry classification row was never measured on the
		// protocol surface, so it now reports the unmeasured verdict
		// its own not_observed baseline produces, rather than the
		// not_qualified a measured failure would produce.
		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityRetryClassification, CaseUnknownOutcome)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictUnmeasured)

		retryBaseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityRetryClassification))
		dispositionBaseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		if retryBaseline == nil || dispositionBaseline == nil {
			T.Fatal("fixture carries no protocol baselines")
		}
		if retryBaseline.Grade != GradeNotObserved {
			T.Errorf("retry baseline = %s, want not_observed", retryBaseline.Grade)
		}
		if dispositionBaseline.Grade != GradeUsable {
			T.Errorf("disposition baseline = %s, want usable to stay untouched", dispositionBaseline.Grade)
		}
	})
}

// swapRecords exchanges the slice positions of two records pointed
// to inside the fixture and renumbers.
func swapRecords(fixture *Fixture, a, b *Record) {
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

// TestValidatorTokenControls covers the token inventory controls:
// valid sentinels, missing inventories, duplicate paths, doubled
// sentinels, mixed sentinel shapes, and baseline disagreement.
func TestValidatorTokenControls(T *testing.T) {
	T.Parallel()

	T.Run("zero-Source sentinel is valid", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceNativeJSON, false)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictQualified)
	})

	T.Run("failed inventory sentinel is valid and unmeasured", func(T *testing.T) {
		T.Parallel()

		// A failed token inventory means the protocol token_ceiling
		// baseline was never measured, so the row is unmeasured rather
		// than a measured failure.
		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceProtocol, true)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictUnmeasured)
	})

	T.Run("missing Surface inventory", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(matchTokenSurface(SurfaceNativeJSON))
		fixture.Renumber()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a Surface has no token inventory")
		}
	})

	T.Run("duplicate Surface and evidence path", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		target := fixture.FindFirst(matchTokenSurface(SurfaceNativeJSON))
		if target == nil {
			T.Fatal("fixture carries no native_json token Record")
		}
		fixture.DuplicateAfter(target)
		fixture.Renumber()
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a duplicate token key")
		} else if !strings.Contains(err.Error(), "duplicate") {
			T.Errorf("ValidateObservations() error = %v, want it to report a duplicate", err)
		}
	})

	T.Run("two sentinels on one Surface", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceNativeJSON, false)
		fixture.Finalize()
		sentinel := fixture.FindFirst(matchTokenSurface(SurfaceNativeJSON))
		if sentinel == nil {
			T.Fatal("fixture carries no native_json sentinel")
		}
		fixture.DuplicateAfter(sentinel)
		fixture.Renumber()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when a Surface carries two token sentinels")
		}
	})

	T.Run("sentinel and non-sentinel on one Surface", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		sentinel := sentinelFixtureRecord(fixture, SurfaceProtocol)
		insertAt := 0
		for i := range fixture.Records {
			if matchTokenSurface(SurfaceProtocol)(&fixture.Records[i]) {
				insertAt = i
				break
			}
		}
		fixture.Records = slices.Insert(fixture.Records, insertAt, sentinel)
		fixture.Renumber()
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a sentinel beside non-sentinel records")
		} else if !strings.Contains(err.Error(), "sentinel") {
			T.Errorf("ValidateObservations() error = %v, want it to name the sentinel conflict", err)
		}
	})

	T.Run("token baseline disagrees with inventory", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTokenCeiling))
		if baseline == nil {
			T.Fatal("fixture carries no protocol token baseline")
		}
		baseline.Grade = GradeGap
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a token baseline inconsistent with its inventory")
		}
	})
}

// sentinelFixtureRecord builds a valid zero-Source sentinel Record for one
// Surface, for controls that append it to a completed inventory.
func sentinelFixtureRecord(fixture *Fixture, Surface Surface) Record {
	sentinel := fixture.base()
	sentinel.Scenario = ScenarioTokenSource
	sentinel.Surface = Surface
	sentinel.Capability = CapabilityTokenCeiling
	sentinel.Source = SourceNone
	sentinel.Grade = GradeGap
	sentinel.Outcome = OutcomePass
	sentinel.InputID = InputTokenInventory
	sentinel.Detail = "inventory completed with no token-bearing path"
	return sentinel
}

// TestValidatorContinuationControls covers the continuation
// session-relation controls and both positive shapes.
func TestValidatorContinuationControls(T *testing.T) {
	T.Parallel()

	T.Run("same-id confirmed continuation is positive", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictQualified)
	})

	T.Run("different-id fresh fallback with identity for both ids is positive", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall Record")
		}
		recall.SessionID = new(FixtureSession(SurfaceProtocol, "fallback"))
		recall.Detail = recallFreshFallback
		recall.Grade = GradeGap
		if baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = GradeGap
		}
		fixture.AppendIdentity(*recall.SessionID)
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictNotQualified)
	})

	T.Run("dangling prior session id", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall Record")
		}
		recall.PriorSessionID = new(FixtureSession(SurfaceProtocol, "dangling"))
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a dangling prior_session_id")
		}
	})

	T.Run("prior id on a forbidden Record", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		seed := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationSeed))
		if seed == nil {
			T.Fatal("fixture carries no protocol seed Record")
		}
		seed.PriorSessionID = new(FixtureSession(SurfaceProtocol, "seed"))
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a prior id on a non-recall Record")
		}
	})

	T.Run("missing runtime identity for the fallback actual id", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
		if recall == nil {
			T.Fatal("fixture carries no protocol recall Record")
		}
		recall.SessionID = new(FixtureSession(SurfaceProtocol, "fallback"))
		recall.Detail = recallFreshFallback
		recall.Grade = GradeGap
		if baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = GradeGap
		}
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection when the fallback actual id lacks a runtime identity record")
		} else if !strings.Contains(err.Error(), "runtime identity record") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing identity record", err)
		}
	})

	T.Run("a runtime identity for a session no other evidence references", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		fixture.AppendIdentity("sess-referenced-by-nothing")
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of an identity record no other evidence references")
		} else if !strings.Contains(err.Error(), "references no non-final evidence") {
			T.Errorf("ValidateObservations() error = %v, want it to name the unreferenced identity record", err)
		}
	})
}

// TestValidatorCrossSurfacePriorSessionControl pins the separate
// rejection of a recall whose prior_session_id resolves to a seed on
// another Surface.
func TestValidatorCrossSurfacePriorSessionControl(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
	nativeSeed := fixture.FindFirst(MatchContinuation(SurfaceNativeText, InputContinuationSeed))
	if recall == nil || nativeSeed == nil {
		T.Fatal("fixture carries no protocol recall or native_text seed Record")
	}
	if nativeSeed.SessionID == nil {
		T.Fatal("native_text seed carries no session id")
	}
	recall.PriorSessionID = new(*nativeSeed.SessionID)
	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path)
	if err == nil {
		T.Error("ValidateObservations() = nil error, want rejection of a cross-Surface prior_session_id")
	} else if !strings.Contains(err.Error(), "does not resolve to that surface's seed session") {
		T.Errorf("ValidateObservations() error = %v, want a same-surface resolution failure", err)
	}
}

// TestValidatorFinalTupleControls covers the two-pass aggregate
// rules: no qualification Record in the first pass, exactly one in the
// final pass, nothing after it, and exact equality with recomputation.
func TestValidatorFinalTupleControls(T *testing.T) {
	T.Parallel()

	T.Run("qualification tuple in the first pass", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		withAggregate := append(slices.Clone(fixture.Records), aggregateFixtureRecord(GradeQualified))
		for i := range withAggregate {
			withAggregate[i].Sequence = i + 1
		}
		path := WriteEvidenceFile(T, withAggregate)
		if _, err := ValidateObservations(path); err == nil {
			T.Error("ValidateObservations() = nil error, want rejection of a qualification tuple in the first pass")
		}
	})

	T.Run("final pass whose aggregate carries a foreign tuple", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		aggregate := aggregateFixtureRecord(GradeQualified)
		aggregate.Capability = CapabilityWorkspaceSecurity
		withAggregate := append(slices.Clone(fixture.Records), aggregate)
		for i := range withAggregate {
			withAggregate[i].Sequence = i + 1
		}
		path := WriteEvidenceFile(T, withAggregate)
		if _, err := ValidateEvidence(path); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of an aggregate outside the closed tuple")
		} else if !strings.Contains(err.Error(), "tuple") {
			T.Errorf("ValidateEvidence() error = %v, want it to name the rejected tuple", err)
		}
	})

	T.Run("final pass without the aggregate", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateEvidence(path); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection when the aggregate is missing")
		} else if !errors.Is(err, errFinalRecordMissing) {
			T.Errorf("ValidateEvidence() error = %v, want the missing-aggregate cause", err)
		}
	})

	T.Run("final pass with two aggregates", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		withAggregates := append(slices.Clone(fixture.Records),
			aggregateFixtureRecord(GradeQualified),
			aggregateFixtureRecord(GradeQualified))
		for i := range withAggregates {
			withAggregates[i].Sequence = i + 1
		}
		path := WriteEvidenceFile(T, withAggregates)
		if _, err := ValidateEvidence(path); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of two aggregate records")
		} else if !errors.Is(err, errFinalRecordDuplicated) {
			T.Errorf("ValidateEvidence() error = %v, want the duplicated-aggregate cause", err)
		}
	})

	T.Run("Record after the aggregate", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		stray := fixture.FindFirst(matchRowClass(RowProcessCleanup))
		if stray == nil {
			T.Fatal("fixture carries no cleanup Record to copy")
		}
		withStray := append(slices.Clone(fixture.Records),
			aggregateFixtureRecord(GradeQualified), *stray)
		for i := range withStray {
			withStray[i].Sequence = i + 1
		}
		path := WriteEvidenceFile(T, withStray)
		if _, err := ValidateEvidence(path); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of a Record after the aggregate")
		} else if !errors.Is(err, errRecordAfterAggregate) {
			T.Errorf("ValidateEvidence() error = %v, want the Record-after-aggregate cause", err)
		}
	})

	T.Run("final Verdict differs from recomputation", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		path := WriteFinalEvidenceFile(T, fixture.Records, GradeNotQualified)
		if _, err := ValidateEvidence(path); err == nil {
			T.Error("ValidateEvidence() = nil error, want rejection of a mismatched final Verdict")
		}
	})

	T.Run("complete qualified final file validates", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		tokenCount := tokenRecordCount(fixture.Records)
		sessionCount := protocolSessionCount(fixture.Records)
		if tokenCount < 4 {
			T.Fatalf("fixture token count = %d, want at least 4", tokenCount)
		}
		if got := len(fixture.Records); got != 66+tokenCount+sessionCount {
			T.Fatalf("fixture Record count = %d, want 66+tokenCount+sessionCount = %d", got, 66+tokenCount+sessionCount)
		}
		path := WriteFinalEvidenceFile(T, fixture.Records, GradeQualified)
		RequireFinalVerdict(T, path, VerdictQualified)
	})
}

// declareSemanticGap rewrites one semantic record in place to a
// declared_gap grade with the given reason, leaving its session_id and
// evidence_path as the fixture originally built them, matching the
// rule that a declared_gap row's session follows the observation.
func declareSemanticGap(f *Fixture, surface Surface, caseID Case, reason string) {
	rec := f.FindFirst(MatchSemantic(surface, CapabilityTurnDisposition, caseID))
	rec.Grade = GradeDeclaredGap
	rec.Outcome = OutcomeNotProducible
	rec.Detail = reason
}

// declareNotInducible rewrites one semantic record in place to a
// not_inducible grade, nulling its session_id and evidence_path per
// the closed rule for that grade.
func declareNotInducible(f *Fixture, surface Surface, capability Capability, caseID Case) {
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	rec.Grade = GradeNotInducible
	rec.Outcome = OutcomeNotInducible
	rec.Detail = NotInducibleDetail
	rec.EvidencePath = nil
	rec.SessionID = nil
}

// TestValidatorExcludedCaseControls covers checkExcludedCases: one
// rejection control per clause, built from a qualified fixture mutated
// with declareSemanticGap and declareNotInducible.
func TestValidatorExcludedCaseControls(T *testing.T) {
	T.Parallel()

	T.Run("both a declared gap and a not-inducible grade on one Case", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, SurfaceProtocol, CaseCancellation, DeclaredGapNeverProduced)
		declareNotInducible(fixture, SurfaceNativeJSON, CapabilityTurnDisposition, CaseCancellation)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of both exclusion kinds on one case")
		}
		if !strings.Contains(err.Error(), "carries both a declared gap and a not-inducible grade") {
			T.Errorf("ValidateObservations() error = %v, want it to name the conflicting exclusion kinds", err)
		}
	})

	T.Run("declared set is not exactly DeclarableSurfaces", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, SurfaceProtocol, CaseRuntimeRefusal, DeclaredGapNeverProduced)
		declareSemanticGap(fixture, SurfaceNativeJSON, CaseRuntimeRefusal, DeclaredGapNeverProduced)
		// native_stream_json intentionally left observed, so the
		// declared set is incomplete.
		fixture.UpdateSemanticBaseline(SurfaceProtocol, CapabilityTurnDisposition)
		fixture.UpdateSemanticBaseline(SurfaceNativeJSON, CapabilityTurnDisposition)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an incomplete declared set")
		}
		if !strings.Contains(err.Error(), "declared gap is missing on surfaces") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing surfaces", err)
		}
	})

	T.Run("catalog set is not exactly every measured surface", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareNotInducible(fixture, SurfaceProtocol, CapabilityRetryClassification, CaseHumanInput)
		declareNotInducible(fixture, SurfaceNativeJSON, CapabilityRetryClassification, CaseHumanInput)
		declareNotInducible(fixture, SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseHumanInput)
		// native_text intentionally left observed: not_inducible must
		// cover every measured surface, not only the declarable three.
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of an incomplete not-inducible catalog set")
		}
		if !strings.Contains(err.Error(), "not-inducible grade is missing on surfaces") {
			T.Errorf("ValidateObservations() error = %v, want it to name the missing surfaces", err)
		}
	})

	T.Run("mixed detail within one declared set", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, SurfaceProtocol, CaseCancellation, DeclaredGapNeverProduced)
		declareSemanticGap(fixture, SurfaceNativeJSON, CaseCancellation, DeclaredGapNeverProduced)
		declareSemanticGap(fixture, SurfaceNativeStreamJSON, CaseCancellation, DeclaredGapFolded)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a non-uniform declared-gap detail")
		}
		if !strings.Contains(err.Error(), "differing declared-gap details") {
			T.Errorf("ValidateObservations() error = %v, want it to name the differing details", err)
		}
	})

	T.Run("declared record no declaration authorizes", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareSemanticGap(fixture, SurfaceProtocol, CaseCancellation, DeclaredGapNeverProduced)
		declareSemanticGap(fixture, SurfaceNativeJSON, CaseCancellation, DeclaredGapNeverProduced)
		declareSemanticGap(fixture, SurfaceNativeStreamJSON, CaseCancellation, DeclaredGapNeverProduced)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, DeclaredGapSet{})
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of an unauthorized declared record")
		}
		if !strings.Contains(err.Error(), "no declaration authorizes") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the unauthorized record", err)
		}
	})

	T.Run("a declaration no record matches", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		declarations := DeclaredGapSet{
			SchemaVersion: 1,
			Declarations:  []DeclaredGap{{Capability: CapabilityTurnDisposition, Case: CaseRuntimeRefusal, Reason: DeclaredGapNeverProduced}},
		}
		_, err := ValidateObservationsWithDeclarations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of an unmatched declaration")
		}
		if !strings.Contains(err.Error(), "matches no declared_gap record") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the unmatched declaration", err)
		}
	})

	T.Run("a peer whose declared set differs from DeclarableSurfaces", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		fixture.Finalize()
		declarations := fixture.Declarations()
		// Revert the peer's native_stream_json record back to observed,
		// leaving CaseRuntimeRefusal itself fully and uniformly declared.
		peer := fixture.FindFirst(MatchSemantic(SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseNonRetryableRefusal))
		peer.Grade = GradeUsable
		peer.Outcome = OutcomePass
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of a peer whose declared set is incomplete")
		}
		if !strings.Contains(err.Error(), "peer") || !strings.Contains(err.Error(), "does not carry a declared gap on surfaces") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the peer's incomplete declared set", err)
		}
	})

	T.Run("a peer whose reason differs", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		fixture.Finalize()
		declarations := fixture.Declarations()
		for _, surface := range DeclarableSurfaces {
			rec := fixture.FindFirst(MatchSemantic(surface, CapabilityRetryClassification, CaseNonRetryableRefusal))
			rec.Detail = DeclaredGapFolded
		}
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of a peer reason mismatch")
		}
		if !strings.Contains(err.Error(), "differing declared-gap reasons") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the differing reasons", err)
		}
	})
}

// TestValidatorAllExcludedBaselineControl covers checkDerivedBaselines'
// rejection of a surface-capability whose every case is excluded,
// leaving no case to derive a baseline from.
func TestValidatorAllExcludedBaselineControl(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	for _, surface := range MeasuredSurfaces {
		declareNotInducible(fixture, surface, CapabilityTurnDisposition, CaseRuntimeRefusal)
		for _, caseID := range CapabilityCases[CapabilityRetryClassification] {
			declareNotInducible(fixture, surface, CapabilityRetryClassification, caseID)
		}
	}
	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path)
	if err == nil {
		T.Fatal("ValidateObservations() = nil error, want rejection of a surface-capability with every case excluded")
	}
	if !strings.Contains(err.Error(), "has every case excluded") {
		T.Errorf("ValidateObservations() error = %v, want it to name the all-excluded surface and capability", err)
	}
}

// TestValidatorClassifyRecordsExcludedRowControls covers the per-record
// rejections ClassifyRecords, CheckOutcomeGradePairing, and
// checkSessionRelation enforce for the two excluded grades.
func TestValidatorClassifyRecordsExcludedRowControls(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Fixture)
		wantSub string
	}{
		{
			name: "declared_gap Grade on a non-semantic row class",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowPermission))
				rec.Grade = GradeDeclaredGap
				rec.Outcome = OutcomeNotProducible
			},
			wantSub: "is valid only on a semantic probe record",
		},
		{
			name: "not_inducible Grade on a non-semantic row class",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowMCPDelivery))
				rec.Grade = GradeNotInducible
				rec.Outcome = OutcomeNotInducible
			},
			wantSub: "is valid only on a semantic probe record",
		},
		{
			// The declared_gap check runs ahead of the generic
			// semantic-probe path check, so the operator who authored the
			// declaration reads which rule his record broke rather than a
			// message about semantic probes in general.
			name: "declared_gap record with a null evidence_path",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeDeclaredGap
				rec.Outcome = OutcomeNotProducible
				rec.Detail = DeclaredGapNeverProduced
				rec.EvidencePath = nil
			},
			wantSub: "declared_gap record must carry a non-null evidence_path",
		},
		{
			name: "not_inducible record with a non-null evidence_path",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeNotInducible
				rec.Outcome = OutcomeNotInducible
				rec.Detail = NotInducibleDetail
			},
			wantSub: "not_inducible record must carry a null evidence_path",
		},
		{
			name: "not_inducible record with a non-null session_id",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeNotInducible
				rec.Outcome = OutcomeNotInducible
				rec.Detail = NotInducibleDetail
				rec.EvidencePath = nil
			},
			wantSub: "not_inducible record must carry a null session_id",
		},
		{
			name: "declared_gap record with a null session_id",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeDeclaredGap
				rec.Outcome = OutcomeNotProducible
				rec.Detail = DeclaredGapNeverProduced
				rec.SessionID = nil
			},
			wantSub: "declared_gap record must carry its own session_id",
		},
		{
			name: "declared_gap Detail outside the closed reason set",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeDeclaredGap
				rec.Outcome = OutcomeNotProducible
				rec.Detail = "bogus_reason"
			},
			wantSub: "outside the closed reason set",
		},
		{
			name: "not_inducible Detail differs from the one closed value",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeNotInducible
				rec.Outcome = OutcomeNotInducible
				rec.Detail = "bogus_detail"
				rec.EvidencePath = nil
				rec.SessionID = nil
			},
			wantSub: "not_inducible record detail",
		},
		{
			name: "declared_gap Surface outside DeclarableSurfaces",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchSemantic(SurfaceNativeText, CapabilityTurnDisposition, CaseCancellation))
				rec.Grade = GradeDeclaredGap
				rec.Outcome = OutcomeNotProducible
				rec.Detail = DeclaredGapNeverProduced
			},
			wantSub: "must carry a surface in DeclarableSurfaces",
		},
		{
			name: "unmeasured Grade on a non-final row",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
				rec.Grade = GradeUnmeasured
			},
			wantSub: "is valid only for capability eligibility",
		},
	}

	for _, tt := range tests {
		T.Run(tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := NewFixture(FixtureQualified)
			fixture.Finalize()
			tt.mutate(fixture)
			path := WriteEvidenceFile(T, fixture.Records)
			_, err := ValidateObservations(path)
			if err == nil {
				T.Fatalf("ValidateObservations() = nil error, want rejection naming %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				T.Errorf("ValidateObservations() error = %v, want it to mention %q", err, tt.wantSub)
			}
		})
	}
}

// TestValidatorSemanticSessionRelationExemption covers the refusal/retry
// session-reuse rule: it still rejects a session mismatch whenever both
// peer records carry a session, and it exempts the comparison only when
// one side carries none, whatever that side's grade. The exemption must
// not silently widen beyond that one condition: a declared_gap peer
// pair still carries a session on both sides (checkSessionRelation
// requires it) and so is never exempt, and a not_observed peer that
// still carries a session is likewise never exempt, proving the skip
// keys on the absent session rather than on either grade.
func TestValidatorSemanticSessionRelationExemption(T *testing.T) {
	T.Parallel()

	T.Run("a session mismatch between two observed peers is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		retry := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal))
		retry.SessionID = new(FixtureSession(SurfaceProtocol, "diverged"))
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection of a session mismatch between two observed peers")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservations() error = %v, want the session-reuse cause", err)
		}
	})

	T.Run("a not_observed disposition peer is exempt even when the retry session diverges", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal)
		retry := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal))
		retry.SessionID = new(FixtureSession(SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil: a not_observed disposition peer carries no session to check", err)
		}
	})

	T.Run("a not_observed retry peer is exempt even when the disposition session diverges", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal)
		disposition := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal))
		disposition.SessionID = new(FixtureSession(SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservations(path); err != nil {
			T.Errorf("ValidateObservations() error = %v, want nil: a not_observed retry peer carries no session to check", err)
		}
	})

	T.Run("a session mismatch between two declared_gap peers is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		retry := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal))
		retry.SessionID = new(FixtureSession(SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, fixture.Declarations())
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of a session mismatch between two declared_gap peers: the exemption must not widen to a grade that still carries a session")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want the session-reuse cause", err)
		}
	})

	T.Run("a not_observed disposition peer that still carries a session is not exempt", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal)
		disposition := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeRefusal))
		disposition.SessionID = new(FixtureSession(SurfaceProtocol, "not-observed-but-sessioned"))
		retry := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityRetryClassification, CaseNonRetryableRefusal))
		retry.SessionID = new(FixtureSession(SurfaceProtocol, "diverged"))
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservations(path)
		if err == nil {
			T.Fatal("ValidateObservations() = nil error, want rejection: a not_observed peer that still carries a session must not be exempt, proving the skip keys on the absent session rather than on the not_observed grade")
		}
		if !strings.Contains(err.Error(), "does not reuse the matching disposition-refusal session id") {
			T.Errorf("ValidateObservations() error = %v, want the session-reuse cause", err)
		}
	})
}
