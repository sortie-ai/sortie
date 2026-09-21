package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// writeCapture stages a capture directory; provenance.json is empty
// because Run only checks its presence.
func writeCapture(t *testing.T, journalLines []string) string {
	t.Helper()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	evidencePath := evidencetest.WriteEvidenceFile(t, fixture.Records)
	dir := filepath.Dir(evidencePath)

	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write provenance.json: %v", err)
	}
	if journalLines != nil {
		content := strings.Join(journalLines, "\n")
		if content != "" {
			content += "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "observations.jsonl"), []byte(content), 0o600); err != nil {
			t.Fatalf("write observations.jsonl: %v", err)
		}
	}
	return dir
}

// measuredThreeSurfaceProfile declares entry points for all three
// measurable surfaces so the cardinality check matches a fixture built
// for all three.
func measuredThreeSurfaceProfile() profile.RuntimeProfile {
	return profile.RuntimeProfile{
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol:         {},
			evidence.SurfaceNativeJSON:       {},
			evidence.SurfaceNativeStreamJSON: {},
		},
	}
}

func marshalJournalEntry(t *testing.T, entry evidence.JournalEntry) string {
	t.Helper()
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v", entry, err)
	}
	return string(line)
}

// writeGeminiCliCapture loads the tracked gemini-cli profile, whose real
// recognizers a journal entry needs to exercise the recognizer and
// token-path machinery a zero-value profile never reaches.
func writeGeminiCliCapture(t *testing.T, journalLines []string) (string, profile.RuntimeProfile) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, "../profiles/gemini-cli.json")
	if err != nil {
		t.Fatalf("Load(...) error = %v, want nil", err)
	}

	dir := t.TempDir()
	for _, name := range []string{"evidence.jsonl", "provenance.json"} {
		content, readErr := os.ReadFile(filepath.Join("../probe/testdata/gemini-cli", name))
		if readErr != nil {
			t.Fatalf("read tracked corpus %s: %v", name, readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dir, name), content, 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", name, writeErr)
		}
	}
	if len(journalLines) > 0 {
		content := strings.Join(journalLines, "\n") + "\n"
		if writeErr := os.WriteFile(filepath.Join(dir, "observations.jsonl"), []byte(content), 0o600); writeErr != nil {
			t.Fatalf("write observations.jsonl: %v", writeErr)
		}
	}
	return dir, p
}

// geminiCliInventoryAgreementJournalLines shapes its entries exactly as
// the tracked gemini-cli profile's recognizer reads them, so other tests
// can reuse them unmodified and isolate their own mutation as the sole
// source of disagreement.
func geminiCliInventoryAgreementJournalLines(t *testing.T) []string {
	t.Helper()

	success1 := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       "success",
		Grade:      "usable",
		Outcome:    "pass",
		Detail:     "the recognized terminal reported end of turn",
		RecordedAt: "2026-01-01T00:00:00Z",
		Derivation: evidence.DerivationRecognizer,
		Launch: &evidence.LaunchRecord{
			Argv:    []string{"gemini", "--output-format", "json", "--prompt", "Reply with exactly SORTIE_BASELINE_OK and do not call any tool."},
			Outcome: evidence.LaunchOutcomeCompleted,
		},
		Streams: &evidence.StreamCapture{
			Stdout:      `{"response":{"text":"SORTIE_BASELINE_OK"},"session_id":"3753603d-5beb-435d-83cf-dd4080e4b12e","stats":{"models":{"gemini-3.5-flash":{"tokens":{"prompt":12,"cached":3}}}}}`,
			StdoutBytes: 168,
			Retention:   evidence.StreamRetentionFull,
		},
	}
	success2 := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       "success",
		Grade:      "usable",
		Outcome:    "pass",
		Detail:     "the recognized terminal reported end of turn",
		RecordedAt: "2026-01-01T00:00:01Z",
		Derivation: evidence.DerivationRecognizer,
		Launch: &evidence.LaunchRecord{
			Argv:    []string{"gemini", "--output-format", "json", "--prompt", "Reply with exactly SORTIE_BASELINE_OK and do not call any tool."},
			Outcome: evidence.LaunchOutcomeCompleted,
		},
		Streams: &evidence.StreamCapture{
			Stdout:      `{"response":{"text":"SORTIE_BASELINE_OK"},"session_id":"3753603d-5beb-435d-83cf-dd4080e4b12e","stats":{"models":{"gemini-3.5-flash":{"tokens":{"candidates":7}}}}}`,
			StdoutBytes: 148,
			Retention:   evidence.StreamRetentionFull,
		},
	}
	seed := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       continuationSeedLabel,
		Grade:      "usable",
		Outcome:    "pass",
		Detail:     "the seed launch completed a turn that left history",
		RecordedAt: "2026-01-01T00:00:02Z",
		Derivation: evidence.DerivationRecognizer,
		Launch: &evidence.LaunchRecord{
			Argv:    []string{"gemini", "--output-format", "json", "--prompt", "Remember the phrase {nonce} for the rest of this conversation and reply exactly STORED."},
			Nonce:   "SORTIE-NONCE-1234",
			Outcome: evidence.LaunchOutcomeCompleted,
		},
		Streams: &evidence.StreamCapture{
			Stdout:      `{"response":{"text":"STORED"},"session_id":"25857ffd-24ad-45f4-853e-7aca46b2acf3","stats":{"models":{"gemini-3.5-flash":{"tokens":{"thoughts":2}}}}}`,
			StdoutBytes: 148,
			Retention:   evidence.StreamRetentionFull,
		},
	}
	recall := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       continuationRecallLabel,
		Grade:      "usable",
		Outcome:    "pass",
		Detail:     "confirmed_same_session",
		RecordedAt: "2026-01-01T00:00:03Z",
		Derivation: evidence.DerivationRecognizer,
		Launch: &evidence.LaunchRecord{
			Argv:           []string{"gemini", "--output-format", "json", "--prompt", "Reply with the phrase from earlier in this conversation and no other text."},
			Nonce:          "SORTIE-NONCE-1234",
			PriorSessionID: "25857ffd-24ad-45f4-853e-7aca46b2acf3",
			Outcome:        evidence.LaunchOutcomeCompleted,
		},
		Streams: &evidence.StreamCapture{
			Stdout:      `{"response":{"text":"SORTIE-NONCE-1234"},"session_id":"25857ffd-24ad-45f4-853e-7aca46b2acf3","stats":{"models":{"gemini-3.5-flash":{"tokens":{"tool":1}}}}}`,
			StdoutBytes: 154,
			Retention:   evidence.StreamRetentionFull,
		},
	}
	inventory := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       "token_inventory",
		Grade:      "gap",
		Outcome:    "pass",
		Detail:     "read 4 recognized observation(s) and resolved 5 token-bearing path(s); no max_tokens stop was induced, so ceiling enforcement stays unverified",
		RecordedAt: "2026-01-01T00:00:04Z",
		Derivation: evidence.DerivationInventory,
	}

	return []string{
		marshalJournalEntry(t, success1),
		marshalJournalEntry(t, success2),
		marshalJournalEntry(t, seed),
		marshalJournalEntry(t, recall),
		marshalJournalEntry(t, inventory),
	}
}

// TestRunReDerivationAgreesWithPublishedRecord is the only case using a
// real recognizer; every other disagreement case runs recognizer-less,
// which proves a difference is reported but never that a real match
// passes.
func TestRunReDerivationAgreesWithPublishedRecord(t *testing.T) {
	t.Parallel()

	dir, p := writeGeminiCliCapture(t, geminiCliInventoryAgreementJournalLines(t))

	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		t.Fatalf("Run(...) error = %v, want nil: every entry's real re-derivation must agree with the tracked evidence.jsonl record at its own coordinate", err)
	}
	if !result.JournalAvailable {
		t.Fatal("Run(...).JournalAvailable = false, want true: observations.jsonl was staged")
	}
	if len(result.Conclusions.Truncated) != 0 {
		t.Errorf("Run(...).Conclusions.Truncated = %v, want none: every staged entry retains full streams", result.Conclusions.Truncated)
	}
}

// writeGeminiCliCaptureWithMutatedTokenGrade copies the tracked corpus
// byte-for-byte except the one record named by path, whose grade it
// overwrites, so the mutated grade is the only divergence under test.
func writeGeminiCliCaptureWithMutatedTokenGrade(t *testing.T, journalLines []string, path string, grade evidence.Grade) (string, profile.RuntimeProfile) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, "../profiles/gemini-cli.json")
	if err != nil {
		t.Fatalf("Load(...) error = %v, want nil", err)
	}

	rawEvidence, err := os.ReadFile("../probe/testdata/gemini-cli/evidence.jsonl")
	if err != nil {
		t.Fatalf("read tracked corpus evidence.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(rawEvidence), "\n"), "\n")
	mutated := false
	for i, line := range lines {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("json.Unmarshal(line %d) error = %v", i+1, err)
		}
		var evidencePath *string
		if err := json.Unmarshal(fields["evidence_path"], &evidencePath); err != nil {
			t.Fatalf("json.Unmarshal(evidence_path, line %d) error = %v", i+1, err)
		}
		if evidencePath == nil || *evidencePath != path {
			continue
		}
		gradeJSON, err := json.Marshal(grade)
		if err != nil {
			t.Fatalf("json.Marshal(%q) error = %v", grade, err)
		}
		fields["grade"] = gradeJSON
		out, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("json.Marshal(line %d) error = %v", i+1, err)
		}
		lines[i] = string(out)
		mutated = true
	}
	if !mutated {
		t.Fatalf("no tracked corpus record carries evidence_path %q to mutate", path)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "evidence.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write evidence.jsonl: %v", err)
	}
	provenance, err := os.ReadFile("../probe/testdata/gemini-cli/provenance.json")
	if err != nil {
		t.Fatalf("read tracked corpus provenance.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), provenance, 0o600); err != nil {
		t.Fatalf("write provenance.json: %v", err)
	}
	if len(journalLines) > 0 {
		content := strings.Join(journalLines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "observations.jsonl"), []byte(content), 0o600); err != nil {
			t.Fatalf("write observations.jsonl: %v", err)
		}
	}
	return dir, p
}

// TestRunTokenInventoryMultiPathGradeMismatch mutates one published
// record's grade; every native_json token_paths entry declares kind
// "spend", so the re-derivation resolves usable regardless, and only a
// live grade comparison catches the planted disagreement.
func TestRunTokenInventoryMultiPathGradeMismatch(t *testing.T) {
	t.Parallel()

	const mutatedPath = "/stats/models/gemini-3.5-flash/tokens/prompt"
	dir, p := writeGeminiCliCaptureWithMutatedTokenGrade(t, geminiCliInventoryAgreementJournalLines(t), mutatedPath, evidence.GradeCorroborationOnly)

	_, err := Run(Input{CaptureDir: dir, Profile: p})
	if err == nil {
		t.Fatal("Run(...) = _, nil, want rejection of a token record whose published grade disagrees with the re-derivation")
	}
	if !strings.Contains(err.Error(), "journal entry 5") {
		t.Errorf("Run(...) error = %v, want it to name journal entry 5, the inventory entry", err)
	}
	if !strings.Contains(err.Error(), mutatedPath) {
		t.Errorf("Run(...) error = %v, want it to name the mutated token record's path %q", err, mutatedPath)
	}
	if !strings.Contains(err.Error(), "carries grade=corroboration_only") {
		t.Errorf("Run(...) error = %v, want it to report the published record's grade=corroboration_only", err)
	}
	if !strings.Contains(err.Error(), "yields grade=usable") {
		t.Errorf("Run(...) error = %v, want it to report the re-derived grade=usable", err)
	}
}

// geminiCliInventoryJournalLinesWithTruncatedStream removes one entry's
// stats object so the re-derivation resolves fewer token paths than the
// corpus publishes.
func geminiCliInventoryJournalLinesWithTruncatedStream(t *testing.T) []string {
	t.Helper()

	lines := geminiCliInventoryAgreementJournalLines(t)

	var entry evidence.JournalEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatalf("json.Unmarshal(journal line 2) error = %v", err)
	}
	entry.Streams.Stdout = `{"response":{"text":"SORTIE_BASELINE_OK"},"session_id":"3753603d-5beb-435d-83cf-dd4080e4b12e"}`
	entry.Streams.Retention = evidence.StreamRetentionTruncated
	lines[1] = marshalJournalEntry(t, entry)
	return lines
}

// TestRunTokenInventoryTruncatedPathCountMismatch expects a genuinely
// truncated path-count mismatch reported in Conclusions.Truncated, not
// failed, since the discarded middle is not what the recognizer read.
func TestRunTokenInventoryTruncatedPathCountMismatch(t *testing.T) {
	t.Parallel()

	dir, p := writeGeminiCliCapture(t, geminiCliInventoryJournalLinesWithTruncatedStream(t))

	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		t.Fatalf("Run(...) error = %v, want nil: a truncated entry's discarded middle is not what the recognizer read", err)
	}
	if len(result.Conclusions.Truncated) != 1 {
		t.Fatalf("Run(...).Conclusions.Truncated = %v, want exactly one reported difference", result.Conclusions.Truncated)
	}
	reported := result.Conclusions.Truncated[0]
	if !strings.Contains(reported, "journal entry 5") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to name journal entry 5, the inventory entry", reported)
	}
	if !strings.Contains(reported, "resolves 4 token-bearing path(s)") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to state the re-derivation resolved 4 paths", reported)
	}
	if !strings.Contains(reported, "published evidence.jsonl carries 5 record(s)") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to state the published set carries 5 records", reported)
	}
}

// stripStatsObject removes the top-level "stats" member from a native_json
// stdout payload, the member a discarded middle would carry away, leaving
// every other member -- "response", "session_id" -- untouched.
func stripStatsObject(t *testing.T, stdout string) string {
	t.Helper()

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", stdout, err)
	}
	if _, ok := payload["stats"]; !ok {
		t.Fatalf("stdout %q carries no stats object to strip", stdout)
	}
	delete(payload, "stats")
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(%q without stats) error = %v", stdout, err)
	}
	return string(out)
}

// geminiCliInventoryJournalLinesWithEveryStreamTruncated removes every
// entry's stats object so the re-derivation resolves zero token paths
// against the corpus's five published records.
func geminiCliInventoryJournalLinesWithEveryStreamTruncated(t *testing.T) []string {
	t.Helper()

	lines := geminiCliInventoryAgreementJournalLines(t)

	for i := range 4 {
		var entry evidence.JournalEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			t.Fatalf("json.Unmarshal(journal line %d) error = %v", i+1, err)
		}
		entry.Streams.Stdout = stripStatsObject(t, entry.Streams.Stdout)
		entry.Streams.Retention = evidence.StreamRetentionTruncated
		lines[i] = marshalJournalEntry(t, entry)
	}
	return lines
}

// TestRunTokenInventoryZeroPathCountMismatch expects a zero-path mismatch
// reported in Conclusions.Truncated, not failed, since the discarded
// middle is not what the recognizer read.
func TestRunTokenInventoryZeroPathCountMismatch(t *testing.T) {
	t.Parallel()

	dir, p := writeGeminiCliCapture(t, geminiCliInventoryJournalLinesWithEveryStreamTruncated(t))

	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		t.Fatalf("Run(...) error = %v, want nil: a truncated entry's discarded middle is not what the recognizer read", err)
	}
	if len(result.Conclusions.Truncated) != 1 {
		t.Fatalf("Run(...).Conclusions.Truncated = %v, want exactly one reported difference", result.Conclusions.Truncated)
	}
	reported := result.Conclusions.Truncated[0]
	if !strings.Contains(reported, "journal entry 5") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to name journal entry 5, the inventory entry", reported)
	}
	if !strings.Contains(reported, "carries 5 record(s) for surface native_json input token_inventory_v1") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to state the published set carries 5 records for the coordinate the zero-path branch looked up", reported)
	}
	if !strings.Contains(reported, "want 1") {
		t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to state the zero-path branch expected exactly one published record", reported)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("the tracked gemini-cli capture re-derives with no PATH runtime, credential, or network", func(t *testing.T) {
		t.Parallel()

		root, err := profile.CheckoutRoot()
		if err != nil {
			t.Fatalf("CheckoutRoot() error = %v, want nil", err)
		}
		p, err := profile.Load(root, "../profiles/gemini-cli.json")
		if err != nil {
			t.Fatalf("Load(...) error = %v, want nil", err)
		}
		result, err := Run(Input{CaptureDir: "../probe/testdata/gemini-cli", Profile: p})
		if err != nil {
			t.Fatalf("Run(...) error = %v, want nil", err)
		}
		if result.JournalAvailable {
			t.Error("Run(...).JournalAvailable = true, want false: the tracked corpus carries no observations.jsonl yet")
		}
		if result.Parity == "" || result.Conformance == "" {
			t.Errorf("Run(...) Parity=%q Conformance=%q, want both non-empty", result.Parity, result.Conformance)
		}
	})

	t.Run("a missing evidence.jsonl fails naming the file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write provenance.json: %v", err)
		}
		_, err := Run(Input{CaptureDir: dir})
		if err == nil {
			t.Fatal("Run(...) = _, nil, want rejection of a capture directory missing evidence.jsonl")
		}
		if !strings.Contains(err.Error(), "evidence.jsonl") {
			t.Errorf("Run(...) error = %v, want it to name evidence.jsonl", err)
		}
	})

	t.Run("a missing provenance.json fails naming the file", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		evidencePath := evidencetest.WriteEvidenceFile(t, fixture.Records)
		dir := filepath.Dir(evidencePath)

		_, err := Run(Input{CaptureDir: dir})
		if err == nil {
			t.Fatal("Run(...) = _, nil, want rejection of a capture directory missing provenance.json")
		}
		if !strings.Contains(err.Error(), "provenance.json") {
			t.Errorf("Run(...) error = %v, want it to name provenance.json", err)
		}
	})

	t.Run("Run writes no file to the capture directory", func(t *testing.T) {
		t.Parallel()

		dir := writeCapture(t, nil)
		before, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("os.ReadDir(%q) error = %v", dir, err)
		}
		if _, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()}); err != nil {
			t.Fatalf("Run(...) error = %v, want nil", err)
		}
		after, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("os.ReadDir(%q) error = %v", dir, err)
		}
		if len(before) != len(after) {
			t.Errorf("capture directory carries %d entries after Run, wanted %d: Run must write no file", len(after), len(before))
		}
	})

	// Each entry below is internally consistent, so only a comparison
	// against the published evidence.jsonl record catches the disagreement.
	t.Run("a recognizer journal entry that agrees with its own launch but disagrees with the published record fails", func(t *testing.T) {
		t.Parallel()

		entry := evidence.JournalEntry{
			Surface:    "native_json",
			Case:       "success",
			Grade:      "not_observed",
			Outcome:    "fixture_induction_failed",
			Detail:     "no recognized end-of-turn terminal was observed",
			RecordedAt: "2026-01-01T00:00:00Z",
			Derivation: evidence.DerivationRecognizer,
			Launch: &evidence.LaunchRecord{
				Argv:    []string{"runtime", "--output-format", "json"},
				Outcome: evidence.LaunchOutcomeCompleted,
			},
			Streams: &evidence.StreamCapture{
				Stdout:      `{"response":{"text":"no recognizable terminal here"}}`,
				StdoutBytes: 48,
				Retention:   evidence.StreamRetentionFull,
			},
		}
		dir := writeCapture(t, []string{marshalJournalEntry(t, entry)})

		_, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()})
		if err == nil {
			t.Fatal("Run(...) = _, nil, want rejection of a re-derivation that disagrees with the published evidence.jsonl record")
		}
		if !strings.Contains(err.Error(), "journal entry 1") {
			t.Errorf("Run(...) error = %v, want it to name the journal entry", err)
		}
		if !strings.Contains(err.Error(), "published evidence.jsonl") {
			t.Errorf("Run(...) error = %v, want it to name the published evidence.jsonl record it disagrees with", err)
		}
	})

	t.Run("an inventory journal entry that agrees with its own streams but disagrees with the published record fails", func(t *testing.T) {
		t.Parallel()

		entry := evidence.JournalEntry{
			Surface:    "native_json",
			Case:       "token_inventory",
			Grade:      "not_observed",
			Outcome:    "fixture_induction_failed",
			Detail:     "the surface states no recognizer, so no terminal could be read",
			RecordedAt: "2026-01-01T00:00:00Z",
			Derivation: evidence.DerivationInventory,
		}
		dir := writeCapture(t, []string{marshalJournalEntry(t, entry)})

		_, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()})
		if err == nil {
			t.Fatal("Run(...) = _, nil, want rejection of a re-derivation that disagrees with the published evidence.jsonl record")
		}
		if !strings.Contains(err.Error(), "journal entry 1") {
			t.Errorf("Run(...) error = %v, want it to name the journal entry", err)
		}
		if !strings.Contains(err.Error(), "published evidence.jsonl") {
			t.Errorf("Run(...) error = %v, want it to name the published evidence.jsonl record it disagrees with", err)
		}
	})

	t.Run("a recognizer journal entry whose case is outside the closed value set fails rather than being skipped", func(t *testing.T) {
		t.Parallel()

		entry := evidence.JournalEntry{
			Surface:    "native_json",
			Case:       "not_a_real_case",
			Grade:      "not_observed",
			Outcome:    "fixture_induction_failed",
			Detail:     "an entry the recognizer cannot key on",
			RecordedAt: "2026-01-01T00:00:00Z",
			Derivation: evidence.DerivationRecognizer,
			Launch: &evidence.LaunchRecord{
				Argv:    []string{"runtime"},
				Outcome: evidence.LaunchOutcomeCompleted,
			},
			Streams: &evidence.StreamCapture{
				Stdout:      "irrelevant output",
				StdoutBytes: 17,
				Retention:   evidence.StreamRetentionFull,
			},
		}
		dir := writeCapture(t, []string{marshalJournalEntry(t, entry)})

		_, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()})
		if err == nil {
			t.Fatal("Run(...) = _, nil, want rejection of a case the recognizer cannot key on rather than a silent skip")
		}
		if !strings.Contains(err.Error(), "not_a_real_case") {
			t.Errorf("Run(...) error = %v, want it to name the unrecognized case", err)
		}
	})

	t.Run("removing the journal from the same fixture succeeds", func(t *testing.T) {
		t.Parallel()

		dir := writeCapture(t, nil)
		result, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()})
		if err != nil {
			t.Fatalf("Run(...) error = %v, want nil once the journal is absent", err)
		}
		if result.JournalAvailable {
			t.Error("Run(...).JournalAvailable = true, want false: no observations.jsonl was staged")
		}
	})

	t.Run("a truncated recognizer entry surfaces as a non-fatal difference rather than being skipped", func(t *testing.T) {
		t.Parallel()

		entry := evidence.JournalEntry{
			Surface:    "native_json",
			Case:       "success",
			Grade:      "usable",
			Outcome:    "pass",
			Detail:     "the recognized terminal reported end of turn",
			RecordedAt: "2026-01-01T00:00:00Z",
			Derivation: evidence.DerivationRecognizer,
			Launch: &evidence.LaunchRecord{
				Argv:    []string{"runtime"},
				Outcome: evidence.LaunchOutcomeCompleted,
			},
			Streams: &evidence.StreamCapture{
				Stdout:      "truncated content that would not match anyway",
				StdoutBytes: 1 << 20,
				Retention:   evidence.StreamRetentionTruncated,
			},
		}
		dir := writeCapture(t, []string{marshalJournalEntry(t, entry)})

		result, err := Run(Input{CaptureDir: dir, Profile: measuredThreeSurfaceProfile()})
		if err != nil {
			t.Fatalf("Run(...) error = %v, want nil: a truncated entry's discarded middle is not what the recognizer read", err)
		}
		if len(result.Conclusions.Truncated) != 1 {
			t.Fatalf("Run(...).Conclusions.Truncated = %v, want exactly one reported difference", result.Conclusions.Truncated)
		}
		if !strings.Contains(result.Conclusions.Truncated[0], "journal entry 1") {
			t.Errorf("Run(...).Conclusions.Truncated[0] = %q, want it to name the journal entry", result.Conclusions.Truncated[0])
		}
		summary := formatSummary(result.Conclusions, !result.JournalAvailable)
		if !strings.Contains(summary, result.Conclusions.Truncated[0]) {
			t.Errorf("formatSummary(...) omits the truncated difference %q it must report", result.Conclusions.Truncated[0])
		}
	})
}
