package evidence

import (
	"encoding/json"
	"testing"
)

func validLaunch() LaunchRecord {
	return LaunchRecord{
		Argv:           []string{"runtime", "--flag"},
		AuthEnvNames:   []string{"RUNTIME_API_KEY"},
		RuntimeVersion: "1.2.3",
		Model:          "gemini-3.5-flash",
		PromptID:       "prompt-1",
		Outcome:        LaunchOutcomeCompleted,
	}
}

func validStreams() StreamCapture {
	return StreamCapture{
		Stdout:      "terminal output",
		StdoutBytes: 16,
		Retention:   StreamRetentionFull,
	}
}

func marshalEntry(t *testing.T, entry JournalEntry) []byte {
	t.Helper()
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v", entry, err)
	}
	return line
}

func TestDecodeJournalEntryDerivationTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entry   JournalEntry
		wantErr bool
	}{
		{
			name: "recognizer requires launch and streams",
			entry: JournalEntry{
				Surface: "native_json", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationRecognizer,
				Launch: new(validLaunch()), Streams: new(validStreams()),
			},
			wantErr: false,
		},
		{
			name: "recognizer without launch/streams is rejected",
			entry: JournalEntry{
				Surface: "native_json", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationRecognizer,
			},
			wantErr: true,
		},
		{
			name: "inventory requires no launch or streams",
			entry: JournalEntry{
				Surface: "native_json", Case: "token_inventory", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationInventory,
			},
			wantErr: false,
		},
		{
			name: "inventory with launch/streams is rejected",
			entry: JournalEntry{
				Surface: "native_json", Case: "token_inventory", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationInventory,
				Launch: new(validLaunch()), Streams: new(validStreams()),
			},
			wantErr: true,
		},
		{
			name: "transport with launch/streams present is accepted",
			entry: JournalEntry{
				Surface: "protocol", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationTransport,
				Launch: new(validLaunch()), Streams: new(validStreams()),
			},
			wantErr: false,
		},
		{
			name: "transport with launch/streams absent is accepted",
			entry: JournalEntry{
				Surface: "protocol", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationTransport,
			},
			wantErr: false,
		},
		{
			name: "composed requires no launch or streams",
			entry: JournalEntry{
				Surface: "protocol", Case: "non_retryable_refusal", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationComposed,
			},
			wantErr: false,
		},
		{
			name: "composed with launch/streams present is rejected",
			entry: JournalEntry{
				Surface: "protocol", Case: "non_retryable_refusal", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationComposed,
				Launch: new(validLaunch()), Streams: new(validStreams()),
			},
			wantErr: true,
		},
		{
			name: "empty derivation is rejected",
			entry: JournalEntry{
				Surface: "protocol", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z",
			},
			wantErr: true,
		},
		{
			name: "unrecognized derivation is rejected",
			entry: JournalEntry{
				Surface: "protocol", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: "invented",
			},
			wantErr: true,
		},
		{
			name: "launch present with streams absent is rejected regardless of derivation",
			entry: JournalEntry{
				Surface: "native_json", Case: "success", Grade: "usable", Outcome: "pass",
				RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationRecognizer,
				Launch: new(validLaunch()),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line := marshalEntry(t, tt.entry)
			_, err := DecodeJournalEntry(line)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("DecodeJournalEntry(%s) error = %v, wantErr %v", line, err, tt.wantErr)
			}
		})
	}
}

func TestDecodeJournalEntryRejectsUnrecognizedLaunchOutcome(t *testing.T) {
	t.Parallel()

	launch := validLaunch()
	launch.Outcome = "invented_outcome"
	entry := JournalEntry{
		Surface: "native_json", Case: "success", Grade: "usable", Outcome: "pass",
		RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationRecognizer,
		Launch: &launch, Streams: new(validStreams()),
	}
	if _, err := DecodeJournalEntry(marshalEntry(t, entry)); err == nil {
		t.Error("DecodeJournalEntry(invented launch outcome) = nil error, want rejection")
	}
}

func TestDecodeJournalEntryRejectsUnrecognizedStreamRetention(t *testing.T) {
	t.Parallel()

	streams := validStreams()
	streams.Retention = "invented_retention"
	entry := JournalEntry{
		Surface: "native_json", Case: "success", Grade: "usable", Outcome: "pass",
		RecordedAt: "2026-01-01T00:00:00Z", Derivation: DerivationRecognizer,
		Launch: new(validLaunch()), Streams: &streams,
	}
	if _, err := DecodeJournalEntry(marshalEntry(t, entry)); err == nil {
		t.Error("DecodeJournalEntry(invented stream retention) = nil error, want rejection")
	}
}

func TestDecodeJournalEntryLaunchRecordCarriesNoCredential(t *testing.T) {
	t.Parallel()

	launch := validLaunch()
	line, err := json.Marshal(launch)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v", launch, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := fields["auth_token"]; ok {
		t.Error("marshaled LaunchRecord carries an auth_token field, want AuthEnvNames as names only")
	}
	if _, ok := fields["credential"]; ok {
		t.Error("marshaled LaunchRecord carries a credential field, want none")
	}
}

func TestStreamCaptureCombined(t *testing.T) {
	t.Parallel()

	s := StreamCapture{Stdout: "out", Stderr: "err"}
	if got, want := s.Combined(), "outerr"; got != want {
		t.Errorf("StreamCapture.Combined() = %q, want %q", got, want)
	}
}
