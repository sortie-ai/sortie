package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sampleProfileJSON is a fully valid runtime profile document,
// independent of any tracked profile under
// internal/qualification/profiles, so this package's own tests do not
// couple to a production fixture's content.
const sampleProfileJSON = `{
  "schema_version": 3,
  "runtime_id": "sample-runtime",
  "identity_tokens": ["sample"],
  "notes_path": "docs/sample-notes.md",
  "measurement_path": "internal/qualification/probe/testdata/sample/measurement.json",
  "published_sample": "examples/WORKFLOW.sample.md",
  "tool_name_format": "mcp_{server}_{tool}",
  "project_config_paths": [],
  "version_args": ["--version"],
  "model_args": ["--model", "{model}"],
  "capability_gap_labels": ["token counts"],
  "entry_points": {
    "protocol": {"args": ["--acp"]},
    "native_json": {"args": ["--output-format", "json", "--prompt", "{prompt}"]},
    "native_stream_json": {"args": ["--output-format", "stream-json", "--prompt", "{prompt}"]}
  },
  "recognizers": {
    "native_json": {
      "locator": {"mode": "first_value", "discriminator_key": "", "discriminator_value": ""},
      "error_members": ["error"],
      "success_member": "response",
      "status_member": "",
      "status_cases": {},
      "status_end_turn": [],
      "model_request_path": []
    },
    "native_stream_json": {
      "locator": {"mode": "discriminated", "discriminator_key": "type", "discriminator_value": "result"},
      "error_members": ["error"],
      "success_member": "",
      "status_member": "status",
      "status_cases": {"refusal": "runtime_refusal", "cancelled": "cancellation"},
      "status_end_turn": ["end_turn"],
      "model_request_path": ["stats", "models"]
    }
  },
  "declarations": [],
  "absent_surfaces": []
}`

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

// writeValidProfileFixture builds a synthetic repository root under
// t.TempDir(), sufficient for qualification.ReadRuntimeProfileFile to
// accept sampleProfileJSON in full: a go.mod marker and the notes,
// measurement, and published-sample files the document names. It
// returns the profile file's own path.
func writeValidProfileFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "go.mod"), "module fixture\n\ngo 1.24\n")
	mustWriteFile(t, filepath.Join(root, "docs", "sample-notes.md"), "# sample notes\n")
	mustWriteFile(t, filepath.Join(root, "internal", "qualification", "probe", "testdata", "sample", "measurement.json"), "{}")
	mustWriteFile(t, filepath.Join(root, "examples", "WORKFLOW.sample.md"), "---\nagent:\n  kind: agent-client-protocol\n  command: sample-runtime --acp\n---\nbody\n")
	profilePath := filepath.Join(root, "sample-runtime.json")
	mustWriteFile(t, profilePath, sampleProfileJSON)
	return profilePath
}

// writeFixtureExecutable writes an executable shell script under dir,
// returning its path. It is a cross-platform-shaped fixture: the
// content never actually runs in these tests, only its path and
// executable bit are read.
func writeFixtureExecutable(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fixture-executable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // a cross-platform fixture executable under the test's own temp directory
		t.Fatalf("write fixture executable: %v", err)
	}
	return path
}

// TestResolveCoordinates confirms the enabled gate fails, never
// resolves, when a coordinate is missing or invalid, with a diagnostic
// naming the prerequisite class and never a credential value, and that
// a complete coordinate set resolves once including the required
// runtime profile.
func TestResolveCoordinates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executable := writeFixtureExecutable(t, dir)
	profilePath := writeValidProfileFixture(t)

	brokenProfilePath := filepath.Join(dir, "broken-profile.json")
	mustWriteFile(t, brokenProfilePath, `{"schema_version": 3,`)

	mismatchedProfilePath := filepath.Join(dir, "wrong-name.json")
	mustWriteFile(t, mismatchedProfilePath, sampleProfileJSON)

	tests := []struct {
		name    string
		coords  map[string]string
		wantSub string
		banned  []string
	}{
		{name: "every coordinate missing", coords: map[string]string{}, wantSub: qualificationCommandEnv},
		{
			name: "command coordinate missing",
			coords: map[string]string{
				qualificationCommandEnv:   "",
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "command carries arguments",
			coords: map[string]string{
				qualificationCommandEnv:   "fixture --acp",
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationCommandEnv,
			banned:  []string{"fixture --acp"},
		},
		{
			name: "command resolves to a directory",
			coords: map[string]string{
				qualificationCommandEnv:   dir,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "command names a missing file",
			coords: map[string]string{
				qualificationCommandEnv:   filepath.Join(dir, "definitely-not-here"),
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "model coordinate empty",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationModelEnv,
		},
		{
			name: "auth names coordinate missing",
			coords: map[string]string{
				qualificationCommandEnv: executable,
				qualificationModelEnv:   "fixture-model",
				qualificationProfileEnv: profilePath,
			},
			wantSub: qualificationAuthNamesEnv,
		},
		{
			name: "auth names entry empty after trimming",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_ONE, ,FIXTURE_AUTH_TWO",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationAuthNamesEnv,
		},
		{
			name: "auth names entry duplicated",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_ONE,FIXTURE_AUTH_ONE",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: qualificationAuthNamesEnv,
		},
		{
			name: "named authentication entry absent from the environment",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_ABSENT_AUTH_NAME",
				qualificationProfileEnv:   profilePath,
			},
			wantSub: "absent from the invoking environment",
		},
		{
			name: "profile coordinate missing entirely",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				"FIXTURE_AUTH_TOKEN_NAME": "present-but-never-printed",
			},
			wantSub: qualificationProfileEnv,
		},
		{
			name: "profile coordinate empty",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				"FIXTURE_AUTH_TOKEN_NAME": "present-but-never-printed",
				qualificationProfileEnv:   "",
			},
			wantSub: qualificationProfileEnv,
		},
		{
			name: "profile coordinate names a file that does not exist",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				"FIXTURE_AUTH_TOKEN_NAME": "present-but-never-printed",
				qualificationProfileEnv:   filepath.Join(dir, "does-not-exist.json"),
			},
			wantSub: qualificationProfileEnv,
		},
		{
			name: "profile coordinate names a document that fails to decode",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				"FIXTURE_AUTH_TOKEN_NAME": "present-but-never-printed",
				qualificationProfileEnv:   brokenProfilePath,
			},
			wantSub: qualificationProfileEnv,
		},
		{
			name: "profile coordinate fails a stage-R rule",
			coords: map[string]string{
				qualificationCommandEnv:   executable,
				qualificationModelEnv:     "fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
				"FIXTURE_AUTH_TOKEN_NAME": "present-but-never-printed",
				qualificationProfileEnv:   mismatchedProfilePath,
			},
			wantSub: qualificationProfileEnv,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := func(name string) (string, bool) {
				value, ok := tt.coords[name]
				return value, ok
			}
			coords, err := ResolveCoordinates(env)
			if err == nil {
				t.Fatalf("ResolveCoordinates() = %+v, want an error", coords)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("ResolveCoordinates() error = %v, want it to name %q", err, tt.wantSub)
			}
			for _, banned := range tt.banned {
				if strings.Contains(err.Error(), banned) {
					t.Errorf("error %v leaks the coordinate value %q, want names only", err, banned)
				}
			}
		})
	}

	t.Run("a complete coordinate set resolves once", func(t *testing.T) {
		t.Parallel()

		env := func(name string) (string, bool) {
			switch name {
			case qualificationCommandEnv:
				return executable, true
			case qualificationModelEnv:
				return "fixture-model", true
			case qualificationAuthNamesEnv:
				return " FIXTURE_AUTH_ONE ,FIXTURE_AUTH_TWO", true
			case qualificationProfileEnv:
				return profilePath, true
			case "FIXTURE_AUTH_ONE", "FIXTURE_AUTH_TWO":
				return "present-but-never-printed", true
			}
			return "", false
		}
		coords, err := ResolveCoordinates(env)
		if err != nil {
			t.Fatalf("ResolveCoordinates() error = %v, want nil", err)
		}
		if coords.CommandPath != executable {
			t.Errorf("CommandPath = %q, want %q", coords.CommandPath, executable)
		}
		if coords.Model != "fixture-model" {
			t.Errorf("Model = %q, want %q", coords.Model, "fixture-model")
		}
		if !slices.Equal(coords.AuthEnvNames, []string{"FIXTURE_AUTH_ONE", "FIXTURE_AUTH_TWO"}) {
			t.Errorf("AuthEnvNames = %v, want trimmed, duplicate-free declared order", coords.AuthEnvNames)
		}
		if coords.Profile.RuntimeID != "sample-runtime" {
			t.Errorf("Profile.RuntimeID = %q, want %q", coords.Profile.RuntimeID, "sample-runtime")
		}
	})
}

// TestGated confirms the gate skips cleanly when unset, and resolves
// coordinates without failing or launching anything when set with a
// complete, valid coordinate set.
func TestGated(t *testing.T) {
	t.Run("gate unset skips cleanly", func(t *testing.T) {
		t.Setenv(qualificationGateEnv, "0")

		var sub *testing.T
		t.Run("gated call", func(st *testing.T) {
			sub = st
			st.Setenv(qualificationGateEnv, "0")
			Gated(st)
			st.Error("Gated() returned instead of skipping with the gate unset, want it to have called t.Skip before this line")
		})
		if sub == nil || !sub.Skipped() {
			t.Error("Gated() with an unset gate did not report Skipped()")
		}
	})

	t.Run("gate set resolves a complete coordinate set without failing", func(t *testing.T) {
		dir := t.TempDir()
		executable := writeFixtureExecutable(t, dir)
		profilePath := writeValidProfileFixture(t)

		t.Setenv(qualificationGateEnv, "1")
		t.Setenv(qualificationCommandEnv, executable)
		t.Setenv(qualificationModelEnv, "fixture-model")
		t.Setenv("FIXTURE_AUTH_TOKEN_NAME", "unused-value")
		t.Setenv(qualificationAuthNamesEnv, "FIXTURE_AUTH_TOKEN_NAME")
		t.Setenv(qualificationProfileEnv, profilePath)

		coords, ok := Gated(t)
		if !ok {
			t.Fatal("Gated() = _, false, want true with the gate enabled and every coordinate valid")
		}
		if coords.CommandPath != executable {
			t.Errorf("Gated().CommandPath = %q, want %q", coords.CommandPath, executable)
		}
		if coords.Profile.RuntimeID != "sample-runtime" {
			t.Errorf("Gated().Profile.RuntimeID = %q, want %q", coords.Profile.RuntimeID, "sample-runtime")
		}
	})

}

// TestGatedFatalsOnInvalidCoordinate confirms Gated fails rather than
// skips when the gate is enabled and a coordinate is invalid. It drives
// this through a subprocess, matching the standard library's own
// TestHelperProcess idiom: calling t.Fatalf directly against this
// test's own *testing.T would mark this whole package's run failed,
// which is not the behavior under test here.
func TestGatedFatalsOnInvalidCoordinate(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_GATED_HELPER_PROCESS") == "1" {
		Gated(t)
		t.Fatal("Gated() returned instead of calling t.Fatalf on a missing command coordinate")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestGatedFatalsOnInvalidCoordinate$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(),
		"PROBE_GATED_HELPER_PROCESS=1",
		qualificationGateEnv+"=1",
		qualificationCommandEnv+"=",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from Gated()'s t.Fatalf on a missing %s; output:\n%s", qualificationCommandEnv, output)
	}
	if !strings.Contains(string(output), qualificationCommandEnv) {
		t.Errorf("helper process output = %s, want it to name %s", output, qualificationCommandEnv)
	}
}
