package qualification

import (
	"testing"
)

// TestSemanticGradeDerivation confirms the per-Capability baseline
// derivation: each of the five disposition Cases and each of the four
// retry Cases is independent, an unobserved Case lowers only its owning
// Capability, and observed-but-conflated Cases Grade gap.
func TestSemanticGradeDerivation(T *testing.T) {
	T.Parallel()

	dispositionCases := CapabilityCases[CapabilityTurnDisposition]
	retryCases := CapabilityCases[CapabilityRetryClassification]

	allUsable := func(Cases []Case, skipped Case) []Grade {
		classes := make([]Grade, 0, len(Cases))
		for _, caseID := range Cases {
			if caseID == skipped {
				classes = append(classes, GradeNotObserved)
				continue
			}
			classes = append(classes, GradeUsable)
		}
		return classes
	}

	T.Run("each unobserved disposition Case lowers only turn_disposition", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range dispositionCases {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				got := DeriveBaselineGrade(allUsable(dispositionCases, caseID))
				if got != GradeNotObserved {
					T.Errorf("DeriveBaselineGrade(disposition with %s not observed) = %s, want not_observed", caseID, got)
				}
				retryGot := DeriveBaselineGrade(allUsable(retryCases, ""))
				if retryGot != GradeUsable {
					T.Errorf("DeriveBaselineGrade(retry Cases) = %s, want usable; the retry baseline must not move with a disposition Case", retryGot)
				}
			})
		}
	})

	T.Run("each unobserved retry Case lowers only retry_classification", func(T *testing.T) {
		T.Parallel()

		for _, caseID := range retryCases {
			T.Run(string(caseID), func(T *testing.T) {
				T.Parallel()

				got := DeriveBaselineGrade(allUsable(retryCases, caseID))
				if got != GradeNotObserved {
					T.Errorf("DeriveBaselineGrade(retry with %s not observed) = %s, want not_observed", caseID, got)
				}
				dispositionGot := DeriveBaselineGrade(allUsable(dispositionCases, ""))
				if dispositionGot != GradeUsable {
					T.Errorf("DeriveBaselineGrade(disposition Cases) = %s, want usable; the disposition baseline must not move with a retry Case", dispositionGot)
				}
			})
		}
	})

	T.Run("observed Case classifications decide usable versus gap", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name            string
			classifications []Grade
			want            Grade
		}{
			{name: "all usable Grades usable", classifications: []Grade{
				GradeUsable, GradeUsable, GradeUsable,
			}, want: GradeUsable},
			{name: "one conflated Case Grades gap", classifications: []Grade{
				GradeUsable, GradeGap, GradeUsable,
			}, want: GradeGap},
			{name: "only conflated Cases Grade gap", classifications: []Grade{
				GradeGap, GradeGap,
			}, want: GradeGap},
			{name: "not_observed dominates gap", classifications: []Grade{
				GradeGap, GradeNotObserved,
			}, want: GradeNotObserved},
		}

		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				got := DeriveBaselineGrade(tt.classifications)
				if got != tt.want {
					T.Errorf("DeriveBaselineGrade(%v) = %s, want %s", tt.classifications, got, tt.want)
				}
			})
		}
	})

	T.Run("the written variants carry the derived Grades end to end", func(T *testing.T) {
		T.Parallel()

		qualified := NewFixture(FixtureQualified)
		qualified.Finalize()
		textDisposition := qualified.FindFirst(MatchBaseline(SurfaceNativeText, CapabilityTurnDisposition))
		if textDisposition == nil {
			T.Fatal("fixture carries no native_text disposition baseline")
		}
		if textDisposition.Grade != GradeGap {
			T.Errorf("native_text disposition baseline = %s, want gap for unstructured residue", textDisposition.Grade)
		}

		notQualified := NewFixture(FixtureNotQualified)
		notQualified.Finalize()
		protocolDisposition := notQualified.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		protocolRetry := notQualified.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityRetryClassification))
		if protocolDisposition == nil || protocolRetry == nil {
			T.Fatal("fixture carries no protocol baselines")
		}
		if protocolDisposition.Grade != GradeNotObserved {
			T.Errorf("not_qualified protocol disposition baseline = %s, want not_observed", protocolDisposition.Grade)
		}
		if protocolRetry.Grade != GradeNotObserved {
			T.Errorf("not_qualified protocol retry baseline = %s, want not_observed", protocolRetry.Grade)
		}
	})
}

// TestRichestNativeReference confirms the reference derivation:
// the higher observed Grade of native JSON and native stream-JSON per
// Capability, with any not_observed Surface forcing the reference to
// not_observed, and the native text Surface excluded entirely.
func TestRichestNativeReference(T *testing.T) {
	T.Parallel()

	T.Run("Grade combination", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name          string
			jsonGrade     Grade
			streamGrade   Grade
			wantReference Grade
		}{
			{name: "two usable Surfaces give a usable reference", jsonGrade: GradeUsable, streamGrade: GradeUsable, wantReference: GradeUsable},
			{name: "one usable and one gap Surface give a usable reference", jsonGrade: GradeUsable, streamGrade: GradeGap, wantReference: GradeUsable},
			{name: "two gap Surfaces give a gap reference", jsonGrade: GradeGap, streamGrade: GradeGap, wantReference: GradeGap},
			{name: "a gap Surface does not outrank a usable one", jsonGrade: GradeGap, streamGrade: GradeUsable, wantReference: GradeUsable},
			{name: "one unobserved Surface forces not_observed over usable", jsonGrade: GradeUsable, streamGrade: GradeNotObserved, wantReference: GradeNotObserved},
			{name: "one unobserved Surface forces not_observed over gap", jsonGrade: GradeGap, streamGrade: GradeNotObserved, wantReference: GradeNotObserved},
			{name: "two unobserved Surfaces give not_observed", jsonGrade: GradeNotObserved, streamGrade: GradeNotObserved, wantReference: GradeNotObserved},
		}

		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				got := richestNativeReference(tt.jsonGrade, tt.streamGrade)
				if got != tt.wantReference {
					T.Errorf("richestNativeReference(%s, %s) = %s, want %s", tt.jsonGrade, tt.streamGrade, got, tt.wantReference)
				}
			})
		}
	})

	T.Run("numeric Grades", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name       string
			Grade      Grade
			wantGrade  int
			wantRanked bool
		}{
			{name: "usable outranks gap", Grade: GradeUsable, wantGrade: 1, wantRanked: true},
			{name: "gap is the zero Grade", Grade: GradeGap, wantGrade: 0, wantRanked: true},
			{name: "not_observed carries no Grade", Grade: GradeNotObserved, wantRanked: false},
			{name: "corroboration_only carries no Grade and never reaches 1", Grade: GradeCorroborationOnly, wantRanked: false},
		}

		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				got, ranked := numericGrade(tt.Grade)
				if ranked != tt.wantRanked || (ranked && got != tt.wantGrade) {
					T.Errorf("numericGrade(%s) = %d, %v, want %d, %v", tt.Grade, got, ranked, tt.wantGrade, tt.wantRanked)
				}
			})
		}
	})

	T.Run("an unobserved structured Surface blocks qualification", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceNativeStreamJSON, true)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictNotQualified)
	})

	T.Run("the native text Surface is excluded from the structured reference", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		before := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, before, VerdictQualified)

		for _, Capability := range comparisonCapabilities[:2] {
			for _, caseID := range CapabilityCases[Capability] {
				rec := fixture.FindFirst(MatchSemantic(SurfaceNativeText, Capability, caseID))
				if rec == nil {
					T.Fatalf("fixture carries no native_text Record for %s %s", Capability, caseID)
				}
				rec.Grade = GradeUsable
			}
			fixture.UpdateSemanticBaseline(SurfaceNativeText, Capability)
		}
		fixture.Renumber()
		after := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, after, VerdictQualified)
	})
}

// TestEligibilityPredicates confirms the exact Verdict Outcomes:
// the complete set of predicates yields qualified, and breaking any one
// of them yields not_qualified from an otherwise valid evidence set.
func TestEligibilityPredicates(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name   string
		mutate func(*Fixture)
	}{
		{
			name:   "all predicates hold yields qualified",
			mutate: func(*Fixture) {},
		},
		{
			name: "protocol token Grade below the richest native reference",
			mutate: func(f *Fixture) {
				f.SetTokenCorroborationOnly(SurfaceProtocol)
			},
		},
		{
			name: "tool server delivery without a server receipt",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowMCPDelivery))
				rec.Grade = GradeGap
			},
		},
		{
			name: "permission request the adapter leaves unanswered",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowPermission))
				rec.Outcome = OutcomeAdapterUnanswered
				rec.Grade = GradeNotObserved
			},
		},
		{
			name: "policy precondition induction failure",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowPolicyPrecondition))
				rec.Outcome = OutcomeFixtureInductionFailed
				rec.Grade = GradeNotObserved
			},
		},
		{
			name: "end-to-end run short of its terminal condition",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowEndToEnd))
				rec.Outcome = OutcomeNotObserved
				rec.Grade = GradeNotObserved
			},
		},
		{
			name: "protocol continuation below a usable native reference",
			mutate: func(f *Fixture) {
				recall := f.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
				recall.SessionID = new(FixtureSession(SurfaceProtocol, "fallback"))
				recall.Detail = recallFreshFallback
				recall.Grade = GradeGap
				if baseline := f.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation)); baseline != nil {
					baseline.Grade = GradeGap
				}
			},
		},
	}

	for _, tt := range tests {
		T.Run(tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := NewFixture(FixtureQualified)
			tt.mutate(fixture)
			fixture.Finalize()
			path := WriteEvidenceFile(T, fixture.Records)
			want := VerdictQualified
			if tt.name != "all predicates hold yields qualified" {
				want = VerdictNotQualified
			}
			RequireObservationVerdict(T, path, want)
		})
	}
}
