package probe

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"

	"gopkg.in/yaml.v3"
)

// stalenessProfilesDir is the tracked profiles directory, relative to
// this package. It is enumerated through a filesystem read rather than
// through git grep, per the risk that a git grep-based enumeration
// passes vacuously on an untracked new artifact.
const stalenessProfilesDir = "../profiles"

// stalenessExamplesDir is where the published WORKFLOW.*.md samples
// live, relative to this package.
const stalenessExamplesDir = "../../../examples"

// stalenessGradeSatisfies is the per-surface satisfying grade set: a
// grade this transport can assign without an observation MUST NOT be a
// member. gap is excluded because this transport sets token_ceiling to
// gap at record construction for every runtime; not_inducible and
// declared_gap are excluded because they are catalog- or
// operator-asserted, never observed.
var stalenessGradeSatisfies = []qualification.Grade{qualification.GradeUsable, qualification.GradeCorroborationOnly}

// stalenessProfilePaths enumerates the tracked profiles directory
// through a filesystem read, failing t when it yields none.
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

// checkProfileMeasured fails t unless every surface in
// profile.MeasuredSurfaces() carries at least one grade row satisfying
// stalenessGradeSatisfies in measurement's expectation grades.
func checkProfileMeasured(t *testing.T, profilePath string, profile qualification.RuntimeProfile, measurement qualification.Measurement) {
	t.Helper()
	for _, surface := range profile.MeasuredSurfaces() {
		satisfied := false
		var seen []qualification.Grade
		for _, grade := range measurement.Expectation.Grades {
			if grade.Surface != surface {
				continue
			}
			seen = append(seen, grade.Grade)
			if slices.Contains(stalenessGradeSatisfies, grade.Grade) {
				satisfied = true
			}
		}
		if !satisfied {
			t.Errorf("%s: surface %s carries no grade row satisfying %v, only %v", profilePath, surface, stalenessGradeSatisfies, seen)
		}
	}
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

// workflowAgentFrontMatter is the subset of WORKFLOW.md front matter
// the staleness gate needs to identify an agent-client-protocol
// sample.
type workflowAgentFrontMatter struct {
	Agent struct {
		Kind string `yaml:"kind"`
	} `yaml:"agent"`
}

// isAgentClientProtocolSample reports whether the WORKFLOW.md at path
// declares agent.kind: agent-client-protocol.
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

// TestStaleness implements the offline staleness gate: every tracked
// runtime profile decodes cleanly, its notes and measurement artifact
// are readable and agree with it, its verdict is not not_qualified,
// every surface it measures carries at least one observed grade row,
// and every published agent-client-protocol WORKFLOW.*.md sample is
// named by exactly one profile's published_sample. It runs in every
// ordinary go test pass and costs nothing: editing a tracked profile,
// measurement, notes document, or published route sample without a
// fresh, digest-bound measurement keeps this test red.
func TestStaleness(t *testing.T) {
	profiles := map[string]qualification.RuntimeProfile{}

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Errorf("%s: decode: %v", profilePath, err)
			continue
		}
		profiles[profilePath] = profile

		root, err := repositoryRootFromWD()
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

		checkProfileMeasured(t, profilePath, profile, measurement)
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
