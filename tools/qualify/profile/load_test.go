package profile

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

func validProfileDoc() map[string]any {
	return map[string]any{
		"schema_version":        4,
		"runtime_id":            "sample-runtime",
		"identity_tokens":       []string{"sample"},
		"notes_path":            "docs/sample-notes.md",
		"measurement_path":      "tools/qualify/probe/testdata/sample/measurement.json",
		"published_sample":      "examples/WORKFLOW.sample.md",
		"tool_name_format":      "mcp_{server}_{tool}",
		"project_config_paths":  []string{".sample/config.json"},
		"version_args":          []string{"--version"},
		"model_args":            []string{"--model", "{model}"},
		"capability_gap_labels": []string{capabilityGapLabelTokenCounts},
		"probe_prompts":         validProbePromptsDoc(),
		"entry_points": map[string]any{
			"protocol":           map[string]any{"args": []string{"--acp"}, "asking_args": []string{"--acp", "--ask"}},
			"native_json":        map[string]any{"args": []string{"--output-format", "json", "--prompt", "{prompt}"}, "asking_args": []string{"--output-format", "json", "--ask", "--prompt", "{prompt}"}},
			"native_stream_json": map[string]any{"args": []string{"--output-format", "stream-json", "--prompt", "{prompt}"}, "asking_args": []string{"--output-format", "stream-json", "--ask", "--prompt", "{prompt}"}},
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
		"not_inducible_cases": []any{
			map[string]any{"surface": "native_json", "case": "limit_reached", "reason": evidence.NotInducibleChannelTooSmall},
			map[string]any{"surface": "native_stream_json", "case": "limit_reached", "reason": evidence.NotInducibleChannelTooSmall},
		},
	}
}

func validProbePromptsDoc() map[string]any {
	return map[string]any{
		"success":             "Reply with exactly SORTIE_BASELINE_OK and do not call any tool.",
		"runtime_refusal":     "Decline to continue this turn and report your refusal outcome without calling a tool.",
		"tool_call":           "Call the tool named {tool} now, with no arguments, then reply with exactly SORTIE_PROBE_DONE.",
		"continuation_seed":   "Remember the nonce {nonce} for the rest of this conversation and reply exactly STORED.",
		"continuation_recall": "Reply with the nonce supplied by the prior conversation and no other text.",
	}
}

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

func marshalProfileDoc(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", doc, err)
	}
	return data
}

func TestDecodeRuntimeProfile(t *testing.T) {
	t.Parallel()

	t.Run("a fully valid document decodes with no error", func(t *testing.T) {
		t.Parallel()

		data := marshalProfileDoc(t, validProfileDoc())
		if _, err := Decode(data); err != nil {
			t.Fatalf("Decode(%s) = %v, want nil", data, err)
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
			name:   "schema_version other than 4 is rejected",
			mutate: func(doc map[string]any) { doc["schema_version"] = 3 },
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
			// The recognizer goes with it, so absence from entry_points is the
			// only rule left to reject the surface.
			name: "entry_points missing a measurable native surface is rejected",
			mutate: func(doc map[string]any) {
				delete(doc["entry_points"].(map[string]any), "native_stream_json")
				delete(doc["recognizers"].(map[string]any), "native_stream_json")
			},
		},
		{
			name: "entry_points naming a surface outside evidence.Surfaces is rejected",
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
			name: "status_cases naming a case outside evidence.Cases is rejected",
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
					map[string]any{"capability": "not_a_real_capability", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "a declaration naming a case outside its capability's own set is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "human_input", "reason": evidence.DeclaredGapNeverProduced},
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
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced, "extra": "surprise"},
				}
			},
		},
		{
			name: "a duplicate declaration capability-and-case pair is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
					map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": evidence.DeclaredGapNeverProduced},
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "declaring a case without its required peer is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
				}
			},
		},
		{
			name: "declaring a case and its peer with differing reasons is rejected",
			mutate: func(doc map[string]any) {
				doc["declarations"] = []any{
					map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
					map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": evidence.DeclaredGapFolded},
				}
			},
		},
		{
			name: "an absent_surfaces entry naming a surface outside DeclarableAbsentSurfaces is rejected",
			mutate: func(doc map[string]any) {
				doc["absent_surfaces"] = []any{
					map[string]any{"surface": "protocol", "reason": evidence.SurfaceNotOffered},
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
					map[string]any{"surface": "native_json", "reason": evidence.SurfaceNotOffered},
					map[string]any{"surface": "native_json", "reason": evidence.SurfaceNotOffered},
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

			if _, err := Decode(data); err == nil {
				t.Errorf("Decode(%s) = nil error, want a rejection", data)
			}
		})
	}
}

func TestRuntimeProfileDigest(t *testing.T) {
	t.Parallel()

	doc := validProfileDoc()
	compact := marshalProfileDoc(t, doc)
	indented, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(%v): %v", doc, err)
	}

	compactProfile, err := Decode(compact)
	if err != nil {
		t.Fatalf("Decode(%s): %v", compact, err)
	}
	indentedProfile, err := Decode(indented)
	if err != nil {
		t.Fatalf("Decode(%s): %v", indented, err)
	}

	if compactProfile.Digest() != indentedProfile.Digest() {
		t.Errorf("Digest() = %q for the compact encoding, %q for the reindented one, want equal", compactProfile.Digest(), indentedProfile.Digest())
	}

	edited := cloneProfileDoc(t, doc)
	edited["version_args"] = []string{"--version", "--verbose"}
	editedProfile, err := Decode(marshalProfileDoc(t, edited))
	if err != nil {
		t.Fatalf("Decode of the edited document: %v", err)
	}

	if editedProfile.Digest() == compactProfile.Digest() {
		t.Errorf("Digest() = %q for both the original and a one-field edit, want it to move", editedProfile.Digest())
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}

// validPublishedSample satisfies Load's published_sample rule: agent.kind is
// agent-client-protocol and agent.command carries an element past element
// zero.
const validPublishedSample = "---\nagent:\n  kind: agent-client-protocol\n  command: sample-runtime --acp\n---\nbody\n"

func writeFakeCheckout(t *testing.T, root string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(root, "docs", "sample-notes.md"), "# sample notes\n")
	mustWriteFile(t, filepath.Join(root, "tools", "qualify", "probe", "testdata", "sample", "measurement.json"), "{}")
	mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.sample.md"), validPublishedSample)
}

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("a valid document resolves against the supplied root", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, validProfileDoc())))

		profile, err := Load(root, profilePath)
		if err != nil {
			t.Fatalf("Load(%q, %q) = %v, want nil", root, profilePath, err)
		}
		if profile.RuntimeID != "sample-runtime" {
			t.Errorf("Load(...).RuntimeID = %q, want %q", profile.RuntimeID, "sample-runtime")
		}
	})

	t.Run("a runtime_id not matching the file's own base name is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		profilePath := filepath.Join(root, "other-name.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, validProfileDoc())))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: runtime_id %q does not match the file base name", root, profilePath, "sample-runtime")
		}
	})

	t.Run("a notes_path naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		doc := validProfileDoc()
		doc["notes_path"] = "docs/does-not-exist.md"
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: notes_path names a file that does not exist", root, profilePath)
		}
	})

	t.Run("a measurement_path naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		doc := validProfileDoc()
		doc["measurement_path"] = "tools/qualify/probe/testdata/sample/does-not-exist.json"
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: measurement_path names a file that does not exist", root, profilePath)
		}
	})

	t.Run("a published_sample naming a file that does not exist is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.does-not-exist.md"
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: published_sample names a file that does not exist", root, profilePath)
		}
	})

	t.Run("a published_sample decoding to a kind other than agent-client-protocol is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.wrong-kind.md"), "---\nagent:\n  kind: cli\n  command: sample-runtime --acp\n---\nbody\n")
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.wrong-kind.md"
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: published_sample's agent.kind is not agent-client-protocol", root, profilePath)
		}
	})

	t.Run("a published_sample command carrying no element past element zero is rejected", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFakeCheckout(t, root)
		mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.bare-command.md"), "---\nagent:\n  kind: agent-client-protocol\n  command: sample-runtime\n---\nbody\n")
		doc := validProfileDoc()
		doc["published_sample"] = "examples/WORKFLOW.bare-command.md"
		profilePath := filepath.Join(root, "sample-runtime.json")
		mustWriteFile(t, profilePath, string(marshalProfileDoc(t, doc)))

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection: agent.command carries no element past element zero", root, profilePath)
		}
	})

	t.Run("a path that does not exist is reported", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		profilePath := filepath.Join(root, "absent.json")

		if _, err := Load(root, profilePath); err == nil {
			t.Errorf("Load(%q, %q) = nil error, want rejection", root, profilePath)
		}
	})
}

// firstValueRecognizer and discriminatedRecognizer are read-only fixtures;
// Terminal never mutates its receiver, so parallel subtests may share them.
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
		StatusCases: map[string]evidence.Case{
			"refusal":   evidence.CaseRuntimeRefusal,
			"cancelled": evidence.CaseCancellation,
		},
		StatusEndTurn: []string{"end_turn", "success", "completed"},
	}

	// A runtime discriminating on the outer value and carrying the terminal
	// members one level below it, a shape a flat locator cannot read.
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
			want:       Terminal{Case: evidence.CaseRuntimeRefusal},
			wantOK:     true,
		},
		{
			name:       "discriminated: a type result line with status cancelled recognizes the mapped case",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"cancelled"}`,
			want:       Terminal{Case: evidence.CaseCancellation},
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

func TestRecognizerRawTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recognizer Recognizer
		output     string
		want       map[string]any
		wantOK     bool
	}{
		{
			name:       "discriminated: a status the recognizer cannot map still returns the located envelope",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"result","status":"mystery"}`,
			want:       map[string]any{"type": "result", "status": "mystery"},
			wantOK:     true,
		},
		{
			name:       "envelope_path: the object below the discriminated value is returned even when its status does not map",
			recognizer: envelopedRecognizer,
			output:     `{"type":"runFinished","data":{"status":"mystery"}}`,
			want:       map[string]any{"status": "mystery"},
			wantOK:     true,
		},
		{
			name:       "empty output locates nothing",
			recognizer: discriminatedRecognizer,
			output:     "",
			want:       nil,
			wantOK:     false,
		},
		{
			name:       "no line matches the discriminator",
			recognizer: discriminatedRecognizer,
			output:     `{"type":"init"}`,
			want:       nil,
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.recognizer.RawTerminal(tt.output)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Recognizer.RawTerminal(%q) = %+v, %v, want %+v, %v", tt.output, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestRecognizerModelRequestsEmptyPath confirms a recognizer naming no
// model-request path reads unreadable, rather than counting the terminal
// object's own members as model requests.
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

func TestRuntimeProfileAskingArgs(t *testing.T) {
	t.Parallel()

	t.Run("a stated asking posture substitutes placeholders", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["asking_args"] = []string{"--acp", "--model", "{model}"}
		profile, err := Decode(marshalProfileDoc(t, doc))
		if err != nil {
			t.Fatalf("Decode() error = %v, want nil", err)
		}
		got, ok := profile.AskingArgs(evidence.SurfaceProtocol, "a-model", "", "")
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

		// asking_args is required on every measured surface, so only a
		// declared-absent surface can state none.
		doc := validProfileDoc()
		entryPoints := doc["entry_points"].(map[string]any)
		delete(entryPoints["native_stream_json"].(map[string]any), "asking_args")
		doc["absent_surfaces"] = []any{
			map[string]any{"surface": "native_stream_json", "reason": evidence.SurfaceNotOffered},
		}
		profile, err := Decode(marshalProfileDoc(t, doc))
		if err != nil {
			t.Fatalf("Decode() error = %v, want nil", err)
		}
		if got, ok := profile.AskingArgs(evidence.SurfaceNativeStreamJSON, "a-model", "", ""); ok {
			t.Errorf("AskingArgs() = %v, true, want no posture reported", got)
		}
	})

	t.Run("omitting asking_args leaves the digest unmoved", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		entryPoints := doc["entry_points"].(map[string]any)
		delete(entryPoints["native_stream_json"].(map[string]any), "asking_args")
		doc["absent_surfaces"] = []any{
			map[string]any{"surface": "native_stream_json", "reason": evidence.SurfaceNotOffered},
		}
		profile, err := Decode(marshalProfileDoc(t, doc))
		if err != nil {
			t.Fatalf("Decode() error = %v, want nil", err)
		}
		bare := profile
		bare.EntryPoints = maps.Clone(profile.EntryPoints)
		entry := bare.EntryPoints[evidence.SurfaceNativeStreamJSON]
		entry.AskingArgs = nil
		bare.EntryPoints[evidence.SurfaceNativeStreamJSON] = entry
		if profile.Digest() != bare.Digest() {
			t.Errorf("Digest() moved for a profile that states no asking posture: %s vs %s", profile.Digest(), bare.Digest())
		}
	})
}

func TestRuntimeProfileEntryArgsPolicyPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		policy string
		want   []string
	}{
		{
			name:   "flag and placeholder both drop when policy is empty",
			args:   []string{"--flag", "--sandbox-policy", "{policy}", "--tail"},
			policy: "",
			want:   []string{"--flag", "--tail"},
		},
		{
			name:   "flag and placeholder stay adjacent and in order when policy is set",
			args:   []string{"--flag", "--sandbox-policy", "{policy}", "--tail"},
			policy: "/tmp/policy.toml",
			want:   []string{"--flag", "--sandbox-policy", "/tmp/policy.toml", "--tail"},
		},
		{
			name:   "placeholder as the first token with no preceding argument drops alone when empty",
			args:   []string{"{policy}", "--tail"},
			policy: "",
			want:   []string{"--tail"},
		},
		{
			name:   "placeholder as the first token substitutes in place when set",
			args:   []string{"{policy}", "--tail"},
			policy: "/tmp/policy.toml",
			want:   []string{"/tmp/policy.toml", "--tail"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			profile := RuntimeProfile{
				EntryPoints: map[evidence.Surface]EntryPoint{
					evidence.SurfaceProtocol: {Args: tt.args},
				},
			}

			got, err := profile.EntryArgs(evidence.SurfaceProtocol, "", tt.policy, "")

			if err != nil {
				t.Fatalf("EntryArgs(%q, policy=%q) unexpected error: %v", tt.args, tt.policy, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("EntryArgs(%q, policy=%q) = %v, want %v", tt.args, tt.policy, got, tt.want)
			}
		})
	}
}

// trackedProfile loads the tracked gemini-cli profile against the real
// checkout, the fixture every accessor test below exercises.
func trackedProfile(t *testing.T) (RuntimeProfile, string) {
	t.Helper()

	root, err := CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() = _, %v, want nil", err)
	}
	path := filepath.Join(root, "tools", "qualify", "profiles", "gemini-cli.json")
	profile, err := Load(root, path)
	if err != nil {
		t.Fatalf("Load(%q, %q) = _, %v, want nil", root, path, err)
	}
	return profile, root
}

func TestNotInducibleDeclared(t *testing.T) {
	t.Parallel()

	profile := RuntimeProfile{
		NotInducibleCases: []SurfaceNotInducible{
			{Surface: evidence.SurfaceNativeJSON, Case: evidence.CaseCancellation, Reason: evidence.NotInducibleTerminalAtExitOnly},
		},
	}

	t.Run("a matching surface and case returns the declared reason", func(t *testing.T) {
		t.Parallel()

		reason, ok := profile.NotInducibleDeclared(evidence.SurfaceNativeJSON, evidence.CaseCancellation)
		if !ok || reason != evidence.NotInducibleTerminalAtExitOnly {
			t.Errorf("NotInducibleDeclared(%s, %s) = %q, %v, want %q, true", evidence.SurfaceNativeJSON, evidence.CaseCancellation, reason, ok, evidence.NotInducibleTerminalAtExitOnly)
		}
	})

	t.Run("a mismatched surface returns nothing", func(t *testing.T) {
		t.Parallel()

		reason, ok := profile.NotInducibleDeclared(evidence.SurfaceNativeStreamJSON, evidence.CaseCancellation)
		if ok || reason != "" {
			t.Errorf("NotInducibleDeclared(%s, %s) = %q, %v, want \"\", false", evidence.SurfaceNativeStreamJSON, evidence.CaseCancellation, reason, ok)
		}
	})

	t.Run("a mismatched case returns nothing", func(t *testing.T) {
		t.Parallel()

		reason, ok := profile.NotInducibleDeclared(evidence.SurfaceNativeJSON, evidence.CaseSuccess)
		if ok || reason != "" {
			t.Errorf("NotInducibleDeclared(%s, %s) = %q, %v, want \"\", false", evidence.SurfaceNativeJSON, evidence.CaseSuccess, reason, ok)
		}
	})
}

func TestMeasuredSurfaces(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)
	got := profile.MeasuredSurfaces()

	want := []evidence.Surface{evidence.SurfaceProtocol, evidence.SurfaceNativeJSON, evidence.SurfaceNativeStreamJSON}
	if !slices.Equal(got, want) {
		t.Errorf("MeasuredSurfaces() = %v, want %v in the closed vocabulary's own order", got, want)
	}
	if slices.Contains(got, evidence.SurfaceAggregate) {
		t.Errorf("MeasuredSurfaces() = %v, want no %s: it names a cross-surface observation rather than a launch", got, evidence.SurfaceAggregate)
	}
}

func TestDeclaredMeasuredSurfaces(t *testing.T) {
	t.Parallel()

	t.Run("the zero RuntimeProfile declares all three", func(t *testing.T) {
		t.Parallel()

		var zero RuntimeProfile
		want := evidence.MeasurableSurfaces()
		if got := zero.DeclaredMeasuredSurfaces(); !slices.Equal(got, want) {
			t.Errorf("DeclaredMeasuredSurfaces() = %v, want %v", got, want)
		}
	})

	t.Run("a declared-absent surface is excluded", func(t *testing.T) {
		t.Parallel()

		profile := RuntimeProfile{
			AbsentSurfaces: []AbsentSurface{{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered}},
		}
		got := profile.DeclaredMeasuredSurfaces()
		if slices.Contains(got, evidence.SurfaceNativeStreamJSON) {
			t.Errorf("DeclaredMeasuredSurfaces() = %v, want no %s: declared absent", got, evidence.SurfaceNativeStreamJSON)
		}
	})
}

func TestEntryArgs(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)

	t.Run("placeholders are substituted positionally", func(t *testing.T) {
		t.Parallel()

		argv, err := profile.EntryArgs(evidence.SurfaceNativeJSON, "a-model", "a-policy", "a prompt")
		if err != nil {
			t.Fatalf("EntryArgs(%s, ...) = _, %v, want nil", evidence.SurfaceNativeJSON, err)
		}
		for _, unresolved := range []string{"{model}", "{policy}", "{prompt}"} {
			if slices.Contains(argv, unresolved) {
				t.Errorf("EntryArgs(%s, ...) = %v, want no unresolved %s", evidence.SurfaceNativeJSON, argv, unresolved)
			}
		}
		if !slices.Contains(argv, "a prompt") {
			t.Errorf("EntryArgs(%s, ...) = %v, want the substituted prompt", evidence.SurfaceNativeJSON, argv)
		}
	})

	t.Run("a surface carrying no entry point is an error", func(t *testing.T) {
		t.Parallel()

		if _, err := profile.EntryArgs(evidence.SurfaceAggregate, "a-model", "", ""); err == nil {
			t.Errorf("EntryArgs(%s, ...) = _, nil, want an error: the surface carries no entry point", evidence.SurfaceAggregate)
		}
	})
}

func TestPublishedPostureArgs(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)

	tests := []struct {
		name    string
		sample  []string
		wantErr bool
	}{
		{name: "a command with a posture past element zero resolves", sample: []string{"gemini", "--acp"}},
		{name: "an empty command is rejected", sample: nil, wantErr: true},
		{name: "a command with no posture past element zero is rejected", sample: []string{"gemini"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			argv, err := profile.PublishedPostureArgs(tt.sample, "/resolved/path", "a-model")
			if tt.wantErr {
				if err == nil {
					t.Errorf("PublishedPostureArgs(%v, ...) = %v, nil, want an error", tt.sample, argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("PublishedPostureArgs(%v, ...) = _, %v, want nil", tt.sample, err)
			}
			if argv[0] != "/resolved/path" {
				t.Errorf("PublishedPostureArgs(%v, ...)[0] = %q, want the resolved command path", tt.sample, argv[0])
			}
			if !slices.Contains(argv, "a-model") {
				t.Errorf("PublishedPostureArgs(%v, ...) = %v, want the substituted model", tt.sample, argv)
			}
		})
	}
}

func TestReadMeasurementFile(t *testing.T) {
	t.Parallel()

	profile, root := trackedProfile(t)
	path := filepath.Join(root, profile.MeasurementPath)

	measurement, err := evidence.ReadMeasurementFile(path)
	if err != nil {
		t.Fatalf("ReadMeasurementFile(%q) = _, %v, want nil", path, err)
	}
	if measurement.ProfileDigest != profile.Digest() {
		t.Errorf("ReadMeasurementFile(%q).ProfileDigest = %q, want %q: the artifact is bound to the profile it measured", path, measurement.ProfileDigest, profile.Digest())
	}
}

func TestReadPublishedSampleCommand(t *testing.T) {
	t.Parallel()

	profile, root := trackedProfile(t)
	path := filepath.Join(root, profile.PublishedSample)

	command, err := ReadPublishedSampleCommand(path)
	if err != nil {
		t.Fatalf("ReadPublishedSampleCommand(%q) = _, %v, want nil", path, err)
	}
	if len(command) < 2 {
		t.Errorf("ReadPublishedSampleCommand(%q) = %v, want a launch posture past element zero", path, command)
	}
}

func TestRecognizerModelRequests(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)
	recognizer, ok := profile.Recognizers[evidence.SurfaceNativeJSON]
	if !ok {
		t.Fatalf("profile.Recognizers carries no %s", evidence.SurfaceNativeJSON)
	}
	if len(recognizer.ModelRequestPath) == 0 {
		t.Skipf("the tracked profile's %s recognizer reads no model-request path", evidence.SurfaceNativeJSON)
	}

	nested := func(value any) string {
		path := recognizer.ModelRequestPath
		cursor := value
		for i := len(path) - 1; i >= 1; i-- {
			cursor = map[string]any{path[i]: cursor}
		}
		terminal := map[string]any{recognizer.SuccessMember: "text", path[0]: cursor}
		encoded, err := json.Marshal(terminal)
		if err != nil {
			t.Fatalf("marshal the synthetic terminal: %v", err)
		}
		return string(encoded)
	}

	tests := []struct {
		name   string
		output string
		want   ModelRequestReading
	}{
		{name: "no terminal at all is unreadable", output: "not json", want: ModelRequestUnreadable},
		{name: "a decoded object holding no member reports none", output: nested(map[string]any{}), want: ModelRequestNone},
		{name: "a decoded object holding a member reports at least one", output: nested(map[string]any{"a-model": 1}), want: ModelRequestAtLeastOne},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := recognizer.ModelRequests(tt.output); got != tt.want {
				t.Errorf("ModelRequests(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestDecodeRuntimeProfileRejectsUnlaunchableDeclarations(t *testing.T) {
	t.Parallel()

	t.Run("an aggregate entry point is rejected", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		doc["entry_points"].(map[string]any)[string(evidence.SurfaceAggregate)] = map[string]any{"args": []string{"--x"}}
		if _, err := Decode(marshalProfileDoc(t, doc)); err == nil {
			t.Errorf("Decode(...) = _, nil, want a rejection: %s carries no entry point", evidence.SurfaceAggregate)
		}
	})

	t.Run("a surface declared absent without its own entry point is rejected", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		delete(doc["entry_points"].(map[string]any), string(evidence.SurfaceNativeStreamJSON))
		delete(doc["recognizers"].(map[string]any), string(evidence.SurfaceNativeStreamJSON))
		doc["absent_surfaces"] = []any{
			map[string]any{"surface": string(evidence.SurfaceNativeStreamJSON), "reason": evidence.SurfaceNotOffered},
		}
		if _, err := Decode(marshalProfileDoc(t, doc)); err == nil {
			t.Error("Decode(...) = _, nil, want a rejection: an absence with no launch cannot be corroborated")
		}
	})
}

func validProfileDocWithContinuation() map[string]any {
	doc := validProfileDoc()
	entry := doc["entry_points"].(map[string]any)["native_json"].(map[string]any)
	entry["seed_args"] = []string{"--resume", "{session_id}"}
	entry["resume_args"] = []string{"--resume-last"}
	recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
	recognizer["session_id_path"] = []string{"session", "id"}
	return doc
}

func validProfileDocWithResumeOnly() map[string]any {
	doc := validProfileDocWithContinuation()
	delete(doc["entry_points"].(map[string]any)["native_json"].(map[string]any), "seed_args")
	return doc
}

func validProfileDocWithTokenPaths() map[string]any {
	doc := validProfileDoc()
	recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
	recognizer["token_paths"] = []any{
		map[string]any{"path": []string{"usage", "total_tokens"}, "kind": tokenPathKindSpend},
	}
	return doc
}

func validProfileDocWithDeclaredPeerPair() map[string]any {
	doc := validProfileDoc()
	doc["declarations"] = []any{
		map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": evidence.DeclaredGapNeverProduced},
		map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": evidence.DeclaredGapNeverProduced},
	}
	return doc
}

func TestDecodeRuntimeProfileV4(t *testing.T) {
	t.Parallel()

	t.Run("the continuation and token_paths fixtures decode cleanly", func(t *testing.T) {
		t.Parallel()

		for _, doc := range []map[string]any{validProfileDocWithContinuation(), validProfileDocWithResumeOnly(), validProfileDocWithTokenPaths(), validProfileDocWithDeclaredPeerPair()} {
			data := marshalProfileDoc(t, doc)
			if _, err := Decode(data); err != nil {
				t.Errorf("Decode(%s) = _, %v, want nil", data, err)
			}
		}
	})

	t.Run("the tracked kiro profile decodes with resume_args and no seed_args", func(t *testing.T) {
		t.Parallel()

		root, err := CheckoutRoot()
		if err != nil {
			t.Fatalf("CheckoutRoot() = _, %v, want nil", err)
		}
		path := filepath.Join(root, "tools", "qualify", "profiles", "kiro-cli.json")
		profile, err := Load(root, path)
		if err != nil {
			t.Fatalf("Load(%q, %q) = _, %v, want nil", root, path, err)
		}
		entry, ok := profile.EntryPoints[evidence.SurfaceNativeStreamJSON]
		if !ok {
			t.Fatalf("EntryPoints[%s] missing, want an entry point", evidence.SurfaceNativeStreamJSON)
		}
		if len(entry.ResumeArgs) == 0 {
			t.Errorf("EntryPoints[%s].ResumeArgs = %v, want non-empty", evidence.SurfaceNativeStreamJSON, entry.ResumeArgs)
		}
		if len(entry.SeedArgs) != 0 {
			t.Errorf("EntryPoints[%s].SeedArgs = %v, want empty", evidence.SurfaceNativeStreamJSON, entry.SeedArgs)
		}
	})

	tests := []struct {
		name string
		base func() map[string]any

		mutate func(doc map[string]any)
		// wantSub, when set, is a substring of the rejection the case means
		// to provoke, so a case cannot pass on an unrelated earlier check.
		wantSub string
	}{
		{
			name: "an unknown probe_prompts key is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["probe_prompts"].(map[string]any)["bogus"] = "an extra prompt"
			},
		},
		{
			name: "a missing probe_prompts key is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				delete(doc["probe_prompts"].(map[string]any), "tool_call")
			},
		},
		{
			name: "a probe_prompts value missing its own required placeholder is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["probe_prompts"].(map[string]any)["tool_call"] = "Call the named tool now, with no arguments."
			},
		},
		{
			name: "a probe_prompts value duplicating its own required placeholder is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["probe_prompts"].(map[string]any)["tool_call"] = "Call {tool} then call {tool} again."
			},
		},
		{
			name: "continuation_seed missing {nonce} is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["probe_prompts"].(map[string]any)["continuation_seed"] = "Remember this and reply STORED."
			},
		},
		{
			name: "continuation_recall carrying {nonce} is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["probe_prompts"].(map[string]any)["continuation_recall"] = "Reply with the nonce {nonce} you were given."
			},
		},
		{
			name: "seed_args missing {session_id} is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["native_json"].(map[string]any)["seed_args"] = []string{"--resume-new"}
			},
		},
		{
			name: "seed_args carrying {session_id} twice is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["native_json"].(map[string]any)["seed_args"] = []string{"--resume", "{session_id}", "--again", "{session_id}"}
			},
		},
		{
			name: "resume_args carrying a placeholder is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["native_json"].(map[string]any)["resume_args"] = []string{"--model", "{model}"}
			},
		},
		{
			name: "seed_args present without resume_args is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				delete(doc["entry_points"].(map[string]any)["native_json"].(map[string]any), "resume_args")
			},
		},
		{
			name: "resume_args present without seed_args or a session_id_path is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				delete(doc["entry_points"].(map[string]any)["native_json"].(map[string]any), "seed_args")
				delete(doc["recognizers"].(map[string]any)["native_json"].(map[string]any), "session_id_path")
			},
		},
		{
			name: "seed_args is rejected on the protocol surface",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["seed_args"] = []string{"--resume", "{session_id}"}
			},
		},
		{
			name: "resume_args is rejected on the protocol surface",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["entry_points"].(map[string]any)["protocol"].(map[string]any)["resume_args"] = []string{"--resume-last"}
			},
		},
		{
			name: "a surface with seed_args/resume_args but no session_id_path is rejected",
			base: validProfileDocWithContinuation,
			mutate: func(doc map[string]any) {
				delete(doc["recognizers"].(map[string]any)["native_json"].(map[string]any), "session_id_path")
			},
		},
		{
			name: "a duplicate token_paths entry is rejected",
			base: validProfileDocWithTokenPaths,
			mutate: func(doc map[string]any) {
				recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
				recognizer["token_paths"] = []any{
					map[string]any{"path": []string{"usage", "total_tokens"}, "kind": tokenPathKindSpend},
					map[string]any{"path": []string{"usage", "total_tokens"}, "kind": tokenPathKindOccupancy},
				}
			},
		},
		{
			name: "an invalid token_paths.kind is rejected",
			base: validProfileDocWithTokenPaths,
			mutate: func(doc map[string]any) {
				recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
				recognizer["token_paths"] = []any{
					map[string]any{"path": []string{"usage", "total_tokens"}, "kind": "bogus_kind"},
				}
			},
		},
		{
			name: "a token_paths entry carrying an empty path key is rejected",
			base: validProfileDocWithTokenPaths,
			mutate: func(doc map[string]any) {
				recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
				recognizer["token_paths"] = []any{
					map[string]any{"path": []string{"usage", ""}, "kind": tokenPathKindSpend},
				}
			},
		},
		{
			name: "a not_inducible_cases entry naming an invalid surface is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "not_a_real_surface", "case": "human_input", "reason": evidence.NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry naming the aggregate surface is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "aggregate", "case": "human_input", "reason": evidence.NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry naming a catalog-wide not-inducible case is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "unknown_outcome", "reason": evidence.NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry carrying an unknown reason is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "human_input", "reason": "made_up_reason"})
			},
		},
		{
			name: "a duplicate not_inducible_cases (surface, case) pair is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "limit_reached", "reason": evidence.NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry contradicting a declaration is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["declarations"] = append(doc["declarations"].([]any),
					map[string]any{"capability": "retry_classification", "case": "human_input", "reason": evidence.DeclaredGapNeverProduced})
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "human_input", "reason": evidence.NotInducibleTerminalVocabularyClosed})
			},
			wantSub: "is also named by declarations",
		},
		{
			name: "a not_inducible_cases entry carrying an unknown field is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "human_input", "reason": evidence.NotInducibleTerminalVocabularyClosed, "scope": "everywhere"})
			},
			wantSub: `unknown field "scope"`,
		},
		{
			name: "a not_inducible_cases entry missing its reason is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "human_input"})
			},
			wantSub: `missing field "reason"`,
		},
		{
			name: "a token_paths entry carrying an unknown field is rejected",
			base: validProfileDocWithTokenPaths,
			mutate: func(doc map[string]any) {
				recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
				recognizer["token_paths"] = []any{
					map[string]any{"path": []string{"usage", "total_tokens"}, "kind": tokenPathKindSpend, "unit": "tokens"},
				}
			},
			wantSub: `unknown field "unit"`,
		},
		{
			name: "a token_paths entry missing its path is rejected",
			base: validProfileDocWithTokenPaths,
			mutate: func(doc map[string]any) {
				recognizer := doc["recognizers"].(map[string]any)["native_json"].(map[string]any)
				recognizer["token_paths"] = []any{
					map[string]any{"kind": tokenPathKindSpend},
				}
			},
			wantSub: `missing field "path"`,
		},
		{
			name: "an entry-point argument embedding {policy} is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				entry := doc["entry_points"].(map[string]any)["native_json"].(map[string]any)
				entry["args"] = []string{"--output-format", "json", "--policy={policy}", "--prompt", "{prompt}"}
			},
			wantSub: "must stand alone as its own argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc := cloneProfileDoc(t, tt.base())
			tt.mutate(doc)
			data := marshalProfileDoc(t, doc)

			_, err := Decode(data)
			if err == nil {
				t.Fatalf("Decode(%s) = nil error, want a rejection", data)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Decode(%s) error = %v, want it to name %q", data, err, tt.wantSub)
			}
		})
	}
}

func TestEntryArgsRejectsEmptySubstitution(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)

	tests := []struct {
		name         string
		model        string
		policy       string
		prompt       string
		wantRejected bool
	}{
		{name: "an empty model is rejected", model: "", policy: "a-policy", prompt: "a prompt", wantRejected: true},
		{name: "an empty prompt is rejected", model: "a-model", policy: "a-policy", prompt: "", wantRejected: true},
		{name: "an empty policy is exempt", model: "a-model", policy: "", prompt: "a prompt", wantRejected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := profile.EntryArgs(evidence.SurfaceNativeJSON, tt.model, tt.policy, tt.prompt)
			if tt.wantRejected && err == nil {
				t.Errorf("EntryArgs(%s, %q, %q, %q) = _, nil, want an error", evidence.SurfaceNativeJSON, tt.model, tt.policy, tt.prompt)
			}
			if !tt.wantRejected && err != nil {
				t.Errorf("EntryArgs(%s, %q, %q, %q) = _, %v, want nil", evidence.SurfaceNativeJSON, tt.model, tt.policy, tt.prompt, err)
			}
		})
	}
}

func TestNotInducibleCasesAreTheProfilesOwnStatement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []any
	}{
		{name: "no entry at all", entries: []any{}},
		{
			name: "an entry on one native surface only",
			entries: []any{
				map[string]any{"surface": "native_json", "case": "limit_reached", "reason": evidence.NotInducibleChannelTooSmall},
			},
		},
		{
			name: "an entry naming another case entirely",
			entries: []any{
				map[string]any{"surface": "native_json", "case": "cancellation", "reason": evidence.NotInducibleTerminalAtExitOnly},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := validProfileDoc()
			doc["not_inducible_cases"] = tc.entries
			data := marshalProfileDoc(t, doc)
			if _, err := Decode(data); err != nil {
				t.Errorf("Decode() error = %v, want nil: the profile states what its own runtime cannot reach, and no case is required of every profile", err)
			}
		})
	}
}

func TestNotInducibleReasonStillPinsItsCase(t *testing.T) {
	t.Parallel()

	doc := validProfileDoc()
	doc["not_inducible_cases"] = []any{
		map[string]any{"surface": "native_json", "case": "cancellation", "reason": evidence.NotInducibleChannelTooSmall},
	}
	data := marshalProfileDoc(t, doc)
	_, err := Decode(data)
	if err == nil {
		t.Fatal("Decode() = nil error, want rejection of a reason paired with a case it does not describe")
	}
	if !strings.Contains(err.Error(), "pairs only with case") {
		t.Errorf("Decode() error = %v, want it to name the mismatched pairing", err)
	}
}
