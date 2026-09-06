package qualification

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestEvidenceRecordStrictDecode confirms a schema-valid record decodes
// to its exact field values, that every nullable field accepts null,
// and that the decode round trip preserves the record.
func TestEvidenceRecordStrictDecode(t *testing.T) {
	t.Parallel()

	t.Run("full record decodes to its field values", func(t *testing.T) {
		t.Parallel()

		want := ValidRecord()
		line, err := MarshalRecord(want)
		if err != nil {
			t.Fatalf("MarshalRecord() error = %v", err)
		}
		got, err := DecodeRecord(line)
		if err != nil {
			t.Fatalf("DecodeRecord(%s) error = %v", line, err)
		}
		if !RecordsEqual(got, want) {
			t.Errorf("DecodeRecord() = %+v, want %+v", got, want)
		}
	})

	t.Run("every field carries exactly its closed wire name", func(t *testing.T) {
		t.Parallel()

		line, err := MarshalRecord(ValidRecord())
		if err != nil {
			t.Fatalf("MarshalRecord() error = %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("unmarshal line %s: %v", line, err)
		}
		if len(fields) != len(recordFields) {
			t.Errorf("field count = %d, want %d", len(fields), len(recordFields))
		}
		for name := range fields {
			if !recordFields[name] {
				t.Errorf("marshaled record carries unexpected field %q", name)
			}
		}
	})

	t.Run("nullable fields decode null", func(t *testing.T) {
		t.Parallel()

		rec := ValidRecord()
		rec.SemanticCase = nil
		rec.EvidencePath = nil
		rec.SessionID = nil
		rec.PriorSessionID = nil
		rec.AgentName = nil
		rec.AgentVersion = nil
		rec.ProtocolVersion = nil
		line, err := MarshalRecord(rec)
		if err != nil {
			t.Fatalf("MarshalRecord() error = %v", err)
		}
		got, err := DecodeRecord(line)
		if err != nil {
			t.Fatalf("DecodeRecord(%s) error = %v", line, err)
		}
		if got.SemanticCase != nil || got.EvidencePath != nil || got.SessionID != nil ||
			got.PriorSessionID != nil || got.AgentName != nil || got.AgentVersion != nil ||
			got.ProtocolVersion != nil {
			t.Errorf("DecodeRecord() = %+v, want all nullable fields nil", got)
		}
	})
}

// TestEvidenceRecordRejectsUnknownAndMissingFields confirms the decoder
// rejects any field outside the closed set and any missing member, one
// control per field.
func TestEvidenceRecordRejectsUnknownAndMissingFields(t *testing.T) {
	t.Parallel()

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()

		line, err := MarshalRecord(ValidRecord())
		if err != nil {
			t.Fatalf("MarshalRecord() error = %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("unmarshal line %s: %v", line, err)
		}
		fields["extra_field"] = json.RawMessage(`"value"`)
		doctored, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal doctored fields: %v", err)
		}
		if _, err := DecodeRecord(doctored); err == nil {
			t.Errorf("DecodeRecord(%s) = nil error, want unknown-field error", doctored)
		} else if !strings.Contains(err.Error(), `unknown field "extra_field"`) {
			t.Errorf("DecodeRecord() error = %v, want it to name the unknown field", err)
		}
	})

	for name := range recordFields {
		t.Run("missing "+name, func(t *testing.T) {
			t.Parallel()

			line, err := MarshalRecord(ValidRecord())
			if err != nil {
				t.Fatalf("MarshalRecord() error = %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(line, &fields); err != nil {
				t.Fatalf("unmarshal line %s: %v", line, err)
			}
			delete(fields, name)
			doctored, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("marshal doctored fields: %v", err)
			}
			if _, err := DecodeRecord(doctored); err == nil {
				t.Errorf("DecodeRecord(%s) = nil error, want missing-field error", doctored)
			} else if !strings.Contains(err.Error(), `missing field "`+name+`"`) {
				t.Errorf("DecodeRecord() error = %v, want it to name %q", err, name)
			}
		})
	}

	t.Run("type and enum violations", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			doctor  func(rec Record) map[string]json.RawMessage
			wantSub string
		}{
			{
				name: "schema_version zero is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					rec.SchemaVersion = 0
					return marshalRecordFields(t, rec)
				},
				wantSub: "schema_version",
			},
			{
				name: "schema_version two is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					rec.SchemaVersion = 2
					return marshalRecordFields(t, rec)
				},
				wantSub: "schema_version",
			},
			{
				name: "null required enum is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["scenario"] = json.RawMessage(`null`)
					return fields
				},
				wantSub: "scenario",
			},
			{
				name: "value outside the scenario set is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["scenario"] = json.RawMessage(`"not_a_scenario"`)
					return fields
				},
				wantSub: "outside the closed value set",
			},
			{
				name: "value outside the grade set is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["grade"] = json.RawMessage(`"excellent"`)
					return fields
				},
				wantSub: "outside the closed value set",
			},
			{
				name: "null semantic_case on a semantic record is accepted but null outcome is not",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["outcome"] = json.RawMessage(`null`)
					return fields
				},
				wantSub: "outcome",
			},
			{
				name: "string protocol_version is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["protocol_version"] = json.RawMessage(`"1"`)
					return fields
				},
				wantSub: "protocol_version",
			},
			{
				name: "null integer is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["sequence"] = json.RawMessage(`null`)
					return fields
				},
				wantSub: "sequence",
			},
			{
				name: "fractional sequence is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["sequence"] = json.RawMessage(`1.5`)
					return fields
				},
				wantSub: "sequence",
			},
			{
				name: "non-UTC timestamp is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					rec.ObservedAt = "2026-01-01T00:00:00+02:00"
					return marshalRecordFields(t, rec)
				},
				wantSub: "not UTC",
			},
			{
				name: "unparseable timestamp is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					rec.ObservedAt = "not-a-time"
					return marshalRecordFields(t, rec)
				},
				wantSub: "observed_at",
			},
			{
				name: "null detail is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					fields := marshalRecordFields(t, rec)
					fields["detail"] = json.RawMessage(`null`)
					return fields
				},
				wantSub: "detail",
			},
			{
				name: "array payload is rejected",
				doctor: func(rec Record) map[string]json.RawMessage {
					return map[string]json.RawMessage{"detail": json.RawMessage(`[]`)}
				},
				wantSub: "missing field",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				line, err := json.Marshal(tt.doctor(ValidRecord()))
				if err != nil {
					t.Fatalf("marshal doctored fields: %v", err)
				}
				_, err = DecodeRecord(line)
				if err == nil {
					t.Errorf("DecodeRecord(%s) = nil error, want error naming %q", line, tt.wantSub)
				} else if !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("DecodeRecord() error = %v, want it to mention %q", err, tt.wantSub)
				}
			})
		}
	})
}

// marshalRecordFields renders a record through its wire member names so
// a test can doctor individual fields while keeping the field set exact.
func marshalRecordFields(t *testing.T, rec Record) map[string]json.RawMessage {
	t.Helper()
	line, err := MarshalRecord(rec)
	if err != nil {
		t.Fatalf("MarshalRecord() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("unmarshal line %s: %v", line, err)
	}
	return fields
}

// TestDecodeRecordWidenedVocabulary confirms DecodeRecord accepts a
// record carrying each grade and outcome this change adds, as long as
// the value is a member of the widened closed sets, and still rejects
// a value outside them.
func TestDecodeRecordWidenedVocabulary(t *testing.T) {
	t.Parallel()

	t.Run("accepts every new grade and outcome value", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			grade   Grade
			outcome Outcome
		}{
			{"declared_gap grade pairs with not_producible", GradeDeclaredGap, OutcomeNotProducible},
			{"not_inducible grade pairs with not_inducible", GradeNotInducible, OutcomeNotInducible},
			{"unmeasured grade is decodable", GradeUnmeasured, OutcomePass},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := ValidRecord()
				rec.Grade = tt.grade
				rec.Outcome = tt.outcome
				line, err := MarshalRecord(rec)
				if err != nil {
					t.Fatalf("MarshalRecord() error = %v", err)
				}
				got, err := DecodeRecord(line)
				if err != nil {
					t.Fatalf("DecodeRecord(%s) error = %v, want nil", line, err)
				}
				if got.Grade != tt.grade || got.Outcome != tt.outcome {
					t.Errorf("DecodeRecord() = %s/%s, want %s/%s", got.Grade, got.Outcome, tt.grade, tt.outcome)
				}
			})
		}
	})

	t.Run("still rejects a grade outside the widened closed set", func(t *testing.T) {
		t.Parallel()

		fields := marshalRecordFields(t, ValidRecord())
		fields["grade"] = json.RawMessage(`"excellent"`)
		line, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal doctored fields: %v", err)
		}
		if _, err := DecodeRecord(line); err == nil {
			t.Error("DecodeRecord() = nil error, want rejection of a grade outside the widened set")
		}
	})

	t.Run("still rejects an outcome outside the widened closed set", func(t *testing.T) {
		t.Parallel()

		fields := marshalRecordFields(t, ValidRecord())
		fields["outcome"] = json.RawMessage(`"mystery_outcome"`)
		line, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal doctored fields: %v", err)
		}
		if _, err := DecodeRecord(line); err == nil {
			t.Error("DecodeRecord() = nil error, want rejection of an outcome outside the widened set")
		}
	})
}

// TestDecodeRecordRejectsEmptyNullableString confirms decodeNullableString's
// rejection reaches all five nullable string fields DecodeRecord decodes
// through it, that null still decodes to a nil pointer on each, and that
// a non-empty value still round trips.
func TestDecodeRecordRejectsEmptyNullableString(t *testing.T) {
	t.Parallel()

	fields := []string{"evidence_path", "session_id", "prior_session_id", "agent_name", "agent_version"}

	for _, name := range fields {
		t.Run(name+" empty string is rejected", func(t *testing.T) {
			t.Parallel()

			doctored := marshalRecordFields(t, ValidRecord())
			doctored[name] = json.RawMessage(`""`)
			line, err := json.Marshal(doctored)
			if err != nil {
				t.Fatalf("marshal doctored fields: %v", err)
			}
			_, err = DecodeRecord(line)
			if err == nil {
				t.Fatalf("DecodeRecord(%s) = nil error, want rejection of an empty %s", line, name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("DecodeRecord() error = %v, want it to name %q", err, name)
			}
		})

		t.Run(name+" null still decodes to nil", func(t *testing.T) {
			t.Parallel()

			doctored := marshalRecordFields(t, ValidRecord())
			doctored[name] = json.RawMessage(`null`)
			line, err := json.Marshal(doctored)
			if err != nil {
				t.Fatalf("marshal doctored fields: %v", err)
			}
			got, err := DecodeRecord(line)
			if err != nil {
				t.Fatalf("DecodeRecord(%s) error = %v, want nil for a null %s", line, err, name)
			}
			if nullableStringField(t, got, name) != nil {
				t.Errorf("DecodeRecord() field %s = %v, want nil", name, nullableStringField(t, got, name))
			}
		})

		t.Run(name+" a non-empty value round trips", func(t *testing.T) {
			t.Parallel()

			doctored := marshalRecordFields(t, ValidRecord())
			doctored[name] = json.RawMessage(`"observed-value"`)
			line, err := json.Marshal(doctored)
			if err != nil {
				t.Fatalf("marshal doctored fields: %v", err)
			}
			got, err := DecodeRecord(line)
			if err != nil {
				t.Fatalf("DecodeRecord(%s) error = %v, want nil for a non-empty %s", line, err, name)
			}
			field := nullableStringField(t, got, name)
			if field == nil || *field != "observed-value" {
				t.Errorf("DecodeRecord() field %s = %v, want a pointer to %q", name, field, "observed-value")
			}
		})
	}
}

// nullableStringField reads one of Record's five nullable string
// fields by its wire name, so the table above can assert on any of
// them without a per-field switch at each call site.
func nullableStringField(t *testing.T, rec Record, name string) *string {
	t.Helper()
	switch name {
	case "evidence_path":
		return rec.EvidencePath
	case "session_id":
		return rec.SessionID
	case "prior_session_id":
		return rec.PriorSessionID
	case "agent_name":
		return rec.AgentName
	case "agent_version":
		return rec.AgentVersion
	}
	t.Fatalf("nullableStringField: unknown field %q", name)
	return nil
}

// TestRowGradesClosedSetTotality confirms RowGrades is a subset of
// Grades and excludes exactly the three eligibility-only grades.
func TestRowGradesClosedSetTotality(t *testing.T) {
	t.Parallel()

	for _, grade := range RowGrades {
		if !slices.Contains(Grades, grade) {
			t.Errorf("RowGrades contains %s, which Grades does not carry", grade)
		}
	}

	eligibilityOnly := []Grade{GradeQualified, GradeNotQualified, GradeUnmeasured}
	for _, grade := range eligibilityOnly {
		if slices.Contains(RowGrades, grade) {
			t.Errorf("RowGrades contains eligibility-only grade %s, want it excluded", grade)
		}
	}
	if want := len(Grades) - len(eligibilityOnly); len(RowGrades) != want {
		t.Errorf("RowGrades holds %d members, want %d (Grades minus the three eligibility-only grades)", len(RowGrades), want)
	}
}

// TestEvidenceRecordDetailBound confirms the detail field accepts
// exactly 256 Unicode code points regardless of byte width and rejects
// an empty detail or one past the bound.
func TestEvidenceRecordDetailBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		detail  string
		wantErr bool
	}{
		{name: "one code point", detail: "x", wantErr: false},
		{name: "exactly 256 ASCII code points", detail: strings.Repeat("d", 256), wantErr: false},
		{name: "257 ASCII code points", detail: strings.Repeat("d", 257), wantErr: true},
		{name: "exactly 256 multibyte code points", detail: strings.Repeat("é", 256), wantErr: false},
		{name: "257 multibyte code points", detail: strings.Repeat("é", 257), wantErr: true},
		{name: "empty detail", detail: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ValidRecord()
			rec.Detail = tt.detail
			line, err := MarshalRecord(rec)
			if err != nil {
				t.Fatalf("MarshalRecord() error = %v", err)
			}
			_, err = DecodeRecord(line)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("DecodeRecord() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
