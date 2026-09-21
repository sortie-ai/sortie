package eval

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// aggregateFixtureRecord mirrors evidencetest's unexported helper of the same
// shape: the one terminal aggregate record a validated run's evidence set
// closes on.
func aggregateFixtureRecord(classification evidence.Grade) evidence.Record {
	return evidence.Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    evidencetest.FixtureTime,
		Scenario:      evidence.ScenarioQualification,
		Surface:       evidence.SurfaceAggregate,
		Capability:    evidence.CapabilityEligibility,
		Source:        evidence.SourceComparison,
		Grade:         classification,
		Outcome:       evidence.OutcomePass,
		InputID:       evidence.InputAggregate,
		EvidencePath:  new(evidence.EvidencePathQualificationVerdict),
		Detail:        "aggregate qualification verdict recomputed from the closed non-final evidence set",
	}
}

func TestValidateObservations(t *testing.T) {
	t.Parallel()

	t.Run("qualified variant computes qualified", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		tokenCount := evidencetest.TokenRecordCount(fixture.Records)
		sessionCount := evidencetest.ProtocolSessionCount(fixture.Records)
		if got := len(fixture.Records); got != 51+tokenCount+sessionCount {
			t.Errorf("qualified fixture Record count = %d, want 51+tokenCount+sessionCount = %d (T=%d, N=%d)", got, 51+tokenCount+sessionCount, tokenCount, sessionCount)
		}
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		RequireObservationVerdict(t, path, evidence.VerdictQualified)
	})

	t.Run("not_qualified variant computes not_qualified", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureNotQualified)
		fixture.Finalize()
		tokenCount := evidencetest.TokenRecordCount(fixture.Records)
		sessionCount := evidencetest.ProtocolSessionCount(fixture.Records)
		if got := len(fixture.Records); got != 51+tokenCount+sessionCount {
			t.Errorf("not_qualified fixture Record count = %d, want 51+tokenCount+sessionCount = %d (T=%d, N=%d)", got, 51+tokenCount+sessionCount, tokenCount, sessionCount)
		}
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		RequireObservationVerdict(t, path, evidence.VerdictNotQualified)
	})

	t.Run("unmeasured variant computes unmeasured", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureUnmeasured)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		RequireObservationVerdict(t, path, evidence.VerdictUnmeasured)
	})

	t.Run("zero-line file", func(t *testing.T) {
		t.Parallel()

		path := writeLines(t)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateObservations() = nil error, want rejection of an empty file")
		} else if !strings.Contains(err.Error(), "no records") {
			t.Errorf("ValidateObservations() error = %v, want the empty-file cause", err)
		}
	})

	t.Run("invalid enum value", func(t *testing.T) {
		t.Parallel()

		fields := marshalRecordFields(t, evidence.ValidRecord())
		fields["scenario"] = json.RawMessage(`"mystery_scenario"`)
		line, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal doctored fields: %v", err)
		}
		path := writeLines(t, string(line))
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateObservations() = nil error, want rejection of an invalid enum")
		} else if !strings.Contains(err.Error(), "outside the closed value set") {
			t.Errorf("ValidateObservations() error = %v, want the closed-set cause", err)
		}
	})

	t.Run("wrong schema version", func(t *testing.T) {
		t.Parallel()

		fields := marshalRecordFields(t, evidence.ValidRecord())
		fields["schema_version"] = json.RawMessage(`2`)
		line, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal doctored fields: %v", err)
		}
		path := writeLines(t, string(line))
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateObservations() = nil error, want rejection of schema_version 2")
		}
	})

	t.Run("non-contiguous Sequence", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.Records[3].Sequence = 99
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateObservations() = nil error, want rejection of a non-contiguous Sequence")
		}
	})

	t.Run("blank lines are tolerated", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		var lines []string
		for _, rec := range fixture.Records {
			line, err := evidence.MarshalRecord(rec)
			if err != nil {
				t.Fatalf("marshal evidence Record: %v", err)
			}
			lines = append(lines, string(line))
		}
		path := writeLines(t, append([]string{""}, lines...)...)
		RequireObservationVerdict(t, path, evidence.VerdictQualified)
	})
}

func TestValidateEvidence(t *testing.T) {
	t.Parallel()

	t.Run("qualified variant", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeQualified)
		RequireFinalVerdict(t, path, evidence.VerdictQualified)
	})

	t.Run("not_qualified variant", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureNotQualified)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeNotQualified)
		RequireFinalVerdict(t, path, evidence.VerdictNotQualified)
	})

	t.Run("unmeasured variant", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureUnmeasured)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeUnmeasured)
		RequireFinalVerdict(t, path, evidence.VerdictUnmeasured)
	})

	t.Run("a stale aggregate names both grades in its recomputation-mismatch error", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeNotQualified)
		_, err := ValidateEvidence(path, profile.RuntimeProfile{})
		if err == nil {
			t.Fatal("ValidateEvidence() = nil error, want rejection of a stale aggregate")
		}
		if !strings.Contains(err.Error(), string(evidence.GradeNotQualified)) || !strings.Contains(err.Error(), string(evidence.GradeQualified)) {
			t.Errorf("ValidateEvidence() error = %v, want it to name both the written grade %q and the recomputed grade %q", err, evidence.GradeNotQualified, evidence.GradeQualified)
		}
	})

	t.Run("aggregate field violations", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			doctor func(rec *evidence.Record)
		}{
			{
				name: "wrong evidence path",
				doctor: func(rec *evidence.Record) {
					rec.EvidencePath = new("qualification.summary")
				},
			},
			{
				name: "null evidence path",
				doctor: func(rec *evidence.Record) {
					rec.EvidencePath = nil
				},
			},
			{
				name: "Verdict not pass",
				doctor: func(rec *evidence.Record) {
					rec.Outcome = evidence.OutcomeNotObserved
				},
			},
			{
				name: "session id set",
				doctor: func(rec *evidence.Record) {
					rec.SessionID = new(evidencetest.FixtureSession(evidence.SurfaceProtocol, "e2e"))
				},
			},
			{
				name: "wrong input id",
				doctor: func(rec *evidence.Record) {
					rec.InputID = evidence.InputBaseline
				},
			},
			{
				name: "protocol version set",
				doctor: func(rec *evidence.Record) {
					rec.ProtocolVersion = new(1)
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
				fixture.Finalize()
				aggregate := aggregateFixtureRecord(evidence.GradeQualified)
				tt.doctor(&aggregate)
				complete := append(slices.Clone(fixture.Records), aggregate)
				for i := range complete {
					complete[i].Sequence = i + 1
				}
				path := evidencetest.WriteEvidenceFile(t, complete)
				if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
					t.Errorf("ValidateEvidence() = nil error, want rejection when the aggregate %s", tt.name)
				}
			})
		}
	})
}

func TestValidateObservationsWithDeclarations(t *testing.T) {
	t.Parallel()

	t.Run("a declared_gap fixture validates qualified against its own declaration set", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)

		verdict, err := ValidateObservations(path, fixture.Declarations())
		if err != nil {
			t.Fatalf("ValidateObservations() error = %v, want nil", err)
		}
		if verdict != evidence.VerdictQualified {
			t.Errorf("ValidateObservations() = %s, want qualified", verdict)
		}
	})

	t.Run("the empty-set entry point rejects every declared_gap record", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)

		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateObservations() = nil error, want rejection of a declared_gap record no declaration authorizes")
		}
	})

	t.Run("a pre-change-shaped file validates identically through both entry points", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)

		plain, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err != nil {
			t.Fatalf("ValidateObservations() error = %v, want nil", err)
		}
		withEmptyDeclarations, err := ValidateObservations(path, profile.RuntimeProfile{})
		if err != nil {
			t.Fatalf("ValidateObservations() (empty declarations) error = %v, want nil", err)
		}
		if plain != withEmptyDeclarations {
			t.Errorf("ValidateObservations() = %s, ValidateObservations(zero profile) = %s, want them to agree on legacy-shaped evidence", plain, withEmptyDeclarations)
		}
		if plain != evidence.VerdictQualified {
			t.Errorf("ValidateObservations() = %s, want qualified", plain)
		}
	})
}

func TestExplainEligibilityNoStructuredNativeSurface(t *testing.T) {
	t.Parallel()

	absent := []profile.AbsentSurface{
		{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
		{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
	}
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, absent...)
	fixture.Finalize()
	declarations := fixture.Declarations()

	report := ExplainEligibility(fixture.Records, declarations)
	if !report.NativeReferenceAbsent {
		t.Error("ExplainEligibility() NativeReferenceAbsent = false, want true when both structured native surfaces are declared absent")
	}
	for _, row := range report.Rows {
		if row.Standing == StandingUnmeasured {
			t.Errorf("row %s Standing = %s, Cause = %q, want a comparison row never unmeasured for lack of a native reference", row.Label, row.Standing, row.Cause)
		}
	}
	if report.Verdict == evidence.VerdictUnmeasured {
		t.Errorf("ExplainEligibility() Verdict = %s, want a verdict other than unmeasured", report.Verdict)
	}

	if got := ComputeEligibility(fixture.Records, declarations); got != report.Verdict {
		t.Errorf("ComputeEligibility() = %s, want it to agree with ExplainEligibility().Verdict = %s", got, report.Verdict)
	}
}

func TestExplainEligibilityOneStructuredNativeSurfaceAbsent(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, profile.AbsentSurface{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered})
	fixture.Finalize()
	declarations := fixture.Declarations()

	report := ExplainEligibility(fixture.Records, declarations)
	if report.NativeReferenceAbsent {
		t.Error("ExplainEligibility() NativeReferenceAbsent = true, want false: native_stream_json is still measured")
	}
	for _, row := range report.Rows {
		if row.Standing == StandingUnmeasured {
			t.Errorf("row %s Standing = %s, Cause = %q, want every row measured against the one remaining structured native surface", row.Label, row.Standing, row.Cause)
		}
	}
}

func TestValidateEvidenceWithDeclarationsAbsentSurface(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		absent []profile.AbsentSurface
	}{
		{
			name:   "one structured native surface declared absent",
			absent: []profile.AbsentSurface{{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered}},
		},
		{
			name: "both structured native surfaces declared absent",
			absent: []profile.AbsentSurface{
				{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
				{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, tt.absent...)
			fixture.Finalize()
			declarations := fixture.Declarations()

			wantVerdict := ComputeEligibility(fixture.Records, declarations)
			if wantVerdict == evidence.VerdictUnmeasured {
				t.Fatalf("ComputeEligibility() = %s, want a runtime missing a structured native surface not to land on unmeasured", wantVerdict)
			}

			path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, AggregateGradeFor(wantVerdict))
			gotVerdict, err := ValidateEvidence(path, declarations)
			if err != nil {
				t.Fatalf("ValidateEvidence() error = %v, want nil", err)
			}
			if gotVerdict != wantVerdict {
				t.Errorf("ValidateEvidence() = %s, want %s: it must agree with the eligibility functions' own independent recomputation over the same records", gotVerdict, wantVerdict)
			}

			for _, decl := range tt.absent {
				for _, rec := range fixture.Records {
					if rec.Scenario == evidence.ScenarioTokenSource && rec.Surface == decl.Surface {
						t.Errorf("fixture carries a token-inventory record for declared-absent surface %s: %+v", decl.Surface, rec)
					}
				}
			}
		})
	}
}

func TestValidateEvidenceWithDeclarationsAbsentSurfaceMalformed(t *testing.T) {
	t.Parallel()

	absent := []profile.AbsentSurface{
		{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
		{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
	}

	t.Run("a record for a declared-absent surface is rejected", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, absent...)
		fixture.Add(evidence.Record{
			SchemaVersion: 1,
			ObservedAt:    evidencetest.FixtureTime,
			Scenario:      evidence.ScenarioSemanticProbe,
			Surface:       evidence.SurfaceNativeJSON,
			Capability:    evidence.CapabilityTurnDisposition,
			SemanticCase:  new(evidence.CaseSuccess),
			InputID:       evidence.CaseInputs[evidence.CaseSuccess],
			Source:        evidence.SourceNativeStructured,
			Grade:         evidence.GradeUsable,
			Outcome:       evidence.OutcomePass,
			SessionID:     new(evidencetest.FixtureSession(evidence.SurfaceNativeJSON, string(evidence.CaseSuccess))),
			EvidencePath:  new(evidence.SemanticEvidencePath(evidence.SurfaceNativeJSON)),
			AgentVersion:  new(evidencetest.FixtureAgentVer),
			Detail:        "a record placed on a surface this fixture declares absent",
		})
		fixture.Finalize()
		declarations := fixture.Declarations()
		verdict := ComputeEligibility(fixture.Records, declarations)
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, AggregateGradeFor(verdict))

		_, err := ValidateEvidence(path, declarations)
		if err == nil {
			t.Fatal("ValidateEvidence() = nil error, want rejection of a record on a declared-absent surface")
		}
		if !strings.Contains(err.Error(), string(evidence.SurfaceNativeJSON)) || !strings.Contains(err.Error(), "declared absent") {
			t.Errorf("ValidateEvidence() error = %v, want it to name %q and the declared-absent cause", err, evidence.SurfaceNativeJSON)
		}
	})

	t.Run("a missing singleton record is rejected by the derived cardinality check", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, absent...)
		target := fixture.FindFirst(func(rec *evidence.Record) bool { return rec.Scenario == evidence.ScenarioToolServer })
		if target == nil {
			t.Fatal("fixture carries no tool server delivery record to remove")
		}
		fixture.Remove(target)
		fixture.Finalize()
		declarations := fixture.Declarations()
		verdict := ComputeEligibility(fixture.Records, declarations)
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, AggregateGradeFor(verdict))

		_, err := ValidateEvidence(path, declarations)
		if err == nil {
			t.Fatal("ValidateEvidence() = nil error, want rejection of a perturbed record count")
		}
		if !strings.Contains(err.Error(), "tool server delivery") || !strings.Contains(err.Error(), "want exactly 1") {
			t.Errorf("ValidateEvidence() error = %v, want it to name the row and the derived count", err)
		}
	})
}

func TestAggregateGradeFor(t *testing.T) {
	t.Parallel()

	want := map[evidence.Verdict]evidence.Grade{
		evidence.VerdictQualified:    evidence.GradeQualified,
		evidence.VerdictNotQualified: evidence.GradeNotQualified,
		evidence.VerdictUnmeasured:   evidence.GradeUnmeasured,
	}
	for _, verdict := range evidence.Verdicts {
		grade, known := want[verdict]
		if !known {
			t.Fatalf("Verdicts carries %q, which this test's want map does not cover", verdict)
		}
		if got := AggregateGradeFor(verdict); got != grade {
			t.Errorf("AggregateGradeFor(%s) = %s, want %s", verdict, got, grade)
		}
	}
	if got := AggregateGradeFor(evidence.Verdict("mystery_verdict")); got != "" {
		t.Errorf("AggregateGradeFor(mystery_verdict) = %s, want the zero value", got)
	}
}

func TestVerdictRationale(t *testing.T) {
	t.Parallel()

	for _, verdict := range evidence.Verdicts {
		if got := VerdictRationale(verdict); got == "" {
			t.Errorf("VerdictRationale(%s) = %q, want a non-empty rationale", verdict, got)
		}
	}
	if got := VerdictRationale(evidence.Verdict("mystery_verdict")); got != "" {
		t.Errorf("VerdictRationale(mystery_verdict) = %q, want the zero value", got)
	}
}

func TestValidateEvidenceWithDeclarations(t *testing.T) {
	t.Parallel()

	t.Run("a declared_gap fixture validates qualified against its own declaration set", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeQualified)

		verdict, err := ValidateEvidence(path, fixture.Declarations())
		if err != nil {
			t.Fatalf("ValidateEvidence() error = %v, want nil", err)
		}
		if verdict != evidence.VerdictQualified {
			t.Errorf("ValidateEvidence() = %s, want qualified", verdict)
		}
	})

	t.Run("the empty-set entry point rejects every declared_gap record", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		path := evidencetest.WriteFinalEvidenceFile(t, fixture.Records, evidence.GradeQualified)

		if _, err := ValidateEvidence(path, profile.RuntimeProfile{}); err == nil {
			t.Error("ValidateEvidence() = nil error, want rejection of a declared_gap record no declaration authorizes")
		}
	})
}
