// Package probe drives the live qualification profile against any
// runtime a [qualification.RuntimeProfile] describes: it resolves the
// operator's coordinates, gates the live run behind
// SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST, and collects, validates,
// and persists the evidence a run produces. Start with [Gated] to
// resolve the coordinates a live test needs, and [Run] to drive one
// collection.
package probe

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// The qualification profile's own coordinates. They are test
// coordinates only: names are read and printed, values never are.
const (
	qualificationGateEnv      = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST"
	qualificationCommandEnv   = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND"
	qualificationModelEnv     = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL"
	qualificationAuthNamesEnv = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES"
	qualificationProfileEnv   = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE"
	qualificationSkipReason   = "skipping Agent Client Protocol qualification: set SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST=1 to enable the live profile"
)

// Coordinates is the resolved coordinate set a live profile launches
// with. It carries names and the decoded profile only: no credential
// value ever travels through it.
type Coordinates struct {
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

	// Profile is the decoded runtime profile the run measures against.
	Profile qualification.RuntimeProfile

	// OutputDir is where Run writes its three artifacts. It defaults to
	// a run-scoped directory under the OS temporary root when empty.
	OutputDir string
}

// Result is what one Run call produced.
type Result struct {
	Verdict         qualification.Verdict
	Measurement     qualification.Measurement
	EvidencePath    string
	SummaryPath     string
	MeasurementPath string
}

// isExecutableMode reports whether the file mode carries any execute
// permission. Windows executability is decided by the loader rather
// than a mode bit, so the check applies only where the bit exists.
func isExecutableMode(mode os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return mode.Perm()&0o111 != 0
}

// parseCommand resolves exactly one executable path with no arguments.
// A bare name resolves through PATH; a path with a separator resolves
// directly.
func parseCommand(raw string) (string, error) {
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
		if !isExecutableMode(info.Mode()) {
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

// parseAuthEnvNames parses the comma-separated list of authentication
// environment variable names. Entries are trimmed; an empty entry or a
// duplicate name is rejected.
func parseAuthEnvNames(raw string) ([]string, error) {
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

// coordinateValue reads one coordinate. A coordinate absent from env
// resolves to the empty string, which each coordinate's own rule
// rejects with its named diagnostic.
func coordinateValue(env func(string) (string, bool), name string) string {
	value, _ := env(name)
	return value
}

// ResolveCoordinates resolves the five gate-enabled coordinates:
// exactly one executable path, one model identifier, a valid list of
// authentication environment names that all exist in the parent
// environment, and one readable runtime profile document. env reports
// a coordinate's value and whether it is present.
func ResolveCoordinates(env func(string) (string, bool)) (Coordinates, error) {
	command := coordinateValue(env, qualificationCommandEnv)
	commandPath, err := parseCommand(command)
	if err != nil {
		return Coordinates{}, err
	}

	model := coordinateValue(env, qualificationModelEnv)
	if model == "" {
		return Coordinates{}, fmt.Errorf("%s must name one model identifier for every surface", qualificationModelEnv)
	}

	rawNames := coordinateValue(env, qualificationAuthNamesEnv)
	authNames, err := parseAuthEnvNames(rawNames)
	if err != nil {
		return Coordinates{}, err
	}
	for _, name := range authNames {
		if _, present := env(name); !present {
			return Coordinates{}, fmt.Errorf("authentication environment variable %q named by %s is absent from the invoking environment", name, qualificationAuthNamesEnv)
		}
	}

	profilePath, present := env(qualificationProfileEnv)
	if !present || strings.TrimSpace(profilePath) == "" {
		return Coordinates{}, fmt.Errorf("%s must name one readable runtime profile document", qualificationProfileEnv)
	}
	profile, err := qualification.ReadRuntimeProfileFile(profilePath)
	if err != nil {
		return Coordinates{}, fmt.Errorf("%s names %q, which failed to load: %w", qualificationProfileEnv, profilePath, err)
	}

	return Coordinates{
		CommandPath:  commandPath,
		Model:        model,
		AuthEnvNames: authNames,
		Profile:      profile,
	}, nil
}

// Gated reports false and calls t.Skip naming the gate variable when
// SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST is not "1". With the gate
// set it resolves the coordinates and calls t.Fatalf on any missing or
// invalid one.
func Gated(t *testing.T) (Coordinates, bool) {
	t.Helper()
	if os.Getenv(qualificationGateEnv) != "1" {
		t.Skip(qualificationSkipReason)
	}
	coords, err := ResolveCoordinates(os.LookupEnv)
	if err != nil {
		t.Fatalf("the qualification gate is enabled but a prerequisite is missing: %v", err)
	}
	return coords, true
}
