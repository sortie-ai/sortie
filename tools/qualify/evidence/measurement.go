package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// MeasurementSchemaVersion is the version of the measurement document
// DecodeMeasurement accepts. It bumps whenever the document's member set
// changes.
const MeasurementSchemaVersion = 5

// Measurement is the tracked artifact one live run or offline re-derivation
// produces. Its date is machine-read data, not notes prose, because
// ValidateNotes rejects a notes line carrying one.
type Measurement struct {
	SchemaVersion int              `json:"schema_version"`
	ProfileDigest string           `json:"profile_digest"`
	MeasuredAt    string           `json:"measured_at"`
	Expectation   NotesExpectation `json:"expectation"`
	// RequestedModel is the model coordinate the run asked for. A runtime may
	// serve a different one, so this states the request, not the answer.
	RequestedModel string `json:"requested_model"`
	// ObservedModel is the model the runtime reported serving. Null while no
	// reader produces one; defaulting it to the request would republish the
	// request as an observation.
	ObservedModel *string `json:"observed_model"`
	// ProvenanceDigest is the sha256 of the provenance document the same run
	// published. Without it a measurement stands only on a profile digest and a
	// date, which two runs of different collector builds share.
	ProvenanceDigest *string `json:"provenance_digest"`
	// EvaluatorVersion is the evaluator build that derived this document from
	// the capture provenance_digest names.
	EvaluatorVersion int `json:"evaluator_version"`
}

// measurementFieldOrder is the exact set of member names a Measurement
// document may carry at the top level.
var measurementFieldOrder = []string{
	"schema_version", "profile_digest", "measured_at", "expectation",
	"requested_model", "observed_model", "provenance_digest",
	"evaluator_version",
}

var measurementFields = func() map[string]bool {
	fields := make(map[string]bool, len(measurementFieldOrder))
	for _, name := range measurementFieldOrder {
		fields[name] = true
	}
	return fields
}()

// DecodeMeasurement strictly decodes a Measurement document, rejecting an
// unknown or missing field, a wrong schema_version, and an omitted nullable member.
func DecodeMeasurement(data []byte) (Measurement, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return Measurement{}, fmt.Errorf("decode measurement: %w", err)
	}
	for name := range top {
		if !measurementFields[name] {
			return Measurement{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range measurementFieldOrder {
		if _, ok := top[name]; !ok {
			return Measurement{}, fmt.Errorf("missing field %q", name)
		}
	}

	var measurement Measurement
	if err := json.Unmarshal(top["schema_version"], &measurement.SchemaVersion); err != nil {
		return Measurement{}, fmt.Errorf("schema_version: %w", err)
	}
	if measurement.SchemaVersion != MeasurementSchemaVersion {
		return Measurement{}, fmt.Errorf("schema_version = %d, want %d", measurement.SchemaVersion, MeasurementSchemaVersion)
	}
	if err := json.Unmarshal(top["profile_digest"], &measurement.ProfileDigest); err != nil {
		return Measurement{}, fmt.Errorf("profile_digest: %w", err)
	}
	if measurement.ProfileDigest == "" {
		return Measurement{}, errors.New("profile_digest must be non-empty")
	}
	if err := json.Unmarshal(top["measured_at"], &measurement.MeasuredAt); err != nil {
		return Measurement{}, fmt.Errorf("measured_at: %w", err)
	}
	if measurement.MeasuredAt == "" {
		return Measurement{}, errors.New("measured_at must be non-empty")
	}
	if err := json.Unmarshal(top["expectation"], &measurement.Expectation); err != nil {
		return Measurement{}, fmt.Errorf("expectation: %w", err)
	}
	if err := requireStatedProductAnswer(top["expectation"]); err != nil {
		return Measurement{}, fmt.Errorf("expectation: %w", err)
	}
	if err := json.Unmarshal(top["requested_model"], &measurement.RequestedModel); err != nil {
		return Measurement{}, fmt.Errorf("requested_model: %w", err)
	}
	if measurement.RequestedModel == "" {
		return Measurement{}, errors.New("requested_model must be non-empty")
	}
	var err error
	if measurement.ObservedModel, err = decodeStatedString(top["observed_model"]); err != nil {
		return Measurement{}, fmt.Errorf("observed_model: %w", err)
	}
	if measurement.ProvenanceDigest, err = decodeStatedString(top["provenance_digest"]); err != nil {
		return Measurement{}, fmt.Errorf("provenance_digest: %w", err)
	}
	if err := json.Unmarshal(top["evaluator_version"], &measurement.EvaluatorVersion); err != nil {
		return Measurement{}, fmt.Errorf("evaluator_version: %w", err)
	}
	return measurement, nil
}

// requireStatedProductAnswer rejects an expectation that says nothing about
// product conformance. Silence would be indistinguishable from a run that
// answered both questions and published one verdict for both.
func requireStatedProductAnswer(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	if _, stated := members["Conformance"]; !stated {
		return errors.New("no Conformance member; state null when the run answered only the transport question")
	}
	return nil
}

// decodeStatedString decodes a member that is either null or a
// non-empty string. An empty string is rejected: it reads as a stated
// value while naming nothing.
func decodeStatedString(raw json.RawMessage) (*string, error) {
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value != nil && *value == "" {
		return nil, errors.New("a stated value must be non-empty; state null when there is none")
	}
	return value, nil
}

// ReadMeasurementFile reads and strictly decodes a Measurement file.
func ReadMeasurementFile(path string) (Measurement, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a repository-relative path resolved from the tracked profile
	if err != nil {
		return Measurement{}, err
	}
	return DecodeMeasurement(data)
}
