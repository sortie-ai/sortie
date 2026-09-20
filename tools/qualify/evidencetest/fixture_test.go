package evidencetest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func fixtureProbePrompts() map[string]string {
	return map[string]string{
		"success":             "Reply with exactly SORTIE_BASELINE_OK and do not call any tool.",
		"runtime_refusal":     "Decline to continue this turn and report your refusal outcome without calling a tool.",
		"tool_call":           "Call the tool named {tool} now, with no arguments, then reply with exactly SORTIE_PROBE_DONE.",
		"continuation_seed":   "Remember the nonce {nonce} for the rest of this conversation and reply exactly STORED.",
		"continuation_recall": "Reply with the nonce supplied by the prior conversation and no other text.",
	}
}

// requireDeclarationsRoundTrip fails t unless declarations, embedded in an
// otherwise-minimal valid profile document, marshals and decodes back
// through profile.Decode to an equal declarations and absent_surfaces set.
func requireDeclarationsRoundTrip(t *testing.T, declarations profile.RuntimeProfile) {
	t.Helper()

	const capabilityGapLabelTokenCounts = "token counts"

	p := profile.RuntimeProfile{
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
		// structured native one carries a recognizer, because a profile
		// missing either is rejected before its declarations are ever read.
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol:         {Args: []string{"--acp"}, AskingArgs: []string{"--acp", "--ask"}},
			evidence.SurfaceNativeJSON:       {Args: []string{"--json", "{prompt}"}, AskingArgs: []string{"--json", "--ask", "{prompt}"}},
			evidence.SurfaceNativeStreamJSON: {Args: []string{"--stream-json", "{prompt}"}, AskingArgs: []string{"--stream-json", "--ask", "{prompt}"}},
		},
		Recognizers: map[evidence.Surface]profile.Recognizer{
			evidence.SurfaceNativeJSON:       {Locator: profile.TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
			evidence.SurfaceNativeStreamJSON: {Locator: profile.TerminalLocator{Mode: "first_value"}, SuccessMember: "response"},
		},
		Declarations:   declarations.Declarations,
		AbsentSurfaces: declarations.AbsentSurfaces,
		NotInducibleCases: []profile.SurfaceNotInducible{
			{Surface: evidence.SurfaceNativeJSON, Case: evidence.CaseLimitReached, Reason: evidence.NotInducibleChannelTooSmall},
			{Surface: evidence.SurfaceNativeStreamJSON, Case: evidence.CaseLimitReached, Reason: evidence.NotInducibleChannelTooSmall},
		},
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal fixture profile: %v", err)
	}
	decoded, err := profile.Decode(data)
	if err != nil {
		t.Fatalf("profile.Decode(%s) error = %v, want the fixture's own declarations to decode", data, err)
	}
	if len(decoded.Declarations) != len(declarations.Declarations) {
		t.Fatalf("profile.Decode() = %d declarations, want %d", len(decoded.Declarations), len(declarations.Declarations))
	}
	for i := range declarations.Declarations {
		if decoded.Declarations[i] != declarations.Declarations[i] {
			t.Errorf("profile.Decode() entry %d = %+v, want %+v", i, decoded.Declarations[i], declarations.Declarations[i])
		}
	}
}

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
		fixture.SetSemanticDeclaredGap(evidence.CapabilityTurnDisposition, evidence.CaseCancellation, evidence.DeclaredGapNeverProduced)
		requireDeclarationsRoundTrip(t, fixture.Declarations())
	})

	t.Run("declaring a peer case directly after its closure already declared it does not duplicate", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureDeclaredGap)
		fixture.SetSemanticDeclaredGap(evidence.CapabilityRetryClassification, evidence.CaseNonRetryableRefusal, evidence.DeclaredGapNeverProduced)
		got := fixture.Declarations()
		if len(got.Declarations) != 2 {
			t.Fatalf("Declarations() = %d entries, want exactly 2: the refusal/retry pair with no duplicate from the repeated call", len(got.Declarations))
		}
		requireDeclarationsRoundTrip(t, got)
	})
}

func TestFixtureSetToolServerDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       evidence.Grade
		detail      string
		wantOutcome evidence.Outcome
	}{
		{"usable", evidence.GradeUsable, "test server received the generated nonce and the turn consumed it", evidence.OutcomePass},
		{"gap", evidence.GradeGap, "turn completed with no call recorded at the server", evidence.OutcomePass},
		{"not_observed", evidence.GradeNotObserved, "the fake server failed to launch", evidence.OutcomeNotObserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			if err := fixture.SetToolServerDelivery(evidence.Observation{Grade: tt.grade, Outcome: tt.wantOutcome, Detail: tt.detail, SessionID: "sess-mcp"}); err != nil {
				t.Fatalf("SetToolServerDelivery(...) error = %v, want nil", err)
			}

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
		long := strings.Repeat("x", evidence.DetailBound+50)
		if err := fixture.SetToolServerDelivery(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: long, SessionID: "sess-mcp"}); err != nil {
			t.Fatalf("SetToolServerDelivery(...) error = %v, want nil", err)
		}

		rec := fixture.FindFirst(matchToolServer())
		if rec == nil {
			t.Fatal("SetToolServerDelivery() left no record matchToolServer() can find")
		}
		if got := len([]rune(rec.Detail)); got != evidence.DetailBound {
			t.Errorf("SetToolServerDelivery(usable, %d-rune detail) record.Detail carries %d code points, want %d", len([]rune(long)), got, evidence.DetailBound)
		}
	})
}

func TestFixtureSetPermissionHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       evidence.Grade
		detail      string
		wantOutcome evidence.Outcome
	}{
		{"usable", evidence.GradeUsable, "request answered with a refusing option and no request left pending", evidence.OutcomePass},
		{"gap", evidence.GradeGap, "a request was raised but offered no refusing option", evidence.OutcomePass},
		{"not_observed", evidence.GradeNotObserved, "no permission request was raised under this posture", evidence.OutcomeNotObserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			policyBefore := *fixture.FindFirst(matchPolicyPrecondition())

			if err := fixture.SetPermissionHandling(evidence.Observation{Grade: tt.grade, Outcome: tt.wantOutcome, Detail: tt.detail, SessionID: "sess-permission"}); err != nil {
				t.Fatalf("SetPermissionHandling(...) error = %v, want nil", err)
			}

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

			class, err := eval.ClassifyRecord(rec)
			if err != nil {
				t.Fatalf("ClassifyRecord(the rewritten permission record) error = %v, want nil", err)
			}
			if class != evidence.RowPermission {
				t.Errorf("ClassifyRecord(the rewritten permission record) = %v, want %v (the graded row SetPermissionHandling must rewrite)", class, evidence.RowPermission)
			}

			policyAfter := fixture.FindFirst(matchPolicyPrecondition())
			if policyAfter == nil {
				t.Fatal("SetPermissionHandling() unexpectedly removed the policy-precondition record")
			}
			policyClass, err := eval.ClassifyRecord(policyAfter)
			if err != nil {
				t.Fatalf("ClassifyRecord(the policy-precondition record) error = %v, want nil", err)
			}
			if policyClass != evidence.RowPolicyPrecondition {
				t.Fatalf("ClassifyRecord(the policy-precondition record) = %v, want %v (never a graded row)", policyClass, evidence.RowPolicyPrecondition)
			}
			if *policyAfter != policyBefore {
				t.Errorf("SetPermissionHandling(%s, ...) changed the policy-precondition record from %+v to %+v, want it untouched", tt.grade, policyBefore, *policyAfter)
			}
		})
	}
}

func TestFixtureSetSessionContinuation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grade       evidence.Grade
		wantOutcome evidence.Outcome
		wantRecall  string
	}{
		{"usable", evidence.GradeUsable, evidence.OutcomePass, evidence.RecallConfirmedSameSession},
		{"gap", evidence.GradeGap, evidence.OutcomePass, evidence.RecallFreshFallback},
		{"not_observed", evidence.GradeNotObserved, evidence.OutcomeNotObserved, evidence.RecallUnobservedActual},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const callerDetail = "replay observed against the live second session"

			fixture := NewFixture(FixtureQualified)
			fixture.SetSessionContinuation(evidence.SurfaceProtocol, tt.grade, callerDetail)

			baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation))
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

			recall := fixture.FindFirst(MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
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
	recall := fixture.FindFirst(MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
	if recall == nil {
		t.Fatal("no continuation recall record for protocol")
	}
	recall.PriorSessionID = &rewrittenPriorSession

	fixture.SetSessionContinuation(evidence.SurfaceProtocol, evidence.GradeUsable, "replay observed against the live second session")

	got := fixture.FindFirst(MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
	if got == nil {
		t.Fatal("no continuation recall record for protocol after SetSessionContinuation")
	}
	if !evidence.NullableEqual(got.SessionID, &rewrittenPriorSession) {
		t.Errorf("SetSessionContinuation(protocol, usable, ...) recall.SessionID = %s, want the recall's own prior_session_id %q",
			continuationRecordDetail(got.SessionID), rewrittenPriorSession)
	}
}

func matchPolicyPrecondition() func(*evidence.Record) bool {
	return func(rec *evidence.Record) bool {
		return rec.Scenario == evidence.ScenarioPolicyPrecondition && rec.Surface == evidence.SurfaceAggregate &&
			rec.Capability == evidence.CapabilityPermissionHandling
	}
}

func declaredGapAbsentSets() []struct {
	name   string
	absent []profile.AbsentSurface
} {
	return []struct {
		name   string
		absent []profile.AbsentSurface
	}{
		{name: "no absent surface"},
		{name: "native_json absent", absent: []profile.AbsentSurface{{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered}}},
		{name: "native_stream_json absent", absent: []profile.AbsentSurface{{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered}}},
		{name: "both native surfaces absent", absent: []profile.AbsentSurface{
			{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
			{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
		}},
	}
}

type semanticTuple struct {
	Capability evidence.Capability
	Case       evidence.Case
}

func declaredGapRewrittenTuples(capability evidence.Capability, caseID evidence.Case) []semanticTuple {
	tuples := []semanticTuple{{Capability: capability, Case: caseID}}
	if peer, ok := evidence.DeclaredGapPeers[caseID]; ok {
		tuples = append(tuples, semanticTuple{Capability: evidence.CapabilityOwning(peer), Case: peer})
	}
	return tuples
}

func TestFixtureSetSemanticDeclaredGapMatchesQualifiedFixture(t *testing.T) {
	t.Parallel()

	capabilities := []evidence.Capability{evidence.CapabilityTurnDisposition, evidence.CapabilityRetryClassification}

	for _, absentSet := range declaredGapAbsentSets() {
		t.Run(absentSet.name, func(t *testing.T) {
			t.Parallel()

			for _, capability := range capabilities {
				for _, caseID := range evidence.CapabilityCases[capability] {
					t.Run(string(capability)+"/"+string(caseID), func(t *testing.T) {
						t.Parallel()

						const reason = evidence.DeclaredGapNeverProduced

						notObserved := NewFixture(FixtureNotObserved, absentSet.absent...)
						qualified := NewFixture(FixtureQualified, absentSet.absent...)
						notObserved.SetSemanticDeclaredGap(capability, caseID, reason)
						qualified.SetSemanticDeclaredGap(capability, caseID, reason)

						declarable := evidence.IntersectSurfaces(evidence.DeclarableSurfaces, notObserved.measured())
						for _, surface := range declarable {
							for _, tuple := range declaredGapRewrittenTuples(capability, caseID) {
								gotNotObserved := notObserved.FindFirst(MatchSemantic(surface, tuple.Capability, tuple.Case))
								gotQualified := qualified.FindFirst(MatchSemantic(surface, tuple.Capability, tuple.Case))
								if gotNotObserved == nil || gotQualified == nil {
									t.Fatalf("MatchSemantic(%s, %s, %s) found not_observed=%v qualified=%v, want both present", surface, tuple.Capability, tuple.Case, gotNotObserved, gotQualified)
								}
								if !evidence.RecordsEqual(*gotNotObserved, *gotQualified) {
									t.Errorf("SetSemanticDeclaredGap(%s, %s, ...) on %s: not_observed record = %+v, want equal to the FixtureQualified record %+v", tuple.Capability, tuple.Case, surface, *gotNotObserved, *gotQualified)
								}
							}
						}

						notObserved.Finalize()
						path := WriteEvidenceFile(t, notObserved.Records)
						if _, err := eval.ValidateObservations(path, notObserved.Declarations()); err != nil {
							t.Errorf("ValidateObservations(...) error = %v, want nil", err)
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

func assertContinuationRow(t *testing.T, fixture *Fixture, surface evidence.Surface, grade evidence.Grade, detail string) {
	t.Helper()

	wantOutcome := BaselineVerdictFor(grade)

	baseline := fixture.FindFirst(evidence.MatchBaseline(surface, evidence.CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatalf("no session_continuation baseline record for %s", surface)
	}
	if baseline.Grade != grade || baseline.Outcome != wantOutcome || baseline.Detail != evidence.BoundDetail(detail) {
		t.Errorf("baseline for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *baseline, grade, wantOutcome, evidence.BoundDetail(detail))
	}

	recall := fixture.FindFirst(MatchContinuation(surface, evidence.InputContinuationRecall))
	if recall == nil {
		t.Fatalf("no continuation recall record for %s", surface)
	}
	seedSession := FixtureSession(surface, "seed")
	fallbackSession := FixtureSession(surface, "recall-fallback")
	var wantRecallDetail string
	var wantRecallSessionID *string
	switch grade {
	case evidence.GradeUsable:
		wantRecallDetail = evidence.RecallConfirmedSameSession
		wantRecallSessionID = &seedSession
	case evidence.GradeGap:
		wantRecallDetail = evidence.RecallFreshFallback
		wantRecallSessionID = &fallbackSession
	case evidence.GradeNotObserved:
		wantRecallDetail = evidence.RecallUnobservedActual
	}
	if recall.Grade != grade || recall.Outcome != wantOutcome || recall.Detail != wantRecallDetail {
		t.Errorf("recall for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *recall, grade, wantOutcome, wantRecallDetail)
	}
	if !evidence.NullableEqual(recall.SessionID, wantRecallSessionID) {
		t.Errorf("recall for %s SessionID = %s, want %s", surface, continuationRecordDetail(recall.SessionID), continuationRecordDetail(wantRecallSessionID))
	}

	seed := fixture.FindFirst(MatchContinuation(surface, evidence.InputContinuationSeed))
	if seed == nil {
		t.Fatalf("no continuation seed record for %s", surface)
	}
	wantSeedGrade, wantSeedOutcome, wantSeedDetail := evidence.GradeUsable, evidence.OutcomePass, "seed session completed a turn that left history"
	if grade == evidence.GradeNotObserved {
		wantSeedGrade, wantSeedOutcome, wantSeedDetail = evidence.GradeNotObserved, evidence.OutcomeNotObserved, notObservedDetail(evidence.RowContinuationSeed)
	}
	if seed.Grade != wantSeedGrade || seed.Outcome != wantSeedOutcome || seed.Detail != wantSeedDetail {
		t.Errorf("seed for %s = %+v, want grade=%s outcome=%s detail=%q", surface, *seed, wantSeedGrade, wantSeedOutcome, wantSeedDetail)
	}
}

func TestFixtureSetSessionContinuationIsOrderIndependent(t *testing.T) {
	t.Parallel()

	variants := []string{FixtureQualified, FixtureNotQualified, FixtureUnmeasured, FixtureDeclaredGap, FixtureNotObserved}
	grades := []evidence.Grade{evidence.GradeUsable, evidence.GradeGap, evidence.GradeNotObserved}

	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()

			for _, surface := range (profile.RuntimeProfile{}).DeclaredMeasuredSurfaces() {
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
									match func(*evidence.Record) bool
								}{
									{"baseline", evidence.MatchBaseline(surface, evidence.CapabilitySessionContinuation)},
									{"recall", MatchContinuation(surface, evidence.InputContinuationRecall)},
									{"seed", MatchContinuation(surface, evidence.InputContinuationSeed)},
								}
								for _, row := range rows {
									gotTwoCalls := twoCalls.FindFirst(row.match)
									gotOneCall := oneCall.FindFirst(row.match)
									if gotTwoCalls == nil || gotOneCall == nil {
										t.Fatalf("%s record missing (two calls=%v, one call=%v)", row.name, gotTwoCalls, gotOneCall)
									}
									if !evidence.RecordsEqual(*gotTwoCalls, *gotOneCall) {
										t.Errorf("SetSessionContinuation(%s, %s, ...) preceded by a %s call: %s record = %+v, want equal to a fresh fixture's single call %+v", surface, g2, g1, row.name, *gotTwoCalls, *gotOneCall)
									}
								}

								assertContinuationRow(t, oneCall, surface, g2, secondDetail)

								oneCall.Finalize()
								path := WriteEvidenceFile(t, oneCall.Records)
								if _, err := eval.ValidateObservations(path, oneCall.Declarations()); err != nil {
									t.Errorf("ValidateObservations(...) error = %v, want nil", err)
								}
							})
						}
					}
				})
			}
		})
	}
}

func TestFixtureSetTokenInventoryProtocolArms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		sessionID         string
		paths             []evidence.TokenObservation
		inventory         evidence.Observation
		wantBaselineGrade evidence.Grade
		wantBaselineOut   evidence.Outcome
	}{
		{
			name:              "turns completed with no usage reported leaves the baseline a gap",
			sessionID:         "",
			paths:             nil,
			inventory:         evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the inventory completed with no token-bearing path resolved"},
			wantBaselineGrade: evidence.GradeGap,
			wantBaselineOut:   evidence.OutcomePass,
		},
		{
			name:              "no turn completed at all sentinels the baseline not_observed",
			sessionID:         "",
			paths:             nil,
			inventory:         evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "no graded protocol turn completed"},
			wantBaselineGrade: evidence.GradeNotObserved,
			wantBaselineOut:   evidence.OutcomeRuntimeFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureQualified)
			if err := fixture.SetTokenInventory(evidence.SurfaceProtocol, tt.sessionID, tt.paths, tt.inventory, nil); err != nil {
				t.Fatalf("SetTokenInventory(protocol, %q, %v, %+v) error = %v, want nil", tt.sessionID, tt.paths, tt.inventory, err)
			}

			baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTokenCeiling))
			if baseline == nil {
				t.Fatal("fixture carries no protocol token-ceiling baseline")
			}
			if baseline.Grade != tt.wantBaselineGrade {
				t.Errorf("SetTokenInventory(protocol, ...) baseline grade = %s, want %s", baseline.Grade, tt.wantBaselineGrade)
			}
			if baseline.Outcome != tt.wantBaselineOut {
				t.Errorf("SetTokenInventory(protocol, ...) baseline outcome = %s, want %s", baseline.Outcome, tt.wantBaselineOut)
			}

			if len(tt.paths) == 0 {
				sentinel := fixture.FindFirst(matchTokenSurface(evidence.SurfaceProtocol))
				if sentinel == nil {
					t.Fatal("fixture carries no protocol token-source sentinel")
				}
				if sentinel.SessionID != nil {
					t.Errorf("SetTokenInventory(protocol, ...) sentinel SessionID = %v, want nil", *sentinel.SessionID)
				}
				if sentinel.EvidencePath != nil {
					t.Errorf("SetTokenInventory(protocol, ...) sentinel EvidencePath = %v, want nil", *sentinel.EvidencePath)
				}
			}
		})
	}
}

func TestFixtureSetTokenInventoryGapArmRejectsResolvedPath(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureQualified)
	err := fixture.SetTokenInventory(evidence.SurfaceProtocol, "sess-1",
		[]evidence.TokenObservation{{EvidencePath: "/turn/result/usage", Kind: "spend"}},
		evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "no graded protocol turn completed"}, nil)
	if err == nil {
		t.Error("SetTokenInventory(protocol, ...) = nil error, want rejection of a not_observed inventory carrying a resolved path")
	}
}

func publishedSemanticRow(t *testing.T, fixture *Fixture, surface evidence.Surface, capability evidence.Capability, caseID evidence.Case) (caseRow, baselineRow evidence.Record) {
	t.Helper()

	published, err := evidence.ReadEvidenceFile(WriteEvidenceFile(t, fixture.Records))
	if err != nil {
		t.Fatalf("ReadEvidenceFile(...) = _, %v, want nil", err)
	}
	var foundCase, foundBaseline bool
	for _, rec := range published {
		switch {
		case rec.Scenario == evidence.ScenarioSemanticProbe && rec.Surface == surface && rec.Capability == capability &&
			rec.SemanticCase != nil && *rec.SemanticCase == caseID:
			caseRow, foundCase = rec, true
		case rec.Scenario == evidence.ScenarioSurfaceBaseline && rec.Surface == surface && rec.Capability == capability:
			baselineRow, foundBaseline = rec, true
		}
	}
	if !foundCase || !foundBaseline {
		t.Fatalf("the published evidence carries no %s %s row for surface %s", capability, caseID, surface)
	}
	return caseRow, baselineRow
}

func TestPublishedRowCarriesTheReasonACaseWasNotInduced(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseCancellation, evidence.NotInducibleTerminalAtExitOnly)
	fixture.Finalize()

	caseRow, _ := publishedSemanticRow(t, fixture, evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseCancellation)
	if caseRow.Detail != evidence.NotInducibleTerminalAtExitOnly {
		t.Errorf("published cancellation row detail = %q, want %q", caseRow.Detail, evidence.NotInducibleTerminalAtExitOnly)
	}
}

func TestPublishedBaselineKeepsAChannelSilentCasesObligation(t *testing.T) {
	t.Parallel()

	t.Run("the surface's own silence", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseCancellation, evidence.NotInducibleTerminalAtExitOnly)
		fixture.Finalize()

		_, baselineRow := publishedSemanticRow(t, fixture, evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseCancellation)
		if baselineRow.Grade != evidence.GradeGap {
			t.Errorf("published turn_disposition baseline = %s, want %s: the surface reports nothing on a condition it does reach", baselineRow.Grade, evidence.GradeGap)
		}
	})

	t.Run("the measurer's own reach", func(t *testing.T) {
		t.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached, evidence.NotInducibleChannelTooSmall)
		fixture.Finalize()

		_, baselineRow := publishedSemanticRow(t, fixture, evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached)
		if baselineRow.Grade != evidence.GradeUsable {
			t.Errorf("published turn_disposition baseline = %s, want %s: a condition this measurer cannot create is no shortfall of the surface", baselineRow.Grade, evidence.GradeUsable)
		}
	})
}

func TestFixtureNotObservedVariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		absent []profile.AbsentSurface
	}{
		{name: "no absent surface"},
		{
			name: "native_json absent",
			absent: []profile.AbsentSurface{
				{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
			},
		},
		{
			name: "native_stream_json absent",
			absent: []profile.AbsentSurface{
				{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
			},
		},
		{
			name: "both native surfaces absent",
			absent: []profile.AbsentSurface{
				{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
				{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureNotObserved, tt.absent...)
			fixture.Finalize()

			for i, rec := range fixture.Records {
				if rec.Grade != evidence.GradeNotObserved {
					t.Errorf("record %d Grade = %s, want %s", i, rec.Grade, evidence.GradeNotObserved)
				}
				if rec.Outcome != evidence.OutcomeNotObserved {
					t.Errorf("record %d Outcome = %s, want %s", i, rec.Outcome, evidence.OutcomeNotObserved)
				}
			}

			declarations := fixture.Declarations()
			if len(declarations.Declarations) != 0 {
				t.Errorf("Declarations().Declarations = %v, want none", declarations.Declarations)
			}

			path := WriteEvidenceFile(t, fixture.Records)
			verdict, err := eval.ValidateObservations(path, declarations)
			if err != nil {
				t.Fatalf("ValidateObservations(...) error = %v, want nil", err)
			}
			if verdict != evidence.VerdictUnmeasured {
				t.Errorf("ValidateObservations(...) = %s, want %s", verdict, evidence.VerdictUnmeasured)
			}
		})
	}
}

func TestSetTokenInventoryRejectsAnUnattributedPath(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureQualified)
	err := fixture.SetTokenInventory(evidence.SurfaceProtocol, "",
		[]evidence.TokenObservation{{EvidencePath: "/turn/result/usage", Kind: "spend"}},
		evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the inventory completed"}, nil)
	if err == nil {
		t.Error("SetTokenInventory(protocol, \"\", ...) = nil error, want rejection of a resolved path no session is named for")
	}
}
