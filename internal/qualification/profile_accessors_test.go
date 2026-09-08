package qualification

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

// trackedProfile decodes the repository's own tracked runtime profile,
// so the accessors below are exercised against the document an operator
// actually authors rather than against a fixture shaped to suit them.
func trackedProfile(t *testing.T) (RuntimeProfile, string) {
	t.Helper()

	root, err := RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("RepositoryRootFromWD() = _, %v, want nil", err)
	}
	path := filepath.Join(root, "internal/qualification/profiles/gemini-cli.json")
	profile, err := ReadRuntimeProfileFile(path)
	if err != nil {
		t.Fatalf("ReadRuntimeProfileFile(%q) = _, %v, want nil", path, err)
	}
	return profile, root
}

func TestRepositoryRootFromWD(t *testing.T) {
	t.Parallel()

	root, err := RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("RepositoryRootFromWD() = _, %v, want nil", err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("RepositoryRootFromWD() = %q, want an absolute path", root)
	}
}

func TestMeasuredSurfaces(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)
	got := profile.MeasuredSurfaces()

	want := []Surface{SurfaceProtocol, SurfaceNativeJSON, SurfaceNativeStreamJSON}
	if !slices.Equal(got, want) {
		t.Errorf("MeasuredSurfaces() = %v, want %v in the closed vocabulary's own order", got, want)
	}
	if slices.Contains(got, SurfaceAggregate) {
		t.Errorf("MeasuredSurfaces() = %v, want no %s: it names a cross-surface observation rather than a launch", got, SurfaceAggregate)
	}
}

func TestEntryArgs(t *testing.T) {
	t.Parallel()

	profile, _ := trackedProfile(t)

	t.Run("placeholders are substituted positionally", func(t *testing.T) {
		t.Parallel()

		argv, err := profile.EntryArgs(SurfaceNativeJSON, "a-model", "a-policy", "a prompt")
		if err != nil {
			t.Fatalf("EntryArgs(%s, ...) = _, %v, want nil", SurfaceNativeJSON, err)
		}
		for _, unresolved := range []string{"{model}", "{policy}", "{prompt}"} {
			if slices.Contains(argv, unresolved) {
				t.Errorf("EntryArgs(%s, ...) = %v, want no unresolved %s", SurfaceNativeJSON, argv, unresolved)
			}
		}
		if !slices.Contains(argv, "a prompt") {
			t.Errorf("EntryArgs(%s, ...) = %v, want the substituted prompt", SurfaceNativeJSON, argv)
		}
	})

	t.Run("a surface carrying no entry point is an error", func(t *testing.T) {
		t.Parallel()

		if _, err := profile.EntryArgs(SurfaceAggregate, "a-model", "", ""); err == nil {
			t.Errorf("EntryArgs(%s, ...) = _, nil, want an error: the surface carries no entry point", SurfaceAggregate)
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

	measurement, err := ReadMeasurementFile(path)
	if err != nil {
		t.Fatalf("ReadMeasurementFile(%q) = _, %v, want nil", path, err)
	}
	if measurement.ProfileDigest != profile.Digest() {
		t.Errorf("ReadMeasurementFile(%q).ProfileDigest = %q, want %q: the artifact is bound to the profile it measured", path, measurement.ProfileDigest, profile.Digest())
	}
}

func TestDecodeMeasurement(t *testing.T) {
	t.Parallel()

	t.Run("an unknown member is rejected", func(t *testing.T) {
		t.Parallel()

		data := []byte(`{"schema_version":1,"profile_digest":"d","measured_at":"2026-01-01","expectation":{},"surprise":true}`)
		if _, err := DecodeMeasurement(data); err == nil {
			t.Error("DecodeMeasurement(...) = _, nil, want a rejection of the unknown member")
		}
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		t.Parallel()

		if _, err := DecodeMeasurement([]byte("{")); err == nil {
			t.Error("DecodeMeasurement(\"{\") = _, nil, want a rejection")
		}
	})
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
	recognizer, ok := profile.Recognizers[SurfaceNativeJSON]
	if !ok {
		t.Fatalf("profile.Recognizers carries no %s", SurfaceNativeJSON)
	}
	if len(recognizer.ModelRequestPath) == 0 {
		t.Skipf("the tracked profile's %s recognizer reads no model-request path", SurfaceNativeJSON)
	}

	// nested wraps value in recognizer.ModelRequestPath, innermost key
	// first, and attaches the chain to a terminal the locator accepts.
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
		doc["entry_points"].(map[string]any)[string(SurfaceAggregate)] = map[string]any{"args": []string{"--x"}}
		if _, err := DecodeRuntimeProfile(marshalProfileDoc(t, doc)); err == nil {
			t.Errorf("DecodeRuntimeProfile(...) = _, nil, want a rejection: %s carries no entry point", SurfaceAggregate)
		}
	})

	t.Run("a surface declared absent without its own entry point is rejected", func(t *testing.T) {
		t.Parallel()

		doc := validProfileDoc()
		delete(doc["entry_points"].(map[string]any), string(SurfaceNativeStreamJSON))
		delete(doc["recognizers"].(map[string]any), string(SurfaceNativeStreamJSON))
		doc["absent_surfaces"] = []any{
			map[string]any{"surface": string(SurfaceNativeStreamJSON), "reason": SurfaceNotOffered},
		}
		if _, err := DecodeRuntimeProfile(marshalProfileDoc(t, doc)); err == nil {
			t.Error("DecodeRuntimeProfile(...) = _, nil, want a rejection: an absence with no launch cannot be corroborated")
		}
	})
}
