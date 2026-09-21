package evidence

import (
	"encoding/json"
	"testing"
)

func measurementDoc() map[string]any {
	return map[string]any{
		"schema_version":    5,
		"profile_digest":    "a-profile-digest",
		"measured_at":       "2026-01-01",
		"expectation":       map[string]any{"Conformance": nil},
		"requested_model":   "gemini-3.5-flash",
		"observed_model":    nil,
		"provenance_digest": nil,
		"evaluator_version": 1,
	}
}

func decodeMeasurementDoc(t *testing.T, doc map[string]any) (Measurement, error) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal(%v) = _, %v, want nil", doc, err)
	}
	return DecodeMeasurement(data)
}

// A runtime may serve a model other than the one requested, so the requested
// and served coordinates cannot share one member.
func TestMeasurementKeepsTheRequestedAndServedModelApart(t *testing.T) {
	t.Parallel()

	measurement, err := decodeMeasurementDoc(t, measurementDoc())
	if err != nil {
		t.Fatalf("DecodeMeasurement(current schema document) = _, %v, want nil", err)
	}
	if measurement.RequestedModel != "gemini-3.5-flash" {
		t.Errorf("requested model = %q, want %q", measurement.RequestedModel, "gemini-3.5-flash")
	}
	if measurement.ObservedModel != nil {
		t.Errorf("served model = %q, want unfilled: nothing read the model the runtime reported serving", *measurement.ObservedModel)
	}
}

// A bare model member is what let a requested coordinate be read as the
// model that served the turn.
func TestMeasurementRejectsAnUnqualifiedModelMember(t *testing.T) {
	t.Parallel()

	doc := measurementDoc()
	delete(doc, "requested_model")
	doc["model"] = "gemini-3.5-flash"

	if _, err := decodeMeasurementDoc(t, doc); err == nil {
		t.Error("DecodeMeasurement(document carrying a bare model member) = _, nil, want a rejection")
	}
}

func TestMeasurementLinksTheRunsOwnProvenance(t *testing.T) {
	t.Parallel()

	doc := measurementDoc()
	doc["provenance_digest"] = "a-provenance-digest"
	measurement, err := decodeMeasurementDoc(t, doc)
	if err != nil {
		t.Fatalf("DecodeMeasurement(document linking its provenance) = _, %v, want nil", err)
	}
	if measurement.ProvenanceDigest == nil || *measurement.ProvenanceDigest != "a-provenance-digest" {
		t.Errorf("provenance digest = %v, want %q", measurement.ProvenanceDigest, "a-provenance-digest")
	}

	missing := measurementDoc()
	delete(missing, "provenance_digest")
	if _, err := decodeMeasurementDoc(t, missing); err == nil {
		t.Error("DecodeMeasurement(document stating no provenance member) = _, nil, want a rejection")
	}

	empty := measurementDoc()
	empty["provenance_digest"] = ""
	if _, err := decodeMeasurementDoc(t, empty); err == nil {
		t.Error("DecodeMeasurement(document carrying an empty provenance digest) = _, nil, want a rejection")
	}
}

func TestMeasurementSchemaVersionGate(t *testing.T) {
	t.Parallel()

	t.Run("schema_version 4 is rejected", func(t *testing.T) {
		t.Parallel()

		doc := measurementDoc()
		doc["schema_version"] = 4
		if _, err := decodeMeasurementDoc(t, doc); err == nil {
			t.Error("DecodeMeasurement(schema_version 4) = _, nil, want rejection: the document carries no evaluator_version under schema 4")
		}
	})

	t.Run("schema_version 5 with evaluator_version is accepted", func(t *testing.T) {
		t.Parallel()

		doc := measurementDoc()
		measurement, err := decodeMeasurementDoc(t, doc)
		if err != nil {
			t.Fatalf("DecodeMeasurement(schema_version 5) = _, %v, want nil", err)
		}
		if measurement.SchemaVersion != MeasurementSchemaVersion {
			t.Errorf("SchemaVersion = %d, want %d", measurement.SchemaVersion, MeasurementSchemaVersion)
		}
		if measurement.EvaluatorVersion != 1 {
			t.Errorf("EvaluatorVersion = %d, want 1", measurement.EvaluatorVersion)
		}
	})

	t.Run("missing evaluator_version is rejected", func(t *testing.T) {
		t.Parallel()

		doc := measurementDoc()
		delete(doc, "evaluator_version")
		if _, err := decodeMeasurementDoc(t, doc); err == nil {
			t.Error("DecodeMeasurement(document missing evaluator_version) = _, nil, want rejection")
		}
	})

	t.Run("unknown field is rejected", func(t *testing.T) {
		t.Parallel()

		doc := measurementDoc()
		doc["extra_field"] = "value"
		if _, err := decodeMeasurementDoc(t, doc); err == nil {
			t.Error("DecodeMeasurement(document carrying an unknown field) = _, nil, want rejection")
		}
	})
}
