package qualification

import (
	"path/filepath"
	"testing"
)

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
		map[string]any{"capability": "turn_disposition", "case": "runtime_refusal", "reason": DeclaredGapNeverProduced},
		map[string]any{"capability": "retry_classification", "case": "non_retryable_refusal", "reason": DeclaredGapNeverProduced},
	}
	return doc
}

func TestDecodeRuntimeProfileV4(t *testing.T) {
	t.Parallel()

	t.Run("the continuation and token_paths fixtures decode cleanly", func(t *testing.T) {
		t.Parallel()

		for _, doc := range []map[string]any{validProfileDocWithContinuation(), validProfileDocWithResumeOnly(), validProfileDocWithTokenPaths(), validProfileDocWithDeclaredPeerPair()} {
			data := marshalProfileDoc(t, doc)
			if _, err := DecodeRuntimeProfile(data); err != nil {
				t.Errorf("DecodeRuntimeProfile(%s) = _, %v, want nil", data, err)
			}
		}
	})

	t.Run("the tracked kiro profile decodes with resume_args and no seed_args", func(t *testing.T) {
		t.Parallel()

		root, err := RepositoryRootFromWD()
		if err != nil {
			t.Fatalf("RepositoryRootFromWD() = _, %v, want nil", err)
		}
		path := filepath.Join(root, "internal/qualification/profiles/kiro-cli.json")
		profile, err := ReadRuntimeProfileFile(path)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%q) = _, %v, want nil", path, err)
		}
		entry, ok := profile.EntryPoints[SurfaceNativeStreamJSON]
		if !ok {
			t.Fatalf("EntryPoints[%s] missing, want an entry point", SurfaceNativeStreamJSON)
		}
		if len(entry.ResumeArgs) == 0 {
			t.Errorf("EntryPoints[%s].ResumeArgs = %v, want non-empty", SurfaceNativeStreamJSON, entry.ResumeArgs)
		}
		if len(entry.SeedArgs) != 0 {
			t.Errorf("EntryPoints[%s].SeedArgs = %v, want empty", SurfaceNativeStreamJSON, entry.SeedArgs)
		}
	})

	tests := []struct {
		name   string
		base   func() map[string]any
		mutate func(doc map[string]any)
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
					map[string]any{"surface": "not_a_real_surface", "case": "human_input", "reason": NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry naming the aggregate surface is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "aggregate", "case": "human_input", "reason": NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry naming a catalog-wide not-inducible case is rejected",
			base: validProfileDoc,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "unknown_outcome", "reason": NotInducibleChannelTooSmall})
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
					map[string]any{"surface": "native_json", "case": "limit_reached", "reason": NotInducibleChannelTooSmall})
			},
		},
		{
			name: "a not_inducible_cases entry contradicting a declaration is rejected",
			base: validProfileDocWithDeclaredPeerPair,
			mutate: func(doc map[string]any) {
				doc["not_inducible_cases"] = append(doc["not_inducible_cases"].([]any),
					map[string]any{"surface": "native_json", "case": "runtime_refusal", "reason": NotInducibleChannelTooSmall})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc := cloneProfileDoc(t, tt.base())
			tt.mutate(doc)
			data := marshalProfileDoc(t, doc)

			if _, err := DecodeRuntimeProfile(data); err == nil {
				t.Errorf("DecodeRuntimeProfile(%s) = nil error, want a rejection", data)
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

			_, err := profile.EntryArgs(SurfaceNativeJSON, tt.model, tt.policy, tt.prompt)
			if tt.wantRejected && err == nil {
				t.Errorf("EntryArgs(%s, %q, %q, %q) = _, nil, want an error", SurfaceNativeJSON, tt.model, tt.policy, tt.prompt)
			}
			if !tt.wantRejected && err != nil {
				t.Errorf("EntryArgs(%s, %q, %q, %q) = _, %v, want nil", SurfaceNativeJSON, tt.model, tt.policy, tt.prompt, err)
			}
		})
	}
}
