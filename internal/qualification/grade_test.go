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

	T.Run("declared_gap and not_inducible Cases are dropped before deriving", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name            string
			classifications []Grade
			want            Grade
		}{
			{name: "an excluded Case beside all-usable Cases still Grades usable", classifications: []Grade{
				GradeDeclaredGap, GradeUsable, GradeUsable,
			}, want: GradeUsable},
			{name: "an excluded Case beside a conflated Case still Grades gap", classifications: []Grade{
				GradeNotInducible, GradeGap, GradeUsable,
			}, want: GradeGap},
			{name: "an excluded Case beside an unobserved Case still Grades not_observed", classifications: []Grade{
				GradeDeclaredGap, GradeNotObserved, GradeUsable,
			}, want: GradeNotObserved},
			{name: "every Case excluded leaves an empty remainder that Grades not_observed", classifications: []Grade{
				GradeDeclaredGap, GradeNotInducible,
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

		// The redefined not_qualified variant conflates only the protocol
		// runtime_refusal disposition record; the retry classification
		// capability is untouched, so its protocol baseline stays usable
		// rather than moving with the disposition baseline.
		notQualified := NewFixture(FixtureNotQualified)
		notQualified.Finalize()
		protocolDisposition := notQualified.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
		protocolRetry := notQualified.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityRetryClassification))
		if protocolDisposition == nil || protocolRetry == nil {
			T.Fatal("fixture carries no protocol baselines")
		}
		if protocolDisposition.Grade != GradeGap {
			T.Errorf("not_qualified protocol disposition baseline = %s, want gap for the conflated refusal record", protocolDisposition.Grade)
		}
		if protocolRetry.Grade != GradeUsable {
			T.Errorf("not_qualified protocol retry baseline = %s, want usable to stay untouched by the redefined fixture", protocolRetry.Grade)
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

		// nativeReferenceStanding, not richestNativeReference directly, is
		// the reference derivation ExplainEligibility actually consults:
		// it checks each structured native Surface's own presence and rank
		// before ever calling richestNativeReference, so a not_observed
		// Surface is reported as an unmeasured standing naming the Surface
		// at fault, never as a bare not_observed Grade reaching the
		// comparison.
		tests := []struct {
			name           string
			jsonGrade      Grade
			streamGrade    Grade
			wantUnmeasured bool
			wantSurface    Surface
			wantReference  Grade
		}{
			{name: "two usable Surfaces give a usable reference", jsonGrade: GradeUsable, streamGrade: GradeUsable, wantReference: GradeUsable},
			{name: "one usable and one gap Surface give a usable reference", jsonGrade: GradeUsable, streamGrade: GradeGap, wantReference: GradeUsable},
			{name: "two gap Surfaces give a gap reference", jsonGrade: GradeGap, streamGrade: GradeGap, wantReference: GradeGap},
			{name: "a gap Surface does not outrank a usable one", jsonGrade: GradeGap, streamGrade: GradeUsable, wantReference: GradeUsable},
			{name: "one unobserved Surface forces an unmeasured standing over usable", jsonGrade: GradeUsable, streamGrade: GradeNotObserved, wantUnmeasured: true, wantSurface: SurfaceNativeStreamJSON},
			{name: "one unobserved Surface forces an unmeasured standing over gap", jsonGrade: GradeGap, streamGrade: GradeNotObserved, wantUnmeasured: true, wantSurface: SurfaceNativeStreamJSON},
			{name: "two unobserved Surfaces give an unmeasured standing naming the first", jsonGrade: GradeNotObserved, streamGrade: GradeNotObserved, wantUnmeasured: true, wantSurface: SurfaceNativeJSON},
		}

		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				grades := map[Surface]map[Capability]Grade{
					SurfaceNativeJSON:       {CapabilityTurnDisposition: tt.jsonGrade},
					SurfaceNativeStreamJSON: {CapabilityTurnDisposition: tt.streamGrade},
				}
				reference, surface, unmeasured := nativeReferenceStanding(grades, CapabilityTurnDisposition)
				if unmeasured != tt.wantUnmeasured {
					T.Fatalf("nativeReferenceStanding(json=%s, stream=%s) unmeasured = %v, want %v", tt.jsonGrade, tt.streamGrade, unmeasured, tt.wantUnmeasured)
				}
				if tt.wantUnmeasured {
					if surface != tt.wantSurface {
						T.Errorf("nativeReferenceStanding(json=%s, stream=%s) blamed Surface = %s, want %s", tt.jsonGrade, tt.streamGrade, surface, tt.wantSurface)
					}
					return
				}
				if reference != tt.wantReference {
					T.Errorf("nativeReferenceStanding(json=%s, stream=%s) = %s, want %s", tt.jsonGrade, tt.streamGrade, reference, tt.wantReference)
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

	T.Run("an unobserved structured Surface yields an unmeasured verdict", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenSentinel(SurfaceNativeStreamJSON, true)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		RequireObservationVerdict(T, path, VerdictUnmeasured)
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

// TestEligibilityPredicates confirms the exact Verdict outcomes: the
// complete set of predicates yields qualified, a measured failure on a
// load-bearing row yields not_qualified, and a row the run never
// measured yields unmeasured rather than being collapsed into
// not_qualified.
func TestEligibilityPredicates(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name   string
		mutate func(*Fixture)
		want   Verdict
	}{
		{
			name:   "all predicates hold yields qualified",
			mutate: func(*Fixture) {},
			want:   VerdictQualified,
		},
		{
			name: "protocol token Grade below the richest native reference",
			mutate: func(f *Fixture) {
				f.SetTokenCorroborationOnly(SurfaceProtocol)
			},
			want: VerdictNotQualified,
		},
		{
			name: "tool server delivery without a server receipt",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowMCPDelivery))
				rec.Grade = GradeGap
			},
			want: VerdictNotQualified,
		},
		{
			// The adapter never answers the permission request, so the
			// row was never measured, and it now reports the unmeasured
			// verdict its own not_observed grade produces.
			name: "permission request the adapter leaves unanswered",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowPermission))
				rec.Outcome = OutcomeAdapterUnanswered
				rec.Grade = GradeNotObserved
			},
			want: VerdictUnmeasured,
		},
		{
			// The policy precondition's own fixture induction failed
			// before the row could be measured at all.
			name: "policy precondition induction failure",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowPolicyPrecondition))
				rec.Outcome = OutcomeFixtureInductionFailed
				rec.Grade = GradeNotObserved
			},
			want: VerdictUnmeasured,
		},
		{
			// The end-to-end run never reached its terminal condition, so
			// the row carries no measurement to grade a failure from.
			name: "end-to-end run short of its terminal condition",
			mutate: func(f *Fixture) {
				rec := f.FindFirst(matchRowClass(RowEndToEnd))
				rec.Outcome = OutcomeNotObserved
				rec.Grade = GradeNotObserved
			},
			want: VerdictUnmeasured,
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
			want: VerdictNotQualified,
		},
	}

	for _, tt := range tests {
		T.Run(tt.name, func(T *testing.T) {
			T.Parallel()

			fixture := NewFixture(FixtureQualified)
			tt.mutate(fixture)
			fixture.Finalize()
			path := WriteEvidenceFile(T, fixture.Records)
			RequireObservationVerdict(T, path, tt.want)
		})
	}
}

// TestExplainEligibility confirms the three-way verdict derivation:
// ComputeEligibility agrees with ExplainEligibility on every input,
// each of the three verdicts is reachable from a constructed record
// set, a below standing takes precedence over an unmeasured standing
// in the same report, and a below standing survives a different Case
// of the same Capability being excluded.
func TestExplainEligibility(T *testing.T) {
	T.Parallel()

	T.Run("ComputeEligibility agrees with ExplainEligibility on every fixture variant", func(T *testing.T) {
		T.Parallel()

		for _, variant := range []string{FixtureQualified, FixtureNotQualified, FixtureUnmeasured} {
			T.Run(variant, func(T *testing.T) {
				T.Parallel()

				fixture := NewFixture(variant)
				report := ExplainEligibility(fixture.Records)
				if got := ComputeEligibility(fixture.Records); got != report.Verdict {
					T.Errorf("ComputeEligibility(%s) = %s, want ExplainEligibility(...).Verdict %s", variant, got, report.Verdict)
				}
			})
		}
	})

	T.Run("each of the three verdicts is reachable from a constructed record set", func(T *testing.T) {
		T.Parallel()

		tests := []struct {
			name    string
			variant string
			want    Verdict
		}{
			{"a fully measured fixture qualifies", FixtureQualified, VerdictQualified},
			{"a conflated protocol disposition Case is not_qualified", FixtureNotQualified, VerdictNotQualified},
			{"an unobserved protocol disposition Case is unmeasured", FixtureUnmeasured, VerdictUnmeasured},
		}
		for _, tt := range tests {
			T.Run(tt.name, func(T *testing.T) {
				T.Parallel()

				fixture := NewFixture(tt.variant)
				report := ExplainEligibility(fixture.Records)
				if report.Verdict != tt.want {
					T.Errorf("ExplainEligibility(%s).Verdict = %s, want %s", tt.variant, report.Verdict, tt.want)
				}
			})
		}
	})

	T.Run("a below standing takes precedence over an unmeasured standing in the same report", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetTokenCorroborationOnly(SurfaceProtocol)
		fixture.SetSemanticNotObserved(SurfaceProtocol, CapabilityRetryClassification, CaseUnknownOutcome)
		report := ExplainEligibility(fixture.Records)
		if report.Verdict != VerdictNotQualified {
			T.Fatalf("ExplainEligibility().Verdict = %s, want not_qualified when a below row and an unmeasured row both appear", report.Verdict)
		}

		var sawBelow, sawUnmeasured bool
		for _, row := range report.Rows {
			switch row.Standing {
			case StandingBelow:
				sawBelow = true
			case StandingUnmeasured:
				sawUnmeasured = true
			}
		}
		if !sawBelow || !sawUnmeasured {
			T.Errorf("report rows = %+v, want at least one below row and one unmeasured row", report.Rows)
		}
	})

	T.Run("a below standing survives a different Case of the same Capability being excluded", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureNotQualified)
		fixture.SetSemanticDeclaredGap(CapabilityTurnDisposition, CaseCancellation, DeclaredGapNeverProduced)
		report := ExplainEligibility(fixture.Records)
		if report.Verdict != VerdictNotQualified {
			T.Errorf("ExplainEligibility().Verdict = %s, want not_qualified: excluding a sibling Case must not mask the conflated refusal Case's below standing", report.Verdict)
		}
	})
}
