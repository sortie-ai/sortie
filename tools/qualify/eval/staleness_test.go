package eval

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

	"gopkg.in/yaml.v3"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// captureDirEnvVar, when set to an unreadable capture directory, MUST fail
// TestStaleness rather than skip it, so a typo cannot pass green.
const captureDirEnvVar = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_CAPTURE_DIR"

// Enumerated through a filesystem read rather than git grep, which would pass
// vacuously on an untracked new artifact.
const stalenessProfilesDir = "../profiles"

const stalenessExamplesDir = "../../../examples"

// stalenessGradeSatisfies is the per-surface satisfying grade set: a grade
// this transport can assign without an observation MUST NOT be a member.
// not_inducible and declared_gap are excluded as catalog- or
// operator-asserted, never observed.
var stalenessGradeSatisfies = []evidence.Grade{evidence.GradeUsable, evidence.GradeGap, evidence.GradeCorroborationOnly}

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

// measuredSurfaceProblems is pure, so a negative control can drive it
// without failing the package run.
func measuredSurfaceProblems(p profile.RuntimeProfile, measurement evidence.Measurement) []string {
	var problems []string
	for _, surface := range p.MeasuredSurfaces() {
		var seen []evidence.Grade
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
func measuredSurfaceProblemsProfile() profile.RuntimeProfile {
	return profile.RuntimeProfile{
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol:         {},
			evidence.SurfaceNativeJSON:       {},
			evidence.SurfaceNativeStreamJSON: {},
		},
	}
}

func TestMeasuredSurfaceProblems(t *testing.T) {
	t.Parallel()

	p := measuredSurfaceProblemsProfile()

	t.Run("a gap row on every measured surface reports no problem", func(t *testing.T) {
		t.Parallel()

		measurement := evidence.Measurement{
			Expectation: evidence.NotesExpectation{
				Grades: []evidence.NotesGrade{
					{Surface: evidence.SurfaceProtocol, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeGap},
					{Surface: evidence.SurfaceNativeJSON, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeGap},
					{Surface: evidence.SurfaceNativeStreamJSON, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeGap},
				},
			},
		}

		if problems := measuredSurfaceProblems(p, measurement); len(problems) != 0 {
			t.Errorf("measuredSurfaceProblems(...) = %v, want none", problems)
		}
	})

	t.Run("every protocol row not_observed reports a problem naming protocol", func(t *testing.T) {
		t.Parallel()

		measurement := evidence.Measurement{
			Expectation: evidence.NotesExpectation{
				Grades: []evidence.NotesGrade{
					{Surface: evidence.SurfaceProtocol, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeNotObserved},
					{Surface: evidence.SurfaceProtocol, Capability: evidence.CapabilityRetryClassification, Grade: evidence.GradeNotObserved},
					{Surface: evidence.SurfaceNativeJSON, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeGap},
					{Surface: evidence.SurfaceNativeStreamJSON, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeGap},
				},
			},
		}

		problems := measuredSurfaceProblems(p, measurement)
		if len(problems) != 1 {
			t.Fatalf("measuredSurfaceProblems(...) = %v, want exactly one problem", problems)
		}
		if !strings.Contains(problems[0], string(evidence.SurfaceProtocol)) {
			t.Errorf("measuredSurfaceProblems(...) = %q, want it to name %q", problems[0], evidence.SurfaceProtocol)
		}
	})

	t.Run("a measured native surface with no satisfying grade row reports a problem naming it", func(t *testing.T) {
		t.Parallel()

		measurement := evidence.Measurement{
			Expectation: evidence.NotesExpectation{
				Grades: []evidence.NotesGrade{
					{Surface: evidence.SurfaceProtocol, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeUsable},
					{Surface: evidence.SurfaceNativeStreamJSON, Capability: evidence.CapabilityTurnDisposition, Grade: evidence.GradeUsable},
				},
			},
		}

		problems := measuredSurfaceProblems(p, measurement)
		if len(problems) != 1 {
			t.Fatalf("measuredSurfaceProblems(...) = %v, want exactly one problem", problems)
		}
		if !strings.Contains(problems[0], string(evidence.SurfaceNativeJSON)) {
			t.Errorf("measuredSurfaceProblems(...) = %q, want it to name %q", problems[0], evidence.SurfaceNativeJSON)
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

func stalenessFileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // a capture directory this test staged or a tracked testdata path
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// regradedExpectation grades the capture at dir through Run, the same
// re-derivation an offline report drives, so the result is what replaying
// those bytes under today's grading code yields.
func regradedExpectation(dir string, p profile.RuntimeProfile) (evidence.NotesExpectation, error) {
	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		return evidence.NotesExpectation{}, err
	}
	return expectationFrom(result.Conclusions), nil
}

// measurementChainProblems ends by re-grading the evidence: agreement
// between two stale transcriptions is not agreement with what was observed.
func measurementChainProblems(dir string, p profile.RuntimeProfile, measurement evidence.Measurement) []string {
	provenancePath := filepath.Join(dir, stalenessProvenanceFile)
	raw, err := os.ReadFile(provenancePath) //nolint:gosec // a capture directory this test staged or a tracked testdata path
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
	if provenance.ProfileDigest != p.Digest() {
		problems = append(problems, fmt.Sprintf("%s states profile digest %s, and the profile's own digest is %s", provenancePath, provenance.ProfileDigest, p.Digest()))
	}

	evidencePath := filepath.Join(dir, filepath.Base(provenance.EvidenceFile))
	digest, err := stalenessFileSHA256(evidencePath)
	if err != nil {
		return append(problems, fmt.Sprintf("read the evidence the provenance names: %v", err))
	}
	if digest != provenance.EvidenceDigest {
		problems = append(problems, fmt.Sprintf("%s hashes to %s, and the provenance names evidence %s", evidencePath, digest, provenance.EvidenceDigest))
	}

	regraded, err := regradedExpectation(dir, p)
	if err != nil {
		return append(problems, fmt.Sprintf("re-grade %s: %v", evidencePath, err))
	}
	return append(problems, expectationProblems(measurement.Expectation, regraded)...)
}

func expectationProblems(stated, regraded evidence.NotesExpectation) []string {
	var problems []string
	if stated.Verdict != regraded.Verdict {
		problems = append(problems, fmt.Sprintf("the measurement states eligibility %s, and re-grading the evidence yields %s", stated.Verdict, regraded.Verdict))
	}
	if !evidence.NullableEqual(stated.Conformance, regraded.Conformance) {
		problems = append(problems, fmt.Sprintf("the measurement states product conformance %s, and re-grading the evidence yields %s", verdictText(stated.Conformance), verdictText(regraded.Conformance)))
	}
	problems = append(problems, gradeProblems(stated.Grades, regraded.Grades)...)
	problems = append(problems, listProblems("excluded case", stated.Excluded, regraded.Excluded)...)
	return append(problems, listProblems("unobserved row", stated.Unobserved, regraded.Unobserved)...)
}

func verdictText(verdict *evidence.Verdict) string {
	if verdict == nil {
		return "none"
	}
	return string(*verdict)
}

func gradeProblems(stated, regraded []evidence.NotesGrade) []string {
	remaining := make(map[string]evidence.NotesGrade, len(stated))
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

func gradeRow(grade evidence.NotesGrade) string {
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
	chainFixtureTestdata = "../probe/testdata/gemini-cli"
	chainFixtureEvidence = "evidence.jsonl"
)

// stageChain copies one runtime's tracked provenance and evidence into a
// temporary directory and returns it, its profile, and a measurement whose
// chain is intact for a control to break exactly one link of.
func stageChain(t *testing.T) (string, profile.RuntimeProfile, evidence.Measurement) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, chainFixtureProfile)
	if err != nil {
		t.Fatalf("Load(%q, %q) error = %v, want nil", root, chainFixtureProfile, err)
	}
	dir := t.TempDir()
	for _, name := range []string{stalenessProvenanceFile, chainFixtureEvidence} {
		content, readErr := os.ReadFile(filepath.Join(chainFixtureTestdata, name)) //nolint:gosec // a tracked testdata path
		if readErr != nil {
			t.Fatalf("read the tracked %s: %v", name, readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dir, name), content, 0o600); writeErr != nil {
			t.Fatalf("stage %s: %v", name, writeErr)
		}
	}

	expectation, err := regradedExpectation(dir, p)
	if err != nil {
		t.Fatalf("regradedExpectation(%s) error = %v, want nil", dir, err)
	}
	digest, err := stalenessFileSHA256(filepath.Join(dir, stalenessProvenanceFile))
	if err != nil {
		t.Fatalf("stalenessFileSHA256(%s) error = %v, want nil", stalenessProvenanceFile, err)
	}
	return dir, p, evidence.Measurement{Expectation: expectation, ProvenanceDigest: &digest}
}

// rewriteProvenance applies edit to the staged provenance and re-points the
// measurement at the rewritten bytes, isolating the link it means to break
// from the digest that would otherwise also report it.
func rewriteProvenance(t *testing.T, dir string, measurement *evidence.Measurement, edit func(map[string]json.RawMessage)) {
	t.Helper()

	path := filepath.Join(dir, stalenessProvenanceFile)
	content, err := os.ReadFile(path) //nolint:gosec // this test's own staged capture directory
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
	digest, err := stalenessFileSHA256(path)
	if err != nil {
		t.Fatalf("stalenessFileSHA256(%s) error = %v, want nil", path, err)
	}
	measurement.ProvenanceDigest = &digest
}

// recomputeBaselineAfterFlip rewrites surface's capability baseline record in
// records to what checkDerivedBaselines requires, once one of its case
// records has been rewritten in place.
func recomputeBaselineAfterFlip(t *testing.T, records []evidence.Record, surface evidence.Surface, capability evidence.Capability) {
	t.Helper()

	var classes []evidence.Grade
	var contributing []evidence.Outcome
	for _, caseID := range evidence.CapabilityCases[capability] {
		for i := range records {
			rec := &records[i]
			if rec.Scenario != evidence.ScenarioSemanticProbe || rec.Surface != surface || rec.Capability != capability ||
				rec.SemanticCase == nil || *rec.SemanticCase != caseID {
				continue
			}
			classes = append(classes, evidence.BaselineClassification(rec.Grade, rec.Detail))
			contributing = append(contributing, rec.Outcome)
			break
		}
	}
	grade := evidence.DeriveBaselineGrade(classes)
	outcome := evidence.DeriveBaselineOutcome(grade, contributing)

	for i := range records {
		if evidence.MatchBaseline(surface, capability)(&records[i]) {
			records[i].Grade = grade
			records[i].Outcome = outcome
			return
		}
	}
	t.Fatalf("no %s %s baseline record to recompute", surface, capability)
}

// flipFirstUsableSemanticRecordToNotObserved rewrites, in the staged evidence
// file at dir, a usable semantic record to its not-observed shape and
// re-points measurement at the rewritten provenance, so a control can force a
// fresh unobserved row without depending on one already present in the
// tracked corpus. The candidate is chosen so its protocol session, if any,
// keeps another non-final record referencing it after the flip; picking the
// session's only reference would orphan its runtime identity record and fail
// checkIdentityCoverage instead of exercising the drop this test pins.
func flipFirstUsableSemanticRecordToNotObserved(t *testing.T, dir string, measurement *evidence.Measurement) {
	t.Helper()

	evidencePath := filepath.Join(dir, chainFixtureEvidence)
	records, err := evidence.ReadEvidenceFile(evidencePath)
	if err != nil {
		t.Fatalf("read the staged evidence: %v", err)
	}

	sessionRefs := map[string]int{}
	for _, rec := range records {
		if rec.Scenario != evidence.ScenarioRuntimeIdentity && rec.Surface == evidence.SurfaceProtocol && rec.SessionID != nil {
			sessionRefs[*rec.SessionID]++
		}
	}

	target := -1
	for i, rec := range records {
		if rec.Scenario != evidence.ScenarioSemanticProbe || rec.Grade != evidence.GradeUsable {
			continue
		}
		if rec.SessionID != nil && sessionRefs[*rec.SessionID] < 2 {
			continue
		}
		target = i
		break
	}
	if target < 0 {
		t.Fatal("the staged evidence carries no usable semantic record whose session survives the flip")
	}
	surface, capability := records[target].Surface, records[target].Capability
	records[target].Grade = evidence.GradeNotObserved
	records[target].Outcome = evidence.OutcomeFixtureInductionFailed
	records[target].SessionID = nil
	records[target].EvidencePath = nil
	records[target].Detail = "flipped for the dropped-unobserved-row control"

	recomputeBaselineAfterFlip(t, records, surface, capability)

	lines := make([]string, len(records))
	for i, rec := range records {
		line, err := evidence.MarshalRecord(rec)
		if err != nil {
			t.Fatalf("marshal the flipped evidence record: %v", err)
		}
		lines[i] = string(line)
	}
	if err := os.WriteFile(evidencePath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the flipped evidence: %v", err)
	}

	digest, err := stalenessFileSHA256(evidencePath)
	if err != nil {
		t.Fatalf("stalenessFileSHA256(%s) error = %v, want nil", evidencePath, err)
	}
	rewriteProvenance(t, dir, measurement, func(document map[string]json.RawMessage) {
		document["evidence_digest"] = json.RawMessage(strconv.Quote(digest))
	})
}

func TestMeasurementChainProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// breakLink edits one link and returns the phrase the one reported
		// problem must carry; nil leaves the chain intact and expects none.
		breakLink func(t *testing.T, dir string, measurement *evidence.Measurement) string
	}{
		{name: "an intact chain reports no problem"},
		{
			name: "a measurement stating a conformance answer the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *evidence.Measurement) string {
				stale := evidence.VerdictNotQualified
				if measurement.Expectation.Conformance != nil && *measurement.Expectation.Conformance == stale {
					stale = evidence.VerdictQualified
				}
				measurement.Expectation.Conformance = &stale
				return "product conformance " + string(stale)
			},
		},
		{
			name: "a measurement grade row edited away from the evidence is reported",
			breakLink: func(t *testing.T, _ string, measurement *evidence.Measurement) string {
				t.Helper()
				if len(measurement.Expectation.Grades) == 0 {
					t.Fatal("the staged measurement carries no grade row to edit")
				}
				row := &measurement.Expectation.Grades[0]
				replacement := evidence.GradeGap
				if row.Grade == replacement {
					replacement = evidence.GradeCorroborationOnly
				}
				row.Grade = replacement
				return fmt.Sprintf("states %s %s", gradeRow(*row), replacement)
			},
		},
		{
			name: "a measurement stating an eligibility verdict the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *evidence.Measurement) string {
				stale := evidence.VerdictQualified
				if measurement.Expectation.Verdict == stale {
					stale = evidence.VerdictUnmeasured
				}
				measurement.Expectation.Verdict = stale
				return "eligibility " + string(stale)
			},
		},
		{
			name: "a measurement stating an excluded case the evidence does not yield is reported",
			breakLink: func(_ *testing.T, _ string, measurement *evidence.Measurement) string {
				const invented = "turn_disposition success: excluded by nobody"
				measurement.Expectation.Excluded = append(measurement.Expectation.Excluded, invented)
				return "states excluded case " + strconv.Quote(invented)
			},
		},
		{
			name: "a measurement dropping an unobserved row the evidence yields is reported",
			breakLink: func(t *testing.T, dir string, measurement *evidence.Measurement) string {
				t.Helper()
				root, err := profile.CheckoutRoot()
				if err != nil {
					t.Fatalf("CheckoutRoot() error = %v, want nil", err)
				}
				p, err := profile.Load(root, chainFixtureProfile)
				if err != nil {
					t.Fatalf("Load(%q, %q) error = %v, want nil", root, chainFixtureProfile, err)
				}

				before := measurement.Expectation.Unobserved
				flipFirstUsableSemanticRecordToNotObserved(t, dir, measurement)

				regraded, err := regradedExpectation(dir, p)
				if err != nil {
					t.Fatalf("regradedExpectation(%s) error = %v, want nil", dir, err)
				}
				var dropped string
				for _, entry := range regraded.Unobserved {
					if !slices.Contains(before, entry) {
						dropped = entry
						break
					}
				}
				if dropped == "" {
					t.Fatal("flipping a semantic record to not_observed yielded no new unobserved row")
				}

				measurement.Expectation = regraded
				measurement.Expectation.Unobserved = slices.DeleteFunc(slices.Clone(regraded.Unobserved), func(entry string) bool {
					return entry == dropped
				})
				return "yields unobserved row " + strconv.Quote(dropped)
			},
		},
		{
			name: "evidence edited under a provenance that still names the old digest is reported",
			breakLink: func(t *testing.T, dir string, _ *evidence.Measurement) string {
				t.Helper()
				path := filepath.Join(dir, chainFixtureEvidence)
				content, err := os.ReadFile(path) //nolint:gosec // this test's own staged capture directory
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
			breakLink: func(t *testing.T, dir string, _ *evidence.Measurement) string {
				t.Helper()
				path := filepath.Join(dir, stalenessProvenanceFile)
				content, err := os.ReadFile(path) //nolint:gosec // this test's own staged capture directory
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
			breakLink: func(t *testing.T, dir string, measurement *evidence.Measurement) string {
				t.Helper()
				rewriteProvenance(t, dir, measurement, func(document map[string]json.RawMessage) {
					document["profile_digest"] = json.RawMessage(`"a-digest-of-another-profile"`)
				})
				return "states profile digest a-digest-of-another-profile"
			},
		},
		{
			name: "provenance naming evidence that is not beside it is reported",
			breakLink: func(t *testing.T, dir string, measurement *evidence.Measurement) string {
				t.Helper()
				rewriteProvenance(t, dir, measurement, func(document map[string]json.RawMessage) {
					document["evidence_file"] = json.RawMessage(`"absent-evidence.jsonl"`)
				})
				return "read the evidence the provenance names"
			},
		},
		{
			name: "a measurement naming no provenance digest is reported",
			breakLink: func(_ *testing.T, _ string, measurement *evidence.Measurement) string {
				measurement.ProvenanceDigest = nil
				return "names no provenance digest"
			},
		},
		{
			name: "a measurement with no provenance beside it is reported",
			breakLink: func(t *testing.T, dir string, _ *evidence.Measurement) string {
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

			dir, p, measurement := stageChain(t)
			if tt.breakLink == nil {
				if problems := measurementChainProblems(dir, p, measurement); len(problems) != 0 {
					t.Errorf("measurementChainProblems(...) = %v, want none", problems)
				}
				return
			}

			want := tt.breakLink(t, dir, &measurement)
			problems := measurementChainProblems(dir, p, measurement)
			if len(problems) != 1 {
				t.Fatalf("measurementChainProblems(...) = %v, want exactly one problem naming %q", problems, want)
			}
			if !strings.Contains(problems[0], want) {
				t.Errorf("measurementChainProblems(...) = %q, want it to name %q", problems[0], want)
			}
		})
	}
}

func checkPublishedSampleOwnership(t *testing.T, sampleRelPath string, profiles map[string]profile.RuntimeProfile) {
	t.Helper()
	var owners []string
	for profilePath, p := range profiles {
		if p.PublishedSample == sampleRelPath {
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

// testStalenessCaptureDir validates exactly the capture directory named by
// captureDirEnvVar. It fails rather than skips on an unreadable directory, so
// a typo cannot pass green.
func testStalenessCaptureDir(t *testing.T, dir string) {
	t.Helper()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s=%q names an unreadable capture directory: %v", captureDirEnvVar, dir, err)
	}

	profilePath := os.Getenv(qualifyReemitProfileEnvVar)
	if profilePath == "" {
		profilePath = chainFixtureProfile
	}
	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, profilePath)
	if err != nil {
		t.Fatalf("Load(%q, %q) error = %v, want nil", root, profilePath, err)
	}

	if _, err := Run(Input{CaptureDir: dir, Profile: p}); err != nil {
		t.Fatalf("Run(CaptureDir: %q) error = %v, want nil", dir, err)
	}
}

// testStalenessTrackedCorpus is the offline staleness gate over every
// tracked profile and capture: editing any tracked artifact without a fresh
// measurement keeps this test red.
func testStalenessTrackedCorpus(t *testing.T) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}

	profiles := map[string]profile.RuntimeProfile{}

	for _, profilePath := range stalenessProfilePaths(t) {
		p, err := profile.Load(root, profilePath)
		if err != nil {
			t.Errorf("%s: decode: %v", profilePath, err)
			continue
		}
		profiles[profilePath] = p

		notes, err := os.ReadFile(filepath.Join(root, p.NotesPath)) //nolint:gosec // a repository-relative path resolved from the tracked profile
		if err != nil {
			t.Errorf("%s: read notes %s: %v", profilePath, p.NotesPath, err)
			continue
		}
		measurementPath := filepath.Join(root, p.MeasurementPath)
		measurement, err := evidence.ReadMeasurementFile(measurementPath)
		if err != nil {
			t.Errorf("%s: read measurement %s: %v", profilePath, p.MeasurementPath, err)
			continue
		}

		for _, problem := range measurementChainProblems(filepath.Dir(measurementPath), p, measurement) {
			t.Errorf("%s: %s", profilePath, problem)
		}
		if measurement.ProfileDigest != p.Digest() {
			t.Errorf("%s: measurement profile_digest %s does not match the profile's own digest %s", profilePath, measurement.ProfileDigest, p.Digest())
		}
		if err := evidence.ValidateNotes(string(notes), measurement.Expectation); err != nil {
			t.Errorf("%s: notes %s disagree with the tracked measurement: %v", profilePath, p.NotesPath, err)
		}

		for _, problem := range measuredSurfaceProblems(p, measurement) {
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

func TestStaleness(t *testing.T) {
	if dir := os.Getenv(captureDirEnvVar); dir != "" {
		testStalenessCaptureDir(t, dir)
		return
	}
	testStalenessTrackedCorpus(t)
}
