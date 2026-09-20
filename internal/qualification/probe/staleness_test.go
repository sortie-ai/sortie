package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

	t.Run("a gap row on every measured surface reports no problem", func(t *testing.T) {
		t.Parallel()

		measurement := qualification.Measurement{
			Expectation: qualification.NotesExpectation{
				Grades: []qualification.NotesGrade{
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
					{Surface: qualification.SurfaceNativeJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
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
					{Surface: qualification.SurfaceNativeJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeGap},
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

	t.Run("a measured native surface with no satisfying grade row reports a problem naming it", func(t *testing.T) {
		t.Parallel()

		measurement := qualification.Measurement{
			Expectation: qualification.NotesExpectation{
				Grades: []qualification.NotesGrade{
					{Surface: qualification.SurfaceProtocol, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeUsable},
					{Surface: qualification.SurfaceNativeStreamJSON, Capability: qualification.CapabilityTurnDisposition, Grade: qualification.GradeUsable},
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

const stalenessProvenanceFile = "provenance.json"

// provenanceLink is the part of a provenance artifact the offline gate reads.
type provenanceLink struct {
	ProfileDigest  string `json:"profile_digest"`
	EvidenceFile   string `json:"evidence_file"`
	EvidenceDigest string `json:"evidence_digest"`
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// regradedExpectation grades the evidence at path through the same reader,
// validator, and summary a live collection publishes from, so the result is
// what replaying those bytes under today's grading code yields.
func regradedExpectation(path string, profile qualification.RuntimeProfile) (qualification.NotesExpectation, error) {
	records, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		return qualification.NotesExpectation{}, err
	}
	verdict, err := qualification.ValidateObservationsWithDeclarations(path, profile)
	if err != nil {
		return qualification.NotesExpectation{}, err
	}
	conclusions, err := ConclusionsFromRecords(records, verdict, profile)
	if err != nil {
		return qualification.NotesExpectation{}, err
	}
	return ExpectationFrom(conclusions), nil
}

// measurementChainProblems reports every break in the chain tying a tracked
// measurement to the observations behind it, ending by re-grading the evidence
// to reproduce the measurement's expectation. That last link is what two
// documents stale in the same direction cannot satisfy: agreement between
// transcriptions is not agreement with what was observed.
func measurementChainProblems(dir string, profile qualification.RuntimeProfile, measurement qualification.Measurement) []string {
	provenancePath := filepath.Join(dir, stalenessProvenanceFile)
	raw, err := os.ReadFile(provenancePath)
	if err != nil {
		return []string{fmt.Sprintf("read the provenance beside the measurement: %v", err)}
	}
	var provenance provenanceLink
	if err := json.Unmarshal(raw, &provenance); err != nil {
		return []string{fmt.Sprintf("decode %s: %v", provenancePath, err)}
	}

	var problems []string
	sum := sha256.Sum256(raw)
	switch digest := hex.EncodeToString(sum[:]); {
	case measurement.ProvenanceDigest == nil:
		problems = append(problems, "the measurement names no provenance digest, so nothing ties it to the run that produced it")
	case *measurement.ProvenanceDigest != digest:
		problems = append(problems, fmt.Sprintf("%s hashes to %s, and the measurement names provenance %s", provenancePath, digest, *measurement.ProvenanceDigest))
	}
	if provenance.ProfileDigest != profile.Digest() {
		problems = append(problems, fmt.Sprintf("%s states profile digest %s, and the profile's own digest is %s", provenancePath, provenance.ProfileDigest, profile.Digest()))
	}

	evidencePath := filepath.Join(dir, filepath.Base(provenance.EvidenceFile))
	digest, err := fileSHA256(evidencePath)
	if err != nil {
		return append(problems, fmt.Sprintf("read the evidence the provenance names: %v", err))
	}
	if digest != provenance.EvidenceDigest {
		problems = append(problems, fmt.Sprintf("%s hashes to %s, and the provenance names evidence %s", evidencePath, digest, provenance.EvidenceDigest))
	}

	regraded, err := regradedExpectation(evidencePath, profile)
	if err != nil {
		return append(problems, fmt.Sprintf("re-grade %s: %v", evidencePath, err))
	}
	return append(problems, expectationProblems(measurement.Expectation, regraded)...)
}

func expectationProblems(stated, regraded qualification.NotesExpectation) []string {
	var problems []string
	if stated.Verdict != regraded.Verdict {
		problems = append(problems, fmt.Sprintf("the measurement states eligibility %s, and re-grading the evidence yields %s", stated.Verdict, regraded.Verdict))
	}
	if !qualification.NullableEqual(stated.Conformance, regraded.Conformance) {
		problems = append(problems, fmt.Sprintf("the measurement states product conformance %s, and re-grading the evidence yields %s", verdictText(stated.Conformance), verdictText(regraded.Conformance)))
	}
	problems = append(problems, gradeProblems(stated.Grades, regraded.Grades)...)
	problems = append(problems, listProblems("excluded case", stated.Excluded, regraded.Excluded)...)
	return append(problems, listProblems("unobserved row", stated.Unobserved, regraded.Unobserved)...)
}

func verdictText(verdict *qualification.Verdict) string {
	if verdict == nil {
		return "none"
	}
	return string(*verdict)
}

func gradeProblems(stated, regraded []qualification.NotesGrade) []string {
	remaining := make(map[string]qualification.NotesGrade, len(stated))
	for _, grade := range stated {
		remaining[gradeRow(grade)] = grade
	}
	var problems []string
	for _, grade := range regraded {
		row := gradeRow(grade)
		was, carried := remaining[row]
		delete(remaining, row)
		switch {
		case !carried:
			problems = append(problems, fmt.Sprintf("the measurement carries no %s row, and re-grading the evidence yields %s", row, grade.Grade))
		case was.Grade != grade.Grade || was.Label != grade.Label:
			problems = append(problems, fmt.Sprintf("the measurement states %s %s (%q), and re-grading the evidence yields %s (%q)", row, was.Grade, was.Label, grade.Grade, grade.Label))
		}
	}
	for _, row := range slices.Sorted(maps.Keys(remaining)) {
		problems = append(problems, fmt.Sprintf("the measurement states %s %s, and re-grading the evidence yields no such row", row, remaining[row].Grade))
	}
	return problems
}

func gradeRow(grade qualification.NotesGrade) string {
	return fmt.Sprintf("%s %s", grade.Surface, grade.Capability)
}

func listProblems(kind string, stated, regraded []string) []string {
	var problems []string
	for _, entry := range regraded {
		if !slices.Contains(stated, entry) {
			problems = append(problems, fmt.Sprintf("re-grading the evidence yields %s %q, which the measurement does not state", kind, entry))
		}
	}
	for _, entry := range stated {
		if !slices.Contains(regraded, entry) {
			problems = append(problems, fmt.Sprintf("the measurement states %s %q, which re-grading the evidence does not yield", kind, entry))
		}
	}
	return problems
}

// The controls below drive measurementChainProblems, never this runtime's own
// result, so any tracked runtime's artifacts serve.
const (
	chainFixtureProfile  = "../profiles/gemini-cli.json"
	chainFixtureTestdata = "testdata/gemini-cli"
	chainFixtureEvidence = "evidence.jsonl"
)

// stageChain copies one runtime's tracked provenance and evidence into a
// temporary directory and returns it, its profile, and a measurement whose
// chain is intact for a control to break exactly one link of.
func stageChain(t *testing.T) (string, qualification.RuntimeProfile, qualification.Measurement) {
	t.Helper()

	profile, err := qualification.ReadRuntimeProfileFile(chainFixtureProfile)
	if err != nil {
		t.Fatalf("ReadRuntimeProfileFile(%q) error = %v, want nil", chainFixtureProfile, err)
	}
	dir := t.TempDir()
	for _, name := range []string{stalenessProvenanceFile, chainFixtureEvidence} {
		content, readErr := os.ReadFile(filepath.Join(chainFixtureTestdata, name))
		if readErr != nil {
			t.Fatalf("read the tracked %s: %v", name, readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dir, name), content, 0o600); writeErr != nil {
			t.Fatalf("stage %s: %v", name, writeErr)
		}
	}

	expectation, err := regradedExpectation(filepath.Join(dir, chainFixtureEvidence), profile)
	if err != nil {
		t.Fatalf("regradedExpectation(%s) error = %v, want nil", chainFixtureEvidence, err)
	}
	digest, err := fileSHA256(filepath.Join(dir, stalenessProvenanceFile))
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v, want nil", stalenessProvenanceFile, err)
	}
	return dir, profile, qualification.Measurement{Expectation: expectation, ProvenanceDigest: &digest}
}

// rewriteProvenance applies edit to the staged provenance and re-points the
// measurement at the rewritten bytes, isolating the link it means to break
// from the digest that would otherwise also report it.
func rewriteProvenance(t *testing.T, dir string, measurement *qualification.Measurement, edit func(map[string]json.RawMessage)) {
	t.Helper()

	path := filepath.Join(dir, stalenessProvenanceFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the staged provenance: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode the staged provenance: %v", err)
	}
	edit(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the rewritten provenance: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write the rewritten provenance: %v", err)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v, want nil", path, err)
	}
	measurement.ProvenanceDigest = &digest
}

func TestMeasurementChainProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// breakLink edits one link and returns the phrase the one reported
		// problem must carry; nil leaves the chain intact and expects none.
		breakLink func(t *testing.T, dir string, measurement *qualification.Measurement) string
	}{
		{name: "an intact chain reports no problem"},
		{
			name: "a measurement stating a conformance answer the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *qualification.Measurement) string {
				stale := qualification.VerdictNotQualified
				if measurement.Expectation.Conformance != nil && *measurement.Expectation.Conformance == stale {
					stale = qualification.VerdictQualified
				}
				measurement.Expectation.Conformance = &stale
				return "product conformance " + string(stale)
			},
		},
		{
			name: "a measurement grade row edited away from the evidence is reported",
			breakLink: func(t *testing.T, _ string, measurement *qualification.Measurement) string {
				t.Helper()
				if len(measurement.Expectation.Grades) == 0 {
					t.Fatal("the staged measurement carries no grade row to edit")
				}
				row := &measurement.Expectation.Grades[0]
				replacement := qualification.GradeGap
				if row.Grade == replacement {
					replacement = qualification.GradeCorroborationOnly
				}
				row.Grade = replacement
				return fmt.Sprintf("states %s %s", gradeRow(*row), replacement)
			},
		},
		{
			name: "a measurement stating an eligibility verdict the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *qualification.Measurement) string {
				stale := qualification.VerdictQualified
				if measurement.Expectation.Verdict == stale {
					stale = qualification.VerdictUnmeasured
				}
				measurement.Expectation.Verdict = stale
				return "eligibility " + string(stale)
			},
		},
		{
			name: "a measurement stating an excluded case the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *qualification.Measurement) string {
				const invented = "turn_disposition success: excluded by nobody"
				measurement.Expectation.Excluded = append(measurement.Expectation.Excluded, invented)
				return "states excluded case " + strconv.Quote(invented)
			},
		},
		{
			name: "a measurement dropping an unobserved row the evidence yields is reported",
			breakLink: func(t *testing.T, _ string, measurement *qualification.Measurement) string {
				t.Helper()
				if len(measurement.Expectation.Unobserved) == 0 {
					t.Fatal("the staged measurement carries no unobserved row to drop")
				}
				dropped := measurement.Expectation.Unobserved[0]
				measurement.Expectation.Unobserved = measurement.Expectation.Unobserved[1:]
				return "yields unobserved row " + strconv.Quote(dropped)
			},
		},
		{
			name: "evidence edited under a provenance that still names the old digest is reported",
			breakLink: func(t *testing.T, dir string, _ *qualification.Measurement) string {
				t.Helper()
				path := filepath.Join(dir, chainFixtureEvidence)
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read the staged evidence: %v", err)
				}
				if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
					t.Fatalf("edit the staged evidence: %v", err)
				}
				return "the provenance names evidence"
			},
		},
		{
			name: "provenance edited under a measurement that still names the old digest is reported",
			breakLink: func(t *testing.T, dir string, _ *qualification.Measurement) string {
				t.Helper()
				path := filepath.Join(dir, stalenessProvenanceFile)
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read the staged provenance: %v", err)
				}
				if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
					t.Fatalf("edit the staged provenance: %v", err)
				}
				return "the measurement names provenance"
			},
		},
		{
			name: "provenance naming another profile is reported",
			breakLink: func(t *testing.T, dir string, measurement *qualification.Measurement) string {
				t.Helper()
				rewriteProvenance(t, dir, measurement, func(document map[string]json.RawMessage) {
					document["profile_digest"] = json.RawMessage(`"a-digest-of-another-profile"`)
				})
				return "states profile digest a-digest-of-another-profile"
			},
		},
		{
			name: "provenance naming evidence that is not beside it is reported",
			breakLink: func(t *testing.T, dir string, measurement *qualification.Measurement) string {
				t.Helper()
				rewriteProvenance(t, dir, measurement, func(document map[string]json.RawMessage) {
					document["evidence_file"] = json.RawMessage(`"absent-evidence.jsonl"`)
				})
				return "read the evidence the provenance names"
			},
		},
		{
			name: "a measurement naming no provenance digest is reported",
			breakLink: func(_ *testing.T, _ string, measurement *qualification.Measurement) string {
				measurement.ProvenanceDigest = nil
				return "names no provenance digest"
			},
		},
		{
			name: "a measurement with no provenance beside it is reported",
			breakLink: func(t *testing.T, dir string, _ *qualification.Measurement) string {
				t.Helper()
				if err := os.Remove(filepath.Join(dir, stalenessProvenanceFile)); err != nil {
					t.Fatalf("remove the staged provenance: %v", err)
				}
				return "read the provenance beside the measurement"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir, profile, measurement := stageChain(t)
			if tt.breakLink == nil {
				if problems := measurementChainProblems(dir, profile, measurement); len(problems) != 0 {
					t.Errorf("measurementChainProblems(...) = %v, want none", problems)
				}
				return
			}

			want := tt.breakLink(t, dir, &measurement)
			problems := measurementChainProblems(dir, profile, measurement)
			if len(problems) != 1 {
				t.Fatalf("measurementChainProblems(...) = %v, want exactly one problem naming %q", problems, want)
			}
			if !strings.Contains(problems[0], want) {
				t.Errorf("measurementChainProblems(...) = %q, want it to name %q", problems[0], want)
			}
		})
	}
}

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
		measurementPath := filepath.Join(root, profile.MeasurementPath)
		measurement, err := qualification.ReadMeasurementFile(measurementPath)
		if err != nil {
			t.Errorf("%s: read measurement %s: %v", profilePath, profile.MeasurementPath, err)
			continue
		}

		for _, problem := range measurementChainProblems(filepath.Dir(measurementPath), profile, measurement) {
			t.Errorf("%s: %s", profilePath, problem)
		}
		if measurement.ProfileDigest != profile.Digest() {
			t.Errorf("%s: measurement profile_digest %s does not match the profile's own digest %s", profilePath, measurement.ProfileDigest, profile.Digest())
		}
		if err := qualification.ValidateNotes(string(notes), measurement.Expectation); err != nil {
			t.Errorf("%s: notes %s disagree with the tracked measurement: %v", profilePath, profile.NotesPath, err)
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
