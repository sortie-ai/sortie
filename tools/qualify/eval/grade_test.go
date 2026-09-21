package eval

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func TestRichestNativeReference(t *testing.T) {
	t.Parallel()

	t.Run("grade combination", func(t *testing.T) {
		t.Parallel()

		// ExplainEligibility consults nativeReferenceStanding, not
		// richestNativeReference, so a not_observed surface reports an
		// unmeasured standing naming the surface.
		tests := []struct {
			name           string
			jsonGrade      evidence.Grade
			streamGrade    evidence.Grade
			wantUnmeasured bool
			wantSurface    evidence.Surface
			wantReference  evidence.Grade
		}{
			{name: "two usable surfaces give a usable reference", jsonGrade: evidence.GradeUsable, streamGrade: evidence.GradeUsable, wantReference: evidence.GradeUsable},
			{name: "one usable and one gap surface give a usable reference", jsonGrade: evidence.GradeUsable, streamGrade: evidence.GradeGap, wantReference: evidence.GradeUsable},
			{name: "two gap surfaces give a gap reference", jsonGrade: evidence.GradeGap, streamGrade: evidence.GradeGap, wantReference: evidence.GradeGap},
			{name: "a gap surface does not outrank a usable one", jsonGrade: evidence.GradeGap, streamGrade: evidence.GradeUsable, wantReference: evidence.GradeUsable},
			{name: "one unobserved surface forces an unmeasured standing over usable", jsonGrade: evidence.GradeUsable, streamGrade: evidence.GradeNotObserved, wantUnmeasured: true, wantSurface: evidence.SurfaceNativeStreamJSON},
			{name: "one unobserved surface forces an unmeasured standing over gap", jsonGrade: evidence.GradeGap, streamGrade: evidence.GradeNotObserved, wantUnmeasured: true, wantSurface: evidence.SurfaceNativeStreamJSON},
			{name: "two unobserved surfaces give an unmeasured standing naming the first", jsonGrade: evidence.GradeNotObserved, streamGrade: evidence.GradeNotObserved, wantUnmeasured: true, wantSurface: evidence.SurfaceNativeJSON},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				grades := map[evidence.Surface]map[evidence.Capability]evidence.Grade{
					evidence.SurfaceNativeJSON:       {evidence.CapabilityTurnDisposition: tt.jsonGrade},
					evidence.SurfaceNativeStreamJSON: {evidence.CapabilityTurnDisposition: tt.streamGrade},
				}
				reference, surface, standing := nativeReferenceStanding(grades, evidence.CapabilityTurnDisposition, []evidence.Surface{evidence.SurfaceNativeJSON, evidence.SurfaceNativeStreamJSON})
				unmeasured := standing == nativeReferenceIncomplete
				if unmeasured != tt.wantUnmeasured {
					t.Fatalf("nativeReferenceStanding(json=%s, stream=%s) unmeasured = %v, want %v", tt.jsonGrade, tt.streamGrade, unmeasured, tt.wantUnmeasured)
				}
				if tt.wantUnmeasured {
					if surface != tt.wantSurface {
						t.Errorf("nativeReferenceStanding(json=%s, stream=%s) blamed surface = %s, want %s", tt.jsonGrade, tt.streamGrade, surface, tt.wantSurface)
					}
					return
				}
				if reference != tt.wantReference {
					t.Errorf("nativeReferenceStanding(json=%s, stream=%s) = %s, want %s", tt.jsonGrade, tt.streamGrade, reference, tt.wantReference)
				}
			})
		}
	})

	t.Run("numeric grades", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name       string
			grade      evidence.Grade
			wantGrade  int
			wantRanked bool
		}{
			{name: "usable outranks gap", grade: evidence.GradeUsable, wantGrade: 1, wantRanked: true},
			{name: "gap is the zero grade", grade: evidence.GradeGap, wantGrade: 0, wantRanked: true},
			{name: "not_observed carries no grade", grade: evidence.GradeNotObserved, wantRanked: false},
			{name: "corroboration_only carries no grade and never reaches 1", grade: evidence.GradeCorroborationOnly, wantRanked: false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got, ranked := numericGrade(tt.grade)
				if ranked != tt.wantRanked || (ranked && got != tt.wantGrade) {
					t.Errorf("numericGrade(%s) = %d, %v, want %d, %v", tt.grade, got, ranked, tt.wantGrade, tt.wantRanked)
				}
			})
		}
	})

	t.Run("an unobserved structured surface yields an unmeasured verdict", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetTokenSentinel(evidence.SurfaceNativeStreamJSON, true)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		RequireObservationVerdict(t, path, evidence.VerdictUnmeasured)
	})
}

func TestEligibilityPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*evidencetest.Fixture)
		want   evidence.Verdict
	}{
		{
			name:   "all predicates hold yields qualified",
			mutate: func(*evidencetest.Fixture) {},
			want:   evidence.VerdictQualified,
		},
		{
			name: "protocol token grade below the richest native reference",
			mutate: func(f *evidencetest.Fixture) {
				f.SetTokenCorroborationOnly(evidence.SurfaceProtocol)
			},
			want: evidence.VerdictNotQualified,
		},
		{
			name: "tool server delivery without a server receipt",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowMCPDelivery))
				rec.Grade = evidence.GradeGap
			},
			want: evidence.VerdictNotQualified,
		},
		{
			// Never answered, so the row is unmeasured, not a failure.
			name: "permission request the adapter leaves unanswered",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowPermission))
				rec.Outcome = evidence.OutcomeAdapterUnanswered
				rec.Grade = evidence.GradeNotObserved
			},
			want: evidence.VerdictUnmeasured,
		},
		{
			// Induction failed before the row could be measured.
			name: "policy precondition induction failure",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowPolicyPrecondition))
				rec.Outcome = evidence.OutcomeFixtureInductionFailed
				rec.Grade = evidence.GradeNotObserved
			},
			want: evidence.VerdictUnmeasured,
		},
		{
			// Never reached its terminal condition, so there is no
			// measurement to grade a failure from.
			name: "end-to-end run short of its terminal condition",
			mutate: func(f *evidencetest.Fixture) {
				rec := f.FindFirst(matchRowClass(evidence.RowEndToEnd))
				rec.Outcome = evidence.OutcomeNotObserved
				rec.Grade = evidence.GradeNotObserved
			},
			want: evidence.VerdictUnmeasured,
		},
		{
			name: "protocol continuation below a usable native reference",
			mutate: func(f *evidencetest.Fixture) {
				recall := f.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
				fallback := evidencetest.FixtureSession(evidence.SurfaceProtocol, "fallback")
				recall.SessionID = &fallback
				recall.Detail = evidence.RecallFreshFallback
				recall.Grade = evidence.GradeGap
				if baseline := f.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
					baseline.Grade = evidence.GradeGap
				}
			},
			want: evidence.VerdictNotQualified,
		},
		{
			name: "protocol continuation recall precondition unmet",
			mutate: func(f *evidencetest.Fixture) {
				recall := f.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
				recall.SessionID = nil
				recall.Detail = evidence.RecallPreconditionUnmet
				recall.Grade = evidence.GradeNotObserved
				recall.Outcome = evidence.OutcomePrerequisiteFailed
				if baseline := f.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation)); baseline != nil {
					baseline.Grade = evidence.GradeNotObserved
					baseline.Outcome = evidence.OutcomePrerequisiteFailed
				}
			},
			want: evidence.VerdictUnmeasured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
			tt.mutate(fixture)
			fixture.Finalize()
			path := evidencetest.WriteEvidenceFile(t, fixture.Records)
			RequireObservationVerdict(t, path, tt.want)
		})
	}
}

func TestExplainEligibility(t *testing.T) {
	t.Parallel()

	t.Run("ComputeEligibility agrees with ExplainEligibility on every fixture variant", func(t *testing.T) {
		t.Parallel()

		for _, variant := range []string{evidencetest.FixtureQualified, evidencetest.FixtureNotQualified, evidencetest.FixtureUnmeasured} {
			t.Run(variant, func(t *testing.T) {
				t.Parallel()

				fixture := evidencetest.NewFixture(variant)
				report := ExplainEligibility(fixture.Records, fixture.Declarations())
				if got := ComputeEligibility(fixture.Records, fixture.Declarations()); got != report.Verdict {
					t.Errorf("ComputeEligibility(%s) = %s, want ExplainEligibility(...).Verdict %s", variant, got, report.Verdict)
				}
			})
		}
	})

	t.Run("each of the three verdicts is reachable from a constructed record set", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			variant string
			want    evidence.Verdict
		}{
			{"a fully measured fixture qualifies", evidencetest.FixtureQualified, evidence.VerdictQualified},
			{"a conflated protocol disposition case is not_qualified", evidencetest.FixtureNotQualified, evidence.VerdictNotQualified},
			{"an unobserved protocol disposition case is unmeasured", evidencetest.FixtureUnmeasured, evidence.VerdictUnmeasured},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				fixture := evidencetest.NewFixture(tt.variant)
				report := ExplainEligibility(fixture.Records, fixture.Declarations())
				if report.Verdict != tt.want {
					t.Errorf("ExplainEligibility(%s).Verdict = %s, want %s", tt.variant, report.Verdict, tt.want)
				}
			})
		}
	})

	t.Run("a below standing takes precedence over an unmeasured standing in the same report", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetTokenCorroborationOnly(evidence.SurfaceProtocol)
		fixture.SetSemanticNotObserved(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome)
		report := ExplainEligibility(fixture.Records, fixture.Declarations())
		if report.Verdict != evidence.VerdictNotQualified {
			t.Fatalf("ExplainEligibility().Verdict = %s, want not_qualified when a below row and an unmeasured row both appear", report.Verdict)
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
			t.Errorf("report rows = %+v, want at least one below row and one unmeasured row", report.Rows)
		}
	})

	t.Run("a below standing survives a different case of the same capability being excluded", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureNotQualified)
		fixture.SetSemanticDeclaredGap(evidence.CapabilityTurnDisposition, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		report := ExplainEligibility(fixture.Records, fixture.Declarations())
		if report.Verdict != evidence.VerdictNotQualified {
			t.Errorf("ExplainEligibility().Verdict = %s, want not_qualified: excluding a sibling case must not mask the conflated refusal case's below standing", report.Verdict)
		}
	})
}

// matchRowClass matches the first record ClassifyRecord places in class.
func matchRowClass(class evidence.RowClass) func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		got, err := ClassifyRecord(rec)
		return err == nil && got == class
	}
}
