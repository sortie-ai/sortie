package clientprotocol

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The Gemini qualification profile's own coordinates. They are test
// coordinates only: names are read and printed, values never are. The
// generic integration helper's variables stay untouched.
const (
	qualificationGateEnv             = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST"
	qualificationCommandEnv          = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND"
	qualificationModelEnv            = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL"
	qualificationAuthNamesEnv        = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES"
	geminiQualificationRunHomeEnv    = "HOME"
	geminiQualificationRunCLIHomeEnv = "GEMINI_CLI_HOME"
	geminiQualificationExecPathEnv   = "PATH"
	qualificationSkipReason          = "skipping Agent Client Protocol qualification: set SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST=1 to enable the live profile"
	qualificationNonUnixFailure      = "Agent Client Protocol qualification requires a Unix process-group oracle"
)

// geminiQualificationToolchainEnvNames are environment variable names a
// shim-based version manager needs to resolve the real interpreter
// behind a command named through PATH, forwarded to the measurement
// subprocess only when present in the invoking environment. A name
// resolved this way, for example the asdf CLI's exec dispatcher,
// otherwise depends on the real HOME to locate the installed toolchain,
// which the run-scoped HOME the qualification profile isolates with
// does not provide.
var geminiQualificationToolchainEnvNames = []string{"ASDF_DATA_DIR"}

// geminiQualificationConfig is the resolved coordinate set the live
// profile launches with. It carries names only: no credential value,
// model payload, or environment value is ever stored in or printed
// from it beyond the operator-selected model identifier, which is not
// a credential.
type geminiQualificationConfig struct {
	// CommandPath is the single resolved executable path with no
	// arguments. Surface-specific flags are appended by the launchers,
	// never stored here.
	CommandPath string

	// Model is the one operator-selected model identifier applied to
	// every surface.
	Model string

	// AuthEnvNames are the authentication environment variable names,
	// trimmed, in declared order. Values never travel with them.
	AuthEnvNames []string

	// EnvAllowlist is the sorted minimum allowlist of environment
	// variable names the measurement subprocess may inherit.
	EnvAllowlist []string
}

// requireGeminiQualification skips the test when the qualification
// gate is disabled and otherwise resolves the live
// coordinates, failing the test rather than skipping when any
// prerequisite is missing or invalid.
func requireGeminiQualification(t *testing.T) geminiQualificationConfig {
	t.Helper()
	if os.Getenv(qualificationGateEnv) != "1" {
		t.Skip(qualificationSkipReason)
	}
	config, err := geminiResolveQualificationCoordinates(os.LookupEnv)
	if err != nil {
		t.Fatalf("the qualification gate is enabled but a prerequisite is missing: %v", err)
	}
	return config
}

// geminiResolveQualificationCoordinates resolves the three enabled-gate
// coordinates: exactly one executable path, one model identifier, and
// a valid list of authentication environment names that all exist in
// the parent environment. env reports a coordinate's value and whether
// it is present; the resolved config's allowlist is built from the
// resolved authentication names.
func geminiResolveQualificationCoordinates(env func(string) (string, bool)) (geminiQualificationConfig, error) {
	command := geminiCoordinateValue(env, qualificationCommandEnv)
	commandPath, err := parseGeminiQualificationCommand(command)
	if err != nil {
		return geminiQualificationConfig{}, err
	}

	model := geminiCoordinateValue(env, qualificationModelEnv)
	if model == "" {
		return geminiQualificationConfig{}, fmt.Errorf("%s must name one model identifier for every surface", qualificationModelEnv)
	}

	rawNames := geminiCoordinateValue(env, qualificationAuthNamesEnv)
	authNames, err := parseGeminiQualificationAuthEnvNames(rawNames)
	if err != nil {
		return geminiQualificationConfig{}, err
	}
	for _, name := range authNames {
		if _, present := env(name); !present {
			return geminiQualificationConfig{}, fmt.Errorf("authentication environment variable %q named by %s is absent from the invoking environment", name, qualificationAuthNamesEnv)
		}
	}

	return geminiQualificationConfig{
		CommandPath:  commandPath,
		Model:        model,
		AuthEnvNames: authNames,
		EnvAllowlist: geminiQualificationEnvAllowlist(authNames, geminiQualificationPresentToolchainEnvNames(env)),
	}, nil
}

// geminiQualificationPresentToolchainEnvNames returns the names from
// geminiQualificationToolchainEnvNames that env reports present, in
// declared order. A name absent from the invoking environment is
// omitted rather than failing the resolution: unlike the required
// coordinates, no version manager is mandatory.
func geminiQualificationPresentToolchainEnvNames(env func(string) (string, bool)) []string {
	var present []string
	for _, name := range geminiQualificationToolchainEnvNames {
		if _, ok := env(name); ok {
			present = append(present, name)
		}
	}
	return present
}

// geminiCoordinateValue reads one coordinate. A coordinate absent from
// env resolves to the empty string, which each coordinate's own rule
// rejects with its named diagnostic.
func geminiCoordinateValue(env func(string) (string, bool), name string) string {
	value, _ := env(name)
	return value
}

// parseGeminiQualificationCommand resolves exactly one executable path
// with no arguments. A bare name resolves through PATH; a path with a
// separator resolves directly. Any argument-bearing value, empty
// value, directory, or missing executable is rejected without echoing
// the raw coordinate value.
func parseGeminiQualificationCommand(raw string) (string, error) {
	command := strings.TrimSpace(raw)
	if command == "" {
		return "", fmt.Errorf("%s must name exactly one executable path with no arguments", qualificationCommandEnv)
	}
	if strings.ContainsAny(command, " \t\n\r") {
		return "", fmt.Errorf("%s must be one executable path with no arguments; surface-specific flags are appended by the harness", qualificationCommandEnv)
	}

	if strings.ContainsRune(command, '/') || strings.ContainsRune(command, '\\') {
		info, err := os.Stat(command)
		if err != nil {
			return "", fmt.Errorf("%s does not resolve to an executable file", qualificationCommandEnv)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s names a directory, want one executable file", qualificationCommandEnv)
		}
		if !isGeminiExecutableMode(info.Mode()) {
			return "", fmt.Errorf("%s names a file that is not executable", qualificationCommandEnv)
		}
		return command, nil
	}

	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("%s does not resolve to an executable on PATH", qualificationCommandEnv)
	}
	return resolved, nil
}

// isGeminiExecutableMode reports whether the file mode carries any
// execute permission. Windows executability is decided by the loader
// rather than a mode bit, so the check applies only where the bit
// exists.
func isGeminiExecutableMode(mode os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return mode.Perm()&0o111 != 0
}

// parseGeminiQualificationAuthEnvNames parses the comma-separated list
// of authentication environment variable names. Entries are trimmed;
// an empty entry or a duplicate name is rejected. The list carries
// names only, never values.
func parseGeminiQualificationAuthEnvNames(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s must be a comma-separated list of non-empty environment variable names", qualificationAuthNamesEnv)
	}

	var names []string
	for entry := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(entry)
		switch {
		case name == "":
			return nil, fmt.Errorf("%s carries an empty entry after trimming", qualificationAuthNamesEnv)
		case strings.ContainsAny(name, " \t\n\r="):
			return nil, fmt.Errorf("%s entry %q is not a bare environment variable name", qualificationAuthNamesEnv, name)
		case slices.Contains(names, name):
			return nil, fmt.Errorf("%s names %q more than once", qualificationAuthNamesEnv, name)
		}
		names = append(names, name)
	}
	return names, nil
}

// geminiQualificationEnvAllowlist builds the minimum environment-name
// allowlist the measurement subprocess may inherit: platform process
// essentials, the run-scoped HOME and GEMINI_CLI_HOME coordinates, the
// named authentication entries, the toolchain names present in the
// invoking environment, and nothing else. Names are sorted
// lexicographically, which is also the order the workspace-security
// record reports them in. Values are never part of an allowlist.
func geminiQualificationEnvAllowlist(authNames, presentToolchainNames []string) []string {
	allowlist := []string{geminiQualificationExecPathEnv, geminiQualificationRunHomeEnv, geminiQualificationRunCLIHomeEnv}
	for _, name := range slices.Concat(authNames, presentToolchainNames) {
		if !slices.Contains(allowlist, name) {
			allowlist = append(allowlist, name)
		}
	}
	slices.Sort(allowlist)
	return allowlist
}

// geminiBuildQualificationSubprocessEnv renders the measurement
// subprocess's environment from the allowlist: the run-scoped home
// coordinates under their own names, each authentication entry's value
// read from the invoking environment, and the PATH value needed to
// execute the selected binary and its children. No value is ever
// logged, and no undeclared orchestrator entry crosses into the
// result.
func geminiBuildQualificationSubprocessEnv(config geminiQualificationConfig, homeDir, cliHomeDir string) ([]string, error) {
	env := make([]string, 0, len(config.EnvAllowlist))
	for _, name := range config.EnvAllowlist {
		switch name {
		case geminiQualificationRunHomeEnv:
			env = append(env, name+"="+homeDir)
		case geminiQualificationRunCLIHomeEnv:
			env = append(env, name+"="+cliHomeDir)
		default:
			value, present := os.LookupEnv(name)
			if !present {
				return nil, fmt.Errorf("allowlisted environment variable %s is absent from the invoking environment", name)
			}
			env = append(env, name+"="+value)
		}
	}
	return env, nil
}

// TestRequireGeminiQualificationDisabled confirms the gate's disabled
// behavior: with SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST unset or not
// "1", requireGeminiQualification skips before resolving any
// coordinate, so no qualification scenario runs and none fails. The
// enabled-gate coordinates resolve in the same test through the
// injected resolver, which is what the enabled paths call.
func TestRequireGeminiQualificationDisabled(t *testing.T) {
	t.Run("skip reason text is the one explicit reason", func(t *testing.T) {
		t.Parallel()

		if !strings.HasPrefix(qualificationSkipReason, "skipping Agent Client Protocol qualification: ") {
			t.Errorf("skip reason = %q, want it to state the skip and the enabling gate", qualificationSkipReason)
		}
		if !strings.Contains(qualificationSkipReason, qualificationGateEnv+"=1") {
			t.Errorf("skip reason = %q, want it to name %s=1", qualificationSkipReason, qualificationGateEnv)
		}
	})

	for _, gate := range []string{"", "0", "true"} {
		t.Run("gate value "+gate, func(t *testing.T) {
			t.Setenv(qualificationGateEnv, gate)

			proceeded := false
			t.Run("requireGeminiQualification skips", func(t *testing.T) {
				requireGeminiQualification(t)
				// An unskipped call with a disabled gate would be a gate
				// defect; reaching this line means no skip happened.
				proceeded = true
			})
			if proceeded {
				t.Errorf("requireGeminiQualification() proceeded with gate %q, want a clean skip", gate)
			}
		})
	}

	t.Run("non-unix enabled failure text is pinned", func(t *testing.T) {
		t.Parallel()

		if qualificationNonUnixFailure != "Agent Client Protocol qualification requires a Unix process-group oracle" {
			t.Errorf("non-unix failure = %q, want the exact unsupported-platform diagnostic", qualificationNonUnixFailure)
		}
	})
}

// TestGeminiQualificationConfigRejectsMissingCoordinates confirms the
// enabled gate fails, never skips, when a coordinate is missing or the
// command cannot resolve, with a diagnostic naming the prerequisite
// class and never the coordinate value.
func TestGeminiQualificationConfigRejectsMissingCoordinates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		coords  map[string]string
		wantSub string
		banned  []string
	}{
		{name: "gate coordinate missing entirely", coords: map[string]string{}, wantSub: qualificationCommandEnv},
		{
			name: "command coordinate missing",
			coords: map[string]string{
				qualificationCommandEnv:   "",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "command carries arguments",
			coords: map[string]string{
				qualificationCommandEnv:   "gemini --acp",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
			},
			wantSub: qualificationCommandEnv,
			banned:  []string{"gemini --acp"},
		},
		{
			name: "command resolves to a directory",
			coords: map[string]string{
				qualificationCommandEnv:   "@DIR@",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "command names a missing file",
			coords: map[string]string{
				qualificationCommandEnv:   "@MISSING@",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
			},
			wantSub: qualificationCommandEnv,
		},
		{
			name: "model coordinate empty",
			coords: map[string]string{
				qualificationCommandEnv:   "@EXEC@",
				qualificationModelEnv:     "",
				qualificationAuthNamesEnv: "FIXTURE_AUTH_TOKEN_NAME",
			},
			wantSub: qualificationModelEnv,
		},
		{
			name: "auth name coordinate missing",
			coords: map[string]string{
				qualificationCommandEnv:   "@EXEC@",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "",
			},
			wantSub: qualificationAuthNamesEnv,
		},
		{
			name: "named authentication entry absent from the environment",
			coords: map[string]string{
				qualificationCommandEnv:   "@EXEC@",
				qualificationModelEnv:     "gemini-fixture-model",
				qualificationAuthNamesEnv: "FIXTURE_ABSENT_AUTH_NAME",
			},
			wantSub: "absent from the invoking environment",
		},
	}

	dir := t.TempDir()
	executable := writeGeminiConfigExecutable(t, dir)
	for i := range tests {
		tests[i].coords[qualificationCommandEnv] = strings.ReplaceAll(tests[i].coords[qualificationCommandEnv], "@EXEC@", executable)
		tests[i].coords[qualificationCommandEnv] = strings.ReplaceAll(tests[i].coords[qualificationCommandEnv], "@DIR@", dir)
		tests[i].coords[qualificationCommandEnv] = strings.ReplaceAll(tests[i].coords[qualificationCommandEnv], "@MISSING@", filepath.Join(dir, "definitely-not-here"))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := func(name string) (string, bool) {
				value, ok := tt.coords[name]
				return value, ok
			}
			config, err := geminiResolveQualificationCoordinates(env)
			if err == nil {
				t.Fatalf("geminiResolveQualificationCoordinates() = %+v, want an error", config)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("geminiResolveQualificationCoordinates() error = %v, want it to name %q", err, tt.wantSub)
			}
			for _, banned := range tt.banned {
				if strings.Contains(err.Error(), banned) {
					t.Errorf("error %v leaks the coordinate value %q, want names only", err, banned)
				}
			}
		})
	}

	t.Run("complete coordinates resolve once", func(t *testing.T) {
		t.Parallel()

		env := func(name string) (string, bool) {
			switch name {
			case qualificationCommandEnv:
				return executable, true
			case qualificationModelEnv:
				return "gemini-fixture-model", true
			case qualificationAuthNamesEnv:
				return " FIXTURE_AUTH_ONE ,FIXTURE_AUTH_TWO", true
			case "FIXTURE_AUTH_ONE", "FIXTURE_AUTH_TWO":
				return "present-but-never-printed", true
			}
			return "", false
		}
		config, err := geminiResolveQualificationCoordinates(env)
		if err != nil {
			t.Fatalf("geminiResolveQualificationCoordinates() error = %v, want nil", err)
		}
		if config.CommandPath != executable {
			t.Errorf("CommandPath = %q, want %q", config.CommandPath, executable)
		}
		if config.Model != "gemini-fixture-model" {
			t.Errorf("Model = %q, want %q", config.Model, "gemini-fixture-model")
		}
		if !slices.Equal(config.AuthEnvNames, []string{"FIXTURE_AUTH_ONE", "FIXTURE_AUTH_TWO"}) {
			t.Errorf("AuthEnvNames = %v, want trimmed, duplicate-free declared order", config.AuthEnvNames)
		}
	})
}

// TestGeminiQualificationAuthNameValidation pins the authentication
// name list rules with table-driven cases: trimming, empty entries,
// duplicates, bare-name shape, and names-only semantics.
func TestGeminiQualificationAuthNameValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "single name", raw: "FIXTURE_AUTH_TOKEN_NAME", want: []string{"FIXTURE_AUTH_TOKEN_NAME"}},
		{name: "trimmed entries", raw: " ONE , TWO\t,THREE ", want: []string{"ONE", "TWO", "THREE"}},
		{name: "empty list", raw: "", wantErr: true},
		{name: "blank list", raw: "   ", wantErr: true},
		{name: "empty entry", raw: "ONE,,TWO", wantErr: true},
		{name: "whitespace-only entry", raw: "ONE, ,TWO", wantErr: true},
		{name: "duplicate name", raw: "ONE,TWO,ONE", wantErr: true},
		{name: "entry with a value shape", raw: "ONE=secret", wantErr: true},
		{name: "entry with interior space", raw: "ONE TWO", wantErr: true},
		{name: "trailing comma", raw: "ONE,", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseGeminiQualificationAuthEnvNames(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGeminiQualificationAuthEnvNames(%q) = %v, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGeminiQualificationAuthEnvNames(%q) error = %v, want nil", tt.raw, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseGeminiQualificationAuthEnvNames(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestGeminiQualificationEnvironmentAllowlist confirms the allowlist
// carries exactly the platform essentials, the run-scoped home
// coordinates, and the authentication names, sorted lexicographically,
// and that the built subprocess environment contains only those
// names with the run-scoped homes overriding the invoking ones and no
// undeclared orchestrator entry admitted. The test stays sequential:
// its built-environment subtest needs t.Setenv, which cannot run under
// a parallel parent.
func TestGeminiQualificationEnvironmentAllowlist(t *testing.T) {
	authNames := []string{"FIXTURE_AUTH_B", "FIXTURE_AUTH_A"}
	allowlist := geminiQualificationEnvAllowlist(authNames, nil)
	want := []string{"FIXTURE_AUTH_A", "FIXTURE_AUTH_B", "GEMINI_CLI_HOME", "HOME", "PATH"}
	if !slices.Equal(allowlist, want) {
		t.Errorf("geminiQualificationEnvAllowlist(%v) = %v, want %v sorted lexicographically", authNames, allowlist, want)
	}

	t.Run("a present toolchain name joins the allowlist and an absent one does not", func(t *testing.T) {
		if !slices.Contains(geminiQualificationToolchainEnvNames, "ASDF_DATA_DIR") {
			t.Fatalf("toolchain names = %v, want the shim data directory among them", geminiQualificationToolchainEnvNames)
		}

		present := geminiQualificationPresentToolchainEnvNames(func(name string) (string, bool) {
			return "/fixture/toolchain/data", slices.Contains(geminiQualificationToolchainEnvNames, name)
		})
		if !slices.Equal(present, geminiQualificationToolchainEnvNames) {
			t.Fatalf("present names = %v, want every declared name in declared order", present)
		}
		withToolchain := geminiQualificationEnvAllowlist(authNames, present)
		for _, name := range geminiQualificationToolchainEnvNames {
			if !slices.Contains(withToolchain, name) {
				t.Errorf("allowlist %v omits the present toolchain name %s", withToolchain, name)
			}
		}
		if !slices.IsSorted(withToolchain) {
			t.Errorf("allowlist %v is not sorted lexicographically", withToolchain)
		}

		absent := geminiQualificationPresentToolchainEnvNames(func(string) (string, bool) { return "", false })
		if len(absent) != 0 {
			t.Fatalf("present names = %v, want none when the invoking environment declares none", absent)
		}
		if got := geminiQualificationEnvAllowlist(authNames, absent); !slices.Equal(got, want) {
			t.Errorf("allowlist without a toolchain name = %v, want %v", got, want)
		}
	})

	t.Run("built environment carries exactly the allowlist names", func(t *testing.T) {
		t.Setenv("SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST", "irrelevant-undeclared-entry-must-not-cross")
		t.Setenv("FIXTURE_AUTH_A", "value-a-not-logged")
		t.Setenv("FIXTURE_AUTH_B", "value-b-not-logged")

		config := geminiQualificationConfig{EnvAllowlist: allowlist}
		env, err := geminiBuildQualificationSubprocessEnv(config, "/run-scoped/home", "/run-scoped/cli-home")
		if err != nil {
			t.Fatalf("geminiBuildQualificationSubprocessEnv() error = %v, want nil", err)
		}

		got := make(map[string]string, len(env))
		for _, entry := range env {
			name, value, found := strings.Cut(entry, "=")
			if !found {
				t.Fatalf("environment entry %q carries no value separator", entry)
			}
			got[name] = value
		}
		if len(got) != len(allowlist) {
			t.Errorf("environment holds %d entries, want exactly the %d allowlisted names", len(got), len(allowlist))
		}
		for _, name := range allowlist {
			if _, present := got[name]; !present {
				t.Errorf("environment is missing allowlisted name %s", name)
			}
		}
		if leaked := got["SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST"]; leaked != "" {
			t.Errorf("environment carries undeclared orchestrator entry SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST")
		}
		if got["HOME"] != "/run-scoped/home" || got["GEMINI_CLI_HOME"] != "/run-scoped/cli-home" {
			t.Errorf("home coordinates = %q/%q, want the run-scoped values to override the invoking environment", got["HOME"], got["GEMINI_CLI_HOME"])
		}
		if got["FIXTURE_AUTH_A"] == "" || got["FIXTURE_AUTH_B"] == "" {
			t.Errorf("authentication entries = %q/%q, want each named entry's value carried without logging", got["FIXTURE_AUTH_A"], got["FIXTURE_AUTH_B"])
		}
		if got["PATH"] == "" {
			t.Error("environment carries no PATH, want the value needed to execute the selected binary")
		}
	})

	t.Run("absent allowlisted authentication entry fails closed", func(t *testing.T) {
		t.Parallel()

		config := geminiQualificationConfig{EnvAllowlist: []string{"HOME", "GEMINI_CLI_HOME", "PATH", "FIXTURE_ABSENT_AUTH_NAME"}}
		if _, err := geminiBuildQualificationSubprocessEnv(config, "/home", "/cli-home"); err == nil {
			t.Error("geminiBuildQualificationSubprocessEnv() = nil error, want a failure for an absent allowlisted entry")
		}
	})
}

// writeGeminiConfigExecutable writes a minimal executable file under
// dir and returns its path. On Windows the loader decides
// executability, so the mode bit is only set where it exists.
func writeGeminiConfigExecutable(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "gemini-fixture-executable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // a cross-platform fixture executable under the test's own temp directory
		t.Fatalf("write fixture executable: %v", err)
	}
	return path
}
