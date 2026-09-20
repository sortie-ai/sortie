package qualification

import (
	"encoding/json"
	"testing"
)

func measurementDoc() map[string]any {
	return map[string]any{
		"schema_version":    4,
		"profile_digest":    "a-profile-digest",
		"measured_at":       "2026-01-01",
		"expectation":       map[string]any{"Conformance": nil},
		"requested_model":   "gemini-3.5-flash",
		"observed_model":    nil,
		"provenance_digest": nil,
	}
}

func decodeMeasurementDoc(T *testing.T, doc map[string]any) (Measurement, error) {
	T.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		T.Fatalf("json.Marshal(%v) = _, %v, want nil", doc, err)
	}
	return DecodeMeasurement(data)
}

// A runtime may serve a model other than the one requested, so the requested
// and served coordinates cannot share one member.
func TestMeasurementKeepsTheRequestedAndServedModelApart(T *testing.T) {
	T.Parallel()

	measurement, err := decodeMeasurementDoc(T, measurementDoc())
	if err != nil {
		T.Fatalf("DecodeMeasurement(current schema document) = _, %v, want nil", err)
	}
	if measurement.RequestedModel != "gemini-3.5-flash" {
		T.Errorf("requested model = %q, want %q", measurement.RequestedModel, "gemini-3.5-flash")
	}
	if measurement.ObservedModel != nil {
		T.Errorf("served model = %q, want unfilled: nothing read the model the runtime reported serving", *measurement.ObservedModel)
	}
}

// A bare model member is what let a requested coordinate be read as the
// model that served the turn.
func TestMeasurementRejectsAnUnqualifiedModelMember(T *testing.T) {
	T.Parallel()

	doc := measurementDoc()
	delete(doc, "requested_model")
	doc["model"] = "gemini-3.5-flash"

	if _, err := decodeMeasurementDoc(T, doc); err == nil {
		T.Error("DecodeMeasurement(document carrying a bare model member) = _, nil, want a rejection")
	}
}

func TestMeasurementLinksTheRunsOwnProvenance(T *testing.T) {
	T.Parallel()

	doc := measurementDoc()
	doc["provenance_digest"] = "a-provenance-digest"
	measurement, err := decodeMeasurementDoc(T, doc)
	if err != nil {
		T.Fatalf("DecodeMeasurement(document linking its provenance) = _, %v, want nil", err)
	}
	if measurement.ProvenanceDigest == nil || *measurement.ProvenanceDigest != "a-provenance-digest" {
		T.Errorf("provenance digest = %v, want %q", measurement.ProvenanceDigest, "a-provenance-digest")
	}

	missing := measurementDoc()
	delete(missing, "provenance_digest")
	if _, err := decodeMeasurementDoc(T, missing); err == nil {
		T.Error("DecodeMeasurement(document stating no provenance member) = _, nil, want a rejection")
	}

	empty := measurementDoc()
	empty["provenance_digest"] = ""
	if _, err := decodeMeasurementDoc(T, empty); err == nil {
		T.Error("DecodeMeasurement(document carrying an empty provenance digest) = _, nil, want a rejection")
	}
}
