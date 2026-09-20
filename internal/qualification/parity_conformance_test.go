package qualification

import (
	"strings"
	"testing"
)

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
