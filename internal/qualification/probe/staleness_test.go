package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"

	"gopkg.in/yaml.v3"
)

// Enumerated through a filesystem read rather than git grep, which would pass
// vacuously on an untracked new artifact.
const stalenessProfilesDir = "../profiles"

const stalenessExamplesDir = "../../../examples"

// stalenessGradeSatisfies is the per-surface satisfying grade set: a grade
// this transport can assign without an observation MUST NOT be a member.
// not_inducible and declared_gap are excluded as catalog- or
// operator-asserted, never observed.
var stalenessGradeSatisfies = []qualification.Grade{qualification.GradeUsable, qualification.GradeGap, qualification.GradeCorroborationOnly}

func stalenessProfilePaths(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(stalenessProfilesDir)
	if err != nil {
		t.Fatalf("read %s: %v", stalenessProfilesDir, err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(stalenessProfilesDir, entry.Name()))
	}
	if len(paths) == 0 {
		t.Fatalf("%s yielded no profile, want at least one", stalenessProfilesDir)
	}
	return paths
}

// measuredSurfaceProblems reports every problem the offline gate finds in
// measurement against profile: the protocol surface must carry a grade row
// satisfying stalenessGradeSatisfies, since that is the only surface this
// transport's inducers grade, and every other measured surface must carry at
// least one grade row. Pure, so a negative control can drive it without
// failing the package run.
func measuredSurfaceProblems(profile qualification.RuntimeProfile, measurement qualification.Measurement) []string {
	var problems []string
	for _, surface := range profile.MeasuredSurfaces() {
		var seen []qualification.Grade
		for _, grade := range measurement.Expectation.Grades {
			if grade.Surface == surface {
				seen = append(seen, grade.Grade)
			}
		}
		if surface != qualification.SurfaceProtocol {
			if len(seen) == 0 {
				problems = append(problems, fmt.Sprintf("surface %s carries no grade row", surface))
			}
			continue
		}
		satisfied := false
		for _, grade := range seen {
			if slices.Contains(stalenessGradeSatisfies, grade) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			problems = append(problems, fmt.Sprintf("surface %s carries no grade row satisfying %v, only %v", surface, stalenessGradeSatisfies, seen))
		}
	}
	return problems
}

// measuredSurfaceProblemsProfile fixes MeasuredSurfaces to a known, closed
// set so the surface walk under test has no other input.
func measuredSurfaceProblemsProfile() qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol:         {},
			qualification.SurfaceNativeJSON:       {},
			qualification.SurfaceNativeStreamJSON: {},
		},
	}
}

func TestMeasuredSurfaceProblems(t *testing.T) {
	t.Parallel()

	profile := measuredSurfaceProblemsProfile()

	t.Run("a gap protocol row and not_observed native rows report no problem", func(t *testing.T) {
		t.Parallel()

		measurement := qualification.Measurement{
			Expectation: qualification.NotesExpectation{
				Grades: []qualification.NotesGrade{
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
					{Surface: qualification.SurfaceNativeJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
				},
			},
		}

		if problems := measuredSurfaceProblems(profile, measurement); len(problems) != 0 {
			t.Errorf("measuredSurfaceProblems(...) = %v, want none", problems)
		}
	})

	t.Run("every protocol row not_observed reports a problem naming protocol", func(t *testing.T) {
		t.Parallel()

		measurement := qualification.Measurement{
			Expectation: qualification.NotesExpectation{
				Grades: []qualification.NotesGrade{
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityRetryClassification, Grade: qualification.GradeNotObserved},
					{Surface: qualification.SurfaceNativeJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
				},
			},
		}

		problems := measuredSurfaceProblems(profile, measurement)
		if len(problems) != 1 {
			t.Fatalf("measuredSurfaceProblems(...) = %v, want exactly one problem", problems)
		}
		if !strings.Contains(problems[0], string(qualification.SurfaceProtocol)) {
			t.Errorf("measuredSurfaceProblems(...) = %q, want it to name %q", problems[0], qualification.SurfaceProtocol)
		}
	})

	t.Run("a measured native surface with no grade row reports a problem naming it", func(t *testing.T) {
		t.Parallel()

		measurement := qualification.Measurement{
			Expectation: qualification.NotesExpectation{
				Grades: []qualification.NotesGrade{
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeUsable},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeNotObserved},
				},
			},
		}

		problems := measuredSurfaceProblems(profile, measurement)
		if len(problems) != 1 {
			t.Fatalf("measuredSurfaceProblems(...) = %v, want exactly one problem", problems)
		}
		if !strings.Contains(problems[0], string(qualification.SurfaceNativeJSON)) {
			t.Errorf("measuredSurfaceProblems(...) = %q, want it to name %q", problems[0], qualification.SurfaceNativeJSON)
		}
	})
}

// checkPublishedSampleOwnership fails t unless exactly one profile
// names sampleRelPath in its own published_sample member.
func checkPublishedSampleOwnership(t *testing.T, sampleRelPath string, profiles map[string]qualification.RuntimeProfile) {
	t.Helper()
	var owners []string
	for profilePath, profile := range profiles {
		if profile.PublishedSample == sampleRelPath {
			owners = append(owners, profilePath)
		}
	}
	if len(owners) != 1 {
		t.Errorf("published sample %s is named by %d profiles, want exactly 1: %v", sampleRelPath, len(owners), owners)
	}
}

type workflowAgentFrontMatter struct {
	Agent struct {
		Kind string `yaml:"kind"`
	} `yaml:"agent"`
}

func isAgentClientProtocolSample(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a tracked example under the repository's own examples directory
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rest, found := strings.CutPrefix(content, "---\n")
	if !found {
		return false
	}
	frontMatter, _, found := strings.Cut(rest, "\n---")
	if !found {
		return false
	}
	var parsed workflowAgentFrontMatter
	if err := yaml.Unmarshal([]byte(frontMatter), &parsed); err != nil {
		t.Fatalf("decode front matter %s: %v", path, err)
	}
	return parsed.Agent.Kind == "agent-client-protocol"
}

// TestStaleness is the offline staleness gate: editing any tracked artifact
// without a fresh measurement keeps this test red.
func TestStaleness(t *testing.T) {
	profiles := map[string]qualification.RuntimeProfile{}

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Errorf("%s: decode: %v", profilePath, err)
			continue
		}
		profiles[profilePath] = profile

		root, err := qualification.RepositoryRootFromWD()
		if err != nil {
			t.Fatalf("resolve repository root: %v", err)
		}
		notes, err := os.ReadFile(filepath.Join(root, profile.NotesPath)) //nolint:gosec // a repository-relative path resolved from the tracked profile
		if err != nil {
			t.Errorf("%s: read notes %s: %v", profilePath, profile.NotesPath, err)
			continue
		}
		measurement, err := qualification.ReadMeasurementFile(filepath.Join(root, profile.MeasurementPath))
		if err != nil {
			t.Errorf("%s: read measurement %s: %v", profilePath, profile.MeasurementPath, err)
			continue
		}

		if measurement.ProfileDigest != profile.Digest() {
			t.Errorf("%s: measurement profile_digest %s does not match the profile's own digest %s", profilePath, measurement.ProfileDigest, profile.Digest())
		}
		if err := qualification.ValidateNotes(string(notes), measurement.Expectation); err != nil {
			t.Errorf("%s: notes %s disagree with the tracked measurement: %v", profilePath, profile.NotesPath, err)
		}
		if measurement.Expectation.Verdict == qualification.VerdictNotQualified {
			t.Errorf("%s: measurement verdict is not_qualified", profilePath)
		}

		for _, problem := range measuredSurfaceProblems(profile, measurement) {
			t.Errorf("%s: %s", profilePath, problem)
		}
	}

	sampleEntries, err := os.ReadDir(stalenessExamplesDir)
	if err != nil {
		t.Fatalf("read %s: %v", stalenessExamplesDir, err)
	}
	for _, entry := range sampleEntries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "WORKFLOW.") || !strings.HasSuffix(name, ".md") {
			continue
		}
		samplePath := filepath.Join(stalenessExamplesDir, name)
		if !isAgentClientProtocolSample(t, samplePath) {
			continue
		}
		sampleRelPath := filepath.ToSlash(filepath.Join("examples", name))
		checkPublishedSampleOwnership(t, sampleRelPath, profiles)
	}
}
