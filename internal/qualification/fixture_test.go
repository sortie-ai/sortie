package qualification

import (
	"encoding/json"
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
		EntryPoints:         map[Surface]EntryPoint{SurfaceProtocol: {Args: []string{"--acp"}}},
		Recognizers:         map[Surface]Recognizer{},
		Declarations:        declarations.Declarations,
		AbsentSurfaces:      declarations.AbsentSurfaces,
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
