package qualification

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFixtureDeclarationsRoundTrip confirms Fixture.Declarations
// produces declarations and absent_surfaces the operator's own decode
// path would accept: embedded in an otherwise-minimal valid profile
// document, they marshal to JSON and back through DecodeRuntimeProfile
// without error, and the decoded entries equal the fixture's own. A
// fixture that authorized entries the decoder would refuse would let
// every control that passes Declarations() straight to
// ValidateObservationsWithDeclarations hide a decoder-rejected
// document behind an in-memory struct.
func TestFixtureDeclarationsRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("the declared_gap variant's declarations decode", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		requireDeclarationsRoundTrip(t, fixture.Declarations())
	})

	t.Run("a no-peer case's declaration decodes", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticDeclaredGap(CapabilityTurnDisposition, CaseCancellation, DeclaredGapNeverProduced)
		requireDeclarationsRoundTrip(t, fixture.Declarations())
	})

	t.Run("declaring a peer case directly after its closure already declared it does not duplicate", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		fixture.SetSemanticDeclaredGap(CapabilityRetryClassification, CaseNonRetryableRefusal, DeclaredGapNeverProduced)
		got := fixture.Declarations()
		if len(got.Declarations) != 2 {
			t.Fatalf("Declarations() = %d entries, want exactly 2: the refusal/retry pair with no duplicate from the repeated call", len(got.Declarations))
		}
		requireDeclarationsRoundTrip(t, got)
	})
}

// requireDeclarationsRoundTrip fails t unless declarations, embedded
// in an otherwise-minimal valid profile document, marshals and decodes
// back through DecodeRuntimeProfile to an equal declarations and
// absent_surfaces set.
func requireDeclarationsRoundTrip(t *testing.T, declarations RuntimeProfile) {
	t.Helper()

	profile := RuntimeProfile{
		SchemaVersion:       3,
		RuntimeID:           "fixture",
		IdentityTokens:      []string{"fixture"},
		NotesPath:           "notes.md",
		MeasurementPath:     "measurement.json",
		PublishedSample:     "sample.md",
		ToolNameFormat:      "mcp_{server}_{tool}",
		ModelArgs:           []string{"--model", "{model}"},
		CapabilityGapLabels: []string{capabilityGapLabelTokenCounts},
		// Every measurable surface carries an entry point, and every
		// structured native one carries a recognizer, because a
		// profile missing either is rejected before its declarations
		// are ever read.
		EntryPoints: map[Surface]EntryPoint{
			SurfaceProtocol:         {Args: []string{"--acp"}},
			SurfaceNativeJSON:       {Args: []string{"--json", "{prompt}"}},
			SurfaceNativeStreamJSON: {Args: []string{"--stream-json", "{prompt}"}},
		},
		Recognizers: map[Surface]Recognizer{
			SurfaceNativeJSON:       {Locator: TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
			SurfaceNativeStreamJSON: {Locator: TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
		},
		Declarations:   declarations.Declarations,
		AbsentSurfaces: declarations.AbsentSurfaces,
	}

	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v, want nil", profile, err)
	}
	decoded, err := DecodeRuntimeProfile(data)
	if err != nil {
		t.Fatalf("DecodeRuntimeProfile(%s) error = %v, want the fixture's own declarations to decode", data, err)
	}
	if len(decoded.Declarations) != len(declarations.Declarations) {
		t.Fatalf("DecodeRuntimeProfile() = %d declarations, want %d", len(decoded.Declarations), len(declarations.Declarations))
	}
	for i := range declarations.Declarations {
		if decoded.Declarations[i] != declarations.Declarations[i] {
			t.Errorf("DecodeRuntimeProfile() entry %d = %+v, want %+v", i, decoded.Declarations[i], declarations.Declarations[i])
		}
	}
}

// TestFixtureSetToolServerDelivery confirms SetToolServerDelivery
// rewrites addToolServer's own seeded record to the given grade and
// detail, deriving Outcome with BaselineVerdictFor's convention, for
// each of the three grades an observed induction can produce.
func TestFixtureSetToolServerDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       Grade
		detail      string
		wantOutcome Outcome
	}{
		{"usable", GradeUsable, "test server received the generated nonce and the turn consumed it", OutcomePass},
		{"gap", GradeGap, "turn completed with no call recorded at the server", OutcomePass},
		{"not_observed", GradeNotObserved, "the fake server failed to launch", OutcomeNotObserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			fixture.SetToolServerDelivery(tt.grade, tt.detail)

			rec := fixture.FindFirst(matchToolServer())
			if rec == nil {
				t.Fatal("SetToolServerDelivery() left no record matchToolServer() can find")
			}
			if rec.Grade != tt.grade {
				t.Errorf("SetToolServerDelivery(%s, ...) record.Grade = %s, want %s", tt.grade, rec.Grade, tt.grade)
			}
			if rec.Detail != tt.detail {
				t.Errorf("SetToolServerDelivery(%s, %q) record.Detail = %q, want %q", tt.grade, tt.detail, rec.Detail, tt.detail)
			}
			if rec.Outcome != tt.wantOutcome {
				t.Errorf("SetToolServerDelivery(%s, ...) record.Outcome = %s, want %s", tt.grade, rec.Outcome, tt.wantOutcome)
			}
		})
	}

	t.Run("detail beyond DetailBound is truncated", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		long := strings.Repeat("x", DetailBound+50)
		fixture.SetToolServerDelivery(GradeUsable, long)

		rec := fixture.FindFirst(matchToolServer())
		if rec == nil {
			t.Fatal("SetToolServerDelivery() left no record matchToolServer() can find")
		}
		if got := len([]rune(rec.Detail)); got != DetailBound {
			t.Errorf("SetToolServerDelivery(usable, %d-rune detail) record.Detail carries %d code points, want %d", len([]rune(long)), got, DetailBound)
		}
	})
}

// TestFixtureSetPermissionHandling confirms SetPermissionHandling
// rewrites addPermission's own seeded record to the given grade and
// detail for each of the three grades a permission-handling
// observation can produce, and confirms the drift from the plan's
// prose: addPolicyPrecondition's record classifies as
// RowPolicyPrecondition, which ConclusionsFromRecords never turns
// into a graded row, so SetPermissionHandling leaves it untouched
// rather than rewriting it too.
func TestFixtureSetPermissionHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       Grade
		detail      string
		wantOutcome Outcome
	}{
		{"usable", GradeUsable, "request answered with a refusing option and no request left pending", OutcomePass},
		{"gap", GradeGap, "a request was raised but offered no refusing option", OutcomePass},
		{"not_observed", GradeNotObserved, "no permission request was raised under this posture", OutcomeNotObserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			policyBefore := *fixture.FindFirst(matchPolicyPrecondition())

			fixture.SetPermissionHandling(tt.grade, tt.detail)

			rec := fixture.FindFirst(matchPermission())
			if rec == nil {
				t.Fatal("SetPermissionHandling() left no record matchPermission() can find")
			}
			if rec.Grade != tt.grade {
				t.Errorf("SetPermissionHandling(%s, ...) record.Grade = %s, want %s", tt.grade, rec.Grade, tt.grade)
			}
			if rec.Detail != tt.detail {
				t.Errorf("SetPermissionHandling(%s, %q) record.Detail = %q, want %q", tt.grade, tt.detail, rec.Detail, tt.detail)
			}
			if rec.Outcome != tt.wantOutcome {
				t.Errorf("SetPermissionHandling(%s, ...) record.Outcome = %s, want %s", tt.grade, rec.Outcome, tt.wantOutcome)
			}

			class, err := ClassifyRecord(rec)
			if err != nil {
				t.Fatalf("ClassifyRecord(the rewritten permission record) error = %v, want nil", err)
			}
			if class != RowPermission {
				t.Errorf("ClassifyRecord(the rewritten permission record) = %v, want %v (the graded row SetPermissionHandling must rewrite)", class, RowPermission)
			}

			policyAfter := fixture.FindFirst(matchPolicyPrecondition())
			if policyAfter == nil {
				t.Fatal("SetPermissionHandling() unexpectedly removed the policy-precondition record")
			}
			policyClass, err := ClassifyRecord(policyAfter)
			if err != nil {
				t.Fatalf("ClassifyRecord(the policy-precondition record) error = %v, want nil", err)
			}
			if policyClass != RowPolicyPrecondition {
				t.Fatalf("ClassifyRecord(the policy-precondition record) = %v, want %v (never a graded row, per ConclusionsFromRecords)", policyClass, RowPolicyPrecondition)
			}
			if *policyAfter != policyBefore {
				t.Errorf("SetPermissionHandling(%s, ...) changed the policy-precondition record from %+v to %+v, want it untouched: it classifies as %v, which ConclusionsFromRecords never grades", tt.grade, policyBefore, *policyAfter, RowPolicyPrecondition)
			}
		})
	}
}

// TestFixtureSetSessionContinuation confirms SetSessionContinuation
// rewrites both the baseline and the recall record addContinuation
// seeded for surface, for each of the three grades a continuation
// replay observation can produce, and confirms the drift from the
// plan's prose: the caller's free-text detail lands on the baseline
// record, while the recall record's Detail is always one of the
// closed-set constants checkRecallRecord validates against, never the
// caller's text.
func TestFixtureSetSessionContinuation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       Grade
		wantOutcome Outcome
		wantRecall  string
	}{
		{"usable", GradeUsable, OutcomePass, RecallConfirmedSameSession},
		{"gap", GradeGap, OutcomePass, RecallFreshFallback},
		{"not_observed", GradeNotObserved, OutcomeNotObserved, RecallUnobservedActual},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const callerDetail = "replay observed against the live second session"

			fixture := NewFixture(FixtureQualified)
			fixture.SetSessionContinuation(SurfaceProtocol, tt.grade, callerDetail)

			baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation))
			if baseline == nil {
				t.Fatal("SetSessionContinuation() left no record MatchBaseline() can find")
			}
			if baseline.Grade != tt.grade {
				t.Errorf("SetSessionContinuation(protocol, %s, ...) baseline.Grade = %s, want %s", tt.grade, baseline.Grade, tt.grade)
			}
			if baseline.Outcome != tt.wantOutcome {
				t.Errorf("SetSessionContinuation(protocol, %s, ...) baseline.Outcome = %s, want %s", tt.grade, baseline.Outcome, tt.wantOutcome)
			}
			if baseline.Detail != callerDetail {
				t.Errorf("SetSessionContinuation(protocol, %s, %q) baseline.Detail = %q, want the caller's own detail", tt.grade, callerDetail, baseline.Detail)
			}

			recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
			if recall == nil {
				t.Fatal("SetSessionContinuation() left no record MatchContinuation() can find")
			}
			if recall.Grade != tt.grade {
				t.Errorf("SetSessionContinuation(protocol, %s, ...) recall.Grade = %s, want %s", tt.grade, recall.Grade, tt.grade)
			}
			if recall.Outcome != tt.wantOutcome {
				t.Errorf("SetSessionContinuation(protocol, %s, ...) recall.Outcome = %s, want %s", tt.grade, recall.Outcome, tt.wantOutcome)
			}
			if recall.Detail != tt.wantRecall {
				t.Errorf("SetSessionContinuation(protocol, %s, %q) recall.Detail = %q, want the closed-set constant %q, not the caller's free text", tt.grade, callerDetail, recall.Detail, tt.wantRecall)
			}
		})
	}
}

// matchPolicyPrecondition matches the single addPolicyPrecondition
// record, mirroring matchPermission's and matchToolServer's shape for
// a seeded record with no exported constructor of its own.
func matchPolicyPrecondition() func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioPolicyPrecondition && rec.Surface == SurfaceAggregate &&
			rec.Capability == CapabilityPermissionHandling
	}
}
