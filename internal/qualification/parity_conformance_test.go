package qualification

import (
	"strings"
	"testing"
)

// publishedReport derives the report from the bytes read back off disk, not
// the in-memory fixture: the defect these controls close survived because the
// two disagreed.
func publishedReport(T *testing.T, records []Record, profile RuntimeProfile) EligibilityReport {
	T.Helper()
	path := WriteEvidenceFile(T, records)
	published, err := ReadEvidenceFile(path)
	if err != nil {
		T.Fatalf("read back the published evidence: %v", err)
	}
	if _, err := ValidateObservationsWithDeclarations(path, profile); err != nil {
		T.Fatalf("validate the published evidence: %v", err)
	}
	return ExplainEligibility(published, profile)
}

func publishedBaseline(T *testing.T, records []Record, surface Surface, capability Capability) Grade {
	T.Helper()
	path := WriteEvidenceFile(T, records)
	published, err := ReadEvidenceFile(path)
	if err != nil {
		T.Fatalf("read back the published evidence: %v", err)
	}
	for i := range published {
		if MatchBaseline(surface, capability)(&published[i]) {
			return published[i].Grade
		}
	}
	T.Fatalf("no published %s %s baseline", surface, capability)
	return ""
}

func noTokenSourceAnywhere(T *testing.T) *Fixture {
	T.Helper()
	fixture := NewFixture(FixtureQualified)
	for _, surface := range []Surface{SurfaceProtocol, SurfaceNativeJSON, SurfaceNativeStreamJSON} {
		fixture.SetTokenSentinel(surface, false)
	}
	fixture.Finalize()
	return fixture
}

func TestPublishedArtifactAnswersBothQuestions(T *testing.T) {
	T.Parallel()

	fixture := noTokenSourceAnywhere(T)
	report := publishedReport(T, fixture.Records, fixture.Declarations())

	if report.Verdict != VerdictQualified {
		T.Errorf("transport parity = %s, want qualified: the protocol route loses nothing the native route gave", report.Verdict)
	}
	if report.Conformance != VerdictNotQualified {
		T.Errorf("product conformance = %s, want not_qualified: the token ceiling works on no route at all", report.Conformance)
	}

	row := rowFor(report, CapabilityTokenCeiling)
	if row.Standing != StandingSatisfied {
		T.Errorf("token_ceiling parity standing = %s, want satisfied", row.Standing)
	}
	if row.Conformance != StandingBelow {
		T.Errorf("token_ceiling conformance standing = %s, want below", row.Conformance)
	}
	if !strings.Contains(row.ConformanceCause, string(GradeGap)) {
		T.Errorf("token_ceiling conformance cause = %q, want it to name the shortfall", row.ConformanceCause)
	}
}

func TestSharedShortfallFailsConformanceOnEveryRuntimeShape(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified,
		AbsentSurface{Surface: SurfaceNativeJSON, Reason: SurfaceNotOffered},
		AbsentSurface{Surface: SurfaceNativeStreamJSON, Reason: SurfaceNotOffered})
	fixture.SetTokenSentinel(SurfaceProtocol, false)
	fixture.Finalize()

	report := publishedReport(T, fixture.Records, fixture.Declarations())
	if report.Verdict != VerdictQualified {
		T.Errorf("transport parity = %s, want qualified: with no native surface there is nothing to be below", report.Verdict)
	}
	if report.Conformance != VerdictNotQualified {
		T.Errorf("product conformance = %s, want not_qualified: having no native route to lose against is not having the capability", report.Conformance)
	}
}

func TestRemovedObservationCannotImproveConformance(T *testing.T) {
	T.Parallel()

	profile := caseLevelProfile(
		SurfaceNotInducible{Surface: SurfaceProtocol, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
		SurfaceNotInducible{Surface: SurfaceNativeJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
		SurfaceNotInducible{Surface: SurfaceNativeStreamJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
	)
	// Set before Finalize so no identity record is written for a session
	// the not-inducible shape removes.
	fixture := NewFixture(FixtureQualified)
	for _, surface := range []Surface{SurfaceProtocol, SurfaceNativeJSON, SurfaceNativeStreamJSON} {
		fixture.SetSemanticNotInducible(surface, CapabilityTurnDisposition, CaseLimitReached, NotInducibleChannelTooSmall)
	}
	fixture.Finalize()

	row := rowFor(publishedReport(T, fixture.Records, profile), CapabilityTurnDisposition)
	if row.Standing != StandingSatisfied {
		T.Errorf("turn_disposition parity standing = %s (cause %q), want satisfied: no side carries the case", row.Standing, row.Cause)
	}
	if row.Conformance != StandingUnmeasured {
		T.Errorf("turn_disposition conformance standing = %s (cause %q), want unmeasured: nobody induced the case, so nothing supports calling the obligation met", row.Conformance, row.ConformanceCause)
	}
	if !strings.Contains(row.ConformanceCause, string(CaseLimitReached)) {
		T.Errorf("conformance cause = %q, want it to name the case nobody measured", row.ConformanceCause)
	}
	if !strings.Contains(row.ConformanceCause, NotInducibleChannelTooSmall) {
		T.Errorf("conformance cause = %q, want it to name why the case went unmeasured: a condition no measurer can reach is not one the surface declined to report", row.ConformanceCause)
	}
}

func TestChannelSilenceIsAConformanceShortfall(T *testing.T) {
	T.Parallel()

	profile := caseLevelProfile(SurfaceNotInducible{
		Surface: SurfaceProtocol,
		Case:    CaseRuntimeFailure,
		Reason:  NotInducibleOutputSilentOnFailure,
	})
	fixture := NewFixture(FixtureQualified)
	fixture.SetSemanticNotInducible(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeFailure, NotInducibleOutputSilentOnFailure)
	fixture.Finalize()

	row := rowFor(publishedReport(T, fixture.Records, profile), CapabilityTurnDisposition)
	if row.Conformance != StandingBelow {
		T.Errorf("turn_disposition conformance standing = %s (cause %q), want below: the condition arises and the adapter reports nothing about it", row.Conformance, row.ConformanceCause)
	}
	if !strings.Contains(row.ConformanceCause, NotInducibleOutputSilentOnFailure) {
		T.Errorf("conformance cause = %q, want it to name the silent channel", row.ConformanceCause)
	}
}

func TestDeclaredGapCarriesNoConformanceObligation(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureDeclaredGap)
	fixture.Finalize()

	report := publishedReport(T, fixture.Records, fixture.Declarations())
	row := rowFor(report, CapabilityTurnDisposition)
	if row.Conformance != StandingSatisfied {
		T.Errorf("turn_disposition conformance standing = %s (cause %q), want satisfied: a declared gap is an outcome the runtime never produces", row.Conformance, row.ConformanceCause)
	}
	if report.Conformance != VerdictQualified {
		T.Errorf("product conformance = %s, want qualified", report.Conformance)
	}
}

func TestCompensationReachesTheProductAnswerWithoutFlatteringTheTransport(T *testing.T) {
	T.Parallel()

	// A corroboration-only reading is the shape a compensated baseline would
	// flatter: one usable record turns the whole surface usable.
	fixture := NewFixture(FixtureQualified)
	fixture.SetTokenCorroborationOnly(SurfaceProtocol)
	fixture.Finalize()

	before := publishedReport(T, fixture.Records, fixture.Declarations())
	if before.Verdict != VerdictNotQualified {
		T.Fatalf("transport parity before compensation = %s, want not_qualified", before.Verdict)
	}
	if before.Conformance != VerdictNotQualified {
		T.Fatalf("product conformance before compensation = %s, want not_qualified", before.Conformance)
	}

	fixture.SetTokenCompensated("sortie/session/turn/usage")

	after := publishedReport(T, fixture.Records, fixture.Declarations())
	if after.Conformance != VerdictUnmeasured {
		T.Errorf("product conformance = %s, want unmeasured: Sortie's own code supplies the spend, and no run crossed a ceiling", after.Conformance)
	}
	row := rowFor(after, CapabilityTokenCeiling)
	if row.Conformance != StandingUnmeasured {
		T.Errorf("token_ceiling conformance standing = %s (cause %q), want unmeasured", row.Conformance, row.ConformanceCause)
	}
	if after.Verdict != VerdictNotQualified {
		T.Errorf("transport parity = %s, want not_qualified: the wire still carries nothing, and compensating for it must not read as the protocol carrying it", after.Verdict)
	}
	if grade := publishedBaseline(T, fixture.Records, SurfaceProtocol, CapabilityTokenCeiling); grade != GradeGap {
		T.Errorf("published protocol token_ceiling baseline = %s, want gap: the baseline states what the surface reported", grade)
	}
}

func TestCorroboratingCompensationDoesNotMeetConformance(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.SetTokenSentinel(SurfaceProtocol, false)
	fixture.Finalize()
	fixture.SetTokenCompensated("sortie/session/turn/usage")
	compensating := fixture.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioTokenSource && SuppliedOutsideProtocol(rec.Source)
	})
	if compensating == nil {
		T.Fatal("no compensating token record was written")
	}
	compensating.Grade = GradeCorroborationOnly

	report := publishedReport(T, fixture.Records, fixture.Declarations())
	if report.Conformance != VerdictNotQualified {
		T.Errorf("product conformance = %s, want not_qualified: a corroborating reading is not a working ceiling", report.Conformance)
	}
}

func TestQuestionRationaleIsTotalAndPrescribesNoAbsentIntegration(T *testing.T) {
	T.Parallel()

	for _, question := range Questions {
		for _, verdict := range Verdicts {
			line := QuestionRationale(question, verdict)
			if line == "" {
				T.Errorf("QuestionRationale(%s, %s) is empty", question, verdict)
				continue
			}
			for _, banned := range []string{"existing integration", "stays on", "remains on"} {
				if strings.Contains(line, banned) {
					T.Errorf("QuestionRationale(%s, %s) = %q, want no advice to keep an integration that may not exist", question, verdict, line)
				}
			}
		}
	}
	for _, verdict := range Verdicts {
		if got, want := VerdictRationale(verdict), QuestionRationale(QuestionTransportParity, verdict); got != want {
			T.Errorf("VerdictRationale(%s) = %q, want the transport parity line %q", verdict, got, want)
		}
	}
}

func TestCompensationIsNotAnInventory(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.SetTokenSentinel(SurfaceProtocol, false)
	fixture.Finalize()
	fixture.SetTokenCompensated("sortie/session/turn/usage")
	fixture.RemoveAll(func(rec *Record) bool {
		return rec.Scenario == ScenarioTokenSource && rec.Surface == SurfaceProtocol &&
			!SuppliedOutsideProtocol(rec.Source)
	})
	fixture.Renumber()

	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservationsWithDeclarations(path, fixture.Declarations())
	if err == nil {
		T.Fatal("validation accepted a protocol surface whose only token record came from outside the protocol")
	}
	if !strings.Contains(err.Error(), "no token inventory record") {
		T.Errorf("error = %v, want it to name the missing inventory", err)
	}
}

func inventoriedExtension(T *testing.T, reading *ExtensionReading) *Fixture {
	T.Helper()
	fixture := NewFixture(FixtureQualified)
	err := fixture.SetTokenInventory(SurfaceProtocol, "", nil,
		Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: "the wire carried no admissible source"},
		reading)
	if err != nil {
		T.Fatalf("SetTokenInventory(protocol, ...) error = %v, want nil", err)
	}
	fixture.Finalize()
	return fixture
}

func protocolInventoryRecord(T *testing.T, records []Record) Record {
	T.Helper()
	path := WriteEvidenceFile(T, records)
	published, err := ReadEvidenceFile(path)
	if err != nil {
		T.Fatalf("read back the published evidence: %v", err)
	}
	for i := range published {
		if published[i].Scenario == ScenarioTokenSource && published[i].Surface == SurfaceProtocol {
			return published[i]
		}
	}
	T.Fatal("no published protocol token inventory row")
	return Record{}
}

func TestPresentButNotAdmittedIsNotAbsent(T *testing.T) {
	T.Parallel()

	partial := inventoriedExtension(T, &ExtensionReading{Source: ExtensionSourcePresent})
	none := inventoriedExtension(T, &ExtensionReading{Source: ExtensionSourceAbsent})

	partialRow := protocolInventoryRecord(T, partial.Records)
	if partialRow.ExtensionSource == nil || *partialRow.ExtensionSource != ExtensionSourcePresent {
		T.Errorf("published extension_source = %v, want present", partialRow.ExtensionSource)
	}
	if partialRow.ExtensionAdmitted == nil || *partialRow.ExtensionAdmitted {
		T.Errorf("published extension_admitted = %v, want false", partialRow.ExtensionAdmitted)
	}

	noneRow := protocolInventoryRecord(T, none.Records)
	if noneRow.ExtensionSource == nil || *noneRow.ExtensionSource != ExtensionSourceAbsent {
		T.Errorf("published extension_source = %v, want absent", noneRow.ExtensionSource)
	}

	partialCause := rowFor(publishedReport(T, partial.Records, partial.Declarations()), CapabilityTokenCeiling).ConformanceCause
	noneCause := rowFor(publishedReport(T, none.Records, none.Declarations()), CapabilityTokenCeiling).ConformanceCause
	if partialCause == noneCause {
		T.Errorf("both causes read %q; a source that arrived and cannot be spent is not the absence of one", partialCause)
	}
	if !strings.Contains(partialCause, string(ExtensionSourcePresent)) {
		T.Errorf("conformance cause = %q, want it to name the source that arrived", partialCause)
	}
}

func TestExtensionReadingBelongsToTheProtocolInventory(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	rec := fixture.FindFirst(MatchSemantic(SurfaceProtocol, CapabilityTurnDisposition, CaseSuccess))
	if rec == nil {
		T.Fatal("no protocol success semantic record")
	}
	rec.ExtensionSource = new(ExtensionSourcePresent)
	rec.ExtensionAdmitted = new(true)

	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservationsWithDeclarations(path, fixture.Declarations())
	if err == nil || !strings.Contains(err.Error(), "only valid on a protocol token_source record") {
		T.Errorf("error = %v, want a rejection of a reading stated off the inventory", err)
	}
}

func TestExtensionMembersStateEachOther(T *testing.T) {
	T.Parallel()

	for _, tt := range []struct {
		name    string
		mutate  func(rec *Record)
		wantErr string
	}{
		{
			name:    "admission without its source",
			mutate:  func(rec *Record) { rec.ExtensionSource = nil },
			wantErr: "requires the extension_source it judges",
		},
		{
			name:    "source without its admission",
			mutate:  func(rec *Record) { rec.ExtensionAdmitted = nil },
			wantErr: "requires the admission verdict on it",
		},
	} {
		T.Run(tt.name, func(T *testing.T) {
			T.Parallel()
			fixture := inventoriedExtension(T, &ExtensionReading{Source: ExtensionSourcePresent})
			rec := fixture.FindFirst(func(rec *Record) bool {
				return rec.Scenario == ScenarioTokenSource && rec.Surface == SurfaceProtocol
			})
			if rec == nil {
				T.Fatal("no protocol token inventory row")
			}
			tt.mutate(rec)

			path := WriteEvidenceFile(T, fixture.Records)
			_, err := ValidateObservationsWithDeclarations(path, fixture.Declarations())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				T.Errorf("error = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestUnreadSourceCannotBeAdmitted(T *testing.T) {
	T.Parallel()

	fixture := inventoriedExtension(T, &ExtensionReading{Source: ExtensionSourcePresent})
	rec := fixture.FindFirst(func(rec *Record) bool {
		return rec.Scenario == ScenarioTokenSource && rec.Surface == SurfaceProtocol
	})
	if rec == nil {
		T.Fatal("no protocol token inventory row")
	}
	rec.ExtensionSource = new(ExtensionSourceNotObserved)
	rec.ExtensionAdmitted = new(true)

	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservationsWithDeclarations(path, fixture.Declarations())
	if err == nil || !strings.Contains(err.Error(), "cannot be admitted to a budget") {
		T.Errorf("error = %v, want a rejection of an admitted source nobody read", err)
	}
}

func TestNativeSurfaceStatesNoExtensionReading(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	err := fixture.SetTokenInventory(SurfaceNativeJSON, "", nil,
		Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: "the native surface carried no source"},
		&ExtensionReading{Source: ExtensionSourcePresent, Admitted: true})
	if err == nil {
		T.Fatal("SetTokenInventory(native_json, ...) = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "reading of the protocol extension point") {
		T.Errorf("error = %v, want it to name what the native surface cannot state", err)
	}
}

func TestConformanceCauseAccountsOnlyItsOwnCapability(T *testing.T) {
	T.Parallel()

	fixture := inventoriedExtension(T, &ExtensionReading{Source: ExtensionSourcePresent})
	seedRec := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationSeed))
	if seedRec == nil || seedRec.SessionID == nil {
		T.Fatal("no protocol continuation seed record carrying a session")
	}
	session := *seedRec.SessionID
	err := fixture.SetSessionContinuationObserved(SurfaceProtocol,
		Observation{Grade: GradeUsable, Outcome: OutcomePass, SessionID: session, Detail: "the seed turn completed and left history"},
		Observation{Grade: GradeGap, Outcome: OutcomePass, SessionID: session, Detail: RecallSameSessionWithoutRecall})
	if err != nil {
		T.Fatalf("SetSessionContinuationObserved(protocol, ...) error = %v, want nil", err)
	}

	report := publishedReport(T, fixture.Records, fixture.Declarations())

	continuation := rowFor(report, CapabilitySessionContinuation)
	if continuation.Conformance != StandingBelow {
		T.Fatalf("session_continuation conformance standing = %q, want %q: the control needs the row below the reference", continuation.Conformance, StandingBelow)
	}
	if strings.Contains(continuation.ConformanceCause, "extension") {
		T.Errorf("session_continuation conformance cause = %q, want no extension account: the reading belongs to the rows that took it", continuation.ConformanceCause)
	}

	tokens := rowFor(report, CapabilityTokenCeiling)
	if !strings.Contains(tokens.ConformanceCause, string(ExtensionSourcePresent)) {
		T.Errorf("token_ceiling conformance cause = %q, want the extension account its own rows carry", tokens.ConformanceCause)
	}
}
