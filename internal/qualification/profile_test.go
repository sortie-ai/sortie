package qualification

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// validProfileDoc returns a fresh, decode-clean runtime profile
// document as a generic JSON tree, so every field is reachable for
// targeted mutation regardless of nesting depth. Every call allocates
// its own maps and slices: callers mutate the result freely without
// aliasing another subtest's fixture.
func validProfileDoc() map[string]any {
	return map[string]any{
		"schema_version":        3,
		"runtime_id":            "sample-runtime",
		"identity_tokens":       []string{"sample"},
		"notes_path":            "docs/sample-notes.md",
		"measurement_path":      "internal/qualification/probe/testdata/sample/measurement.json",
		"published_sample":      "examples/WORKFLOW.sample.md",
		"tool_name_format":      "mcp_{server}_{tool}",
		"project_config_paths":  []string{".sample/config.json"},
		"version_args":          []string{"--version"},
		"model_args":            []string{"--model", "{model}"},
		"capability_gap_labels": []string{capabilityGapLabelTokenCounts},
		"entry_points": map[string]any{
			"protocol":           map[string]any{"args": []string{"--acp"}},
			"native_json":        map[string]any{"args": []string{"--output-format", "json", "--prompt", "{prompt}"}},
			"native_stream_json": map[string]any{"args": []string{"--output-format", "stream-json", "--prompt", "{prompt}"}},
		},
		"recognizers": map[string]any{
			"native_json": map[string]any{
				"locator":            map[string]any{"mode": "first_value", "discriminator_key": "", "discriminator_value": ""},
				"error_members":      []string{"error"},
				"success_member":     "response",
				"status_member":      "",
				"status_cases":       map[string]any{},
				"status_end_turn":    []string{},
				"model_request_path": []string{},
			},
			"native_stream_json": map[string]any{
				"locator":            map[string]any{"mode": "discriminated", "discriminator_key": "type", "discriminator_value": "result"},
				"error_members":      []string{"error"},
				"success_member":     "",
				"status_member":      "status",
				"status_cases":       map[string]any{"refusal": "runtime_refusal", "cancelled": "cancellation"},
				"status_end_turn":    []string{"end_turn"},
				"model_request_path": []string{"stats", "models"},
			},
		},
		"declarations":    []any{},
		"absent_surfaces": []any{},
	}
}

// cloneProfileDoc round-trips doc through JSON so the clone carries
// its own maps and slices at every depth, leaving doc itself
// untouched for the next subtest.
func cloneProfileDoc(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", doc, err)
	}
	var clone map[string]any
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", data, err)
	}
	return clone
}

// marshalProfileDoc fails t rather than returning an error a caller
// must check, since every caller here treats a marshal failure of an
// in-memory map as a test-fixture bug, not a case under test.
func marshalProfileDoc(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", doc, err)
	}
	return data
}

// TestDecodeRuntimeProfile covers stage D of the validation table: one
// subtest per rejected case, plus a baseline confirming the valid
// fixture itself decodes cleanly.
func TestDecodeRuntimeProfile(t *testing.T) {
	t.Parallel()

	t.Run("a fully valid document decodes with no error", func(t *testing.T) {
		t.Parallel()

		data := marshalProfileDoc(t, validProfileDoc())
		if _, err := DecodeRuntimeProfile(data); err != nil {
			t.Fatalf("DecodeRuntimeProfile(%s) = %v, want nil", data, err)
		}
	})

	tests := []struct {
		name   string
		mutate func(doc map[string]any)
	}{
		{
			name:   "an unknown top-level field is rejected",
			mutate: func(doc map[string]any) { doc["unexpected_field"] = true },
		},
		{
			name:   "a missing top-level field is rejected",
			mutate: func(doc map[string]any) { delete(doc, "declarations") },
		},
		{
			name:   "schema_version other than 3 is rejected",
			mutate: func(doc map[string]any) { doc["schema_version"] = 2 },
		},
		{
			name:   "a non-lowercase runtime_id is rejected",
			mutate: func(doc map[string]any) { doc["runtime_id"] = "Sample-Runtime" },
		},
		{
			name:   "an empty runtime_id is rejected",
			mutate: func(doc map[string]any) { doc["runtime_id"] = "" },
		},
		{
			name:   "empty identity_tokens is rejected",
			mutate: func(doc map[string]any) { doc["identity_tokens"] = []string{} },
		},
		{
			name:   "a non-lowercase identity_tokens entry is rejected",
			mutate: func(doc map[string]any) { doc["identity_tokens"] = []string{"Sample"} },
		},
		{
			name:   "tool_name_format missing a required placeholder is rejected",
			mutate: func(doc map[string]any) { doc["tool_name_format"] = "mcp_{server}" },
		},
		{
			name:   "tool_name_format carrying a placeholder outside the allowed set is rejected",
			mutate: func(doc map[string]any) { doc["tool_name_format"] = "mcp_{server}_{tool}_{extra}" },
		},
		{
			name:   "model_args missing the model placeholder is rejected",
			mutate: func(doc map[string]any) { doc["model_args"] = []string{"--model"} },
		},
		{
			name:   "model_args carrying the model placeholder twice is rejected",
			mutate: func(doc map[string]any) { doc["model_args"] = []string{"--model", "{model}", "--again", "{model}"} },
		},
		{
			name:   "capability_gap_labels omitting token counts is rejected",
			mutate: func(doc map[string]any) { doc["capability_gap_labels"] = []string{"agent version"} },
		},
		{
			name: "capability_gap_labels outside the closed set is rejected",
			mutate: func(doc map[string]any) {
				doc["capability_gap_labels"] = []string{"nonsense label", capabilityGapLabelTokenCounts}
			},
		},
		{
			name: "capability_gap_labels not sorted is rejected",
			mutate: func(doc map[string]any) {
				doc["capability_gap_labels"] = []string{capabilityGapLabelTokenCounts, "agent version"}
			},
		},
		{
			name: "capability_gap_labels carrying a duplicate is rejected",
			mutate: func(doc map[string]any) {
				doc["capability_gap_labels"] = []string{capabilityGapLabelTokenCounts, capabilityGapLabelTokenCounts}
			},
		},
		{
			name: "entry_points missing protocol is rejected",
			mutate: func(doc map[string]any) {
				delete(doc["entry_points"].(map[string]any), "protocol")
			},
		},
		{
			name: "an entry_points asking_args that is empty is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["asking_args"] = []string{}
			},
		},
		{
			name: "an entry_points asking_args entry carrying a placeholder outside the allowed set is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["asking_args"] = []string{"--acp", "{bogus}"}
			},
		},
		{
			// The recognizer goes with it, so the surface's absence
			// from entry_points is the only rule left to reject it.
			name: "entry_points missing a measurable native surface is rejected",
			mutate: func(doc map[string]any) {
				delete(doc["entry_points"].(map[string]any), "native_stream_json")
				delete(doc["recognizers"].(map[string]any), "native_stream_json")
			},
		},
		{
			name: "entry_points naming a surface outside qualification.Surfaces is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["not_a_real_surface"] = map[string]any{"args": []string{"--x"}}
			},
		},
		{
			name: "an entry_points args list that is empty is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["args"] = []string{}
			},
		},
		{
			name: "an entry_points args entry carrying a placeholder outside the allowed set is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["args"] = []string{"--acp", "{bogus}"}
			},
		},
		{
			name: "an entry_points sub-object carrying an unknown field is rejected",
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["extra"] = "surprise"
			},
		},
		{
			name: "recognizers key set missing a structured surface present in entry_points is rejected",
			mutate: func(doc map[string]any) {
				delete(doc["recognizers"].(map[string]any), "native_stream_json")
			},
		},
		{
			name: "recognizers naming protocol is rejected",
			mutate: func(doc map[string]any) {
				recognizers := doc["recognizers"].(map[string]any)
				recognizers["protocol"] = recognizers["native_json"]
			},
		},
		{
			name: "a recognizer entry carrying an unknown field is rejected",
			mutate: func(doc map[string]any) {
				doc["recognizers"].(map[string]any)["native_json"].(map[string]any)["extra"] = "surprise"
			},
		},
		{
			name: "a recognizer entry missing a required field is rejected",
			mutate: func(doc map[string]any) {
				delete(doc["recognizers"].(map[string]any)["native_json"].(map[string]any), "success_member")
			},
		},
		{
			name: "a recognizer's locator carrying an unknown field is rejected",
			mutate: func(doc map[string]any) {
				doc["recognizers"].(map[string]any)["native_json"].(map[string]any)["locator"].(map[string]any)["extra"] = "surprise"
			},
		},
		{
			name: "a discriminated locator missing its discriminator key is rejected",
			mutate: func(doc map[string]any) {
				doc["recognizers"].(map[string]any)["native_stream_json"].(map[string]any)["locator"].(map[string]any)["discriminator_key"] = ""
			},
		},
		{
			name: "status_cases naming a case outside qualification.Cases is rejected",
			mutate: func(doc map[string]any) {
				statusCases := doc["recognizers"].(map[string]any)["native_stream_json"].(map[string]any)["status_cases"].(map[string]any)
				statusCases["bogus_status"] = "not_a_real_case"
			},
		},
		{
			name: "status_end_turn overlapping a status_cases key is rejected",
			mutate: func(doc map[string]any) {
				doc["recognizers"].(map[string]any)["native_stream_json"].(map[string]any)["status_end_turn"] = []string{"refusal"}
			},
		},
		{
			name: "a declaration naming a capability outside CapabilityCases is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "not_a_real_capability", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "a declaration naming a case outside its capability's own set is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "human_input", "reason": DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "a declaration carrying a reason outside the closed value set is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": "made_up_reason"},
				}
			},
		},
		{
			name: "a declaration carrying an unknown field is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced, "extra": "surprise"},
				}
			},
		},
		{
			name: "a duplicate declaration capability-and-case pair is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
					map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": DeclaredGapNeverProduced},
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "declaring a case without its required peer is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "declaring a case and its peer with differing reasons is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
					map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": DeclaredGapFolded},
				}
			},
		},
		{
			name: "an absent_surfaces entry naming a surface outside DeclarableAbsentSurfaces is rejected",
			mutate: func(doc map[string]any) {
				doc["absent_surfaces"] = []any{
					map[string]any{"surface": "protocol", "reason": SurfaceNotOffered},
				}
			},
		},
		{
			name: "an absent_surfaces entry carrying a reason outside the closed value set is rejected",
			mutate: func(doc map[string]any) {
				doc["absent_surfaces"] = []any{
					map[string]any{"surface": "native_json", "reason": "made_up_reason"},
				}
			},
		},
		{
			name: "a duplicate absent_surfaces entry is rejected",
			mutate: func(doc map[string]any) {
				doc["absent_surfaces"] = []any{
					map[string]any{"surface": "native_json", "reason": SurfaceNotOffered},
					map[string]any{"surface": "native_json", "reason": SurfaceNotOffered},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc := cloneProfileDoc(t, validProfileDoc())
			tt.mutate(doc)
			data := marshalProfileDoc(t, doc)

			if _, err := DecodeRuntimeProfile(data); err == nil {
				t.Errorf("DecodeRuntimeProfile(%s) = nil error, want a rejection", data)
			}
		})
	}
}

// TestRuntimeProfileDigest confirms Digest is stable across a
// reformatted encoding of the same decoded value and moves on a
// one-field edit.
func TestRuntimeProfileDigest(t *testing.T) {
	t.Parallel()

	doc := validProfileDoc()
	compact := marshalProfileDoc(t, doc)
	indented, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(%v): %v", doc, err)
	}

	compactProfile, err := DecodeRuntimeProfile(compact)
	if err != nil {
		t.Fatalf("DecodeRuntimeProfile(%s): %v", compact, err)
	}
	indentedProfile, err := DecodeRuntimeProfile(indented)
	if err != nil {
		t.Fatalf("DecodeRuntimeProfile(%s): %v", indented, err)
	}

	if compactProfile.Digest() != indentedProfile.Digest() {
		t.Errorf("Digest() = %q for the compact encoding, %q for the reindented one, want equal", compactProfile.Digest(), indentedProfile.Digest())
	}

	edited := cloneProfileDoc(t, doc)
	edited["version_args"] = []string{"--version", "--verbose"}
	editedProfile, err := DecodeRuntimeProfile(marshalProfileDoc(t, edited))
	if err != nil {
		t.Fatalf("DecodeRuntimeProfile of the edited document: %v", err)
	}

	if editedProfile.Digest() == compactProfile.Digest() {
		t.Errorf("Digest() = %q for both the original and a one-field edit, want it to move", editedProfile.Digest())
	}
}

// mustWriteFile creates path's parent directories as needed and writes
// content, failing t on any error.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}

// validPublishedSample is a minimal workflow front-matter document
// satisfying the published_sample stage-R rule: agent.kind is
// agent-client-protocol and agent.command carries an element past
// element zero.
const validPublishedSample = "---\nagent:\n  kind: agent-client-protocol\n  command: sample-runtime --acp\n---\nbody\n"

// writeFakeRepo writes a synthetic repository root at root: a go.mod
// marker and the three files sampleRuntimeProfileDoc's own paths name,
// so ReadRuntimeProfileFile's stage-R file-existence and published
// sample checks succeed against it.
func writeFakeRepo(t *testing.T, root string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(root, "go.mod"), "module fixture\n\ngo 1.24\n")
	mustWriteFile(t, filepath.Join(root, "docs", "sample-notes.md"), "# sample notes\n")
	mustWriteFile(t, filepath.Join(root, "internal", "qualification", "probe", "testdata", "sample", "measurement.json"), "{}")
	mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.sample.md"), validPublishedSample)
}

// nestedPath joins depth synthetic path segments onto root, so a
// caller can place a fixture file an arbitrary number of directories
// below it.
func nestedPath(root string, depth int, leaf string) string {
	segments := []string{root}
	for i := range depth {
		segments = append(segments, fmt.Sprintf("level%d", i))
	}
	segments = append(segments, leaf)
	return filepath.Join(segments...)
}

// TestReadRuntimeProfileFile covers stage R of the validation table:
// repository-root resolution at two nesting depths, and one subtest
// per rejected case.
func TestReadRuntimeProfileFile(t *testing.T) {
	t.Parallel()

	for _, depth := range []int{2, 4} {
		t.Run(fmt.Sprintf("resolves the repository root %d directories below go.mod", depth), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeFakeRepo(t, root)
			profilePath := nestedPath(root, depth, "sample-runtime.json")
			mustWriteFile(t, profilePath, string(marshalProfileDoc(t, validProfileDoc())))

			profile, err := ReadRuntimeProfileFile(profilePath)
			if err != nil {
				t.Fatalf("ReadRuntimeProfileFile(%q) = %v, want nil", profilePath, err)
			}
			if profile.RuntimeID != "sample-runtime" {
				t.Errorf("ReadRuntimeProfileFile(%q).RuntimeID = %q, want %q", profilePath, profile.RuntimeID, "sample-runtime")
			}
		})
	}

	t.Run("a runtime_id not matching the file's own base name is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		profilePath := nestedPath(root, 1, "other-name.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, validProfileDoc())))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: runtime_id %q does not match the file base name", profilePath, "sample-runtime")
		}
	})

	t.Run("a notes_path naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		doc := validProfileDoc()
		doc["notes_path"] = "docs/does-not-exist.md"
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: notes_path names a file that does not exist", profilePath)
		}
	})

	t.Run("a measurement_path naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		doc := validProfileDoc()
		doc["measurement_path"] = "internal/qualification/probe/testdata/sample/does-not-exist.json"
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: measurement_path names a file that does not exist", profilePath)
		}
	})

	t.Run("a published_sample naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.does-not-exist.md"
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: published_sample names a file that does not exist", profilePath)
		}
	})

	t.Run("a published_sample decoding to a kind other than agent-client-protocol is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.wrong-kind.md"), "---\nagent:\n  kind: cli\n  command: sample-runtime --acp\n---\nbody\n")
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.wrong-kind.md"
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: published_sample's agent.kind is not agent-client-protocol", profilePath)
		}
	})

	t.Run("a published_sample command carrying no element past element zero is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeRepo(t, root)
		mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.bare-command.md"), "---\nagent:\n  kind: agent-client-protocol\n  command: sample-runtime\n---\nbody\n")
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.bare-command.md"
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: agent.command carries no element past element zero", profilePath)
		}
	})

	t.Run("no ancestor of the profile file carrying go.mod is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		profilePath := nestedPath(root, 1, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, validProfileDoc())))

		if _, err := ReadRuntimeProfileFile(profilePath); err == nil {
			t.Errorf("ReadRuntimeProfileFile(%q) = nil error, want rejection: no ancestor directory carries go.mod", profilePath)
		}
	})
}

// firstValueRecognizer and discriminatedRecognizer are read-only
// fixtures covering TerminalLocator's two modes. Neither Terminal call
// mutates its receiver, so sharing them across parallel subtests is
// safe.
var (
	firstValueRecognizer = Recognizer{
		Locator:       TerminalLocator{Mode: "first_value"},
		ErrorMembers:  []string{"error"},
		SuccessMember: "response",
	}

	discriminatedRecognizer = Recognizer{
		Locator:      TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
		ErrorMembers: []string{"error"},
		StatusMember: "status",
		StatusCases: map[string]Case{
			"refusal":   CaseRuntimeRefusal,
			"cancelled": CaseCancellation,
		},
		StatusEndTurn: []string{"end_turn", "success", "completed"},
	}

	// A runtime that discriminates on the outer value and carries the
	// terminal members one level below it, which is the shape a flat
	// locator cannot read at all.
	envelopedRecognizer = Recognizer{
		Locator: TerminalLocator{
			Mode:               "discriminated",
			DiscriminatorKey:   "type",
			DiscriminatorValue: "runFinished",
			EnvelopePath:       []string{"data"},
		},
		StatusMember:  "status",
		StatusEndTurn: []string{"success"},
	}
)

// TestRecognizerTerminal covers Recognizer.Terminal against both
// TerminalLocator modes, reimplementing the two structured-surface
// terminal-recognition arms this package's driver now expresses
// generically as profile data.
func TestRecognizerTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recognizer Recognizer
		output     string
		want       Terminal
		wantOK     bool
	}{
		{
			name:       "first_value: a response-bearing document recognizes end_turn",
			recognizer: firstValueRecognizer,
			output:     `{"session_id":"sess-fixture","response":{"text":"hi"},"stats":{"input":1}}`,
			want:       Terminal{EndTurn: true},
			wantOK:     true,
		},
		{
			name:       "first_value: empty output recognizes nothing",
			recognizer: firstValueRecognizer,
			output:     "",
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "first_value: an error-bearing document recognizes Error",
			recognizer: firstValueRecognizer,
			output:     `{"error":{"message":"boom"}}`,
			want:       Terminal{Error: true},
			wantOK:     true,
		},
		{
			name:       "first_value: a document carrying neither the error nor the success member recognizes nothing",
			recognizer: firstValueRecognizer,
			output:     `{"session_id":"sess-fixture"}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "discriminated: a type result line with status success recognizes end_turn",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"success"}`,
			want:       Terminal{EndTurn: true},
			wantOK:     true,
		},
		{
			name:       "discriminated: a type result line with status refusal recognizes the mapped case",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"refusal"}`,
			want:       Terminal{Case: CaseRuntimeRefusal},
			wantOK:     true,
		},
		{
			name:       "discriminated: a type result line with status cancelled recognizes the mapped case",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"cancelled"}`,
			want:       Terminal{Case: CaseCancellation},
			wantOK:     true,
		},
		{
			name:       "discriminated: error members are checked before the status member",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"success","error":{"message":"boom"}}`,
			want:       Terminal{Error: true},
			wantOK:     true,
		},
		{
			name:       "discriminated: an unrecognized status recognizes nothing",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"mystery"}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "discriminated: a status value that is not a string recognizes nothing",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":123}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "discriminated: a non-terminal line carrying a status-named member is never the terminal event",
			recognizer: discriminatedRecognizer,
			output:     "{\"type\":\"init\",\"status\":\"success\"}\n{\"type\":\"message\",\"status\":\"success\"}",
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "envelope_path: the status member is read below the discriminated value",
			recognizer: envelopedRecognizer,
			output:     `{"type":"runFinished","data":{"status":"success","finalText":"hi"}}`,
			want:       Terminal{EndTurn: true},
			wantOK:     true,
		},
		{
			name:       "envelope_path: a status member left at the outer value is not read",
			recognizer: envelopedRecognizer,
			output:     `{"type":"runFinished","status":"success"}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "envelope_path: an envelope member that is not an object recognizes nothing",
			recognizer: envelopedRecognizer,
			output:     `{"type":"runFinished","data":"success"}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "envelope_path: a status the profile does not map recognizes nothing",
			recognizer: envelopedRecognizer,
			output:     `{"type":"runFinished","data":{"status":"error"}}`,
			want:       Terminal{},
			wantOK:     false,
		},
		{
			name:       "envelope_path: a non-terminal line of the same stream is skipped",
			recognizer: envelopedRecognizer,
			output:     "{\"type\":\"runStarted\",\"data\":{\"status\":\"success\"}}\n{\"type\":\"runFinished\",\"data\":{\"status\":\"success\"}}",
			want:       Terminal{EndTurn: true},
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.recognizer.Terminal(tt.output)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Recognizer.Terminal(%q) = %+v, %v, want %+v, %v", tt.output, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestRecognizerModelRequestsEmptyPath confirms a recognizer naming no
// model-request path reads unreadable, rather than counting the
// terminal object's own members as model requests.
func TestRecognizerModelRequestsEmptyPath(t *testing.T) {
	t.Parallel()

	recognizer := Recognizer{
		Locator:       TerminalLocator{Mode: "first_value"},
		SuccessMember: "response",
	}
	output := `{"response":"hi","stats":{"models":{"a-model":1}}}`
	if got := recognizer.ModelRequests(output); got != ModelRequestUnreadable {
		t.Errorf("Recognizer.ModelRequests(%q) = %v, want %v", output, got, ModelRequestUnreadable)
	}
}

// TestRuntimeProfileAskingArgs confirms the asking posture is read from
// the profile rather than derived from the graded launch, and that a
// profile stating none reports so instead of returning a launch the
// caller would have to guess at.
func TestRuntimeProfileAskingArgs(t *testing.T) {
	t.Parallel()

	t.Run("a stated asking posture substitutes placeholders", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["asking_args"] = []string{"--acp", "--model", "{model}"}
		profile, err := DecodeRuntimeProfile(marshalProfileDoc(t, doc))
		if err != nil {
			t.Fatalf("DecodeRuntimeProfile() error = %v, want nil", err)
		}
		got, ok := profile.AskingArgs(SurfaceProtocol, "a-model", "", "")
		if !ok {
			t.Fatal("AskingArgs() ok = false, want a stated posture to resolve")
		}
		want := []string{"--acp", "--model", "a-model"}
		if !slices.Equal(got, want) {
			t.Errorf("AskingArgs() = %v, want %v", got, want)
		}
	})

	t.Run("a profile stating no asking posture reports none", func(t *testing.T) {
		t.Parallel()

		profile, err := DecodeRuntimeProfile(marshalProfileDoc(t, validProfileDoc()))
		if err != nil {
			t.Fatalf("DecodeRuntimeProfile() error = %v, want nil", err)
		}
		if got, ok := profile.AskingArgs(SurfaceProtocol, "a-model", "", ""); ok {
			t.Errorf("AskingArgs() = %v, true, want no posture reported", got)
		}
	})

	t.Run("omitting asking_args leaves the digest unmoved", func(t *testing.T) {
		t.Parallel()

		profile, err := DecodeRuntimeProfile(marshalProfileDoc(t, validProfileDoc()))
		if err != nil {
			t.Fatalf("DecodeRuntimeProfile() error = %v, want nil", err)
		}
		bare := profile
		bare.EntryPoints = maps.Clone(profile.EntryPoints)
		entry := bare.EntryPoints[SurfaceProtocol]
		entry.AskingArgs = nil
		bare.EntryPoints[SurfaceProtocol] = entry
		if profile.Digest() != bare.Digest() {
			t.Errorf("Digest() moved for a profile that states no asking posture: %s vs %s", profile.Digest(), bare.Digest())
		}
	})
}
