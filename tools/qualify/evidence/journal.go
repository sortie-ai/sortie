package evidence

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Derivation names what decided one journal entry's grade, and so what a
// re-derivation can reproduce.
type Derivation string

const (
	// DerivationRecognizer states that one launch's Streams.Stdout, read
	// through the profile's recognizer for the entry's surface, decided the
	// grade.
	DerivationRecognizer Derivation = "recognizer"
	// DerivationInventory states that every launch the journal retains for the
	// entry's surface, in journal order, decided the grade.
	DerivationInventory Derivation = "inventory"
	// DerivationTransport states that protocol events, process state, or an
	// induction that failed before its launch produced bytes decided the
	// grade.
	DerivationTransport Derivation = "transport"
	// DerivationComposed states that another entry's observation, restated,
	// decided the grade.
	DerivationComposed Derivation = "composed"
)

// Derivations is the closed derivation set.
var Derivations = []Derivation{
	DerivationRecognizer, DerivationInventory, DerivationTransport, DerivationComposed,
}

// LaunchOutcome is the closed outcome set a LaunchRecord carries. Recognition
// consults it, so a re-derivation cannot proceed without it.
type LaunchOutcome string

const (
	LaunchOutcomeCompleted    LaunchOutcome = "completed"
	LaunchOutcomeLaunchFailed LaunchOutcome = "launch_failed"
	LaunchOutcomeRunFailed    LaunchOutcome = "run_failed"
)

// LaunchOutcomes is the closed launch-outcome set.
var LaunchOutcomes = []LaunchOutcome{
	LaunchOutcomeCompleted, LaunchOutcomeLaunchFailed, LaunchOutcomeRunFailed,
}

// StreamRetention is the closed retention set a StreamCapture carries. An
// empty stream is "full" with a zero byte count, never a truncation.
type StreamRetention string

const (
	StreamRetentionFull      StreamRetention = "full"
	StreamRetentionTruncated StreamRetention = "truncated"
)

// StreamRetentions is the closed stream-retention set.
var StreamRetentions = []StreamRetention{StreamRetentionFull, StreamRetentionTruncated}

// LaunchRecord is one launch's coordinates and the disposition it ended
// under. A credential value never appears: Argv carries no credential, and
// AuthEnvNames carries names only.
type LaunchRecord struct {
	Argv            []string `json:"argv"`
	AuthEnvNames    []string `json:"auth_env_names"`
	RuntimeVersion  string   `json:"runtime_version"`
	Model           string   `json:"model"`
	PromptID        string   `json:"prompt_id"`
	Nonce           string   `json:"nonce,omitempty"`
	SeededSessionID string   `json:"seeded_session_id,omitempty"`
	// PriorSessionID is an earlier launch's reported identifier, compared
	// against this launch's grading, empty when unobserved; a resume
	// argument's own value is SeededSessionID.
	PriorSessionID string `json:"prior_session_id,omitempty"`
	// ProbeMarker reports whether the probe's marker file was present in the
	// launch workspace when the launch ended. The native human_input arms
	// grade on it and no stream carries it.
	ProbeMarker bool `json:"probe_marker"`
	// Outcome is the closed set: "completed", "launch_failed", "run_failed".
	Outcome LaunchOutcome `json:"outcome"`
}

// StreamCapture is the retained bytes one launch's standard streams
// produced, bounded at 64 KiB per stream, retained head and tail with the
// middle discarded, with the size before the bound recorded.
type StreamCapture struct {
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	StdoutBytes int64  `json:"stdout_bytes"`
	StderrBytes int64  `json:"stderr_bytes"`
	// Retention is the closed set: "full", "truncated".
	Retention StreamRetention `json:"retention"`
}

// Combined returns Stdout and Stderr concatenated, the shape a native
// launch's recognizer reads; the streams are captured apart only for
// independent retention bounding.
func (s StreamCapture) Combined() string {
	return s.Stdout + s.Stderr
}

// JournalEntry is one observation journal line: what a live capture recorded
// before grading, so an offline re-derivation can reproduce the grade.
type JournalEntry struct {
	Surface    string `json:"surface"`
	Case       string `json:"case"`
	Grade      string `json:"grade"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	RecordedAt string `json:"recorded_at"`

	// Derivation states what decided this entry's grade. Closed set and
	// presence rule: see the Launch/Streams presence table this type's
	// package documents.
	Derivation Derivation `json:"derivation"`

	// Launch and Streams are both present or both absent, per Derivation. A
	// launch that produced nothing carries both, with zero byte counts.
	Launch  *LaunchRecord  `json:"launch,omitempty"`
	Streams *StreamCapture `json:"streams,omitempty"`
}

// derivationLaunchPresence states whether an entry's Derivation MUST carry
// Launch and Streams; DerivationTransport is excluded and checked separately.
var derivationLaunchPresence = map[Derivation]bool{
	DerivationRecognizer: true,
	DerivationInventory:  false,
	DerivationComposed:   false,
}

// DecodeJournalEntry decodes one journal line, rejecting an invalid or absent
// derivation, a launch/streams presence the derivation forbids, and an
// invalid outcome or retention.
func DecodeJournalEntry(line []byte) (JournalEntry, error) {
	var entry JournalEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return JournalEntry{}, fmt.Errorf("decode journal entry: %w", err)
	}
	if entry.Derivation == "" || !slices.Contains(Derivations, entry.Derivation) {
		return JournalEntry{}, fmt.Errorf("derivation %q is outside the closed value set", entry.Derivation)
	}

	present := entry.Launch != nil && entry.Streams != nil
	if entry.Launch != nil && entry.Streams == nil || entry.Launch == nil && entry.Streams != nil {
		return JournalEntry{}, fmt.Errorf("derivation %q carries launch and streams inconsistently", entry.Derivation)
	}

	if wantPresent, constrained := derivationLaunchPresence[entry.Derivation]; constrained && present != wantPresent {
		return JournalEntry{}, fmt.Errorf("derivation %q requires launch/streams presence %v, got %v", entry.Derivation, wantPresent, present)
	}

	if !present {
		return entry, nil
	}
	if entry.Launch.Outcome == "" || !slices.Contains(LaunchOutcomes, entry.Launch.Outcome) {
		return JournalEntry{}, fmt.Errorf("launch outcome %q is outside the closed value set", entry.Launch.Outcome)
	}
	if entry.Streams.Retention == "" || !slices.Contains(StreamRetentions, entry.Streams.Retention) {
		return JournalEntry{}, fmt.Errorf("stream retention %q is outside the closed value set", entry.Streams.Retention)
	}
	return entry, nil
}
