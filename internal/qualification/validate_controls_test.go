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

	T.Run("unobserved retry Case lowers only retry classification", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityRetryClassification, CaseUnknownOutcome)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictNotQualified)

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

	T.Run("failed inventory sentinel is valid and not qualified", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceProtocol, true)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictNotQualified)
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
