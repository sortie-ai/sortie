package qualification

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func fixtureProbePrompts() map[string]string {
	return map[string]string{
		"success":             "Reply with exactly SORTIE_BASELINE_OK and do not call any tool.",
		"runtime_refusal":     "Decline to continue this turn and report your refusal outcome without calling a tool.",
		"tool_call":           "Call the tool named {tool} now, with no arguments, then reply with exactly SORTIE_PROBE_DONE.",
		"continuation_seed":   "Remember the nonce {nonce} for the rest of this conversation and reply exactly STORED.",
		"continuation_recall": "Reply with the nonce supplied by the prior conversation and no other text.",
	}
}

// requireDeclarationsRoundTrip fails t unless declarations, embedded
// in an otherwise-minimal valid profile document, marshals and decodes
// back through DecodeRuntimeProfile to an equal declarations and
// absent_surfaces set.
func requireDeclarationsRoundTrip(t *testing.T, declarations RuntimeProfile) {
	t.Helper()

	profile := RuntimeProfile{
		SchemaVersion:       4,
		RuntimeID:           "fixture",
		IdentityTokens:      []string{"fixture"},
		NotesPath:           "notes.md",
		MeasurementPath:     "measurement.json",
		PublishedSample:     "sample.md",
		ToolNameFormat:      "mcp_{server}_{tool}",
		ModelArgs:           []string{"--model", "{model}"},
		CapabilityGapLabels: []string{capabilityGapLabelTokenCounts},
		ProbePrompts:        fixtureProbePrompts(),
		// Every measurable surface carries an entry point, and every
		// structured native one carries a recognizer, because a
		// profile missing either is rejected before its declarations
		// are ever read.
		EntryPoints: map[Surface]EntryPoint{
			SurfaceProtocol:         {Args: []string{"--acp"}, AskingArgs: []string{"--acp", "--ask"}},
			SurfaceNativeJSON:       {Args: []string{"--json", "{prompt}"}, AskingArgs: []string{"--json", "--ask", "{prompt}"}},
			SurfaceNativeStreamJSON: {Args: []string{"--stream-json", "{prompt}"}, AskingArgs: []string{"--stream-json", "--ask", "{prompt}"}},
		},
		Recognizers: map[Surface]Recognizer{
			SurfaceNativeJSON:       {Locator: TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
			SurfaceNativeStreamJSON: {Locator: TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
		},
		Declarations:   declarations.Declarations,
		AbsentSurfaces: declarations.AbsentSurfaces,
		NotInducibleCases: []SurfaceNotInducible{
			{Surface: SurfaceNativeJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
			{Surface: SurfaceNativeStreamJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
		},
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

func TestFixtureSetSessionContinuationUsableReadsRecallsOwnPriorSessionID(t *testing.T) {
	t.Parallel()

	rewrittenPriorSession := "sess-protocol-resumed-from-elsewhere"

	fixture := NewFixture(FixtureQualified)
	recall := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
	if recall == nil {
		t.Fatal("no continuation recall record for protocol")
	}
	recall.PriorSessionID = new(rewrittenPriorSession)

	fixture.SetSessionContinuation(SurfaceProtocol, GradeUsable, "replay observed against the live second session")

	got := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
	if got == nil {
		t.Fatal("no continuation recall record for protocol after SetSessionContinuation")
	}
	if !NullableEqual(got.SessionID, &rewrittenPriorSession) {
		t.Errorf("SetSessionContinuation(protocol, usable, ...) recall.SessionID = %s, want the recall's own prior_session_id %q",
			continuationRecordDetail(got.SessionID), rewrittenPriorSession)
	}
}

func matchPolicyPrecondition() func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioPolicyPrecondition && rec.Surface == SurfaceAggregate &&
			rec.Capability == CapabilityPermissionHandling
	}
}

func declaredGapAbsentSets() []struct {
	name   string
	absent []AbsentSurface
} {
	return []struct {
		name   string
		absent []AbsentSurface
	}{
		{name: "no absent surface"},
		{name: "native_json absent", absent: []AbsentSurface{{Surface: SurfaceNativeJSON, Reason: SurfaceNotOffered}}},
		{name: "native_stream_json absent", absent: []AbsentSurface{{Surface: SurfaceNativeStreamJSON, Reason: SurfaceNotOffered}}},
		{name: "both native surfaces absent", absent: []AbsentSurface{
			{Surface: SurfaceNativeJSON, Reason: SurfaceNotOffered},
			{Surface: SurfaceNativeStreamJSON, Reason: SurfaceNotOffered},
		}},
	}
}

type semanticTuple struct {
	Capability Capability
	Case       Case
}

func declaredGapRewrittenTuples(capability Capability, caseID Case) []semanticTuple {
	tuples := []semanticTuple{{Capability: capability, Case: caseID}}
	if peer, ok := DeclaredGapPeers[caseID]; ok {
		tuples = append(tuples, semanticTuple{Capability: capabilityOwning(peer), Case: peer})
	}
	return tuples
}

func TestFixtureSetSemanticDeclaredGapMatchesQualifiedFixture(t *testing.T) {
	t.Parallel()

	capabilities := []Capability{CapabilityTurnDisposition, CapabilityRetryClassification}

	for _, absentSet := range declaredGapAbsentSets() {
		t.Run(absentSet.name, func(t *testing.T) {
			t.Parallel()

			for _, capability := range capabilities {
				for _, caseID := range CapabilityCases[capability] {
					t.Run(string(capability)+"/"+string(caseID), func(t *testing.T) {
						t.Parallel()

						const reason = DeclaredGapNeverProduced

						notObserved := NewFixture(FixtureNotObserved, absentSet.absent...)
						qualified := NewFixture(FixtureQualified, absentSet.absent...)
						notObserved.SetSemanticDeclaredGap(capability, caseID, reason)
						qualified.SetSemanticDeclaredGap(capability, caseID, reason)

						declarable := intersectSurfaces(DeclarableSurfaces, notObserved.measured())
						for _, surface := range declarable {
							for _, tuple := range declaredGapRewrittenTuples(capability, caseID) {
								gotNotObserved := notObserved.FindFirst(MatchSemantic(surface, tuple.Capability, tuple.Case))
								gotQualified := qualified.FindFirst(MatchSemantic(surface, tuple.Capability, tuple.Case))
								if gotNotObserved == nil || gotQualified == nil {
									t.Fatalf("MatchSemantic(%s, %s, %s) found not_observed=%v qualified=%v, want both present", surface, tuple.Capability, tuple.Case, gotNotObserved, gotQualified)
								}
								if !RecordsEqual(*gotNotObserved, *gotQualified) {
									t.Errorf("SetSemanticDeclaredGap(%s, %s, ...) on %s: not_observed record = %+v, want equal to the FixtureQualified record %+v", tuple.Capability, tuple.Case, surface, *gotNotObserved, *gotQualified)
								}
							}
						}

						notObserved.Finalize()
						path := WriteEvidenceFile(t, notObserved.Records)
						if _, err := ValidateObservationsWithDeclarations(path, notObserved.Declarations()); err != nil {
							t.Errorf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
						}
					})
				}
			}
		})
	}
}

func continuationRecordDetail(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}

func assertContinuationRow(t *testing.T, fixture *Fixture, surface Surface, grade Grade, detail string) {
	t.Helper()

	wantOutcome := BaselineVerdictFor(grade)

	baseline := fixture.FindFirst(MatchBaseline(surface, CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatalf("no session_continuation baseline record for %s", surface)
	}
	if baseline.Grade != grade || baseline.Outcome != wantOutcome || baseline.Detail != boundDetail(detail) {
		t.Errorf("baseline for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *baseline, grade, wantOutcome, boundDetail(detail))
	}

	recall := fixture.FindFirst(MatchContinuation(surface, InputContinuationRecall))
	if recall == nil {
		t.Fatalf("no continuation recall record for %s", surface)
	}
	seedSession := FixtureSession(surface, "seed")
	fallbackSession := FixtureSession(surface, "recall-fallback")
	var wantRecallDetail string
	var wantRecallSessionID *string
	switch grade {
	case GradeUsable:
		wantRecallDetail = RecallConfirmedSameSession
		wantRecallSessionID = &seedSession
	case GradeGap:
		wantRecallDetail = RecallFreshFallback
		wantRecallSessionID = &fallbackSession
	case GradeNotObserved:
		wantRecallDetail = RecallUnobservedActual
	}
	if recall.Grade != grade || recall.Outcome != wantOutcome || recall.Detail != wantRecallDetail {
		t.Errorf("recall for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *recall, grade, wantOutcome, wantRecallDetail)
	}
	if !NullableEqual(recall.SessionID, wantRecallSessionID) {
		t.Errorf("recall for %s SessionID = %s, want %s", surface, continuationRecordDetail(recall.SessionID), continuationRecordDetail(wantRecallSessionID))
	}

	seed := fixture.FindFirst(MatchContinuation(surface, InputContinuationSeed))
	if seed == nil {
		t.Fatalf("no continuation seed record for %s", surface)
	}
	wantSeedGrade, wantSeedOutcome, wantSeedDetail := GradeUsable, OutcomePass, "seed session completed a turn that left history"
	if grade == GradeNotObserved {
		wantSeedGrade, wantSeedOutcome, wantSeedDetail = GradeNotObserved, OutcomeNotObserved, notObservedDetail(RowContinuationSeed)
	}
	if seed.Grade != wantSeedGrade || seed.Outcome != wantSeedOutcome || seed.Detail != wantSeedDetail {
		t.Errorf("seed for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *seed, wantSeedGrade, wantSeedOutcome, wantSeedDetail)
	}
}

func TestFixtureSetSessionContinuationIsOrderIndependent(t *testing.T) {
	t.Parallel()

	variants := []string{FixtureQualified, FixtureNotQualified, FixtureUnmeasured, FixtureDeclaredGap, FixtureNotObserved}
	grades := []Grade{GradeUsable, GradeGap, GradeNotObserved}

	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()

			for _, surface := range measuredSurfaces(RuntimeProfile{}) {
				t.Run(string(surface), func(t *testing.T) {
					t.Parallel()

					for _, g1 := range grades {
						for _, g2 := range grades {
							t.Run(string(g1)+"_then_"+string(g2), func(t *testing.T) {
								t.Parallel()

								const firstDetail = "first replay observation"
								const secondDetail = "second replay observation"

								twoCalls := NewFixture(variant)
								twoCalls.SetSessionContinuation(surface, g1, firstDetail)
								twoCalls.SetSessionContinuation(surface, g2, secondDetail)

								oneCall := NewFixture(variant)
								oneCall.SetSessionContinuation(surface, g2, secondDetail)

								rows := []struct {
									name  string
									match func(*Record) bool
								}{
									{"baseline", MatchBaseline(surface, CapabilitySessionContinuation)},
									{"recall", MatchContinuation(surface, InputContinuationRecall)},
									{"seed", MatchContinuation(surface, InputContinuationSeed)},
								}
								for _, row := range rows {
									gotTwoCalls := twoCalls.FindFirst(row.match)
									gotOneCall := oneCall.FindFirst(row.match)
									if gotTwoCalls == nil || gotOneCall == nil {
										t.Fatalf("%s record missing (two calls=%v, one call=%v)", row.name, gotTwoCalls, gotOneCall)
									}
									if !RecordsEqual(*gotTwoCalls, *gotOneCall) {
										t.Errorf("SetSessionContinuation(%s, %s, ...) preceded by a %s call: %s record = %+v, want equal to a fresh fixture's single call %+v", surface, g2, g1, row.name, *gotTwoCalls, *gotOneCall)
									}
								}

								assertContinuationRow(t, oneCall, surface, g2, secondDetail)

								oneCall.Finalize()
								path := WriteEvidenceFile(t, oneCall.Records)
								if _, err := ValidateObservationsWithDeclarations(path, oneCall.Declarations()); err != nil {
									t.Errorf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
								}
							})
						}
					}
				})
			}
		})
	}
}
