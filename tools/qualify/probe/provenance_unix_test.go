//go:build unix

package probe

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func firstProfile(t *testing.T) profile.RuntimeProfile {
	t.Helper()
	paths := stalenessProfilePaths(t)
	p, err := profile.Load(mustRepositoryRoot(t), paths[0])
	if err != nil {
		t.Fatalf("profile.Load(%s) error = %v, want nil", paths[0], err)
	}
	return p
}

func TestGradedEvidenceStampsTheCurrentRun(t *testing.T) {
	t.Parallel()

	p := firstProfile(t)
	collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)

	before := time.Now().UTC().Add(-time.Second)
	fixture, err := gradedEvidence(p, collected, time.Now().UTC())
	if err != nil {
		t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
	}
	after := time.Now().UTC().Add(time.Second)

	for _, rec := range fixture.Records {
		stamped, parseErr := time.Parse(time.RFC3339, rec.ObservedAt)
		if parseErr != nil {
			t.Fatalf("record %d observed_at %q does not parse: %v", rec.Sequence, rec.ObservedAt, parseErr)
		}
		if stamped.Before(before) || stamped.After(after) {
			t.Errorf("record %d (%s/%s) observed_at = %s, want a time inside this run's own window [%s, %s]",
				rec.Sequence, rec.Scenario, rec.Capability, rec.ObservedAt, before.Format(time.RFC3339), after.Format(time.RFC3339))
		}
	}
}

// Not parallel: agenttest.InstallLogSpy mutates the global slog default.
func TestGradedEvidenceAttributesEachHandshakeToItsOwnSession(t *testing.T) {
	p := firstProfile(t)
	collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)

	spy := agenttest.InstallLogSpy(t)
	slog.Default().Info("agent implementation",
		slog.String("session_id", "sess-protocol-permission"),
		slog.String("name", "sample-runtime"), slog.String("version", "1.0.0"))
	slog.Default().Info("agent implementation",
		slog.String("session_id", "sess-protocol-e2e"),
		slog.String("name", "sample-runtime"), slog.String("version", "2.0.0"))

	obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})
	collected.identityObs = obs
	collected.identities = identities
	collected.identityProtocolVersion = 1

	fixture, err := gradedEvidence(p, collected, time.Now().UTC())
	if err != nil {
		t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
	}

	want := map[string]string{
		"sess-protocol-permission": "1.0.0",
		"sess-protocol-e2e":        "2.0.0",
	}
	for session, wantVersion := range want {
		rec := identityRecordFor(t, fixture.Records, session)
		if rec.AgentVersion == nil || *rec.AgentVersion != wantVersion {
			t.Errorf("runtime identity record for %s carries agent_version %s, want the version that session's own handshake reported (%q)",
				session, stringOrNull(rec.AgentVersion), wantVersion)
		}
	}
}

// Not parallel: agenttest.InstallLogSpy mutates the global slog default.
func TestGradedEvidenceLeavesAnUnhandshakenSessionUnidentified(t *testing.T) {
	p := firstProfile(t)
	collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)

	spy := agenttest.InstallLogSpy(t)
	slog.Default().Info("agent implementation",
		slog.String("session_id", "sess-protocol-permission"),
		slog.String("name", "sample-runtime"), slog.String("version", "1.0.0"))

	obs, identities := induceRuntimeIdentity(&sharedFixture{logSpy: spy})
	collected.identityObs = obs
	collected.identities = identities
	collected.identityProtocolVersion = 1

	fixture, err := gradedEvidence(p, collected, time.Now().UTC())
	if err != nil {
		t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
	}

	rec := identityRecordFor(t, fixture.Records, "sess-protocol-e2e")
	if rec.Grade != evidence.GradeNotObserved {
		t.Errorf("runtime identity record for a session no handshake named carries grade %s, want %s",
			rec.Grade, evidence.GradeNotObserved)
	}
	if rec.AgentName != nil || rec.AgentVersion != nil {
		t.Errorf("runtime identity record for a session no handshake named carries agent %s/%s, want both null",
			stringOrNull(rec.AgentName), stringOrNull(rec.AgentVersion))
	}
}

func identityRecordFor(t *testing.T, records []evidence.Record, sessionID string) evidence.Record {
	t.Helper()
	for _, rec := range records {
		if rec.Scenario == evidence.ScenarioRuntimeIdentity && rec.SessionID != nil && *rec.SessionID == sessionID {
			return rec
		}
	}
	t.Fatalf("no runtime identity record for session %s", sessionID)
	return evidence.Record{}
}

func stringOrNull(value *string) string {
	if value == nil {
		return "<null>"
	}
	return *value
}

func TestPublishedEvidenceReplaysToTheSameGrades(t *testing.T) {
	t.Parallel()

	p := firstProfile(t)
	collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)

	fixture, err := gradedEvidence(p, collected, time.Now().UTC())
	if err != nil {
		t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
	}

	evidencePath := filepath.Join(t.TempDir(), "evidence.jsonl")
	if err := writeEvidenceRecords(evidencePath, fixture.Records); err != nil {
		t.Fatalf("writeEvidenceRecords(...) error = %v, want nil", err)
	}

	published, err := evidence.ReadEvidenceFile(evidencePath)
	if err != nil {
		t.Fatalf("ReadEvidenceFile(%s) error = %v, want nil", evidencePath, err)
	}
	if len(published) != len(fixture.Records) {
		t.Fatalf("published evidence carries %d records, want the %d the run graded", len(published), len(fixture.Records))
	}
	for i, rec := range published {
		if !evidence.RecordsEqual(rec, fixture.Records[i]) {
			t.Errorf("published record %d = %+v, want the graded record %+v", i+1, rec, fixture.Records[i])
		}
	}
}

func TestCollectorDigestFollowsCollectorSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSource := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	digest := func() string {
		t.Helper()
		sum, err := collectorDigest(root)
		if err != nil {
			t.Fatalf("collectorDigest(%s) error = %v, want nil", root, err)
		}
		return sum
	}

	writeSource("grading.go", "package probe\n\nconst threshold = 1\n")
	writeSource("probe/inducer.go", "package inner\n")
	writeSource("grading_test.go", "package probe\n")
	base := digest()

	writeSource("grading_test.go", "package probe\n\nfunc TestSomething() {}\n")
	if got := digest(); got != base {
		t.Errorf("collectorDigest changed with a test source alone: %s, want %s", got, base)
	}

	writeSource("grading.go", "package probe\n\nconst threshold = 2\n")
	changed := digest()
	if changed == base {
		t.Errorf("collectorDigest = %s after a grading change, want a different digest", changed)
	}

	writeSource("grading.go", "package probe\n\nconst threshold = 1\n")
	writeSource("probe/moved.go", "package inner\n")
	if err := os.Remove(filepath.Join(root, "probe", "inducer.go")); err != nil {
		t.Fatalf("remove the moved source: %v", err)
	}
	if got := digest(); got == base {
		t.Errorf("collectorDigest = %s after a source moved to another path, want a different digest", got)
	}
}

func TestCollectionProvenanceNamesTheRunWithoutCredentials(t *testing.T) {
	const secret = "sk-provenance-control-value"
	t.Setenv("PROVENANCE_CONTROL_TOKEN", secret)

	p := firstProfile(t)
	evidencePath := filepath.Join(t.TempDir(), "evidence.jsonl")
	if err := os.WriteFile(evidencePath, []byte("{\"sequence\":1}\n"), 0o600); err != nil {
		t.Fatalf("write the evidence artifact: %v", err)
	}
	coords := Coordinates{
		Model:        "a-model-identifier",
		AuthEnvNames: []string{"PROVENANCE_CONTROL_TOKEN", "PROVENANCE_CONTROL_ABSENT"},
		Profile:      p,
	}
	startedAt := time.Now().UTC()
	identities := map[string]evidence.SessionIdentity{"sess-a": {Name: "sample-runtime", Version: "1.2.3"}}

	provenance := collectionProvenanceOf(t, coords, startedAt, evidencePath, identities)

	if provenance.ObservedAt != startedAt.Format(time.RFC3339) {
		t.Errorf("provenance observed_at = %s, want the collection's own start %s", provenance.ObservedAt, startedAt.Format(time.RFC3339))
	}
	if provenance.ProfileDigest != p.Digest() {
		t.Errorf("provenance profile_digest = %s, want %s", provenance.ProfileDigest, p.Digest())
	}
	if provenance.CollectorDigest == "" {
		t.Error("provenance carries no collector_digest, so a changed collector passes as the same measurement")
	}
	wantEvidence, err := fileDigest(evidencePath)
	if err != nil {
		t.Fatalf("fileDigest(%s) error = %v, want nil", evidencePath, err)
	}
	if provenance.EvidenceDigest != wantEvidence || provenance.EvidenceFile != "evidence.jsonl" {
		t.Errorf("provenance names evidence %s/%s, want evidence.jsonl/%s", provenance.EvidenceFile, provenance.EvidenceDigest, wantEvidence)
	}
	if provenance.RequestedModel != "a-model-identifier" {
		t.Errorf("provenance requested_model = %q, want the model the run asked for", provenance.RequestedModel)
	}
	if got := provenance.SessionAgents["sess-a"]; got.Name != "sample-runtime" || got.Version != "1.2.3" {
		t.Errorf("provenance session agent = %+v, want the handshake that session reported", got)
	}
	if provenance.CredentialMode["PROVENANCE_CONTROL_TOKEN"] != "present" {
		t.Errorf("provenance credential mode for a supplied variable = %q, want present", provenance.CredentialMode["PROVENANCE_CONTROL_TOKEN"])
	}
	if provenance.CredentialMode["PROVENANCE_CONTROL_ABSENT"] != "absent" {
		t.Errorf("provenance credential mode for an unset variable = %q, want absent", provenance.CredentialMode["PROVENANCE_CONTROL_ABSENT"])
	}

	path := filepath.Join(t.TempDir(), "provenance.json")
	if err := writeProvenance(path, provenance); err != nil {
		t.Fatalf("writeProvenance(%s) error = %v, want nil", path, err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(written), secret) {
		t.Error("the provenance artifact carries the credential value itself, want only whether it was supplied")
	}
}
