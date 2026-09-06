package qualification

import (
	"encoding/json"
	"testing"
)

// TestFixtureDeclarationsRoundTrip confirms Fixture.Declarations
// produces a document the operator's own decode path would accept: it
// marshals to JSON and back through DecodeDeclarationSet without
// error, and the decoded set carries the same entries the fixture's
// declared_gap records were built from. A fixture that authorized a
// document the decoder would refuse would let every control that
// passes Declarations() straight to ValidateObservationsWithDeclarations
// hide a decoder-rejected document behind an in-memory struct.
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

// requireDeclarationsRoundTrip fails t unless declarations marshals and
// decodes back through DecodeDeclarationSet to an equal set.
func requireDeclarationsRoundTrip(t *testing.T, declarations DeclarationSet) {
	t.Helper()

	data, err := json.Marshal(declarations)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v, want nil", declarations, err)
	}
	decoded, err := DecodeDeclarationSet(data)
	if err != nil {
		t.Fatalf("DecodeDeclarationSet(%s) error = %v, want the fixture's own declarations to decode", data, err)
	}
	if decoded.SchemaVersion != declarations.SchemaVersion {
		t.Errorf("DecodeDeclarationSet() SchemaVersion = %d, want %d", decoded.SchemaVersion, declarations.SchemaVersion)
	}
	if len(decoded.Declarations) != len(declarations.Declarations) {
		t.Fatalf("DecodeDeclarationSet() = %d declarations, want %d", len(decoded.Declarations), len(declarations.Declarations))
	}
	for i := range declarations.Declarations {
		if decoded.Declarations[i] != declarations.Declarations[i] {
			t.Errorf("DecodeDeclarationSet() entry %d = %+v, want %+v", i, decoded.Declarations[i], declarations.Declarations[i])
		}
	}
}
